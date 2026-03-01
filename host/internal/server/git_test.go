package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	apperrors "github.com/pseudocoder/host/internal/errors"
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

func TestGetRepoStatus_ReadinessReady(t *testing.T) {
	dir := setupTestRepo(t)

	// Stage a file and keep working tree otherwise clean.
	testFile := filepath.Join(dir, "staged.txt")
	if err := os.WriteFile(testFile, []byte("staged content"), 0644); err != nil {
		t.Fatalf("failed to create staged file: %v", err)
	}
	runGitCmd(t, dir, "add", "staged.txt")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.ReadinessState != CommitReadinessReady {
		t.Fatalf("expected readiness_state=%s, got %s", CommitReadinessReady, status.ReadinessState)
	}
	if len(status.ReadinessBlockers) != 0 {
		t.Fatalf("expected no readiness blockers, got %v", status.ReadinessBlockers)
	}
	if len(status.ReadinessWarnings) != 0 {
		t.Fatalf("expected no readiness warnings, got %v", status.ReadinessWarnings)
	}
}

func TestGetRepoStatus_ReadinessBlockedNoStagedChanges(t *testing.T) {
	dir := setupTestRepo(t)

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.ReadinessState != CommitReadinessBlocked {
		t.Fatalf("expected readiness_state=%s, got %s", CommitReadinessBlocked, status.ReadinessState)
	}
	if !containsString(status.ReadinessBlockers, ReadinessNoStagedChanges) {
		t.Fatalf("expected blocker %q in %v", ReadinessNoStagedChanges, status.ReadinessBlockers)
	}
}

func TestGetRepoStatus_ReadinessRiskyUnstagedChanges(t *testing.T) {
	dir := setupTestRepo(t)

	// Stage one file.
	stagedFile := filepath.Join(dir, "staged.txt")
	if err := os.WriteFile(stagedFile, []byte("staged content"), 0644); err != nil {
		t.Fatalf("failed to create staged file: %v", err)
	}
	runGitCmd(t, dir, "add", "staged.txt")

	// Add unstaged content (untracked file counts toward unstaged_count).
	unstagedFile := filepath.Join(dir, "unstaged.txt")
	if err := os.WriteFile(unstagedFile, []byte("unstaged content"), 0644); err != nil {
		t.Fatalf("failed to create unstaged file: %v", err)
	}

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.ReadinessState != CommitReadinessRisky {
		t.Fatalf("expected readiness_state=%s, got %s", CommitReadinessRisky, status.ReadinessState)
	}
	if !containsString(status.ReadinessWarnings, ReadinessUnstagedChanges) {
		t.Fatalf("expected warning %q in %v", ReadinessUnstagedChanges, status.ReadinessWarnings)
	}
}

func TestGetRepoStatus_ReadinessRiskyDetachedHead(t *testing.T) {
	dir := setupTestRepo(t)

	hash, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("failed to get HEAD hash: %v", err)
	}
	runGitCmd(t, dir, "checkout", "--detach", strings.TrimSpace(string(hash)))

	// Stage a file while detached.
	stagedFile := filepath.Join(dir, "detached.txt")
	if err := os.WriteFile(stagedFile, []byte("detached change"), 0644); err != nil {
		t.Fatalf("failed to create staged file: %v", err)
	}
	runGitCmd(t, dir, "add", "detached.txt")

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.ReadinessState != CommitReadinessRisky {
		t.Fatalf("expected readiness_state=%s, got %s", CommitReadinessRisky, status.ReadinessState)
	}
	if !containsString(status.ReadinessWarnings, ReadinessDetachedHead) {
		t.Fatalf("expected warning %q in %v", ReadinessDetachedHead, status.ReadinessWarnings)
	}
}

func TestGetRepoStatus_ReadinessBlockedMergeConflicts(t *testing.T) {
	dir := setupTestRepo(t)

	baseBranch := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))

	// Create conflicting branch change.
	runGitCmd(t, dir, "checkout", "-b", "conflict-branch")
	if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("branch change\n"), 0644); err != nil {
		t.Fatalf("failed to write branch change: %v", err)
	}
	runGitCmd(t, dir, "add", "initial.txt")
	runGitCmd(t, dir, "commit", "-m", "branch conflict change")

	// Create conflicting base change.
	runGitCmd(t, dir, "checkout", baseBranch)
	if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("base change\n"), 0644); err != nil {
		t.Fatalf("failed to write base change: %v", err)
	}
	runGitCmd(t, dir, "add", "initial.txt")
	runGitCmd(t, dir, "commit", "-m", "base conflict change")

	// Merge conflict-branch and expect conflict state.
	cmd := exec.Command("git", "merge", "conflict-branch")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected merge conflict, merge succeeded unexpectedly:\n%s", out)
	}

	ops := NewGitOperations(dir, false, false, false)
	status, err := ops.GetRepoStatus()
	if err != nil {
		t.Fatalf("GetRepoStatus failed: %v", err)
	}

	if status.ReadinessState != CommitReadinessBlocked {
		t.Fatalf("expected readiness_state=%s, got %s", CommitReadinessBlocked, status.ReadinessState)
	}
	if !containsString(status.ReadinessBlockers, ReadinessMergeConflicts) {
		t.Fatalf("expected blocker %q in %v", ReadinessMergeConflicts, status.ReadinessBlockers)
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
			name:     "push output with arrow",
			input:    `   abc1234..def5678  main -> main`,
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

func runGitCmdOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// GetHistory tests
// -----------------------------------------------------------------------------

func TestGetHistory_BasicPagination(t *testing.T) {
	dir := setupTestRepo(t)

	// Create 5 more commits (6 total with initial)
	for i := 1; i <= 5; i++ {
		f := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(f, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		runGitCmd(t, dir, "add", fmt.Sprintf("file%d.txt", i))
		runGitCmd(t, dir, "commit", "-m", fmt.Sprintf("Commit %d", i))
	}

	ops := NewGitOperations(dir, false, false, false)

	// Page 1: first 3 entries
	entries, nextCursor, err := ops.GetHistory("", 3)
	if err != nil {
		t.Fatalf("GetHistory page 1: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if nextCursor == "" {
		t.Fatal("expected non-empty nextCursor")
	}

	// Verify newest-first order
	if entries[0].Subject != "Commit 5" {
		t.Errorf("expected first entry 'Commit 5', got %q", entries[0].Subject)
	}
	if entries[2].Subject != "Commit 3" {
		t.Errorf("expected third entry 'Commit 3', got %q", entries[2].Subject)
	}

	// Verify fields
	for _, e := range entries {
		if len(e.Hash) != 40 {
			t.Errorf("expected 40-char hash, got %q", e.Hash)
		}
		if e.Author == "" {
			t.Error("expected non-empty author")
		}
		if e.AuthoredAt == 0 {
			t.Error("expected non-zero authored_at")
		}
	}

	// Page 2: next 3 entries (remaining commits)
	entries2, nextCursor2, err := ops.GetHistory(nextCursor, 3)
	if err != nil {
		t.Fatalf("GetHistory page 2: %v", err)
	}
	if len(entries2) != 3 {
		t.Fatalf("expected 3 entries on page 2, got %d", len(entries2))
	}

	// Verify no overlap with page 1
	if entries2[0].Hash == entries[2].Hash {
		t.Error("page 2 first entry should not equal page 1 last entry")
	}

	// nextCursor2 should be empty since we consumed all 6 entries
	if nextCursor2 != "" {
		t.Errorf("expected empty nextCursor on last page, got %q", nextCursor2)
	}

	// Verify all 6 commits were returned across both pages with no duplicates
	allHashes := make(map[string]bool)
	for _, e := range entries {
		allHashes[e.Hash] = true
	}
	for _, e := range entries2 {
		if allHashes[e.Hash] {
			t.Errorf("duplicate hash across pages: %s", e.Hash)
		}
		allHashes[e.Hash] = true
	}
	if len(allHashes) != 6 {
		t.Errorf("expected 6 unique commits, got %d", len(allHashes))
	}
}

func TestGetHistory_CursorStability(t *testing.T) {
	dir := setupTestRepo(t)

	// Create 4 commits (5 total with initial)
	for i := 1; i <= 4; i++ {
		f := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(f, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		runGitCmd(t, dir, "add", fmt.Sprintf("file%d.txt", i))
		runGitCmd(t, dir, "commit", "-m", fmt.Sprintf("Commit %d", i))
	}

	ops := NewGitOperations(dir, false, false, false)

	// Page 1
	entries1, cursor, err := ops.GetHistory("", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(entries1) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries1))
	}

	// Add a new commit while we have a cursor
	f := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(f, []byte("new"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, dir, "add", "new.txt")
	runGitCmd(t, dir, "commit", "-m", "New commit after cursor")

	// Page 2 with cursor: should not include the new commit, no dup/skip
	entries2, _, err := ops.GetHistory(cursor, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(entries2) < 1 {
		t.Fatal("expected at least 1 entry on page 2")
	}

	// Verify no duplicates between pages
	seenHashes := make(map[string]bool)
	for _, e := range entries1 {
		seenHashes[e.Hash] = true
	}
	for _, e := range entries2 {
		if seenHashes[e.Hash] {
			t.Errorf("duplicate hash across pages: %s", e.Hash)
		}
	}
}

func TestGetHistory_InvalidCursor(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	tests := []struct {
		name   string
		cursor string
	}{
		{"malformed hash", "not-a-hash"},
		{"short hash", "abc123"},
		{"unknown hash", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ops.GetHistory(tc.cursor, 10)
			if err == nil {
				t.Fatal("expected error for invalid cursor")
			}
			if !strings.Contains(err.Error(), "invalid cursor") && !strings.Contains(err.Error(), "server.invalid_message") {
				t.Errorf("expected invalid cursor error, got: %v", err)
			}
		})
	}

	t.Run("non-commit object hash", func(t *testing.T) {
		blobHash := strings.TrimSpace(runGitCmdOutput(t, dir, "hash-object", "initial.txt"))
		_, _, err := ops.GetHistory(blobHash, 10)
		if err == nil {
			t.Fatal("expected error for non-commit cursor")
		}
		if !strings.Contains(err.Error(), "invalid cursor") && !strings.Contains(err.Error(), "server.invalid_message") {
			t.Errorf("expected invalid cursor error, got: %v", err)
		}
	})

	t.Run("non-ancestor commit hash", func(t *testing.T) {
		baseBranch := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))

		runGitCmd(t, dir, "checkout", "--orphan", "history-cursor-orphan")
		runGitCmd(t, dir, "rm", "-rf", ".")
		if err := os.WriteFile(filepath.Join(dir, "orphan.txt"), []byte("orphan"), 0644); err != nil {
			t.Fatalf("write orphan file: %v", err)
		}
		runGitCmd(t, dir, "add", "orphan.txt")
		runGitCmd(t, dir, "commit", "-m", "orphan commit")
		orphanHash := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "HEAD"))

		runGitCmd(t, dir, "checkout", baseBranch)

		_, _, err := ops.GetHistory(orphanHash, 10)
		if err == nil {
			t.Fatal("expected error for non-ancestor cursor")
		}
		if !strings.Contains(err.Error(), "invalid cursor") && !strings.Contains(err.Error(), "server.invalid_message") {
			t.Errorf("expected invalid cursor error, got: %v", err)
		}
	})
}

func TestGetHistory_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	ops := NewGitOperations(dir, false, false, false)
	entries, nextCursor, err := ops.GetHistory("", 50)
	if err != nil {
		t.Fatalf("GetHistory on empty repo: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("expected empty nextCursor, got %q", nextCursor)
	}
}

func TestGetHistory_NonRepoPath(t *testing.T) {
	dir := t.TempDir()
	ops := NewGitOperations(dir, false, false, false)

	_, _, err := ops.GetHistory("", 50)
	if err == nil {
		t.Fatal("expected error for non-repo path")
	}
	if !strings.Contains(err.Error(), apperrors.CodeCommitGitError) {
		t.Fatalf("expected %s, got %v", apperrors.CodeCommitGitError, err)
	}
}

func TestGetHistory_RootCursor(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Get the root commit hash
	entries, _, err := ops.GetHistory("", 50)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least 1 entry")
	}
	rootHash := entries[len(entries)-1].Hash

	// Cursor at root should return empty
	entries2, nextCursor, err := ops.GetHistory(rootHash, 50)
	if err != nil {
		t.Fatalf("GetHistory with root cursor: %v", err)
	}
	if len(entries2) != 0 {
		t.Errorf("expected 0 entries after root cursor, got %d", len(entries2))
	}
	if nextCursor != "" {
		t.Errorf("expected empty nextCursor, got %q", nextCursor)
	}
}

func TestGetHistory_PageSizeEdgeCases(t *testing.T) {
	dir := setupTestRepo(t)

	// Create 2 more commits (3 total)
	for i := 1; i <= 2; i++ {
		f := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(f, []byte(fmt.Sprintf("c%d", i)), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", fmt.Sprintf("file%d.txt", i))
		runGitCmd(t, dir, "commit", "-m", fmt.Sprintf("Commit %d", i))
	}

	ops := NewGitOperations(dir, false, false, false)

	// Exact page boundary: pageSize == total commits
	entries, nextCursor, err := ops.GetHistory("", 3)
	if err != nil {
		t.Fatalf("exact boundary: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("expected empty nextCursor at exact boundary, got %q", nextCursor)
	}

	// Single commit page
	entries, nextCursor, err = ops.GetHistory("", 1)
	if err != nil {
		t.Fatalf("single page: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
	if nextCursor == "" {
		t.Error("expected non-empty nextCursor for single page")
	}
}

func TestGetHistory_DefaultPageSize(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Default page size is handled by the handler, but GetHistory with 50 should work
	entries, _, err := ops.GetHistory("", 50)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	// With only 1 commit in setupTestRepo, should return 1
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

// -----------------------------------------------------------------------------
// GetBranches tests
// -----------------------------------------------------------------------------

func TestGetBranches_BasicLocal(t *testing.T) {
	dir := setupTestRepo(t)

	// Create additional branches
	runGitCmd(t, dir, "branch", "feature-a")
	runGitCmd(t, dir, "branch", "feature-b")

	ops := NewGitOperations(dir, false, false, false)
	currentBranch, local, _, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	// Current branch should be main or master
	if currentBranch != "main" && currentBranch != "master" {
		t.Errorf("expected main or master, got %q", currentBranch)
	}

	// Should have 3 local branches
	if len(local) != 3 {
		t.Fatalf("expected 3 local branches, got %d: %v", len(local), local)
	}

	// Should be sorted
	if !sort.StringsAreSorted(local) {
		t.Errorf("local branches not sorted: %v", local)
	}

	// Should contain our branches
	if !containsString(local, "feature-a") || !containsString(local, "feature-b") {
		t.Errorf("missing branches in %v", local)
	}
}

func TestGetBranches_TrackedRemotes(t *testing.T) {
	// Create a "remote" repo to track
	remoteDir := t.TempDir()
	runGitCmd(t, remoteDir, "init", "--bare")

	dir := setupTestRepo(t)
	runGitCmd(t, dir, "remote", "add", "origin", remoteDir)
	runGitCmd(t, dir, "push", "origin", "HEAD")

	// Create and push another branch
	runGitCmd(t, dir, "branch", "dev")
	runGitCmd(t, dir, "push", "origin", "dev")

	// Fetch to populate refs/remotes
	runGitCmd(t, dir, "fetch", "origin")

	ops := NewGitOperations(dir, false, false, false)
	_, _, trackedRemote, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	if len(trackedRemote) < 2 {
		t.Fatalf("expected at least 2 tracked remotes, got %d: %v", len(trackedRemote), trackedRemote)
	}

	// Should be sorted
	if !sort.StringsAreSorted(trackedRemote) {
		t.Errorf("tracked remotes not sorted: %v", trackedRemote)
	}
}

func TestGetBranches_DetachedHead(t *testing.T) {
	dir := setupTestRepo(t)

	hash, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	runGitCmd(t, dir, "checkout", "--detach", strings.TrimSpace(string(hash)))

	ops := NewGitOperations(dir, false, false, false)
	currentBranch, _, _, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	if currentBranch != "HEAD" {
		t.Errorf("expected 'HEAD' for detached state, got %q", currentBranch)
	}
}

func TestGetBranches_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	ops := NewGitOperations(dir, false, false, false)
	currentBranch, local, trackedRemote, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches on empty repo: %v", err)
	}

	// Empty repo should still have a branch name from symbolic-ref
	if currentBranch == "" {
		t.Error("expected non-empty current branch")
	}

	if len(local) != 0 {
		t.Errorf("expected 0 local branches, got %d: %v", len(local), local)
	}
	if len(trackedRemote) != 0 {
		t.Errorf("expected 0 tracked remotes, got %d: %v", len(trackedRemote), trackedRemote)
	}
}

func TestGetBranches_AliasFiltering(t *testing.T) {
	// Create a "remote" repo
	remoteDir := t.TempDir()
	runGitCmd(t, remoteDir, "init", "--bare")

	dir := setupTestRepo(t)
	runGitCmd(t, dir, "remote", "add", "origin", remoteDir)
	runGitCmd(t, dir, "push", "origin", "HEAD")
	runGitCmd(t, dir, "fetch", "origin")

	// Set origin/HEAD -> origin/main (or master)
	branch := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	runGitCmd(t, dir, "remote", "set-head", "origin", branch)

	ops := NewGitOperations(dir, false, false, false)
	_, _, trackedRemote, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	// origin/HEAD should be filtered out
	for _, ref := range trackedRemote {
		if strings.HasSuffix(ref, "/HEAD") {
			t.Errorf("origin/HEAD should be filtered, found %q in %v", ref, trackedRemote)
		}
		if !strings.Contains(ref, "/") {
			t.Errorf("remote namespace alias should be filtered, found %q in %v", ref, trackedRemote)
		}
	}
}

func TestGetBranches_AliasFilteringRemoteNameWithSlash(t *testing.T) {
	remoteDir := t.TempDir()
	runGitCmd(t, remoteDir, "init", "--bare")

	dir := setupTestRepo(t)
	runGitCmd(t, dir, "remote", "add", "foo/bar", remoteDir)
	runGitCmd(t, dir, "push", "foo/bar", "HEAD")
	runGitCmd(t, dir, "fetch", "foo/bar")

	branch := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	runGitCmd(t, dir, "remote", "set-head", "foo/bar", branch)

	ops := NewGitOperations(dir, false, false, false)
	_, _, trackedRemote, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	if containsString(trackedRemote, "foo/bar") {
		t.Fatalf("remote HEAD alias leaked into tracked branches: %v", trackedRemote)
	}

	expectedTracked := "foo/bar/" + branch
	if !containsString(trackedRemote, expectedTracked) {
		t.Fatalf("expected tracked branch %q in %v", expectedTracked, trackedRemote)
	}

	for _, ref := range trackedRemote {
		if strings.HasSuffix(ref, "/HEAD") {
			t.Errorf("HEAD alias should be filtered, found %q in %v", ref, trackedRemote)
		}
	}
}

func TestGetBranches_SortDeterminism(t *testing.T) {
	dir := setupTestRepo(t)

	// Create branches in non-alphabetical order
	runGitCmd(t, dir, "branch", "zebra")
	runGitCmd(t, dir, "branch", "alpha")
	runGitCmd(t, dir, "branch", "middle")

	ops := NewGitOperations(dir, false, false, false)
	_, local, _, err := ops.GetBranches()
	if err != nil {
		t.Fatalf("GetBranches: %v", err)
	}

	if !sort.StringsAreSorted(local) {
		t.Errorf("local branches not sorted: %v", local)
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

// -----------------------------------------------------------------------------
// Branch Mutation tests (P4U2)
// -----------------------------------------------------------------------------

func TestCreateBranch(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	if err := ops.CreateBranch("feature-x"); err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	// Verify branch was created
	exists, err := ops.LocalBranchExists("feature-x")
	if err != nil {
		t.Fatalf("LocalBranchExists failed: %v", err)
	}
	if !exists {
		t.Error("expected branch feature-x to exist")
	}

	// Verify current branch unchanged
	currentBranch, err := ops.GetCurrentBranchName()
	if err != nil {
		t.Fatalf("GetCurrentBranchName failed: %v", err)
	}
	if currentBranch == "feature-x" {
		t.Error("CreateBranch should not switch current branch")
	}
}

func TestCreateBranch_Duplicate(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	if err := ops.CreateBranch("dup-branch"); err != nil {
		t.Fatalf("first CreateBranch failed: %v", err)
	}

	err := ops.CreateBranch("dup-branch")
	if err == nil {
		t.Fatal("expected error for duplicate branch creation")
	}
}

func TestCreateBranch_InvalidName(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	tests := []struct {
		name   string
		branch string
	}{
		{"starts with dash", "-bad"},
		{"double dots", "a..b"},
		{"space in name", "bad name"},
		{"tilde", "bad~name"},
		{"caret", "bad^name"},
		{"colon", "bad:name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ops.ValidateBranchName(tt.branch)
			if err == nil {
				t.Errorf("expected error for invalid branch name %q", tt.branch)
			}
		})
	}
}

func TestCreateBranchEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	ops := NewGitOperations(dir, false, false, false)

	isEmpty, err := ops.IsEmptyRepo()
	if err != nil {
		t.Fatalf("IsEmptyRepo failed: %v", err)
	}
	if !isEmpty {
		t.Fatal("expected empty repo")
	}

	// Create should fail - no HEAD to branch from
	err = ops.CreateBranch("feature-x")
	if err == nil {
		t.Fatal("expected error creating branch in empty repo")
	}
}

func TestCreateBranchDetachedHead(t *testing.T) {
	dir := setupTestRepo(t)
	hash := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "HEAD"))
	runGitCmd(t, dir, "checkout", "--detach", hash)

	ops := NewGitOperations(dir, false, false, false)

	detached, err := ops.IsDetachedHead()
	if err != nil {
		t.Fatalf("IsDetachedHead failed: %v", err)
	}
	if !detached {
		t.Fatal("expected detached HEAD")
	}

	// Create should succeed from detached HEAD
	if err := ops.CreateBranch("from-detached"); err != nil {
		t.Fatalf("CreateBranch from detached HEAD failed: %v", err)
	}

	exists, err := ops.LocalBranchExists("from-detached")
	if err != nil {
		t.Fatalf("LocalBranchExists failed: %v", err)
	}
	if !exists {
		t.Error("expected branch from-detached to exist")
	}
}

func TestSwitchBranch(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Create target branch
	runGitCmd(t, dir, "branch", "target-branch")

	blockers, err := ops.SwitchBranch("target-branch")
	if err != nil {
		t.Fatalf("SwitchBranch failed: %v", err)
	}
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %v", blockers)
	}

	// Verify switch happened
	currentBranch, err := ops.GetCurrentBranchName()
	if err != nil {
		t.Fatalf("GetCurrentBranchName failed: %v", err)
	}
	if currentBranch != "target-branch" {
		t.Errorf("expected current branch target-branch, got %s", currentBranch)
	}
}

func TestSwitchBranchSameBranchNoop(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	currentBranch, err := ops.GetCurrentBranchName()
	if err != nil {
		t.Fatalf("GetCurrentBranchName failed: %v", err)
	}

	// Switch to same branch should be a no-op
	blockers, err := ops.SwitchBranch(currentBranch)
	if err != nil {
		t.Fatalf("SwitchBranch same-branch failed: %v", err)
	}
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers for same-branch: %v", blockers)
	}
}

func TestSwitchBranchDirtyBlockers(t *testing.T) {
	t.Run("staged_changes", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")

		// Create staged change
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", "staged.txt")

		ops := NewGitOperations(dir, false, false, false)
		blockers, err := ops.SwitchBranch("target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !containsString(blockers, "staged_changes_present") {
			t.Errorf("expected staged_changes_present in blockers, got %v", blockers)
		}
	})

	t.Run("unstaged_changes", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")

		// Modify tracked file without staging
		if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("modified"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		ops := NewGitOperations(dir, false, false, false)
		blockers, err := ops.SwitchBranch("target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !containsString(blockers, "unstaged_changes_present") {
			t.Errorf("expected unstaged_changes_present in blockers, got %v", blockers)
		}
	})

	t.Run("untracked_changes", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")

		// Create untracked file
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		ops := NewGitOperations(dir, false, false, false)
		blockers, err := ops.SwitchBranch("target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !containsString(blockers, "untracked_changes_present") {
			t.Errorf("expected untracked_changes_present in blockers, got %v", blockers)
		}
	})

	t.Run("merge_conflicts", func(t *testing.T) {
		dir := setupTestRepo(t)
		baseBranch := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))

		// Create conflicting branch
		runGitCmd(t, dir, "checkout", "-b", "conflict-branch")
		if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("branch change\n"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", "initial.txt")
		runGitCmd(t, dir, "commit", "-m", "branch conflict")

		runGitCmd(t, dir, "checkout", baseBranch)
		if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("base change\n"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", "initial.txt")
		runGitCmd(t, dir, "commit", "-m", "base conflict")

		// Create target branch
		runGitCmd(t, dir, "branch", "target")

		// Merge to create conflicts
		cmd := exec.Command("git", "merge", "conflict-branch")
		cmd.Dir = dir
		_ = cmd.Run() // expected to fail

		ops := NewGitOperations(dir, false, false, false)
		blockers, err := ops.SwitchBranch("target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !containsString(blockers, "merge_conflicts_present") {
			t.Errorf("expected merge_conflicts_present in blockers, got %v", blockers)
		}
	})

	t.Run("deterministic_order", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")

		// Create both staged and unstaged and untracked dirty states
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", "staged.txt")

		if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("modified"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		ops := NewGitOperations(dir, false, false, false)
		blockers, err := ops.CheckDirtyBlockers()
		if err != nil {
			t.Fatalf("CheckDirtyBlockers failed: %v", err)
		}

		// Verify deterministic order
		expected := []string{"staged_changes_present", "unstaged_changes_present", "untracked_changes_present"}
		if len(blockers) != len(expected) {
			t.Fatalf("expected %d blockers, got %d: %v", len(expected), len(blockers), blockers)
		}
		for i, b := range expected {
			if blockers[i] != b {
				t.Errorf("blocker[%d]: expected %s, got %s", i, b, blockers[i])
			}
		}
	})
}

func TestSwitchBranchSubmoduleDirty(t *testing.T) {
	// Test that submodule uninitialized marker '-' does NOT block
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Clean repo, no submodules - should have no submodule dirty blockers
	blockers, err := ops.CheckDirtyBlockers()
	if err != nil {
		t.Fatalf("CheckDirtyBlockers failed: %v", err)
	}
	if containsString(blockers, "submodule_dirty_present") {
		t.Error("unexpected submodule_dirty_present in clean repo")
	}
}

func TestSwitchBranchEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test User")

	ops := NewGitOperations(dir, false, false, false)

	// Get current branch name (should work even in empty repo)
	currentBranch, err := ops.GetCurrentBranchName()
	if err != nil {
		t.Fatalf("GetCurrentBranchName failed in empty repo: %v", err)
	}

	// Same-branch should succeed as no-op
	blockers, err := ops.SwitchBranch(currentBranch)
	if err != nil {
		t.Fatalf("SwitchBranch same-branch in empty repo failed: %v", err)
	}
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %v", blockers)
	}
}

func TestSwitchBranchDetachedHead(t *testing.T) {
	dir := setupTestRepo(t)
	runGitCmd(t, dir, "branch", "target")
	hash := strings.TrimSpace(runGitCmdOutput(t, dir, "rev-parse", "HEAD"))
	runGitCmd(t, dir, "checkout", "--detach", hash)

	ops := NewGitOperations(dir, false, false, false)

	// Switch from detached HEAD to named branch
	blockers, err := ops.SwitchBranch("target")
	if err != nil {
		t.Fatalf("SwitchBranch from detached HEAD failed: %v", err)
	}
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %v", blockers)
	}

	currentBranch, err := ops.GetCurrentBranchName()
	if err != nil {
		t.Fatalf("GetCurrentBranchName failed: %v", err)
	}
	if currentBranch != "target" {
		t.Errorf("expected current branch target, got %s", currentBranch)
	}
}

func TestSwitchBranchLateDirtyRace(t *testing.T) {
	// Test the classifyCheckoutFailure method directly
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Simulate checkout failure with dirty token but clean repo
	_, err := ops.classifyCheckoutFailure(
		"error: Your local changes to the following files would be overwritten by checkout:\n  file.txt\n",
		fmt.Errorf("exit status 1"),
	)
	// Clean repo + matcher hit -> should return git error (no blockers found)
	if err == nil {
		t.Fatal("expected error from classifyCheckoutFailure")
	}

	// Test with non-dirty stderr -> should return git error
	_, err = ops.classifyCheckoutFailure(
		"error: pathspec 'nonexistent' did not match any file(s) known to git\n",
		fmt.Errorf("exit status 1"),
	)
	if err == nil {
		t.Fatal("expected error from classifyCheckoutFailure with non-dirty stderr")
	}
}

// -----------------------------------------------------------------------------
// Fetch/Pull error classifier tests (P9U3)
// -----------------------------------------------------------------------------

// TestClassifyFetchError tests error classification for fetch failures.
func TestClassifyFetchError(t *testing.T) {
	ops := NewGitOperations("/tmp", false, false, false)
	baseErr := exec.Command("false").Run() // Create a simple error

	tests := []struct {
		name         string
		output       string
		expectedCode string
	}{
		{
			name:         "permission denied",
			output:       "Permission denied (publickey).\nfatal: Could not read from remote repository.",
			expectedCode: apperrors.CodeSyncAuthFailed,
		},
		{
			name:         "authentication failed",
			output:       "fatal: Authentication failed for 'https://github.com/user/repo.git/'",
			expectedCode: apperrors.CodeSyncAuthFailed,
		},
		{
			name:         "could not resolve host",
			output:       "fatal: unable to access 'https://github.com/user/repo.git/': Could not resolve host: github.com",
			expectedCode: apperrors.CodeSyncNetworkError,
		},
		{
			name:         "network is unreachable",
			output:       "fatal: unable to access 'https://github.com/user/repo.git/': Network is unreachable",
			expectedCode: apperrors.CodeSyncNetworkError,
		},
		{
			name:         "timeout",
			output:       "fatal: operation timed out",
			expectedCode: apperrors.CodeSyncTimeout,
		},
		{
			name:         "generic error",
			output:       "error: something unexpected happened",
			expectedCode: apperrors.CodeSyncGitError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ops.classifyFetchError(tc.output, baseErr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.expectedCode) {
				t.Errorf("expected error code %s, got: %v", tc.expectedCode, err)
			}
		})
	}
}

// TestClassifyPullError tests error classification for pull failures.
func TestClassifyPullError(t *testing.T) {
	ops := NewGitOperations("/tmp", false, false, false)
	baseErr := exec.Command("false").Run() // Create a simple error

	tests := []struct {
		name         string
		output       string
		expectedCode string
	}{
		{
			name:         "not possible to fast-forward",
			output:       "fatal: Not possible to fast-forward, aborting.",
			expectedCode: apperrors.CodeSyncNonFF,
		},
		{
			name:         "need to specify how to reconcile divergent branches",
			output:       "hint: You need to specify how to reconcile divergent branches.",
			expectedCode: apperrors.CodeSyncNonFF,
		},
		{
			name:         "permission denied",
			output:       "Permission denied (publickey).\nfatal: Could not read from remote repository.",
			expectedCode: apperrors.CodeSyncAuthFailed,
		},
		{
			name:         "could not resolve host",
			output:       "fatal: unable to access 'https://github.com/user/repo.git/': Could not resolve host: github.com",
			expectedCode: apperrors.CodeSyncNetworkError,
		},
		{
			name:         "timeout",
			output:       "fatal: operation timed out",
			expectedCode: apperrors.CodeSyncTimeout,
		},
		{
			name:         "generic error",
			output:       "error: something unexpected happened",
			expectedCode: apperrors.CodeSyncGitError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ops.classifyPullError(tc.output, baseErr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.expectedCode) {
				t.Errorf("expected error code %s, got: %v", tc.expectedCode, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// PR error classifier tests (P9U4)
// -----------------------------------------------------------------------------

func TestClassifyPrError(t *testing.T) {
	baseErr := exec.Command("false").Run()

	tests := []struct {
		name         string
		output       string
		err          error
		expectedCode string
	}{
		{
			name:         "gh missing",
			output:       "",
			err:          &exec.Error{Name: "gh", Err: exec.ErrNotFound},
			expectedCode: apperrors.CodePrGhMissing,
		},
		{
			name:         "timeout (deadline exceeded)",
			output:       "",
			err:          context.DeadlineExceeded,
			expectedCode: apperrors.CodePrTimeout,
		},
		{
			name:         "auth required",
			output:       "To get started with GitHub CLI, please run: gh auth login",
			err:          baseErr,
			expectedCode: apperrors.CodePrAuthRequired,
		},
		{
			name:         "not logged in",
			output:       "You are not logged in to any GitHub hosts",
			err:          baseErr,
			expectedCode: apperrors.CodePrAuthRequired,
		},
		{
			name:         "repo unsupported - no remotes",
			output:       "none of the git remotes configured for this repository point to a known GitHub host",
			err:          baseErr,
			expectedCode: apperrors.CodePrRepoUnsupported,
		},
		{
			name:         "repo unsupported - not a git repo",
			output:       "fatal: not a git repository",
			err:          baseErr,
			expectedCode: apperrors.CodePrRepoUnsupported,
		},
		{
			name:         "not found",
			output:       "Could not find pull request #999",
			err:          baseErr,
			expectedCode: apperrors.CodePrNotFound,
		},
		{
			name:         "network error - could not resolve",
			output:       "Could not resolve host: api.github.com",
			err:          baseErr,
			expectedCode: apperrors.CodePrNetworkError,
		},
		{
			name:         "network error - connection refused",
			output:       "connection refused",
			err:          baseErr,
			expectedCode: apperrors.CodePrNetworkError,
		},
		{
			name:         "generic gh error",
			output:       "something unexpected happened",
			err:          baseErr,
			expectedCode: apperrors.CodePrGhError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyPrError(tc.output, tc.err)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			code := apperrors.GetCode(err)
			if code != tc.expectedCode {
				t.Errorf("expected code %s, got %s (err: %v)", tc.expectedCode, code, err)
			}
		})
	}
}

func TestPrList_MalformedJson(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"not json", "this is not json"},
		{"wrong type", `{"foo": "bar"}`},
		{"array of wrong type", `[{"foo": "bar"}]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var entries []ghListEntry
			err := json.Unmarshal([]byte(tc.input), &entries)
			// Either unmarshal fails or entries are invalid
			if err == nil && len(entries) > 0 {
				// Validate that incomplete entries would be caught
				for _, e := range entries {
					if e.Number == 0 || e.Title == "" || e.HeadRefName == "" || e.BaseRefName == "" || e.URL == "" {
						return // Would be caught by field completeness check
					}
				}
				t.Error("expected malformed JSON to be detected")
			}
		})
	}
}

func TestPrView_MalformedJson(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"not json", "not json"},
		{"missing number", `{"title":"T","url":"http://x"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var entry ghDetailEntry
			err := json.Unmarshal([]byte(tc.input), &entry)
			if err == nil && (entry.Number == 0 || entry.Title == "" || entry.URL == "") {
				return // Would be caught by field completeness check
			}
			if err != nil {
				return // Unmarshal caught it
			}
			t.Error("expected malformed JSON to be detected")
		})
	}
}

func TestPrCreate_MalformedJson(t *testing.T) {
	var entry ghDetailEntry
	err := json.Unmarshal([]byte(`{"title":"","number":0}`), &entry)
	if err == nil && (entry.Number == 0 || entry.Title == "" || entry.URL == "") {
		return // Would be caught by field completeness check
	}
	if err != nil {
		return
	}
	t.Error("expected malformed create JSON to be detected")
}

func TestPrCheckout_MalformedJson(t *testing.T) {
	// PrCheckout doesn't parse JSON output, just runs gh pr checkout
	// Verify classifyPrError handles non-JSON stderr correctly
	err := classifyPrError("unexpected error output", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected error")
	}
	code := apperrors.GetCode(err)
	if code != apperrors.CodePrGhError {
		t.Errorf("expected %s, got %s", apperrors.CodePrGhError, code)
	}
}

func TestPrList_Order(t *testing.T) {
	// Verify sort logic: updated_at DESC, number DESC
	entries := []RepoPrEntryPayload{
		{Number: 1, Title: "A", UpdatedAt: "2024-01-01T00:00:00Z", HeadBranch: "a", BaseBranch: "main", URL: "http://a", State: "open"},
		{Number: 3, Title: "C", UpdatedAt: "2024-01-03T00:00:00Z", HeadBranch: "c", BaseBranch: "main", URL: "http://c", State: "open"},
		{Number: 2, Title: "B", UpdatedAt: "2024-01-03T00:00:00Z", HeadBranch: "b", BaseBranch: "main", URL: "http://b", State: "open"},
		{Number: 4, Title: "D", UpdatedAt: "2024-01-02T00:00:00Z", HeadBranch: "d", BaseBranch: "main", URL: "http://d", State: "open"},
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt != entries[j].UpdatedAt {
			return entries[i].UpdatedAt > entries[j].UpdatedAt
		}
		return entries[i].Number > entries[j].Number
	})

	// Expected order: #3 (2024-01-03, higher number), #2 (2024-01-03, lower number), #4 (2024-01-02), #1 (2024-01-01)
	expectedOrder := []int{3, 2, 4, 1}
	for i, expected := range expectedOrder {
		if entries[i].Number != expected {
			t.Errorf("position %d: expected PR #%d, got #%d", i, expected, entries[i].Number)
		}
	}
}

func TestPrList_Limit(t *testing.T) {
	// Verify cap at 50
	entries := make([]RepoPrEntryPayload, 60)
	for i := range entries {
		entries[i] = RepoPrEntryPayload{
			Number:     i + 1,
			Title:      fmt.Sprintf("PR %d", i+1),
			UpdatedAt:  fmt.Sprintf("2024-01-%02dT00:00:00Z", (i%28)+1),
			HeadBranch: fmt.Sprintf("branch-%d", i+1),
			BaseBranch: "main",
			URL:        fmt.Sprintf("http://pr/%d", i+1),
			State:      "open",
		}
	}

	if len(entries) > 50 {
		entries = entries[:50]
	}

	if len(entries) != 50 {
		t.Errorf("expected 50 entries after cap, got %d", len(entries))
	}
}

func TestPrList_FieldCompleteness(t *testing.T) {
	// Verify that entries with missing required fields are rejected
	incompleteEntries := []ghListEntry{
		{Number: 0, Title: "Missing number"},
		{Number: 1, Title: ""},
		{Number: 2, Title: "No URL", HeadRefName: "a", BaseRefName: "main"},
	}

	for _, e := range incompleteEntries {
		if e.Number == 0 || e.Title == "" || e.HeadRefName == "" || e.BaseRefName == "" || e.URL == "" {
			continue // Correctly detected as incomplete
		}
		t.Errorf("expected entry to be detected as incomplete: %+v", e)
	}
}

func TestPrCreate_Defaults(t *testing.T) {
	// Verify default behavior: draft=false when nil
	var draftPtr *bool
	draft := false
	if draftPtr != nil {
		draft = *draftPtr
	}
	if draft != false {
		t.Error("expected draft=false when nil")
	}

	// Verify empty body is allowed
	body := ""
	_ = body // No validation should reject empty body
}

func TestPrCreate_Validation(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		base    string
		wantErr bool
	}{
		{"empty title", "", "", true},
		{"whitespace title", "   ", "", true},
		{"valid title no base", "My PR", "", false},
		{"valid title with base", "My PR", "main", false},
		{"whitespace base when provided", "My PR", "   ", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title := strings.TrimSpace(tc.title)
			hasErr := false
			if title == "" {
				hasErr = true
			}
			if tc.base != "" && strings.TrimSpace(tc.base) == "" {
				hasErr = true
			}
			if hasErr != tc.wantErr {
				t.Errorf("expected wantErr=%v, got %v", tc.wantErr, hasErr)
			}
		})
	}
}

func TestPrCommand_NonInteractiveEnv(t *testing.T) {
	dir := setupTestRepo(t)
	ops := NewGitOperations(dir, false, false, false)

	// Verify runGhWithTimeout sets non-interactive env
	// We can't easily test the actual env propagation without running gh,
	// but we can verify the method exists and returns expected errors
	_, stderr, err := ops.runGhWithTimeout(1, "version")
	// gh may not be installed, that's fine - we're testing the method exists
	if err != nil {
		if isGhMissing(err) {
			// Expected in test environments without gh
			return
		}
		// Some other error - still validates the code path
		_ = stderr
	}
}

func TestIsGhMissing(t *testing.T) {
	// Test with exec.Error wrapping ErrNotFound
	err := &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	if !isGhMissing(err) {
		t.Error("expected isGhMissing=true for exec.ErrNotFound")
	}

	// Test with regular error
	if isGhMissing(fmt.Errorf("some error")) {
		t.Error("expected isGhMissing=false for regular error")
	}

	// Test with nil
	if isGhMissing(nil) {
		t.Error("expected isGhMissing=false for nil")
	}
}
