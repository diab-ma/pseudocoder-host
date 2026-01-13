// Package server provides WebSocket server functionality for the pseudocoder host.
// This file implements HTTP endpoints for session management (Unit 9.5).
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/pseudocoder/host/internal/pty"
)

// MaxSessionNameLength is the maximum allowed length for session names.
const MaxSessionNameLength = 100

// SessionNewRequest is the request body for POST /api/session/new.
type SessionNewRequest struct {
	// Name is an optional human-readable name for the session.
	Name string `json:"name,omitempty"`

	// Command is the command to run (defaults to the host's default shell).
	Command string `json:"command,omitempty"`

	// Args are optional command arguments.
	Args []string `json:"args,omitempty"`
}

// SessionNewResponse is the response for POST /api/session/new.
type SessionNewResponse struct {
	// ID is the unique session identifier.
	ID string `json:"id"`

	// Name is the session's human-readable name.
	Name string `json:"name,omitempty"`

	// Command is the command being run.
	Command string `json:"command"`
}

// SessionListItem represents a session in the list response.
type SessionListItem struct {
	// ID is the unique session identifier.
	ID string `json:"id"`

	// Name is the session's human-readable name.
	Name string `json:"name,omitempty"`

	// Command is the command running in this session.
	Command string `json:"command"`

	// CreatedAt is when the session was started.
	CreatedAt time.Time `json:"created_at"`

	// Running indicates whether the session is currently executing.
	Running bool `json:"running"`
}

// SessionKillResponse is the response for POST /api/session/{id}/kill.
type SessionKillResponse struct {
	// ID is the session that was killed.
	ID string `json:"id"`

	// Killed indicates the session was successfully terminated.
	Killed bool `json:"killed"`
}

// SessionRenameRequest is the request body for POST /api/session/{id}/rename.
type SessionRenameRequest struct {
	// Name is the new name for the session.
	Name string `json:"name"`
}

// SessionRenameResponse is the response for POST /api/session/{id}/rename.
type SessionRenameResponse struct {
	// ID is the session that was renamed.
	ID string `json:"id"`

	// Name is the new name.
	Name string `json:"name"`
}

// SessionAPIConfig holds configuration for creating sessions via the API.
type SessionAPIConfig struct {
	// DefaultCommand is the command to run if not specified in the request.
	DefaultCommand string

	// DefaultArgs are the default arguments for the command.
	DefaultArgs []string

	// HistoryLines is how many lines of output to keep in the buffer.
	HistoryLines int

	// OnSessionOutput is called for each line of session output.
	// The callback receives the session ID and the output line.
	OnSessionOutput func(sessionID, line string)
}

// SessionAPIHandler handles HTTP requests for session management.
// All endpoints are restricted to loopback addresses for security.
//
// Routes:
//   - POST /api/session/new - Create a new session
//   - GET  /api/session/list - List all sessions
//   - POST /api/session/{id}/kill - Kill a session
//   - POST /api/session/{id}/rename - Rename a session
type SessionAPIHandler struct {
	manager *pty.SessionManager
	config  SessionAPIConfig
}

// NewSessionAPIHandler creates a new session API handler.
//
// Parameters:
//   - mgr: The session manager to use for session operations.
//   - cfg: Configuration for creating new sessions.
func NewSessionAPIHandler(mgr *pty.SessionManager, cfg SessionAPIConfig) *SessionAPIHandler {
	return &SessionAPIHandler{
		manager: mgr,
		config:  cfg,
	}
}

// ServeHTTP routes requests to the appropriate handler based on the URL path.
// All endpoints require loopback access (localhost only).
func (h *SessionAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses.
	if !isLoopbackRequest(r) {
		http.Error(w, "Forbidden: session API is loopback-only", http.StatusForbidden)
		return
	}

	// Parse the path to determine the action.
	// Expected paths:
	//   /api/session/new
	//   /api/session/list
	//   /api/session/{id}/kill
	//   /api/session/{id}/rename
	path := strings.TrimPrefix(r.URL.Path, "/api/session/")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "new":
		h.handleNew(w, r)
	case path == "list":
		h.handleList(w, r)
	case strings.HasSuffix(path, "/kill"):
		id := strings.TrimSuffix(path, "/kill")
		h.handleKill(w, r, id)
	case strings.HasSuffix(path, "/rename"):
		id := strings.TrimSuffix(path, "/rename")
		h.handleRename(w, r, id)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// handleNew creates a new session.
// POST /api/session/new
func (h *SessionAPIHandler) handleNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body (optional, all fields have defaults).
	var req SessionNewRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	// Validate session name.
	if errMsg := validateSessionName(req.Name); errMsg != "" {
		writeJSONError(w, errMsg, http.StatusBadRequest)
		return
	}

	// Use defaults if not specified.
	command := req.Command
	if command == "" {
		command = h.config.DefaultCommand
	}
	args := req.Args
	if len(args) == 0 && len(h.config.DefaultArgs) > 0 {
		args = h.config.DefaultArgs
	}

	// Create the session.
	session, err := h.manager.Create(pty.SessionConfig{
		Name:           req.Name,
		HistoryLines:   h.config.HistoryLines,
		OnOutputWithID: h.config.OnSessionOutput,
	})
	if err != nil {
		if err == pty.ErrMaxSessionsReached {
			msg := fmt.Sprintf("Maximum number of sessions reached (limit: %d)", h.manager.MaxSessions())
			writeJSONError(w, msg, http.StatusServiceUnavailable)
			return
		}
		writeJSONError(w, "Failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Start the command.
	if err := session.Start(command, args...); err != nil {
		// Clean up the session if start fails.
		_ = h.manager.Close(session.ID)
		writeJSONError(w, "Failed to start session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response.
	resp := SessionNewResponse{
		ID:      session.ID,
		Name:    req.Name,
		Command: command,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleList returns all active sessions.
// GET /api/session/list
func (h *SessionAPIHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessions := h.manager.List()

	// Convert to API response format.
	items := make([]SessionListItem, len(sessions))
	for i, s := range sessions {
		items[i] = SessionListItem{
			ID:        s.ID,
			Name:      s.Name,
			Command:   s.Command,
			CreatedAt: s.CreatedAt,
			Running:   s.Running,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// handleKill terminates a session.
// POST /api/session/{id}/kill
func (h *SessionAPIHandler) handleKill(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if id == "" {
		writeJSONError(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	// Close the session.
	if err := h.manager.Close(id); err != nil {
		if err == pty.ErrSessionNotFound {
			writeJSONError(w, "Session not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "Failed to kill session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := SessionKillResponse{
		ID:     id,
		Killed: true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRename changes a session's human-readable name.
// POST /api/session/{id}/rename
func (h *SessionAPIHandler) handleRename(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if id == "" {
		writeJSONError(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	// Parse request body.
	var req SessionRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Validate session name.
	if errMsg := validateSessionName(req.Name); errMsg != "" {
		writeJSONError(w, errMsg, http.StatusBadRequest)
		return
	}

	// Rename the session.
	if err := h.manager.Rename(id, req.Name); err != nil {
		if err == pty.ErrSessionNotFound {
			writeJSONError(w, "Session not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "Failed to rename session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := SessionRenameResponse{
		ID:   id,
		Name: req.Name,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// validateSessionName checks if a session name is valid.
// Returns an error message if invalid, or empty string if valid.
// Empty names are allowed (optional field).
func validateSessionName(name string) string {
	if name == "" {
		return "" // Empty is valid (optional)
	}

	if len(name) > MaxSessionNameLength {
		return "Session name too long (max 100 characters)"
	}

	// Check for control characters
	for _, r := range name {
		if unicode.IsControl(r) {
			return "Session name contains invalid characters"
		}
	}

	return ""
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
