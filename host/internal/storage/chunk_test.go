package storage

import (
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/diff"
)

// TestCardFromChunk verifies that a chunk is correctly converted to a card.
func TestCardFromChunk(t *testing.T) {
	chunk := diff.NewChunk("main.go", 1, 3, 1, 4, "@@ -1,3 +1,4 @@\n+new line")

	card := CardFromChunk(chunk, "session-123")

	if card.ID != chunk.ID {
		t.Errorf("ID = %q, want %q", card.ID, chunk.ID)
	}
	if card.SessionID != "session-123" {
		t.Errorf("SessionID = %q, want %q", card.SessionID, "session-123")
	}
	if card.File != chunk.File {
		t.Errorf("File = %q, want %q", card.File, chunk.File)
	}
	if card.Diff != chunk.Content {
		t.Errorf("Diff = %q, want %q", card.Diff, chunk.Content)
	}
	if card.Status != CardPending {
		t.Errorf("Status = %q, want %q", card.Status, CardPending)
	}

	// CreatedAt should match (within millisecond precision)
	expectedTime := time.UnixMilli(chunk.CreatedAt)
	if card.CreatedAt.Sub(expectedTime).Abs() > time.Millisecond {
		t.Errorf("CreatedAt = %v, want %v", card.CreatedAt, expectedTime)
	}
}

// TestCardFromChunkEmptySession verifies that empty session ID is preserved.
func TestCardFromChunkEmptySession(t *testing.T) {
	chunk := diff.NewChunk("test.go", 10, 5, 10, 6, "diff content")

	card := CardFromChunk(chunk, "")

	if card.SessionID != "" {
		t.Errorf("SessionID = %q, want empty string", card.SessionID)
	}
}

// TestCardsFromChunks verifies that multiple chunks are converted correctly.
func TestCardsFromChunks(t *testing.T) {
	chunks := []*diff.Chunk{
		diff.NewChunk("a.go", 1, 2, 1, 3, "diff a"),
		diff.NewChunk("b.go", 5, 3, 5, 4, "diff b"),
		diff.NewChunk("c.go", 10, 1, 10, 2, "diff c"),
	}

	cards := CardsFromChunks(chunks, "session-xyz")

	if len(cards) != len(chunks) {
		t.Fatalf("len(cards) = %d, want %d", len(cards), len(chunks))
	}

	for i, card := range cards {
		if card.ID != chunks[i].ID {
			t.Errorf("card[%d].ID = %q, want %q", i, card.ID, chunks[i].ID)
		}
		if card.File != chunks[i].File {
			t.Errorf("card[%d].File = %q, want %q", i, card.File, chunks[i].File)
		}
		if card.SessionID != "session-xyz" {
			t.Errorf("card[%d].SessionID = %q, want %q", i, card.SessionID, "session-xyz")
		}
	}
}

// TestCardsFromChunksEmpty verifies that empty input produces empty output.
func TestCardsFromChunksEmpty(t *testing.T) {
	cards := CardsFromChunks([]*diff.Chunk{}, "session-123")

	if len(cards) != 0 {
		t.Errorf("expected empty slice, got %d cards", len(cards))
	}
}

// TestCardsFromChunksNil verifies that nil input produces empty output.
func TestCardsFromChunksNil(t *testing.T) {
	cards := CardsFromChunks(nil, "session-123")

	if len(cards) != 0 {
		t.Errorf("expected empty slice, got %d cards", len(cards))
	}
}
