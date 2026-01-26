package actions

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
)

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
