// Package server provides the WebSocket server for client connections.
// This file contains git operations for commit and repository status workflows.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	apperrors "github.com/pseudocoder/host/internal/errors"
)

// validHexHash matches a full 40-character hex commit hash.
var validHexHash = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	// CommitReadinessReady means commit can proceed with no warnings.
	CommitReadinessReady = "ready"
	// CommitReadinessBlocked means one or more hard blockers prevent commit.
	CommitReadinessBlocked = "blocked"
	// CommitReadinessRisky means commit can proceed only with explicit override.
	CommitReadinessRisky = "risky"
)

const (
	// Readiness blocker and warning codes (stable protocol values).
	ReadinessNoStagedChanges = "no_staged_changes"
	ReadinessMergeConflicts  = "merge_conflicts_present"
	ReadinessUnstagedChanges = "unstaged_changes_present"
	ReadinessDetachedHead    = "detached_head"
)

// GitOperations handles git commands for commit/push workflows.
// It provides methods to query repository status and create commits
// while respecting host configuration for security-sensitive options.
type GitOperations struct {
	// repoPath is the path to the git repository.
	repoPath string

	// allowNoVerify controls whether clients can skip pre-commit hooks.
	// This should be false in production to ensure hooks are always run.
	allowNoVerify bool

	// allowNoGpgSign controls whether clients can skip GPG signing.
	// This should be false if GPG signing is required by policy.
	allowNoGpgSign bool

	// allowForceWithLease controls whether clients can use force-with-lease.
	// This should be false unless you explicitly trust mobile clients with force push.
	allowForceWithLease bool
}

// NewGitOperations creates a new GitOperations handler.
// The allowNoVerify, allowNoGpgSign, and allowForceWithLease options control
// whether clients can request those features; if false, such requests are ignored.
func NewGitOperations(repoPath string, allowNoVerify, allowNoGpgSign, allowForceWithLease bool) *GitOperations {
	return &GitOperations{
		repoPath:            repoPath,
		allowNoVerify:       allowNoVerify,
		allowNoGpgSign:      allowNoGpgSign,
		allowForceWithLease: allowForceWithLease,
	}
}

// GetStagedFiles returns the list of currently staged files.
// This is useful for tracking which files will be included in a commit,
// enabling commit association with decided cards for undo support.
func (g *GitOperations) GetStagedFiles() ([]string, error) {
	output, err := g.runGit("diff", "--cached", "--name-only")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get staged files", err)
	}
	return splitLines(output), nil
}

// GetRepoStatus returns the current repository status.
// This includes the current branch, upstream tracking branch,
// counts of staged and unstaged files, and the last commit info.
func (g *GitOperations) GetRepoStatus() (*RepoStatusPayload, error) {
	status := &RepoStatusPayload{}

	// Get current branch - try rev-parse first, fall back to symbolic-ref for empty repos
	branch, err := g.runGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// In empty repos (no commits), rev-parse fails. Try symbolic-ref instead.
		branch, err = g.runGit("symbolic-ref", "--short", "HEAD")
		if err != nil {
			return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get current branch", err)
		}
	}
	status.Branch = strings.TrimSpace(branch)

	// Get upstream (may not exist)
	upstream, err := g.runGit("rev-parse", "--abbrev-ref", "@{u}")
	if err == nil {
		status.Upstream = strings.TrimSpace(upstream)
	}

	// Get staged files list and count
	stagedOutput, err := g.runGit("diff", "--cached", "--name-only")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get staged files", err)
	}
	status.StagedFiles = splitLines(stagedOutput)
	status.StagedCount = len(status.StagedFiles)

	// Count unstaged files (modified tracked files + untracked files)
	unstagedOutput, err := g.runGit("diff", "--name-only")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get unstaged files", err)
	}
	modifiedCount := countLines(unstagedOutput)

	// Count untracked files (new files not yet added to git)
	untrackedOutput, err := g.runGit("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get untracked files", err)
	}
	untrackedCount := countLines(untrackedOutput)

	status.UnstagedCount = modifiedCount + untrackedCount

	// Get last commit (may not exist in empty repo)
	lastCommit, err := g.runGit("log", "-1", "--format=%h %s")
	if err == nil {
		status.LastCommit = strings.TrimSpace(lastCommit)
	}

	// Compute commit readiness from current repository state.
	if err := g.applyReadiness(status); err != nil {
		return nil, err
	}

	return status, nil
}

// applyReadiness computes commit readiness and writes fields into status.
// Readiness is deterministic and ordered:
// - blockers: no_staged_changes, merge_conflicts_present
// - warnings: unstaged_changes_present, detached_head
func (g *GitOperations) applyReadiness(status *RepoStatusPayload) error {
	if status == nil {
		return nil
	}

	var blockers []string
	var warnings []string
	var actions []string

	if status.StagedCount == 0 {
		blockers = append(blockers, ReadinessNoStagedChanges)
		actions = append(actions, "Stage at least one file before committing.")
	}

	hasConflicts, err := g.hasMergeConflicts()
	if err != nil {
		return apperrors.Wrap(apperrors.CodeCommitGitError, "failed to detect merge conflicts", err)
	}
	if hasConflicts {
		blockers = append(blockers, ReadinessMergeConflicts)
		actions = append(actions, "Resolve merge conflicts and stage the resolved files.")
	}

	if status.UnstagedCount > 0 {
		warnings = append(warnings, ReadinessUnstagedChanges)
		actions = append(actions, "Review or stage unstaged changes, or commit anyway to include only staged files.")
	}

	if status.Branch == "HEAD" {
		warnings = append(warnings, ReadinessDetachedHead)
		actions = append(actions, "Check out a branch before committing, or commit anyway if detached HEAD is intentional.")
	}

	status.ReadinessBlockers = blockers
	status.ReadinessWarnings = warnings
	status.ReadinessActions = actions
	switch {
	case len(blockers) > 0:
		status.ReadinessState = CommitReadinessBlocked
	case len(warnings) > 0:
		status.ReadinessState = CommitReadinessRisky
	default:
		status.ReadinessState = CommitReadinessReady
	}

	return nil
}

// hasMergeConflicts returns true when the repository has unresolved merge conflicts.
func (g *GitOperations) hasMergeConflicts() (bool, error) {
	output, err := g.runGit("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, err
	}
	return countLines(output) > 0, nil
}

// Commit creates a git commit with the specified message.
// Returns the commit hash and a summary of changes on success.
// The noVerify and noGpgSign flags are only honored if the host allows them.
func (g *GitOperations) Commit(message string, noVerify, noGpgSign bool) (hash, summary string, err error) {
	// Validate message
	message = strings.TrimSpace(message)
	if message == "" {
		return "", "", apperrors.CommitEmptyMessage()
	}

	// Check for staged changes
	hasStagedChanges, err := g.hasStagedChanges()
	if err != nil {
		return "", "", err
	}
	if !hasStagedChanges {
		return "", "", apperrors.CommitNoStagedChanges()
	}

	// Build commit command
	args := []string{"commit", "-m", message}

	// Only add --no-verify if allowed by host config
	if noVerify && g.allowNoVerify {
		args = append(args, "--no-verify")
	}

	// Only add --no-gpg-sign if allowed by host config
	if noGpgSign && g.allowNoGpgSign {
		args = append(args, "--no-gpg-sign")
	}

	// Run commit with non-interactive mode
	output, err := g.runGitWithEnv(args, []string{"GIT_TERMINAL_PROMPT=0"})
	if err != nil {
		// Check if it's a hook failure
		if strings.Contains(output, "hook") || strings.Contains(strings.ToLower(output), "pre-commit") {
			return "", "", apperrors.CommitHookFailed(strings.TrimSpace(output))
		}
		return "", "", apperrors.CommitGitError(err)
	}

	// Get the commit hash
	hash, err = g.runGit("rev-parse", "HEAD")
	if err != nil {
		// Commit succeeded but we couldn't get the hash
		return "", output, nil
	}
	hash = strings.TrimSpace(hash)

	// Extract summary from commit output (e.g., "1 file changed, 5 insertions(+)")
	summary = extractCommitSummary(output)

	return hash, summary, nil
}

// hasStagedChanges checks if there are any staged changes.
// Returns true if there are changes staged for commit.
func (g *GitOperations) hasStagedChanges() (bool, error) {
	// git diff --cached --quiet returns exit 0 if no changes, exit 1 if changes
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = g.repoPath

	err := cmd.Run()
	if err != nil {
		// Exit code 1 means there are changes
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return true, nil
		}
		// Other errors are actual failures
		return false, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check staged changes", err)
	}
	// Exit code 0 means no changes
	return false, nil
}

// runGit executes a git command and returns stdout as a string.
func (g *GitOperations) runGit(args ...string) (string, error) {
	return g.runGitWithEnv(args, nil)
}

// runGitWithEnv executes a git command with additional environment variables.
func (g *GitOperations) runGitWithEnv(args []string, env []string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.repoPath

	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Combine stdout and stderr for error context
		errOutput := stderr.String()
		if errOutput == "" {
			errOutput = stdout.String()
		}
		return errOutput, err
	}

	return stdout.String(), nil
}

// countLines counts non-empty lines in a string.
func countLines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// splitLines splits a string into non-empty lines.
// Returns an empty slice (not nil) if the input is empty.
// Handles both LF (\n) and CRLF (\r\n) line endings (Windows compatibility).
// Only trailing newlines/carriage returns are removed to preserve filenames with leading/trailing spaces.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	// Remove \r from each line to handle CRLF endings
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, "\r")
	}
	return lines
}

// extractCommitSummary extracts the summary line from git commit output.
// Example: "1 file changed, 5 insertions(+), 2 deletions(-)"
func extractCommitSummary(output string) string {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Look for the summary line pattern
		if strings.Contains(line, "changed") &&
			(strings.Contains(line, "insertion") || strings.Contains(line, "deletion")) {
			return line
		}
	}
	// Return the last non-empty line as fallback
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

// validatePushArg checks if a remote or branch name is safe to use.
// Returns an error if the argument:
//   - Starts with '-' (option injection attack)
//   - Contains shell metacharacters (command injection risk)
//
// Empty strings are allowed (will use upstream tracking branch).
func validatePushArg(name, argType string) error {
	if name == "" {
		return nil // Empty is allowed (will use upstream)
	}

	// Reject option injection - args starting with dash could be interpreted as git flags
	if strings.HasPrefix(name, "-") {
		return apperrors.PushInvalidArgs(argType + " cannot start with '-'")
	}

	// Reject shell metacharacters that could be used for command injection.
	// Although exec.Command doesn't use a shell, this is defense-in-depth
	// in case the code is ever refactored to use shell execution.
	dangerous := []string{";", "|", "&", "<", ">", "$", "`", "*", "?", "'", "\"", "\n", "\r", "\t", "\x00"}
	for _, char := range dangerous {
		if strings.Contains(name, char) {
			return apperrors.PushInvalidArgs(argType + " contains invalid character")
		}
	}

	return nil
}

// Push performs a git push operation.
// If remote and branch are empty, uses the upstream tracking branch.
// The forceWithLease flag is only honored if allowed by host config.
// Returns the push output summary or an error with standardized error code.
func (g *GitOperations) Push(remote, branch string, forceWithLease bool) (string, error) {
	// Validate arguments to prevent option injection (Unit 7.7)
	if err := validatePushArg(remote, "remote"); err != nil {
		return "", err
	}
	if err := validatePushArg(branch, "branch"); err != nil {
		return "", err
	}

	// Determine remote and branch from upstream if not provided
	if remote == "" || branch == "" {
		upstream, err := g.runGit("rev-parse", "--abbrev-ref", "@{u}")
		if err != nil {
			return "", apperrors.PushNoUpstream()
		}
		// Parse "origin/main" format - use SplitN to handle branch names with slashes
		parts := strings.SplitN(strings.TrimSpace(upstream), "/", 2)
		if len(parts) != 2 {
			return "", apperrors.PushNoUpstream()
		}
		remote = parts[0]
		branch = parts[1]

		// Validate upstream-derived values (defense against malicious git config)
		if err := validatePushArg(remote, "upstream remote"); err != nil {
			return "", err
		}
		if err := validatePushArg(branch, "upstream branch"); err != nil {
			return "", err
		}
	}

	// Build push command (options must come before remote/refspec)
	args := []string{"push"}
	if forceWithLease && g.allowForceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remote, branch)

	// Run with non-interactive mode to prevent credential prompts
	output, err := g.runGitWithEnv(args, []string{"GIT_TERMINAL_PROMPT=0"})
	if err != nil {
		return "", g.classifyPushError(output, err)
	}

	return extractPushSummary(output), nil
}

// classifyPushError analyzes git push output to determine the appropriate error type.
// This helps provide actionable error messages to the user.
func (g *GitOperations) classifyPushError(output string, err error) error {
	errLower := strings.ToLower(output)

	// Non-fast-forward / rejected errors
	if strings.Contains(errLower, "non-fast-forward") ||
		strings.Contains(errLower, "rejected") ||
		strings.Contains(errLower, "fetch first") ||
		strings.Contains(errLower, "failed to push") {
		return apperrors.PushNonFastForward(strings.TrimSpace(output))
	}

	// Authentication / credential errors
	if strings.Contains(errLower, "permission denied") ||
		strings.Contains(errLower, "authentication failed") ||
		strings.Contains(errLower, "could not read") ||
		strings.Contains(errLower, "fatal: could not read username") ||
		strings.Contains(errLower, "repository not found") {
		return apperrors.PushAuthFailed(strings.TrimSpace(output))
	}

	// Generic error
	return apperrors.PushGitError(err)
}

// GetHistory returns paginated commit history for the current branch.
// Entries are returned in newest-first order along the first-parent chain.
// cursor is the commit hash to start after (exclusive); empty means HEAD.
// pageSize is the number of entries per page.
// Returns entries, nextCursor (empty if no more pages), and error.
func (g *GitOperations) GetHistory(cursor string, pageSize int) ([]RepoHistoryEntry, string, error) {
	// Ensure the repo path is a git work tree before classifying HEAD errors.
	insideWorkTree, err := g.runGit("rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(insideWorkTree) != "true" {
		return nil, "", apperrors.Wrap(apperrors.CodeCommitGitError, "failed to resolve repository path", err)
	}

	// Empty repos have no HEAD commit and should return an empty page, but
	// other git failures must remain surfaced as commit.git_error.
	headOutput, err := g.runGit("rev-parse", "--verify", "HEAD")
	if err != nil {
		if isMissingHeadRevision(headOutput) {
			return nil, "", nil
		}
		return nil, "", apperrors.Wrap(apperrors.CodeCommitGitError, "failed to resolve repository HEAD", err)
	}

	// NUL-separated format: hash\0subject\0author\0ISO8601-date
	format := "%H%x00%s%x00%aN%x00%aI"
	// Request one extra to detect if there are more pages
	limit := fmt.Sprintf("-n%d", pageSize+1)

	var args []string
	if cursor == "" {
		args = []string{"log", "--first-parent", fmt.Sprintf("--format=%s", format), limit, "HEAD"}
	} else {
		isRootCursor, err := g.validateHistoryCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		if isRootCursor {
			// Cursor is the root commit; no more history.
			return nil, "", nil
		}
		// Use cursor^ to start from the parent of the cursor commit
		// (so cursor itself is excluded from the results).
		parentRef := cursor + "^"
		args = []string{"log", "--first-parent", fmt.Sprintf("--format=%s", format), limit, parentRef}
	}

	output, err := g.runGit(args...)
	if err != nil {
		return nil, "", apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get commit history", err)
	}

	lines := splitLines(output)
	var entries []RepoHistoryEntry
	for _, line := range lines {
		parts := strings.SplitN(line, "\x00", 4)
		if len(parts) != 4 {
			continue
		}
		authoredAt := parseISO8601Millis(parts[3])
		entries = append(entries, RepoHistoryEntry{
			Hash:       parts[0],
			Subject:    parts[1],
			Author:     parts[2],
			AuthoredAt: authoredAt,
		})
	}

	var nextCursor string
	if len(entries) > pageSize {
		// Set nextCursor to the last entry we'll return (the pageSize-th entry)
		nextCursor = entries[pageSize-1].Hash
		entries = entries[:pageSize]
	}

	return entries, nextCursor, nil
}

// isMissingHeadRevision returns true when git output indicates HEAD does not
// resolve because the repository has no commits yet.
func isMissingHeadRevision(output string) bool {
	normalized := strings.ToLower(strings.TrimSpace(output))
	return strings.Contains(normalized, "needed a single revision") ||
		strings.Contains(normalized, "unknown revision or path not in the working tree") ||
		strings.Contains(normalized, "bad revision 'head'")
}

// validateHistoryCursor validates history pagination cursor semantics.
// Returns (isRootCursor, error) where isRootCursor=true means cursor is valid
// and points at the root commit, so there is no next page.
func (g *GitOperations) validateHistoryCursor(cursor string) (bool, error) {
	if !validHexHash.MatchString(cursor) {
		return false, apperrors.New(apperrors.CodeServerInvalidMessage, "invalid cursor: not a valid commit hash")
	}

	// Cursor must resolve to a commit object.
	objectType, err := g.runGit("cat-file", "-t", cursor)
	if err != nil {
		return false, apperrors.New(apperrors.CodeServerInvalidMessage, "invalid cursor: commit not found")
	}
	if strings.TrimSpace(objectType) != "commit" {
		return false, apperrors.New(apperrors.CodeServerInvalidMessage, "invalid cursor: commit not found")
	}

	// Cursor must be on the current branch's first-parent chain.
	firstParentHistory, err := g.runGit("rev-list", "--first-parent", "HEAD")
	if err != nil {
		return false, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to validate history cursor ancestry", err)
	}
	onFirstParentChain := false
	for _, hash := range splitLines(firstParentHistory) {
		if hash == cursor {
			onFirstParentChain = true
			break
		}
	}
	if !onFirstParentChain {
		return false, apperrors.New(apperrors.CodeServerInvalidMessage, "invalid cursor: commit not on current branch history")
	}

	// Root cursor has no parent, so there are no additional entries.
	if _, err := g.runGit("rev-parse", "--verify", cursor+"^"); err != nil {
		return true, nil
	}
	return false, nil
}

// parseISO8601Millis parses an ISO 8601 date string to Unix milliseconds.
func parseISO8601Millis(s string) int64 {
	s = strings.TrimSpace(s)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Try with timezone offset format (+0100 vs +01:00)
		t, err = time.Parse("2006-01-02T15:04:05-07:00", s)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// GetBranches returns branch information for the repository.
// Returns currentBranch (or "HEAD" if detached), local branches, tracked remote branches, and error.
func (g *GitOperations) GetBranches() (currentBranch string, local []string, trackedRemote []string, err error) {
	// Get current branch
	branch, branchErr := g.runGit("symbolic-ref", "--short", "HEAD")
	if branchErr != nil {
		// Detached HEAD or empty repo
		currentBranch = "HEAD"
	} else {
		currentBranch = strings.TrimSpace(branch)
	}

	// Get local branches
	localOutput, err := g.runGit("for-each-ref", "--sort=refname", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return currentBranch, nil, nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to list local branches", err)
	}
	local = splitLines(localOutput)
	if len(local) == 1 && local[0] == "" {
		local = nil
	}

	// Get remote-tracking refs with symref metadata so we can reliably
	// filter symbolic aliases (for example origin/HEAD or foo/bar -> ...).
	remoteOutput, err := g.runGit("for-each-ref", "--sort=refname", "--format=%(refname:short)\t%(symref)", "refs/remotes/")
	if err != nil {
		return currentBranch, local, nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to list remote branches", err)
	}
	remoteLines := splitLines(remoteOutput)

	// Filter out symbolic aliases and dedupe branch refs.
	seen := make(map[string]bool)
	for _, line := range remoteLines {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		ref := strings.TrimSpace(parts[0])
		symref := strings.TrimSpace(parts[1])
		if ref == "" {
			continue
		}
		// Symbolic refs (for example refs/remotes/origin/HEAD) are aliases,
		// not real tracked branch refs.
		if symref != "" {
			continue
		}
		// Remote-tracking branches must include "<remote>/<branch>".
		if !strings.Contains(ref, "/") {
			continue
		}
		if !seen[ref] {
			seen[ref] = true
			trackedRemote = append(trackedRemote, ref)
		}
	}

	// Ensure sorted (for-each-ref should already sort, but safety)
	sort.Strings(local)
	sort.Strings(trackedRemote)

	return currentBranch, local, trackedRemote, nil
}

// ValidateBranchName checks if a branch name is valid using git check-ref-format.
func (g *GitOperations) ValidateBranchName(name string) error {
	_, err := g.runGit("check-ref-format", "--branch", name)
	if err != nil {
		return apperrors.New(apperrors.CodeActionInvalid, "invalid branch name: "+name)
	}
	return nil
}

// LocalBranchExists checks if a local branch with the given name exists.
func (g *GitOperations) LocalBranchExists(name string) (bool, error) {
	_, err := g.runGit("show-ref", "--verify", "refs/heads/"+name)
	if err != nil {
		// show-ref returns non-zero (1 or 128 depending on version) when ref not found.
		// Any exec.ExitError means the ref doesn't exist; other errors are real failures.
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check branch existence", err)
	}
	return true, nil
}

// IsEmptyRepo returns true when the repository has no commits (HEAD does not resolve).
func (g *GitOperations) IsEmptyRepo() (bool, error) {
	output, err := g.runGit("rev-parse", "--verify", "HEAD")
	if err != nil {
		if isMissingHeadRevision(output) {
			return true, nil
		}
		return false, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check HEAD", err)
	}
	return false, nil
}

// IsDetachedHead returns true when HEAD is not a symbolic reference (detached).
func (g *GitOperations) IsDetachedHead() (bool, error) {
	_, err := g.runGit("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return true, nil
	}
	return false, nil
}

// GetCurrentBranchName returns the current branch name.
// Returns an error if HEAD is detached (use IsDetachedHead to check first).
func (g *GitOperations) GetCurrentBranchName() (string, error) {
	output, err := g.runGit("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", apperrors.Wrap(apperrors.CodeCommitGitError, "failed to get current branch name", err)
	}
	return strings.TrimSpace(output), nil
}

// CreateBranch creates a new local branch from HEAD without switching to it.
func (g *GitOperations) CreateBranch(name string) error {
	_, err := g.runGit("branch", name)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeCommitGitError, "failed to create branch", err)
	}
	return nil
}

// CheckDirtyBlockers probes 5 dirty-state classes in deterministic order.
// Returns a slice of blocker codes for any active dirty state.
func (g *GitOperations) CheckDirtyBlockers() ([]string, error) {
	var blockers []string

	// 1. Staged changes
	stagedOutput, err := g.runGit("diff", "--cached", "--name-only")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check staged changes", err)
	}
	if countLines(stagedOutput) > 0 {
		blockers = append(blockers, "staged_changes_present")
	}

	// 2. Unstaged changes
	unstagedOutput, err := g.runGit("diff", "--name-only")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check unstaged changes", err)
	}
	if countLines(unstagedOutput) > 0 {
		blockers = append(blockers, "unstaged_changes_present")
	}

	// 3. Untracked files
	untrackedOutput, err := g.runGit("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check untracked files", err)
	}
	if countLines(untrackedOutput) > 0 {
		blockers = append(blockers, "untracked_changes_present")
	}

	// 4. Merge conflicts
	conflictOutput, err := g.runGit("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "failed to check merge conflicts", err)
	}
	if countLines(conflictOutput) > 0 {
		blockers = append(blockers, "merge_conflicts_present")
	}

	// 5. Submodule dirty state
	submoduleOutput, err := g.runGit("submodule", "status", "--recursive")
	if err != nil {
		// Submodule command may fail if no .gitmodules; treat as no submodules.
		return blockers, nil
	}
	for _, line := range splitLines(submoduleOutput) {
		if len(line) > 0 && (line[0] == '+' || line[0] == 'U') {
			blockers = append(blockers, "submodule_dirty_present")
			break
		}
	}

	return blockers, nil
}

// SwitchBranch orchestrates a branch switch with dirty-state blocking.
// Returns blockers (non-nil if dirty), or an error.
// A same-branch target is a successful no-op with nil blockers and nil error.
func (g *GitOperations) SwitchBranch(name string) (blockers []string, err error) {
	// Same-branch no-op check
	currentBranch, branchErr := g.GetCurrentBranchName()
	if branchErr == nil && currentBranch == name {
		return nil, nil
	}

	// Check dirty blockers
	blockers, err = g.CheckDirtyBlockers()
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return blockers, nil
	}

	// Perform checkout
	output, checkoutErr := g.runGit("checkout", name)
	if checkoutErr != nil {
		return g.classifyCheckoutFailure(output, checkoutErr)
	}

	return nil, nil
}

// classifyCheckoutFailure classifies a checkout failure as either dirty blockers or git error.
func (g *GitOperations) classifyCheckoutFailure(stderr string, err error) ([]string, error) {
	stderrLower := strings.ToLower(stderr)

	dirtyTokens := []string{
		"local changes to the following files would be overwritten by checkout",
		"would be overwritten by checkout",
		"please commit your changes or stash them before you switch branches",
		"please commit your changes or stash them before you checkout",
	}

	matchFound := false
	for _, token := range dirtyTokens {
		if strings.Contains(stderrLower, token) {
			matchFound = true
			break
		}
	}

	if matchFound {
		// Recompute blockers
		blockers, blockerErr := g.CheckDirtyBlockers()
		if blockerErr != nil {
			return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "checkout failed and blocker recompute failed", blockerErr)
		}
		if len(blockers) > 0 {
			return blockers, nil
		}
		// Matcher positive but no blockers found - unexpected, classify as git error
		return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "checkout failed", err)
	}

	return nil, apperrors.Wrap(apperrors.CodeCommitGitError, "checkout failed", err)
}

// Fetch performs a git fetch in non-interactive mode.
// Returns the fetch output or an error with standardized error code.
func (g *GitOperations) Fetch() (string, error) {
	output, err := g.runGitWithEnv([]string{"fetch"}, []string{"GIT_TERMINAL_PROMPT=0"})
	if err != nil {
		return "", g.classifyFetchError(output, err)
	}
	return strings.TrimSpace(output), nil
}

// PullFFOnly performs a git pull --ff-only in non-interactive mode.
// Returns the pull output or an error with standardized error code.
func (g *GitOperations) PullFFOnly() (string, error) {
	output, err := g.runGitWithEnv([]string{"pull", "--ff-only"}, []string{"GIT_TERMINAL_PROMPT=0"})
	if err != nil {
		return "", g.classifyPullError(output, err)
	}
	return strings.TrimSpace(output), nil
}

// classifyFetchError analyzes git fetch output to determine the appropriate error type.
func (g *GitOperations) classifyFetchError(output string, err error) error {
	errLower := strings.ToLower(output)

	if stderrors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(errLower, "timed out") {
		return apperrors.SyncTimeout(err)
	}

	// Authentication / credential errors
	if strings.Contains(errLower, "permission denied") ||
		strings.Contains(errLower, "authentication failed") ||
		strings.Contains(errLower, "could not read") ||
		strings.Contains(errLower, "fatal: could not read username") ||
		strings.Contains(errLower, "repository not found") {
		return apperrors.SyncAuthFailed(strings.TrimSpace(output))
	}

	// Network errors
	if strings.Contains(errLower, "could not resolve host") ||
		strings.Contains(errLower, "network is unreachable") ||
		strings.Contains(errLower, "connection refused") ||
		strings.Contains(errLower, "unable to access") {
		return apperrors.SyncNetworkError(err)
	}

	// Generic error
	return apperrors.SyncGitError(err)
}

// classifyPullError analyzes git pull output to determine the appropriate error type.
func (g *GitOperations) classifyPullError(output string, err error) error {
	errLower := strings.ToLower(output)

	if stderrors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(errLower, "timed out") {
		return apperrors.SyncTimeout(err)
	}

	// Non-fast-forward / divergence errors
	if strings.Contains(errLower, "not possible to fast-forward") ||
		strings.Contains(errLower, "fatal: not possible to fast-forward") ||
		strings.Contains(errLower, "non-fast-forward") ||
		strings.Contains(errLower, "need to specify how to reconcile divergent branches") {
		return apperrors.SyncNonFF(strings.TrimSpace(output))
	}

	// Authentication / credential errors
	if strings.Contains(errLower, "permission denied") ||
		strings.Contains(errLower, "authentication failed") ||
		strings.Contains(errLower, "could not read") ||
		strings.Contains(errLower, "fatal: could not read username") ||
		strings.Contains(errLower, "repository not found") {
		return apperrors.SyncAuthFailed(strings.TrimSpace(output))
	}

	// Network errors
	if strings.Contains(errLower, "could not resolve host") ||
		strings.Contains(errLower, "network is unreachable") ||
		strings.Contains(errLower, "connection refused") ||
		strings.Contains(errLower, "unable to access") {
		return apperrors.SyncNetworkError(err)
	}

	// Generic error
	return apperrors.SyncGitError(err)
}

// PR operation timeout constants.
const (
	prListTimeout     = 15 * time.Second
	prViewTimeout     = 15 * time.Second
	prCreateTimeout   = 45 * time.Second
	prCheckoutTimeout = 30 * time.Second
)

// runGhWithTimeout executes a gh command with a timeout context.
// It forces non-interactive mode via environment variables.
func (g *GitOperations) runGhWithTimeout(timeout time.Duration, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = g.repoPath
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// isGhMissing checks if the error indicates the gh binary was not found.
func isGhMissing(err error) bool {
	var pathErr *exec.Error
	if stderrors.As(err, &pathErr) {
		return stderrors.Is(pathErr.Err, exec.ErrNotFound)
	}
	return false
}

// classifyPrError maps gh command output and errors to typed PR error codes.
// Checks are ordered: gh-missing, timeout, auth, unsupported-repo, not-found, network, fallback.
func classifyPrError(combinedOutput string, err error) error {
	if isGhMissing(err) {
		return apperrors.PrGhMissing()
	}

	if stderrors.Is(err, context.DeadlineExceeded) {
		return apperrors.PrTimeout(err)
	}

	lower := strings.ToLower(combinedOutput)

	// Auth required
	if strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "gh auth login") ||
		strings.Contains(lower, "not logged in") {
		return apperrors.PrAuthRequired(strings.TrimSpace(combinedOutput))
	}

	// Repo unsupported (not a GitHub repo)
	if strings.Contains(lower, "not a git repository") ||
		strings.Contains(lower, "none of the git remotes") ||
		strings.Contains(lower, "could not determine repo") ||
		strings.Contains(lower, "no github remotes found") ||
		strings.Contains(lower, "not a github repository") {
		return apperrors.PrRepoUnsupported(strings.TrimSpace(combinedOutput))
	}

	// Not found
	if strings.Contains(lower, "could not find pull request") ||
		strings.Contains(lower, "not found") && strings.Contains(lower, "pull request") {
		return apperrors.New(apperrors.CodePrNotFound, strings.TrimSpace(combinedOutput))
	}

	// Network errors
	if strings.Contains(lower, "could not resolve host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "unable to access") ||
		strings.Contains(lower, "timed out") {
		return apperrors.PrNetworkError(err)
	}

	// Fallback
	return apperrors.PrGhError(err)
}

// ghPrListFields are the fields requested from gh pr list --json.
var ghPrListFields = "number,title,state,isDraft,headRefName,baseRefName,author,url,updatedAt"

// ghPrViewFields are the fields requested from gh pr view --json.
var ghPrViewFields = "number,title,state,isDraft,headRefName,baseRefName,author,url,body"

// ghPrCreateFields are the fields requested from gh pr create --json.
var ghPrCreateFields = ghPrViewFields

// ghListEntry matches the JSON shape returned by gh pr list --json.
type ghListEntry struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	IsDraft     bool      `json:"isDraft"`
	HeadRefName string    `json:"headRefName"`
	BaseRefName string    `json:"baseRefName"`
	Author      ghAuthor  `json:"author"`
	URL         string    `json:"url"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ghDetailEntry matches the JSON shape returned by gh pr view/create --json.
type ghDetailEntry struct {
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	State       string   `json:"state"`
	IsDraft     bool     `json:"isDraft"`
	HeadRefName string   `json:"headRefName"`
	BaseRefName string   `json:"baseRefName"`
	Author      ghAuthor `json:"author"`
	URL         string   `json:"url"`
	Body        string   `json:"body"`
}

type ghAuthor struct {
	Login string `json:"login"`
}

// PrList returns up to 50 open PRs sorted by updated_at DESC, number DESC.
func (g *GitOperations) PrList() ([]RepoPrEntryPayload, error) {
	stdout, stderr, err := g.runGhWithTimeout(prListTimeout,
		"pr", "list", "--state", "open", "--json", ghPrListFields, "--limit", "50")
	if err != nil {
		return nil, classifyPrError(stderr+stdout, err)
	}

	var entries []ghListEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		return nil, apperrors.PrGhError(fmt.Errorf("malformed gh pr list JSON: %w", err))
	}

	// Validate all required fields and convert to payload
	result := make([]RepoPrEntryPayload, 0, len(entries))
	for _, e := range entries {
		if e.Number == 0 || e.Title == "" || e.HeadRefName == "" || e.BaseRefName == "" || e.URL == "" {
			return nil, apperrors.PrGhError(fmt.Errorf("malformed PR list entry: missing required fields in PR #%d", e.Number))
		}
		result = append(result, RepoPrEntryPayload{
			Number:     e.Number,
			Title:      e.Title,
			State:      strings.ToLower(e.State),
			IsDraft:    e.IsDraft,
			HeadBranch: e.HeadRefName,
			BaseBranch: e.BaseRefName,
			Author:     e.Author.Login,
			URL:        e.URL,
			UpdatedAt:  e.UpdatedAt.Format(time.RFC3339),
		})
	}

	// Sort by updated_at DESC, tie-break number DESC
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt != result[j].UpdatedAt {
			return result[i].UpdatedAt > result[j].UpdatedAt
		}
		return result[i].Number > result[j].Number
	})

	// Cap at 50
	if len(result) > 50 {
		result = result[:50]
	}

	return result, nil
}

// PrView returns the detail for a single PR by number.
func (g *GitOperations) PrView(number int) (*RepoPrDetailPayload, error) {
	stdout, stderr, err := g.runGhWithTimeout(prViewTimeout,
		"pr", "view", fmt.Sprintf("%d", number), "--json", ghPrViewFields)
	if err != nil {
		return nil, classifyPrError(stderr+stdout, err)
	}

	var entry ghDetailEntry
	if err := json.Unmarshal([]byte(stdout), &entry); err != nil {
		return nil, apperrors.PrGhError(fmt.Errorf("malformed gh pr view JSON: %w", err))
	}

	if entry.Number == 0 || entry.Title == "" || entry.URL == "" {
		return nil, apperrors.PrGhError(fmt.Errorf("malformed PR view entry: missing required fields"))
	}

	return &RepoPrDetailPayload{
		Number:     entry.Number,
		Title:      entry.Title,
		State:      strings.ToLower(entry.State),
		IsDraft:    entry.IsDraft,
		HeadBranch: entry.HeadRefName,
		BaseBranch: entry.BaseRefName,
		Author:     entry.Author.Login,
		URL:        entry.URL,
		Body:       entry.Body,
	}, nil
}

// PrCreate creates a new PR and returns the detail.
func (g *GitOperations) PrCreate(title, body, baseBranch string, draft bool) (*RepoPrDetailPayload, error) {
	args := []string{"pr", "create", "--title", title}
	if body != "" {
		args = append(args, "--body", body)
	} else {
		args = append(args, "--body", "")
	}
	if baseBranch != "" {
		args = append(args, "--base", baseBranch)
	}
	if draft {
		args = append(args, "--draft")
	}
	// Request structured JSON output for the created PR
	args = append(args, "--json", ghPrCreateFields)

	stdout, stderr, err := g.runGhWithTimeout(prCreateTimeout, args...)
	if err != nil {
		return nil, classifyPrError(stderr+stdout, err)
	}

	// gh pr create with --json outputs a structured PR JSON object
	var entry ghDetailEntry
	if err := json.Unmarshal([]byte(stdout), &entry); err != nil {
		return nil, apperrors.PrGhError(fmt.Errorf("malformed gh pr create JSON: %w", err))
	}

	if entry.Number == 0 || entry.Title == "" || entry.URL == "" {
		return nil, apperrors.PrGhError(fmt.Errorf("malformed PR create response: missing required fields"))
	}

	return &RepoPrDetailPayload{
		Number:     entry.Number,
		Title:      entry.Title,
		State:      strings.ToLower(entry.State),
		IsDraft:    entry.IsDraft,
		HeadBranch: entry.HeadRefName,
		BaseBranch: entry.BaseRefName,
		Author:     entry.Author.Login,
		URL:        entry.URL,
		Body:       entry.Body,
	}, nil
}

// PrCheckout checks out a PR by number.
// Returns branchName, changedBranch, and error.
// On GetCurrentBranchName failure, defaults changedBranch=false (safe no-op).
func (g *GitOperations) PrCheckout(number int) (branchName string, changedBranch bool, err error) {
	// Capture branch before checkout
	branchBefore, beforeErr := g.GetCurrentBranchName()

	_, stderr, runErr := g.runGhWithTimeout(prCheckoutTimeout,
		"pr", "checkout", fmt.Sprintf("%d", number))
	if runErr != nil {
		return "", false, classifyPrError(stderr, runErr)
	}

	// Capture branch after checkout
	branchAfter, afterErr := g.GetCurrentBranchName()
	if afterErr != nil {
		// Cannot determine branch after checkout; default changedBranch=false
		return "", false, nil
	}

	changedBranch = beforeErr != nil || branchBefore != branchAfter
	return branchAfter, changedBranch, nil
}

// extractPushSummary extracts a summary line from git push output.
// Git push output typically includes "To <remote>" and ref update info.
func extractPushSummary(output string) string {
	lines := strings.Split(output, "\n")

	// Look for the "To <remote>" line or ref update line
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "To ") || strings.Contains(line, "->") {
			return line
		}
	}

	// Return the last non-empty line as fallback
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}

	return "Push completed"
}
