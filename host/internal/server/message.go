// Package server provides the WebSocket server for client connections.
// It handles streaming terminal output and (in future units) review cards
// and other messages between the host and mobile clients.
package server

import (
	"time"
)

// MessageType identifies the kind of message being sent over WebSocket.
// Each type has a specific payload structure defined below.
type MessageType string

const (
	// MessageTypeTerminalAppend sends terminal output to clients.
	// Payload: TerminalAppendPayload
	MessageTypeTerminalAppend MessageType = "terminal.append"

	// MessageTypeDiffCard sends a new review card to clients.
	// Payload: DiffCardPayload
	MessageTypeDiffCard MessageType = "diff.card"

	// MessageTypeCardRemoved notifies clients that a card was removed.
	// This happens when a file is staged/reverted externally (e.g., VS Code).
	// Payload: CardRemovedPayload
	MessageTypeCardRemoved MessageType = "diff.card_removed"

	// MessageTypeReviewDecision is sent by clients to accept/reject a card.
	// Payload: ReviewDecisionPayload
	MessageTypeReviewDecision MessageType = "review.decision"

	// MessageTypeDecisionResult is sent by the server to confirm a decision.
	// Payload: DecisionResultPayload
	MessageTypeDecisionResult MessageType = "decision.result"

	// MessageTypeSessionStatus sends session state updates.
	// Payload: SessionStatusPayload
	MessageTypeSessionStatus MessageType = "session.status"

	// MessageTypeError sends error information to clients.
	// Payload: ErrorPayload
	MessageTypeError MessageType = "error"

	// MessageTypeHeartbeat is used to keep the connection alive.
	// Payload: none (empty object)
	MessageTypeHeartbeat MessageType = "heartbeat"

	// MessageTypeChunkDecision is sent by clients to accept/reject a single chunk.
	// This enables per-chunk decisions within a file card.
	// Payload: ChunkDecisionPayload
	MessageTypeChunkDecision MessageType = "chunk.decision"

	// MessageTypeChunkDecisionResult is sent by the server to confirm a chunk decision.
	// Payload: ChunkDecisionResultPayload
	MessageTypeChunkDecisionResult MessageType = "chunk.decision_result"

	// MessageTypeReviewDelete is sent by clients to delete an untracked file.
	// This is used after the user confirms deletion of a new file that cannot
	// be restored via git. Requires explicit confirmation flag.
	// Payload: ReviewDeletePayload
	MessageTypeReviewDelete MessageType = "review.delete"

	// MessageTypeDeleteResult is sent by the server to confirm a file deletion.
	// Payload: DeleteResultPayload
	MessageTypeDeleteResult MessageType = "delete.result"

	// MessageTypeTerminalInput is sent by clients to send input to the terminal.
	// This enables bidirectional terminal interaction from mobile devices.
	// Payload: TerminalInputPayload
	MessageTypeTerminalInput MessageType = "terminal.input"

	// MessageTypeTerminalResize is sent by clients to resize the terminal PTY.
	// This allows TUI apps to render correctly for the mobile device's screen size.
	// Payload: TerminalResizePayload
	MessageTypeTerminalResize MessageType = "terminal.resize"

	// MessageTypeApprovalRequest is sent by the server to request approval from clients.
	// This is used by CLI tools to broker command approval through the mobile app.
	// Payload: ApprovalRequestPayload
	MessageTypeApprovalRequest MessageType = "approval.request"

	// MessageTypeApprovalDecision is sent by clients to approve or deny a request.
	// This is the response to an approval.request message.
	// Payload: ApprovalDecisionPayload
	MessageTypeApprovalDecision MessageType = "approval.decision"

	// MessageTypeSessionList sends the list of recent sessions to clients.
	// This is sent on client connect after session.status to provide session history.
	// Payload: SessionListPayload
	MessageTypeSessionList MessageType = "session.list"

	// MessageTypeRepoStatus requests or sends git repository status.
	// When received from client, triggers a status refresh.
	// When sent to client, contains current repo state.
	// Payload: RepoStatusPayload
	MessageTypeRepoStatus MessageType = "repo.status"

	// MessageTypeRepoCommit is sent by clients to request a git commit.
	// Payload: RepoCommitPayload
	MessageTypeRepoCommit MessageType = "repo.commit"

	// MessageTypeRepoCommitResult is sent by the server after a commit attempt.
	// Payload: RepoCommitResultPayload
	MessageTypeRepoCommitResult MessageType = "repo.commit_result"

	// MessageTypeRepoPush is sent by clients to request a git push.
	// Payload: RepoPushPayload
	MessageTypeRepoPush MessageType = "repo.push"

	// MessageTypeRepoPushResult is sent by the server after a push attempt.
	// Payload: RepoPushResultPayload
	MessageTypeRepoPushResult MessageType = "repo.push_result"

	// Multi-session PTY management messages (Phase 9)

	// MessageTypeSessionCreate is sent by clients to request a new PTY session.
	// Payload: SessionCreatePayload
	MessageTypeSessionCreate MessageType = "session.create"

	// MessageTypeSessionCreated is sent by the server to confirm session creation.
	// Payload: SessionCreatedPayload
	MessageTypeSessionCreated MessageType = "session.created"

	// MessageTypeSessionClose is sent by clients to close a PTY session.
	// Payload: SessionClosePayload
	MessageTypeSessionClose MessageType = "session.close"

	// MessageTypeSessionClosed is sent by the server to confirm session termination.
	// Payload: SessionClosedPayload
	MessageTypeSessionClosed MessageType = "session.closed"

	// MessageTypeSessionSwitch is sent by clients to change their active session.
	// Payload: SessionSwitchPayload
	MessageTypeSessionSwitch MessageType = "session.switch"

	// MessageTypeSessionRename is sent by clients to rename a session.
	// Payload: SessionRenamePayload
	MessageTypeSessionRename MessageType = "session.rename"

	// MessageTypeSessionBuffer is sent by the server to replay terminal buffer on switch.
	// Payload: SessionBufferPayload
	MessageTypeSessionBuffer MessageType = "session.buffer"

	// tmux session integration messages (Phase 12)

	// MessageTypeTmuxList is sent by clients to request available tmux sessions.
	// Payload: empty (no fields required)
	MessageTypeTmuxList MessageType = "tmux.list"

	// MessageTypeTmuxSessions is sent by the server with the list of tmux sessions.
	// Payload: TmuxSessionsPayload
	MessageTypeTmuxSessions MessageType = "tmux.sessions"

	// MessageTypeTmuxAttach is sent by clients to attach to a tmux session.
	// Payload: TmuxAttachPayload
	MessageTypeTmuxAttach MessageType = "tmux.attach"

	// MessageTypeTmuxAttached is sent by the server to confirm tmux attachment.
	// Includes full session info so mobile can update its state.
	// Payload: TmuxAttachedPayload
	MessageTypeTmuxAttached MessageType = "tmux.attached"

	// MessageTypeTmuxDetach is sent by clients to detach from a tmux session.
	// The PTY is closed but the tmux session continues running.
	// Payload: TmuxDetachPayload
	MessageTypeTmuxDetach MessageType = "tmux.detach"

	// MessageTypeTmuxDetached is sent by the server to confirm tmux detachment.
	// Payload: TmuxDetachedPayload
	MessageTypeTmuxDetached MessageType = "tmux.detached"

	// Undo messages (Phase 20)

	// MessageTypeReviewUndo is sent by clients to undo a file-level decision.
	// This reverses a previous accept or reject, restoring the card to pending.
	// Payload: ReviewUndoPayload
	MessageTypeReviewUndo MessageType = "review.undo"

	// MessageTypeChunkUndo is sent by clients to undo a per-chunk decision.
	// This reverses a previous chunk accept or reject.
	// Payload: ChunkUndoPayload
	MessageTypeChunkUndo MessageType = "chunk.undo"

	// MessageTypeUndoResult is sent by the server to confirm an undo operation.
	// Payload: UndoResultPayload
	MessageTypeUndoResult MessageType = "undo.result"
)

// Message is the envelope for all WebSocket messages.
// Every message has a type and an optional ID for request/response correlation.
// The Payload field contains type-specific data.
type Message struct {
	// Type identifies what kind of message this is.
	Type MessageType `json:"type"`

	// ID is an optional message identifier for correlation.
	// Clients can use this to match responses to requests.
	ID string `json:"id,omitempty"`

	// Payload contains the message-specific data.
	// The structure depends on the Type field.
	Payload interface{} `json:"payload"`
}

// TerminalAppendPayload carries terminal output data.
// This is sent whenever new output is captured from the PTY.
type TerminalAppendPayload struct {
	// SessionID identifies which session this output belongs to.
	// For now we only support one session, but this allows future expansion.
	SessionID string `json:"session_id"`

	// Chunk is the actual terminal output (one or more lines).
	Chunk string `json:"chunk"`

	// Timestamp is when this output was captured (Unix milliseconds).
	Timestamp int64 `json:"timestamp"`
}

// SessionStatusPayload carries session state information.
type SessionStatusPayload struct {
	// SessionID identifies the session.
	SessionID string `json:"session_id"`

	// Status is the current state: "running", "waiting", "complete", "error"
	Status string `json:"status"`

	// LastActivity is the timestamp of the last activity (Unix milliseconds).
	LastActivity int64 `json:"last_activity"`
}

// ErrorPayload carries error information to the client.
type ErrorPayload struct {
	// Code is a stable error code for programmatic handling.
	Code string `json:"code"`

	// Message is a human-readable error description.
	Message string `json:"message"`
}

// ChunkInfo describes the boundaries of a single chunk within a diff.
// This allows clients to extract and display individual chunks,
// and to send per-chunk decisions using the chunk index.
type ChunkInfo struct {
	// Index is the zero-based position of this chunk within the card's diff.
	Index int `json:"index"`

	// OldStart is the starting line number in the original file.
	OldStart int `json:"old_start"`

	// OldCount is the number of lines from the original file.
	OldCount int `json:"old_count"`

	// NewStart is the starting line number in the new file.
	NewStart int `json:"new_start"`

	// NewCount is the number of lines in the new file.
	NewCount int `json:"new_count"`

	// Offset is the byte offset where this chunk starts in the Diff string.
	// Use Diff[Offset:Offset+Length] to extract the chunk content.
	// Deprecated: Use Content field directly to avoid UTF-8/UTF-16 encoding issues.
	Offset int `json:"offset"`

	// Length is the byte length of this chunk's content in the Diff string.
	// Deprecated: Use Content field directly to avoid UTF-8/UTF-16 encoding issues.
	Length int `json:"length"`

	// Content is the raw chunk content (including @@ header).
	// Sent directly to avoid UTF-8/UTF-16 encoding issues with offset extraction.
	Content string `json:"content,omitempty"`

	// ContentHash is the SHA256 hash of the chunk content (first 16 hex chars / 8 bytes).
	// Used to detect stale decisions: client sends this back in chunk.decision,
	// server validates it matches current content before applying.
	ContentHash string `json:"content_hash,omitempty"`

	// GroupIndex is the group number for proximity grouping (0-based).
	// Chunks within configurable proximity (default 20 lines) share the same group.
	// Always included; clients check chunk_groups at card level to determine if grouping is active.
	GroupIndex int `json:"group_index"`
}

// DiffStats provides size metrics for a diff to help mobile clients
// display warnings for large diffs that may be slow to review.
type DiffStats struct {
	// ByteSize is the total size of the diff content in bytes.
	ByteSize int `json:"byte_size"`

	// LineCount is the total number of lines in the diff.
	LineCount int `json:"line_count"`

	// AddedLines is the count of lines starting with '+'.
	AddedLines int `json:"added_lines"`

	// DeletedLines is the count of lines starting with '-'.
	DeletedLines int `json:"deleted_lines"`
}

// ChunkGroupInfo provides metadata about a group of proximity-related chunks.
// Groups collect chunks within a configurable number of lines (default 20),
// allowing mobile clients to display and act on related chunks together.
type ChunkGroupInfo struct {
	// GroupIndex is the 0-based position of this group within the card.
	GroupIndex int `json:"group_index"`

	// LineStart is the starting line number of the group (minimum of member chunks).
	LineStart int `json:"line_start"`

	// LineEnd is the ending line number of the group (maximum of member chunks).
	LineEnd int `json:"line_end"`

	// ChunkCount is the number of chunks in this group.
	ChunkCount int `json:"chunk_count"`
}

// DiffCardPayload carries a review card for file changes.
// This is sent when new changes are detected in the repository.
// A card may contain multiple chunks; use the Chunks field to access them.
type DiffCardPayload struct {
	// CardID is the unique identifier for this review card.
	CardID string `json:"card_id"`

	// File is the path to the file that was changed.
	File string `json:"file"`

	// Diff is the raw diff content (all chunks concatenated).
	Diff string `json:"diff"`

	// Chunks describes the boundaries of each chunk within the Diff.
	// Clients can use this to display and act on individual chunks.
	// May be nil for backward compatibility with older hosts.
	Chunks []ChunkInfo `json:"chunks,omitempty"`

	// ChunkGroups provides metadata about proximity-based chunk groups.
	// When present, chunks are grouped by line proximity (configurable, default 20 lines).
	// Each chunk's GroupIndex references an entry in this array.
	// Missing or empty means grouping is disabled; render chunks as flat list.
	ChunkGroups []ChunkGroupInfo `json:"chunk_groups,omitempty"`

	// IsBinary indicates the file is a binary file.
	// Binary files cannot use per-chunk actions - only file-level accept/reject.
	// The Diff field may be empty or contain a placeholder for binary files.
	IsBinary bool `json:"is_binary,omitempty"`

	// IsDeleted indicates this is a file deletion.
	// File deletions should use file-level actions - per-chunk doesn't apply.
	IsDeleted bool `json:"is_deleted,omitempty"`

	// Stats provides size metrics for large diff warnings.
	// May be nil for small diffs or backward compatibility.
	Stats *DiffStats `json:"stats,omitempty"`

	// CreatedAt is when this card was created (Unix milliseconds).
	CreatedAt int64 `json:"created_at"`
}

// CardRemovedPayload notifies that a card was removed externally.
// This is sent when changes are staged/reverted outside the mobile app.
type CardRemovedPayload struct {
	// CardID is the unique identifier of the removed card.
	CardID string `json:"card_id"`
}

// ReviewDecisionPayload carries a user's accept/reject decision.
// This is sent from the mobile client to the host when the user
// acts on a review card.
type ReviewDecisionPayload struct {
	// CardID is the unique identifier of the card being decided.
	CardID string `json:"card_id"`

	// Action is either "accept" or "reject".
	Action string `json:"action"`

	// Comment is an optional explanation for the decision.
	Comment string `json:"comment,omitempty"`
}

// DecisionResultPayload carries the result of processing a decision.
// This is sent back to the client to confirm the action was applied.
type DecisionResultPayload struct {
	// CardID is the unique identifier of the decided card.
	CardID string `json:"card_id"`

	// Action is the action that was taken ("accept" or "reject").
	Action string `json:"action"`

	// Success indicates whether the action was applied successfully.
	Success bool `json:"success"`

	// ErrorCode is a stable error code if Success is false.
	// Clients can use this for programmatic error handling.
	// Format: {domain}.{error} (e.g., "storage.not_found", "action.git_failed")
	ErrorCode string `json:"error_code,omitempty"`

	// Error contains a human-readable error message if Success is false.
	Error string `json:"error,omitempty"`
}

// ChunkDecisionPayload carries a user's accept/reject decision for a single chunk.
// This enables per-chunk decisions within a file card, allowing granular control
// over which changes to stage or restore.
type ChunkDecisionPayload struct {
	// CardID is the unique identifier of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based index of the chunk within the card.
	// This corresponds to the Index field in ChunkInfo from the diff.card.
	ChunkIndex int `json:"chunk_index"`

	// Action is either "accept" or "reject".
	Action string `json:"action"`

	// Comment is an optional explanation for the decision.
	Comment string `json:"comment,omitempty"`

	// ContentHash is the hash of the chunk content when the user viewed it.
	// Server validates this matches current content to prevent stale decisions.
	// If empty, validation is skipped (backward compatibility during transition).
	ContentHash string `json:"content_hash,omitempty"`
}

// ChunkDecisionResultPayload carries the result of processing a chunk decision.
// This is sent back to the client to confirm the action was applied.
type ChunkDecisionResultPayload struct {
	// CardID is the unique identifier of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based index of the decided chunk.
	ChunkIndex int `json:"chunk_index"`

	// Action is the action that was taken ("accept" or "reject").
	Action string `json:"action"`

	// Success indicates whether the action was applied successfully.
	Success bool `json:"success"`

	// ErrorCode is a stable error code if Success is false.
	ErrorCode string `json:"error_code,omitempty"`

	// Error contains a human-readable error message if Success is false.
	Error string `json:"error,omitempty"`
}

// ReviewDeletePayload carries a request to delete an untracked file.
// This is used when the user confirms deletion of a new file that cannot
// be restored via git (because it has never been committed).
type ReviewDeletePayload struct {
	// CardID is the unique identifier of the card for the file to delete.
	CardID string `json:"card_id"`

	// Confirmed must be true to proceed with deletion.
	// This is a safety flag to ensure the user explicitly confirmed.
	Confirmed bool `json:"confirmed"`
}

// DeleteResultPayload carries the result of a file deletion request.
// This is sent back to the client after processing a review.delete message.
type DeleteResultPayload struct {
	// CardID is the unique identifier of the deleted card.
	CardID string `json:"card_id"`

	// Success indicates whether the file was deleted successfully.
	Success bool `json:"success"`

	// ErrorCode is a stable error code if Success is false.
	ErrorCode string `json:"error_code,omitempty"`

	// Error contains a human-readable error message if Success is false.
	Error string `json:"error,omitempty"`
}

// TerminalInputPayload carries terminal input from mobile client.
// This enables bidirectional terminal interaction: mobile can send
// keystrokes and commands to the PTY running on the host.
type TerminalInputPayload struct {
	// SessionID identifies which session to send input to.
	// Must match the current session ID to prevent input to wrong session.
	SessionID string `json:"session_id"`

	// Data is the UTF-8 input to send to the terminal.
	// This can be a single character, an escape sequence, or a command.
	Data string `json:"data"`

	// Timestamp is when the input was sent (Unix milliseconds).
	// Used for latency tracking and debugging.
	Timestamp int64 `json:"timestamp"`
}

// TerminalResizePayload carries terminal resize dimensions from mobile client.
// This allows the host PTY to be resized to match the mobile device's screen,
// ensuring TUI applications render correctly.
type TerminalResizePayload struct {
	// SessionID identifies which session to resize.
	SessionID string `json:"session_id"`

	// Cols is the number of columns (character width) of the terminal.
	// Must be greater than 0.
	Cols int `json:"cols"`

	// Rows is the number of rows (character height) of the terminal.
	// Must be greater than 0.
	Rows int `json:"rows"`
}

// ApprovalRequestPayload carries an approval request from the host to mobile.
// This is used by CLI tools (like Claude Code) to broker command approval
// through the mobile app. The user can approve or deny the request.
type ApprovalRequestPayload struct {
	// RequestID is the unique identifier for this approval request (UUID).
	RequestID string `json:"request_id"`

	// Command is the command that is requesting approval.
	// This is shown to the user so they can make an informed decision.
	Command string `json:"command"`

	// Cwd is the working directory where the command will run.
	Cwd string `json:"cwd"`

	// Repo is the repository path for the current session.
	Repo string `json:"repo"`

	// Rationale explains why the command needs to run.
	// This helps the user understand the context of the request.
	Rationale string `json:"rationale"`

	// ExpiresAt is the RFC3339 timestamp when this request expires.
	// If no decision is made by this time, the request is auto-denied.
	ExpiresAt string `json:"expires_at"`
}

// ApprovalDecisionPayload carries the user's decision on an approval request.
// This is sent from the mobile client back to the host in response to an
// approval.request message.
type ApprovalDecisionPayload struct {
	// RequestID is the unique identifier of the approval request being decided.
	RequestID string `json:"request_id"`

	// Decision is either "approve" or "deny".
	Decision string `json:"decision"`

	// TemporaryAllowUntil is an optional RFC3339 timestamp.
	// If set, similar commands are auto-approved until this time.
	// This enables "allow for 15 minutes" type functionality.
	TemporaryAllowUntil string `json:"temporary_allow_until,omitempty"`
}

// SessionListPayload carries the list of recent sessions.
// This is sent to clients on connect to provide session history.
type SessionListPayload struct {
	// Sessions is the list of recent sessions, newest first.
	Sessions []SessionInfo `json:"sessions"`
}

// SessionInfo represents a session in the session.list message.
// This mirrors the storage.Session struct but uses Unix milliseconds for timestamps.
type SessionInfo struct {
	// ID is the unique session identifier.
	ID string `json:"id"`

	// Repo is the repository path.
	Repo string `json:"repo"`

	// Branch is the git branch name.
	Branch string `json:"branch"`

	// StartedAt is when the session started (Unix milliseconds).
	StartedAt int64 `json:"started_at"`

	// LastSeen is the most recent activity (Unix milliseconds).
	LastSeen int64 `json:"last_seen"`

	// LastCommit is the most recent commit hash (optional).
	LastCommit string `json:"last_commit,omitempty"`

	// Status is the session state: "running", "complete", or "error".
	Status string `json:"status"`
}

// RepoStatusPayload carries git repository status information.
// This includes branch, upstream, staged/unstaged counts, and last commit info.
type RepoStatusPayload struct {
	// Branch is the current git branch name.
	Branch string `json:"branch"`

	// Upstream is the remote tracking branch (e.g., "origin/main").
	// Empty if no upstream is configured.
	Upstream string `json:"upstream,omitempty"`

	// StagedCount is the number of files with staged changes.
	StagedCount int `json:"staged_count"`

	// StagedFiles is the list of file paths with staged changes.
	// Paths are relative to the repository root.
	StagedFiles []string `json:"staged_files"`

	// UnstagedCount is the number of files with unstaged changes.
	UnstagedCount int `json:"unstaged_count"`

	// LastCommit is the short hash and subject of the most recent commit.
	// Format: "abc1234 Commit message subject"
	LastCommit string `json:"last_commit,omitempty"`
}

// RepoCommitPayload carries a commit request from the mobile client.
// The host will create a git commit with the specified message.
type RepoCommitPayload struct {
	// Message is the commit message. Must not be empty.
	Message string `json:"message"`

	// NoVerify skips pre-commit and commit-msg hooks.
	// Only honored if the host allows it via --commit-allow-no-verify.
	NoVerify bool `json:"no_verify,omitempty"`

	// NoGpgSign skips GPG signing of the commit.
	// Only honored if the host allows it via --commit-allow-no-gpg-sign.
	NoGpgSign bool `json:"no_gpg_sign,omitempty"`
}

// RepoCommitResultPayload carries the result of a commit attempt.
// On success, includes the commit hash and summary.
// On failure, includes error code and message.
type RepoCommitResultPayload struct {
	// Success indicates whether the commit was created successfully.
	Success bool `json:"success"`

	// Hash is the full commit hash (40 hex chars) on success.
	Hash string `json:"hash,omitempty"`

	// Summary is a short description of what was committed.
	// Format: "1 file changed, 5 insertions(+), 2 deletions(-)"
	Summary string `json:"summary,omitempty"`

	// ErrorCode is a stable error code if Success is false.
	// Codes: commit.no_staged_changes, commit.empty_message, commit.hook_failed, commit.git_error
	ErrorCode string `json:"error_code,omitempty"`

	// Error contains a human-readable error message if Success is false.
	Error string `json:"error,omitempty"`
}

// RepoPushPayload carries a push request from the mobile client.
// The host will push commits using the specified remote/branch or configured upstream.
type RepoPushPayload struct {
	// Remote is the remote name (e.g., "origin"). Optional if upstream is configured.
	Remote string `json:"remote,omitempty"`

	// Branch is the remote branch name. Optional if upstream is configured.
	Branch string `json:"branch,omitempty"`

	// ForceWithLease uses force-with-lease instead of regular push.
	// Only honored if the host allows it via --push-allow-force-with-lease.
	ForceWithLease bool `json:"force_with_lease,omitempty"`
}

// RepoPushResultPayload carries the result of a push attempt.
// On success, includes the output summary.
// On failure, includes error code and message.
type RepoPushResultPayload struct {
	// Success indicates whether the push succeeded.
	Success bool `json:"success"`

	// Output is the git push output on success (e.g., "To origin/main abc1234..def5678").
	Output string `json:"output,omitempty"`

	// ErrorCode is a stable error code if Success is false.
	// Codes: push.no_upstream, push.non_ff, push.auth_failed, push.git_error
	ErrorCode string `json:"error_code,omitempty"`

	// Error contains a human-readable error message if Success is false.
	Error string `json:"error,omitempty"`
}

// Multi-session PTY management payloads (Phase 9)

// SessionCreatePayload carries a request to create a new PTY session.
// All fields are optional - the host will use defaults if not provided.
type SessionCreatePayload struct {
	// Command is the command to run in the new session.
	// If empty, the host uses its default command.
	Command string `json:"command,omitempty"`

	// Args are the command arguments.
	Args []string `json:"args,omitempty"`

	// Name is a display name for the session.
	// If empty, the host generates one (e.g., "Session 1").
	Name string `json:"name,omitempty"`
}

// SessionCreatedPayload confirms that a new session was created.
// Sent in response to session.create messages.
type SessionCreatedPayload struct {
	// SessionID is the unique identifier for the new session (UUID).
	SessionID string `json:"session_id"`

	// Name is the display name for the session.
	Name string `json:"name,omitempty"`

	// Command is the command running in the session.
	Command string `json:"command,omitempty"`

	// Status is the current session state: "running", "waiting", etc.
	Status string `json:"status"`

	// CreatedAt is when the session was created (Unix milliseconds).
	CreatedAt int64 `json:"created_at"`
}

// SessionClosePayload carries a request to close a PTY session.
type SessionClosePayload struct {
	// SessionID identifies the session to close.
	SessionID string `json:"session_id"`
}

// SessionClosedPayload confirms that a session was closed.
// Sent in response to session.close or when a session exits naturally.
type SessionClosedPayload struct {
	// SessionID identifies the session that was closed.
	SessionID string `json:"session_id"`

	// Reason explains why the session was closed.
	// Examples: "user_requested", "command_exited", "error"
	Reason string `json:"reason,omitempty"`
}

// SessionSwitchPayload carries a request to change the active session.
// The server responds with a session.buffer message to replay history.
type SessionSwitchPayload struct {
	// SessionID identifies the session to switch to.
	SessionID string `json:"session_id"`
}

// SessionRenamePayload carries a request to rename a session.
type SessionRenamePayload struct {
	// SessionID identifies the session to rename.
	SessionID string `json:"session_id"`

	// Name is the new display name for the session.
	Name string `json:"name"`
}

// SessionBufferPayload carries terminal buffer content for replay.
// Sent when a client switches to a session to restore terminal state.
type SessionBufferPayload struct {
	// SessionID identifies which session this buffer belongs to.
	SessionID string `json:"session_id"`

	// Lines contains the terminal buffer content (last N lines).
	Lines []string `json:"lines"`

	// CursorRow is the current cursor row position (0-based).
	CursorRow int `json:"cursor_row"`

	// CursorCol is the current cursor column position (0-based).
	CursorCol int `json:"cursor_col"`
}

// NewTerminalAppendMessage creates a message for terminal output.
// This is a convenience function to ensure consistent message creation.
func NewTerminalAppendMessage(sessionID, chunk string) Message {
	return Message{
		Type: MessageTypeTerminalAppend,
		Payload: TerminalAppendPayload{
			SessionID: sessionID,
			Chunk:     chunk,
			Timestamp: time.Now().UnixMilli(),
		},
	}
}

// NewSessionStatusMessage creates a message for session status updates.
func NewSessionStatusMessage(sessionID, status string) Message {
	return Message{
		Type: MessageTypeSessionStatus,
		Payload: SessionStatusPayload{
			SessionID:    sessionID,
			Status:       status,
			LastActivity: time.Now().UnixMilli(),
		},
	}
}

// NewErrorMessage creates an error message to send to clients.
func NewErrorMessage(code, message string) Message {
	return Message{
		Type: MessageTypeError,
		Payload: ErrorPayload{
			Code:    code,
			Message: message,
		},
	}
}

// NewHeartbeatMessage creates a heartbeat message for keep-alive.
func NewHeartbeatMessage() Message {
	return Message{
		Type:    MessageTypeHeartbeat,
		Payload: struct{}{},
	}
}

// NewDiffCardMessage creates a message for a review card.
// The chunks parameter is optional; pass nil for backward compatibility.
// The chunkGroups parameter provides proximity grouping metadata (nil when disabled).
// The isBinary, isDeleted, and stats parameters are optional for backward compatibility.
func NewDiffCardMessage(cardID, file, diff string, chunks []ChunkInfo, chunkGroups []ChunkGroupInfo, isBinary, isDeleted bool, stats *DiffStats, createdAt int64) Message {
	return Message{
		Type: MessageTypeDiffCard,
		Payload: DiffCardPayload{
			CardID:      cardID,
			File:        file,
			Diff:        diff,
			Chunks:      chunks,
			ChunkGroups: chunkGroups,
			IsBinary:    isBinary,
			IsDeleted:   isDeleted,
			Stats:       stats,
			CreatedAt:   createdAt,
		},
	}
}

// NewCardRemovedMessage creates a message notifying card removal.
// This is sent when changes are staged/reverted externally.
func NewCardRemovedMessage(cardID string) Message {
	return Message{
		Type: MessageTypeCardRemoved,
		Payload: CardRemovedPayload{
			CardID: cardID,
		},
	}
}

// NewDecisionResultMessage creates a message confirming a decision result.
// This is sent back to the client after processing a review.decision message.
// Use errCode and errMsg for failure cases; both should be empty for success.
func NewDecisionResultMessage(cardID, action string, success bool, errCode, errMsg string) Message {
	return Message{
		Type: MessageTypeDecisionResult,
		Payload: DecisionResultPayload{
			CardID:    cardID,
			Action:    action,
			Success:   success,
			ErrorCode: errCode,
			Error:     errMsg,
		},
	}
}

// NewChunkDecisionResultMessage creates a message confirming a per-chunk decision.
// This is sent back to the client after processing a chunk.decision message.
// Use errCode and errMsg for failure cases; both should be empty for success.
func NewChunkDecisionResultMessage(cardID string, chunkIndex int, action string, success bool, errCode, errMsg string) Message {
	return Message{
		Type: MessageTypeChunkDecisionResult,
		Payload: ChunkDecisionResultPayload{
			CardID:     cardID,
			ChunkIndex: chunkIndex,
			Action:     action,
			Success:    success,
			ErrorCode:  errCode,
			Error:      errMsg,
		},
	}
}

// NewDeleteResultMessage creates a message confirming a file deletion.
// This is sent back to the client after processing a review.delete message.
// Use errCode and errMsg for failure cases; both should be empty for success.
func NewDeleteResultMessage(cardID string, success bool, errCode, errMsg string) Message {
	return Message{
		Type: MessageTypeDeleteResult,
		Payload: DeleteResultPayload{
			CardID:    cardID,
			Success:   success,
			ErrorCode: errCode,
			Error:     errMsg,
		},
	}
}

// NewApprovalRequestMessage creates an approval request message.
// This is sent to connected mobile clients when a CLI tool requests approval
// for a command. The expiresAt time is formatted as RFC3339.
func NewApprovalRequestMessage(requestID, command, cwd, repo, rationale string, expiresAt time.Time) Message {
	return Message{
		Type: MessageTypeApprovalRequest,
		Payload: ApprovalRequestPayload{
			RequestID: requestID,
			Command:   command,
			Cwd:       cwd,
			Repo:      repo,
			Rationale: rationale,
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
	}
}

// NewSessionListMessage creates a session list message from storage sessions.
// This converts the storage Session structs to SessionInfo payloads suitable
// for transmission to mobile clients.
func NewSessionListMessage(sessions []SessionInfo) Message {
	return Message{
		Type: MessageTypeSessionList,
		Payload: SessionListPayload{
			Sessions: sessions,
		},
	}
}

// SessionsToInfoList converts storage.Session pointers to SessionInfo list.
// This is a helper for the common pattern of calling NewSessionListMessage
// with data from storage.ListSessions().
func SessionsToInfoList(sessions []*SessionData) []SessionInfo {
	infos := make([]SessionInfo, len(sessions))
	for i, s := range sessions {
		infos[i] = SessionInfo{
			ID:         s.ID,
			Repo:       s.Repo,
			Branch:     s.Branch,
			StartedAt:  s.StartedAt.UnixMilli(),
			LastSeen:   s.LastSeen.UnixMilli(),
			LastCommit: s.LastCommit,
			Status:     s.Status,
		}
	}
	return infos
}

// SessionData represents session data for conversion to SessionInfo.
// This interface avoids importing the storage package to prevent import cycles.
type SessionData struct {
	ID         string
	Repo       string
	Branch     string
	StartedAt  time.Time
	LastSeen   time.Time
	LastCommit string
	Status     string
}

// NewRepoStatusMessage creates a message with current repository status.
// This is sent in response to a repo.status request from the client.
func NewRepoStatusMessage(branch, upstream string, stagedCount int, stagedFiles []string, unstagedCount int, lastCommit string) Message {
	return Message{
		Type: MessageTypeRepoStatus,
		Payload: RepoStatusPayload{
			Branch:        branch,
			Upstream:      upstream,
			StagedCount:   stagedCount,
			StagedFiles:   stagedFiles,
			UnstagedCount: unstagedCount,
			LastCommit:    lastCommit,
		},
	}
}

// NewRepoCommitResultMessage creates a message with commit result.
// On success, include hash and summary. On failure, include error code and message.
func NewRepoCommitResultMessage(success bool, hash, summary, errCode, errMsg string) Message {
	return Message{
		Type: MessageTypeRepoCommitResult,
		Payload: RepoCommitResultPayload{
			Success:   success,
			Hash:      hash,
			Summary:   summary,
			ErrorCode: errCode,
			Error:     errMsg,
		},
	}
}

// NewRepoPushResultMessage creates a message with push result.
// On success, include output. On failure, include error code and message.
func NewRepoPushResultMessage(success bool, output, errCode, errMsg string) Message {
	return Message{
		Type: MessageTypeRepoPushResult,
		Payload: RepoPushResultPayload{
			Success:   success,
			Output:    output,
			ErrorCode: errCode,
			Error:     errMsg,
		},
	}
}

// Multi-session PTY message constructors (Phase 9)

// NewSessionCreatedMessage creates a message confirming session creation.
// This is sent to clients in response to a session.create request.
func NewSessionCreatedMessage(sessionID, name, command, status string, createdAt int64) Message {
	return Message{
		Type: MessageTypeSessionCreated,
		Payload: SessionCreatedPayload{
			SessionID: sessionID,
			Name:      name,
			Command:   command,
			Status:    status,
			CreatedAt: createdAt,
		},
	}
}

// NewSessionClosedMessage creates a message confirming session termination.
// This is sent when a session is closed (by request or naturally).
func NewSessionClosedMessage(sessionID, reason string) Message {
	return Message{
		Type: MessageTypeSessionClosed,
		Payload: SessionClosedPayload{
			SessionID: sessionID,
			Reason:    reason,
		},
	}
}

// NewSessionBufferMessage creates a message with terminal buffer content.
// This is sent when a client switches to a session to replay history.
func NewSessionBufferMessage(sessionID string, lines []string, cursorRow, cursorCol int) Message {
	return Message{
		Type: MessageTypeSessionBuffer,
		Payload: SessionBufferPayload{
			SessionID: sessionID,
			Lines:     lines,
			CursorRow: cursorRow,
			CursorCol: cursorCol,
		},
	}
}

// tmux session integration payloads and constructors (Phase 12)

// TmuxSessionInfo represents a tmux session in the tmux.sessions message.
// This is the wire format for tmux session metadata, using Unix milliseconds
// for timestamps to match the protocol convention.
type TmuxSessionInfo struct {
	// Name is the tmux session name (e.g., "main", "dev", "0").
	Name string `json:"name"`

	// Windows is the number of windows in this session.
	Windows int `json:"windows"`

	// Attached indicates whether another client is currently attached.
	Attached bool `json:"attached"`

	// CreatedAt is when this tmux session was created (Unix milliseconds).
	CreatedAt int64 `json:"created_at"`
}

// TmuxSessionsPayload carries the list of available tmux sessions.
// Sent in response to tmux.list requests.
type TmuxSessionsPayload struct {
	// Sessions is the list of available tmux sessions.
	Sessions []TmuxSessionInfo `json:"sessions"`

	// ErrorCode is a stable error code if the request failed.
	// Examples: "tmux.not_installed", "tmux.no_server"
	// Empty string on success.
	ErrorCode string `json:"error_code,omitempty"`
}

// NewTmuxSessionsMessage creates a tmux.sessions message with session list.
// Use errorCode for failure cases (e.g., "tmux.not_installed"); empty for success.
func NewTmuxSessionsMessage(sessions []TmuxSessionInfo, errorCode string) Message {
	return Message{
		Type: MessageTypeTmuxSessions,
		Payload: TmuxSessionsPayload{
			Sessions:  sessions,
			ErrorCode: errorCode,
		},
	}
}

// TmuxAttachPayload carries a request to attach to an existing tmux session.
// The client sends this to start a PTY session attached to a tmux session.
type TmuxAttachPayload struct {
	// TmuxSession is the name of the tmux session to attach to.
	TmuxSession string `json:"tmux_session"`
}

// TmuxAttachedPayload carries the confirmation of a tmux session attachment.
// This is similar to SessionCreatedPayload but includes tmux-specific fields.
type TmuxAttachedPayload struct {
	// SessionID is the unique identifier for the new PTY session (UUID).
	SessionID string `json:"session_id"`

	// TmuxSession is the name of the tmux session that was attached.
	TmuxSession string `json:"tmux_session"`

	// IsTmux is always true for tmux attachments.
	// Mobile uses this to distinguish tmux sessions from regular PTY sessions.
	IsTmux bool `json:"is_tmux"`

	// Name is the display name for the session.
	// For tmux sessions, this defaults to the tmux session name.
	Name string `json:"name,omitempty"`

	// Command is the command running (always "tmux attach-session ...").
	Command string `json:"command,omitempty"`

	// Status is the current session state: "running".
	Status string `json:"status"`

	// CreatedAt is when the session was created (Unix milliseconds).
	CreatedAt int64 `json:"created_at"`
}

// NewTmuxAttachedMessage creates a tmux.attached message.
// This is sent after successfully attaching to a tmux session.
func NewTmuxAttachedMessage(sessionID, tmuxSession, name, command, status string, createdAt int64) Message {
	return Message{
		Type: MessageTypeTmuxAttached,
		Payload: TmuxAttachedPayload{
			SessionID:   sessionID,
			TmuxSession: tmuxSession,
			IsTmux:      true,
			Name:        name,
			Command:     command,
			Status:      status,
			CreatedAt:   createdAt,
		},
	}
}

// TmuxDetachPayload carries a request to detach from a tmux session.
// Detaching closes the PTY but preserves the tmux session for later re-attach.
type TmuxDetachPayload struct {
	// SessionID is the PTY session ID (UUID) to detach.
	SessionID string `json:"session_id"`

	// Kill optionally destroys the tmux session instead of just detaching.
	// When true, both the PTY and the underlying tmux session are terminated.
	Kill bool `json:"kill,omitempty"`
}

// TmuxDetachedPayload carries the confirmation of tmux session detachment.
type TmuxDetachedPayload struct {
	// SessionID is the PTY session ID that was detached.
	SessionID string `json:"session_id"`

	// TmuxSession is the name of the tmux session that was detached from.
	TmuxSession string `json:"tmux_session"`

	// Killed indicates whether the tmux session was also destroyed.
	Killed bool `json:"killed"`
}

// NewTmuxDetachedMessage creates a tmux.detached message.
func NewTmuxDetachedMessage(sessionID, tmuxSession string, killed bool) Message {
	return Message{
		Type: MessageTypeTmuxDetached,
		Payload: TmuxDetachedPayload{
			SessionID:   sessionID,
			TmuxSession: tmuxSession,
			Killed:      killed,
		},
	}
}

// -----------------------------------------------------------------------------
// Undo Messages (Phase 20)
// -----------------------------------------------------------------------------

// ReviewUndoPayload carries a request to undo a file-level decision.
// This reverses a previous accept or reject action, restoring the card to pending.
type ReviewUndoPayload struct {
	// CardID is the unique identifier of the card to undo.
	CardID string `json:"card_id"`

	// Confirmed indicates explicit user confirmation for committed card undo.
	// Required when undoing cards that have been committed (requires git operations).
	Confirmed bool `json:"confirmed,omitempty"`
}

// ChunkUndoPayload carries a request to undo a per-chunk decision.
// This reverses a previous chunk accept or reject action.
type ChunkUndoPayload struct {
	// CardID is the unique identifier of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based index of the chunk to undo.
	ChunkIndex int `json:"chunk_index"`

	// Confirmed indicates explicit user confirmation for committed chunk undo.
	// Required when undoing chunks that have been committed.
	Confirmed bool `json:"confirmed,omitempty"`
}

// UndoResultPayload carries the result of an undo operation.
// This is sent in response to review.undo or chunk.undo messages.
type UndoResultPayload struct {
	// CardID is the unique identifier of the card that was undone.
	CardID string `json:"card_id"`

	// ChunkIndex is the chunk index if this was a per-chunk undo.
	// Omitted (zero value) for file-level undos.
	ChunkIndex int `json:"chunk_index,omitempty"`

	// Success indicates whether the undo operation succeeded.
	Success bool `json:"success"`

	// ErrorCode is a stable error code for programmatic handling.
	// Only present when Success is false.
	ErrorCode string `json:"error_code,omitempty"`

	// Error is a human-readable error description.
	// Only present when Success is false.
	Error string `json:"error,omitempty"`
}

// NewUndoResultMessage creates an undo.result message.
// For file-level undos, set chunkIndex to -1 (it will be omitted from JSON).
// For chunk-level undos, set chunkIndex to the actual chunk index.
func NewUndoResultMessage(cardID string, chunkIndex int, success bool, errCode, errMsg string) Message {
	payload := UndoResultPayload{
		CardID:    cardID,
		Success:   success,
		ErrorCode: errCode,
		Error:     errMsg,
	}
	// Only include chunk index for chunk-level undos (non-negative)
	if chunkIndex >= 0 {
		payload.ChunkIndex = chunkIndex
	}
	return Message{
		Type:    MessageTypeUndoResult,
		Payload: payload,
	}
}
