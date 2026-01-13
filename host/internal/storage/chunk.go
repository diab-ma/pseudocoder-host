package storage

import (
	"time"

	"github.com/pseudocoder/host/internal/diff"
)

// CardFromChunk creates a ReviewCard from a diff.Chunk.
// The card inherits the chunk's ID for deduplication and uses the
// chunk's content as the diff. The session ID can be empty for
// standalone cards.
func CardFromChunk(chunk *diff.Chunk, sessionID string) *ReviewCard {
	return &ReviewCard{
		ID:        chunk.ID,
		SessionID: sessionID,
		File:      chunk.File,
		Diff:      chunk.Content,
		Status:    CardPending,
		CreatedAt: time.UnixMilli(chunk.CreatedAt),
	}
}

// CardsFromChunks converts a slice of chunks to review cards.
// This is a convenience wrapper around CardFromChunk.
func CardsFromChunks(chunks []*diff.Chunk, sessionID string) []*ReviewCard {
	cards := make([]*ReviewCard, len(chunks))
	for i, h := range chunks {
		cards[i] = CardFromChunk(h, sessionID)
	}
	return cards
}
