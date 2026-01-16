package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	// gorilla/websocket is the most popular WebSocket library for Go.
	// It provides a complete implementation of the WebSocket protocol
	// with support for reading/writing messages, ping/pong, and close handling.
	"github.com/gorilla/websocket"

	// Diff package provides diff parsing and binary file detection.
	"github.com/pseudocoder/host/internal/diff"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"

	// Storage package provides card and session persistence.
	"github.com/pseudocoder/host/internal/storage"

	// Stream package provides CardBroadcaster types for streaming diff cards.
	"github.com/pseudocoder/host/internal/stream"

	// Tmux package provides integration with tmux terminal multiplexer.
	"github.com/pseudocoder/host/internal/tmux"

	// Rate limiting for terminal input to prevent message flooding.
	"golang.org/x/time/rate"
)

// channelBufferSize is the buffer size for the broadcast channel and per-client
// send channels. This value balances memory usage against the ability to absorb
// bursts of messages without blocking senders. If the buffer fills up, messages
// may be dropped for slow clients.
const channelBufferSize = 256

// DecisionHandler processes review decisions from clients.
// It is called when a client sends a review.decision message.
// Implementations should apply the decision (accept/reject) to the
// repository and storage, returning any error that occurred.
type DecisionHandler func(cardID, action, comment string) error

// ChunkDecisionHandler processes per-chunk decisions from clients.
// It is called when a client sends a chunk.decision message.
// The handler should apply the decision to the specific chunk within the card.
// The contentHash parameter is the hash of the chunk content when the user viewed it.
// If non-empty, the handler should validate it matches current content before applying.
type ChunkDecisionHandler func(cardID string, chunkIndex int, action string, contentHash string) error

// DeleteHandler processes file deletion requests from clients.
// It is called when a client sends a review.delete message for an untracked file.
// The handler should delete the file from the filesystem and remove the card from storage.
type DeleteHandler func(cardID string) error

// ReconnectHandler is called when a client connects to replay history.
// It should send terminal history and pending cards to the client.
// The handler receives a function to send messages directly to the client.
type ReconnectHandler func(sendToClient func(msg Message))

// TokenValidator validates authentication tokens for WebSocket connections.
// Returns the device ID if the token is valid, or an error if not.
type TokenValidator func(token string) (deviceID string, err error)

// DeviceActivityTracker is called to update device activity timestamps.
// The server calls this when a message is received from an authenticated client.
type DeviceActivityTracker func(deviceID string)

// UndoHandler processes undo requests for file-level cards.
// It reverses a previous accept/reject decision, restoring the card to pending.
// Returns the restored card for re-emission to clients.
// Phase 20.2: Enables undo flow from mobile.
type UndoHandler func(cardID string, confirmed bool) (*UndoResult, error)

// ChunkUndoHandler processes undo requests for chunk-level decisions.
// It reverses a previous chunk accept/reject decision.
// Returns the restored chunk info for re-emission to clients.
// Phase 20.2: Enables per-chunk undo flow from mobile.
type ChunkUndoHandler func(cardID string, chunkIndex int, confirmed bool) (*UndoResult, error)

// UndoResult carries the restored card/chunk information after a successful undo.
// This is used to re-emit the card to connected clients.
type UndoResult struct {
	CardID       string
	ChunkIndex   int // -1 for file-level undo
	File         string
	Diff         string
	OriginalDiff string // Full diff for card re-emission
}

// Server manages WebSocket connections and broadcasts messages to clients.
// It handles multiple concurrent clients and ensures messages are delivered
// to all connected clients without blocking the sender.
type Server struct {
	// addr is the address to listen on (e.g., "127.0.0.1:7070")
	addr string

	// upgrader converts HTTP connections to WebSocket connections.
	// We configure it to accept connections from any origin for development.
	upgrader websocket.Upgrader

	// clients tracks all connected WebSocket clients.
	// The map key is a pointer to the client, value is always true.
	// Using a map makes add/remove O(1) operations.
	clients map[*Client]bool

	// mu protects the clients map and stopped flag from concurrent access.
	mu sync.RWMutex

	// stopped indicates whether the server has been stopped.
	// This prevents sending to a closed broadcast channel.
	stopped bool

	// broadcast receives messages to send to all clients.
	// Using a channel decouples message production from delivery.
	broadcast chan Message

	// httpServer is the underlying HTTP server for graceful shutdown.
	httpServer *http.Server

	// sessionID is the current session identifier.
	// For now we only support one session at a time.
	sessionID string

	// decisionHandler is called when a client sends a review.decision message.
	// If nil, decisions are logged but not processed.
	decisionHandler DecisionHandler

	// chunkDecisionHandler is called when a client sends a chunk.decision message.
	// If nil, chunk decisions are logged but not processed.
	chunkDecisionHandler ChunkDecisionHandler

	// deleteHandler is called when a client sends a review.delete message.
	// If nil, delete requests are logged but not processed.
	deleteHandler DeleteHandler

	// undoHandler is called when a client sends a review.undo message.
	// If nil, undo requests are logged but not processed.
	// Phase 20.2: Enables undo flow from mobile.
	undoHandler UndoHandler

	// chunkUndoHandler is called when a client sends a chunk.undo message.
	// If nil, chunk undo requests are logged but not processed.
	// Phase 20.2: Enables per-chunk undo flow from mobile.
	chunkUndoHandler ChunkUndoHandler

	// reconnectHandler is called when a new client connects to replay history.
	// If nil, no history is replayed on connect.
	reconnectHandler ReconnectHandler

	// tokenValidator validates tokens for WebSocket authentication.
	// If nil, authentication is disabled (open access).
	tokenValidator TokenValidator

	// requireAuth controls whether authentication is required for WebSocket connections.
	// When true and tokenValidator is set, connections without valid tokens are rejected.
	requireAuth bool

	// pairHandler handles the /pair endpoint for code-to-token exchange.
	// Set via SetPairHandler.
	pairHandler http.Handler

	// generateCodeHandler handles the /pair/generate endpoint.
	// Set via SetGenerateCodeHandler.
	generateCodeHandler http.Handler

	// revokeDeviceHandler handles the /devices/{id}/revoke endpoint.
	// Set via SetRevokeDeviceHandler.
	revokeDeviceHandler http.Handler

	// deviceActivityTracker is called when a message is received from an
	// authenticated client. This allows updating last_seen timestamps.
	deviceActivityTracker DeviceActivityTracker

	// ptyWriter is the PTY session for writing terminal input.
	// Set via SetPTYWriter. If nil, terminal input messages are rejected.
	// Phase 5.5: Enables bidirectional terminal from mobile.
	ptyWriter io.Writer

	// approvalQueue manages pending approval requests from CLI tools.
	// Set via SetApprovalQueue. If nil, approval decisions are rejected.
	// Phase 6: Enables CLI command approval through mobile app.
	approvalQueue *ApprovalQueue

	// approveHandler handles the /approve endpoint for CLI approval requests.
	// Set via SetApproveHandler.
	// Phase 6.1b: HTTP endpoint for CLI command approval.
	approveHandler http.Handler

	// gitOps handles git operations for commit/push workflows.
	// Set via SetGitOperations. If nil, repo messages are rejected.
	// Phase 6.4: Enables commit workflow from mobile.
	gitOps *GitOperations

	// decidedStore provides access to decided cards for commit association.
	// Set via SetDecidedStore. Used to mark accepted cards as committed.
	// Phase 20.2: Enables commit association for undo support.
	decidedStore storage.DecidedCardStore

	// statusHandler handles the /status endpoint for CLI status queries.
	// Set via SetStatusHandler. Provides host info like uptime, clients, etc.
	// Phase 7.5: Enables "pseudocoder host status" CLI command.
	statusHandler http.Handler

	// sessionManager manages multiple concurrent PTY sessions.
	// Set via SetSessionManager. If nil, session management messages are rejected.
	// Phase 9.3: Enables multi-session PTY support from mobile.
	sessionManager *pty.SessionManager

	// sessionAPIHandler handles the /api/session/ endpoints for CLI session management.
	// Set via SetSessionAPIHandler.
	// Phase 9.5: Enables CLI session commands (new, list, kill, rename).
	sessionAPIHandler http.Handler

	// tmuxManager handles tmux session discovery and attachment.
	// Set via SetTmuxManager. If nil, tmux messages are rejected.
	// Phase 12: Enables tmux session integration from mobile.
	tmuxManager *tmux.Manager

	// tmuxAPIHandler handles the /api/tmux/ endpoints for CLI tmux management.
	// Set via SetTmuxAPIHandler.
	// Phase 12.9: Enables CLI tmux commands (list-tmux, attach-tmux, detach).
	tmuxAPIHandler http.Handler
}

// Client represents a single WebSocket connection.
// Each client has its own goroutine for writing messages,
// which prevents slow clients from blocking the broadcast.
type Client struct {
	// conn is the underlying WebSocket connection.
	conn *websocket.Conn

	// send is a buffered channel for outgoing messages.
	// The write goroutine reads from this and sends to the WebSocket.
	// Buffering prevents blocking when the client is slow.
	send chan Message

	// done is closed to signal the client should shut down.
	// Used to coordinate clean shutdown without racing on send channel.
	done chan struct{}

	// sendOnce ensures the send channel is only closed once.
	// Both Stop() and readPump() may try to close it, so we use
	// sync.Once to prevent a "close of closed channel" panic.
	sendOnce sync.Once

	// server is a reference back to the parent server.
	server *Server

	// deviceID is the ID of the paired device for this connection.
	// Set during WebSocket upgrade if authentication is enabled.
	// Empty string means unauthenticated (allowed when requireAuth is false).
	deviceID string

	// inputLimiter rate-limits terminal input messages to prevent flooding.
	// Configured at 1000 messages/sec with a burst of 10.
	// Phase 5.5: Protects PTY from excessive input.
	inputLimiter *rate.Limiter

	// activeSessionID is the PTY session this client is currently viewing.
	// Empty string means the client is viewing the default/main session.
	// Used for session.switch to replay correct terminal buffer.
	// Phase 9.3: Multi-session PTY support.
	//
	// Threading model:
	// - Written only by the client's own readPump goroutine (in handleSessionSwitch)
	// - Read by handleSessionClose when resetting clients viewing a closed session
	//   (protected by server.mu during iteration over clients map)
	// - This is safe because writes happen in a single goroutine and reads are
	//   protected by server.mu when accessing from other goroutines.
	activeSessionID string
}

// closeSend safely signals the client to shut down exactly once.
// This is safe to call multiple times from different goroutines.
// We only close the done channel (not send) to avoid racing with
// ongoing send operations. All senders check done before sending.
func (c *Client) closeSend() {
	c.sendOnce.Do(func() {
		close(c.done)
	})
}

// NewServer creates a new WebSocket server.
// Call Start() to begin accepting connections.
func NewServer(addr string) *Server {
	return &Server{
		addr:      addr,
		clients:   make(map[*Client]bool),
		broadcast: make(chan Message, channelBufferSize),
		sessionID: fmt.Sprintf("session-%d", time.Now().Unix()),
		upgrader: websocket.Upgrader{
			// Allow connections from any origin during development.
			// In production with TLS and auth, this is less critical.
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			// Buffer sizes for reading and writing WebSocket frames.
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	}
}

// Start begins listening for WebSocket connections.
// This method blocks, so call it in a goroutine if you need to do other work.
// For non-blocking startup with error handling, use StartAsync() instead.
func (s *Server) Start() error {
	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the HTTP server
	s.httpServer = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	// Start the broadcast goroutine that sends messages to all clients
	go s.runBroadcaster()

	log.Printf("WebSocket server listening on %s", s.addr)

	// ListenAndServe blocks until the server is stopped or an error occurs.
	// It returns http.ErrServerClosed on graceful shutdown.
	return s.httpServer.ListenAndServe()
}

// createMux creates the HTTP mux with all endpoints.
func (s *Server) createMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Handle WebSocket connections at the /ws endpoint
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Health check endpoint for monitoring
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Pairing endpoints
	s.mu.RLock()
	pairHandler := s.pairHandler
	generateCodeHandler := s.generateCodeHandler
	revokeHandler := s.revokeDeviceHandler
	approveHandler := s.approveHandler
	statusHandler := s.statusHandler
	s.mu.RUnlock()

	if pairHandler != nil {
		mux.Handle("/pair", pairHandler)
		log.Printf("Pairing endpoint registered at /pair")
	}

	if generateCodeHandler != nil {
		mux.Handle("/pair/generate", generateCodeHandler)
		log.Printf("Generate code endpoint registered at /pair/generate")
	}

	// Device revocation endpoint: /devices/{id}/revoke
	// This allows the CLI to signal the running host to close connections
	// for revoked devices immediately.
	if revokeHandler != nil {
		mux.Handle("/devices/", revokeHandler)
		log.Printf("Device revocation endpoint registered at /devices/{id}/revoke")
	}

	// CLI approval endpoint: /approve
	// This allows CLI tools to request approval for commands via HTTP.
	// Phase 6.1b: Enables CLI command approval broker.
	if approveHandler != nil {
		mux.Handle("/approve", approveHandler)
		log.Printf("Approval endpoint registered at /approve")
	}

	// Status endpoint: /status
	// This allows the CLI to query host status (uptime, clients, etc).
	// Phase 7.5: Enables "pseudocoder host status" CLI command.
	if statusHandler != nil {
		mux.Handle("/status", statusHandler)
		log.Printf("Status endpoint registered at /status")
	}

	// Session API endpoints: /api/session/*
	// This allows the CLI to manage sessions (new, list, kill, rename).
	// Phase 9.5: Enables CLI session commands.
	sessionAPIHandler := s.sessionAPIHandler
	if sessionAPIHandler != nil {
		mux.Handle("/api/session/", sessionAPIHandler)
		log.Printf("Session API registered at /api/session/")
	}

	// Tmux API endpoints: /api/tmux/*
	// This allows the CLI to manage tmux sessions (list-tmux, attach-tmux, detach).
	// Phase 12.9: Enables CLI tmux commands.
	tmuxAPIHandler := s.tmuxAPIHandler
	if tmuxAPIHandler != nil {
		mux.Handle("/api/tmux/", tmuxAPIHandler)
		log.Printf("Tmux API registered at /api/tmux/")
	}

	return mux
}

// StartAsync starts the server in a goroutine and returns any startup errors.
// This is useful when you need to verify the server started successfully
// before proceeding with other initialization (e.g., starting a PTY session).
//
// The returned channel receives nil if startup succeeded, or an error if
// the listener could not be created (e.g., port already in use).
// After receiving from the channel, the server is either running or failed.
func (s *Server) StartAsync() <-chan error {
	errCh := make(chan error, 1)

	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the listener first to detect port conflicts immediately.
	// net.Listen returns an error if the port is already in use.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		errCh <- fmt.Errorf("failed to listen on %s: %w", s.addr, err)
		close(errCh)
		return errCh
	}

	// Create the HTTP server
	s.httpServer = &http.Server{
		Handler: mux,
	}

	// Start the broadcast goroutine
	go s.runBroadcaster()

	// Start serving in a goroutine
	go func() {
		log.Printf("WebSocket server listening on %s", s.addr)
		// Signal successful startup
		errCh <- nil
		close(errCh)

		// Serve blocks until the server is stopped
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("WebSocket server error: %v", err)
		}
	}()

	return errCh
}

// TLSConfig holds the TLS configuration for the server.
type TLSConfig struct {
	// CertPath is the path to the TLS certificate file.
	CertPath string
	// KeyPath is the path to the TLS private key file.
	KeyPath string
}

// StartAsyncTLS starts the server with TLS in a goroutine and returns any startup errors.
// This is the TLS-enabled version of StartAsync. When TLS is configured, the server
// only accepts HTTPS/WSS connections, rejecting any plaintext HTTP/WS attempts.
//
// The returned channel receives nil if startup succeeded, or an error if
// the listener could not be created or TLS configuration failed.
func (s *Server) StartAsyncTLS(tlsCfg TLSConfig) <-chan error {
	errCh := make(chan error, 1)

	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the listener first to detect port conflicts immediately.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		errCh <- fmt.Errorf("failed to listen on %s: %w", s.addr, err)
		close(errCh)
		return errCh
	}

	// Load TLS certificate and key
	cert, err := tls.LoadX509KeyPair(tlsCfg.CertPath, tlsCfg.KeyPath)
	if err != nil {
		ln.Close()
		errCh <- fmt.Errorf("failed to load TLS certificate: %w", err)
		close(errCh)
		return errCh
	}

	// Configure TLS with secure defaults.
	// MinVersion TLS 1.2 is widely supported and excludes older insecure versions.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Wrap the listener with TLS
	tlsLn := tls.NewListener(ln, tlsConfig)

	// Create the HTTP server
	s.httpServer = &http.Server{
		Handler: mux,
	}

	// Start the broadcast goroutine
	go s.runBroadcaster()

	// Start serving in a goroutine
	go func() {
		log.Printf("WebSocket server listening on %s (TLS enabled)", s.addr)
		// Signal successful startup
		errCh <- nil
		close(errCh)

		// Serve blocks until the server is stopped.
		// With TLS, only encrypted connections are accepted.
		if err := s.httpServer.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
			log.Printf("WebSocket server error: %v", err)
		}
	}()

	return errCh
}

// Stop gracefully shuts down the server.
// It sends close frames to all clients, closes connections, and stops
// accepting new ones. This also closes the broadcast channel to allow
// the runBroadcaster goroutine to exit cleanly.
func (s *Server) Stop() error {
	s.mu.Lock()

	// Mark server as stopped to prevent new broadcasts
	if s.stopped {
		s.mu.Unlock()
		return nil // Already stopped
	}
	s.stopped = true

	// Signal all clients to stop by closing their send channels.
	// writePump will send a close frame and close the connection when
	// it sees the closed channel. We don't write directly here to avoid
	// racing with writePump.
	for client := range s.clients {
		// Close the send channel to signal writePump to exit.
		// writePump will send the close frame and close the connection.
		client.closeSend()
	}

	// Clear the clients map
	s.clients = make(map[*Client]bool)

	// Close the broadcast channel to allow runBroadcaster to exit.
	// This must happen after setting stopped=true to prevent panics
	// from concurrent Broadcast() calls.
	close(s.broadcast)

	s.mu.Unlock()

	// Shutdown the HTTP server
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// Broadcast sends a message to all connected clients.
// This method is non-blocking; messages are queued for delivery.
// If the server has been stopped, this method does nothing.
func (s *Server) Broadcast(msg Message) {
	// Hold RLock while checking stopped AND sending to avoid race with Stop().
	// Stop() takes the write lock, sets stopped=true, then closes the channel.
	// By holding RLock through the send, we ensure the channel can't be closed
	// while we're sending to it.
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.stopped {
		return
	}

	// Use select with default to make this non-blocking.
	// If the broadcast channel is full, we log and drop the message
	// rather than blocking the caller (the PTY output goroutine).
	select {
	case s.broadcast <- msg:
	default:
		log.Printf("Warning: broadcast channel full, dropping message")
	}
}

// BroadcastTerminalOutput is a convenience method for sending terminal output.
// This is the main method called from the PTY session's OnOutput callback.
func (s *Server) BroadcastTerminalOutput(chunk string) {
	s.Broadcast(NewTerminalAppendMessage(s.sessionID, chunk))
}

// BroadcastTerminalOutputWithID sends a terminal output chunk with an explicit session ID.
// This is used by the SessionManager to route output from multiple sessions.
// Phase 9.5: Multi-session terminal output broadcasting.
func (s *Server) BroadcastTerminalOutputWithID(sessionID, chunk string) {
	s.Broadcast(NewTerminalAppendMessage(sessionID, chunk))
}

// BroadcastDiffCard sends a review card to all connected clients.
// This is called when the diff poller detects new or changed chunks.
// The card is sent as a diff.card message per the WebSocket protocol.
// The chunks parameter provides per-chunk metadata for granular decisions.
// The chunkGroups parameter provides proximity grouping metadata (nil when disabled).
// The isBinary flag indicates per-chunk actions should be disabled.
// The isDeleted flag indicates this is a file deletion (use file-level actions).
// The stats parameter provides size metrics for large diff warnings.
func (s *Server) BroadcastDiffCard(cardID, file, diff string, chunks []stream.ChunkInfo, chunkGroups []stream.ChunkGroupInfo, isBinary, isDeleted bool, stats *stream.DiffStats, createdAt int64) {
	// Convert stream.ChunkInfo to server.ChunkInfo
	serverChunks := make([]ChunkInfo, len(chunks))
	for i, h := range chunks {
		serverChunks[i] = ChunkInfo{
			Index:       h.Index,
			OldStart:    h.OldStart,
			OldCount:    h.OldCount,
			NewStart:    h.NewStart,
			NewCount:    h.NewCount,
			Offset:      h.Offset,
			Length:      h.Length,
			Content:     h.Content,
			ContentHash: h.ContentHash,
			GroupIndex:  h.GroupIndex,
		}
	}

	// Convert stream.ChunkGroupInfo to server.ChunkGroupInfo
	var serverChunkGroups []ChunkGroupInfo
	if chunkGroups != nil {
		serverChunkGroups = make([]ChunkGroupInfo, len(chunkGroups))
		for i, g := range chunkGroups {
			serverChunkGroups[i] = ChunkGroupInfo{
				GroupIndex: g.GroupIndex,
				LineStart:  g.LineStart,
				LineEnd:    g.LineEnd,
				ChunkCount: g.ChunkCount,
			}
		}
	}

	// Convert stream.DiffStats to server.DiffStats
	var serverStats *DiffStats
	if stats != nil {
		serverStats = &DiffStats{
			ByteSize:     stats.ByteSize,
			LineCount:    stats.LineCount,
			AddedLines:   stats.AddedLines,
			DeletedLines: stats.DeletedLines,
		}
	}

	s.Broadcast(NewDiffCardMessage(cardID, file, diff, serverChunks, serverChunkGroups, isBinary, isDeleted, serverStats, createdAt))
}

// BroadcastCardRemoved notifies clients that a card was removed.
// This is called when changes are staged/reverted externally (e.g., VS Code).
func (s *Server) BroadcastCardRemoved(cardID string) {
	s.Broadcast(NewCardRemovedMessage(cardID))
}

// SessionID returns the current session identifier.
func (s *Server) SessionID() string {
	return s.sessionID
}

// Addr returns the server's listening address.
// This is used by the status handler to report the configured address.
func (s *Server) Addr() string {
	return s.addr
}

// ClientCount returns the number of connected clients.
func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// CloseDeviceConnections closes all active WebSocket connections for the given device.
// This is called when a device is revoked to immediately terminate access.
// Returns the number of connections that were closed. Thread-safe.
func (s *Server) CloseDeviceConnections(deviceID string) int {
	if deviceID == "" {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var closed int
	for client := range s.clients {
		if client.deviceID == deviceID {
			client.closeSend()
			closed++
			log.Printf("Closed connection for revoked device %s", deviceID)
		}
	}

	return closed
}

// SetDecisionHandler sets the callback for processing review decisions.
// This should be called before any clients connect. The handler is called
// when a client sends a review.decision message with action "accept" or "reject".
func (s *Server) SetDecisionHandler(handler DecisionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisionHandler = handler
}

// SetChunkDecisionHandler sets the callback for processing per-chunk decisions.
// This should be called before any clients connect. The handler is called
// when a client sends a chunk.decision message for a specific chunk within a card.
func (s *Server) SetChunkDecisionHandler(handler ChunkDecisionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkDecisionHandler = handler
}

// SetDeleteHandler sets the callback for processing file deletion requests.
// This should be called before any clients connect. The handler is called
// when a client sends a review.delete message for an untracked file.
func (s *Server) SetDeleteHandler(handler DeleteHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteHandler = handler
}

// SetUndoHandler sets the callback for processing file-level undo requests.
// The handler is called when a client sends a review.undo message.
// It should reverse the decision and return the restored card info.
// Phase 20.2: Enables undo flow from mobile.
func (s *Server) SetUndoHandler(handler UndoHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.undoHandler = handler
}

// SetChunkUndoHandler sets the callback for processing chunk-level undo requests.
// The handler is called when a client sends a chunk.undo message.
// It should reverse the chunk decision and return the restored chunk info.
// Phase 20.2: Enables per-chunk undo flow from mobile.
func (s *Server) SetChunkUndoHandler(handler ChunkUndoHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkUndoHandler = handler
}

// SetReconnectHandler sets the callback for replaying history on client connect.
// This handler is called each time a new client connects, allowing the server
// to send terminal history and pending review cards to bring the client up to date.
// The handler receives a function to send messages directly to the connecting client.
func (s *Server) SetReconnectHandler(handler ReconnectHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnectHandler = handler
}

// SetTokenValidator sets the token validation function for WebSocket auth.
// When requireAuth is true, connections without valid tokens are rejected.
func (s *Server) SetTokenValidator(validator TokenValidator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenValidator = validator
}

// SetRequireAuth controls whether authentication is required.
// When true, all WebSocket connections must provide a valid token.
func (s *Server) SetRequireAuth(require bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireAuth = require
}

// SetPairHandler sets the HTTP handler for the /pair endpoint.
// This must be called before Start() or StartAsync().
func (s *Server) SetPairHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairHandler = handler
}

// SetGenerateCodeHandler sets the HTTP handler for the /pair/generate endpoint.
// This must be called before Start() or StartAsync().
func (s *Server) SetGenerateCodeHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generateCodeHandler = handler
}

// SetRevokeDeviceHandler sets the HTTP handler for the /devices/{id}/revoke endpoint.
// This endpoint allows the CLI to signal the running host to close connections
// for revoked devices. Must be called before Start() or StartAsync().
func (s *Server) SetRevokeDeviceHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeDeviceHandler = handler
}

// SetDeviceActivityTracker sets the callback for tracking device activity.
// This is called when a message is received from an authenticated client,
// allowing the application to update last_seen timestamps.
func (s *Server) SetDeviceActivityTracker(tracker DeviceActivityTracker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceActivityTracker = tracker
}

// SetPTYWriter sets the PTY session for writing terminal input.
// This enables bidirectional terminal: mobile clients can send input
// to the PTY running on the host. Phase 5.5.
func (s *Server) SetPTYWriter(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptyWriter = w
}

// SetApprovalQueue sets the approval queue for CLI command approval.
// This enables mobile clients to approve or deny commands requested
// by CLI tools through the approval broker. Phase 6.
func (s *Server) SetApprovalQueue(queue *ApprovalQueue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvalQueue = queue
}

// GetApprovalQueue returns the approval queue for external access.
// This allows the HTTP endpoint to submit approval requests.
func (s *Server) GetApprovalQueue() *ApprovalQueue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.approvalQueue
}

// SetApproveHandler sets the HTTP handler for the /approve endpoint.
// This endpoint allows CLI tools to request command approval via HTTP.
// Phase 6.1b: HTTP endpoint for CLI command approval broker.
func (s *Server) SetApproveHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approveHandler = handler
}

// SetGitOperations sets the git operations handler for commit workflows.
// This enables mobile clients to query repository status and create commits.
// Phase 6.4: Enables commit workflow from mobile.
func (s *Server) SetGitOperations(ops *GitOperations) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gitOps = ops
}

// SetDecidedStore sets the decided card store for commit association.
// After a successful commit, accepted cards are marked as committed using this store.
// Phase 20.2: Enables commit association for undo support.
func (s *Server) SetDecidedStore(store storage.DecidedCardStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decidedStore = store
}

// SetStatusHandler sets the HTTP handler for the /status endpoint.
// This endpoint allows the CLI to query host status (uptime, clients, etc).
// Phase 7.5: Enables "pseudocoder host status" CLI command.
func (s *Server) SetStatusHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusHandler = handler
}

// SetSessionManager sets the PTY session manager for multi-session support.
// This enables mobile clients to create, close, and switch between multiple
// terminal sessions. Phase 9.3: Multi-session PTY management.
func (s *Server) SetSessionManager(mgr *pty.SessionManager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionManager = mgr
}

// GetSessionManager returns the session manager for external access.
// This allows the host command to manage sessions directly.
func (s *Server) GetSessionManager() *pty.SessionManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionManager
}

// SetSessionAPIHandler sets the HTTP handler for the /api/session/ endpoints.
// This enables CLI commands for session management (new, list, kill, rename).
// Phase 9.5: Enables CLI session commands.
func (s *Server) SetSessionAPIHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionAPIHandler = handler
}

// SetTmuxManager sets the tmux manager for session integration.
// This enables mobile clients to list and attach to tmux sessions.
// Phase 12: tmux session integration.
func (s *Server) SetTmuxManager(mgr *tmux.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tmuxManager = mgr
}

// GetTmuxManager returns the tmux manager for external access.
func (s *Server) GetTmuxManager() *tmux.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tmuxManager
}

// SetTmuxAPIHandler sets the HTTP handler for the /api/tmux/ endpoints.
// This enables CLI commands for tmux session management (list-tmux, attach-tmux, detach).
// Phase 12.9: Enables CLI tmux commands.
func (s *Server) SetTmuxAPIHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tmuxAPIHandler = handler
}

// handleWebSocket upgrades an HTTP connection to a WebSocket connection.
// This is called by the HTTP server for each new connection to /ws.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check authentication if required
	s.mu.RLock()
	requireAuth := s.requireAuth
	tokenValidator := s.tokenValidator
	s.mu.RUnlock()

	var deviceID string

	if requireAuth && tokenValidator != nil {
		// Extract token from Authorization header
		// Expected format: "Bearer <token>"
		token := extractBearerToken(r)
		if token == "" {
			log.Printf("WebSocket connection rejected: missing authorization token")
			http.Error(w, "Unauthorized: missing token", http.StatusUnauthorized)
			return
		}

		// Validate the token
		var err error
		deviceID, err = tokenValidator(token)
		if err != nil {
			log.Printf("WebSocket connection rejected: invalid token: %v", err)
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}

		log.Printf("WebSocket connection authenticated for device %s", deviceID)
	}

	// Upgrade the HTTP connection to a WebSocket connection.
	// This performs the WebSocket handshake (HTTP 101 Switching Protocols).
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// Create a new client with a buffered send channel.
	// The buffer allows the client to fall behind temporarily
	// without blocking the broadcaster.
	client := &Client{
		conn:     conn,
		send:     make(chan Message, channelBufferSize),
		done:     make(chan struct{}),
		server:   s,
		deviceID: deviceID,
		// Rate limiter for terminal input: 1000 messages/sec with burst of 10.
		// This prevents mobile clients from flooding the PTY with input.
		inputLimiter: rate.NewLimiter(rate.Limit(1000), 10),
	}

	// Register the client
	s.mu.Lock()
	s.clients[client] = true
	s.mu.Unlock()

	log.Printf("Client connected (%d total)", s.ClientCount())

	// Send a session status message to the new client
	client.send <- NewSessionStatusMessage(s.sessionID, "running")

	// Start writePump BEFORE calling reconnect handler. This is critical because
	// the reconnect handler may send thousands of messages (terminal history lines
	// and pending cards). If writePump isn't running, the channel buffer (256 msgs)
	// fills up and most history is dropped. With writePump running, it actively
	// drains the channel while the handler adds messages.
	go client.writePump()

	// Call the reconnect handler to replay history and pending cards.
	// writePump is already running, so it will drain messages as we add them.
	s.mu.RLock()
	reconnectHandler := s.reconnectHandler
	s.mu.RUnlock()

	if reconnectHandler != nil {
		// Create a function that sends directly to this client's send channel.
		// This avoids broadcasting to all clients.
		//
		// Unlike regular broadcasts which use non-blocking sends (and may drop
		// messages for slow clients), history replay uses a BLOCKING send with
		// a timeout. This ensures all history is delivered as long as the client
		// is responsive. We use a generous timeout (5s per message) to handle
		// temporary slowdowns while still failing fast for dead clients.
		sendToClient := func(msg Message) {
			select {
			case <-client.done:
				// Client is shutting down - skip message
				return
			case client.send <- msg:
				// Message queued successfully
			case <-time.After(5 * time.Second):
				// Client is too slow or dead - give up on this message
				log.Printf("Warning: timeout sending history to client, skipping message")
			}
		}
		reconnectHandler(sendToClient)
	}

	// Start readPump for receiving client messages.
	go client.readPump()
}

// extractBearerToken extracts the token from an Authorization header.
// Returns empty string if no valid bearer token is found.
// Supports both "Bearer <token>" header and "token" query parameter as fallback.
func extractBearerToken(r *http.Request) string {
	// Try Authorization header first
	auth := r.Header.Get("Authorization")
	if auth != "" {
		// Check for "Bearer " prefix (case-insensitive)
		const bearerPrefix = "Bearer "
		if len(auth) > len(bearerPrefix) {
			prefix := auth[:len(bearerPrefix)]
			if prefix == bearerPrefix || prefix == "bearer " {
				return auth[len(bearerPrefix):]
			}
		}
	}

	// Fallback to query parameter for WebSocket connections
	// (some WebSocket clients don't support custom headers)
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}

	return ""
}

// runBroadcaster reads from the broadcast channel and sends to all clients.
// This runs in its own goroutine started by Start().
func (s *Server) runBroadcaster() {
	for msg := range s.broadcast {
		s.mu.RLock()
		for client := range s.clients {
			// Try to send to each client, but don't block if their buffer is full
			// or if the client is shutting down.
			select {
			case <-client.done:
				// Client is shutting down - skip
			case client.send <- msg:
			default:
				// Client is too slow; we could disconnect them here,
				// but for now we just drop the message for this client.
				log.Printf("Warning: client send buffer full, dropping message")
			}
		}
		s.mu.RUnlock()
	}
}

// writePump continuously sends messages from the send channel to the WebSocket.
// It also sends periodic pings to keep the connection alive.
func (c *Client) writePump() {
	// Set up a ticker for sending pings every 30 seconds.
	// Pings help detect dead connections and keep NAT/firewalls happy.
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case <-c.done:
			// Shutdown signaled; send close frame and exit.
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case msg, ok := <-c.send:
			// Set a write deadline to prevent hanging on slow connections
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			if !ok {
				// The send channel was closed; send a close frame.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Serialize the message to JSON
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Failed to marshal message: %v", err)
				continue
			}

			// Write the message to the WebSocket
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("Write error: %v", err)
				return
			}

		case <-ticker.C:
			// Send a ping to keep the connection alive
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads messages from the WebSocket and handles them.
// For now, we mainly use this to detect when the client disconnects.
// In future units, this will handle review.decision messages from clients.
func (c *Client) readPump() {
	defer func() {
		// Unregister the client when this goroutine exits
		c.server.mu.Lock()
		delete(c.server.clients, c)
		c.server.mu.Unlock()

		// Use closeSend() to safely close the channel.
		// Stop() may have already closed it during shutdown.
		// This signals writePump to exit, which will close the connection.
		c.closeSend()

		log.Printf("Client disconnected (%d remaining)", c.server.ClientCount())
	}()

	// Configure connection parameters
	c.conn.SetReadLimit(512 * 1024) // Max message size: 512KB
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Set up pong handler to reset the read deadline.
	// When we receive a pong (response to our ping), we know the client is alive.
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		// Read the next message from the WebSocket.
		// This blocks until a message arrives or an error occurs.
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// Check if this is a normal close (client disconnected)
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure) {
				log.Printf("Read error: %v", err)
			}
			return
		}

		// Track device activity for authenticated clients.
		// This updates the last_seen timestamp on each message received.
		if c.deviceID != "" {
			c.server.mu.RLock()
			tracker := c.server.deviceActivityTracker
			c.server.mu.RUnlock()

			if tracker != nil {
				tracker(c.deviceID)
			}
		}

		// Parse the message
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Failed to parse message: %v", err)
			continue
		}

		// Handle the message based on type
		switch msg.Type {
		case MessageTypeReviewDecision:
			c.handleReviewDecision(data)
		case MessageTypeChunkDecision:
			c.handleChunkDecision(data)
		case MessageTypeReviewDelete:
			c.handleReviewDelete(data)
		// Phase 20.2: Undo messages for review card state reversal
		case MessageTypeReviewUndo:
			c.handleReviewUndo(data)
		case MessageTypeChunkUndo:
			c.handleChunkUndo(data)
		case MessageTypeTerminalInput:
			c.handleTerminalInput(data)
		case MessageTypeTerminalResize:
			c.handleTerminalResize(data)
		case MessageTypeApprovalDecision:
			c.handleApprovalDecision(data)
		case MessageTypeRepoStatus:
			c.handleRepoStatus(data)
		case MessageTypeRepoCommit:
			c.handleRepoCommit(data)
		case MessageTypeRepoPush:
			c.handleRepoPush(data)
		// Phase 9.3: Multi-session PTY management messages
		case MessageTypeSessionCreate:
			c.handleSessionCreate(data)
		case MessageTypeSessionClose:
			c.handleSessionClose(data)
		case MessageTypeSessionSwitch:
			c.handleSessionSwitch(data)
		case MessageTypeSessionRename:
			c.handleSessionRename(data)
		// Phase 12: tmux session integration messages
		case MessageTypeTmuxList:
			c.handleTmuxList(data)
		case MessageTypeTmuxAttach:
			c.handleTmuxAttach(data)
		case MessageTypeTmuxDetach:
			c.handleTmuxDetach(data)
		default:
			log.Printf("Received message: type=%s", msg.Type)
		}
	}
}

// handleReviewDecision processes a review.decision message from the client.
// It validates the payload, calls the decision handler, and sends a result
// message back to the client with standardized error codes.
func (c *Client) handleReviewDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType           `json:"type"`
		ID      string                `json:"id,omitempty"`
		Payload ReviewDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.decision payload: %v", err)
		c.sendDecisionResult("", "", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.decision missing card_id")
		c.sendDecisionResult("", payload.Action, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if payload.Action != "accept" && payload.Action != "reject" {
		log.Printf("review.decision invalid action: %s", payload.Action)
		c.sendDecisionResult(payload.CardID, payload.Action, false,
			apperrors.CodeActionInvalid, "action must be 'accept' or 'reject'")
		return
	}

	// Get the decision handler from the server
	c.server.mu.RLock()
	handler := c.server.decisionHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No decision handler registered, ignoring decision for card %s", payload.CardID)
		c.sendDecisionResult(payload.CardID, payload.Action, false,
			apperrors.CodeServerHandlerMissing, "decision handler not configured")
		return
	}

	// Call the handler to apply the decision
	if err := handler(payload.CardID, payload.Action, payload.Comment); err != nil {
		log.Printf("Decision handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendDecisionResult(payload.CardID, payload.Action, false, code, message)
		return
	}

	log.Printf("Decision applied: card=%s action=%s", payload.CardID, payload.Action)

	// Broadcast the decision result to all connected clients so they can
	// update their UI (e.g., remove the card from pending list).
	// This reaches the sender too, so no separate direct send needed.
	c.server.Broadcast(NewDecisionResultMessage(payload.CardID, payload.Action, true, "", ""))
}

// sendDecisionResult sends a decision result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendDecisionResult(cardID, action string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewDecisionResultMessage(cardID, action, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping decision result")
	}
}

// handleChunkDecision processes a chunk.decision message from the client.
// It validates the payload, calls the chunk decision handler, and sends a result
// message back to the client with standardized error codes.
func (c *Client) handleChunkDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload ChunkDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse chunk.decision payload: %v", err)
		c.sendChunkDecisionResult("", -1, "", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("chunk.decision missing card_id")
		c.sendChunkDecisionResult("", payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if payload.ChunkIndex < 0 {
		log.Printf("chunk.decision invalid chunk_index: %d", payload.ChunkIndex)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerInvalidMessage, "chunk_index must be >= 0")
		return
	}

	if payload.Action != "accept" && payload.Action != "reject" {
		log.Printf("chunk.decision invalid action: %s", payload.Action)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeActionInvalid, "action must be 'accept' or 'reject'")
		return
	}

	// Get the chunk decision handler from the server
	c.server.mu.RLock()
	handler := c.server.chunkDecisionHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No chunk decision handler registered, ignoring decision for card %s chunk %d",
			payload.CardID, payload.ChunkIndex)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerHandlerMissing, "chunk decision handler not configured")
		return
	}

	// Call the handler to apply the decision
	// Pass contentHash for stale detection (empty means skip validation for backward compat)
	if err := handler(payload.CardID, payload.ChunkIndex, payload.Action, payload.ContentHash); err != nil {
		log.Printf("Chunk decision handler error for card %s chunk %d: %v",
			payload.CardID, payload.ChunkIndex, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false, code, message)
		return
	}

	log.Printf("Chunk decision applied: card=%s chunk=%d action=%s",
		payload.CardID, payload.ChunkIndex, payload.Action)

	// Broadcast the decision result to all connected clients so they can
	// update their UI (e.g., mark the chunk as decided).
	c.server.Broadcast(NewChunkDecisionResultMessage(
		payload.CardID, payload.ChunkIndex, payload.Action, true, "", ""))
}

// sendChunkDecisionResult sends a chunk decision result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendChunkDecisionResult(cardID string, chunkIndex int, action string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewChunkDecisionResultMessage(cardID, chunkIndex, action, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping chunk decision result")
	}
}

// handleReviewDelete processes a review.delete message from the client.
// This is used to delete untracked files that cannot be restored via git.
// It validates the payload, calls the delete handler, and sends a result.
func (c *Client) handleReviewDelete(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType         `json:"type"`
		ID      string              `json:"id,omitempty"`
		Payload ReviewDeletePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.delete payload: %v", err)
		c.sendDeleteResult("", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.delete missing card_id")
		c.sendDeleteResult("", false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if !payload.Confirmed {
		log.Printf("review.delete missing confirmation for card %s", payload.CardID)
		c.sendDeleteResult(payload.CardID, false,
			apperrors.CodeServerInvalidMessage, "deletion requires confirmed=true")
		return
	}

	// Get the delete handler from the server
	c.server.mu.RLock()
	handler := c.server.deleteHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No delete handler registered, ignoring delete for card %s", payload.CardID)
		c.sendDeleteResult(payload.CardID, false,
			apperrors.CodeServerHandlerMissing, "delete handler not configured")
		return
	}

	// Call the handler to delete the file
	if err := handler(payload.CardID); err != nil {
		log.Printf("Delete handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendDeleteResult(payload.CardID, false, code, message)
		return
	}

	log.Printf("File deleted: card=%s", payload.CardID)

	// Broadcast the delete result to all connected clients so they can
	// update their UI (e.g., remove the card from pending list).
	c.server.Broadcast(NewDeleteResultMessage(payload.CardID, true, "", ""))
}

// sendDeleteResult sends a delete result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendDeleteResult(cardID string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewDeleteResultMessage(cardID, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping delete result")
	}
}

// handleReviewUndo processes a review.undo message from the client.
// This reverses a previous accept/reject decision and restores the card to pending.
// Phase 20.2: Enables undo flow from mobile.
func (c *Client) handleReviewUndo(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload ReviewUndoPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.undo payload: %v", err)
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.undo missing card_id")
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	// Get the undo handler from the server
	c.server.mu.RLock()
	handler := c.server.undoHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No undo handler registered, ignoring undo for card %s", payload.CardID)
		c.sendUndoResult(payload.CardID, -1, false,
			apperrors.CodeServerHandlerMissing, "undo handler not configured")
		return
	}

	// Call the handler to undo the decision
	result, err := handler(payload.CardID, payload.Confirmed)
	if err != nil {
		log.Printf("Undo handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendUndoResult(payload.CardID, -1, false, code, message)
		return
	}

	log.Printf("Undo applied: card=%s file=%s", payload.CardID, result.File)

	// Re-emit the restored card to all clients so it reappears as pending.
	// Detect if the card is for a binary file based on the stored placeholder content.
	isBinary := result.OriginalDiff == diff.BinaryDiffPlaceholder

	// Parse chunk info from the original diff to reconstruct the card.
	// For binary files, chunks will be empty (which is correct - no per-chunk actions).
	var serverChunks []ChunkInfo
	var serverStats *DiffStats

	if !isBinary {
		chunkInfoList := stream.ParseChunkInfoFromDiff(result.OriginalDiff)
		serverChunks = make([]ChunkInfo, len(chunkInfoList))
		for i, h := range chunkInfoList {
			serverChunks[i] = ChunkInfo{
				Index:       h.Index,
				OldStart:    h.OldStart,
				OldCount:    h.OldCount,
				NewStart:    h.NewStart,
				NewCount:    h.NewCount,
				Offset:      h.Offset,
				Length:      h.Length,
				Content:     h.Content,
				ContentHash: h.ContentHash,
			}
		}

		// Calculate diff stats for the re-emitted card
		if len(result.OriginalDiff) > 0 {
			serverStats = &DiffStats{
				ByteSize:  len(result.OriginalDiff),
				LineCount: strings.Count(result.OriginalDiff, "\n") + 1,
			}
			// Count added/deleted lines
			for _, line := range strings.Split(result.OriginalDiff, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					serverStats.AddedLines++
				} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					serverStats.DeletedLines++
				}
			}
		}
	}

	// Detect deletion from stats: no added lines but has deleted lines.
	// This matches the heuristic used in StreamPendingCards.
	isDeleted := serverStats != nil && serverStats.AddedLines == 0 && serverStats.DeletedLines > 0

	// Broadcast the restored card to all clients
	c.server.Broadcast(NewDiffCardMessage(
		result.CardID,
		result.File,
		result.OriginalDiff,
		serverChunks,
		nil, // chunkGroups: not recomputed during undo
		isBinary,
		isDeleted,
		serverStats,
		time.Now().UnixMilli(),
	))

	// Broadcast success result to all clients
	c.server.Broadcast(NewUndoResultMessage(payload.CardID, -1, true, "", ""))
}

// handleChunkUndo processes a chunk.undo message from the client.
// This reverses a previous per-chunk accept/reject decision.
// Phase 20.2: Enables per-chunk undo flow from mobile.
func (c *Client) handleChunkUndo(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType      `json:"type"`
		ID      string           `json:"id,omitempty"`
		Payload ChunkUndoPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse chunk.undo payload: %v", err)
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("chunk.undo missing card_id")
		c.sendUndoResult("", payload.ChunkIndex, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	// Get the chunk undo handler from the server
	c.server.mu.RLock()
	handler := c.server.chunkUndoHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No chunk undo handler registered, ignoring undo for card %s chunk %d",
			payload.CardID, payload.ChunkIndex)
		c.sendUndoResult(payload.CardID, payload.ChunkIndex, false,
			apperrors.CodeServerHandlerMissing, "chunk undo handler not configured")
		return
	}

	// Call the handler to undo the chunk decision
	result, err := handler(payload.CardID, payload.ChunkIndex, payload.Confirmed)
	if err != nil {
		log.Printf("Chunk undo handler error for card %s chunk %d: %v",
			payload.CardID, payload.ChunkIndex, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendUndoResult(payload.CardID, payload.ChunkIndex, false, code, message)
		return
	}

	log.Printf("Chunk undo applied: card=%s chunk=%d file=%s",
		payload.CardID, payload.ChunkIndex, result.File)

	// Note: For chunk undos, we don't re-emit the full card immediately.
	// The card/chunk state transitions happen in storage, and the mobile
	// client will update its local state based on the undo result.
	// If the card needs to be re-emitted (all chunks now pending), the
	// handler should handle that via the broadcaster.

	// Broadcast success result to all clients
	c.server.Broadcast(NewUndoResultMessage(payload.CardID, payload.ChunkIndex, true, "", ""))
}

// sendUndoResult sends an undo result message to this client.
// For file-level undos, set chunkIndex to -1 (omitted from JSON).
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendUndoResult(cardID string, chunkIndex int, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewUndoResultMessage(cardID, chunkIndex, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping undo result")
	}
}

// handleTerminalInput processes a terminal.input message from the client.
// This enables bidirectional terminal: mobile clients can send keystrokes
// and commands to the PTY running on the host.
// Phase 5.5: Single session support.
// Phase 9.3: Multi-session routing via SessionManager.
func (c *Client) handleTerminalInput(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload TerminalInputPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse terminal.input payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	// Check rate limit to prevent flooding the PTY
	if !c.inputLimiter.Allow() {
		log.Printf("terminal.input rate limited for device %s", c.deviceID)
		c.sendError(apperrors.CodeInputRateLimited, "rate limit exceeded")
		return
	}

	// Get session manager and legacy pty writer from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	legacyWriter := c.server.ptyWriter
	legacySessionID := c.server.sessionID
	c.server.mu.RUnlock()

	// Phase 9.3: Try multi-session routing first if session manager is configured
	if mgr != nil && payload.SessionID != "" {
		session := mgr.Get(payload.SessionID)
		if session != nil {
			// Log input event without content for privacy
			if c.deviceID != "" {
				log.Printf("Terminal input: device=%s session=%s timestamp=%d bytes=%d",
					c.deviceID, payload.SessionID, payload.Timestamp, len(payload.Data))
			}

			// Write to the specific session
			if _, err := session.Write([]byte(payload.Data)); err != nil {
				log.Printf("Failed to write to session %s: %v", payload.SessionID, err)
				c.sendError(apperrors.CodeInputWriteFailed, "failed to write to terminal")
			}
			return
		}
		// Session not found in manager - return consistent error with other handlers.
		// Don't fall through to legacy mode when SessionManager is configured,
		// as that would give a misleading "session mismatch" error.
		log.Printf("Terminal input session not found: %s", payload.SessionID)
		c.sendError(apperrors.CodeSessionNotFound, "session not found")
		return
	}

	// Legacy single-session mode (backward compatibility)
	// Validate session_id matches current session
	if payload.SessionID != legacySessionID {
		log.Printf("terminal.input session mismatch: got %s, want %s",
			payload.SessionID, legacySessionID)
		c.sendError(apperrors.CodeInputSessionInvalid, "session ID mismatch")
		return
	}

	if legacyWriter == nil {
		log.Printf("No PTY writer configured, ignoring terminal input")
		c.sendError(apperrors.CodeServerHandlerMissing, "terminal input not available")
		return
	}

	// Log input event without content for privacy (only device_id, timestamp, size)
	if c.deviceID != "" {
		log.Printf("Terminal input: device=%s timestamp=%d bytes=%d",
			c.deviceID, payload.Timestamp, len(payload.Data))
	}

	// Write to legacy PTY
	if _, err := legacyWriter.Write([]byte(payload.Data)); err != nil {
		log.Printf("Failed to write to PTY: %v", err)
		c.sendError(apperrors.CodeInputWriteFailed, "failed to write to terminal")
		return
	}
}

// handleTerminalResize processes a terminal.resize message from the client.
// This allows mobile clients to notify the host when the terminal dimensions
// change (e.g., device rotation, keyboard show/hide, tablet window resize).
// The host then resizes the PTY, which sends SIGWINCH to the running process.
//
// Unlike terminal input, resize only routes through SessionManager (no legacy
// fallback) because proper multi-session support is required for this feature.
func (c *Client) handleTerminalResize(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType           `json:"type"`
		ID      string                `json:"id,omitempty"`
		Payload TerminalResizePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse terminal.resize payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	// Validate required fields
	if payload.SessionID == "" {
		log.Printf("terminal.resize missing session_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "session_id is required")
		return
	}

	if payload.Cols <= 0 || payload.Rows <= 0 {
		log.Printf("terminal.resize invalid dimensions: cols=%d rows=%d",
			payload.Cols, payload.Rows)
		c.sendError(apperrors.CodeServerInvalidMessage, "cols and rows must be > 0")
		return
	}

	// Get session manager from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("terminal.resize: session manager not configured")
		c.sendError(apperrors.CodeServerHandlerMissing, "session manager not available")
		return
	}

	// Find the session
	session := mgr.Get(payload.SessionID)
	if session == nil {
		log.Printf("terminal.resize: session not found: %s", payload.SessionID)
		c.sendError(apperrors.CodeSessionNotFound, "session not found")
		return
	}

	// Check if session is running
	if !session.IsRunning() {
		log.Printf("terminal.resize: session not running: %s", payload.SessionID)
		c.sendError(apperrors.CodeSessionNotRunning, "session not running")
		return
	}

	// Resize the PTY
	if err := session.Resize(payload.Cols, payload.Rows); err != nil {
		log.Printf("terminal.resize failed for session %s: %v", payload.SessionID, err)
		c.sendError(apperrors.CodeSessionWriteFailed, "resize failed")
		return
	}

	log.Printf("Terminal resize: session=%s cols=%d rows=%d",
		payload.SessionID, payload.Cols, payload.Rows)
}

// sendError sends an error message to this client.
// Uses non-blocking send to avoid blocking on slow clients.
func (c *Client) sendError(code, message string) {
	select {
	case c.send <- NewErrorMessage(code, message):
	default:
		log.Printf("Warning: client send buffer full, dropping error message")
	}
}

// handleApprovalDecision processes an approval.decision message from the client.
// This is the response to an approval.request message sent by a CLI tool.
// It validates the payload and forwards the decision to the approval queue.
func (c *Client) handleApprovalDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType             `json:"type"`
		ID      string                  `json:"id,omitempty"`
		Payload ApprovalDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse approval.decision payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	// Validate required fields
	if payload.RequestID == "" {
		log.Printf("approval.decision missing request_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	if payload.Decision != "approve" && payload.Decision != "deny" {
		log.Printf("approval.decision invalid decision: %s", payload.Decision)
		c.sendError(apperrors.CodeServerInvalidMessage, "decision must be 'approve' or 'deny'")
		return
	}

	// Get the approval queue from the server
	c.server.mu.RLock()
	queue := c.server.approvalQueue
	c.server.mu.RUnlock()

	if queue == nil {
		log.Printf("No approval queue configured, ignoring decision for request %s", payload.RequestID)
		c.sendError(apperrors.CodeServerHandlerMissing, "approval queue not configured")
		return
	}

	// Parse optional temporary allow until timestamp
	var tempAllowUntil *time.Time
	if payload.TemporaryAllowUntil != "" {
		t, err := time.Parse(time.RFC3339, payload.TemporaryAllowUntil)
		if err != nil {
			log.Printf("approval.decision invalid temporary_allow_until: %v", err)
			c.sendError(apperrors.CodeServerInvalidMessage, "invalid temporary_allow_until timestamp")
			return
		}
		tempAllowUntil = &t
	}

	// Forward decision to the queue with device ID for audit tracking
	if err := queue.DecideWithDevice(payload.RequestID, payload.Decision, tempAllowUntil, c.deviceID); err != nil {
		log.Printf("Approval decision error for request %s: %v", payload.RequestID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendError(code, message)
		return
	}

	log.Printf("Approval decision received: request=%s decision=%s device=%s", payload.RequestID, payload.Decision, c.deviceID)
}

// handleRepoStatus processes a repo.status message from the client.
// It returns the current git repository status including branch, staged/unstaged counts.
func (c *Client) handleRepoStatus(data []byte) {
	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.status request")
		c.sendError(apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Get repository status
	status, err := gitOps.GetRepoStatus()
	if err != nil {
		log.Printf("Failed to get repo status: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendError(code, message)
		return
	}

	// Send status to the requesting client only (not broadcast)
	msg := NewRepoStatusMessage(status.Branch, status.Upstream, status.StagedCount, status.StagedFiles, status.UnstagedCount, status.LastCommit)
	select {
	case <-c.done:
		return
	case c.send <- msg:
		log.Printf("Sent repo.status: branch=%s staged=%d unstaged=%d", status.Branch, status.StagedCount, status.UnstagedCount)
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending repo.status to client")
	}
}

// handleRepoCommit processes a repo.commit message from the client.
// It creates a git commit with the specified message and returns the result.
func (c *Client) handleRepoCommit(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload RepoCommitPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.commit message: %v", err)
		c.sendCommitResult(false, "", "", apperrors.CodeServerInvalidMessage, "invalid JSON")
		return
	}

	payload := msg.Payload

	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.commit request")
		c.sendCommitResult(false, "", "", apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Create the commit
	hash, summary, err := gitOps.Commit(payload.Message, payload.NoVerify, payload.NoGpgSign)
	if err != nil {
		log.Printf("Commit failed: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendCommitResult(false, "", "", code, message)
		return
	}

	log.Printf("Commit created: hash=%s summary=%s device=%s", hash, summary, c.deviceID)

	// Phase 20.2: Mark accepted cards as committed for undo support.
	// This associates the commit hash with any cards that were staged.
	c.server.mu.RLock()
	decidedStore := c.server.decidedStore
	c.server.mu.RUnlock()

	if decidedStore != nil {
		// Get all accepted (staged) file-level cards and mark them as committed
		acceptedCards, err := decidedStore.ListDecidedByStatus(storage.CardAccepted)
		if err != nil {
			log.Printf("repo.commit: failed to list accepted cards: %v", err)
		} else if len(acceptedCards) > 0 {
			// Collect card IDs to mark as committed
			cardIDs := make([]string, len(acceptedCards))
			for i, card := range acceptedCards {
				cardIDs[i] = card.ID
			}

			if err := decidedStore.MarkCardsCommitted(cardIDs, hash); err != nil {
				log.Printf("repo.commit: failed to mark cards as committed: %v", err)
			} else {
				log.Printf("repo.commit: marked %d cards as committed with hash %s", len(cardIDs), hash)
			}
		}

		// Mark any accepted chunks as committed.
		// IMPORTANT: We must iterate ALL decided cards, not just accepted ones.
		// For chunk-level decisions, the parent DecidedCard.Status reflects the
		// first chunk's decision, which may have been "rejected" even if later
		// chunks were accepted and staged. Those accepted chunks need to be
		// marked as committed regardless of the parent card's status.
		allCards, err := decidedStore.ListAllDecidedCards()
		if err != nil {
			log.Printf("repo.commit: failed to list all decided cards for chunk processing: %v", err)
		} else {
			for _, card := range allCards {
				chunks, err := decidedStore.GetDecidedChunks(card.ID)
				if err != nil {
					log.Printf("repo.commit: failed to get chunks for card %s: %v", card.ID, err)
					continue
				}

				// Collect accepted chunk indexes
				var acceptedIndexes []int
				for _, chunk := range chunks {
					if chunk.Status == storage.CardAccepted {
						acceptedIndexes = append(acceptedIndexes, chunk.ChunkIndex)
					}
				}

				if len(acceptedIndexes) > 0 {
					if err := decidedStore.MarkChunksCommitted(card.ID, acceptedIndexes, hash); err != nil {
						log.Printf("repo.commit: failed to mark chunks as committed for card %s: %v", card.ID, err)
					}
				}
			}
		}
	}

	// Broadcast success to all clients
	c.server.Broadcast(NewRepoCommitResultMessage(true, hash, summary, "", ""))
}

// sendCommitResult sends a repo.commit_result message to the client.
func (c *Client) sendCommitResult(success bool, hash, summary, errCode, errMsg string) {
	msg := NewRepoCommitResultMessage(success, hash, summary, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping commit result")
	}
}

// handleRepoPush processes a repo.push message from the client.
// It pushes commits to the remote and returns the result.
func (c *Client) handleRepoPush(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType     `json:"type"`
		ID      string          `json:"id,omitempty"`
		Payload RepoPushPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.push message: %v", err)
		c.sendPushResult(false, "", apperrors.CodeServerInvalidMessage, "invalid JSON")
		return
	}

	payload := msg.Payload

	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.push request")
		c.sendPushResult(false, "", apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Push to remote
	output, err := gitOps.Push(payload.Remote, payload.Branch, payload.ForceWithLease)
	if err != nil {
		log.Printf("Push failed: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendPushResult(false, "", code, message)
		return
	}

	log.Printf("Push succeeded: output=%s device=%s", output, c.deviceID)

	// Broadcast success to all clients
	c.server.Broadcast(NewRepoPushResultMessage(true, output, "", ""))
}

// sendPushResult sends a repo.push_result message to the client.
func (c *Client) sendPushResult(success bool, output, errCode, errMsg string) {
	msg := NewRepoPushResultMessage(success, output, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping push result")
	}
}

// DeviceStore is the interface for device storage operations.
// This allows the server to delete devices without importing the storage package.
type DeviceStore interface {
	GetDevice(id string) (*DeviceInfo, error)
	DeleteDevice(id string) error
}

// DeviceInfo represents a paired device (minimal interface for server package).
type DeviceInfo struct {
	ID   string
	Name string
}

// RevokeDeviceHandler handles the HTTP endpoint for device revocation.
// It closes active connections for the device and removes it from storage.
type RevokeDeviceHandler struct {
	server      *Server
	deviceStore DeviceStore
}

// NewRevokeDeviceHandler creates a handler for the /devices/{id}/revoke endpoint.
// The server is used to close active WebSocket connections for the revoked device.
// The store is used to delete the device from persistent storage.
func NewRevokeDeviceHandler(server *Server, store DeviceStore) *RevokeDeviceHandler {
	return &RevokeDeviceHandler{server: server, deviceStore: store}
}

// ServeHTTP handles POST /devices/{id}/revoke requests.
// This endpoint is restricted to loopback (localhost) requests only for security.
// The handler first closes any active WebSocket connections for the device,
// then removes the device from storage.
func (h *RevokeDeviceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses
	if !isLoopbackRequest(r) {
		log.Printf("server: rejected device revoke from non-loopback address: %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "forbidden",
			"message": "Device revocation is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "method_not_allowed",
			"message": "Only POST is allowed",
		})
		return
	}

	// Extract device ID from URL path: /devices/{id}/revoke
	// Path format: /devices/UUID/revoke
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) != 3 || pathParts[0] != "devices" || pathParts[2] != "revoke" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "invalid_path",
			"message": "Expected path format: /devices/{id}/revoke",
		})
		return
	}
	deviceID := pathParts[1]

	if deviceID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "missing_device_id",
			"message": "Device ID is required",
		})
		return
	}

	// Check if device exists
	device, err := h.deviceStore.GetDevice(deviceID)
	if err != nil {
		log.Printf("server: failed to lookup device %s: %v", deviceID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "lookup_failed",
			"message": "Failed to lookup device",
		})
		return
	}
	if device == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "not_found",
			"message": "Device not found",
		})
		return
	}

	// Close active connections first (before deleting from storage)
	// This ensures the device cannot use an existing connection after revocation
	closedCount := h.server.CloseDeviceConnections(deviceID)

	// Delete from storage
	if err := h.deviceStore.DeleteDevice(deviceID); err != nil {
		log.Printf("server: failed to delete device %s: %v", deviceID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "delete_failed",
			"message": "Failed to delete device",
		})
		return
	}

	log.Printf("server: revoked device %s (%s), closed %d connection(s)", deviceID, device.Name, closedCount)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"device_id":          deviceID,
		"device_name":        device.Name,
		"connections_closed": closedCount,
	})
}

// isLoopbackRequest checks if the HTTP request came from a loopback address.
// This is used to restrict sensitive endpoints to localhost-only access.
// Returns true for 127.0.0.0/8 (IPv4) and ::1 (IPv6).
// Note: This duplicates the function in auth/handler.go to avoid circular imports.
func isLoopbackRequest(r *http.Request) bool {
	// Extract the host part from RemoteAddr (format is "host:port" or "[host]:port" for IPv6)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// If we can't parse the address, be conservative and reject
		log.Printf("server: failed to parse RemoteAddr %q: %v", r.RemoteAddr, err)
		return false
	}

	// Parse the IP address
	ip := net.ParseIP(host)
	if ip == nil {
		// If we can't parse the IP, be conservative and reject
		log.Printf("server: failed to parse IP from host %q", host)
		return false
	}

	// Check if it's a loopback address (127.0.0.0/8 for IPv4, ::1 for IPv6)
	return ip.IsLoopback()
}

// ApprovalTokenValidator validates approval tokens for the /approve endpoint.
// This interface allows mocking the token validation in tests.
type ApprovalTokenValidator interface {
	ValidateToken(token string) bool
}

// ApproveHandler handles the HTTP endpoint for CLI command approval.
// It validates the request, extracts the bearer token, and calls the approval queue.
// Phase 6.1b: HTTP endpoint for CLI command approval broker.
//
// Security notes:
//   - Loopback only: Requests from non-loopback addresses are rejected with 403.
//   - Token auth: Bearer token is validated before processing the request.
//   - Context cancellation: If the HTTP request context is cancelled (client
//     disconnect), the approval queue handles cleanup and returns context.Canceled.
//
// Accepted risks:
//   - No request body size limit: Large request bodies are accepted. This is
//     acceptable as the endpoint is localhost-only and authenticated.
//   - Response always 200: Approval decisions (denied/timeout) return 200 with
//     approved=false. This simplifies client handling and matches REST conventions
//     where the request itself succeeded even if the business logic result is negative.
type ApproveHandler struct {
	server         *Server
	tokenValidator ApprovalTokenValidator
}

// NewApproveHandler creates a handler for the /approve endpoint.
// The server provides access to the approval queue.
// The tokenValidator is used to authenticate requests.
func NewApproveHandler(server *Server, tokenValidator ApprovalTokenValidator) *ApproveHandler {
	return &ApproveHandler{
		server:         server,
		tokenValidator: tokenValidator,
	}
}

// approveRequest is the JSON request body for POST /approve.
type approveRequest struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
	Repo      string `json:"repo"`
	Rationale string `json:"rationale"`
}

// approveResponse is the JSON response for POST /approve.
type approveResponse struct {
	Approved            bool    `json:"approved"`
	Error               string  `json:"error,omitempty"`
	Message             string  `json:"message,omitempty"`
	TemporaryAllowUntil *string `json:"temporary_allow_until,omitempty"`
}

// ServeHTTP handles POST /approve requests from CLI tools.
// This endpoint is restricted to loopback (localhost) requests and requires
// a valid Bearer token in the Authorization header.
func (h *ApproveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses
	if !isLoopbackRequest(r) {
		log.Printf("server: rejected /approve from non-loopback address: %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "forbidden",
			Message:  "Approval endpoint is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "method_not_allowed",
			Message:  "Only POST is allowed",
		})
		return
	}

	// Extract and validate Bearer token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Authorization header required",
		})
		return
	}

	// Parse Bearer token
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Invalid authorization format (expected Bearer token)",
		})
		return
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)

	// Validate token
	if !h.tokenValidator.ValidateToken(token) {
		log.Printf("server: rejected /approve with invalid token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Invalid approval token",
		})
		return
	}

	// Parse request body
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "invalid_request",
			Message:  fmt.Sprintf("Invalid JSON body: %v", err),
		})
		return
	}

	// Validate required fields
	if req.Command == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "invalid_request",
			Message:  "Command is required",
		})
		return
	}

	// Get approval queue
	queue := h.server.GetApprovalQueue()
	if queue == nil {
		log.Printf("server: /approve called but no approval queue configured")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "service_unavailable",
			Message:  "Approval queue not configured",
		})
		return
	}

	// Submit approval request and wait for decision
	approvalReq := ApprovalRequest{
		Command:   req.Command,
		Cwd:       req.Cwd,
		Repo:      req.Repo,
		Rationale: req.Rationale,
	}

	log.Printf("server: /approve request for command %q", req.Command)
	response, err := queue.Queue(r.Context(), approvalReq)

	// Build response
	result := approveResponse{
		Approved: err == nil && response.Decision == "approve",
	}

	if err != nil {
		// Extract error code and message
		result.Error = apperrors.GetCode(err)
		result.Message = apperrors.GetMessage(err)
	}

	if response.TemporaryAllowUntil != nil {
		ts := response.TemporaryAllowUntil.Format(time.RFC3339)
		result.TemporaryAllowUntil = &ts
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

// handleSessionCreate processes a session.create message from the client.
// It creates a new PTY session via the SessionManager and broadcasts
// the session.created message to all connected clients.
// Phase 9.3: Multi-session PTY management.
func (c *Client) handleSessionCreate(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload SessionCreatePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.create payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	// Get session manager from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("No session manager configured, ignoring session.create request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Determine command to run (default to user's shell if not specified)
	command := payload.Command
	args := payload.Args
	if command == "" {
		// Use user's preferred shell from $SHELL, or fall back to /bin/sh
		command = os.Getenv("SHELL")
		if command == "" {
			command = "/bin/sh"
		}
		// Run as login shell to load user's profile (.bashrc, .zshrc, etc.)
		args = []string{"-l"}
	}

	// Create session configuration with output callback
	cfg := pty.SessionConfig{
		HistoryLines: 5000, // Default history size
		OnOutputWithID: func(sessionID, line string) {
			// Broadcast terminal output to all clients with session ID
			c.server.Broadcast(NewTerminalAppendMessage(sessionID, line))
		},
	}

	// Create the session
	session, err := mgr.Create(cfg)
	if err != nil {
		log.Printf("Failed to create session: %v", err)
		c.sendError(apperrors.CodeSessionCreateFailed, fmt.Sprintf("failed to create session: %v", err))
		return
	}

	// Start the session with the command
	if err := session.Start(command, args...); err != nil {
		log.Printf("Failed to start session %s: %v", session.ID, err)
		// Clean up the session since it failed to start
		mgr.Close(session.ID)
		c.sendError(apperrors.CodeSessionStartFailed, fmt.Sprintf("failed to start session: %v", err))
		return
	}

	// Determine display name
	name := payload.Name
	if name == "" {
		name = fmt.Sprintf("Session %d", mgr.Count())
	}

	// Broadcast session.created to all clients
	createdMsg := NewSessionCreatedMessage(
		session.ID,
		name,
		command,
		"running",
		time.Now().UnixMilli(),
	)
	c.server.Broadcast(createdMsg)

	log.Printf("Session created: id=%s command=%s device=%s", session.ID, command, c.deviceID)
}

// handleSessionClose processes a session.close message from the client.
// It closes the specified PTY session and broadcasts the session.closed
// message to all connected clients.
// Phase 9.3: Multi-session PTY management.
func (c *Client) handleSessionClose(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType         `json:"type"`
		ID      string              `json:"id,omitempty"`
		Payload SessionClosePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.close payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.SessionID == "" {
		log.Printf("session.close missing session_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "session_id is required")
		return
	}

	// Get session manager from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("No session manager configured, ignoring session.close request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Close the session
	if err := mgr.Close(payload.SessionID); err != nil {
		if err == pty.ErrSessionNotFound {
			log.Printf("Session not found: %s", payload.SessionID)
			c.sendError(apperrors.CodeSessionNotFound, "session not found")
		} else {
			log.Printf("Failed to close session %s: %v", payload.SessionID, err)
			c.sendError(apperrors.CodeSessionCloseFailed, fmt.Sprintf("failed to close session: %v", err))
		}
		return
	}

	// Reset activeSessionID for any clients viewing this session
	c.server.mu.Lock()
	for client := range c.server.clients {
		if client.activeSessionID == payload.SessionID {
			client.activeSessionID = ""
		}
	}
	c.server.mu.Unlock()

	// Broadcast session.closed to all clients
	closedMsg := NewSessionClosedMessage(payload.SessionID, "user_requested")
	c.server.Broadcast(closedMsg)

	log.Printf("Session closed: id=%s device=%s", payload.SessionID, c.deviceID)
}

// handleSessionSwitch processes a session.switch message from the client.
// It updates the client's active session and sends the terminal buffer
// for the new session to replay history.
// Phase 9.3: Multi-session PTY management.
func (c *Client) handleSessionSwitch(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload SessionSwitchPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.switch payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.SessionID == "" {
		log.Printf("session.switch missing session_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "session_id is required")
		return
	}

	// Get session manager from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("No session manager configured, ignoring session.switch request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Verify session exists
	session := mgr.Get(payload.SessionID)
	if session == nil {
		log.Printf("Session not found for switch: %s", payload.SessionID)
		c.sendError(apperrors.CodeSessionNotFound, "session not found")
		return
	}

	// Update client's active session
	c.activeSessionID = payload.SessionID

	// Get terminal buffer from session
	lines := session.Lines()

	// Send session.buffer to this client (not broadcast)
	bufferMsg := NewSessionBufferMessage(payload.SessionID, lines, 0, 0)
	select {
	case <-c.done:
		return
	case c.send <- bufferMsg:
		log.Printf("Sent session buffer: id=%s lines=%d device=%s", payload.SessionID, len(lines), c.deviceID)
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending session buffer to client")
	}
}

// handleSessionRename processes a session.rename message from the client.
// It renames a session's human-readable name.
// Phase 9.6: Mobile session protocol.
func (c *Client) handleSessionRename(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload SessionRenamePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.rename payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.SessionID == "" {
		log.Printf("session.rename missing session_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "session_id is required")
		return
	}

	// Get session manager from server
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("No session manager configured, ignoring session.rename request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Rename the session
	if err := mgr.Rename(payload.SessionID, payload.Name); err != nil {
		if err == pty.ErrSessionNotFound {
			log.Printf("Session not found for rename: %s", payload.SessionID)
			c.sendError(apperrors.CodeSessionNotFound, "session not found")
			return
		}
		log.Printf("Failed to rename session %s: %v", payload.SessionID, err)
		c.sendError(apperrors.CodeSessionRenameFailed, "failed to rename session")
		return
	}

	log.Printf("Session renamed: id=%s name=%s", payload.SessionID, payload.Name)
	// No response message - client updates name locally
}

// handleTmuxList processes a tmux.list message from the client.
// It queries the TmuxManager for available sessions and sends
// a tmux.sessions response with the results or an error code.
// Phase 12: tmux session integration.
func (c *Client) handleTmuxList(data []byte) {
	// tmux.list has an empty payload, no need to parse

	// Get tmux manager from server
	c.server.mu.RLock()
	mgr := c.server.tmuxManager
	c.server.mu.RUnlock()

	if mgr == nil {
		log.Printf("No tmux manager configured, ignoring tmux.list request")
		// Send empty list with error code
		msg := NewTmuxSessionsMessage(nil, apperrors.CodeServerHandlerMissing)
		c.sendTmuxSessions(msg)
		return
	}

	// Get list of tmux sessions
	sessions, err := mgr.ListSessions()
	if err != nil {
		// Extract error code and send in response
		code := apperrors.GetCode(err)
		log.Printf("Failed to list tmux sessions: %v (code=%s)", err, code)
		msg := NewTmuxSessionsMessage(nil, code)
		c.sendTmuxSessions(msg)
		return
	}

	// Convert tmux.TmuxSessionInfo to wire format TmuxSessionInfo
	wireInfos := make([]TmuxSessionInfo, len(sessions))
	for i, s := range sessions {
		wireInfos[i] = TmuxSessionInfo{
			Name:      s.Name,
			Windows:   s.Windows,
			Attached:  s.Attached,
			CreatedAt: s.CreatedAt.UnixMilli(),
		}
	}

	// Send response to requesting client only (not broadcast)
	msg := NewTmuxSessionsMessage(wireInfos, "")
	c.sendTmuxSessions(msg)
	log.Printf("Sent tmux.sessions: count=%d device=%s", len(wireInfos), c.deviceID)
}

// sendTmuxSessions sends a tmux.sessions message to this client.
func (c *Client) sendTmuxSessions(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending tmux.sessions to client")
	}
}

// handleTmuxAttach processes a tmux.attach message from the client.
// It uses TmuxManager.AttachToTmux() to create a PTY attached to the
// specified tmux session, then broadcasts tmux.attached to all clients.
// Phase 12.4: tmux attach protocol.
func (c *Client) handleTmuxAttach(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload TmuxAttachPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse tmux.attach payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.TmuxSession == "" {
		log.Printf("tmux.attach missing tmux_session")
		c.sendError(apperrors.CodeServerInvalidMessage, "tmux_session is required")
		return
	}

	// Get tmux manager and session manager from server
	c.server.mu.RLock()
	tmuxMgr := c.server.tmuxManager
	sessionMgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if tmuxMgr == nil {
		log.Printf("No tmux manager configured, ignoring tmux.attach request")
		c.sendError(apperrors.CodeServerHandlerMissing, "tmux integration not configured")
		return
	}

	if sessionMgr == nil {
		log.Printf("No session manager configured, ignoring tmux.attach request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Create session config with output callback
	cfg := pty.SessionConfig{
		HistoryLines: 5000, // Default history size
		OnOutputWithID: func(sessionID, line string) {
			// Broadcast terminal output to all clients with session ID
			c.server.Broadcast(NewTerminalAppendMessage(sessionID, line))
		},
	}

	// Attach to tmux session via TmuxManager
	session, err := tmuxMgr.AttachToTmux(payload.TmuxSession, cfg)
	if err != nil {
		log.Printf("Failed to attach to tmux session %s: %v", payload.TmuxSession, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendError(code, message)
		return
	}

	// Capture and prepend scrollback history (before PTY starts outputting)
	scrollback, scrollbackErr := tmuxMgr.CaptureScrollback(payload.TmuxSession, 0)
	if scrollbackErr != nil {
		log.Printf("Failed to capture scrollback for %s: %v", payload.TmuxSession, scrollbackErr)
		// Continue without scrollback - it's optional
	} else if scrollback != "" {
		session.PrependToBuffer(scrollback)
	}

	// Register the session with SessionManager
	if err := sessionMgr.Register(session); err != nil {
		// Registration failed - stop the session we just created
		session.Stop()
		log.Printf("Failed to register tmux session %s: %v", session.ID, err)
		c.sendError(apperrors.CodeSessionCreateFailed, "failed to register session")
		return
	}

	// Build display name (use tmux session name by default)
	name := payload.TmuxSession
	command := fmt.Sprintf("tmux attach-session -t %s", payload.TmuxSession)

	// Broadcast tmux.attached to all clients
	// Use actual session creation time for consistency
	createdAt := session.GetCreatedAt().UnixMilli()
	attachedMsg := NewTmuxAttachedMessage(
		session.ID,
		payload.TmuxSession,
		name,
		command,
		"running",
		createdAt,
	)
	c.server.Broadcast(attachedMsg)

	// Send scrollback buffer to the requesting client
	lines := session.Lines()
	if len(lines) > 0 {
		bufferMsg := NewSessionBufferMessage(session.ID, lines, 0, 0)
		select {
		case <-c.done:
			return
		case c.send <- bufferMsg:
		}
	}

	log.Printf("Attached to tmux session: id=%s tmux=%s scrollback=%d device=%s",
		session.ID, payload.TmuxSession, len(lines), c.deviceID)
}

// handleTmuxDetach processes a tmux.detach message from the client.
// It closes the PTY session and optionally kills the underlying tmux session.
// Phase 12.5: tmux detach support.
func (c *Client) handleTmuxDetach(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload TmuxDetachPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse tmux.detach payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.SessionID == "" {
		log.Printf("tmux.detach missing session_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "session_id is required")
		return
	}

	// Get managers from server
	c.server.mu.RLock()
	tmuxMgr := c.server.tmuxManager
	sessionMgr := c.server.sessionManager
	c.server.mu.RUnlock()

	if sessionMgr == nil {
		log.Printf("No session manager configured, ignoring tmux.detach request")
		c.sendError(apperrors.CodeServerHandlerMissing, "session management not configured")
		return
	}

	// Get the session to check if it's a tmux session
	session := sessionMgr.Get(payload.SessionID)
	if session == nil {
		log.Printf("Session not found: %s", payload.SessionID)
		c.sendError(apperrors.CodeSessionNotFound, "session not found")
		return
	}

	tmuxSession := session.GetTmuxSession()
	if tmuxSession == "" {
		log.Printf("Session %s is not a tmux session", payload.SessionID)
		c.sendError(apperrors.CodeServerInvalidMessage, "session is not a tmux session")
		return
	}

	// If kill flag is set, kill the tmux session
	killed := false
	if payload.Kill {
		if tmuxMgr == nil {
			log.Printf("No tmux manager configured, cannot kill tmux session")
			c.sendError(apperrors.CodeServerHandlerMissing, "tmux integration not configured")
			return
		}

		if err := tmuxMgr.KillSession(tmuxSession); err != nil {
			log.Printf("Failed to kill tmux session %s: %v", tmuxSession, err)
			// Continue with PTY close even if tmux kill fails
			// The tmux session may have already terminated
		} else {
			killed = true
		}
	}

	// Close the PTY session (this detaches from tmux)
	if err := sessionMgr.Close(payload.SessionID); err != nil {
		log.Printf("Failed to close session %s: %v", payload.SessionID, err)
		c.sendError(apperrors.CodeSessionCloseFailed, fmt.Sprintf("failed to close session: %v", err))
		return
	}

	// Reset activeSessionID for any clients viewing this session
	c.server.mu.Lock()
	for client := range c.server.clients {
		if client.activeSessionID == payload.SessionID {
			client.activeSessionID = ""
		}
	}
	c.server.mu.Unlock()

	// Broadcast tmux.detached to all clients
	c.server.Broadcast(NewTmuxDetachedMessage(payload.SessionID, tmuxSession, killed))

	log.Printf("Detached from tmux session: id=%s tmux=%s killed=%v device=%s",
		payload.SessionID, tmuxSession, killed, c.deviceID)
}
