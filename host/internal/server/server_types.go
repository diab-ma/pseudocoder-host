package server

import (
	"io"
	"net/http"
	"sync"
	"time"

	// gorilla/websocket is the most popular WebSocket library for Go.
	// It provides a complete implementation of the WebSocket protocol
	// with support for reading/writing messages, ping/pong, and close handling.
	"github.com/gorilla/websocket"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"

	// Storage package provides card and session persistence.
	"github.com/pseudocoder/host/internal/storage"

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
// contentHash is preferred over chunkIndex for stable identity (indices can shift after staging).
type ChunkUndoHandler func(cardID string, chunkIndex int, contentHash string, confirmed bool) (*UndoResult, error)

// UndoResult carries the restored card/chunk information after a successful undo.
// This is used to re-emit the card to connected clients.
type UndoResult struct {
	CardID       string
	ChunkIndex   int    // -1 for file-level undo
	ContentHash  string // Stable chunk identifier (returned for client to clear pending state)
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

// NewServer creates a new WebSocket server.
// Call Start() to begin accepting connections.
func NewServer(addr string) *Server {
	return &Server{
		addr:      addr,
		clients:   make(map[*Client]bool),
		broadcast: make(chan Message, channelBufferSize),
		sessionID: "session-" + formatTimestamp(time.Now()),
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

// formatTimestamp formats a time as Unix seconds for session IDs.
// This is a helper to avoid importing fmt in the constructor.
func formatTimestamp(t time.Time) string {
	// Simple integer-to-string conversion for session ID suffix
	n := t.Unix()
	if n == 0 {
		return "0"
	}
	// Build string in reverse
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
