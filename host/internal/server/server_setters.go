package server

import (
	"io"
	"log"
	"net/http"

	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/tmux"
)

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
