// Package stream coordinates diff detection, card storage, and WebSocket streaming.
// CardStreamer ties together the diff poller, SQLite storage, and WebSocket server
// to deliver review cards to connected clients in real time.
package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/storage"
)

// SemanticGroupInfo describes a semantic group of related chunks.
// Semantic groups are determined by code analysis (C2) rather than
// proximity. This type mirrors server.SemanticGroupInfo for the
// streaming layer to avoid import cycles.
type SemanticGroupInfo struct {
	// GroupID is the deterministic identifier (e.g., "sg-" + 12 hex).
	GroupID string

	// Label is the display label for the group.
	Label string

	// Kind is the semantic classification (e.g., "import", "function").
	Kind string

	// LineStart is the starting line number of the group.
	LineStart int

	// LineEnd is the ending line number of the group.
	LineEnd int

	// ChunkIndexes lists the chunk indices belonging to this group.
	ChunkIndexes []int

	// RiskLevel is an optional risk assessment for this group.
	RiskLevel string
}

// DiffStats mirrors server.DiffStats for the streaming layer.
type DiffStats struct {
	ByteSize     int
	LineCount    int
	AddedLines   int
	DeletedLines int
}

// CardBroadcaster defines the interface for broadcasting cards to clients.
// This abstraction allows testing without a real WebSocket server.
type CardBroadcaster interface {
	// BroadcastDiffCard sends a card to all connected clients.
	// The semanticGroups parameter provides semantic grouping metadata (nil when disabled).
	// The isBinary flag indicates this is a binary file (file-level actions only).
	// The isDeleted flag indicates this is a file deletion (use file-level actions).
	// The stats parameter provides size metrics for large diff warnings.
	BroadcastDiffCard(cardID, file, diffContent string, semanticGroups []SemanticGroupInfo, isBinary, isDeleted bool, stats *DiffStats, createdAt int64)

	// BroadcastCardRemoved notifies clients that a card was removed.
	// This is called when changes are staged/reverted externally.
	BroadcastCardRemoved(cardID string)
}

// CardStreamerConfig holds configuration for the card streamer.
type CardStreamerConfig struct {
	// Poller monitors the git repository for changes.
	Poller *diff.Poller

	// Store persists cards to SQLite for restart safety.
	Store storage.CardStore

	// Broadcaster sends cards to connected WebSocket clients.
	Broadcaster CardBroadcaster

	// SessionID identifies the current session for card association.
	SessionID string

	// OnError is called when an error occurs during streaming.
	// If nil, errors are logged but not propagated.
	OnError func(err error)
}

// CardStreamer coordinates diff polling, storage, and streaming.
// It listens for chunk changes from the diff poller, converts them to cards,
// persists them to storage, and broadcasts them to connected clients.
//
// Cards are created on a per-file basis: all chunks for a file are consolidated
// into a single card. When chunks change, the card is updated and re-broadcast.
type CardStreamer struct {
	config CardStreamerConfig

	// mu protects the seenFileHashes map from concurrent access.
	mu sync.Mutex

	// seenFileHashes tracks content hash per file to detect changes.
	// Key is file path, value is hash of concatenated chunk content.
	// This allows detecting when chunks change for a file (requiring re-broadcast).
	seenFileHashes map[string]string

	// running indicates whether the streamer is active.
	running bool
}

// NewCardStreamer creates a new card streamer with the given configuration.
// Call Start() to begin processing chunks and streaming cards.
func NewCardStreamer(config CardStreamerConfig) *CardStreamer {
	return &CardStreamer{
		config:         config,
		seenFileHashes: make(map[string]string),
	}
}

// Start begins processing chunks from the diff poller.
// It loads existing pending cards from storage and marks them as seen,
// then listens for new chunks and streams them as cards.
func (cs *CardStreamer) Start() error {
	cs.mu.Lock()
	if cs.running {
		cs.mu.Unlock()
		return nil
	}
	cs.running = true
	cs.mu.Unlock()

	// Load existing pending cards to populate content hashes.
	// This prevents re-broadcasting unchanged cards on restart.
	if cs.config.Store != nil {
		pendingCards, err := cs.config.Store.ListPending()
		if err != nil {
			log.Printf("Warning: failed to load pending cards: %v", err)
		} else {
			cs.mu.Lock()
			for _, card := range pendingCards {
				// Store hash of existing card's diff content
				cs.seenFileHashes[card.File] = hashContent(card.Diff)
			}
			cs.mu.Unlock()
			log.Printf("Loaded %d pending cards from storage", len(pendingCards))
		}
	}

	// The poller's OnChunks callback is already set up in config.
	// We just need to start it.
	if cs.config.Poller != nil {
		cs.config.Poller.Start()
	}

	return nil
}

// Stop halts the card streamer and underlying diff poller.
func (cs *CardStreamer) Stop() {
	cs.mu.Lock()
	if !cs.running {
		cs.mu.Unlock()
		return
	}
	cs.running = false
	cs.mu.Unlock()

	if cs.config.Poller != nil {
		cs.config.Poller.Stop()
	}
}

// ClearFileHash removes the cached content hash for a file.
// This should be called when a card is removed via decision or deletion,
// so that if identical changes appear again, they will be detected as new.
func (cs *CardStreamer) ClearFileHash(file string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.seenFileHashes, file)
}

// ProcessChunksRaw handles a batch of chunks with the raw diff output.
// It aggregates chunks by file, creates one card per file, and detects
// binary files and calculates diff statistics for large file warnings.
//
// If storage fails, the file is NOT marked as seen, allowing retry on the
// next call. Use this as the OnChunksRaw callback for diff.Poller.
func (cs *CardStreamer) ProcessChunksRaw(chunks []*diff.Chunk, rawDiff string) {
	// Aggregate chunks by file
	fileChunks := diff.AggregateByFile(chunks)

	// Detect binary files from raw diff output
	binaryFiles := diff.ParseBinaryFiles(rawDiff)

	// Detect deleted files from raw diff output
	deletedFiles := diff.ParseDeletedFiles(rawDiff)

	// Build set of current files for stale detection (includes binary files)
	currentFiles := make(map[string]bool)
	for file := range fileChunks {
		currentFiles[file] = true
	}
	for file := range binaryFiles {
		currentFiles[file] = true
	}

	// Check for stale cards (files no longer in diff)
	cs.removeStaleFileCards(currentFiles)

	// Build ordered file list from input chunks (preserves input order)
	var orderedFiles []string
	seenFiles := make(map[string]bool)
	for _, h := range chunks {
		if !seenFiles[h.File] {
			orderedFiles = append(orderedFiles, h.File)
			seenFiles[h.File] = true
		}
	}
	// Add binary files that weren't in chunks (they don't have chunk output)
	for file := range binaryFiles {
		if !seenFiles[file] {
			orderedFiles = append(orderedFiles, file)
			seenFiles[file] = true
		}
	}

	// Process each file in input order
	for _, file := range orderedFiles {
		isBinary := binaryFiles[file]
		isDeleted := deletedFiles[file]
		fileChunkList := fileChunks[file]
		cardID := diff.FileCardID(file)

		var diffContent string
		var stats *DiffStats

		// For hashing, we need content that changes when the file changes.
		// For text files, use the concatenated chunk content.
		// For binary files, extract the raw diff section which includes blob hashes.
		var hashSource string

		if isBinary {
			// Binary file - use placeholder content for display
			diffContent = diff.BinaryDiffPlaceholder
			// But hash the raw diff section (includes index line with blob hashes)
			hashSource = diff.ExtractFileDiffSection(rawDiff, file)
			if hashSource == "" {
				hashSource = diffContent // Fallback to placeholder if extraction fails
			}
		} else {
			diffContent = diff.ConcatChunkContent(fileChunkList)
			hashSource = diffContent

			// Calculate diff stats for large file warnings
			diffStats := diff.CalculateDiffStats(diffContent)
			stats = &DiffStats{
				ByteSize:     diffStats.ByteSize,
				LineCount:    diffStats.LineCount,
				AddedLines:   diffStats.AddedLines,
				DeletedLines: diffStats.DeletedLines,
			}
		}

		contentHash := hashContent(hashSource)

		// Check if content has changed since last broadcast
		cs.mu.Lock()
		prevHash := cs.seenFileHashes[file]
		cs.mu.Unlock()

		if prevHash == contentHash {
			// Content unchanged - skip
			continue
		}

		// Create or update card
		card := &storage.ReviewCard{
			ID:        cardID,
			SessionID: cs.config.SessionID,
			File:      file,
			Diff:      diffContent,
			Status:    storage.CardPending,
			CreatedAt: time.Now(),
		}

		// Save to storage (INSERT OR REPLACE handles updates)
		if cs.config.Store != nil {
			if err := cs.config.Store.SaveCard(card); err != nil {
				log.Printf("Failed to save file card %s: %v", cardID, err)
				if cs.config.OnError != nil {
					cs.config.OnError(err)
				}
				continue
			}
		}

		// Update seen hash only after successful save
		cs.mu.Lock()
		cs.seenFileHashes[file] = contentHash
		cs.mu.Unlock()

		// Semantic enrichment (non-blocking).
		semanticGroups := EnrichFileWithSemantics(cardID, file, diffContent, isBinary, isDeleted)

		// Broadcast to clients (same message for new and updated cards)
		if cs.config.Broadcaster != nil {
			cs.config.Broadcaster.BroadcastDiffCard(
				card.ID,
				card.File,
				card.Diff,
				semanticGroups,
				isBinary,
				isDeleted,
				stats,
				card.CreatedAt.UnixMilli(),
			)
		}
	}
}

// removeStaleFileCards checks pending cards against current files and removes
// cards for files that no longer have changes (staged/reverted externally).
func (cs *CardStreamer) removeStaleFileCards(currentFiles map[string]bool) {
	if cs.config.Store == nil {
		return
	}

	pendingCards, err := cs.config.Store.ListPending()
	if err != nil {
		log.Printf("Failed to list pending cards for stale check: %v", err)
		return
	}

	for _, card := range pendingCards {
		if !currentFiles[card.File] {
			// File no longer has changes - was staged/reverted externally
			log.Printf("Removing stale card %s (file: %s)", card.ID, card.File)

			if err := cs.config.Store.DeleteCard(card.ID); err != nil {
				log.Printf("Failed to delete stale card %s: %v", card.ID, err)
				continue
			}

			// Remove from seen hashes so it can be re-added if changes reappear
			cs.mu.Lock()
			delete(cs.seenFileHashes, card.File)
			cs.mu.Unlock()

			// Notify clients
			if cs.config.Broadcaster != nil {
				cs.config.Broadcaster.BroadcastCardRemoved(card.ID)
			}
		}
	}
}

// ValidateAndCleanStaleCards removes cards for files no longer in the unstaged diff.
// Call this before sending pending cards on reconnect to avoid stale cards.
// Uses the configured Poller to get the current diff state.
func (cs *CardStreamer) ValidateAndCleanStaleCards() error {
	if cs.config.Poller == nil {
		return nil // No poller configured, skip validation
	}

	// Get current diff state without advancing the poller's hash.
	// This keeps the next poll tick from thinking nothing changed.
	chunks, rawDiff, err := cs.config.Poller.SnapshotRaw()
	if err != nil {
		return fmt.Errorf("poll failed: %w", err)
	}

	// Build currentFiles map (same logic as ProcessChunksRaw)
	fileChunks := diff.AggregateByFile(chunks)
	binaryFiles := diff.ParseBinaryFiles(rawDiff)

	currentFiles := make(map[string]bool)
	for file := range fileChunks {
		currentFiles[file] = true
	}
	for file := range binaryFiles {
		currentFiles[file] = true
	}

	// Remove stale cards (broadcasts removal to clients, cleans DB and caches)
	cs.removeStaleFileCards(currentFiles)
	return nil
}

// StreamPendingCards sends all pending cards to clients.
// This is useful for newly connected clients who need to catch up
// on existing cards. Call this when a new client connects.
func (cs *CardStreamer) StreamPendingCards() error {
	if cs.config.Store == nil || cs.config.Broadcaster == nil {
		return nil
	}

	pendingCards, err := cs.config.Store.ListPending()
	if err != nil {
		return err
	}

	for _, card := range pendingCards {
		// Detect if card is for a binary file based on stored placeholder content
		isBinary := card.Diff == diff.BinaryDiffPlaceholder

		var stats *DiffStats

		if !isBinary {
			// Calculate diff stats for large file warnings
			diffStats := diff.CalculateDiffStats(card.Diff)
			stats = &DiffStats{
				ByteSize:     diffStats.ByteSize,
				LineCount:    diffStats.LineCount,
				AddedLines:   diffStats.AddedLines,
				DeletedLines: diffStats.DeletedLines,
			}
		}

		// Detect deletion from stats: no added lines but has deleted lines
		// This is a heuristic for pending cards loaded from storage
		isDeleted := stats != nil && stats.AddedLines == 0 && stats.DeletedLines > 0

		// Semantic enrichment (non-blocking).
		semanticGroups := EnrichFileWithSemantics(card.ID, card.File, card.Diff, isBinary, isDeleted)

		cs.config.Broadcaster.BroadcastDiffCard(
			card.ID,
			card.File,
			card.Diff,
			semanticGroups,
			isBinary,
			isDeleted,
			stats,
			card.CreatedAt.UnixMilli(),
		)
	}

	log.Printf("Streamed %d pending cards to clients", len(pendingCards))
	return nil
}

// hashContent creates a content hash for change detection.
// Uses first 16 chars of SHA256 for sufficient uniqueness.
func hashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8])
}

