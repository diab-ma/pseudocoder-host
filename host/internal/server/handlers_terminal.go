package server

import (
	"encoding/json"
	"log"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"
)

// handleTerminalInput processes a terminal.input message from the client.
// This enables bidirectional terminal: mobile clients can send keystrokes
// and commands to the PTY running on the host.
// Phase 5.5: Single session support.
// Phase 9.3: Multi-session routing via SessionManager.
func (c *Client) handleTerminalInput(data []byte) {
	start := time.Now()
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
				return
			}
			c.recordTerminalLatency(start)
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
	c.recordTerminalLatency(start)
}

// recordTerminalLatency records terminal input latency to the metrics store.
func (c *Client) recordTerminalLatency(start time.Time) {
	c.server.mu.RLock()
	ms := c.server.metricsStore
	c.server.mu.RUnlock()
	if ms == nil {
		return
	}
	elapsed := time.Since(start).Milliseconds()
	if err := ms.RecordLatency("terminal_input", elapsed); err != nil {
		log.Printf("metrics: failed to record terminal latency: %v", err)
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
