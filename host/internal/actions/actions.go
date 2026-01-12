// Package actions provides git operations for accepting and rejecting review cards.
// It uses git commands to stage (accept) or restore (reject) file changes based
// on user decisions from the mobile client.
package actions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/stream"
)

// CardRemovedCallback is called when a card is removed from storage.
// The file path is provided so callers can clear any cached state.
type CardRemovedCallback func(file string)

// Processor handles accept/reject actions on review cards.
// It coordinates between storage (for card lookup and decision recording)
// and git (for staging or restoring changes).
type Processor struct {
	// store is the card storage for looking up cards and recording decisions.
	store storage.CardStore

	// chunkStore is the optional chunk storage for per-chunk decisions.
	// If nil, per-chunk decisions are not supported.
	chunkStore storage.ChunkStore

	// decidedStore is the optional storage for archiving decided cards.
	// If nil, decided cards are not archived (undo not supported).
	decidedStore storage.DecidedCardStore

	// repoPath is the path to the git repository.
	repoPath string

	// onCardRemoved is called when a card is removed from storage.
	// This allows callers to clear cached state (e.g., seenFileHashes).
	onCardRemoved CardRemovedCallback
}

// NewProcessor creates a new action processor.
// The store is used to look up cards and record decisions.
// The repoPath is the git repository where changes are staged or restored.
func NewProcessor(store storage.CardStore, repoPath string) *Processor {
	return &Processor{
		store:    store,
		repoPath: repoPath,
	}
}

// SetChunkStore sets the optional chunk store for per-chunk decisions.
// If the store implements ChunkStore, this enables ProcessChunkDecision.
func (p *Processor) SetChunkStore(hs storage.ChunkStore) {
	p.chunkStore = hs
}

// SetDecidedStore sets the optional decided card store for undo support.
// If set, decided cards are archived before deletion for later undo.
func (p *Processor) SetDecidedStore(ds storage.DecidedCardStore) {
	p.decidedStore = ds
}

// SetCardRemovedCallback sets the callback called when a card is removed.
// Use this to clear cached state (e.g., seenFileHashes in CardStreamer).
func (p *Processor) SetCardRemovedCallback(cb CardRemovedCallback) {
	p.onCardRemoved = cb
}

// notifyCardRemoved calls the onCardRemoved callback if set.
func (p *Processor) notifyCardRemoved(file string) {
	if p.onCardRemoved != nil {
		p.onCardRemoved(file)
	}
}

// validateFilePath checks that a file path is safe to use within the repo.
// It prevents path traversal attacks by ensuring:
// 1. The path is a valid local path (no .. components that escape, no absolute paths)
// 2. The resolved path stays within the repository root
//
// Returns a CodedError with action.invalid if validation fails.
func validateFilePath(repoPath, file string) error {
	// Check for local path (Go 1.20+) - rejects absolute paths and paths starting with ..
	if !filepath.IsLocal(file) {
		return apperrors.New(apperrors.CodeActionInvalid,
			fmt.Sprintf("invalid file path: %s (must be relative to repo)", file))
	}

	// Resolve absolute paths for comparison
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to resolve repo path", err)
	}

	// Build and clean the full path
	fullPath := filepath.Join(absRepo, file)
	resolved := filepath.Clean(fullPath)
	absRepo = filepath.Clean(absRepo)

	// Ensure resolved path is within repo (handles symlinks and edge cases)
	if !strings.HasPrefix(resolved, absRepo+string(filepath.Separator)) && resolved != absRepo {
		return apperrors.New(apperrors.CodeActionInvalid,
			fmt.Sprintf("file path escapes repository: %s", file))
	}

	return nil
}

// ProcessDecision handles an accept or reject action on a review card.
// For "accept", it stages the file changes using git add.
// For "reject", it restores the file to HEAD using git restore.
//
// This method is atomic with respect to concurrent decisions:
// 1. First, the decision is recorded in storage (atomic claim via WHERE status='pending')
// 2. Then, the git operation is applied
// 3. If git fails after storage update, the decision is recorded but file state may be inconsistent
//
// Returns a CodedError with appropriate error code:
//   - action.card_not_found if the card doesn't exist
//   - action.already_decided if another request already decided this card
//   - action.invalid if the action is not 'accept' or 'reject'
//   - action.file_deleted if the file no longer exists on disk
//   - action.git_failed if the git operation fails
func (p *Processor) ProcessDecision(cardID, action, comment string) error {
	// Validate action
	if action != "accept" && action != "reject" {
		return apperrors.InvalidAction(action)
	}

	// Look up the card to get the file path
	card, err := p.store.GetCard(cardID)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to get card", err)
	}
	if card == nil {
		return apperrors.NotFound("card")
	}

	// Validate file path to prevent path traversal attacks
	if err := validateFilePath(p.repoPath, card.File); err != nil {
		return err
	}

	// Check if the file still exists on disk
	filePath := filepath.Join(p.repoPath, card.File)
	fileExists := true
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		fileExists = false
	}

	// If file doesn't exist, check if it's a tracked deletion (file existed in HEAD)
	// vs truly stale (file never existed or was already committed as deleted)
	if !fileExists {
		wasTracked, err := p.wasTrackedInHEAD(card.File)
		if err != nil {
			log.Printf("actions: failed to check if file was tracked: %v", err)
			// Be conservative - treat as stale
			if delErr := p.store.DeleteCard(cardID); delErr != nil {
				log.Printf("actions: failed to delete card %s: %v", cardID, delErr)
			} else {
				p.notifyCardRemoved(card.File)
			}
			return apperrors.FileDeleted(card.File)
		}
		if !wasTracked {
			// File was never tracked - this is truly stale (e.g., user deleted untracked file)
			if delErr := p.store.DeleteCard(cardID); delErr != nil {
				log.Printf("actions: failed to delete stale card %s: %v", cardID, delErr)
			} else {
				p.notifyCardRemoved(card.File)
			}
			return apperrors.FileDeleted(card.File)
		}
		// File was tracked but is now deleted - this is a deletion diff
		// Accept = stage the deletion, Reject = restore the file
		// Fall through to normal processing
	}

	// For reject actions, check if the file is untracked BEFORE recording the decision.
	// Untracked files cannot be restored with git restore - they need to be deleted.
	// We return a specific error so the mobile client can offer deletion instead.
	// The card remains pending so the user can retry with delete action.
	if action == "reject" {
		untracked, err := p.isUntrackedFile(card.File)
		if err != nil {
			log.Printf("actions: failed to check if file is untracked: %v", err)
			// Continue anyway - the git restore will fail with a more specific error
		} else if untracked {
			// Return specific error so mobile can offer deletion
			return apperrors.UntrackedFile(card.File)
		}
	}

	// Record the decision in storage FIRST (atomic claim).
	// This ensures only one concurrent request can succeed.
	// The storage layer uses WHERE status='pending' to make this atomic.
	status := storage.CardAccepted
	if action == "reject" {
		status = storage.CardRejected
	}

	decision := &storage.Decision{
		CardID:    cardID,
		Status:    status,
		Comment:   comment,
		Timestamp: time.Now(),
	}

	if err := p.store.RecordDecision(decision); err != nil {
		// Map storage errors to coded errors for API consistency
		if errors.Is(err, storage.ErrCardNotFound) {
			return apperrors.NotFound("card")
		}
		if errors.Is(err, storage.ErrAlreadyDecided) {
			return apperrors.AlreadyDecided(cardID)
		}
		return apperrors.Wrap(apperrors.CodeStorageSaveFailed, "failed to record decision", err)
	}

	// Apply the git operation AFTER claiming the card.
	// This prevents race conditions where two requests could both apply
	// conflicting git operations.
	var gitErr error
	if action == "accept" {
		gitErr = p.stageFile(card.File)
	} else {
		gitErr = p.restoreFile(card.File)
	}

	if gitErr != nil {
		// Decision is already recorded, but git failed.
		// Log the error but don't fail - the card is marked as decided.
		// The user can see the decision status; file state may need manual fix.
		log.Printf("actions: git operation failed after recording decision: %v", gitErr)
		return apperrors.GitFailed(action, card.File, gitErr)
	}

	// Archive the card for undo support before deleting from active storage.
	// Build the patch that was applied so we can reverse it on undo.
	if p.decidedStore != nil {
		isNew, _ := p.isNewFile(card.File)
		patch := BuildPatch(card.File, card.Diff, isNew)

		decidedCard := &storage.DecidedCard{
			ID:           card.ID,
			SessionID:    card.SessionID,
			File:         card.File,
			Patch:        patch,
			Status:       status,
			DecidedAt:    time.Now(),
			OriginalDiff: card.Diff,
		}

		if err := p.decidedStore.SaveDecidedCard(decidedCard); err != nil {
			// Log but don't fail - the decision was processed successfully
			log.Printf("actions: failed to archive decided card %s: %v", cardID, err)
		}
	}

	// Delete the card from storage after successful git operation.
	// This ensures old decided cards don't accumulate in the database.
	if err := p.store.DeleteCard(cardID); err != nil {
		// Log but don't fail - the decision was already processed successfully
		log.Printf("actions: failed to delete card %s after decision: %v", cardID, err)
	} else {
		// Notify that card was removed so cached state can be cleared
		p.notifyCardRemoved(card.File)
	}

	return nil
}

// ProcessChunkDecision handles an accept or reject action on a single chunk.
// This enables granular per-chunk decisions within a file card.
//
// For "accept", it stages only the specified chunk using git apply --cached.
// For "reject", it restores only the specified chunk using git apply --reverse.
//
// The contentHash parameter is optional for backward compatibility. If provided,
// it must match the current chunk content hash, otherwise an action.chunk_stale
// error is returned to prevent applying decisions to changed content.
//
// Returns a CodedError with appropriate error code:
//   - action.card_not_found if the card or chunk doesn't exist
//   - action.binary_file if the card is a binary file (per-chunk actions not supported)
//   - action.already_decided if another request already decided this chunk
//   - action.invalid if the action is not 'accept' or 'reject'
//   - action.chunk_stale if contentHash doesn't match current content
//   - action.file_deleted if the file no longer exists on disk
//   - action.git_failed if the git operation fails
func (p *Processor) ProcessChunkDecision(cardID string, chunkIndex int, action string, contentHash string) error {
	// Validate action
	if action != "accept" && action != "reject" {
		return apperrors.InvalidAction(action)
	}

	// Ensure chunk store is available
	if p.chunkStore == nil {
		return apperrors.New(apperrors.CodeInternal, "chunk store not configured")
	}

	// Look up the card first to check for binary files
	// We do this before looking up chunks to give a better error for binary files
	card, err := p.store.GetCard(cardID)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to get card", err)
	}
	if card == nil {
		return apperrors.NotFound("card")
	}

	// Validate file path to prevent path traversal attacks
	if err := validateFilePath(p.repoPath, card.File); err != nil {
		return err
	}

	// Check if this is a binary file card
	// Binary files use a placeholder diff and have no chunks stored
	if card.Diff == diff.BinaryDiffPlaceholder {
		return apperrors.BinaryFile(card.File)
	}

	// Look up the chunk to get file path and content
	chunk, err := p.chunkStore.GetChunk(cardID, chunkIndex)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to get chunk", err)
	}
	if chunk == nil {
		return apperrors.NotFound("chunk")
	}

	// Validate content hash if provided (prevents stale decisions)
	// Empty hash skips validation for backward compatibility during transition
	if contentHash != "" {
		currentHash := hashChunkContent(chunk.Content)
		if contentHash != currentHash {
			log.Printf("actions: stale chunk decision rejected for %s:%d (expected %s, got %s)",
				cardID, chunkIndex, currentHash, contentHash)
			return apperrors.ChunkStale(cardID, chunkIndex)
		}
	}

	// Check if chunk is already decided (prevents duplicate attempts)
	if chunk.Status != storage.CardPending {
		return apperrors.AlreadyDecided(fmt.Sprintf("%s:chunk%d", cardID, chunkIndex))
	}

	// Check if the file still exists on disk (may have been deleted externally)
	filePath := filepath.Join(p.repoPath, card.File)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// File was deleted - check if this is a tracked deletion
		wasTracked, trackErr := p.wasTrackedInHEAD(card.File)
		if trackErr == nil && wasTracked {
			// This is a tracked file deletion - per-chunk doesn't make sense
			// Return specific error so UI can fall back to file-level actions
			return apperrors.New(apperrors.CodeActionGitFailed,
				"file deleted - use file-level accept/reject")
		}
		// Truly stale - remove the card and its chunks from storage
		if delErr := p.store.DeleteCard(cardID); delErr != nil {
			log.Printf("actions: failed to delete card %s after file deletion: %v", cardID, delErr)
		} else {
			p.notifyCardRemoved(card.File)
		}
		if p.chunkStore != nil {
			if delErr := p.chunkStore.DeleteChunks(cardID); delErr != nil {
				log.Printf("actions: failed to delete chunks for %s after file deletion: %v", cardID, delErr)
			}
		}
		return apperrors.FileDeleted(card.File)
	}

	// Apply the git operation FIRST.
	// Unlike file-level decisions, chunk operations are more likely to fail
	// (patch conflicts, modified working tree), so we defer the status update
	// until after git succeeds. This allows retry on failure.
	var gitErr error
	if action == "accept" {
		gitErr = p.StageChunk(card.File, chunk.Content)
	} else {
		gitErr = p.RestoreChunk(card.File, chunk.Content)
	}

	if gitErr != nil {
		log.Printf("actions: git operation failed for chunk %s:%d: %v", cardID, chunkIndex, gitErr)
		return apperrors.GitFailed(action, card.File, gitErr)
	}

	// Record the decision in storage AFTER git succeeds.
	// This ensures failed git operations remain retryable.
	status := storage.CardAccepted
	if action == "reject" {
		status = storage.CardRejected
	}

	decision := &storage.ChunkDecision{
		CardID:    cardID,
		ChunkIndex: chunkIndex,
		Status:    status,
		Timestamp: time.Now(),
	}

	if err := p.chunkStore.RecordChunkDecision(decision); err != nil {
		// Git succeeded but storage failed. We return an error to inform the user
		// that something went wrong. The chunk will remain "pending" in storage
		// but the git state reflects the decision. Retry will fail at git apply
		// level (duplicate change) rather than with "already decided".
		log.Printf("actions: failed to record chunk decision after git success: %v", err)
		return apperrors.Wrap(apperrors.CodeStorageSaveFailed, "failed to record chunk decision", err)
	}

	// Archive the decided chunk for undo support.
	// Build the patch that was applied so we can reverse it on undo.
	if p.decidedStore != nil {
		isNew, _ := p.isNewFile(card.File)
		patch := BuildPatch(card.File, chunk.Content, isNew)

		// First, ensure a parent DecidedCard exists for this chunk.
		// The DecidedCard stores the file path and original diff needed for undo.
		// We check if one exists first to avoid overwriting on subsequent chunk decisions.
		existingDecidedCard, _ := p.decidedStore.GetDecidedCard(cardID)
		if existingDecidedCard == nil {
			// Create the parent DecidedCard to store file info and original diff.
			// Status is set to "accepted" as a placeholder - individual chunk statuses
			// are tracked in DecidedChunk records. The Patch field stores the full
			// original diff since chunk-level patches are in DecidedChunk.
			decidedCard := &storage.DecidedCard{
				ID:           cardID,
				SessionID:    card.SessionID,
				File:         card.File,
				Patch:        card.Diff, // Store full diff as reference
				Status:       status,    // Initial status from first chunk
				DecidedAt:    time.Now(),
				OriginalDiff: card.Diff,
			}
			if err := p.decidedStore.SaveDecidedCard(decidedCard); err != nil {
				log.Printf("actions: failed to archive parent card %s for chunk undo: %v", cardID, err)
			}
		}

		decidedChunk := &storage.DecidedChunk{
			CardID:     cardID,
			ChunkIndex: chunkIndex,
			Patch:      patch,
			Status:     status,
			DecidedAt:  time.Now(),
		}

		if err := p.decidedStore.SaveDecidedChunk(decidedChunk); err != nil {
			// Log but don't fail - the decision was processed successfully
			log.Printf("actions: failed to archive decided chunk %s:%d: %v", cardID, chunkIndex, err)
		}
	}

	// Check if all chunks are decided - if so, clean up the card
	pendingCount, err := p.chunkStore.CountPendingChunks(cardID)
	if err != nil {
		log.Printf("actions: failed to count pending chunks for %s: %v", cardID, err)
	} else if pendingCount == 0 {
		// All chunks decided - delete the card and its chunks
		if err := p.store.DeleteCard(cardID); err != nil {
			log.Printf("actions: failed to delete card %s after all chunks decided: %v", cardID, err)
		} else {
			// Notify that card was removed so cached state can be cleared
			p.notifyCardRemoved(card.File)
		}
		if err := p.chunkStore.DeleteChunks(cardID); err != nil {
			log.Printf("actions: failed to delete chunks for card %s: %v", cardID, err)
		}
	}

	return nil
}

// stageFile stages a file using git add.
// This is called when the user accepts a change.
func (p *Processor) stageFile(file string) error {
	cmd := exec.Command("git", "add", "--", file)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git add failed for %s: %w (%s)", file, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// restoreFile restores a file to HEAD using git restore.
// This is called when the user rejects a change, reverting the file
// to its committed state.
func (p *Processor) restoreFile(file string) error {
	cmd := exec.Command("git", "restore", "--", file)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git restore failed for %s: %w (%s)", file, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// isNewFile checks if a file is "new" - meaning it was never committed to HEAD.
// A new file cannot be properly restored via git restore because there's no
// committed version to restore to. This includes:
// - Truly untracked files (never added)
// - Files added with "git add -N" (intent-to-add, no committed version)
// - Files staged but never committed
//
// Returns true if the file has never been committed (is new), false otherwise.
func (p *Processor) isNewFile(file string) (bool, error) {
	// Check if the file exists in HEAD (the last commit)
	// git ls-tree -r HEAD --name-only lists all files in HEAD
	// We check if our file is among them
	cmd := exec.Command("git", "ls-tree", "-r", "HEAD", "--name-only", "--", file)
	cmd.Dir = p.repoPath

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		// ls-tree can fail if there's no HEAD (empty repo) - treat as new file
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
			return true, nil
		}
		return false, fmt.Errorf("git ls-tree failed: %w", err)
	}

	// If output contains the file, it exists in HEAD (not new)
	output := strings.TrimSpace(stdout.String())
	if output != "" {
		return false, nil
	}

	// File not in HEAD - it's a new file
	return true, nil
}

// isUntrackedFile checks if a file is not in the git index.
// This is a legacy function name - for detecting restorable files, use isNewFile instead.
func (p *Processor) isUntrackedFile(file string) (bool, error) {
	return p.isNewFile(file)
}

// wasTrackedInHEAD checks if a file existed in HEAD (last commit).
// Returns true if the file was tracked, false if it was never committed.
// This is used to distinguish deletion diffs from truly stale cards.
func (p *Processor) wasTrackedInHEAD(file string) (bool, error) {
	isNew, err := p.isNewFile(file)
	if err != nil {
		return false, err
	}
	return !isNew, nil
}

// DeleteUntrackedFile deletes an untracked file and removes its card from storage.
// This is used when the user confirms deletion of a new file that cannot be restored.
//
// Safety checks:
//   - File must exist and be untracked (fails for tracked files)
//   - Card must exist in storage
//
// Returns a CodedError with appropriate error code:
//   - action.card_not_found if the card doesn't exist
//   - action.git_failed if the file is tracked (safety check)
//   - action.git_failed if the file deletion fails
func (p *Processor) DeleteUntrackedFile(cardID string) error {
	// Look up the card to get the file path
	card, err := p.store.GetCard(cardID)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to get card", err)
	}
	if card == nil {
		return apperrors.NotFound("card")
	}

	// Validate file path to prevent path traversal attacks
	if err := validateFilePath(p.repoPath, card.File); err != nil {
		return err
	}

	// Safety check: only allow deletion of untracked files
	untracked, err := p.isUntrackedFile(card.File)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeActionGitFailed, "failed to check file status", err)
	}
	if !untracked {
		return apperrors.New(apperrors.CodeActionGitFailed, "cannot delete tracked file - use reject instead")
	}

	// Build full path to the file
	filePath := filepath.Join(p.repoPath, card.File)

	// Delete the file
	if err := os.Remove(filePath); err != nil {
		if os.IsNotExist(err) {
			// File already deleted - that's fine
			log.Printf("actions: file %s already deleted", filePath)
		} else {
			return apperrors.Wrap(apperrors.CodeActionGitFailed, "failed to delete file", err)
		}
	}

	// Delete associated chunks from storage (if chunk store configured)
	if p.chunkStore != nil {
		if err := p.chunkStore.DeleteChunks(cardID); err != nil {
			// Log but don't fail - the file was already deleted
			log.Printf("actions: failed to delete chunks for card %s: %v", cardID, err)
		}
	}

	// Delete the card from storage
	if err := p.store.DeleteCard(cardID); err != nil {
		// Log but don't fail - the file was already deleted
		log.Printf("actions: failed to delete card %s after file deletion: %v", cardID, err)
	} else {
		// Notify that card was removed so cached state can be cleared
		p.notifyCardRemoved(card.File)
	}

	return nil
}

// BuildPatch constructs a git patch from a file path and chunk content.
// The chunk content should include the @@ header line and all diff lines.
// This patch can be applied using git apply.
// If isNewFile is true, uses /dev/null as the old file (for new files).
//
// Example output (existing file):
//
//	diff --git a/file.txt b/file.txt
//	--- a/file.txt
//	+++ b/file.txt
//	@@ -1,4 +1,4 @@
//	-old line
//	+new line
//	 context
//
// Example output (new file):
//
//	diff --git a/file.txt b/file.txt
//	new file mode 100644
//	--- /dev/null
//	+++ b/file.txt
//	@@ -0,0 +1,2 @@
//	+new line
func BuildPatch(file, chunkContent string, isNewFile bool) string {
	var sb strings.Builder
	sb.WriteString("diff --git a/")
	sb.WriteString(file)
	sb.WriteString(" b/")
	sb.WriteString(file)
	sb.WriteString("\n")
	if isNewFile {
		sb.WriteString("new file mode 100644\n")
		sb.WriteString("--- /dev/null\n")
	} else {
		sb.WriteString("--- a/")
		sb.WriteString(file)
		sb.WriteString("\n")
	}
	sb.WriteString("+++ b/")
	sb.WriteString(file)
	sb.WriteString("\n")
	sb.WriteString(chunkContent)
	// Ensure patch ends with newline for git apply
	if !strings.HasSuffix(chunkContent, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// StageChunk stages a single chunk using git apply --cached.
// This allows accepting individual chunks within a file without staging
// other changes in the same file. The chunk content should include the
// @@ header line from the diff.
//
// This is the programmatic equivalent of selecting a single chunk in
// git add -p and choosing 'y' to stage it.
func (p *Processor) StageChunk(file, chunkContent string) error {
	isNew, err := p.isNewFile(file)
	if err != nil {
		log.Printf("actions: failed to check if file is new for staging: %v", err)
		// Default to existing file behavior
		isNew = false
	}
	patch := BuildPatch(file, chunkContent, isNew)
	return p.applyPatch(patch, true, false)
}

// RestoreChunk reverts a single chunk using git apply --reverse.
// This allows rejecting individual chunks within a file without affecting
// other changes in the same file. The chunk content should include the
// @@ header line from the diff.
//
// This is the programmatic equivalent of reverting a specific change
// while preserving other edits in the same file.
func (p *Processor) RestoreChunk(file, chunkContent string) error {
	// For new files, reverse apply doesn't make sense (can't restore to /dev/null)
	// The file would need to be deleted instead. For now, treat as existing file.
	patch := BuildPatch(file, chunkContent, false)
	return p.applyPatch(patch, false, true)
}

// applyPatch applies a patch using git apply.
// If cached is true, applies to the index only (--cached flag).
// If reverse is true, applies the patch in reverse (--reverse flag).
func (p *Processor) applyPatch(patch string, cached, reverse bool) error {
	args := []string{"apply"}
	if cached {
		args = append(args, "--cached")
	}
	if reverse {
		args = append(args, "--reverse")
	}
	// Read patch from stdin
	args = append(args, "-")

	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	cmd.Stdin = strings.NewReader(patch)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// hashChunkContent creates a content hash for stale detection.
// Uses first 16 chars of SHA256 for sufficient uniqueness.
// This matches the hash format used in stream.hashContent().
func hashChunkContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8])
}

// ProcessUndo reverses a file-level decision and restores the card to pending.
// The card must be in the decided store (accepted, rejected, or committed).
//
// For accepted (staged) cards: reverse-apply the patch to the index (unstage).
// For committed cards: require confirmed=true, reverse-apply to working tree.
// For rejected cards: forward-apply the patch to working tree (restore the change).
//
// On success, the card is removed from decided storage and restored to active storage.
// Returns the restored card information for re-emission to clients.
//
// Returns a CodedError with appropriate error code:
//   - storage.not_found if the card doesn't exist in decided storage
//   - undo.already_pending if the card is already pending (shouldn't happen)
//   - undo.conflict if the patch fails to apply (merge conflict)
//   - undo.base_missing if the file state doesn't match patch assumptions
func (p *Processor) ProcessUndo(cardID string, confirmed bool) (*storage.DecidedCard, error) {
	// Ensure decided store is available
	if p.decidedStore == nil {
		return nil, apperrors.New(apperrors.CodeInternal, "decided store not configured")
	}

	// Retrieve the decided card
	card, err := p.decidedStore.GetDecidedCard(cardID)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.CodeInternal, "failed to get decided card", err)
	}
	if card == nil {
		return nil, apperrors.NotFound("decided card")
	}

	// Check if this card has decided chunks (per-chunk decisions were made).
	// File-level undo is not supported for per-chunk decided cards because the
	// stored patch is a fragment without headers. Return specific error so
	// mobile can use chunk-level undo instead (Phase 25.1).
	if p.decidedStore != nil {
		chunks, err := p.decidedStore.GetDecidedChunks(cardID)
		if err != nil {
			log.Printf("actions: failed to check for decided chunks: %v", err)
			// Continue anyway - if chunks exist, patch apply will fail with a clear error
		} else if len(chunks) > 0 {
			return nil, apperrors.UndoChunkOnly(cardID)
		}
	}

	// Validate file path to prevent path traversal attacks
	if err := validateFilePath(p.repoPath, card.File); err != nil {
		return nil, err
	}

	// Apply the appropriate git operation based on the card's status
	switch card.Status {
	case storage.CardAccepted:
		// Accepted (staged): reverse-apply to index to unstage
		// git apply --cached --reverse
		if err := p.applyPatch(card.Patch, true, true); err != nil {
			log.Printf("actions: undo accept failed for card %s: %v", cardID, err)
			return nil, mapPatchError(err, card.File)
		}

	case storage.CardCommitted:
		// Committed: require confirmation, reverse-apply to working tree
		if !confirmed {
			return nil, apperrors.New(apperrors.CodeUndoNotStaged,
				"committed card undo requires confirmation")
		}
		// git apply --reverse (to working tree)
		if err := p.applyPatch(card.Patch, false, true); err != nil {
			log.Printf("actions: undo commit failed for card %s: %v", cardID, err)
			return nil, mapPatchError(err, card.File)
		}

	case storage.CardRejected:
		// Rejected: forward-apply to working tree to restore the change
		// git apply (forward apply to working tree)
		if err := p.applyPatch(card.Patch, false, false); err != nil {
			log.Printf("actions: undo reject failed for card %s: %v", cardID, err)
			return nil, mapPatchError(err, card.File)
		}

	default:
		// Card is pending or unknown status - shouldn't be in decided storage
		return nil, apperrors.New(apperrors.CodeUndoAlreadyPending,
			"card is already pending")
	}

	// Git operation succeeded - restore the card to active storage
	restoredCard := &storage.ReviewCard{
		ID:        card.ID,
		SessionID: card.SessionID,
		File:      card.File,
		Diff:      card.OriginalDiff,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}

	if err := p.store.SaveCard(restoredCard); err != nil {
		// Git operation succeeded but storage save failed.
		// This leaves the system in an inconsistent state.
		// Log the error but return success since git state is correct.
		log.Printf("actions: failed to restore card %s to active storage: %v", cardID, err)
	}

	// Delete from decided storage
	if err := p.decidedStore.DeleteDecidedCard(cardID); err != nil {
		log.Printf("actions: failed to delete decided card %s after undo: %v", cardID, err)
	}

	// Also delete any decided chunks for this card
	if err := p.decidedStore.DeleteDecidedChunks(cardID); err != nil {
		log.Printf("actions: failed to delete decided chunks for %s after undo: %v", cardID, err)
	}

	log.Printf("actions: undo completed for card %s (was %s)", cardID, card.Status)

	return card, nil
}

// ProcessChunkUndo reverses a chunk-level decision and restores the chunk to pending.
// The chunk must be in the decided store (accepted, rejected, or committed).
//
// For accepted (staged) chunks: reverse-apply the patch to the index (unstage).
// For committed chunks: require confirmed=true, reverse-apply to working tree.
// For rejected chunks: forward-apply the patch to working tree (restore the change).
//
// On success, the chunk is removed from decided storage and its status is restored.
// Returns the restored chunk and parent card information for re-emission to clients.
//
// Returns a CodedError with appropriate error code:
//   - storage.not_found if the chunk doesn't exist in decided storage
//   - undo.already_pending if the chunk is already pending
//   - undo.conflict if the patch fails to apply
//   - undo.base_missing if the file state doesn't match patch assumptions
func (p *Processor) ProcessChunkUndo(cardID string, chunkIndex int, confirmed bool) (*storage.DecidedChunk, *storage.DecidedCard, error) {
	// Ensure decided store is available
	if p.decidedStore == nil {
		return nil, nil, apperrors.New(apperrors.CodeInternal, "decided store not configured")
	}

	// Retrieve the decided chunk
	chunk, err := p.decidedStore.GetDecidedChunk(cardID, chunkIndex)
	if err != nil {
		return nil, nil, apperrors.Wrap(apperrors.CodeInternal, "failed to get decided chunk", err)
	}
	if chunk == nil {
		return nil, nil, apperrors.NotFound("decided chunk")
	}

	// Also need the parent card for file path and original diff
	card, err := p.decidedStore.GetDecidedCard(cardID)
	if err != nil {
		return nil, nil, apperrors.Wrap(apperrors.CodeInternal, "failed to get parent card", err)
	}
	if card == nil {
		// Chunk exists but parent card doesn't - orphaned chunk
		return nil, nil, apperrors.NotFound("parent card")
	}

	// Validate file path to prevent path traversal attacks
	if err := validateFilePath(p.repoPath, card.File); err != nil {
		return nil, nil, err
	}

	// Apply the appropriate git operation based on the chunk's status
	switch chunk.Status {
	case storage.CardAccepted:
		// Accepted (staged): reverse-apply to index to unstage
		if err := p.applyPatch(chunk.Patch, true, true); err != nil {
			log.Printf("actions: undo chunk accept failed for %s:%d: %v", cardID, chunkIndex, err)
			return nil, nil, mapPatchError(err, card.File)
		}

	case storage.CardCommitted:
		// Committed: require confirmation, reverse-apply to working tree
		if !confirmed {
			return nil, nil, apperrors.New(apperrors.CodeUndoNotStaged,
				"committed chunk undo requires confirmation")
		}
		if err := p.applyPatch(chunk.Patch, false, true); err != nil {
			log.Printf("actions: undo chunk commit failed for %s:%d: %v", cardID, chunkIndex, err)
			return nil, nil, mapPatchError(err, card.File)
		}

	case storage.CardRejected:
		// Rejected: forward-apply to working tree to restore the change
		if err := p.applyPatch(chunk.Patch, false, false); err != nil {
			log.Printf("actions: undo chunk reject failed for %s:%d: %v", cardID, chunkIndex, err)
			return nil, nil, mapPatchError(err, card.File)
		}

	default:
		return nil, nil, apperrors.New(apperrors.CodeUndoAlreadyPending,
			"chunk is already pending")
	}

	// Git operation succeeded - restore the chunk to pending in chunk store.
	// The card and chunks may have been deleted when all chunks were decided,
	// so we need to check and recreate them if necessary.
	if p.chunkStore != nil {
		// First, check if the chunk exists in active storage
		existingChunk, err := p.chunkStore.GetChunk(cardID, chunkIndex)
		if err != nil {
			log.Printf("actions: failed to check chunk existence %s:%d: %v", cardID, chunkIndex, err)
		}

		if existingChunk == nil {
			// Chunk doesn't exist - card and chunks were likely deleted when all were decided.
			// We need to recreate the card and all chunks from the decided card's original diff.
			log.Printf("actions: chunk %s:%d not found in active storage, recreating card and chunks", cardID, chunkIndex)

			// Check if the parent card exists in active storage
			existingCard, err := p.store.GetCard(cardID)
			if err != nil {
				log.Printf("actions: failed to check card existence %s: %v", cardID, err)
			}

			if existingCard == nil {
				// Recreate the card from decided card data
				restoredCard := &storage.ReviewCard{
					ID:        card.ID,
					SessionID: card.SessionID,
					File:      card.File,
					Diff:      card.OriginalDiff,
					Status:    storage.CardPending,
					CreatedAt: time.Now(),
				}
				if err := p.store.SaveCard(restoredCard); err != nil {
					log.Printf("actions: failed to recreate card %s: %v", cardID, err)
					// Continue anyway - we'll try to recreate chunks
				} else {
					log.Printf("actions: recreated card %s in active storage", cardID)
				}
			}

			// Parse original diff to get chunk info and recreate all chunks
			chunkInfoList := stream.ParseChunkInfoFromDiff(card.OriginalDiff)
			if len(chunkInfoList) > 0 {
				// Build chunk statuses - the chunk being undone is pending,
				// others retain their decided status from decided storage
				chunkStatuses := make([]*storage.ChunkStatus, len(chunkInfoList))
				for i, ci := range chunkInfoList {
					status := storage.CardPending
					if i != chunkIndex {
						// Check if this chunk is in decided storage with a status
						if decidedChunk, _ := p.decidedStore.GetDecidedChunk(cardID, i); decidedChunk != nil {
							status = decidedChunk.Status
						}
					}
					chunkStatuses[i] = &storage.ChunkStatus{
						CardID:     cardID,
						ChunkIndex: i,
						Content:    ci.Content,
						Status:     status,
					}
				}

				if err := p.chunkStore.SaveChunks(cardID, chunkStatuses); err != nil {
					log.Printf("actions: failed to recreate chunks for %s: %v", cardID, err)
				} else {
					log.Printf("actions: recreated %d chunks for card %s", len(chunkStatuses), cardID)
				}
			}
		} else {
			// Chunk exists - just update its status to pending
			restoredDecision := &storage.ChunkDecision{
				CardID:     cardID,
				ChunkIndex: chunkIndex,
				Status:     storage.CardPending,
				Timestamp:  time.Now(),
			}
			if err := p.chunkStore.RecordChunkDecision(restoredDecision); err != nil {
				log.Printf("actions: failed to restore chunk %s:%d to pending: %v", cardID, chunkIndex, err)
			}
		}
	}

	// Delete the chunk from decided storage
	// Note: We delete the single chunk, not all chunks for the card
	if err := p.decidedStore.DeleteDecidedChunk(cardID, chunkIndex); err != nil {
		log.Printf("actions: failed to delete decided chunk %s:%d after undo: %v", cardID, chunkIndex, err)
	}

	log.Printf("actions: chunk undo completed for %s:%d (was %s)", cardID, chunkIndex, chunk.Status)

	return chunk, card, nil
}

// mapPatchError maps a git apply error to an appropriate coded error.
// This examines the error message to provide a specific error code.
func mapPatchError(err error, file string) error {
	errMsg := err.Error()

	// Check for common git apply error patterns
	if strings.Contains(errMsg, "does not apply") ||
		strings.Contains(errMsg, "patch does not apply") {
		return apperrors.New(apperrors.CodeUndoConflict,
			fmt.Sprintf("patch conflict for %s: file has changed", file))
	}

	if strings.Contains(errMsg, "No such file") ||
		strings.Contains(errMsg, "does not exist") {
		return apperrors.New(apperrors.CodeUndoBaseMissing,
			fmt.Sprintf("base missing for %s: file not found", file))
	}

	if strings.Contains(errMsg, "already exists") {
		return apperrors.New(apperrors.CodeUndoConflict,
			fmt.Sprintf("conflict for %s: file already exists", file))
	}

	// Generic patch failure
	return apperrors.New(apperrors.CodeUndoConflict,
		fmt.Sprintf("patch apply failed for %s: %v", file, err))
}
