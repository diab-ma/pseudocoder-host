package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"
)

// currentGitBranch returns the current git branch for the given repo path,
// or an empty string if git is not available or the path is not a repo.
func currentGitBranch(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
	outputGate := newSessionOutputGate(func(sessionID, line string) {
		c.server.BroadcastTerminalOutputWithID(sessionID, line)
	})
	cfg := pty.SessionConfig{
		HistoryLines:   5000, // Default history size
		OnOutputWithID: outputGate.Callback,
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

	startedAt := time.Now()
	if c.server.sessionStore != nil {
		if err := persistSessionRecord(c.server.sessionStore, session.ID, command, args, startedAt); err != nil {
			outputGate.Drop()
			_ = mgr.Close(session.ID)
			c.sendError(apperrors.CodeSessionCreateFailed, fmt.Sprintf("failed to persist session metadata: %v", err))
			return
		}
	}

	// Reserve the next creation sequence only after the create path has fully
	// succeeded. Failed creates must not consume future auto-name slots.
	sessionNumber := c.server.NextCreatedSessionSequence()

	// Determine display name
	name := payload.Name
	if name == "" {
		name = fmt.Sprintf("Session %d", sessionNumber)
	}

	// Broadcast session.created to all clients (includes repo/branch for UI)
	sessionMeta := classifyStructuredChatSession(command, args, "")
	sessionMeta.Repo = c.server.RepoPath()
	sessionMeta.Branch = currentGitBranch(c.server.RepoPath())
	createdMsg := NewSessionCreatedMessage(
		session.ID,
		name,
		command,
		"running",
		startedAt.UnixMilli(),
		sessionMeta,
	)
	c.server.Broadcast(createdMsg)
	outputGate.Activate()

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

	c.server.clearSessionPrompt(payload.SessionID)

	// Stop JSONL pipeline (watcher + reader) if one is running for this session.
	c.server.mu.RLock()
	runtime := c.server.structuredChatRuntime
	c.server.mu.RUnlock()
	if runtime != nil {
		runtime.StopSession(payload.SessionID)
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

	// Try active PTY session first, then fall back to archived session store.
	session := mgr.Get(payload.SessionID)
	isArchived := false
	if session == nil {
		// Check if this is an archived session in the store.
		if c.server.sessionStore != nil {
			storedSession, err := c.server.sessionStore.GetSession(payload.SessionID)
			if err != nil {
				log.Printf("Warning: failed to check session store for %s: %v", payload.SessionID, err)
			}
			if storedSession != nil {
				isArchived = true
			}
		}
		if !isArchived {
			log.Printf("Session not found for switch: %s", payload.SessionID)
			c.sendError(apperrors.CodeSessionNotFound, "session not found")
			return
		}
	}

	// Update client's active session (under lock: handleSessionClose reads/writes
	// activeSessionID for other clients under the same lock).
	c.server.mu.Lock()
	c.activeSessionID = payload.SessionID
	c.server.mu.Unlock()

	c.server.mu.RLock()
	structuredChatEnabled := c.server.structuredChatEnabled
	structuredChatController := c.server.structuredChatController
	c.server.mu.RUnlock()

	if structuredChatEnabled && structuredChatController != nil {
		shouldReplay := true
		if c.server.sessionStore != nil {
			storedSession, err := c.server.sessionStore.GetSession(payload.SessionID)
			if err != nil {
				log.Printf("Warning: failed to load session metadata for structured replay %s: %v", payload.SessionID, err)
			} else if storedSession != nil {
				info, err := SessionInfoFromStoredSession(storedSession)
				if err != nil {
					log.Printf("Warning: failed to decode structured replay metadata for session %s: %v", payload.SessionID, err)
					shouldReplay = false
				} else {
					shouldReplay = info.ChatCapabilities != nil && info.ChatCapabilities.StructuredTimeline
				}
			}
		}
		if shouldReplay {
			snapshotMsg, err := structuredChatController.LoadSnapshotMessage(payload.SessionID)
			if err != nil {
				log.Printf("Warning: failed to load structured chat snapshot for session %s: %v", payload.SessionID, err)
			} else if snapshotMsg != nil {
				c.sendResult(structuredChatController.BuildSessionSwitchReset(payload.SessionID))
				c.sendResult(*snapshotMsg)
			}
		}
	}

	// For archived sessions, send an empty buffer (no PTY to replay).
	if isArchived {
		bufferMsg := NewSessionBufferMessage(payload.SessionID, nil, 0, 0)
		select {
		case <-c.done:
			return
		case c.send <- bufferMsg:
			log.Printf("Sent empty session buffer for archived session: id=%s device=%s", payload.SessionID, c.deviceID)
		case <-time.After(5 * time.Second):
			log.Printf("Warning: timeout sending session buffer to client")
		}
		return
	}

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

// handleSessionClearHistory processes a session.clear_history message.
// It deletes archived sessions from persistent history and returns a
// requester-scoped session.clear_history_result envelope.
func (c *Client) handleSessionClearHistory(data []byte) {
	var msg struct {
		Type    MessageType                `json:"type"`
		ID      string                     `json:"id,omitempty"`
		Payload SessionClearHistoryPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.clear_history payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	requestID := msg.Payload.RequestID
	if requestID == "" {
		log.Printf("session.clear_history missing request_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	c.server.mu.RLock()
	store := c.server.sessionStore
	c.server.mu.RUnlock()
	if store == nil {
		log.Printf("No session store configured, ignoring session.clear_history request")
		c.sendResult(
			NewSessionClearHistoryResultMessage(
				requestID,
				false,
				0,
				apperrors.CodeServerHandlerMissing,
				"session history not configured",
			),
		)
		return
	}

	clearedCount, err := store.ClearArchivedSessions()
	if err != nil {
		log.Printf("Failed to clear archived sessions: %v", err)
		c.sendResult(
			NewSessionClearHistoryResultMessage(
				requestID,
				false,
				0,
				apperrors.CodeStorageQueryFailed,
				"failed to clear archived sessions",
			),
		)
		return
	}

	c.sendResult(
		NewSessionClearHistoryResultMessage(
			requestID,
			true,
			clearedCount,
			"",
			"",
		),
	)

	// Broadcast refreshed session.list to all clients so all UIs converge.
	sessions, err := store.ListSessions(20)
	if err != nil {
		log.Printf("Warning: failed to list sessions after clear_history: %v", err)
		return
	}
	infos := make([]SessionInfo, len(sessions))
	for i, s := range sessions {
		info, err := SessionInfoFromStoredSession(s)
		if err != nil {
			log.Printf("Warning: failed to decode stored session metadata for %s: %v", s.ID, err)
			info = sessionInfoFromData(&SessionData{
				ID:         s.ID,
				Repo:       s.Repo,
				Branch:     s.Branch,
				StartedAt:  s.StartedAt,
				LastSeen:   s.LastSeen,
				LastCommit: s.LastCommit,
				Status:     string(s.Status),
				IsSystem:   s.IsSystem,
			})
		}
		infos[i] = info
	}
	c.server.Broadcast(NewSessionListMessage(infos))
}

// handleSessionClearAll processes a session.clear_all message from the client.
// It deletes all non-system sessions and broadcasts an updated session.list.
func (c *Client) handleSessionClearAll(data []byte) {
	var msg struct {
		Type    MessageType            `json:"type"`
		ID      string                 `json:"id,omitempty"`
		Payload SessionClearAllPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.clear_all payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	requestID := msg.Payload.RequestID
	if requestID == "" {
		log.Printf("session.clear_all missing request_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	c.server.mu.RLock()
	store := c.server.sessionStore
	c.server.mu.RUnlock()
	if store == nil {
		c.sendResult(NewSessionClearAllResultMessage(requestID, false, 0, apperrors.CodeServerHandlerMissing, "session history not configured"))
		return
	}

	clearedCount, err := store.ClearAllSessions()
	if err != nil {
		log.Printf("Failed to clear all sessions: %v", err)
		c.sendResult(NewSessionClearAllResultMessage(requestID, false, 0, apperrors.CodeStorageQueryFailed, "failed to clear sessions"))
		return
	}

	c.sendResult(NewSessionClearAllResultMessage(requestID, true, clearedCount, "", ""))

	// Broadcast refreshed session.list to all clients so all UIs converge.
	sessions, err := store.ListSessions(20)
	if err != nil {
		log.Printf("Warning: failed to list sessions after clear_all: %v", err)
		return
	}
	infos := make([]SessionInfo, len(sessions))
	for i, s := range sessions {
		info, err := SessionInfoFromStoredSession(s)
		if err != nil {
			log.Printf("Warning: failed to decode stored session metadata for %s: %v", s.ID, err)
			info = sessionInfoFromData(&SessionData{
				ID:         s.ID,
				Repo:       s.Repo,
				Branch:     s.Branch,
				StartedAt:  s.StartedAt,
				LastSeen:   s.LastSeen,
				LastCommit: s.LastCommit,
				Status:     string(s.Status),
				IsSystem:   s.IsSystem,
			})
		}
		infos[i] = info
	}
	c.server.Broadcast(NewSessionListMessage(infos))

	log.Printf("Cleared all sessions: count=%d device=%s", clearedCount, c.deviceID)
}

// handleSessionDelete processes a session.delete message from the client.
// It deletes a single archived session and broadcasts an updated session.list.
func (c *Client) handleSessionDelete(data []byte) {
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload SessionDeletePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse session.delete payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	requestID := msg.Payload.RequestID
	sessionID := msg.Payload.SessionID
	if requestID == "" {
		log.Printf("session.delete missing request_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}
	if sessionID == "" {
		log.Printf("session.delete missing session_id")
		c.sendResult(NewSessionDeleteResultMessage(requestID, false, "", apperrors.CodeServerInvalidMessage, "session_id is required"))
		return
	}

	c.server.mu.RLock()
	store := c.server.sessionStore
	c.server.mu.RUnlock()
	if store == nil {
		c.sendResult(NewSessionDeleteResultMessage(requestID, false, sessionID, apperrors.CodeServerHandlerMissing, "session history not configured"))
		return
	}

	// Validate session exists in store.
	storedSession, err := store.GetSession(sessionID)
	if err != nil {
		log.Printf("Failed to look up session %s for delete: %v", sessionID, err)
		c.sendResult(NewSessionDeleteResultMessage(requestID, false, sessionID, apperrors.CodeSessionDeleteFailed, "failed to look up session"))
		return
	}
	if storedSession == nil {
		c.sendResult(NewSessionDeleteResultMessage(requestID, false, sessionID, apperrors.CodeSessionNotFound, "session not found"))
		return
	}

	if err := store.DeleteSession(sessionID); err != nil {
		log.Printf("Failed to delete session %s: %v", sessionID, err)
		c.sendResult(NewSessionDeleteResultMessage(requestID, false, sessionID, apperrors.CodeSessionDeleteFailed, "failed to delete session"))
		return
	}

	c.sendResult(NewSessionDeleteResultMessage(requestID, true, sessionID, "", ""))

	// Broadcast refreshed session.list to all clients so all UIs converge.
	sessions, err := store.ListSessions(20)
	if err != nil {
		log.Printf("Warning: failed to list sessions after delete: %v", err)
		return
	}
	infos := make([]SessionInfo, len(sessions))
	for i, s := range sessions {
		info, err := SessionInfoFromStoredSession(s)
		if err != nil {
			log.Printf("Warning: failed to decode stored session metadata for %s: %v", s.ID, err)
			info = sessionInfoFromData(&SessionData{
				ID:         s.ID,
				Repo:       s.Repo,
				Branch:     s.Branch,
				StartedAt:  s.StartedAt,
				LastSeen:   s.LastSeen,
				LastCommit: s.LastCommit,
				Status:     string(s.Status),
				IsSystem:   s.IsSystem,
			})
		}
		infos[i] = info
	}
	c.server.Broadcast(NewSessionListMessage(infos))

	log.Printf("Session deleted: id=%s device=%s", sessionID, c.deviceID)
}

func (c *Client) handleChatPrompt(data []byte) {
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload ChatPromptPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse chat.prompt payload: %v", err)
		c.sendResult(NewChatPromptResultMessage("", "", false, apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendResult(NewChatPromptResultMessage(payload.SessionID, payload.RequestID, false, apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	fp := chatPromptFingerprint(payload.SessionID, payload.Text)
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeChatPrompt, requestID, fp)
	if replay {
		c.sendResult(cached)
		return
	}
	if err != nil {
		c.sendResult(NewChatPromptResultMessage(payload.SessionID, requestID, false, apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendResult(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeChatPrompt, requestID, fp, result)
		c.sendResult(result)
	}

	if payload.SessionID == "" {
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, apperrors.CodeServerInvalidMessage, "session_id is required"))
		return
	}
	if strings.TrimSpace(payload.Text) == "" {
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, apperrors.CodeServerInvalidMessage, "text is required"))
		return
	}

	if !c.server.beginSessionPrompt(payload.SessionID, c, requestID) {
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, "chat.prompt_in_flight", "prompt already in flight"))
		return
	}
	defer c.server.endSessionPrompt(payload.SessionID, requestID)

	writer, errCode, errMsg := c.resolveChatPromptWriter(payload.SessionID)
	if writer == nil {
		log.Printf("chat.prompt: writer unavailable session=%s code=%s", payload.SessionID, errCode)
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, errCode, errMsg))
		return
	}

	c.server.mu.RLock()
	runtime := c.server.structuredChatRuntime
	c.server.mu.RUnlock()
	if runtime == nil {
		log.Printf("chat.prompt: runtime unavailable session=%s", payload.SessionID)
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, "structured_chat.unavailable", "structured chat unavailable"))
		return
	}

	if err := runtime.SeedPrompt(payload.SessionID, payload.Text, requestID, time.Now().UnixMilli()); err != nil {
		log.Printf("chat.prompt: SeedPrompt failed session=%s: %v", payload.SessionID, err)
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, "structured_chat.unavailable", "structured chat unavailable"))
		return
	}

	// Write the prompt text and Enter as separate PTY writes. TUI apps like
	// Codex (Ink/React) process stdin data events as batches — when text and
	// the Enter keystroke arrive in a single read(), the framework may not
	// recognise the trailing CR as a submit action. Splitting into two writes
	// mimics how real keystrokes arrive (each in its own write) and ensures
	// the Enter is processed as a discrete key event.
	// We use \r (carriage return) because that is the byte the Enter key
	// produces in a terminal; most CLIs (Claude, Codex, Gemini) accept it.
	if _, err := writer.Write([]byte(payload.Text)); err != nil {
		if rollbackErr := runtime.RollbackPrompt(payload.SessionID, requestID); rollbackErr != nil {
			log.Printf("Failed to roll back structured prompt seed for session %s request %s: %v", payload.SessionID, requestID, rollbackErr)
		}
		finish(NewChatPromptResultMessage(payload.SessionID, requestID, false, apperrors.CodeInputWriteFailed, "failed to write prompt"))
		return
	}
	// Small delay so the PTY delivers text and Enter as separate read() calls.
	// TUI frameworks (Ink/React in Codex) may not recognise Enter when it
	// arrives in the same data event as the preceding text.
	time.Sleep(50 * time.Millisecond)
	if _, err := writer.Write([]byte("\r")); err != nil {
		// Text was already written; don't rollback the seed since the
		// prompt text is visible in the terminal. Report success so the
		// mobile client doesn't retry.
		log.Printf("chat.prompt: Enter write failed session=%s: %v", payload.SessionID, err)
	}

	log.Printf("chat.prompt: success session=%s request=%s text_len=%d", payload.SessionID, requestID, len(payload.Text))
	finish(NewChatPromptResultMessage(payload.SessionID, requestID, true, "", ""))
}

func (c *Client) resolveChatPromptWriter(sessionID string) (io.Writer, string, string) {
	c.server.mu.RLock()
	mgr := c.server.sessionManager
	legacyWriter := c.server.ptyWriter
	legacySessionID := c.server.sessionID
	store := c.server.sessionStore
	c.server.mu.RUnlock()

	if store == nil {
		return nil, "structured_chat.unavailable", "structured chat unavailable"
	}
	session, err := store.GetSession(sessionID)
	if err != nil || session == nil {
		return nil, apperrors.CodeSessionNotFound, "session not found"
	}
	info, err := SessionInfoFromStoredSession(session)
	if err != nil || info.SessionKind != SessionKindAgent || info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
		return nil, "structured_chat.unavailable", "structured chat unavailable"
	}

	if mgr != nil {
		if target := mgr.Get(sessionID); target != nil {
			return target, "", ""
		}
	}
	if sessionID == legacySessionID && legacyWriter != nil {
		return legacyWriter, "", ""
	}
	return nil, apperrors.CodeSessionNotFound, "session not found"
}

func (s *Server) beginSessionPrompt(sessionID string, owner *Client, requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.sessionPromptState[sessionID]; exists {
		log.Printf("beginSessionPrompt: rejected session=%s new_request=%s existing_request=%s existing_device=%s",
			sessionID, requestID, existing.RequestID, existing.DeviceID)
		return false
	}
	deviceID := ""
	if owner != nil {
		deviceID = owner.deviceID
	}
	s.sessionPromptState[sessionID] = sessionPromptState{
		Owner:     owner,
		DeviceID:  deviceID,
		RequestID: requestID,
	}
	return true
}

func (s *Server) endSessionPrompt(sessionID, requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessionPromptState[sessionID]
	if !exists || current.RequestID != requestID {
		return
	}
	delete(s.sessionPromptState, sessionID)
}

func (s *Server) clearSessionPrompt(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessionPromptState, sessionID)
}

func (s *Server) clearSessionPromptsForDevice(deviceID string) {
	if deviceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sessionID, state := range s.sessionPromptState {
		if state.DeviceID == deviceID {
			delete(s.sessionPromptState, sessionID)
		}
	}
}

func (s *Server) clearSessionPromptsForClient(owner *Client) {
	if owner == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sessionID, state := range s.sessionPromptState {
		if state.Owner == owner {
			delete(s.sessionPromptState, sessionID)
		}
	}
}

func (s *Server) hasSessionPrompt(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.sessionPromptState[sessionID]
	return exists
}

// sendResult sends a requester-scoped result message to this client.
func (c *Client) sendResult(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending requester-scoped message %s", msg.Type)
	}
}
