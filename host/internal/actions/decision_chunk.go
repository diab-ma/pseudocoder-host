package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/stream"
)

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
		CardID:     cardID,
		ChunkIndex: chunkIndex,
		Status:     status,
		Timestamp:  time.Now(),
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

		// Use the contentHash passed to the function for stable identity.
		// If not provided, compute it from chunk content for consistency.
		chunkContentHash := contentHash
		if chunkContentHash == "" && chunk != nil {
			chunkContentHash = hashChunkContent(chunk.Content)
		}

		decidedChunk := &storage.DecidedChunk{
			CardID:      cardID,
			ChunkIndex:  chunkIndex,
			ContentHash: chunkContentHash, // Stable identity for undo operations
			Patch:       patch,
			Status:      status,
			DecidedAt:   time.Now(),
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

// hashChunkContent creates a content hash for stale detection.
// Uses first 16 chars of SHA256 for sufficient uniqueness.
// This matches the hash format used in stream.hashContent().
func hashChunkContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8])
}

// recreateCardAndChunksFromDecided recreates a card and its chunks from decided storage.
// This is used by ProcessChunkUndo when the card/chunks were deleted after all chunks were decided.
func (p *Processor) recreateCardAndChunksFromDecided(cardID string, card *storage.DecidedCard, undoChunkIndex int) {
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
			if i != undoChunkIndex {
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
}
