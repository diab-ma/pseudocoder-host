// Package stream coordinates diff detection, card storage, and WebSocket streaming.
// CardStreamer ties together the diff poller, SQLite storage, and WebSocket server
// to deliver review cards to connected clients in real time.
package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/storage"
)

// ChunkInfo mirrors server.ChunkInfo for the streaming layer.
// This avoids an import cycle between stream and server packages.
type ChunkInfo struct {
	Index       int
	OldStart    int
	OldCount    int
	NewStart    int
	NewCount    int
	Offset      int    // Deprecated: use Content directly
	Length      int    // Deprecated: use Content directly
	Content     string // Raw chunk content (including @@ header)
	ContentHash string // SHA256 hash prefix for stale detection
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
	// The chunks parameter provides per-chunk metadata for granular decisions.
	// The isBinary flag indicates per-chunk actions should be disabled.
	// The isDeleted flag indicates this is a file deletion (use file-level actions).
	// The stats parameter provides size metrics for large diff warnings.
	BroadcastDiffCard(cardID, file, diffContent string, chunks []ChunkInfo, isBinary, isDeleted bool, stats *DiffStats, createdAt int64)

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

	// ChunkStore persists individual chunk records for per-chunk decisions.
	// If nil, per-chunk decisions will fail with storage.not_found errors.
	ChunkStore storage.ChunkStore

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

// ProcessChunks handles a batch of chunks from the diff poller.
// Chunks are aggregated by file, and one card is created per file.
// If chunks change for a file (different content hash), the card is updated.
//
// It also detects files that no longer have changes (staged/reverted externally)
// and removes their cards from storage, broadcasting the removal to clients.
//
// If storage fails, the file is NOT marked as seen, allowing retry on the
// next ProcessChunks call. This method is designed to be used as the OnChunks
// callback for diff.Poller.
//
// Note: This method cannot detect binary files. Use ProcessChunksRaw for full support.
func (cs *CardStreamer) ProcessChunks(chunks []*diff.Chunk) {
	// Delegate to ProcessChunksRaw with empty raw diff (disables binary detection)
	cs.ProcessChunksRaw(chunks, "")
}

// ProcessChunksRaw handles a batch of chunks with the raw diff output.
// This is an enhanced version of ProcessChunks that can detect binary files
// and calculate diff statistics for large file warnings.
//
// Use this as the OnChunksRaw callback for diff.Poller.
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
		var chunkInfoList []ChunkInfo
		var stats *DiffStats

		// For hashing, we need content that changes when the file changes.
		// For text files, use the concatenated chunk content.
		// For binary files, extract the raw diff section which includes blob hashes.
		var hashSource string

		if isBinary {
			// Binary file - no chunks, use placeholder content for display
			diffContent = diff.BinaryDiffPlaceholder
			// But hash the raw diff section (includes index line with blob hashes)
			hashSource = diff.ExtractFileDiffSection(rawDiff, file)
			if hashSource == "" {
				hashSource = diffContent // Fallback to placeholder if extraction fails
			}
		} else {
			diffContent = diff.ConcatChunkContent(fileChunkList)
			hashSource = diffContent
			chunkInfoList = buildChunkInfo(fileChunkList, diffContent)

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

		// Save individual chunks to ChunkStore for per-chunk decision support.
		// This allows ProcessChunkDecision to look up chunk content by index.
		// Skip for binary files as they have no chunks.
		if cs.config.ChunkStore != nil && !isBinary && len(chunkInfoList) > 0 {
			chunkStatuses := make([]*storage.ChunkStatus, len(chunkInfoList))
			for i, h := range chunkInfoList {
				chunkStatuses[i] = &storage.ChunkStatus{
					CardID:    cardID,
					ChunkIndex: i,
					Content:   h.Content,
					Status:    storage.CardPending,
				}
			}
			if err := cs.config.ChunkStore.SaveChunks(cardID, chunkStatuses); err != nil {
				log.Printf("Failed to save chunks for card %s: %v", cardID, err)
				if cs.config.OnError != nil {
					cs.config.OnError(err)
				}
				// Don't continue - card is saved, chunks just won't be available for per-chunk decisions
			}
		}

		// Update seen hash only after successful save
		cs.mu.Lock()
		cs.seenFileHashes[file] = contentHash
		cs.mu.Unlock()

		// Broadcast to clients (same message for new and updated cards)
		if cs.config.Broadcaster != nil {
			cs.config.Broadcaster.BroadcastDiffCard(
				card.ID,
				card.File,
				card.Diff,
				chunkInfoList,
				isBinary,
				isDeleted,
				stats,
				card.CreatedAt.UnixMilli(),
			)
		}
	}
}

// buildChunkInfo creates ChunkInfo slice from parsed chunks and concatenated diff.
// It calculates byte offsets for each chunk within the diff string and includes
// the raw content and content hash for each chunk.
// Note: ConcatChunkContent joins chunks with "\n" separators, so offsets must
// account for these separators between chunks.
func buildChunkInfo(chunks []*diff.Chunk, diffContent string) []ChunkInfo {
	result := make([]ChunkInfo, len(chunks))
	offset := 0

	for i, h := range chunks {
		length := len(h.Content)

		result[i] = ChunkInfo{
			Index:       i,
			OldStart:    h.OldStart,
			OldCount:    h.OldCount,
			NewStart:    h.NewStart,
			NewCount:    h.NewCount,
			Offset:      offset,
			Length:      length,
			Content:     h.Content,
			ContentHash: hashContent(h.Content),
		}

		offset += length
		// Account for the "\n" separator between chunks (added by ConcatChunkContent)
		if i < len(chunks)-1 {
			offset += 1
		}
	}

	return result
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

			// Delete associated chunks to avoid orphaned rows
			if cs.config.ChunkStore != nil {
				if err := cs.config.ChunkStore.DeleteChunks(card.ID); err != nil {
					log.Printf("Failed to delete chunks for stale card %s: %v", card.ID, err)
					// Continue anyway - card is already deleted
				}
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

	// Get current diff state using the poller (unstaged only, same as normal polling)
	chunks, rawDiff, err := cs.config.Poller.PollOnceRaw()
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
//
// This method also reconciles missing chunk rows: if a pending card has
// no chunks in the ChunkStore (e.g., due to database corruption or migration),
// it parses the diff content and recreates the chunk rows. This ensures
// per-chunk decisions work correctly after reconnect.
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

		var chunkInfoList []ChunkInfo
		var stats *DiffStats

		if !isBinary {
			// Parse chunk boundaries from stored diff content
			chunkInfoList = ParseChunkInfoFromDiff(card.Diff)

			// Reconcile: if ChunkStore is configured but has no chunks for this card,
			// save the parsed chunks to enable per-chunk decisions.
			cs.reconcileChunks(card.ID, chunkInfoList)

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

		cs.config.Broadcaster.BroadcastDiffCard(
			card.ID,
			card.File,
			card.Diff,
			chunkInfoList,
			isBinary,
			isDeleted,
			stats,
			card.CreatedAt.UnixMilli(),
		)
	}

	log.Printf("Streamed %d pending cards to clients", len(pendingCards))
	return nil
}

// ReconcileChunksForCard checks if chunks exist and are in sync for a card,
// rebuilding them if necessary. This is exported for use by the reconnect
// handler in host/cmd/host.go which needs to reconcile chunks before sending
// cards to reconnecting clients.
func (cs *CardStreamer) ReconcileChunksForCard(cardID string, chunkInfoList []ChunkInfo) {
	cs.reconcileChunks(cardID, chunkInfoList)
}

// reconcileChunks checks if chunks exist and are in sync for a card.
// It creates missing chunks or rebuilds stale chunks when:
// - No chunks exist (missing)
// - Chunk count doesn't match (stale)
// - Any chunk's content hash doesn't match (stale)
//
// This handles cases where a pending card exists in storage but its chunk rows
// were never created, are outdated, or were corrupted.
// Without correct chunk rows, per-chunk decisions fail with "chunk not found"
// or "chunk stale" errors.
func (cs *CardStreamer) reconcileChunks(cardID string, chunkInfoList []ChunkInfo) {
	if cs.config.ChunkStore == nil || len(chunkInfoList) == 0 {
		return
	}

	// Check if chunks already exist
	existingChunks, err := cs.config.ChunkStore.GetChunks(cardID)
	if err != nil {
		log.Printf("Warning: failed to get chunks for reconciliation (card %s): %v", cardID, err)
		return
	}

	// Determine if we need to rebuild chunks
	needsRebuild := false
	reason := ""

	if len(existingChunks) == 0 {
		// No chunks exist - need to create them
		needsRebuild = true
		reason = "missing"
	} else if len(existingChunks) != len(chunkInfoList) {
		// Chunk count mismatch - stale data
		needsRebuild = true
		reason = fmt.Sprintf("count mismatch (stored=%d, expected=%d)", len(existingChunks), len(chunkInfoList))
	} else {
		// Compare all chunks' content hashes to detect stale content
		for i := range existingChunks {
			storedHash := hashContent(existingChunks[i].Content)
			expectedHash := chunkInfoList[i].ContentHash
			if storedHash != expectedHash {
				needsRebuild = true
				reason = fmt.Sprintf("hash mismatch at chunk %d (stored=%s, expected=%s)", i, storedHash[:8], expectedHash[:8])
				break
			}
		}
	}

	if !needsRebuild {
		return
	}

	// Delete existing stale chunks before recreating
	if len(existingChunks) > 0 {
		if err := cs.config.ChunkStore.DeleteChunks(cardID); err != nil {
			log.Printf("Warning: failed to delete stale chunks for card %s: %v", cardID, err)
			return
		}
	}

	// Create chunk rows from parsed info
	log.Printf("Reconciling %d chunks for card %s (%s)", len(chunkInfoList), cardID, reason)

	chunkStatuses := make([]*storage.ChunkStatus, len(chunkInfoList))
	for i, h := range chunkInfoList {
		chunkStatuses[i] = &storage.ChunkStatus{
			CardID:     cardID,
			ChunkIndex: i,
			Content:    h.Content,
			Status:     storage.CardPending,
		}
	}

	if err := cs.config.ChunkStore.SaveChunks(cardID, chunkStatuses); err != nil {
		log.Printf("Warning: failed to reconcile chunks for card %s: %v", cardID, err)
		// Don't fail - card will still be streamed, just without per-chunk support
	}
}

// ParseChunkInfoFromDiff extracts chunk boundaries from a diff string.
// It parses @@ -old,count +new,count @@ headers to build ChunkInfo.
// This is exported for use in reconnect scenarios where chunk metadata
// must be reconstructed from stored diff content.
//
// IMPORTANT: This function must produce identical Content/ContentHash values
// to buildChunkInfo for the same diff content. ConcatChunkContent joins chunks
// with "\n" separators, so when we finalize a non-last chunk (upon seeing the
// next @@ header), we must exclude that trailing separator newline.
func ParseChunkInfoFromDiff(diffContent string) []ChunkInfo {
	var result []ChunkInfo
	lines := strings.Split(diffContent, "\n")
	offset := 0
	index := 0

	var currentChunk *ChunkInfo

	for i, line := range lines {
		lineLen := len(line)
		if i < len(lines)-1 {
			lineLen++ // Account for newline
		}

		if strings.HasPrefix(line, "@@") {
			// If we had a previous chunk, finalize its length and content.
			// Exclude the trailing separator newline - ConcatChunkContent adds
			// "\n" between chunks, but that separator is not part of the chunk content.
			if currentChunk != nil {
				currentChunk.Length = offset - currentChunk.Offset
				// Trim trailing separator newline (the "\n" between chunks)
				if currentChunk.Length > 0 && diffContent[currentChunk.Offset+currentChunk.Length-1] == '\n' {
					currentChunk.Length--
				}
				currentChunk.Content = diffContent[currentChunk.Offset : currentChunk.Offset+currentChunk.Length]
				currentChunk.ContentHash = hashContent(currentChunk.Content)
				result = append(result, *currentChunk)
			}

			// Parse the @@ header: @@ -old,count +new,count @@
			oldStart, oldCount, newStart, newCount := parseChunkHeader(line)

			currentChunk = &ChunkInfo{
				Index:    index,
				OldStart: oldStart,
				OldCount: oldCount,
				NewStart: newStart,
				NewCount: newCount,
				Offset:   offset,
			}
			index++
		}

		offset += lineLen
	}

	// Finalize the last chunk - no trailing separator to trim here
	if currentChunk != nil {
		currentChunk.Length = offset - currentChunk.Offset
		currentChunk.Content = diffContent[currentChunk.Offset : currentChunk.Offset+currentChunk.Length]
		currentChunk.ContentHash = hashContent(currentChunk.Content)
		result = append(result, *currentChunk)
	}

	// Handle edge case: empty result means single chunk from offset 0
	if len(result) == 0 && len(diffContent) > 0 {
		result = append(result, ChunkInfo{
			Index:       0,
			Offset:      0,
			Length:      len(diffContent),
			Content:     diffContent,
			ContentHash: hashContent(diffContent),
		})
	}

	return result
}

// parseChunkHeader extracts line numbers from @@ -old,count +new,count @@ header.
func parseChunkHeader(line string) (oldStart, oldCount, newStart, newCount int) {
	// Default counts to 1 if not specified
	oldCount = 1
	newCount = 1

	// Find the parts between @@ markers
	parts := strings.Split(line, "@@")
	if len(parts) < 2 {
		return
	}

	header := strings.TrimSpace(parts[1])
	// header is like "-1,3 +1,4" or "-1 +1"

	fields := strings.Fields(header)
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			// Parse -old,count
			nums := strings.Split(strings.TrimPrefix(f, "-"), ",")
			if len(nums) >= 1 {
				fmt.Sscanf(nums[0], "%d", &oldStart)
			}
			if len(nums) >= 2 {
				fmt.Sscanf(nums[1], "%d", &oldCount)
			}
		} else if strings.HasPrefix(f, "+") {
			// Parse +new,count
			nums := strings.Split(strings.TrimPrefix(f, "+"), ",")
			if len(nums) >= 1 {
				fmt.Sscanf(nums[0], "%d", &newStart)
			}
			if len(nums) >= 2 {
				fmt.Sscanf(nums[1], "%d", &newCount)
			}
		}
	}

	return
}

// ClearSeen resets the seen file hashes tracking.
// This is useful for testing or when you want to re-process all chunks.
func (cs *CardStreamer) ClearSeen() {
	cs.mu.Lock()
	cs.seenFileHashes = make(map[string]string)
	cs.mu.Unlock()
}

// hashContent creates a content hash for change detection.
// Uses first 16 chars of SHA256 for sufficient uniqueness.
func hashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8])
}
