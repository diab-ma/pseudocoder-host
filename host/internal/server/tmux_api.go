// Package server provides WebSocket server functionality for the pseudocoder host.
// This file implements HTTP endpoints for tmux session management (Unit 12.9).
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/tmux"
)

// TmuxManager defines the interface for tmux operations used by TmuxAPIHandler.
// This interface allows for dependency injection and testing without a real tmux binary.
type TmuxManager interface {
	// ListSessions returns all available tmux sessions.
	ListSessions() ([]tmux.TmuxSessionInfo, error)

	// AttachToTmux creates a PTY session attached to an existing tmux session.
	AttachToTmux(name string, sessionConfig pty.SessionConfig) (*pty.Session, error)

	// KillSession terminates a tmux session.
	KillSession(name string) error
}

// TmuxListResponse is the response for GET /api/tmux/list.
type TmuxListResponse struct {
	// Sessions is the list of available tmux sessions.
	Sessions []TmuxSessionItem `json:"sessions"`
}

// TmuxSessionItem represents a tmux session in the list response.
type TmuxSessionItem struct {
	// Name is the tmux session name.
	Name string `json:"name"`

	// Windows is the number of windows in this session.
	Windows int `json:"windows"`

	// Attached indicates whether another client is attached.
	Attached bool `json:"attached"`

	// CreatedAt is when the tmux session was created.
	CreatedAt time.Time `json:"created_at"`
}

// TmuxAttachRequest is the request body for POST /api/tmux/attach.
type TmuxAttachRequest struct {
	// TmuxSession is the tmux session name to attach to.
	TmuxSession string `json:"tmux_session"`

	// Name is an optional human-readable name for the pseudocoder session.
	Name string `json:"name,omitempty"`
}

// TmuxAttachResponse is the response for POST /api/tmux/attach.
type TmuxAttachResponse struct {
	// SessionID is the unique pseudocoder session identifier.
	SessionID string `json:"session_id"`

	// TmuxSession is the tmux session name.
	TmuxSession string `json:"tmux_session"`

	// Name is the session's human-readable name.
	Name string `json:"name,omitempty"`
}

// TmuxDetachRequest is the request body for POST /api/tmux/{id}/detach.
type TmuxDetachRequest struct {
	// Kill indicates whether to also kill the tmux session (not just detach).
	Kill bool `json:"kill,omitempty"`
}

// TmuxDetachResponse is the response for POST /api/tmux/{id}/detach.
type TmuxDetachResponse struct {
	// SessionID is the pseudocoder session that was detached.
	SessionID string `json:"session_id"`

	// TmuxSession is the tmux session name.
	TmuxSession string `json:"tmux_session"`

	// Killed indicates whether the tmux session was also killed.
	Killed bool `json:"killed"`
}

// TmuxAPIConfig holds configuration for the tmux API handler.
type TmuxAPIConfig struct {
	// HistoryLines is how many lines of output to keep in the buffer.
	HistoryLines int

	// OnSessionOutput is called for each line of session output.
	// The callback receives the session ID and the output line.
	OnSessionOutput func(sessionID, line string)
}

// TmuxAPIHandler handles HTTP requests for tmux session management.
// All endpoints are restricted to loopback addresses for security.
//
// Routes:
//   - GET  /api/tmux/list         - List available tmux sessions
//   - POST /api/tmux/attach       - Attach to a tmux session
//   - POST /api/tmux/{id}/detach  - Detach from a tmux session
type TmuxAPIHandler struct {
	tmuxManager    TmuxManager
	sessionManager *pty.SessionManager
	config         TmuxAPIConfig
}

// NewTmuxAPIHandler creates a new tmux API handler.
//
// Parameters:
//   - tmuxMgr: The tmux manager for interacting with tmux (can be nil to disable tmux features).
//   - sessionMgr: The session manager for PTY session management.
//   - cfg: Configuration for creating new sessions.
func NewTmuxAPIHandler(tmuxMgr TmuxManager, sessionMgr *pty.SessionManager, cfg TmuxAPIConfig) *TmuxAPIHandler {
	return &TmuxAPIHandler{
		tmuxManager:    tmuxMgr,
		sessionManager: sessionMgr,
		config:         cfg,
	}
}

// ServeHTTP routes requests to the appropriate handler based on the URL path.
// All endpoints require loopback access (localhost only).
func (h *TmuxAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses.
	if !isLoopbackRequest(r) {
		http.Error(w, "Forbidden: tmux API is loopback-only", http.StatusForbidden)
		return
	}

	// Parse the path to determine the action.
	// Expected paths:
	//   /api/tmux/list
	//   /api/tmux/attach
	//   /api/tmux/{id}/detach
	path := strings.TrimPrefix(r.URL.Path, "/api/tmux/")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "list":
		h.handleList(w, r)
	case path == "attach":
		h.handleAttach(w, r)
	case strings.HasSuffix(path, "/detach"):
		id := strings.TrimSuffix(path, "/detach")
		h.handleDetach(w, r, id)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// handleList returns all available tmux sessions.
// GET /api/tmux/list
func (h *TmuxAPIHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.tmuxManager == nil {
		writeJSONError(w, "tmux manager not configured", http.StatusServiceUnavailable)
		return
	}

	sessions, err := h.tmuxManager.ListSessions()
	if err != nil {
		// Check for specific tmux errors
		errMsg := err.Error()
		if strings.Contains(errMsg, "tmux.not_installed") {
			writeJSONError(w, "tmux is not installed on this system", http.StatusServiceUnavailable)
			return
		}
		writeJSONError(w, "Failed to list tmux sessions: "+errMsg, http.StatusInternalServerError)
		return
	}

	// Convert to API response format
	items := make([]TmuxSessionItem, len(sessions))
	for i, s := range sessions {
		items[i] = TmuxSessionItem{
			Name:      s.Name,
			Windows:   s.Windows,
			Attached:  s.Attached,
			CreatedAt: s.CreatedAt,
		}
	}

	resp := TmuxListResponse{Sessions: items}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAttach attaches to a tmux session.
// POST /api/tmux/attach
func (h *TmuxAPIHandler) handleAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.tmuxManager == nil {
		writeJSONError(w, "tmux manager not configured", http.StatusServiceUnavailable)
		return
	}

	if h.sessionManager == nil {
		writeJSONError(w, "session manager not configured", http.StatusServiceUnavailable)
		return
	}

	// Parse request body
	var req TmuxAttachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.TmuxSession == "" {
		writeJSONError(w, "tmux_session is required", http.StatusBadRequest)
		return
	}

	// Validate session name if provided
	if errMsg := validateSessionName(req.Name); errMsg != "" {
		writeJSONError(w, errMsg, http.StatusBadRequest)
		return
	}

	// Attach to the tmux session via TmuxManager
	sessionConfig := pty.SessionConfig{
		Name:           req.Name,
		HistoryLines:   h.config.HistoryLines,
		OnOutputWithID: h.config.OnSessionOutput,
	}

	session, err := h.tmuxManager.AttachToTmux(req.TmuxSession, sessionConfig)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "tmux.not_installed") {
			writeJSONError(w, "tmux is not installed on this system", http.StatusServiceUnavailable)
			return
		}
		if strings.Contains(errMsg, "tmux.session_not_found") {
			writeJSONError(w, "tmux session not found: "+req.TmuxSession, http.StatusNotFound)
			return
		}
		if strings.Contains(errMsg, "tmux.attach_failed") {
			writeJSONError(w, "Failed to attach to tmux session: "+errMsg, http.StatusInternalServerError)
			return
		}
		writeJSONError(w, "Failed to attach to tmux session: "+errMsg, http.StatusInternalServerError)
		return
	}

	// Register the session with the session manager
	if err := h.sessionManager.Register(session); err != nil {
		// Clean up the PTY session if registration fails
		_ = session.Stop()
		writeJSONError(w, "Failed to register session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response
	resp := TmuxAttachResponse{
		SessionID:   session.ID,
		TmuxSession: req.TmuxSession,
		Name:        req.Name,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleDetach detaches from a tmux session.
// POST /api/tmux/{id}/detach
func (h *TmuxAPIHandler) handleDetach(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if id == "" {
		writeJSONError(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	if h.sessionManager == nil {
		writeJSONError(w, "session manager not configured", http.StatusServiceUnavailable)
		return
	}

	// Parse request body (optional)
	var req TmuxDetachRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	// Get the session from the session manager
	session := h.sessionManager.Get(id)
	if session == nil {
		writeJSONError(w, "Session not found", http.StatusNotFound)
		return
	}

	// Check if this is a tmux session
	if session.TmuxSession == "" {
		writeJSONError(w, "Session is not a tmux session", http.StatusBadRequest)
		return
	}

	tmuxSessionName := session.TmuxSession

	// If kill flag is set, kill the tmux session
	var killed bool
	if req.Kill && h.tmuxManager != nil {
		if err := h.tmuxManager.KillSession(tmuxSessionName); err != nil {
			// Log but don't fail - still close the PTY session
			// The tmux session might already be gone
		} else {
			killed = true
		}
	}

	// Close the PTY session (detach from tmux)
	if err := h.sessionManager.Close(id); err != nil {
		writeJSONError(w, "Failed to close session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response
	resp := TmuxDetachResponse{
		SessionID:   id,
		TmuxSession: tmuxSessionName,
		Killed:      killed,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
