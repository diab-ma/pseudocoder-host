package actions

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
)

// setupGitRepo creates a temporary git repo with an initial commit.
// Returns the repo path and a cleanup function.
func setupGitRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	// Initialize git repo
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")

	// Create and commit initial file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	runGit(t, dir, "add", "test.txt")
	runGit(t, dir, "commit", "-m", "initial")

	return dir
}

// runGit runs a git command in the specified directory.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// getGitStatus returns the git status output for a file.
func getGitStatus(t *testing.T, dir, file string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain", file)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v\n%s", err, out)
	}
	return string(out)
}

// getFileContent reads a file's content.
func getFileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	return string(data)
}

func TestProcessDecisionAccept(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify the file to create uncommitted changes
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nadded line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Verify file is modified (unstaged)
	status := getGitStatus(t, repoDir, "test.txt")
	if status != " M test.txt\n" {
		t.Fatalf("expected modified status, got: %q", status)
	}

	// Create a card for the change
	card := &storage.ReviewCard{
		ID:        "card-123",
		File:      "test.txt",
		Diff:      "@@ -1 +1,2 @@\n initial content\n+added line\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Process the accept decision
	processor := NewProcessor(store, repoDir)
	if err := processor.ProcessDecision("card-123", "accept", "looks good"); err != nil {
		t.Fatalf("process decision failed: %v", err)
	}

	// Verify file is now staged
	status = getGitStatus(t, repoDir, "test.txt")
	if status != "M  test.txt\n" {
		t.Fatalf("expected staged status, got: %q", status)
	}

	// Verify card is deleted after successful decision
	card, err = store.GetCard("card-123")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card != nil {
		t.Error("expected card to be deleted after successful decision")
	}
}

func TestProcessDecisionReject(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify the file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("modified content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Verify file has modified content
	content := getFileContent(t, testFile)
	if content != "modified content\n" {
		t.Fatalf("unexpected initial content: %q", content)
	}

	// Create a card for the change
	card := &storage.ReviewCard{
		ID:        "card-456",
		File:      "test.txt",
		Diff:      "@@ -1 +1 @@\n-initial content\n+modified content\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Process the reject decision
	processor := NewProcessor(store, repoDir)
	if err := processor.ProcessDecision("card-456", "reject", "not needed"); err != nil {
		t.Fatalf("process decision failed: %v", err)
	}

	// Verify file is restored to original content
	content = getFileContent(t, testFile)
	if content != "initial content\n" {
		t.Fatalf("expected original content, got: %q", content)
	}

	// Verify card is deleted after successful decision
	card, err = store.GetCard("card-456")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card != nil {
		t.Error("expected card to be deleted after successful decision")
	}
}

func TestProcessDecisionValidationFailureKeepsPending(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify the file to create changes
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\ninvalid patch\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card with an invalid diff (missing @@ header)
	card := &storage.ReviewCard{
		ID:        "card-invalid",
		File:      "test.txt",
		Diff:      "+invalid patch line",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-invalid", "accept", "")
	if err == nil {
		t.Fatal("expected validation error for invalid diff")
	}
	if !apperrors.IsCode(err, apperrors.CodeConflictDetected) &&
		!apperrors.IsCode(err, apperrors.CodeValidationFailed) {
		t.Errorf("expected validation/conflict error, got: %v", err)
	}

	// Verify no staged changes were created
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = repoDir
	out, _ := cmd.CombinedOutput()
	if string(out) != "" {
		t.Errorf("expected no staged changes, got: %s", string(out))
	}

	// Card should remain pending
	card, err = store.GetCard("card-invalid")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card == nil || card.Status != storage.CardPending {
		t.Errorf("expected card to remain pending, got: %+v", card)
	}
}

func TestProcessDecisionCardNotFound(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("nonexistent", "accept", "")

	if err == nil {
		t.Fatal("expected error for nonexistent card")
	}
	// Now returns a CodedError with storage.not_found code
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected error code %s, got: %v", apperrors.CodeStorageNotFound, err)
	}
}

func TestProcessDecisionInvalidAction(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-123", "maybe", "")

	if err == nil {
		t.Fatal("expected error for invalid action")
	}
	// Now returns a CodedError with action.invalid code
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected error code %s, got: %v", apperrors.CodeActionInvalid, err)
	}
}

func TestProcessDecisionPathTraversal(t *testing.T) {
	testCases := []struct {
		name string
		path string
	}{
		{"parent directory", "../outside.txt"},
		{"absolute path", "/etc/passwd"},
		{"nested escape", "foo/../../../outside.txt"},
		{"hidden parent", "a/b/../../.."},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := setupGitRepo(t)
			store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
			if err != nil {
				t.Fatalf("create store failed: %v", err)
			}
			defer store.Close()

			// Create a card with a path traversal attempt
			card := &storage.ReviewCard{
				ID:        "card-traversal",
				File:      tc.path,
				Diff:      "+malicious content",
				Status:    storage.CardPending,
				CreatedAt: time.Now(),
			}
			if err := store.SaveCard(card); err != nil {
				t.Fatalf("save card failed: %v", err)
			}

			processor := NewProcessor(store, repoDir)
			err = processor.ProcessDecision("card-traversal", "accept", "")

			if err == nil {
				t.Fatal("expected error for path traversal attempt")
			}
			if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
				t.Errorf("expected error code %s, got: %v", apperrors.CodeActionInvalid, err)
			}
		})
	}
}

func TestProcessDecisionAlreadyDecided(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card that's already accepted
	now := time.Now()
	card := &storage.ReviewCard{
		ID:        "card-789",
		File:      "test.txt",
		Diff:      "+something",
		Status:    storage.CardAccepted, // Already decided
		CreatedAt: now,
		DecidedAt: &now,
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-789", "reject", "")

	if err == nil {
		t.Fatal("expected error for already decided card")
	}
	// Now returns a CodedError with storage.already_decided code
	if !apperrors.IsCode(err, apperrors.CodeStorageAlreadyDecided) {
		t.Errorf("expected error code %s, got: %v", apperrors.CodeStorageAlreadyDecided, err)
	}
}

func TestProcessDecisionNewFile(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a new untracked file
	newFile := filepath.Join(repoDir, "new.txt")
	if err := os.WriteFile(newFile, []byte("new file content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Verify file is untracked
	status := getGitStatus(t, repoDir, "new.txt")
	if status != "?? new.txt\n" {
		t.Fatalf("expected untracked status, got: %q", status)
	}

	// Create a card for the new file
	card := &storage.ReviewCard{
		ID:        "card-new",
		File:      "new.txt",
		Diff:      "@@ -0,0 +1 @@\n+new file content\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Accept the new file
	processor := NewProcessor(store, repoDir)
	if err := processor.ProcessDecision("card-new", "accept", ""); err != nil {
		t.Fatalf("process decision failed: %v", err)
	}

	// Verify file is now staged
	status = getGitStatus(t, repoDir, "new.txt")
	if status != "A  new.txt\n" {
		t.Fatalf("expected staged (added) status, got: %q", status)
	}
}

func TestProcessDecisionRejectNewFile(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a new untracked file
	newFile := filepath.Join(repoDir, "new.txt")
	if err := os.WriteFile(newFile, []byte("new file content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card for the new file
	card := &storage.ReviewCard{
		ID:        "card-new-reject",
		File:      "new.txt",
		Diff:      "@@ -0,0 +1 @@\n+new file content\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Reject the new file - should fail with action.untracked_file error
	// BEFORE recording the decision (so user can retry with delete).
	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-new-reject", "reject", "")

	// Should get untracked file error
	if err == nil {
		t.Fatal("expected error for rejecting untracked file")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionUntrackedFile) {
		t.Errorf("expected action.untracked_file error, got: %v", err)
	}

	// Card should remain pending (not rejected) so user can retry with delete
	card, _ = store.GetCard("card-new-reject")
	if card.Status != storage.CardPending {
		t.Errorf("expected card to remain pending for retry, got: %s", card.Status)
	}
}

func TestRunDecisionTransactionVerificationFailureRollsBack(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	card := &storage.ReviewCard{
		ID:        "card-verify-fail",
		File:      "test.txt",
		Diff:      "@@ -1 +1 @@\n-initial content\n+changed\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	processor := NewProcessor(store, repoDir)

	applyCalled := false
	rollbackCalled := false

	apply := func() error {
		applyCalled = true
		return nil
	}
	verify := func() error {
		return fmt.Errorf("verification failed")
	}
	rollback := func() error {
		rollbackCalled = true
		return nil
	}

	err = processor.runDecisionTransaction(card, "accept", "", apply, verify, rollback)
	if err == nil {
		t.Fatal("expected verification error")
	}
	if !apperrors.IsCode(err, apperrors.CodeVerificationFailed) {
		t.Errorf("expected verification.failed error, got: %v", err)
	}
	if !applyCalled {
		t.Error("expected apply to be called")
	}
	if !rollbackCalled {
		t.Error("expected rollback to be called")
	}

	// Card should remain pending after verification failure.
	updated, err := store.GetCard("card-verify-fail")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if updated == nil || updated.Status != storage.CardPending {
		t.Errorf("expected card to remain pending, got: %+v", updated)
	}
}

// -----------------------------------------------------------------------------
// Binary File ProcessDecision Tests (Unit 5.4)
// -----------------------------------------------------------------------------

func TestProcessDecisionBinaryFileAccept(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a binary file (PNG header bytes)
	binaryFile := filepath.Join(repoDir, "image.png")
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if err := os.WriteFile(binaryFile, pngHeader, 0644); err != nil {
		t.Fatalf("write binary file failed: %v", err)
	}

	// Create a card for the binary file
	card := &storage.ReviewCard{
		ID:        "card-binary-accept",
		File:      "image.png",
		Diff:      diff.BinaryDiffPlaceholder,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Accept the binary file
	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-binary-accept", "accept", "")
	if err != nil {
		t.Fatalf("ProcessDecision failed: %v", err)
	}

	// Verify the card is deleted after being decided
	card, _ = store.GetCard("card-binary-accept")
	if card != nil {
		t.Error("expected card to be deleted after decision")
	}

	// Verify the file is staged
	cmd := exec.Command("git", "status", "--porcelain", "image.png")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	status := strings.TrimSpace(string(output))
	if !strings.HasPrefix(status, "A") {
		t.Errorf("expected staged (added) status, got: %q", status)
	}
}

func TestProcessDecisionBinaryFileReject(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create and commit a binary file first
	binaryFile := filepath.Join(repoDir, "data.bin")
	originalContent := []byte{0x00, 0x01, 0x02, 0x03}
	if err := os.WriteFile(binaryFile, originalContent, 0644); err != nil {
		t.Fatalf("write binary file failed: %v", err)
	}

	// Stage and commit the binary file
	cmd := exec.Command("git", "add", "data.bin")
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git add failed: %v", err)
	}
	cmd = exec.Command("git", "commit", "-m", "add binary file")
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git commit failed: %v", err)
	}

	// Modify the binary file (simulate unwanted change)
	modifiedContent := []byte{0xFF, 0xFE, 0xFD, 0xFC}
	if err := os.WriteFile(binaryFile, modifiedContent, 0644); err != nil {
		t.Fatalf("write modified binary file failed: %v", err)
	}

	// Create a card for the modified binary file
	card := &storage.ReviewCard{
		ID:        "card-binary-reject",
		File:      "data.bin",
		Diff:      diff.BinaryDiffPlaceholder,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Reject the binary file (should restore to committed version)
	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-binary-reject", "reject", "")
	if err != nil {
		t.Fatalf("ProcessDecision failed: %v", err)
	}

	// Verify the card is deleted after being decided
	card, _ = store.GetCard("card-binary-reject")
	if card != nil {
		t.Error("expected card to be deleted after decision")
	}

	// Verify the file content was restored to original
	restoredContent, err := os.ReadFile(binaryFile)
	if err != nil {
		t.Fatalf("read restored file failed: %v", err)
	}
	if !bytes.Equal(restoredContent, originalContent) {
		t.Errorf("expected original content %v, got: %v", originalContent, restoredContent)
	}
}

// -----------------------------------------------------------------------------
// Untracked File Tests (Unit 5.2)
// -----------------------------------------------------------------------------

func TestIsUntrackedFileTrue(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a new untracked file
	newFile := filepath.Join(repoDir, "untracked.txt")
	if err := os.WriteFile(newFile, []byte("untracked content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	processor := NewProcessor(store, repoDir)

	// Check if the file is untracked
	untracked, err := processor.isUntrackedFile("untracked.txt")
	if err != nil {
		t.Fatalf("isUntrackedFile failed: %v", err)
	}
	if !untracked {
		t.Error("expected file to be untracked")
	}
}

func TestIsUntrackedFileFalse(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, repoDir)

	// test.txt was committed in setupGitRepo - it should be tracked
	untracked, err := processor.isUntrackedFile("test.txt")
	if err != nil {
		t.Fatalf("isUntrackedFile failed: %v", err)
	}
	if untracked {
		t.Error("expected committed file to be tracked, not untracked")
	}
}

func TestIsUntrackedFileStagedNewFile(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create and stage a new file (but don't commit)
	newFile := filepath.Join(repoDir, "staged-new.txt")
	if err := os.WriteFile(newFile, []byte("staged new content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	runGit(t, repoDir, "add", "staged-new.txt")

	processor := NewProcessor(store, repoDir)

	// A staged (but not committed) file is NOT in HEAD, so it's "new"
	// from git restore's perspective - cannot be properly restored
	isNew, err := processor.isUntrackedFile("staged-new.txt")
	if err != nil {
		t.Fatalf("isUntrackedFile failed: %v", err)
	}
	if !isNew {
		t.Error("expected staged-but-not-committed file to be considered 'new'")
	}
}

func TestDeleteUntrackedFileSuccess(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a new untracked file
	newFile := filepath.Join(repoDir, "to-delete.txt")
	if err := os.WriteFile(newFile, []byte("delete me\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card for the file
	card := &storage.ReviewCard{
		ID:        "card-delete",
		File:      "to-delete.txt",
		Diff:      "+delete me",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Delete the untracked file
	processor := NewProcessor(store, repoDir)
	if err := processor.DeleteUntrackedFile("card-delete"); err != nil {
		t.Fatalf("DeleteUntrackedFile failed: %v", err)
	}

	// Verify file is deleted
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}

	// Verify card is deleted
	card, _ = store.GetCard("card-delete")
	if card != nil {
		t.Error("expected card to be deleted after file deletion")
	}
}

func TestDeleteUntrackedFileTrackedFails(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card for the tracked file (test.txt was committed in setupGitRepo)
	card := &storage.ReviewCard{
		ID:        "card-tracked",
		File:      "test.txt",
		Diff:      "+some change",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Try to delete the tracked file - should fail
	processor := NewProcessor(store, repoDir)
	err = processor.DeleteUntrackedFile("card-tracked")

	if err == nil {
		t.Fatal("expected error when trying to delete tracked file")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionGitFailed) {
		t.Errorf("expected action.git_failed error, got: %v", err)
	}

	// Verify file still exists
	testFile := filepath.Join(repoDir, "test.txt")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Error("tracked file should NOT be deleted")
	}
}

func TestDeleteUntrackedFileCardNotFound(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, repoDir)
	err = processor.DeleteUntrackedFile("nonexistent")

	if err == nil {
		t.Fatal("expected error for nonexistent card")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found error, got: %v", err)
	}
}

func TestDeleteUntrackedFilePathTraversal(t *testing.T) {
	testCases := []struct {
		name string
		path string
	}{
		{"parent directory", "../outside.txt"},
		{"absolute path", "/etc/passwd"},
		{"nested escape", "foo/../../../outside.txt"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := setupGitRepo(t)
			store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
			if err != nil {
				t.Fatalf("create store failed: %v", err)
			}
			defer store.Close()

			// Create a card with a path traversal attempt
			card := &storage.ReviewCard{
				ID:        "card-traversal",
				File:      tc.path,
				Diff:      "+malicious content",
				Status:    storage.CardPending,
				CreatedAt: time.Now(),
			}
			if err := store.SaveCard(card); err != nil {
				t.Fatalf("save card failed: %v", err)
			}

			processor := NewProcessor(store, repoDir)
			err = processor.DeleteUntrackedFile("card-traversal")

			if err == nil {
				t.Fatal("expected error for path traversal attempt")
			}
			if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
				t.Errorf("expected error code %s, got: %v", apperrors.CodeActionInvalid, err)
			}
		})
	}
}

func TestDeleteUntrackedFileAlreadyDeleted(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card for a file that doesn't exist on disk
	card := &storage.ReviewCard{
		ID:        "card-missing",
		File:      "already-gone.txt",
		Diff:      "+content",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Delete should succeed even if file is already gone
	// (it's untracked since it doesn't exist in git index either)
	processor := NewProcessor(store, repoDir)
	if err := processor.DeleteUntrackedFile("card-missing"); err != nil {
		t.Fatalf("DeleteUntrackedFile should succeed for already-deleted file: %v", err)
	}

	// Card should be deleted
	card, _ = store.GetCard("card-missing")
	if card != nil {
		t.Error("expected card to be deleted")
	}
}

func TestProcessDecisionStorageGetCardError(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}

	// Close the store to cause GetCard to fail with a database error
	store.Close()

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-123", "accept", "")

	if err == nil {
		t.Fatal("expected error when store is closed")
	}

	// Should return error.internal for database errors
	if !apperrors.IsCode(err, apperrors.CodeInternal) {
		t.Errorf("expected error code %s for GetCard failure, got: %v", apperrors.CodeInternal, err)
	}
}

// NOTE: Testing the specific RecordDecision error path (storage.save_failed)
// requires a mock store to isolate it from GetCard. The error mapping code
// at actions.go:91 is present but hard to exercise without mocks.
// The sentinel error paths (ErrCardNotFound, ErrAlreadyDecided) are tested
// in TestProcessDecisionCardNotFound and TestProcessDecisionAlreadyDecided.

// -----------------------------------------------------------------------------
// Per-Chunk Operations Tests (Unit 4.5b.1)
// -----------------------------------------------------------------------------

func TestBuildPatch(t *testing.T) {
	chunkContent := "@@ -1,4 +1,4 @@\n-old line\n+new line\n context\n"
	patch := BuildPatch("test.txt", chunkContent, false)

	expected := `diff --git a/test.txt b/test.txt
--- a/test.txt
+++ b/test.txt
@@ -1,4 +1,4 @@
-old line
+new line
 context
`
	if patch != expected {
		t.Errorf("unexpected patch:\ngot:\n%s\nwant:\n%s", patch, expected)
	}
}

func TestBuildPatchNewFile(t *testing.T) {
	chunkContent := "@@ -0,0 +1,2 @@\n+line 1\n+line 2\n"
	patch := BuildPatch("newfile.txt", chunkContent, true)

	expected := `diff --git a/newfile.txt b/newfile.txt
new file mode 100644
--- /dev/null
+++ b/newfile.txt
@@ -0,0 +1,2 @@
+line 1
+line 2
`
	if patch != expected {
		t.Errorf("unexpected patch for new file:\ngot:\n%s\nwant:\n%s", patch, expected)
	}
}

func TestBuildPatchAddsNewline(t *testing.T) {
	// Test that BuildPatch adds trailing newline if missing
	chunkContent := "@@ -1,1 +1,1 @@\n-old\n+new"
	patch := BuildPatch("file.go", chunkContent, false)

	if patch[len(patch)-1] != '\n' {
		t.Error("expected patch to end with newline")
	}
}


// contains is a helper to check if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Tracked File Deletion Tests (Phase 5 Fixes)
// -----------------------------------------------------------------------------

// TestProcessDecisionTrackedDeletionAccept tests that accepting a deletion diff
// stages the deletion (removes file from index).
func TestProcessDecisionTrackedDeletionAccept(t *testing.T) {
	dir := setupGitRepo(t)

	// Delete the tracked file (creates a deletion diff)
	testFile := filepath.Join(dir, "test.txt")
	if err := os.Remove(testFile); err != nil {
		t.Fatalf("remove file failed: %v", err)
	}

	// Verify git sees it as deleted
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !contains(string(out), " D test.txt") {
		t.Fatalf("expected file to show as deleted, got: %s", out)
	}

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	card := &storage.ReviewCard{
		ID:     "fc-deletion",
		File:   "test.txt",
		Diff:   "diff showing deletion",
		Status: storage.CardPending,
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	proc := NewProcessor(store, dir)

	// Accept the deletion
	err = proc.ProcessDecision("fc-deletion", "accept", "")
	if err != nil {
		t.Fatalf("ProcessDecision failed: %v", err)
	}

	// Verify the deletion is staged
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, _ = cmd.CombinedOutput()
	if !contains(string(out), "D  test.txt") { // "D " at start means staged
		t.Errorf("expected deletion to be staged, got: %s", out)
	}
}

// TestProcessDecisionTrackedDeletionReject tests that rejecting a deletion diff
// restores the file from HEAD.
func TestProcessDecisionTrackedDeletionReject(t *testing.T) {
	dir := setupGitRepo(t)

	// Delete the tracked file (creates a deletion diff)
	testFile := filepath.Join(dir, "test.txt")
	if err := os.Remove(testFile); err != nil {
		t.Fatalf("remove file failed: %v", err)
	}

	// Verify file doesn't exist
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Fatal("expected file to be deleted")
	}

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	card := &storage.ReviewCard{
		ID:     "fc-deletion-reject",
		File:   "test.txt",
		Diff:   "diff showing deletion",
		Status: storage.CardPending,
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	proc := NewProcessor(store, dir)

	// Reject the deletion (restore the file)
	err = proc.ProcessDecision("fc-deletion-reject", "reject", "")
	if err != nil {
		t.Fatalf("ProcessDecision failed: %v", err)
	}

	// Verify the file is restored
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Error("expected file to be restored")
	}

	// Verify content is correct
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	if string(content) != "initial content\n" {
		t.Errorf("unexpected content: %s", content)
	}
}

// The card should be unstaged and returned to pending state.
func TestProcessUndo_AcceptedCard(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify a tracked file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nmodified line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create the diff content that matches what would be generated
	diffContent := "@@ -1 +1,2 @@\n initial content\n+modified line\n"

	// Create a card for the change
	card := &storage.ReviewCard{
		ID:        "card-undo-accept",
		File:      "test.txt",
		Diff:      diffContent,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Create processor with decided store
	processor := NewProcessor(store, repoDir)
	processor.SetDecidedStore(store)

	// Accept the card (stages the file)
	err = processor.ProcessDecision("card-undo-accept", "accept", "")
	if err != nil {
		t.Fatalf("ProcessDecision accept failed: %v", err)
	}

	// Verify card is in decided storage as accepted
	decidedCard, err := store.GetDecidedCard("card-undo-accept")
	if err != nil {
		t.Fatalf("GetDecidedCard failed: %v", err)
	}
	if decidedCard == nil {
		t.Fatal("expected decided card to exist")
	}
	if decidedCard.Status != storage.CardAccepted {
		t.Errorf("expected status accepted, got %s", decidedCard.Status)
	}

	// Verify file is staged
	status := getGitStatus(t, repoDir, "test.txt")
	if !strings.Contains(status, "M") {
		t.Errorf("expected staged status, got: %q", status)
	}

	// Undo the accept
	restoredCard, err := processor.ProcessUndo("card-undo-accept", false)
	if err != nil {
		t.Fatalf("ProcessUndo failed: %v", err)
	}
	if restoredCard == nil {
		t.Fatal("expected restored card")
	}
	if restoredCard.File != "test.txt" {
		t.Errorf("expected file test.txt, got %s", restoredCard.File)
	}

	// Verify file is no longer staged (should show unstaged changes)
	status = getGitStatus(t, repoDir, "test.txt")
	if !strings.HasPrefix(status, " M") {
		t.Errorf("expected unstaged changes, got: %q", status)
	}

	// Verify card is back in active storage as pending
	activeCard, err := store.GetCard("card-undo-accept")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if activeCard == nil {
		t.Fatal("expected card to be restored to active storage")
	}
	if activeCard.Status != storage.CardPending {
		t.Errorf("expected pending status, got %s", activeCard.Status)
	}

	// Verify card is removed from decided storage
	decidedCard, _ = store.GetDecidedCard("card-undo-accept")
	if decidedCard != nil {
		t.Error("expected decided card to be deleted after undo")
	}
}

// TestProcessUndo_RejectedCard tests undoing a rejected card.
// The discarded change should be re-applied to the working tree.
func TestProcessUndo_RejectedCard(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify a tracked file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nrejected line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create the diff content
	diffContent := "@@ -1 +1,2 @@\n initial content\n+rejected line\n"

	// Create a card for the change
	card := &storage.ReviewCard{
		ID:        "card-undo-reject",
		File:      "test.txt",
		Diff:      diffContent,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Create processor with decided store
	processor := NewProcessor(store, repoDir)
	processor.SetDecidedStore(store)

	// Reject the card (restores the file to HEAD)
	err = processor.ProcessDecision("card-undo-reject", "reject", "")
	if err != nil {
		t.Fatalf("ProcessDecision reject failed: %v", err)
	}

	// Verify file is restored to HEAD (no changes)
	status := getGitStatus(t, repoDir, "test.txt")
	if status != "" {
		t.Errorf("expected clean status after reject, got: %q", status)
	}

	// Verify content is restored
	content := getFileContent(t, testFile)
	if content != "initial content\n" {
		t.Errorf("expected original content, got: %q", content)
	}

	// Verify card is in decided storage as rejected
	decidedCard, err := store.GetDecidedCard("card-undo-reject")
	if err != nil {
		t.Fatalf("GetDecidedCard failed: %v", err)
	}
	if decidedCard == nil {
		t.Fatal("expected decided card to exist")
	}
	if decidedCard.Status != storage.CardRejected {
		t.Errorf("expected status rejected, got %s", decidedCard.Status)
	}

	// Undo the reject (re-apply the change to working tree)
	_, err = processor.ProcessUndo("card-undo-reject", false)
	if err != nil {
		t.Fatalf("ProcessUndo failed: %v", err)
	}

	// Verify file has the change again (unstaged)
	status = getGitStatus(t, repoDir, "test.txt")
	if !strings.HasPrefix(status, " M") {
		t.Errorf("expected unstaged changes after undo reject, got: %q", status)
	}

	// Verify content has the rejected line back
	content = getFileContent(t, testFile)
	if !strings.Contains(content, "rejected line") {
		t.Errorf("expected rejected line to be restored, got: %q", content)
	}
}

// TestProcessUndo_CommittedCard_RequiresConfirmation tests that undoing
// a committed card requires confirmation.
func TestProcessUndo_CommittedCard_RequiresConfirmation(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a decided card that's already committed
	// (simulate the state after accept + commit)
	committedCard := &storage.DecidedCard{
		ID:           "card-committed",
		SessionID:    "session-1",
		File:         "test.txt",
		Patch:        "diff --git a/test.txt b/test.txt\n--- a/test.txt\n+++ b/test.txt\n@@ -1 +1,2 @@\n initial content\n+committed line\n",
		Status:       storage.CardCommitted,
		DecidedAt:    time.Now(),
		CommitHash:   "abc123",
		OriginalDiff: "@@ -1 +1,2 @@\n initial content\n+committed line\n",
	}
	if err := store.SaveDecidedCard(committedCard); err != nil {
		t.Fatalf("save decided card failed: %v", err)
	}

	processor := NewProcessor(store, repoDir)
	processor.SetDecidedStore(store)

	// Try to undo without confirmation - should fail
	_, err = processor.ProcessUndo("card-committed", false)
	if err == nil {
		t.Fatal("expected error when undoing committed card without confirmation")
	}
	if !strings.Contains(err.Error(), "requires confirmation") {
		t.Errorf("expected confirmation error, got: %v", err)
	}

	// Card should still be in decided storage
	decidedCard, _ := store.GetDecidedCard("card-committed")
	if decidedCard == nil {
		t.Error("expected decided card to still exist after failed undo")
	}
}

// TestProcessUndo_NotFound tests that undoing a non-existent card returns an error.
func TestProcessUndo_NotFound(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, repoDir)
	processor.SetDecidedStore(store)

	_, err = processor.ProcessUndo("non-existent-card", false)
	if err == nil {
		t.Fatal("expected error for non-existent card")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("expected not_found error, got: %v", err)
	}
}

func TestProcessDecisionStateDivergedAccept(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify the file to create uncommitted changes
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nadded line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card representing the change
	card := &storage.ReviewCard{
		ID:        "card-diverge-accept",
		File:      "test.txt",
		Diff:      "@@ -1 +1,2 @@\n initial content\n+added line\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Externally stage the change before the processor sees it.
	// This causes the forward patch (accept → git apply --cached) to fail
	// because the index already contains the change, while the reverse check
	// succeeds — triggering the divergence branch.
	runGit(t, repoDir, "add", "test.txt")

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-diverge-accept", "accept", "")
	if err == nil {
		t.Fatal("expected state.diverged error")
	}
	if !apperrors.IsCode(err, apperrors.CodeStateDiverged) {
		t.Errorf("expected state.diverged error code, got: %v", err)
	}

	// Verify no unintended staging side effects (index should still contain
	// only the externally staged change, no double-apply).
	cmd := exec.Command("git", "diff", "--cached", "--stat")
	cmd.Dir = repoDir
	out, _ := cmd.CombinedOutput()
	staged := strings.TrimSpace(string(out))
	if staged == "" {
		t.Error("expected the externally staged change to remain in the index")
	}

	// Card must remain pending (retryable after refresh).
	card, err = store.GetCard("card-diverge-accept")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card == nil || card.Status != storage.CardPending {
		t.Errorf("expected card to remain pending, got: %+v", card)
	}
}

// TestProcessDecisionStateDivergedReject verifies the file-level text decision
// divergence branch on the reject path: when the working tree change has already
// been reverted externally, reject returns state.diverged.
func TestProcessDecisionStateDivergedReject(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify the file to create uncommitted changes
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nadded line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card for the change
	card := &storage.ReviewCard{
		ID:        "card-diverge-reject",
		File:      "test.txt",
		Diff:      "@@ -1 +1,2 @@\n initial content\n+added line\n",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Externally restore the file before the processor sees it.
	// This causes the reverse patch (reject → git apply --reverse) to fail
	// because the working tree already matches HEAD, while the forward check
	// succeeds — triggering the divergence branch.
	runGit(t, repoDir, "restore", "test.txt")

	processor := NewProcessor(store, repoDir)
	err = processor.ProcessDecision("card-diverge-reject", "reject", "")
	if err == nil {
		t.Fatal("expected state.diverged error")
	}
	if !apperrors.IsCode(err, apperrors.CodeStateDiverged) {
		t.Errorf("expected state.diverged error code, got: %v", err)
	}

	// Verify no staging side effects.
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = repoDir
	out, _ := cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("expected no staged changes, got: %s", out)
	}

	// Card must remain pending.
	card, err = store.GetCard("card-diverge-reject")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card == nil || card.Status != storage.CardPending {
		t.Errorf("expected card to remain pending, got: %+v", card)
	}
}

// TestProcessDeletionDecisionDivergedReject verifies the deletion decision
// divergence branch: when rejecting a tracked deletion but the file already
// exists on disk (externally restored), returns state.diverged. This calls
// processDeletionDecision directly because ProcessDecision's file-existence
// check would route to processTextDecision when the file is present.
func TestProcessDeletionDecisionDivergedReject(t *testing.T) {
	repoDir := setupGitRepo(t)

	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// The card represents a deletion diff for a tracked file.
	card := &storage.ReviewCard{
		ID:        "card-del-diverge",
		File:      "test.txt",
		Diff:      "diff showing deletion",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// File exists on disk (simulates external restore after deletion was
	// detected). processDeletionDecision's reject path checks os.Stat and
	// finds the file exists → state.diverged.
	testFile := filepath.Join(repoDir, "test.txt")
	content := getFileContent(t, testFile)
	if !strings.Contains(content, "initial content") {
		t.Fatalf("expected file to exist with initial content, got: %s", content)
	}

	processor := NewProcessor(store, repoDir)
	err = processor.processDeletionDecision(card, "reject", "")
	if err == nil {
		t.Fatal("expected state.diverged error")
	}
	if !apperrors.IsCode(err, apperrors.CodeStateDiverged) {
		t.Errorf("expected state.diverged error code, got: %v", err)
	}

	// Verify the file is untouched (no side effects from the rejection).
	afterContent := getFileContent(t, testFile)
	if afterContent != content {
		t.Errorf("expected file unchanged, got: %s", afterContent)
	}

	// Verify no staging side effects.
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = repoDir
	out, _ := cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("expected no staged changes, got: %s", out)
	}

	// Card must remain pending.
	card, err = store.GetCard("card-del-diverge")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if card == nil || card.Status != storage.CardPending {
		t.Errorf("expected card to remain pending, got: %+v", card)
	}
}
