// Package errors provides standardized error codes for the host application.
//
// Error codes follow the format {domain}.{error} where:
//   - domain: The subsystem that generated the error (storage, action, server, session)
//   - error: The specific error type within that domain
//
// These codes are stable and can be used by mobile clients for programmatic
// error handling. Human-readable messages are provided alongside codes.
package errors

import (
	"errors"
	"fmt"
	"strings"
)

// Error codes by domain.
// These are stable identifiers that mobile clients can rely on for error handling.
const (
	// Storage domain - database and persistence errors
	CodeStorageNotFound       = "storage.not_found"       // Card or resource not found
	CodeStorageAlreadyExists  = "storage.already_exists"  // Resource already exists
	CodeStorageAlreadyDecided = "storage.already_decided" // Card already has a decision
	CodeStorageOpenFailed     = "storage.open_failed"     // Database open failed
	CodeStorageQueryFailed    = "storage.query_failed"    // Database query failed
	CodeStorageSaveFailed     = "storage.save_failed"     // Failed to save data

	// Action domain - git operations and decision processing
	CodeActionInvalid        = "action.invalid"         // Invalid action (not accept/reject)
	CodeActionGitFailed      = "action.git_failed"      // Git operation failed
	CodeActionCardNotFound   = "action.card_not_found"  // Card not found for action
	CodeActionAlreadyDecided = "action.already_decided" // Card already has a decision
	CodeActionChunkStale     = "action.chunk_stale"     // Chunk content has changed since viewed
	CodeActionUntrackedFile  = "action.untracked_file"  // File is untracked (cannot restore)
	CodeActionFileDeleted    = "action.file_deleted"    // File was deleted from disk
	CodeActionBinaryFile     = "action.binary_file"     // Cannot apply per-chunk actions to binary files

	// Decision reliability domain - validation/verification and divergence
	CodeValidationFailed   = "validation.failed"   // Pre-apply validation failed
	CodeVerificationFailed = "verification.failed" // Post-apply verification failed
	CodeStateDiverged      = "state.diverged"      // Storage and git state diverged
	CodeConflictDetected   = "conflict.detected"   // Conflict detected during apply/verify

	// Server domain - WebSocket and network errors
	CodeServerUpgradeFailed  = "server.upgrade_failed"  // WebSocket upgrade failed
	CodeServerInvalidMessage = "server.invalid_message" // Malformed or invalid message
	CodeServerHandlerMissing = "server.handler_missing" // No handler for message type
	CodeServerSendFailed     = "server.send_failed"     // Failed to send message
	CodeServerConnectionLost = "server.connection_lost" // Connection unexpectedly closed

	// Session domain - PTY and process errors
	CodeSessionAlreadyRunning = "session.already_running" // Session already started
	CodeSessionNotRunning     = "session.not_running"     // Session not started
	CodeSessionSpawnFailed    = "session.spawn_failed"    // Failed to spawn PTY
	CodeSessionWriteFailed    = "session.write_failed"    // Failed to write to PTY
	CodeSessionNotFound       = "session.not_found"       // Session ID does not exist (Phase 9.3)
	CodeSessionCreateFailed   = "session.create_failed"   // Failed to create session (Phase 9.3)
	CodeSessionStartFailed    = "session.start_failed"    // Failed to start session command (Phase 9.3)
	CodeSessionCloseFailed    = "session.close_failed"    // Failed to close session (Phase 9.3)
	CodeSessionRenameFailed   = "session.rename_failed"   // Failed to rename session (Phase 9.6)

	// Input domain - terminal input errors (Phase 5.5)
	CodeInputRateLimited    = "input.rate_limited"    // Too many input messages per second
	CodeInputSessionInvalid = "input.session_invalid" // Session ID doesn't match current session
	CodeInputWriteFailed    = "input.write_failed"    // Failed to write input to PTY

	// Diff domain - diff polling and parsing errors
	CodeDiffParseFailed = "diff.parse_failed" // Failed to parse git diff
	CodeDiffPollFailed  = "diff.poll_failed"  // Diff polling failed

	// Auth domain - authentication and authorization (for Phase 3)
	CodeAuthRequired      = "auth.required"       // Authentication required
	CodeAuthInvalid       = "auth.invalid"        // Invalid token or credentials
	CodeAuthExpired       = "auth.expired"        // Token or session expired
	CodeAuthDeviceRevoked = "auth.device_revoked" // Device has been revoked

	// Approval domain - CLI command approval broker (Phase 6)
	CodeApprovalTimeout      = "approval.timeout"       // Approval request timed out
	CodeApprovalDenied       = "approval.denied"        // Approval explicitly denied
	CodeApprovalNotFound     = "approval.not_found"     // Approval request not found
	CodeApprovalDuplicate    = "approval.duplicate"     // Duplicate request ID already pending
	CodeApprovalInvalidToken = "approval.invalid_token" // Invalid or missing approval token

	// Commit domain - git commit errors (Phase 6)
	CodeCommitNoStagedChanges = "commit.no_staged_changes" // Nothing staged to commit
	CodeCommitEmptyMessage    = "commit.empty_message"     // Commit message is empty
	CodeCommitHookFailed      = "commit.hook_failed"       // Pre-commit or commit-msg hook failed
	CodeCommitGitError        = "commit.git_error"         // General git commit error

	// Push domain - git push errors (Phase 6.6)
	CodePushNoUpstream     = "push.no_upstream"  // No upstream configured and remote/branch not provided
	CodePushNonFastForward = "push.non_ff"       // Push rejected, not fast-forward
	CodePushAuthFailed     = "push.auth_failed"  // SSH key or credential failure
	CodePushGitError       = "push.git_error"    // General git push error
	CodePushInvalidArgs    = "push.invalid_args" // Remote or branch contains invalid characters (Unit 7.7)

	// tmux domain - tmux session integration errors (Phase 12)
	CodeTmuxNotInstalled    = "tmux.not_installed"     // tmux command not found on host
	CodeTmuxNoServer        = "tmux.no_server"         // tmux server not running (no sessions)
	CodeTmuxSessionNotFound = "tmux.session_not_found" // Requested tmux session does not exist
	CodeTmuxAttachFailed    = "tmux.attach_failed"     // Failed to attach to tmux session
	CodeTmuxKillFailed      = "tmux.kill_failed"       // Failed to kill tmux session

	// Undo domain - undo operation errors (Phase 20, 25)
	CodeUndoConflict       = "undo.conflict"        // Patch apply failed during undo (conflicts)
	CodeUndoBaseMissing    = "undo.base_missing"    // Cannot apply patch (file or content missing)
	CodeUndoNotStaged      = "undo.not_staged"      // Cannot undo commit (changes not in current index)
	CodeUndoAlreadyPending = "undo.already_pending" // Card is already in pending state
	CodeUndoChunkOnly      = "undo.chunk_only"      // Card has per-chunk decisions, use chunk-level undo (Phase 25)

	// General domain - catch-all errors
	CodeUnknown  = "error.unknown"  // Unknown error
	CodeInternal = "error.internal" // Internal server error
)

// CodedError wraps an error with a stable error code.
// This allows errors to carry both a code for programmatic handling
// and a message for human consumption.
type CodedError struct {
	Code    string // Stable error code (e.g., "storage.not_found")
	Message string // Human-readable error message
	Cause   error  // Underlying error (may be nil)
}

// Error implements the error interface.
func (e *CodedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause for errors.Is/As support.
func (e *CodedError) Unwrap() error {
	return e.Cause
}

// New creates a new CodedError with the given code and message.
func New(code, message string) *CodedError {
	return &CodedError{
		Code:    code,
		Message: message,
	}
}

// Wrap creates a new CodedError wrapping an existing error.
func Wrap(code, message string, cause error) *CodedError {
	return &CodedError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// GetCode extracts the error code from an error.
// If the error is a CodedError, returns its code.
// Otherwise, attempts to map known error types to codes.
// Falls back to CodeUnknown for unrecognized errors.
func GetCode(err error) string {
	if err == nil {
		return ""
	}

	// Check if it's already a CodedError
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}

	// Return unknown for unrecognized errors
	return CodeUnknown
}

// GetMessage extracts a human-readable message from an error.
// If the error is a CodedError, returns its message.
// Otherwise, returns the error's Error() string.
func GetMessage(err error) string {
	if err == nil {
		return ""
	}

	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Message
	}

	return err.Error()
}

// ToCodeAndMessage extracts both code and message from an error.
// This is the primary function for converting errors to client responses.
func ToCodeAndMessage(err error) (code, message string) {
	if err == nil {
		return "", ""
	}

	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code, coded.Message
	}

	return CodeUnknown, err.Error()
}

// IsCode checks if an error has a specific error code.
func IsCode(err error, code string) bool {
	return GetCode(err) == code
}

// Common error constructors for frequently used error types.

// NotFound creates a "storage.not_found" error.
func NotFound(resource string) *CodedError {
	return New(CodeStorageNotFound, fmt.Sprintf("%s not found", resource))
}

// AlreadyDecided creates a "storage.already_decided" error.
func AlreadyDecided(cardID string) *CodedError {
	return New(CodeStorageAlreadyDecided, fmt.Sprintf("card %s already has a decision", cardID))
}

// InvalidAction creates an "action.invalid" error.
func InvalidAction(action string) *CodedError {
	return New(CodeActionInvalid, fmt.Sprintf("invalid action: %s (must be 'accept' or 'reject')", action))
}

// GitFailed creates an "action.git_failed" error.
func GitFailed(operation, file string, cause error) *CodedError {
	msg := fmt.Sprintf("git %s failed for %s", operation, file)
	return Wrap(CodeActionGitFailed, msg, cause)
}

// InvalidMessage creates a "server.invalid_message" error.
func InvalidMessage(reason string) *CodedError {
	return New(CodeServerInvalidMessage, reason)
}

// Internal creates an "error.internal" error.
func Internal(message string, cause error) *CodedError {
	return Wrap(CodeInternal, message, cause)
}

// ChunkStale creates an "action.chunk_stale" error.
// This indicates the chunk content has changed since the user viewed it,
// and they should refresh to see the current state before deciding.
func ChunkStale(cardID string, chunkIndex int) *CodedError {
	msg := fmt.Sprintf("chunk %d in card %s has changed, please refresh", chunkIndex, cardID)
	return New(CodeActionChunkStale, msg)
}

// UntrackedFile creates an "action.untracked_file" error.
// This indicates the file is not tracked by git and cannot be restored.
// The user should delete the file manually or use the delete action.
func UntrackedFile(file string) *CodedError {
	msg := fmt.Sprintf("file %s is untracked and cannot be restored (delete it instead)", file)
	return New(CodeActionUntrackedFile, msg)
}

// FileDeleted creates an "action.file_deleted" error.
// This indicates the file no longer exists on disk and the decision cannot be applied.
// The user should refresh to see the current state.
func FileDeleted(file string) *CodedError {
	msg := fmt.Sprintf("file %s was deleted and the decision cannot be applied (refresh to update)", file)
	return New(CodeActionFileDeleted, msg)
}

// BinaryFile creates an "action.binary_file" error.
// This indicates per-chunk actions are not supported for binary files.
// The user should use file-level accept/reject instead.
func BinaryFile(file string) *CodedError {
	msg := fmt.Sprintf("file %s is binary - use file-level accept/reject instead of per-chunk actions", file)
	return New(CodeActionBinaryFile, msg)
}

// ValidationFailed creates a "validation.failed" error.
// This indicates a pre-apply validation step failed and no changes were applied.
func ValidationFailed(message string) *CodedError {
	msg := "validation failed"
	if message != "" {
		msg = fmt.Sprintf("%s: %s", msg, message)
	}
	return New(CodeValidationFailed, msg)
}

// VerificationFailed creates a "verification.failed" error.
// This indicates a post-apply verification step failed.
func VerificationFailed(message string) *CodedError {
	msg := "verification failed"
	if message != "" {
		msg = fmt.Sprintf("%s: %s", msg, message)
	}
	return New(CodeVerificationFailed, msg)
}

// StateDiverged creates a "state.diverged" error.
// This indicates storage state and git state are inconsistent.
func StateDiverged(files []string) *CodedError {
	msg := "state diverged"
	if len(files) > 0 {
		msg = fmt.Sprintf("%s for files: %s", msg, strings.Join(files, ", "))
	}
	return New(CodeStateDiverged, msg)
}

// ConflictDetected creates a "conflict.detected" error.
// This indicates a patch could not be applied cleanly due to conflicts.
func ConflictDetected(message string) *CodedError {
	msg := "conflict detected"
	if message != "" {
		msg = fmt.Sprintf("%s: %s", msg, message)
	}
	return New(CodeConflictDetected, msg)
}

// ApprovalTimeout creates an "approval.timeout" error.
// This indicates the approval request was not answered within the timeout period.
// The default behavior is to deny the request when this happens.
func ApprovalTimeout(requestID string) *CodedError {
	msg := fmt.Sprintf("approval request %s timed out (defaulting to deny)", requestID)
	return New(CodeApprovalTimeout, msg)
}

// ApprovalDenied creates an "approval.denied" error.
// This indicates the user explicitly denied the approval request.
func ApprovalDenied(requestID string) *CodedError {
	msg := fmt.Sprintf("approval request %s was denied by user", requestID)
	return New(CodeApprovalDenied, msg)
}

// ApprovalNotFound creates an "approval.not_found" error.
// This indicates the approval request was not found (already expired or decided).
func ApprovalNotFound(requestID string) *CodedError {
	msg := fmt.Sprintf("approval request %s not found (may have expired)", requestID)
	return New(CodeApprovalNotFound, msg)
}

// ApprovalDuplicate creates an "approval.duplicate" error.
// This indicates a request with this ID is already pending.
func ApprovalDuplicate(requestID string) *CodedError {
	msg := fmt.Sprintf("approval request %s is already pending", requestID)
	return New(CodeApprovalDuplicate, msg)
}

// ApprovalInvalidToken creates an "approval.invalid_token" error.
// This indicates the approval token is missing or invalid.
func ApprovalInvalidToken() *CodedError {
	return New(CodeApprovalInvalidToken, "invalid or missing approval token")
}

// CommitNoStagedChanges creates a "commit.no_staged_changes" error.
// This indicates there are no staged changes to commit.
func CommitNoStagedChanges() *CodedError {
	return New(CodeCommitNoStagedChanges, "nothing to commit (no staged changes)")
}

// CommitEmptyMessage creates a "commit.empty_message" error.
// This indicates the commit message is empty or whitespace-only.
func CommitEmptyMessage() *CodedError {
	return New(CodeCommitEmptyMessage, "commit message cannot be empty")
}

// CommitHookFailed creates a "commit.hook_failed" error.
// This indicates a pre-commit or commit-msg hook failed.
// The output contains the hook's error message.
func CommitHookFailed(output string) *CodedError {
	msg := "commit hook failed"
	if output != "" {
		msg = fmt.Sprintf("commit hook failed: %s", output)
	}
	return New(CodeCommitHookFailed, msg)
}

// CommitGitError creates a "commit.git_error" error.
// This wraps general git commit errors that don't fit other categories.
func CommitGitError(cause error) *CodedError {
	return Wrap(CodeCommitGitError, "git commit failed", cause)
}

// PushNoUpstream creates a "push.no_upstream" error.
// This indicates no upstream is configured and remote/branch must be specified.
func PushNoUpstream() *CodedError {
	return New(CodePushNoUpstream, "no upstream configured - specify remote and branch")
}

// PushNonFastForward creates a "push.non_ff" error.
// This indicates the push was rejected because it is not a fast-forward.
// The user should pull first or use force-with-lease if allowed.
func PushNonFastForward(output string) *CodedError {
	msg := "push rejected - not fast-forward (pull first or use force-with-lease)"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodePushNonFastForward, msg)
}

// PushAuthFailed creates a "push.auth_failed" error.
// This indicates authentication failed (SSH key or credentials issue).
func PushAuthFailed(output string) *CodedError {
	msg := "push failed - check SSH keys or credentials"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodePushAuthFailed, msg)
}

// PushGitError creates a "push.git_error" error.
// This wraps general git push errors that don't fit other categories.
func PushGitError(cause error) *CodedError {
	return Wrap(CodePushGitError, "git push failed", cause)
}

// PushInvalidArgs creates a "push.invalid_args" error.
// This indicates the remote or branch argument contains invalid characters
// (e.g., starts with '-' for option injection, or contains shell metacharacters).
func PushInvalidArgs(reason string) *CodedError {
	return New(CodePushInvalidArgs, fmt.Sprintf("invalid push argument: %s", reason))
}

// TmuxNotInstalled creates a "tmux.not_installed" error.
// This indicates the tmux command was not found on the host system.
// Users should install tmux to use tmux session features.
func TmuxNotInstalled() *CodedError {
	return New(CodeTmuxNotInstalled, "tmux is not installed on this system")
}

// TmuxNoServer creates a "tmux.no_server" error.
// This indicates no tmux server is running (no sessions available).
// This is expected when there are no active tmux sessions.
func TmuxNoServer() *CodedError {
	return New(CodeTmuxNoServer, "no tmux server running (no sessions available)")
}

// TmuxSessionNotFound creates a "tmux.session_not_found" error.
// This indicates the requested tmux session does not exist.
func TmuxSessionNotFound(name string) *CodedError {
	return New(CodeTmuxSessionNotFound, fmt.Sprintf("tmux session '%s' not found", name))
}

// TmuxAttachFailed creates a "tmux.attach_failed" error.
// This indicates the attempt to attach to a tmux session failed.
func TmuxAttachFailed(name string, cause error) *CodedError {
	return Wrap(CodeTmuxAttachFailed, fmt.Sprintf("failed to attach to tmux session '%s'", name), cause)
}

// TmuxKillFailed creates a "tmux.kill_failed" error.
// This indicates the attempt to kill a tmux session failed.
func TmuxKillFailed(name string, cause error) *CodedError {
	return Wrap(CodeTmuxKillFailed, fmt.Sprintf("failed to kill tmux session '%s'", name), cause)
}

// UndoConflict creates an "undo.conflict" error.
// This indicates the patch could not be applied due to conflicts.
// The user should manually resolve conflicts or refresh the view.
func UndoConflict(cardID string, cause error) *CodedError {
	msg := fmt.Sprintf("undo failed for card %s - patch conflicts with current file state", cardID)
	return Wrap(CodeUndoConflict, msg, cause)
}

// UndoBaseMissing creates an "undo.base_missing" error.
// This indicates the file or content required for undo is missing.
func UndoBaseMissing(cardID, file string) *CodedError {
	msg := fmt.Sprintf("cannot undo card %s - file %s is missing or content has changed", cardID, file)
	return New(CodeUndoBaseMissing, msg)
}

// UndoNotStaged creates an "undo.not_staged" error.
// This indicates committed changes cannot be undone (not in current index).
func UndoNotStaged(cardID string) *CodedError {
	msg := fmt.Sprintf("cannot undo card %s - changes are committed and not in staging area", cardID)
	return New(CodeUndoNotStaged, msg)
}

// UndoAlreadyPending creates an "undo.already_pending" error.
// This indicates the card is already in pending state (nothing to undo).
func UndoAlreadyPending(cardID string) *CodedError {
	msg := fmt.Sprintf("card %s is already pending - nothing to undo", cardID)
	return New(CodeUndoAlreadyPending, msg)
}

// UndoChunkOnly creates an "undo.chunk_only" error.
// This indicates the card has per-chunk decisions and file-level undo is not supported.
// The user should use chunk-level undo instead.
func UndoChunkOnly(cardID string) *CodedError {
	msg := fmt.Sprintf("card %s has per-chunk decisions - use chunk-level undo instead", cardID)
	return New(CodeUndoChunkOnly, msg)
}
