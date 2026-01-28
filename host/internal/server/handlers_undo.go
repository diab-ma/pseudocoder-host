package server

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	// Diff package provides diff parsing and binary file detection.
	"github.com/pseudocoder/host/internal/diff"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// Stream package provides CardBroadcaster types for streaming diff cards.
	"github.com/pseudocoder/host/internal/stream"
)

// handleReviewUndo processes a review.undo message from the client.
// This reverses a previous accept/reject decision and restores the card to pending.
// Phase 20.2: Enables undo flow from mobile.
func (c *Client) handleReviewUndo(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload ReviewUndoPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.undo payload: %v", err)
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.undo missing card_id")
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	// Get the undo handler from the server
	c.server.mu.RLock()
	handler := c.server.undoHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No undo handler registered, ignoring undo for card %s", payload.CardID)
		c.sendUndoResult(payload.CardID, -1, false,
			apperrors.CodeServerHandlerMissing, "undo handler not configured")
		return
	}

	// Call the handler to undo the decision
	result, err := handler(payload.CardID, payload.Confirmed)
	if err != nil {
		log.Printf("Undo handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendUndoResult(payload.CardID, -1, false, code, message)
		return
	}

	log.Printf("Undo applied: card=%s file=%s", payload.CardID, result.File)

	// Re-emit the restored card to all clients so it reappears as pending.
	// Detect if the card is for a binary file based on the stored placeholder content.
	isBinary := result.OriginalDiff == diff.BinaryDiffPlaceholder

	// Parse chunk info from the original diff to reconstruct the card.
	// For binary files, chunks will be empty (which is correct - no per-chunk actions).
	var serverChunks []ChunkInfo
	var serverStats *DiffStats

	if !isBinary {
		chunkInfoList := stream.ParseChunkInfoFromDiff(result.OriginalDiff)
		serverChunks = make([]ChunkInfo, len(chunkInfoList))
		for i, h := range chunkInfoList {
			serverChunks[i] = ChunkInfo{
				Index:       h.Index,
				OldStart:    h.OldStart,
				OldCount:    h.OldCount,
				NewStart:    h.NewStart,
				NewCount:    h.NewCount,
				Offset:      h.Offset,
				Length:      h.Length,
				Content:     h.Content,
				ContentHash: h.ContentHash,
			}
		}

		// Calculate diff stats for the re-emitted card
		if len(result.OriginalDiff) > 0 {
			serverStats = &DiffStats{
				ByteSize:  len(result.OriginalDiff),
				LineCount: strings.Count(result.OriginalDiff, "\n") + 1,
			}
			// Count added/deleted lines
			for _, line := range strings.Split(result.OriginalDiff, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					serverStats.AddedLines++
				} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					serverStats.DeletedLines++
				}
			}
		}
	}

	// Detect deletion from stats: no added lines but has deleted lines.
	// This matches the heuristic used in StreamPendingCards.
	isDeleted := serverStats != nil && serverStats.AddedLines == 0 && serverStats.DeletedLines > 0

	// Broadcast the restored card to all clients
	c.server.Broadcast(NewDiffCardMessage(
		result.CardID,
		result.File,
		result.OriginalDiff,
		serverChunks,
		nil, // chunkGroups: not recomputed during undo
		isBinary,
		isDeleted,
		serverStats,
		time.Now().UnixMilli(),
	))

	// Broadcast success result to all clients
	c.server.Broadcast(NewUndoResultMessage(payload.CardID, -1, true, "", ""))
}

// handleChunkUndo processes a chunk.undo message from the client.
// This reverses a previous per-chunk accept/reject decision.
// Phase 20.2: Enables per-chunk undo flow from mobile.
// ContentHash is preferred over ChunkIndex for stable identity (indices can shift after staging).
func (c *Client) handleChunkUndo(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType      `json:"type"`
		ID      string           `json:"id,omitempty"`
		Payload ChunkUndoPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse chunk.undo payload: %v", err)
		c.sendUndoResult("", -1, false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("chunk.undo missing card_id")
		c.sendUndoResult("", payload.ChunkIndex, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	// Get the chunk undo handler from the server
	c.server.mu.RLock()
	handler := c.server.chunkUndoHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No chunk undo handler registered, ignoring undo for card %s chunk %d hash=%s",
			payload.CardID, payload.ChunkIndex, payload.ContentHash)
		c.sendUndoResultWithHash(payload.CardID, payload.ChunkIndex, payload.ContentHash, false,
			apperrors.CodeServerHandlerMissing, "chunk undo handler not configured")
		return
	}

	// Call the handler to undo the chunk decision
	// ContentHash is preferred for stable identity when available
	result, err := handler(payload.CardID, payload.ChunkIndex, payload.ContentHash, payload.Confirmed)
	if err != nil {
		log.Printf("Chunk undo handler error for card %s chunk %d hash=%s: %v",
			payload.CardID, payload.ChunkIndex, payload.ContentHash, err)
		code, message := apperrors.ToCodeAndMessage(err)
		// Include content_hash from request so client can clear hash-keyed pending state
		c.sendUndoResultWithHash(payload.CardID, payload.ChunkIndex, payload.ContentHash, false, code, message)
		return
	}

	// Use result's content_hash if available, otherwise fall back to request's hash.
	// This handles legacy decided chunks (pre-migration) where the stored hash is empty
	// but the client sent a hash from the current card's chunkInfo.
	contentHash := result.ContentHash
	if contentHash == "" {
		contentHash = payload.ContentHash
	}

	log.Printf("Chunk undo applied: card=%s chunk=%d hash=%s file=%s",
		payload.CardID, payload.ChunkIndex, contentHash, result.File)

	// Note: For chunk undos, we don't re-emit the full card immediately.
	// The card/chunk state transitions happen in storage, and the mobile
	// client will update its local state based on the undo result.
	// If the card needs to be re-emitted (all chunks now pending), the
	// handler should handle that via the broadcaster.

	// Broadcast success result to all clients, including content_hash for stable identity
	c.server.Broadcast(NewUndoResultMessageWithHash(
		payload.CardID,
		result.ChunkIndex,
		contentHash,
		true, "", "",
	))
}
