package actions

import (
	"fmt"
	"path/filepath"
	"strings"

	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
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
