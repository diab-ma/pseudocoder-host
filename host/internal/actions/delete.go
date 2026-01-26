package actions

import (
	"log"
	"os"
	"path/filepath"

	apperrors "github.com/pseudocoder/host/internal/errors"
)

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
