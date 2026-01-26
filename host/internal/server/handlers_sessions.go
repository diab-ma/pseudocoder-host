package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"
)

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
