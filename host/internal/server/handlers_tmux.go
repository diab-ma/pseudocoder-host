package server

import (
	"encoding/json"
	"fmt"
	"log"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"
)

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
