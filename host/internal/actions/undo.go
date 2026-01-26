package actions

import (
	"fmt"
	"log"
	"strings"
	"time"

	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
)

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
			p.recreateCardAndChunksFromDecided(cardID, card, chunkIndex)
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
