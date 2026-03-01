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

	// Server domain - connection classification taxonomy (A3: mobile-side triggers)
	CodeServerCertExpired   = "server.cert_expired"   // Certificate date invalid (expired or not-yet-valid)
	CodeServerCertMismatch  = "server.cert_mismatch"  // Fingerprint mismatch (MITM or cert rotation)
	CodeServerCertUntrusted = "server.cert_untrusted" // TLS handshake failed (unknown CA / self-signed)
	CodeServerTimeout       = "server.timeout"        // Connection or operation timed out
	CodeServerUnreachable   = "server.unreachable"    // DNS/socket/transport failure fallback

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

	// Auth domain - pairing endpoint taxonomy (A3: /pair)
	CodeAuthPairMethodNotAllowed = "auth.pair_method_not_allowed" // POST-only endpoint
	CodeAuthPairMissingCode      = "auth.pair_missing_code"       // No pairing code in request
	CodeAuthPairInvalidRequest   = "auth.pair_invalid_request"    // Malformed request JSON
	CodeAuthPairInvalidCode      = "auth.pair_invalid_code"       // Wrong pairing code
	CodeAuthPairExpiredCode      = "auth.pair_expired_code"       // Pairing code expired
	CodeAuthPairUsedCode         = "auth.pair_used_code"          // One-time code already used
	CodeAuthPairRateLimited      = "auth.pair_rate_limited"       // Too many pairing attempts
	CodeAuthPairInternal         = "auth.pair_internal"           // Unexpected host error during pairing

	// Auth domain - pairing code generation endpoint taxonomy (A3: /pair/generate)
	CodeAuthPairGenerateForbidden        = "auth.pair_generate_forbidden"          // Not from loopback/unix socket
	CodeAuthPairGenerateMethodNotAllowed = "auth.pair_generate_method_not_allowed" // POST-only endpoint
	CodeAuthPairGenerateInternal         = "auth.pair_generate_internal"           // Host failed to generate code

	// Approval domain - CLI command approval broker (Phase 6)
	CodeApprovalTimeout      = "approval.timeout"       // Approval request timed out
	CodeApprovalDenied       = "approval.denied"        // Approval explicitly denied
	CodeApprovalNotFound     = "approval.not_found"     // Approval request not found
	CodeApprovalDuplicate    = "approval.duplicate"     // Duplicate request ID already pending
	CodeApprovalInvalidToken = "approval.invalid_token" // Invalid or missing approval token

	// Commit domain - git commit errors (Phase 6)
	CodeCommitNoStagedChanges  = "commit.no_staged_changes" // Nothing staged to commit
	CodeCommitEmptyMessage     = "commit.empty_message"     // Commit message is empty
	CodeCommitHookFailed       = "commit.hook_failed"       // Pre-commit or commit-msg hook failed
	CodeCommitGitError         = "commit.git_error"         // General git commit error
	CodeCommitReadinessBlocked = "commit.readiness_blocked" // Readiness blockers prevent commit
	CodeCommitOverrideRequired = "commit.override_required" // Advisory warnings require explicit override

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

	// Sync domain - git fetch/pull errors (Phase 9 P9U3)
	CodeSyncNonFF        = "sync.non_ff"        // Pull cannot fast-forward
	CodeSyncAuthFailed   = "sync.auth_failed"   // SSH key or credential failure during fetch/pull
	CodeSyncNetworkError = "sync.network_error" // Network unreachable during fetch/pull
	CodeSyncTimeout      = "sync.timeout"       // Fetch/pull operation timed out
	CodeSyncGitError     = "sync.git_error"     // General git fetch/pull error

	// PR domain - GitHub PR operations via gh CLI (Phase 9 P9U4)
	CodePrGhMissing            = "pr.gh_missing"             // gh CLI not found on host
	CodePrAuthRequired         = "pr.auth_required"          // gh requires authentication
	CodePrRepoUnsupported      = "pr.repo_unsupported"       // Repository not hosted on GitHub
	CodePrNotFound             = "pr.not_found"              // PR number does not exist
	CodePrValidationFailed     = "pr.validation_failed"      // Input validation failed (title, number, etc.)
	CodePrCheckoutBlockedDirty = "pr.checkout_blocked_dirty" // Working tree dirty blocks checkout
	CodePrCheckoutBlockedDraft = "pr.checkout_blocked_draft" // Mobile-local draft blocks checkout (never emitted by host)
	CodePrNetworkError         = "pr.network_error"          // Network failure during gh operation
	CodePrTimeout              = "pr.timeout"                // gh operation timed out
	CodePrGhError              = "pr.gh_error"               // General gh CLI error

	// Keep-awake domain - host runtime keep-awake lifecycle (Phase 17)
	CodeKeepAwakePolicyDisabled         = "keep_awake.policy_disabled"         // Remote keep-awake policy is disabled
	CodeKeepAwakeUnauthorized           = "keep_awake.unauthorized"            // Caller lacks permission for keep-awake mutation
	CodeKeepAwakeUnsupportedEnvironment = "keep_awake.unsupported_environment" // Host environment cannot acquire inhibitor
	CodeKeepAwakeAcquireFailed          = "keep_awake.acquire_failed"          // Host failed to acquire inhibitor
	CodeKeepAwakeConflict               = "keep_awake.conflict"                // Keep-awake mutation conflict or fail-closed audit conflict
	CodeKeepAwakeExpired                = "keep_awake.expired"                 // Lease expired before requested operation

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

// NextAction maps taxonomy error codes to their primary recovery guidance.
// Each high-frequency failure has exactly one concrete next action.
var NextAction = map[string]string{
	// /pair endpoint
	CodeAuthPairMethodNotAllowed: "Submit pairing from the mobile app (POST /pair with JSON body containing code).",
	CodeAuthPairMissingCode:      "Enter the 6-digit code shown by `pseudocoder pair`.",
	CodeAuthPairInvalidRequest:   "Retry pairing from mobile; if repeated, verify host and mobile are on compatible versions.",
	CodeAuthPairInvalidCode:      "Run `pseudocoder pair` and enter the newest 6-digit code.",
	CodeAuthPairExpiredCode:      "Run `pseudocoder pair` to generate a new code (valid for 2 minutes).",
	CodeAuthPairUsedCode:         "Run `pseudocoder pair` to generate a new one-time code.",
	CodeAuthPairRateLimited:      "Wait 60 seconds, then retry with a fresh code.",
	CodeAuthPairInternal:         "Run `pseudocoder doctor`, then retry pairing.",
	// /pair/generate endpoint
	CodeAuthPairGenerateForbidden:        "Run `pseudocoder pair` on the host machine (local shell or SSH), then retry.",
	CodeAuthPairGenerateMethodNotAllowed: "Use `pseudocoder pair`; do not call /pair/generate with non-POST methods.",
	CodeAuthPairGenerateInternal:         "Run `pseudocoder doctor`, restart host with `pseudocoder host start --require-auth`, then retry `pseudocoder pair`.",
	// Connection classification (mobile-side, for reference parity)
	CodeServerCertExpired:   "Regenerate host certs under ~/.pseudocoder/certs and restart host.",
	CodeServerCertMismatch:  "Compare fingerprints with host output and re-pair only if match is expected.",
	CodeServerCertUntrusted: "Verify fingerprint from `pseudocoder pair --qr` and trust only if it matches.",
	CodeServerTimeout:       "Check LAN/Tailscale reachability and rerun `pseudocoder doctor`.",
	CodeServerUnreachable:   "Verify host/port and host process (`pseudocoder start`), then retry.",
	// Commit readiness
	CodeCommitReadinessBlocked: "Resolve commit blockers before retrying commit.",
	CodeCommitOverrideRequired: "Review warnings and confirm commit override if intended.",
	// Keep-awake
	CodeKeepAwakePolicyDisabled:         "Enable keep-awake policy on the host before retrying.",
	CodeKeepAwakeUnauthorized:           "Use an authorized device or request host admin approval.",
	CodeKeepAwakeUnsupportedEnvironment: "This host cannot provide keep-awake in the current environment.",
	CodeKeepAwakeAcquireFailed:          "Retry keep-awake; if repeated, run `pseudocoder doctor` and inspect host logs.",
	CodeKeepAwakeConflict:               "Retry with a new request after host state refresh.",
	CodeKeepAwakeExpired:                "Request a new keep-awake lease.",
}

// GetNextAction returns the primary recovery guidance for an error code.
// Returns empty string if no next action is defined.
func GetNextAction(code string) string {
	return NextAction[code]
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

// CommitReadinessBlocked creates a "commit.readiness_blocked" error.
// This indicates one or more hard readiness blockers prevent commit.
func CommitReadinessBlocked(blockers []string) *CodedError {
	msg := "commit blocked by readiness checks"
	if len(blockers) > 0 {
		msg = fmt.Sprintf("commit blocked by readiness checks: %s", strings.Join(blockers, ", "))
	}
	return New(CodeCommitReadinessBlocked, msg)
}

// CommitOverrideRequired creates a "commit.override_required" error.
// This indicates advisory warnings require explicit user override before commit.
func CommitOverrideRequired(warnings []string) *CodedError {
	msg := "commit warnings require explicit override"
	if len(warnings) > 0 {
		msg = fmt.Sprintf("commit warnings require explicit override: %s", strings.Join(warnings, ", "))
	}
	return New(CodeCommitOverrideRequired, msg)
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

// SyncNonFF creates a "sync.non_ff" error.
// This indicates a pull cannot fast-forward and requires manual resolution.
func SyncNonFF(output string) *CodedError {
	msg := "pull cannot fast-forward - merge or rebase required"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodeSyncNonFF, msg)
}

// SyncAuthFailed creates a "sync.auth_failed" error.
// This indicates authentication failed during fetch or pull.
func SyncAuthFailed(output string) *CodedError {
	msg := "fetch/pull failed - check SSH keys or credentials"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodeSyncAuthFailed, msg)
}

// SyncNetworkError creates a "sync.network_error" error.
// This indicates a network error during fetch or pull.
func SyncNetworkError(cause error) *CodedError {
	return Wrap(CodeSyncNetworkError, "fetch/pull failed - network error", cause)
}

// SyncTimeout creates a "sync.timeout" error.
// This indicates the fetch or pull operation timed out.
func SyncTimeout(cause error) *CodedError {
	return Wrap(CodeSyncTimeout, "fetch/pull timed out", cause)
}

// SyncGitError creates a "sync.git_error" error.
// This wraps general git fetch/pull errors that don't fit other categories.
func SyncGitError(cause error) *CodedError {
	return Wrap(CodeSyncGitError, "git fetch/pull failed", cause)
}

// PrGhMissing creates a "pr.gh_missing" error.
// This indicates the gh CLI is not installed on the host.
func PrGhMissing() *CodedError {
	return New(CodePrGhMissing, "gh CLI not found - install GitHub CLI to use PR features")
}

// PrAuthRequired creates a "pr.auth_required" error.
// This indicates gh requires authentication before PR operations.
func PrAuthRequired(output string) *CodedError {
	msg := "gh authentication required - run 'gh auth login'"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodePrAuthRequired, msg)
}

// PrRepoUnsupported creates a "pr.repo_unsupported" error.
// This indicates the repository is not hosted on GitHub.
func PrRepoUnsupported(output string) *CodedError {
	msg := "repository not hosted on GitHub"
	if output != "" {
		msg = fmt.Sprintf("%s: %s", msg, output)
	}
	return New(CodePrRepoUnsupported, msg)
}

// PrNotFound creates a "pr.not_found" error.
// This indicates the specified PR number does not exist.
func PrNotFound(number int) *CodedError {
	return New(CodePrNotFound, fmt.Sprintf("pull request #%d not found", number))
}

// PrValidationFailed creates a "pr.validation_failed" error.
// This indicates input validation failed for a PR operation.
func PrValidationFailed(message string) *CodedError {
	msg := "PR validation failed"
	if message != "" {
		msg = fmt.Sprintf("%s: %s", msg, message)
	}
	return New(CodePrValidationFailed, msg)
}

// PrCheckoutBlockedDirty creates a "pr.checkout_blocked_dirty" error.
// This indicates the working tree has dirty state that blocks PR checkout.
func PrCheckoutBlockedDirty(blockers []string) *CodedError {
	msg := "PR checkout blocked by dirty working tree"
	if len(blockers) > 0 {
		msg = fmt.Sprintf("%s: %s", msg, strings.Join(blockers, ", "))
	}
	return New(CodePrCheckoutBlockedDirty, msg)
}

// PrNetworkError creates a "pr.network_error" error.
// This indicates a network failure during a gh operation.
func PrNetworkError(cause error) *CodedError {
	return Wrap(CodePrNetworkError, "gh operation failed - network error", cause)
}

// PrTimeout creates a "pr.timeout" error.
// This indicates a gh operation timed out.
func PrTimeout(cause error) *CodedError {
	return Wrap(CodePrTimeout, "gh operation timed out", cause)
}

// PrGhError creates a "pr.gh_error" error.
// This wraps general gh CLI errors that don't fit other categories.
func PrGhError(cause error) *CodedError {
	return Wrap(CodePrGhError, "gh operation failed", cause)
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
