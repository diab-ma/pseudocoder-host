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

func TestDeleteUntrackedFileDeletesChunks(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a new untracked file
	newFile := filepath.Join(repoDir, "to-delete-with-chunks.txt")
	if err := os.WriteFile(newFile, []byte("delete me\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create a card for the file
	card := &storage.ReviewCard{
		ID:        "card-delete-chunks",
		File:      "to-delete-with-chunks.txt",
		Diff:      "+delete me",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Save chunks for the card
	chunks := []*storage.ChunkStatus{
		{
			CardID:     "card-delete-chunks",
			ChunkIndex: 0,
			Content:    "@@ -0,0 +1 @@\n+delete me",
			Status:     storage.CardPending,
		},
	}
	if err := store.SaveChunks("card-delete-chunks", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	// Verify chunks exist before deletion
	savedChunks, err := store.GetChunks("card-delete-chunks")
	if err != nil {
		t.Fatalf("get chunks failed: %v", err)
	}
	if len(savedChunks) != 1 {
		t.Fatalf("expected 1 chunk before deletion, got %d", len(savedChunks))
	}

	// Delete the untracked file with chunk store configured
	processor := NewProcessor(store, repoDir)
	processor.SetChunkStore(store)
	if err := processor.DeleteUntrackedFile("card-delete-chunks"); err != nil {
		t.Fatalf("DeleteUntrackedFile failed: %v", err)
	}

	// Verify file is deleted
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}

	// Verify card is deleted
	card, _ = store.GetCard("card-delete-chunks")
	if card != nil {
		t.Error("expected card to be deleted after file deletion")
	}

	// Verify chunks are deleted
	savedChunks, err = store.GetChunks("card-delete-chunks")
	if err != nil {
		t.Fatalf("get chunks after deletion failed: %v", err)
	}
	if len(savedChunks) != 0 {
		t.Errorf("expected 0 chunks after deletion, got %d", len(savedChunks))
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

// setupGitRepoWithMultipleChunks creates a repo with a file that has two chunks of changes.
// Returns the repo dir, the two chunk contents, and the file path.
func setupGitRepoWithMultipleChunks(t *testing.T) (string, string, string, string) {
	t.Helper()

	dir := t.TempDir()

	// Initialize git repo
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")

	// Create initial file with 10 lines (enough for 2 separate chunks)
	testFile := filepath.Join(dir, "multi.txt")
	content := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	runGit(t, dir, "add", "multi.txt")
	runGit(t, dir, "commit", "-m", "initial")

	// Modify lines 1 and 9 to create two separate chunks
	modified := "line1-CHANGED\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9-CHANGED\nline10\n"
	if err := os.WriteFile(testFile, []byte(modified), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Get the actual diff to extract chunk content
	chunk1 := "@@ -1,4 +1,4 @@\n-line1\n+line1-CHANGED\n line2\n line3\n line4"
	chunk2 := "@@ -6,5 +6,5 @@\n line6\n line7\n line8\n-line9\n+line9-CHANGED\n line10"

	return dir, chunk1, chunk2, "multi.txt"
}

func TestStageChunk(t *testing.T) {
	dir, chunk1, _, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, dir)

	// Stage only the first chunk
	if err := processor.StageChunk(fileName, chunk1); err != nil {
		t.Fatalf("StageChunk failed: %v", err)
	}

	// Verify: staged diff should show only chunk1
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --cached failed: %v", err)
	}
	staged := string(out)

	if !contains(staged, "line1-CHANGED") {
		t.Error("expected chunk1 to be staged")
	}
	if contains(staged, "line9-CHANGED") {
		t.Error("expected chunk2 to NOT be staged yet")
	}

	// Verify: unstaged diff should still show chunk2
	cmd = exec.Command("git", "diff")
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff failed: %v", err)
	}
	unstaged := string(out)

	if contains(unstaged, "line1-CHANGED") {
		t.Error("expected chunk1 to NOT be in unstaged diff")
	}
	if !contains(unstaged, "line9-CHANGED") {
		t.Error("expected chunk2 to still be in unstaged diff")
	}
}

func TestRestoreChunk(t *testing.T) {
	dir, chunk1, _, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	processor := NewProcessor(store, dir)

	// Restore (reject) only the first chunk
	if err := processor.RestoreChunk(fileName, chunk1); err != nil {
		t.Fatalf("RestoreChunk failed: %v", err)
	}

	// Read the file content
	content := getFileContent(t, filepath.Join(dir, fileName))

	// Chunk1 should be reverted (line1 back to original)
	if contains(content, "line1-CHANGED") {
		t.Error("expected chunk1 to be reverted (line1 should be original)")
	}
	if !contains(content, "line1\n") {
		t.Error("expected line1 to be restored to original")
	}

	// Chunk2 should still be present
	if !contains(content, "line9-CHANGED") {
		t.Error("expected chunk2 to still be present (line9-CHANGED)")
	}
}

func TestProcessChunkDecisionAccept(t *testing.T) {
	dir, chunk1, chunk2, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a file card
	card := &storage.ReviewCard{
		ID:        "fc-multi",
		File:      fileName,
		Diff:      chunk1 + "\n" + chunk2,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Save chunks for the card
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-multi", ChunkIndex: 0, Content: chunk1, Status: storage.CardPending},
		{CardID: "fc-multi", ChunkIndex: 1, Content: chunk2, Status: storage.CardPending},
	}
	if err := store.SaveChunks("fc-multi", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Accept only chunk 0
	if err := processor.ProcessChunkDecision("fc-multi", 0, "accept", ""); err != nil {
		t.Fatalf("ProcessChunkDecision failed: %v", err)
	}

	// Verify chunk 0 is staged
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	staged := string(out)
	if !contains(staged, "line1-CHANGED") {
		t.Error("expected chunk 0 to be staged")
	}

	// Verify chunk 0 is marked as accepted
	h, err := store.GetChunk("fc-multi", 0)
	if err != nil {
		t.Fatalf("get chunk failed: %v", err)
	}
	if h.Status != storage.CardAccepted {
		t.Errorf("expected chunk 0 status to be accepted, got: %s", h.Status)
	}

	// Verify chunk 1 is still pending
	h, err = store.GetChunk("fc-multi", 1)
	if err != nil {
		t.Fatalf("get chunk failed: %v", err)
	}
	if h.Status != storage.CardPending {
		t.Errorf("expected chunk 1 status to be pending, got: %s", h.Status)
	}

	// Card should still exist (not all chunks decided)
	c, err := store.GetCard("fc-multi")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if c == nil {
		t.Error("expected card to still exist (chunk 1 is pending)")
	}
}

func TestProcessChunkDecisionReject(t *testing.T) {
	dir, chunk1, chunk2, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a file card
	card := &storage.ReviewCard{
		ID:        "fc-reject",
		File:      fileName,
		Diff:      chunk1 + "\n" + chunk2,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Save chunks for the card
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-reject", ChunkIndex: 0, Content: chunk1, Status: storage.CardPending},
		{CardID: "fc-reject", ChunkIndex: 1, Content: chunk2, Status: storage.CardPending},
	}
	if err := store.SaveChunks("fc-reject", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Reject chunk 0
	if err := processor.ProcessChunkDecision("fc-reject", 0, "reject", ""); err != nil {
		t.Fatalf("ProcessChunkDecision failed: %v", err)
	}

	// Verify chunk 0 is reverted in working tree
	content := getFileContent(t, filepath.Join(dir, fileName))
	if contains(content, "line1-CHANGED") {
		t.Error("expected chunk 0 to be reverted")
	}
	if !contains(content, "line9-CHANGED") {
		t.Error("expected chunk 1 to still be present")
	}
}

func TestProcessChunkDecisionAllDecidedCleansUp(t *testing.T) {
	dir, chunk1, chunk2, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a file card
	card := &storage.ReviewCard{
		ID:        "fc-cleanup",
		File:      fileName,
		Diff:      chunk1 + "\n" + chunk2,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Save chunks for the card
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-cleanup", ChunkIndex: 0, Content: chunk1, Status: storage.CardPending},
		{CardID: "fc-cleanup", ChunkIndex: 1, Content: chunk2, Status: storage.CardPending},
	}
	if err := store.SaveChunks("fc-cleanup", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Accept both chunks
	if err := processor.ProcessChunkDecision("fc-cleanup", 0, "accept", ""); err != nil {
		t.Fatalf("ProcessChunkDecision for chunk 0 failed: %v", err)
	}
	if err := processor.ProcessChunkDecision("fc-cleanup", 1, "accept", ""); err != nil {
		t.Fatalf("ProcessChunkDecision for chunk 1 failed: %v", err)
	}

	// Verify card is deleted (all chunks decided)
	c, err := store.GetCard("fc-cleanup")
	if err != nil {
		t.Fatalf("get card failed: %v", err)
	}
	if c != nil {
		t.Error("expected card to be deleted after all chunks decided")
	}

	// Verify chunks are deleted
	hs, err := store.GetChunks("fc-cleanup")
	if err != nil {
		t.Fatalf("get chunks failed: %v", err)
	}
	if len(hs) != 0 {
		t.Errorf("expected chunks to be deleted, got %d", len(hs))
	}
}

func TestProcessChunkDecisionBinaryFile(t *testing.T) {
	dir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card for a binary file (uses placeholder diff)
	card := &storage.ReviewCard{
		ID:        "fc-binary",
		File:      "image.png",
		Diff:      diff.BinaryDiffPlaceholder,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Try to decide a chunk on a binary file
	err = processor.ProcessChunkDecision("fc-binary", 0, "accept", "")
	if err == nil {
		t.Fatal("expected error for binary file chunk decision")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionBinaryFile) {
		t.Errorf("expected action.binary_file error, got: %v", err)
	}

	// Error message should mention the file
	if !strings.Contains(err.Error(), "image.png") {
		t.Errorf("error should mention the file name, got: %v", err)
	}
}

func TestProcessChunkDecisionPathTraversal(t *testing.T) {
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
			dir := setupGitRepo(t)
			store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatalf("create store failed: %v", err)
			}
			defer store.Close()

			// Create a card with a path traversal attempt
			card := &storage.ReviewCard{
				ID:        "fc-traversal",
				File:      tc.path,
				Diff:      "@@ -1 +1 @@\n-a\n+b",
				Status:    storage.CardPending,
				CreatedAt: time.Now(),
			}
			if err := store.SaveCard(card); err != nil {
				t.Fatalf("save card failed: %v", err)
			}

			processor := NewProcessor(store, dir)
			processor.SetChunkStore(store)

			err = processor.ProcessChunkDecision("fc-traversal", 0, "accept", "")
			if err == nil {
				t.Fatal("expected error for path traversal attempt")
			}
			if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
				t.Errorf("expected error code %s, got: %v", apperrors.CodeActionInvalid, err)
			}
		})
	}
}

func TestProcessChunkDecisionChunkNotFound(t *testing.T) {
	dir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card but no chunks
	card := &storage.ReviewCard{
		ID:        "fc-nochunks",
		File:      "test.txt",
		Diff:      "@@ -1 +1 @@\n-a\n+b",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Try to decide a nonexistent chunk
	err = processor.ProcessChunkDecision("fc-nochunks", 0, "accept", "")
	if err == nil {
		t.Fatal("expected error for nonexistent chunk")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found error, got: %v", err)
	}
}

func TestProcessChunkDecisionAlreadyDecided(t *testing.T) {
	dir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card
	card := &storage.ReviewCard{
		ID:        "fc-already",
		File:      "test.txt",
		Diff:      "@@ -1 +1 @@\n-a\n+b",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Create an already-decided chunk
	now := time.Now()
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-already", ChunkIndex: 0, Content: "@@ -1 +1 @@\n-a\n+b", Status: storage.CardAccepted, DecidedAt: &now},
	}
	if err := store.SaveChunks("fc-already", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Try to decide an already-decided chunk
	err = processor.ProcessChunkDecision("fc-already", 0, "reject", "")
	if err == nil {
		t.Fatal("expected error for already decided chunk")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageAlreadyDecided) {
		t.Errorf("expected storage.already_decided error, got: %v", err)
	}
}

func TestProcessChunkDecisionNoChunkStore(t *testing.T) {
	dir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create processor without setting chunk store
	processor := NewProcessor(store, dir)

	err = processor.ProcessChunkDecision("card-123", 0, "accept", "")
	if err == nil {
		t.Fatal("expected error when chunk store not configured")
	}
	if !apperrors.IsCode(err, apperrors.CodeInternal) {
		t.Errorf("expected error.internal, got: %v", err)
	}
}

func TestProcessChunkDecisionGitFailureAllowsRetry(t *testing.T) {
	dir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a card
	card := &storage.ReviewCard{
		ID:        "fc-retry",
		File:      "test.txt",
		Diff:      "@@ invalid chunk",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Create a chunk with invalid content that will cause git apply to fail
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-retry", ChunkIndex: 0, Content: "@@ invalid chunk content", Status: storage.CardPending},
	}
	if err := store.SaveChunks("fc-retry", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// First attempt - should fail because the chunk content is invalid
	err = processor.ProcessChunkDecision("fc-retry", 0, "accept", "")
	if err == nil {
		t.Fatal("expected validation error for invalid chunk")
	}
	if !apperrors.IsCode(err, apperrors.CodeConflictDetected) &&
		!apperrors.IsCode(err, apperrors.CodeValidationFailed) {
		t.Errorf("expected validation/conflict error, got: %v", err)
	}

	// Verify chunk is still pending (not marked as decided)
	chunk, err := store.GetChunk("fc-retry", 0)
	if err != nil {
		t.Fatalf("get chunk failed: %v", err)
	}
	if chunk.Status != storage.CardPending {
		t.Errorf("expected chunk to remain pending after git failure, got: %s", chunk.Status)
	}

	// Second attempt - should also fail with validation/conflict (NOT AlreadyDecided)
	// This proves the chunk is retryable after validation failure
	err = processor.ProcessChunkDecision("fc-retry", 0, "accept", "")
	if err == nil {
		t.Fatal("expected validation error on retry")
	}
	if apperrors.IsCode(err, apperrors.CodeStorageAlreadyDecided) {
		t.Error("chunk should be retryable after validation failure, but got AlreadyDecided")
	}
	if !apperrors.IsCode(err, apperrors.CodeConflictDetected) &&
		!apperrors.IsCode(err, apperrors.CodeValidationFailed) {
		t.Errorf("expected validation/conflict error on retry, got: %v", err)
	}
}

func TestProcessChunkDecisionStaleContentHash(t *testing.T) {
	dir, chunk1, chunk2, fileName := setupGitRepoWithMultipleChunks(t)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a file card
	card := &storage.ReviewCard{
		ID:        "fc-stale",
		File:      fileName,
		Diff:      chunk1 + "\n" + chunk2,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Save chunks for the card
	chunks := []*storage.ChunkStatus{
		{CardID: "fc-stale", ChunkIndex: 0, Content: chunk1, Status: storage.CardPending},
		{CardID: "fc-stale", ChunkIndex: 1, Content: chunk2, Status: storage.CardPending},
	}
	if err := store.SaveChunks("fc-stale", chunks); err != nil {
		t.Fatalf("save chunks failed: %v", err)
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(store)

	// Try to decide chunk 0 with a wrong content hash
	// The correct hash would be computed from chunk1, but we pass a fake one
	wrongHash := "deadbeef12345678" // 16 hex chars like a real hash but wrong
	err = processor.ProcessChunkDecision("fc-stale", 0, "accept", wrongHash)

	// Should fail with action.chunk_stale error
	if err == nil {
		t.Fatal("expected error for stale content hash")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionChunkStale) {
		t.Errorf("expected action.chunk_stale error, got: %v", err)
	}

	// Verify chunk is still pending (not changed)
	chunk, err := store.GetChunk("fc-stale", 0)
	if err != nil {
		t.Fatalf("get chunk failed: %v", err)
	}
	if chunk.Status != storage.CardPending {
		t.Errorf("expected chunk to remain pending after stale hash rejection, got: %s", chunk.Status)
	}

	// Verify git state is unchanged (nothing staged)
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if string(out) != "" {
		t.Errorf("expected no staged changes after stale rejection, got: %s", string(out))
	}
}

// staticChunkStore returns a fixed chunk without persisting it.
// This forces RecordChunkDecision (tx) to fail with ErrChunkNotFound.
type staticChunkStore struct {
	chunk *storage.ChunkStatus
}

func (s *staticChunkStore) SaveChunks(cardID string, chunks []*storage.ChunkStatus) error {
	return nil
}

func (s *staticChunkStore) GetChunks(cardID string) ([]*storage.ChunkStatus, error) {
	if s.chunk == nil {
		return []*storage.ChunkStatus{}, nil
	}
	return []*storage.ChunkStatus{s.chunk}, nil
}

func (s *staticChunkStore) GetChunk(cardID string, chunkIndex int) (*storage.ChunkStatus, error) {
	if s.chunk == nil {
		return nil, nil
	}
	if s.chunk.CardID == cardID && s.chunk.ChunkIndex == chunkIndex {
		return s.chunk, nil
	}
	return nil, nil
}

func (s *staticChunkStore) RecordChunkDecision(decision *storage.ChunkDecision) error {
	return nil
}

func (s *staticChunkStore) DeleteChunks(cardID string) error {
	return nil
}

func (s *staticChunkStore) CountPendingChunks(cardID string) (int, error) {
	if s.chunk == nil {
		return 0, nil
	}
	return 1, nil
}

func TestProcessChunkDecisionStorageFailureAfterGit(t *testing.T) {
	dir, chunk1, _, fileName := setupGitRepoWithMultipleChunks(t)
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Create a file card
	card := &storage.ReviewCard{
		ID:        "fc-storefail",
		File:      fileName,
		Diff:      chunk1,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("save card failed: %v", err)
	}

	// Provide a chunk via a static store but do NOT persist it to DB.
	staticStore := &staticChunkStore{
		chunk: &storage.ChunkStatus{
			CardID:     "fc-storefail",
			ChunkIndex: 0,
			Content:    chunk1,
			Status:     storage.CardPending,
		},
	}

	processor := NewProcessor(store, dir)
	processor.SetChunkStore(staticStore)

	// Attempt to decide - git will succeed, but RecordChunkDecision will fail (chunk missing in DB)
	err = processor.ProcessChunkDecision("fc-storefail", 0, "accept", "")

	// Should return an error (storage failure after git success)
	if err == nil {
		t.Fatal("expected error when storage fails after git success")
	}

	// The error should be storage.not_found since the chunk isn't in DB
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found error, got: %v", err)
	}

	// Rollback should undo staging after storage failure
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	staged := string(out)
	if staged != "" {
		t.Errorf("expected no staged changes after rollback, got: %s", staged)
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

// TestStageChunkNewFile tests that staging a chunk on a new file uses the correct
// patch format (--- /dev/null instead of --- a/file).
func TestStageChunkNewFile(t *testing.T) {
	dir := setupGitRepo(t)

	// Create a new file (not tracked)
	newFile := filepath.Join(dir, "newfile.txt")
	if err := os.WriteFile(newFile, []byte("line 1\nline 2\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	proc := NewProcessor(store, dir)

	// The chunk content for a new file
	chunkContent := "@@ -0,0 +1,2 @@\n+line 1\n+line 2\n"

	// Stage the chunk
	err = proc.StageChunk("newfile.txt", chunkContent)
	if err != nil {
		t.Fatalf("StageChunk failed: %v", err)
	}

	// Verify the content is staged
	cmd := exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !contains(string(out), "newfile.txt") {
		t.Errorf("expected newfile.txt to be staged, got: %s", out)
	}
}

// -----------------------------------------------------------------------------
// Undo Tests (Phase 20.2)
// -----------------------------------------------------------------------------

// TestProcessUndo_AcceptedCard tests undoing an accepted (staged) card.
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

// TestProcessChunkUndo_AcceptedChunk tests undoing an accepted chunk.
func TestProcessChunkUndo_AcceptedChunk(t *testing.T) {
	repoDir := setupGitRepo(t)
	store, err := storage.NewSQLiteStore(filepath.Join(repoDir, "test.db"))
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	// Modify a tracked file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nchunk line\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	// Create the chunk content
	chunkContent := "@@ -1 +1,2 @@\n initial content\n+chunk line\n"
	chunkPatch := BuildPatch("test.txt", chunkContent, false)

	// Create a decided card and chunk (simulating accepted chunk)
	decidedCard := &storage.DecidedCard{
		ID:           "card-chunk-undo",
		SessionID:    "session-1",
		File:         "test.txt",
		Patch:        chunkPatch,
		Status:       storage.CardAccepted,
		DecidedAt:    time.Now(),
		OriginalDiff: chunkContent,
	}
	if err := store.SaveDecidedCard(decidedCard); err != nil {
		t.Fatalf("save decided card failed: %v", err)
	}

	decidedChunk := &storage.DecidedChunk{
		CardID:     "card-chunk-undo",
		ChunkIndex: 0,
		Patch:      chunkPatch,
		Status:     storage.CardAccepted,
		DecidedAt:  time.Now(),
	}
	if err := store.SaveDecidedChunk(decidedChunk); err != nil {
		t.Fatalf("save decided chunk failed: %v", err)
	}

	// Stage the file to simulate accepted state
	runGit(t, repoDir, "add", "test.txt")

	// Create processor
	processor := NewProcessor(store, repoDir)
	processor.SetDecidedStore(store)
	processor.SetChunkStore(store)

	// Undo the chunk (empty contentHash falls back to index lookup)
	chunk, card, err := processor.ProcessChunkUndo("card-chunk-undo", 0, "", false)
	if err != nil {
		t.Fatalf("ProcessChunkUndo failed: %v", err)
	}
	if chunk == nil || card == nil {
		t.Fatal("expected chunk and card to be returned")
	}
	if card.File != "test.txt" {
		t.Errorf("expected file test.txt, got %s", card.File)
	}

	// Verify chunk is no longer in decided storage
	decidedChunk, _ = store.GetDecidedChunk("card-chunk-undo", 0)
	if decidedChunk != nil {
		t.Error("expected decided chunk to be deleted after undo")
	}
}
