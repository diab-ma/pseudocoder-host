// Package server provides the WebSocket server for client connections.
// This file contains git operations for commit and repository status workflows.
package server

import (
	"bytes"
	"os/exec"
	"strings"

	apperrors "github.com/pseudocoder/host/internal/errors"
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

	return status, nil
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
