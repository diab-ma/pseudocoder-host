package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetRepoStatus_Branch tests that GetRepoStatus returns the current branch.
func TestGetRepoStatus_Branch(t *testing.T) {
	dir := setupTestRepo(t)

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// Default branch should be main or master depending on git version
	if status.Branch != "main" && status.Branch != "master" {
		t.Errorf("expected branch main or master, got %s", status.Branch)
	}
}

// TestGetRepoStatus_StagedCount tests staged file counting.
func TestGetRepoStatus_StagedCount(t *testing.T) {
	dir := setupTestRepo(t)

	// Create and stage a file
	testFile := filepath.Join(dir, "staged.txt")
	if err := os.WriteFile(testFile, []byte("staged content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "staged.txt")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.StagedCount != 1 {
		t.Errorf("expected 1 staged file, got %d", status.StagedCount)
	}
}

// TestGetRepoStatus_UnstagedCount tests unstaged file counting.
func TestGetRepoStatus_UnstagedCount(t *testing.T) {
	dir := setupTestRepo(t)

	// Modify the initial file without staging
	testFile := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(testFile, []byte("modified content"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.UnstagedCount != 1 {
		t.Errorf("expected 1 unstaged file, got %d", status.UnstagedCount)
	}
}

// TestGetRepoStatus_UntrackedFiles tests that untracked files are included in unstaged count.
func TestGetRepoStatus_UntrackedFiles(t *testing.T) {
	dir := setupTestRepo(t)

	// Create an untracked file (not git added)
	untrackedFile := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("new file content"), 0644); err != nil {
		t.Fatalf("failed to create untracked file: %v", err)
	}

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// Untracked file should be counted in unstaged
	if status.UnstagedCount != 1 {
		t.Errorf("expected 1 unstaged file (untracked), got %d", status.UnstagedCount)
	}
}

// TestGetRepoStatus_MixedUnstagedAndUntracked tests counting both modified and untracked files.
func TestGetRepoStatus_MixedUnstagedAndUntracked(t *testing.T) {
	dir := setupTestRepo(t)

	// Modify an existing tracked file
	trackedFile := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(trackedFile, []byte("modified content"), 0644); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}

	// Create an untracked file
	untrackedFile := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("new file content"), 0644); err != nil {
		t.Fatalf("failed to create untracked file: %v", err)
	}

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// Should count both: 1 modified + 1 untracked = 2
	if status.UnstagedCount != 2 {
		t.Errorf("expected 2 unstaged files (1 modified + 1 untracked), got %d", status.UnstagedCount)
	}
}

// TestGetRepoStatus_LastCommit tests last commit display.
func TestGetRepoStatus_LastCommit(t *testing.T) {
	dir := setupTestRepo(t)

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// Should contain "Initial commit"
	if !strings.Contains(status.LastCommit, "Initial commit") {
		t.Errorf("expected last commit to contain 'Initial commit', got %s", status.LastCommit)
	}
}

// TestGetRepoStatus_EmptyRepo tests repo.status on a repository with no commits.
func TestGetRepoStatus_EmptyRepo(t *testing.T) {
	dir := t.TempDir()

	// Initialize git repo but don't create any commits
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed on empty repo: %v", err)
	}

	// Branch should still be detected (main or master depending on git version)
	if status.Branch != "main" && status.Branch != "master" {
		t.Errorf("expected branch main or master, got %s", status.Branch)
	}

	// No upstream in fresh repo
	if status.Upstream != "" {
		t.Errorf("expected empty upstream, got %s", status.Upstream)
	}

	// No staged or unstaged changes
	if status.StagedCount != 0 {
		t.Errorf("expected 0 staged, got %d", status.StagedCount)
	}
	if status.UnstagedCount != 0 {
		t.Errorf("expected 0 unstaged, got %d", status.UnstagedCount)
	}

	// No last commit
	if status.LastCommit != "" {
		t.Errorf("expected empty last commit, got %s", status.LastCommit)
	}
}

// TestGetRepoStatus_DetachedHead tests repo.status when HEAD is detached.
func TestGetRepoStatus_DetachedHead(t *testing.T) {
	dir := setupTestRepo(t)

	// Get the commit hash and checkout detached
	hash, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("failed to get HEAD hash: %v", err)
	}
	runGitCmd(t, dir, "checkout", "--detach", strings.TrimSpace(string(hash)))

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed on detached HEAD: %v", err)
	}

	// Detached HEAD shows "HEAD" as branch name
	if status.Branch != "HEAD" {
		t.Errorf("expected branch HEAD for detached state, got %s", status.Branch)
	}

	// Last commit should still be available
	if status.LastCommit == "" {
		t.Error("expected non-empty last commit in detached HEAD state")
	}
}

// TestGetRepoStatus_StagedFiles tests that staged file names are returned.
func TestGetRepoStatus_StagedFiles(t *testing.T) {
	dir := setupTestRepo(t)

	// Create and stage two files
	file1 := filepath.Join(dir, "staged1.txt")
	file2 := filepath.Join(dir, "staged2.txt")
	if err := os.WriteFile(file1, []byte("content1"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if err := os.WriteFile(file2, []byte("content2"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "staged1.txt", "staged2.txt")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// Check count matches file list length
	if status.StagedCount != 2 {
		t.Errorf("expected 2 staged files, got %d", status.StagedCount)
	}
	if len(status.StagedFiles) != 2 {
		t.Errorf("expected 2 staged file names, got %d", len(status.StagedFiles))
	}

	// Check file names are present (order may vary)
	found1, found2 := false, false
	for _, f := range status.StagedFiles {
		if f == "staged1.txt" {
			found1 = true
		}
		if f == "staged2.txt" {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Errorf("expected staged1.txt and staged2.txt in StagedFiles, got %v", status.StagedFiles)
	}
}

// TestGetRepoStatus_StagedFilesEmpty tests that StagedFiles is an empty slice when nothing is staged.
func TestGetRepoStatus_StagedFilesEmpty(t *testing.T) {
	dir := setupTestRepo(t)

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	// StagedFiles should be empty slice, not nil
	if status.StagedFiles == nil {
		t.Error("expected StagedFiles to be empty slice, got nil")
	}
	if len(status.StagedFiles) != 0 {
		t.Errorf("expected 0 staged files, got %d: %v", len(status.StagedFiles), status.StagedFiles)
	}
	if status.StagedCount != 0 {
		t.Errorf("expected StagedCount 0, got %d", status.StagedCount)
	}
}

// TestGetRepoStatus_StagedFilesInSubdir tests that staged files in subdirectories are returned with paths.
func TestGetRepoStatus_StagedFilesInSubdir(t *testing.T) {
	dir := setupTestRepo(t)

	// Create a subdirectory and stage a file in it
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	file := filepath.Join(subdir, "nested.txt")
	if err := os.WriteFile(file, []byte("nested content"), 0644); err != nil {
		t.Fatalf("failed to create nested file: %v", err)
	}
	runGitCmd(t, dir, "add", "subdir/nested.txt")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if len(status.StagedFiles) != 1 {
		t.Errorf("expected 1 staged file, got %d", len(status.StagedFiles))
	}
	if status.StagedFiles[0] != "subdir/nested.txt" {
		t.Errorf("expected 'subdir/nested.txt', got '%s'", status.StagedFiles[0])
	}
}

// TestCommit_Success tests successful commit creation.
func TestCommit_Success(t *testing.T) {
	dir := setupTestRepo(t)

	// Stage a new file
	testFile := filepath.Join(dir, "newfile.txt")
	if err := os.WriteFile(testFile, []byte("new content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "newfile.txt")

	ops := NewGitOperations(dir, false, false, false)
	hash, summary, err := ops.Commit("Add newfile.txt", false, false)
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	if hash == "" {
		t.Error("expected non-empty commit hash")
	}
	if len(hash) < 7 {
		t.Errorf("expected commit hash of at least 7 chars, got %s", hash)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}

	// Verify the commit exists
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("failed to get commit log: %v", err)
	}
	if !strings.Contains(string(out), "Add newfile.txt") {
		t.Errorf("commit message not found in log: %s", out)
	}
}

// TestCommit_EmptyMessage tests that empty commit messages are rejected.
func TestCommit_EmptyMessage(t *testing.T) {
	dir := setupTestRepo(t)

	// Stage a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "test.txt")

	ops := NewGitOperations(dir, false, false, false)

	// Test empty message
	_, _, err := ops.Commit("", false, false)
	if err == nil {
		t.Fatal("expected error for empty message")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected error to mention 'empty', got: %v", err)
	}

	// Test whitespace-only message
	_, _, err = ops.Commit("   \t\n  ", false, false)
	if err == nil {
		t.Fatal("expected error for whitespace-only message")
	}
}

// TestCommit_NoStagedChanges tests that commits with no staged changes are rejected.
func TestCommit_NoStagedChanges(t *testing.T) {
	dir := setupTestRepo(t)

	// Don't stage anything
	ops := NewGitOperations(dir, false, false, false)
	_, _, err := ops.Commit("This should fail", false, false)
	if err == nil {
		t.Fatal("expected error for no staged changes")
	}
	if !strings.Contains(err.Error(), "staged") {
		t.Errorf("expected error to mention 'staged', got: %v", err)
	}
}

// TestCommit_NoVerifyAllowed tests --no-verify when allowed.
func TestCommit_NoVerifyAllowed(t *testing.T) {
	dir := setupTestRepo(t)

	// Create a pre-commit hook that always fails
	hooksDir := filepath.Join(dir, ".git", "hooks")
	hookFile := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(hookFile, []byte("#!/bin/sh\nexit 1"), 0755); err != nil {
		t.Fatalf("failed to create hook: %v", err)
	}

	// Stage a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "test.txt")

	// With allowNoVerify=true and noVerify=true, commit should succeed
	ops := NewGitOperations(dir, true, false, false)
	hash, _, err := ops.Commit("Commit with --no-verify", true, false)
	if err != nil {
		t.Fatalf("Commit with --no-verify failed: %v", err)
	}
	if hash == "" {
		t.Error("expected non-empty commit hash")
	}
}

// TestCommit_NoVerifyDisallowed tests --no-verify when not allowed.
func TestCommit_NoVerifyDisallowed(t *testing.T) {
	dir := setupTestRepo(t)

	// Create a pre-commit hook that always fails
	hooksDir := filepath.Join(dir, ".git", "hooks")
	hookFile := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(hookFile, []byte("#!/bin/sh\necho 'hook failed'\nexit 1"), 0755); err != nil {
		t.Fatalf("failed to create hook: %v", err)
	}

	// Stage a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "test.txt")

	// With allowNoVerify=false, noVerify=true should be ignored and hook should run
	ops := NewGitOperations(dir, false, false, false)
	_, _, err := ops.Commit("This should fail because hook runs", true, false)
	if err == nil {
		t.Fatal("expected hook failure when --no-verify is disallowed")
	}
	if !strings.Contains(err.Error(), "hook") {
		t.Errorf("expected error to mention 'hook', got: %v", err)
	}
}

// TestCommit_HookFailure tests hook failure detection.
func TestCommit_HookFailure(t *testing.T) {
	dir := setupTestRepo(t)

	// Create a pre-commit hook that fails with a message
	hooksDir := filepath.Join(dir, ".git", "hooks")
	hookFile := filepath.Join(hooksDir, "pre-commit")
	hookScript := "#!/bin/sh\necho 'pre-commit hook failed: lint error'\nexit 1"
	if err := os.WriteFile(hookFile, []byte(hookScript), 0755); err != nil {
		t.Fatalf("failed to create hook: %v", err)
	}

	// Stage a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "test.txt")

	ops := NewGitOperations(dir, false, false, false)
	_, _, err := ops.Commit("This should fail", false, false)
	if err == nil {
		t.Fatal("expected hook failure error")
	}

	// Error should mention hook
	errStr := err.Error()
	if !strings.Contains(errStr, "hook") {
		t.Errorf("expected error to mention 'hook', got: %s", errStr)
	}
}

// TestCommit_NoGpgSignAllowed tests --no-gpg-sign when allowed.
func TestCommit_NoGpgSignAllowed(t *testing.T) {
	dir := setupTestRepo(t)

	// Stage a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCmd(t, dir, "add", "test.txt")

	// With allowNoGpgSign=true, commit should work even if user has gpg signing configured
	ops := NewGitOperations(dir, false, true, false)
	hash, _, err := ops.Commit("Commit with --no-gpg-sign", false, true)
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if hash == "" {
		t.Error("expected non-empty commit hash")
	}
}

// TestCountLines tests the countLines helper function.
func TestCountLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"empty", "", 0},
		{"whitespace only", "   \n  \t  ", 0},
		{"single line", "file.txt", 1},
		{"two lines", "file1.txt\nfile2.txt", 2},
		{"trailing newline", "file.txt\n", 1},
		{"multiple newlines", "a\nb\nc\n", 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countLines(tc.input)
			if got != tc.expected {
				t.Errorf("countLines(%q) = %d, want %d", tc.input, got, tc.expected)
			}
		})
	}
}

// TestExtractCommitSummary tests the extractCommitSummary helper function.
func TestExtractCommitSummary(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "typical commit output",
			input: `[main 1234567] Add feature
 1 file changed, 5 insertions(+)`,
			expected: "1 file changed, 5 insertions(+)",
		},
		{
			name: "insertions and deletions",
			input: `[main abc1234] Update code
 3 files changed, 20 insertions(+), 10 deletions(-)`,
			expected: "3 files changed, 20 insertions(+), 10 deletions(-)",
		},
		{
			name:     "fallback to last line",
			input:    "[main def5678] Some message",
			expected: "[main def5678] Some message",
		},
		{
			name:     "empty output",
			input:    "",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCommitSummary(tc.input)
			if got != tc.expected {
				t.Errorf("extractCommitSummary() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestExtractPushSummary tests the extractPushSummary helper function.
func TestExtractPushSummary(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "typical push output with To line",
			input: `Enumerating objects: 5, done.
Counting objects: 100% (5/5), done.
Writing objects: 100% (3/3), 300 bytes | 300.00 KiB/s, done.
Total 3 (delta 0), reused 0 (delta 0)
To github.com:user/repo.git
   abc1234..def5678  main -> main`,
			expected: "To github.com:user/repo.git",
		},
		{
			name: "push output with arrow",
			input: `   abc1234..def5678  main -> main`,
			expected: "abc1234..def5678  main -> main",
		},
		{
			name:     "empty output",
			input:    "",
			expected: "Push completed",
		},
		{
			name:     "whitespace only",
			input:    "   \n  \t  ",
			expected: "Push completed",
		},
		{
			name:     "fallback to last line",
			input:    "Everything up-to-date",
			expected: "Everything up-to-date",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPushSummary(tc.input)
			if got != tc.expected {
				t.Errorf("extractPushSummary() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestClassifyPushError tests error classification for push failures.
func TestClassifyPushError(t *testing.T) {
	ops := NewGitOperations("/tmp", false, false, false)
	baseErr := exec.Command("false").Run() // Create a simple error

	tests := []struct {
		name         string
		output       string
		expectedCode string
	}{
		{
			name:         "non-fast-forward",
			output:       "error: failed to push some refs\nhint: Updates were rejected because the tip of your current branch is behind\nhint: non-fast-forward",
			expectedCode: "push.non_ff",
		},
		{
			name:         "rejected",
			output:       "! [rejected]        main -> main (fetch first)",
			expectedCode: "push.non_ff",
		},
		{
			name:         "fetch first hint",
			output:       "error: failed to push some refs\nhint: fetch first",
			expectedCode: "push.non_ff",
		},
		{
			name:         "permission denied",
			output:       "Permission denied (publickey).\nfatal: Could not read from remote repository.",
			expectedCode: "push.auth_failed",
		},
		{
			name:         "authentication failed",
			output:       "fatal: Authentication failed for 'https://github.com/user/repo.git/'",
			expectedCode: "push.auth_failed",
		},
		{
			name:         "could not read username",
			output:       "fatal: could not read Username for 'https://github.com': terminal prompts disabled",
			expectedCode: "push.auth_failed",
		},
		{
			name:         "repository not found",
			output:       "ERROR: Repository not found.\nfatal: Could not read from remote repository.",
			expectedCode: "push.auth_failed",
		},
		{
			name:         "generic error",
			output:       "error: something unexpected happened",
			expectedCode: "push.git_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ops.classifyPushError(tc.output, baseErr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.expectedCode) {
				t.Errorf("expected error code %s, got: %v", tc.expectedCode, err)
			}
		})
	}
}

// TestPush_NoUpstream tests that push without upstream returns appropriate error.
func TestPush_NoUpstream(t *testing.T) {
	dir := setupTestRepo(t)

	// No upstream configured in a fresh repo
	ops := NewGitOperations(dir, false, false, false)

	// Push with empty remote/branch should fail with no_upstream error
	_, err := ops.Push("", "", false)
	if err == nil {
		t.Fatal("expected error for push without upstream")
	}
	if !strings.Contains(err.Error(), "push.no_upstream") {
		t.Errorf("expected push.no_upstream error, got: %v", err)
	}
}

// TestPush_ExplicitRemoteBranch tests push with explicit remote and branch.
// This test verifies the code path but expects failure since there's no actual remote.
func TestPush_ExplicitRemoteBranch(t *testing.T) {
	dir := setupTestRepo(t)

	ops := NewGitOperations(dir, false, false, false)

	// Push with explicit remote/branch - will fail because remote doesn't exist
	// but this tests the code path that doesn't require upstream
	_, err := ops.Push("nonexistent", "main", false)
	if err == nil {
		t.Fatal("expected error for push to nonexistent remote")
	}
	// Should be a git error, not a no_upstream error
	if strings.Contains(err.Error(), "push.no_upstream") {
		t.Errorf("should not be no_upstream error when explicit remote/branch provided, got: %v", err)
	}
}

// TestPush_ForceWithLeaseDisallowed tests that force-with-lease is ignored when not allowed.
func TestPush_ForceWithLeaseDisallowed(t *testing.T) {
	dir := setupTestRepo(t)

	// allowForceWithLease = false
	ops := NewGitOperations(dir, false, false, false)

	// Even with forceWithLease=true, it should be ignored
	// This will fail for other reasons, but we're testing that the flag doesn't cause issues
	_, err := ops.Push("nonexistent", "main", true)
	if err == nil {
		t.Fatal("expected error for push to nonexistent remote")
	}
	// The error should NOT mention force-with-lease since it was ignored
	// Just verify we got an error and the code path worked
}

// TestPush_InvalidArgs tests that invalid remote/branch arguments are rejected.
// This prevents option injection and command injection attacks (Unit 7.7).
func TestPush_InvalidArgs(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		branch  string
		wantErr string
	}{
		// Option injection - remote
		{"remote starts with dash", "--force", "main", "push.invalid_args"},
		{"remote is -v", "-v", "main", "push.invalid_args"},
		{"remote is --force-with-lease", "--force-with-lease", "main", "push.invalid_args"},

		// Option injection - branch
		{"branch starts with dash", "origin", "--delete", "push.invalid_args"},
		{"branch is -f", "origin", "-f", "push.invalid_args"},

		// Shell metacharacters - remote
		{"remote with semicolon", "origin;rm", "main", "push.invalid_args"},
		{"remote with pipe", "origin|cat", "main", "push.invalid_args"},
		{"remote with ampersand", "origin&", "main", "push.invalid_args"},
		{"remote with redirect", "origin>file", "main", "push.invalid_args"},

		// Shell metacharacters - branch
		{"branch with semicolon", "origin", "; rm -rf /", "push.invalid_args"},
		{"branch with command sub", "origin", "$(whoami)", "push.invalid_args"},
		{"branch with backtick", "origin", "`id`", "push.invalid_args"},
		{"branch with redirect", "origin", "main>file", "push.invalid_args"},
		{"branch with glob star", "origin", "main*", "push.invalid_args"},
		{"branch with glob question", "origin", "main?", "push.invalid_args"},
		{"branch with newline", "origin", "main\necho bad", "push.invalid_args"},
		{"branch with tab", "origin", "main\tevil", "push.invalid_args"},
		{"branch with single quote", "origin", "main'evil", "push.invalid_args"},
		{"branch with double quote", "origin", "main\"evil", "push.invalid_args"},
		{"branch with dollar", "origin", "$PATH", "push.invalid_args"},
		{"branch with less than", "origin", "main<file", "push.invalid_args"},
		{"branch with null byte", "origin", "main\x00evil", "push.invalid_args"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			gitOps := NewGitOperations(dir, false, false, false)

			_, err := gitOps.Push(tt.remote, tt.branch, false)
			if err == nil {
				t.Error("expected error, got nil")
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// TestPush_ValidArgs tests that valid remote/branch names pass validation.
// These may still fail later (e.g., no upstream), but should not fail validation.
func TestPush_ValidArgs(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		branch string
	}{
		{"simple names", "origin", "main"},
		{"with slash", "origin", "feature/test"},
		{"with dots", "origin", "release.1.0"},
		{"with underscore", "upstream", "fix_bug"},
		{"with hyphen", "my-remote", "my-branch"},
		{"numeric", "origin2", "v1.0.0"},
		{"empty remote uses upstream", "", "main"},
		{"empty branch uses upstream", "origin", ""},
		{"both empty uses upstream", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			gitOps := NewGitOperations(dir, false, false, false)

			_, err := gitOps.Push(tt.remote, tt.branch, false)
			// We expect an error (no upstream or no such remote),
			// but it should NOT be push.invalid_args
			if err != nil && strings.Contains(err.Error(), "push.invalid_args") {
				t.Errorf("unexpected invalid_args error for valid input: %v", err)
			}
		})
	}
}

// TestValidatePushArg tests the validatePushArg helper function directly.
func TestValidatePushArg(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		argType string
		wantErr bool
	}{
		// Valid inputs
		{"empty allowed", "", "remote", false},
		{"simple name", "origin", "remote", false},
		{"with hyphen", "my-remote", "remote", false},
		{"with underscore", "my_remote", "remote", false},
		{"with slash", "feature/test", "branch", false},
		{"with dots", "v1.0.0", "branch", false},

		// Invalid - option injection
		{"starts with dash", "-v", "remote", true},
		{"starts with double dash", "--force", "remote", true},

		// Invalid - shell metacharacters
		{"semicolon", "a;b", "remote", true},
		{"pipe", "a|b", "remote", true},
		{"ampersand", "a&b", "remote", true},
		{"less than", "a<b", "remote", true},
		{"greater than", "a>b", "remote", true},
		{"dollar", "$var", "remote", true},
		{"backtick", "`cmd`", "remote", true},
		{"star", "a*", "remote", true},
		{"question", "a?", "remote", true},
		{"single quote", "a'b", "remote", true},
		{"double quote", "a\"b", "remote", true},
		{"newline", "a\nb", "remote", true},
		{"carriage return", "a\rb", "remote", true},
		{"tab", "a\tb", "remote", true},
		{"null byte", "a\x00b", "remote", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePushArg(tt.input, tt.argType)
			if tt.wantErr && err == nil {
				t.Errorf("validatePushArg(%q, %q) = nil, want error", tt.input, tt.argType)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validatePushArg(%q, %q) = %v, want nil", tt.input, tt.argType, err)
			}
		})
	}
}

// setupTestRepo creates a temporary git repository for testing.
// It returns the path to the repo directory. The directory is automatically
// cleaned up when the test completes.
func setupTestRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	// Initialize git repo
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	// Create an initial commit so we have a HEAD
	initialFile := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(initialFile, []byte("initial content"), 0644); err != nil {
		t.Fatalf("failed to create initial file: %v", err)
	}
	runGitCmd(t, dir, "add", "initial.txt")
	runGitCmd(t, dir, "commit", "-m", "Initial commit")

	return dir
}

// runGitCmd runs a git command in the specified directory.
func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// TestSplitLines tests that splitLines correctly handles both LF and CRLF line endings.
func TestSplitLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: []string{},
		},
		{
			name:     "single line LF",
			input:    "file.txt\n",
			expected: []string{"file.txt"},
		},
		{
			name:     "single line CRLF",
			input:    "file.txt\r\n",
			expected: []string{"file.txt"},
		},
		{
			name:     "multiple lines LF",
			input:    "file1.txt\nfile2.go\nfile3.md\n",
			expected: []string{"file1.txt", "file2.go", "file3.md"},
		},
		{
			name:     "multiple lines CRLF (Windows)",
			input:    "file1.txt\r\nfile2.go\r\nfile3.md\r\n",
			expected: []string{"file1.txt", "file2.go", "file3.md"},
		},
		{
			name:     "mixed line endings",
			input:    "file1.txt\r\nfile2.go\nfile3.md\r\n",
			expected: []string{"file1.txt", "file2.go", "file3.md"},
		},
		{
			name:     "no trailing newline",
			input:    "file1.txt\nfile2.go",
			expected: []string{"file1.txt", "file2.go"},
		},
		{
			name:     "preserve spaces in filenames",
			input:    " file with spaces.txt\r\nanother file.go\r\n",
			expected: []string{" file with spaces.txt", "another file.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitLines(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d lines, got %d", len(tt.expected), len(result))
				return
			}
			for i, line := range result {
				if line != tt.expected[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.expected[i], line)
				}
				// Verify no trailing \r remains (Windows bug check)
				if strings.HasSuffix(line, "\r") {
					t.Errorf("line %d still has trailing \\r: %q", i, line)
				}
			}
		})
	}
}
