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

	"github.com/pseudocoder/host/internal/diff"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
)

// ProcessDecision handles an accept or reject action on a review card.
// It validates the patch or non-text change, applies the git change, verifies
// the outcome, and then records the decision inside a storage transaction.
//
// Returns a CodedError with appropriate error code:
//   - action.card_not_found if the card doesn't exist
//   - action.already_decided if another request already decided this card
//   - action.invalid if the action is not 'accept' or 'reject'
//   - action.file_deleted if the file no longer exists on disk
//   - action.untracked_file if rejecting a new file
//   - validation.failed / verification.failed / state.diverged for reliability checks
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
	if card.Status != storage.CardPending {
		return apperrors.AlreadyDecided(cardID)
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
	// vs truly stale (file never existed or was already committed as deleted).
	if !fileExists {
		wasTracked, err := p.wasTrackedInHEAD(card.File)
		if err != nil {
			log.Printf("actions: failed to check if file was tracked: %v", err)
			p.deleteStaleCard(cardID, card.File)
			return apperrors.FileDeleted(card.File)
		}
		if !wasTracked {
			p.deleteStaleCard(cardID, card.File)
			return apperrors.FileDeleted(card.File)
		}
		// File was tracked but is now deleted - handle as deletion change.
		return p.processDeletionDecision(card, action, comment)
	}

	// Detect renames before checking for new files.
	renameOld, renameNew, renameDetected, renameErr := p.detectRename(card.File)
	if renameErr != nil {
		return apperrors.Internal("failed to detect rename status", renameErr)
	}
	if renameDetected {
		return p.processRenameDecision(card, action, comment, renameOld, renameNew)
	}

	// Binary files require non-text handling.
	if card.Diff == diff.BinaryDiffPlaceholder {
		return p.processBinaryDecision(card, action, comment)
	}

	// Determine if this is a new file (not in HEAD).
	isNew, err := p.isNewFile(card.File)
	if err != nil {
		return apperrors.Wrap(apperrors.CodeInternal, "failed to check if file is new", err)
	}
	if isNew && action == "reject" {
		return apperrors.UntrackedFile(card.File)
	}

	return p.processTextDecision(card, action, comment, isNew)
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

// stagePaths stages changes (including deletions) for the provided paths.
func (p *Processor) stagePaths(paths ...string) error {
	args := append([]string{"add", "-A", "--"}, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git add -A failed for %s: %w (%s)", strings.Join(paths, ", "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// unstagePaths removes staged changes for the provided paths.
func (p *Processor) unstagePaths(paths ...string) error {
	args := append([]string{"restore", "--staged", "--"}, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git restore --staged failed for %s: %w (%s)", strings.Join(paths, ", "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// restorePaths restores working tree changes for the provided paths.
func (p *Processor) restorePaths(paths ...string) error {
	args := append([]string{"restore", "--"}, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git restore failed for %s: %w (%s)", strings.Join(paths, ", "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// getPorcelainStatus returns the git status --porcelain output for paths.
func (p *Processor) getPorcelainStatus(paths ...string) (string, error) {
	args := append([]string{"status", "--porcelain", "--"}, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git status failed for %s: %w (%s)", strings.Join(paths, ", "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// detectRename checks if the file is part of a rename in the current diff.
func (p *Processor) detectRename(file string) (string, string, bool, error) {
	for _, cached := range []bool{false, true} {
		oldPath, newPath, found, err := p.detectRenameInDiff(file, cached)
		if err != nil {
			return "", "", false, err
		}
		if found {
			return oldPath, newPath, true, nil
		}
	}
	return "", "", false, nil
}

func (p *Processor) detectRenameInDiff(file string, cached bool) (string, string, bool, error) {
	args := []string{"diff", "--name-status", "-M"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--", file)

	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", false, fmt.Errorf("git diff --name-status failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		if strings.HasPrefix(fields[0], "R") {
			oldPath := fields[1]
			newPath := fields[2]
			if file == oldPath || file == newPath {
				return oldPath, newPath, true, nil
			}
		}
	}

	return "", "", false, nil
}

func (p *Processor) deleteStaleCard(cardID, file string) {
	if delErr := p.store.DeleteCard(cardID); delErr != nil {
		log.Printf("actions: failed to delete stale card %s: %v", cardID, delErr)
	} else {
		p.notifyCardRemoved(file)
	}
}

func (p *Processor) processTextDecision(card *storage.ReviewCard, action, comment string, isNewFile bool) error {
	patch := BuildPatch(card.File, card.Diff, isNewFile)
	cached := action == "accept"
	reverse := action == "reject"

	// Pre-apply validation.
	if err := p.checkPatch(patch, cached, reverse); err != nil {
		// Detect divergence if the inverse check succeeds.
		if p.checkPatch(patch, cached, !reverse) == nil {
			return apperrors.StateDiverged([]string{card.File})
		}
		return classifyPatchCheckError(err)
	}

	apply := func() error {
		if err := p.applyPatch(patch, cached, reverse); err != nil {
			return classifyPatchApplyError(action, card.File, err)
		}
		return nil
	}
	verify := func() error {
		return p.verifyPatchApplied(patch, cached, reverse)
	}
	rollback := func() error {
		return p.applyPatch(patch, cached, !reverse)
	}

	if err := p.runDecisionTransaction(card, action, comment, apply, verify, rollback); err != nil {
		return err
	}

	p.archiveDecidedCard(card, action, isNewFile)
	p.notifyCardRemoved(card.File)
	return nil
}

func (p *Processor) processDeletionDecision(card *storage.ReviewCard, action, comment string) error {
	filePath := filepath.Join(p.repoPath, card.File)
	if action == "reject" {
		// If the file already exists, this deletion was already reverted.
		if _, err := os.Stat(filePath); err == nil {
			return apperrors.StateDiverged([]string{card.File})
		}
	}

	apply := func() error {
		if action == "accept" {
			if err := p.stagePaths(card.File); err != nil {
				return apperrors.GitFailed(action, card.File, err)
			}
			return nil
		}
		if err := p.restoreFile(card.File); err != nil {
			return apperrors.GitFailed(action, card.File, err)
		}
		return nil
	}
	verify := func() error {
		if action == "accept" {
			status, err := p.getPorcelainStatus(card.File)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(status, "D ") && !strings.HasPrefix(status, "D\t") {
				return fmt.Errorf("expected staged deletion, got status %q", status)
			}
			return nil
		}
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Errorf("expected file to be restored: %w", err)
		}
		return nil
	}
	rollback := func() error {
		if action == "accept" {
			return p.unstagePaths(card.File)
		}
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := p.runDecisionTransaction(card, action, comment, apply, verify, rollback); err != nil {
		return err
	}

	p.archiveDecidedCard(card, action, false)
	p.notifyCardRemoved(card.File)
	return nil
}

func (p *Processor) processRenameDecision(card *storage.ReviewCard, action, comment, oldPath, newPath string) error {
	apply := func() error {
		if action == "accept" {
			if err := p.stagePaths(oldPath, newPath); err != nil {
				return apperrors.GitFailed(action, newPath, err)
			}
			return nil
		}
		if err := p.restorePaths(oldPath, newPath); err != nil {
			return apperrors.GitFailed(action, newPath, err)
		}
		// Remove new path if it doesn't exist in HEAD.
		isNew, err := p.isNewFile(newPath)
		if err == nil && isNew {
			if err := os.Remove(filepath.Join(p.repoPath, newPath)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	verify := func() error {
		if action == "accept" {
			status, err := p.getCachedNameStatus(oldPath, newPath)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(status, "R") {
				return fmt.Errorf("expected staged rename, got status %q", status)
			}
			return nil
		}
		// Reject should restore old path and remove new path.
		if _, err := os.Stat(filepath.Join(p.repoPath, oldPath)); err != nil {
			return fmt.Errorf("expected old path to exist: %w", err)
		}
		if _, err := os.Stat(filepath.Join(p.repoPath, newPath)); err == nil {
			return fmt.Errorf("expected new path to be removed")
		}
		return nil
	}
	rollback := func() error {
		if action == "accept" {
			return p.unstagePaths(oldPath, newPath)
		}
		oldFull := filepath.Join(p.repoPath, oldPath)
		newFull := filepath.Join(p.repoPath, newPath)
		if _, err := os.Stat(oldFull); err == nil {
			if err := os.Rename(oldFull, newFull); err != nil {
				return err
			}
		}
		return nil
	}

	if err := p.runDecisionTransaction(card, action, comment, apply, verify, rollback); err != nil {
		return err
	}

	p.archiveDecidedCard(card, action, false)
	p.notifyCardRemoved(card.File)
	return nil
}

func (p *Processor) processBinaryDecision(card *storage.ReviewCard, action, comment string) error {
	apply := func() error {
		if action == "accept" {
			if err := p.stagePaths(card.File); err != nil {
				return apperrors.GitFailed(action, card.File, err)
			}
			return nil
		}
		if err := p.restoreFile(card.File); err != nil {
			return apperrors.GitFailed(action, card.File, err)
		}
		return nil
	}
	verify := func() error {
		status, err := p.getPorcelainStatus(card.File)
		if err != nil {
			return err
		}
		if action == "accept" {
			if status == "" || status[0] == ' ' {
				return fmt.Errorf("expected staged binary change, got status %q", status)
			}
			return nil
		}
		if status != "" {
			return fmt.Errorf("expected clean status after reject, got %q", status)
		}
		return nil
	}
	rollback := func() error {
		if action == "accept" {
			return p.unstagePaths(card.File)
		}
		return nil
	}

	if err := p.runDecisionTransaction(card, action, comment, apply, verify, rollback); err != nil {
		return err
	}

	p.archiveDecidedCard(card, action, false)
	p.notifyCardRemoved(card.File)
	return nil
}

func (p *Processor) runDecisionTransaction(
	card *storage.ReviewCard,
	action string,
	comment string,
	apply func() error,
	verify func() error,
	rollback func() error,
) error {
	storeTx, ok := p.store.(interface {
		WithTransaction(func(tx storage.Transaction) error) error
	})
	if !ok {
		return apperrors.Internal("transactional store not configured", nil)
	}

	status := storage.CardAccepted
	if action == "reject" {
		status = storage.CardRejected
	}

	applied := false
	rolledBack := false

	tryRollback := func(context string) {
		if rolledBack || rollback == nil {
			return
		}
		if rbErr := rollback(); rbErr != nil {
			log.Printf("actions: rollback failed (%s): %v", context, rbErr)
		} else {
			rolledBack = true
		}
	}

	txErr := storeTx.WithTransaction(func(tx storage.Transaction) error {
		if err := apply(); err != nil {
			return err
		}
		applied = true

		if verify != nil {
			if err := verify(); err != nil {
				tryRollback("verification failure")
				return apperrors.VerificationFailed(err.Error())
			}
		}

		decision := &storage.Decision{
			CardID:    card.ID,
			Status:    status,
			Comment:   comment,
			Timestamp: time.Now(),
		}

		if err := tx.RecordDecision(decision); err != nil {
			tryRollback("record decision failure")
			return mapDecisionStorageError(err, card.ID)
		}

		if err := tx.DeleteCard(card.ID); err != nil {
			tryRollback("delete card failure")
			return apperrors.Wrap(apperrors.CodeStorageSaveFailed, "failed to delete card", err)
		}

		return nil
	})

	if txErr != nil {
		if applied && !rolledBack {
			tryRollback("transaction failure")
		}
		if apperrors.GetCode(txErr) == apperrors.CodeUnknown {
			return apperrors.Wrap(apperrors.CodeStorageSaveFailed, "decision transaction failed", txErr)
		}
		return txErr
	}

	return nil
}

func (p *Processor) archiveDecidedCard(card *storage.ReviewCard, action string, isNewFile bool) {
	if p.decidedStore == nil {
		return
	}

	status := storage.CardAccepted
	if action == "reject" {
		status = storage.CardRejected
	}

	patch := BuildPatch(card.File, card.Diff, isNewFile)
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
		log.Printf("actions: failed to archive decided card %s: %v", card.ID, err)
	}
}

func (p *Processor) getCachedNameStatus(oldPath, newPath string) (string, error) {
	args := []string{"diff", "--cached", "--name-status", "-M", "--", oldPath, newPath}
	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff --cached --name-status failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func classifyPatchCheckError(err error) error {
	if err == nil {
		return nil
	}
	if isPatchConflict(err) {
		return apperrors.ConflictDetected(err.Error())
	}
	return apperrors.ValidationFailed(err.Error())
}

func classifyPatchApplyError(action, file string, err error) error {
	if err == nil {
		return nil
	}
	if isPatchConflict(err) {
		return apperrors.ConflictDetected(err.Error())
	}
	return apperrors.GitFailed(action, file, err)
}

func isPatchConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "patch failed") || strings.Contains(msg, "conflict")
}

func mapDecisionStorageError(err error, cardID string) error {
	if errors.Is(err, storage.ErrCardNotFound) {
		return apperrors.NotFound("card")
	}
	if errors.Is(err, storage.ErrAlreadyDecided) {
		return apperrors.AlreadyDecided(cardID)
	}
	return apperrors.Wrap(apperrors.CodeStorageSaveFailed, "failed to record decision", err)
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
