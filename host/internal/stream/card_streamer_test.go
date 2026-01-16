package stream

import (
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/storage"
)

// mockBroadcaster records cards that were broadcast for testing.
// It implements the CardBroadcaster interface.
type mockBroadcaster struct {
	mu           sync.Mutex
	cards        []broadcastedCard
	removedCards []string
}

type broadcastedCard struct {
	CardID      string
	File        string
	Diff        string
	Chunks      []ChunkInfo
	ChunkGroups []ChunkGroupInfo
	IsBinary    bool
	IsDeleted   bool
	Stats       *DiffStats
	CreatedAt   int64
}

func newMockBroadcaster() *mockBroadcaster {
	return &mockBroadcaster{
		cards:        make([]broadcastedCard, 0),
		removedCards: make([]string, 0),
	}
}

func (m *mockBroadcaster) BroadcastDiffCard(cardID, file, diffContent string, chunks []ChunkInfo, chunkGroups []ChunkGroupInfo, isBinary, isDeleted bool, stats *DiffStats, createdAt int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cards = append(m.cards, broadcastedCard{
		CardID:      cardID,
		File:        file,
		Diff:        diffContent,
		Chunks:      chunks,
		ChunkGroups: chunkGroups,
		IsBinary:    isBinary,
		IsDeleted:   isDeleted,
		Stats:       stats,
		CreatedAt:   createdAt,
	})
}

func (m *mockBroadcaster) BroadcastCardRemoved(cardID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removedCards = append(m.removedCards, cardID)
}

func (m *mockBroadcaster) getCards() []broadcastedCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]broadcastedCard, len(m.cards))
	copy(result, m.cards)
	return result
}

func (m *mockBroadcaster) getRemovedCards() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.removedCards))
	copy(result, m.removedCards)
	return result
}

func (m *mockBroadcaster) clearCards() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cards = make([]broadcastedCard, 0)
}

// TestCardStreamer_ProcessChunks tests that chunks are converted to cards,
// saved to storage, and broadcast to clients.
func TestCardStreamer_ProcessChunks(t *testing.T) {
	// Create an in-memory store for testing
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create test chunks
	chunks := []*diff.Chunk{
		diff.NewChunk("file1.go", 10, 5, 10, 7, "+added line\n-removed line"),
		diff.NewChunk("file2.go", 20, 3, 20, 4, "+another change"),
	}

	// Process the chunks
	streamer.ProcessChunks(chunks)

	// Verify cards were broadcast
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 2 {
		t.Errorf("expected 2 broadcast cards, got %d", len(broadcastedCards))
	}

	// Verify cards were saved to storage
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 2 {
		t.Errorf("expected 2 stored cards, got %d", len(allCards))
	}

	// Verify card properties
	for i, card := range allCards {
		if card.File != chunks[i].File {
			t.Errorf("card %d file mismatch: expected %s, got %s", i, chunks[i].File, card.File)
		}
		if card.Diff != chunks[i].Content {
			t.Errorf("card %d diff mismatch: expected %s, got %s", i, chunks[i].Content, card.Diff)
		}
		if card.Status != storage.CardPending {
			t.Errorf("card %d status mismatch: expected %s, got %s", i, storage.CardPending, card.Status)
		}
		if card.SessionID != "test-session" {
			t.Errorf("card %d session mismatch: expected test-session, got %s", i, card.SessionID)
		}
	}
}

// TestCardStreamer_DeduplicatesCards tests that the same chunk is not
// processed multiple times.
func TestCardStreamer_DeduplicatesCards(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create a chunk and process it twice
	chunk := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	chunks := []*diff.Chunk{chunk}

	streamer.ProcessChunks(chunks)
	streamer.ProcessChunks(chunks)

	// Should only have one broadcast and one stored card
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card (deduplicated), got %d", len(broadcastedCards))
	}

	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Errorf("expected 1 stored card (deduplicated), got %d", len(allCards))
	}
}

// TestCardStreamer_LoadsPendingOnStart tests that existing pending cards
// are marked as seen on startup to avoid re-broadcasting.
func TestCardStreamer_LoadsPendingOnStart(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Pre-populate store with a pending card
	existingCard := &storage.ReviewCard{
		ID:        "existing-card-id",
		SessionID: "old-session",
		File:      "old-file.go",
		Diff:      "+old change",
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(existingCard); err != nil {
		t.Fatalf("failed to save existing card: %v", err)
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Start the streamer (loads existing cards)
	if err := streamer.Start(); err != nil {
		t.Fatalf("failed to start streamer: %v", err)
	}
	defer streamer.Stop()

	// Try to process a chunk with the same ID as the existing card
	// (simulating the poller re-emitting a known chunk)
	fakeChunk := &diff.Chunk{
		ID:        "existing-card-id",
		File:      "old-file.go",
		Content:   "+old change",
		CreatedAt: time.Now().UnixMilli(),
	}

	streamer.ProcessChunks([]*diff.Chunk{fakeChunk})

	// Should not broadcast since the card was already pending
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 0 {
		t.Errorf("expected 0 broadcast cards (existing pending skipped), got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_StreamPendingCards tests that all pending cards can be
// streamed to clients (useful for reconnecting clients).
func TestCardStreamer_StreamPendingCards(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Pre-populate store with pending cards
	cards := []*storage.ReviewCard{
		{
			ID:        "card-1",
			File:      "file1.go",
			Diff:      "+change1",
			Status:    storage.CardPending,
			CreatedAt: time.Now(),
		},
		{
			ID:        "card-2",
			File:      "file2.go",
			Diff:      "+change2",
			Status:    storage.CardPending,
			CreatedAt: time.Now(),
		},
		{
			ID:        "card-3",
			File:      "file3.go",
			Diff:      "+change3",
			Status:    storage.CardAccepted, // Not pending
			CreatedAt: time.Now(),
		},
	}
	for _, card := range cards {
		if err := store.SaveCard(card); err != nil {
			t.Fatalf("failed to save card: %v", err)
		}
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Stream pending cards
	if err := streamer.StreamPendingCards(); err != nil {
		t.Fatalf("failed to stream pending cards: %v", err)
	}

	// Should broadcast only the 2 pending cards
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 2 {
		t.Errorf("expected 2 broadcast cards (pending only), got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_ClearSeen tests that clearing seen cards allows
// re-processing of chunks.
func TestCardStreamer_ClearSeen(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Process a chunk
	chunk := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	streamer.ProcessChunks([]*diff.Chunk{chunk})

	// Clear seen cards
	streamer.ClearSeen()

	// Process the same chunk again - should be broadcast again
	streamer.ProcessChunks([]*diff.Chunk{chunk})

	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 2 {
		t.Errorf("expected 2 broadcast cards after ClearSeen, got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_NilStore tests that the streamer works without storage
// (broadcast only mode).
func TestCardStreamer_NilStore(t *testing.T) {
	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       nil, // No storage
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Process chunks
	chunk := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	streamer.ProcessChunks([]*diff.Chunk{chunk})

	// Should still broadcast
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card (no store), got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_NilBroadcaster tests that the streamer works without
// a broadcaster (storage only mode).
func TestCardStreamer_NilBroadcaster(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: nil, // No broadcaster
		SessionID:   "test-session",
	})

	// Process chunks
	chunk := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	streamer.ProcessChunks([]*diff.Chunk{chunk})

	// Should still save to storage
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Errorf("expected 1 stored card (no broadcaster), got %d", len(allCards))
	}
}

// TestCardStreamer_StartStop tests that Start and Stop are idempotent.
func TestCardStreamer_StartStop(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Double start should not panic
	if err := streamer.Start(); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	if err := streamer.Start(); err != nil {
		t.Fatalf("second start failed: %v", err)
	}

	// Double stop should not panic
	streamer.Stop()
	streamer.Stop()
}

// TestCardStreamer_CardPayloadMatchesProtocol tests that broadcast cards
// have the correct payload structure matching the WebSocket protocol.
func TestCardStreamer_CardPayloadMatchesProtocol(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create a chunk with known values
	chunk := diff.NewChunk("src/main.go", 100, 10, 100, 12, "@@ -100,10 +100,12 @@\n context\n+added line")

	streamer.ProcessChunks([]*diff.Chunk{chunk})

	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(broadcastedCards))
	}

	card := broadcastedCards[0]

	// Verify all required fields are present
	if card.CardID == "" {
		t.Error("card_id should not be empty")
	}
	if card.File != "src/main.go" {
		t.Errorf("file mismatch: expected src/main.go, got %s", card.File)
	}
	if card.Diff == "" {
		t.Error("diff should not be empty")
	}
	if card.CreatedAt <= 0 {
		t.Error("created_at should be a positive Unix millisecond timestamp")
	}

	// Verify timestamp is reasonable (within last minute)
	now := time.Now().UnixMilli()
	if card.CreatedAt > now || card.CreatedAt < now-60000 {
		t.Errorf("created_at timestamp seems wrong: %d (now: %d)", card.CreatedAt, now)
	}
}

// failingStore is a mock CardStore that fails SaveCard a configurable number
// of times before succeeding. Used to test retry behavior.
type failingStore struct {
	storage.CardStore     // Embed real store for other methods
	failCount         int // Number of times to fail before succeeding
	callCount         int // Number of SaveCard calls made
	mu                sync.Mutex
}

func newFailingStore(realStore storage.CardStore, failCount int) *failingStore {
	return &failingStore{
		CardStore: realStore,
		failCount: failCount,
	}
}

func (f *failingStore) SaveCard(card *storage.ReviewCard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	if f.callCount <= f.failCount {
		return storage.ErrCardNotFound // Use existing error type for simplicity
	}
	return f.CardStore.SaveCard(card)
}

func (f *failingStore) getCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// TestCardStreamer_RetryAfterSaveFailure tests that when SaveCard fails,
// the chunk is NOT marked as seen and can be retried on the next ProcessChunks call.
func TestCardStreamer_RetryAfterSaveFailure(t *testing.T) {
	// Create a real store for the failing store to delegate to
	realStore, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer realStore.Close()

	// Create a failing store that fails the first SaveCard call
	store := newFailingStore(realStore, 1)

	broadcaster := newMockBroadcaster()

	var capturedErrors []error
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
		OnError: func(err error) {
			capturedErrors = append(capturedErrors, err)
		},
	})

	// Create a chunk
	chunk := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	chunks := []*diff.Chunk{chunk}

	// First call - should fail to save
	streamer.ProcessChunks(chunks)

	// Verify: OnError was called, no broadcast, no card in storage
	if len(capturedErrors) != 1 {
		t.Errorf("expected 1 error, got %d", len(capturedErrors))
	}
	if len(broadcaster.getCards()) != 0 {
		t.Errorf("expected 0 broadcast cards after failure, got %d", len(broadcaster.getCards()))
	}
	if store.getCallCount() != 1 {
		t.Errorf("expected 1 SaveCard call, got %d", store.getCallCount())
	}

	// Second call - same chunk should be retried and succeed
	streamer.ProcessChunks(chunks)

	// Verify: card now saved and broadcast
	if store.getCallCount() != 2 {
		t.Errorf("expected 2 SaveCard calls (retry), got %d", store.getCallCount())
	}
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card after retry, got %d", len(broadcastedCards))
	}

	// Verify card was saved to real store
	allCards, err := realStore.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Errorf("expected 1 stored card after retry, got %d", len(allCards))
	}

	// Third call - chunk should now be seen, no retry
	streamer.ProcessChunks(chunks)
	if store.getCallCount() != 2 {
		t.Errorf("expected no additional SaveCard calls (already seen), got %d", store.getCallCount())
	}
}

// TestCardStreamer_MultipleCardsStreamedInOrder tests that multiple cards
// are streamed in deterministic order.
func TestCardStreamer_MultipleCardsStreamedInOrder(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create multiple chunks
	chunks := []*diff.Chunk{
		diff.NewChunk("a.go", 1, 1, 1, 2, "+first"),
		diff.NewChunk("b.go", 1, 1, 1, 2, "+second"),
		diff.NewChunk("c.go", 1, 1, 1, 2, "+third"),
	}

	streamer.ProcessChunks(chunks)

	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 3 {
		t.Fatalf("expected 3 broadcast cards, got %d", len(broadcastedCards))
	}

	// Verify order matches input order
	expectedFiles := []string{"a.go", "b.go", "c.go"}
	for i, card := range broadcastedCards {
		if card.File != expectedFiles[i] {
			t.Errorf("card %d file mismatch: expected %s, got %s", i, expectedFiles[i], card.File)
		}
	}
}

// listPendingFailingStore is a mock that fails ListPending.
// Used to test Start() error handling.
type listPendingFailingStore struct {
	storage.CardStore
	listPendingErr error
}

func (s *listPendingFailingStore) ListPending() ([]*storage.ReviewCard, error) {
	return nil, s.listPendingErr
}

// TestCardStreamer_StartHandlesListPendingFailure tests that Start() continues
// even if ListPending fails, logging a warning but not returning an error.
func TestCardStreamer_StartHandlesListPendingFailure(t *testing.T) {
	// Create a real store for delegation
	realStore, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer realStore.Close()

	// Create a store that fails ListPending
	failingListStore := &listPendingFailingStore{
		CardStore:      realStore,
		listPendingErr: storage.ErrCardNotFound,
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       failingListStore,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Start should succeed despite ListPending failure (logs warning)
	err = streamer.Start()
	if err != nil {
		t.Fatalf("Start() should not fail when ListPending fails: %v", err)
	}
	defer streamer.Stop()

	// Processing new chunks should still work
	chunk := diff.NewChunk("file.go", 1, 1, 1, 2, "+new")
	streamer.ProcessChunks([]*diff.Chunk{chunk})

	// Card should be broadcast (ListPending failure doesn't block processing)
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card, got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_DuplicateIDsInSingleBatch tests that duplicate chunk IDs
// within a single ProcessChunks batch are handled correctly (only first processed).
func TestCardStreamer_DuplicateIDsInSingleBatch(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create chunks with the same ID (same file, line, content = same hash)
	// NewChunk generates ID from file:oldStart:newStart:content
	chunk1 := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line")
	chunk2 := diff.NewChunk("file.go", 10, 5, 10, 7, "+added line") // Same ID as chunk1

	// Verify they have the same ID
	if chunk1.ID != chunk2.ID {
		t.Fatalf("expected same IDs, got %s and %s", chunk1.ID, chunk2.ID)
	}

	// Process batch with duplicate IDs
	streamer.ProcessChunks([]*diff.Chunk{chunk1, chunk2})

	// Only one should be processed (first one marks as seen, second is skipped)
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card (duplicate skipped), got %d", len(broadcastedCards))
	}

	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Errorf("expected 1 stored card (duplicate skipped), got %d", len(allCards))
	}
}

// TestBuildChunkInfo_MultiChunkOffsets tests that buildChunkInfo correctly
// calculates byte offsets for multiple chunks, accounting for the newline
// separators that ConcatChunkContent inserts between chunks.
func TestBuildChunkInfo_MultiChunkOffsets(t *testing.T) {
	// Create multiple chunks for the same file
	chunk1 := diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n context\n+added")
	chunk2 := diff.NewChunk("file.go", 30, 2, 31, 3, "@@ -30,2 +31,3 @@\n more\n+stuff")
	chunk3 := diff.NewChunk("file.go", 50, 1, 52, 2, "@@ -50,1 +52,2 @@\n+end")

	chunks := []*diff.Chunk{chunk1, chunk2, chunk3}

	// Concatenate chunks the same way ConcatChunkContent does
	diffContent := diff.ConcatChunkContent(chunks)

	// Build chunk info
	chunkInfos := buildChunkInfo(chunks, diffContent)

	if len(chunkInfos) != 3 {
		t.Fatalf("expected 3 chunk infos, got %d", len(chunkInfos))
	}

	// Verify each chunk's offset/length allows correct extraction from diffContent
	for i, info := range chunkInfos {
		// Extract the chunk content using the offset and length
		if info.Offset+info.Length > len(diffContent) {
			t.Errorf("chunk %d: offset %d + length %d exceeds diffContent length %d",
				i, info.Offset, info.Length, len(diffContent))
			continue
		}

		extracted := diffContent[info.Offset : info.Offset+info.Length]
		expected := chunks[i].Content

		if extracted != expected {
			t.Errorf("chunk %d: extracted content mismatch\nexpected: %q\ngot:      %q",
				i, expected, extracted)
		}

		// Verify the index is correct
		if info.Index != i {
			t.Errorf("chunk %d: expected index %d, got %d", i, i, info.Index)
		}

		// Verify line numbers match
		if info.OldStart != chunks[i].OldStart {
			t.Errorf("chunk %d: OldStart mismatch: expected %d, got %d",
				i, chunks[i].OldStart, info.OldStart)
		}
		if info.NewStart != chunks[i].NewStart {
			t.Errorf("chunk %d: NewStart mismatch: expected %d, got %d",
				i, chunks[i].NewStart, info.NewStart)
		}
	}

	// Verify the total length of all chunks plus separators equals diffContent length
	totalLength := 0
	for i, info := range chunkInfos {
		totalLength += info.Length
		if i < len(chunkInfos)-1 {
			totalLength += 1 // separator newline
		}
	}
	if totalLength != len(diffContent) {
		t.Errorf("total length mismatch: chunks sum to %d, diffContent is %d",
			totalLength, len(diffContent))
	}
}

// TestCardStreamer_StaleCardRemovalDeletesChunks tests that when a card is removed
// as stale (file no longer in diff), its associated chunks are also deleted.
func TestCardStreamer_StaleCardRemovalDeletesChunks(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		ChunkStore:  store, // SQLiteStore implements both interfaces
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create and process a multi-chunk file
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n context\n+added1"),
		diff.NewChunk("file.go", 30, 2, 31, 3, "@@ -30,2 +31,3 @@\n more\n+added2"),
	}

	streamer.ProcessChunks(chunks)

	// Verify card and chunks were saved
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(allCards))
	}
	cardID := allCards[0].ID

	savedChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks: %v", err)
	}
	if len(savedChunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(savedChunks))
	}

	// Now simulate the file being staged externally (no longer in diff)
	// by processing with an empty chunk list
	streamer.ProcessChunks([]*diff.Chunk{})

	// Verify card was removed
	pendingCards, err := store.ListPending()
	if err != nil {
		t.Fatalf("failed to list pending cards: %v", err)
	}
	if len(pendingCards) != 0 {
		t.Errorf("expected 0 pending cards after stale removal, got %d", len(pendingCards))
	}

	// Verify chunks were also deleted (not orphaned)
	remainingChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks after removal: %v", err)
	}
	if len(remainingChunks) != 0 {
		t.Errorf("expected 0 chunks after stale card removal (orphan cleanup), got %d", len(remainingChunks))
	}

	// Verify card_removed was broadcast
	removedCards := broadcaster.getRemovedCards()
	if len(removedCards) != 1 {
		t.Errorf("expected 1 removed card broadcast, got %d", len(removedCards))
	}
}

// TestCardStreamer_ProcessChunksRaw_BinaryFile tests that binary files are detected
// and broadcast with isBinary=true and placeholder diff content.
func TestCardStreamer_ProcessChunksRaw_BinaryFile(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Simulate raw diff output with a binary file
	rawDiff := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ`

	// Binary files have no chunks in parsed output
	chunks := []*diff.Chunk{}

	streamer.ProcessChunksRaw(chunks, rawDiff)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card broadcast, got %d", len(cards))
	}

	card := cards[0]
	if card.File != "image.png" {
		t.Errorf("expected file 'image.png', got '%s'", card.File)
	}
	if !card.IsBinary {
		t.Error("expected isBinary=true for binary file")
	}
	if card.Diff != diff.BinaryDiffPlaceholder {
		t.Errorf("expected placeholder diff for binary, got: %s", card.Diff)
	}
	// Binary files should have no chunks
	if len(card.Chunks) != 0 {
		t.Errorf("expected 0 chunks for binary file, got %d", len(card.Chunks))
	}
}

// TestCardStreamer_ProcessChunksRaw_BinaryFileRebroadcast tests that binary file
// updates are re-broadcast when the blob hash changes.
func TestCardStreamer_ProcessChunksRaw_BinaryFileRebroadcast(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// First version of binary file
	rawDiffV1 := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ`

	streamer.ProcessChunksRaw([]*diff.Chunk{}, rawDiffV1)

	if len(broadcaster.getCards()) != 1 {
		t.Fatalf("expected 1 card after first broadcast")
	}

	// Same diff again - should NOT broadcast (deduplicated)
	streamer.ProcessChunksRaw([]*diff.Chunk{}, rawDiffV1)
	if len(broadcaster.getCards()) != 1 {
		t.Errorf("expected 1 card (no duplicate), got %d", len(broadcaster.getCards()))
	}

	// Second version with different blob hash - SHOULD broadcast
	rawDiffV2 := `diff --git a/image.png b/image.png
index abc1234..def5678 100644
Binary files a/image.png and b/image.png differ`

	streamer.ProcessChunksRaw([]*diff.Chunk{}, rawDiffV2)

	cards := broadcaster.getCards()
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards after binary update, got %d", len(cards))
	}

	// Both should be for the same file
	if cards[0].File != "image.png" || cards[1].File != "image.png" {
		t.Error("expected both cards to be for image.png")
	}
}

// TestCardStreamer_ProcessChunksRaw_TextFileWithStats tests that text files
// get proper diff stats calculated.
func TestCardStreamer_ProcessChunksRaw_TextFileWithStats(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	rawDiff := `diff --git a/test.txt b/test.txt
index abc123..def456 100644
--- a/test.txt
+++ b/test.txt
@@ -1,3 +1,5 @@
 line1
+added1
+added2
 line2
-deleted
 line3`

	chunks := []*diff.Chunk{{
		File:     "test.txt",
		OldStart: 1,
		OldCount: 3,
		NewStart: 1,
		NewCount: 5,
		Content: `@@ -1,3 +1,5 @@
 line1
+added1
+added2
 line2
-deleted
 line3`,
	}}

	streamer.ProcessChunksRaw(chunks, rawDiff)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}

	card := cards[0]
	if card.IsBinary {
		t.Error("expected isBinary=false for text file")
	}
	if card.Stats == nil {
		t.Fatal("expected stats to be set for text file")
	}
	if card.Stats.AddedLines != 2 {
		t.Errorf("expected 2 added lines, got %d", card.Stats.AddedLines)
	}
	if card.Stats.DeletedLines != 1 {
		t.Errorf("expected 1 deleted line, got %d", card.Stats.DeletedLines)
	}
	if card.Stats.LineCount == 0 {
		t.Error("expected line count > 0")
	}
}

// TestCardStreamer_ProcessChunksRaw_MixedBinaryAndText tests that a diff with
// both binary and text files processes both correctly.
func TestCardStreamer_ProcessChunksRaw_MixedBinaryAndText(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	rawDiff := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ
diff --git a/readme.txt b/readme.txt
index 111111..222222 100644
--- a/readme.txt
+++ b/readme.txt
@@ -1 +1,2 @@
 Hello
+World`

	// Only text file produces chunks
	chunks := []*diff.Chunk{{
		File:     "readme.txt",
		OldStart: 1,
		OldCount: 1,
		NewStart: 1,
		NewCount: 2,
		Content: `@@ -1 +1,2 @@
 Hello
+World`,
	}}

	streamer.ProcessChunksRaw(chunks, rawDiff)

	cards := broadcaster.getCards()
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (binary + text), got %d", len(cards))
	}

	// Find each card
	var binaryCard, textCard *broadcastedCard
	for i := range cards {
		if cards[i].File == "image.png" {
			binaryCard = &cards[i]
		} else if cards[i].File == "readme.txt" {
			textCard = &cards[i]
		}
	}

	if binaryCard == nil {
		t.Fatal("expected binary card for image.png")
	}
	if textCard == nil {
		t.Fatal("expected text card for readme.txt")
	}

	if !binaryCard.IsBinary {
		t.Error("expected image.png to be marked as binary")
	}
	if textCard.IsBinary {
		t.Error("expected readme.txt to NOT be marked as binary")
	}
}

// TestCardStreamer_StreamPendingCards_ReconcilesMissingChunks tests that
// StreamPendingCards creates chunk rows for pending cards that don't have them.
// This handles the case where cards exist from an older database without chunk support.
func TestCardStreamer_StreamPendingCards_ReconcilesMissingChunks(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Pre-populate store with a pending card that has NO chunk rows.
	// This simulates a card from an older database or one where chunks weren't saved.
	diffContent := "@@ -10,3 +10,4 @@\n context\n+added1\n@@ -30,2 +31,3 @@\n more\n+added2"
	cardID := "card-without-chunks"
	card := &storage.ReviewCard{
		ID:        cardID,
		SessionID: "test-session",
		File:      "test.go",
		Diff:      diffContent,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("failed to save card: %v", err)
	}

	// Verify no chunks exist initially
	existingChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks: %v", err)
	}
	if len(existingChunks) != 0 {
		t.Fatalf("expected 0 chunks initially, got %d", len(existingChunks))
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		ChunkStore:  store, // SQLiteStore implements both interfaces
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Stream pending cards - this should reconcile missing chunks
	if err := streamer.StreamPendingCards(); err != nil {
		t.Fatalf("failed to stream pending cards: %v", err)
	}

	// Verify chunks were created by reconciliation
	reconciledChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get reconciled chunks: %v", err)
	}

	// The diff has 2 chunks (two @@ headers)
	if len(reconciledChunks) != 2 {
		t.Errorf("expected 2 chunks after reconciliation, got %d", len(reconciledChunks))
	}

	// Verify chunk content and status
	for i, chunk := range reconciledChunks {
		if chunk.CardID != cardID {
			t.Errorf("chunk %d: expected cardID %s, got %s", i, cardID, chunk.CardID)
		}
		if chunk.ChunkIndex != i {
			t.Errorf("chunk %d: expected index %d, got %d", i, i, chunk.ChunkIndex)
		}
		if chunk.Status != storage.CardPending {
			t.Errorf("chunk %d: expected pending status, got %s", i, chunk.Status)
		}
		if chunk.Content == "" {
			t.Errorf("chunk %d: content should not be empty", i)
		}
	}

	// Verify card was also broadcast
	broadcastedCards := broadcaster.getCards()
	if len(broadcastedCards) != 1 {
		t.Errorf("expected 1 broadcast card, got %d", len(broadcastedCards))
	}
}

// TestCardStreamer_StreamPendingCards_SkipsExistingChunks tests that
// StreamPendingCards does NOT recreate chunks if they already exist.
func TestCardStreamer_StreamPendingCards_SkipsExistingChunks(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		ChunkStore:  store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create a card with chunks via normal ProcessChunks flow
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n context\n+added"),
	}
	streamer.ProcessChunks(chunks)

	// Get the card ID
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(allCards))
	}
	cardID := allCards[0].ID

	// Verify chunks exist
	originalChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks: %v", err)
	}
	if len(originalChunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(originalChunks))
	}

	// Clear broadcaster to track new broadcasts
	broadcaster.clearCards()

	// Stream pending cards - should NOT recreate chunks
	if err := streamer.StreamPendingCards(); err != nil {
		t.Fatalf("failed to stream pending cards: %v", err)
	}

	// Verify chunks are unchanged (same reference/content)
	afterChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks after stream: %v", err)
	}
	if len(afterChunks) != 1 {
		t.Errorf("expected 1 chunk after stream, got %d", len(afterChunks))
	}

	// Card should still be broadcast
	if len(broadcaster.getCards()) != 1 {
		t.Errorf("expected 1 broadcast card, got %d", len(broadcaster.getCards()))
	}
}

// TestClearFileHashAllowsRebroadcast tests that ClearFileHash removes the cached
// hash, allowing identical diffs to be re-broadcast after a card is decided.
func TestClearFileHashAllowsRebroadcast(t *testing.T) {
	broadcaster := newMockBroadcaster()
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Process initial chunks
	chunks := []*diff.Chunk{
		{File: "test.txt", Content: "@@ -1 +1 @@\n-old\n+new\n"},
	}
	streamer.ProcessChunks(chunks)

	// Verify card was broadcast
	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}

	// Clear the broadcasted cards to simulate fresh state
	broadcaster.clearCards()

	// Process the same chunks again (should be skipped - same hash)
	streamer.ProcessChunks(chunks)
	if len(broadcaster.getCards()) != 0 {
		t.Error("expected same chunks to be skipped (duplicate hash)")
	}

	// Now clear the file hash (simulating card removal via decision)
	streamer.ClearFileHash("test.txt")

	// Process the same chunks again - should be broadcast now
	streamer.ProcessChunks(chunks)
	cards = broadcaster.getCards()
	if len(cards) != 1 {
		t.Errorf("expected card to be re-broadcast after ClearFileHash, got %d cards", len(cards))
	}
}

// TestParseChunkInfoFromDiff_MatchesBuildChunkInfo verifies that parsing chunk info
// from concatenated diff content produces identical Content and ContentHash values
// to buildChunkInfo. This is critical for per-chunk decisions after reconnect.
func TestParseChunkInfoFromDiff_MatchesBuildChunkInfo(t *testing.T) {
	// Create multi-chunk test data matching real diff patterns
	chunk1 := diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n context line\n+added line 1")
	chunk2 := diff.NewChunk("file.go", 30, 2, 31, 3, "@@ -30,2 +31,3 @@\n more context\n+added line 2\n-removed line")
	chunk3 := diff.NewChunk("file.go", 50, 1, 52, 2, "@@ -50,1 +52,2 @@\n final context\n+last addition")

	chunks := []*diff.Chunk{chunk1, chunk2, chunk3}

	// Concatenate chunks the same way the live path does
	diffContent := diff.ConcatChunkContent(chunks)

	// Build chunk info using the live path function
	liveChunkInfos := buildChunkInfo(chunks, diffContent)

	// Parse chunk info using the reconnect path function
	parsedChunkInfos := ParseChunkInfoFromDiff(diffContent)

	// Verify same number of chunks
	if len(parsedChunkInfos) != len(liveChunkInfos) {
		t.Fatalf("chunk count mismatch: live=%d, parsed=%d", len(liveChunkInfos), len(parsedChunkInfos))
	}

	// Verify each chunk has identical Content and ContentHash
	for i := range liveChunkInfos {
		live := liveChunkInfos[i]
		parsed := parsedChunkInfos[i]

		if live.Content != parsed.Content {
			t.Errorf("chunk %d Content mismatch:\nlive:   %q\nparsed: %q", i, live.Content, parsed.Content)
		}

		if live.ContentHash != parsed.ContentHash {
			t.Errorf("chunk %d ContentHash mismatch: live=%s, parsed=%s", i, live.ContentHash, parsed.ContentHash)
		}

		if live.Length != parsed.Length {
			t.Errorf("chunk %d Length mismatch: live=%d, parsed=%d", i, live.Length, parsed.Length)
		}

		if live.Index != parsed.Index {
			t.Errorf("chunk %d Index mismatch: live=%d, parsed=%d", i, live.Index, parsed.Index)
		}

		// Verify line numbers are parsed correctly
		if live.OldStart != parsed.OldStart {
			t.Errorf("chunk %d OldStart mismatch: live=%d, parsed=%d", i, live.OldStart, parsed.OldStart)
		}
		if live.NewStart != parsed.NewStart {
			t.Errorf("chunk %d NewStart mismatch: live=%d, parsed=%d", i, live.NewStart, parsed.NewStart)
		}
	}
}

// TestParseChunkInfoFromDiff_SingleChunk verifies single-chunk diffs work correctly.
func TestParseChunkInfoFromDiff_SingleChunk(t *testing.T) {
	chunk := diff.NewChunk("file.go", 1, 5, 1, 6, "@@ -1,5 +1,6 @@\n line1\n line2\n+new line")
	chunks := []*diff.Chunk{chunk}
	diffContent := diff.ConcatChunkContent(chunks)

	liveChunkInfos := buildChunkInfo(chunks, diffContent)
	parsedChunkInfos := ParseChunkInfoFromDiff(diffContent)

	if len(parsedChunkInfos) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(parsedChunkInfos))
	}

	if liveChunkInfos[0].Content != parsedChunkInfos[0].Content {
		t.Errorf("single chunk Content mismatch:\nlive:   %q\nparsed: %q",
			liveChunkInfos[0].Content, parsedChunkInfos[0].Content)
	}

	if liveChunkInfos[0].ContentHash != parsedChunkInfos[0].ContentHash {
		t.Errorf("single chunk ContentHash mismatch: live=%s, parsed=%s",
			liveChunkInfos[0].ContentHash, parsedChunkInfos[0].ContentHash)
	}
}

// TestReconcileChunks_RebuildsStaleMismatch verifies that reconcileChunks rebuilds
// chunks when the stored content hash doesn't match the expected hash.
func TestReconcileChunks_RebuildsStaleMismatch(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		ChunkStore:  store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create a card with chunks via normal ProcessChunks flow
	originalChunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n original context\n+original add"),
		diff.NewChunk("file.go", 30, 2, 31, 3, "@@ -30,2 +31,3 @@\n more original\n+another original"),
	}
	streamer.ProcessChunks(originalChunks)

	// Get the card ID
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	if len(allCards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(allCards))
	}
	cardID := allCards[0].ID

	// Verify original chunks were saved
	savedChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks: %v", err)
	}
	if len(savedChunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(savedChunks))
	}
	originalHash := savedChunks[0].Content

	// Now create DIFFERENT chunk info representing updated diff content
	// This simulates what happens when stored chunks are stale
	updatedChunkInfos := []ChunkInfo{
		{Index: 0, Content: "@@ -10,3 +10,4 @@\n UPDATED context\n+UPDATED add", ContentHash: "different1"},
		{Index: 1, Content: "@@ -30,2 +31,3 @@\n more UPDATED\n+another UPDATED", ContentHash: "different2"},
	}
	// Recompute the actual hash for comparison
	updatedChunkInfos[0].ContentHash = hashContent(updatedChunkInfos[0].Content)
	updatedChunkInfos[1].ContentHash = hashContent(updatedChunkInfos[1].Content)

	// Call reconcile with the "updated" chunk info
	// This should detect hash mismatch and rebuild
	streamer.ReconcileChunksForCard(cardID, updatedChunkInfos)

	// Verify chunks were rebuilt with new content
	rebuiltChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get rebuilt chunks: %v", err)
	}
	if len(rebuiltChunks) != 2 {
		t.Fatalf("expected 2 rebuilt chunks, got %d", len(rebuiltChunks))
	}

	// Content should now match the updated chunk info
	if rebuiltChunks[0].Content == originalHash {
		t.Error("expected chunk content to be updated, but it still has original content")
	}
	if rebuiltChunks[0].Content != updatedChunkInfos[0].Content {
		t.Errorf("rebuilt chunk 0 content mismatch:\nexpected: %q\ngot: %q",
			updatedChunkInfos[0].Content, rebuiltChunks[0].Content)
	}
	if rebuiltChunks[1].Content != updatedChunkInfos[1].Content {
		t.Errorf("rebuilt chunk 1 content mismatch:\nexpected: %q\ngot: %q",
			updatedChunkInfos[1].Content, rebuiltChunks[1].Content)
	}
}

// TestReconcileChunks_RebuildOnCountMismatch verifies that reconcileChunks rebuilds
// chunks when the stored chunk count doesn't match the expected count.
func TestReconcileChunks_RebuildOnCountMismatch(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:       store,
		ChunkStore:  store,
		Broadcaster: broadcaster,
		SessionID:   "test-session",
	})

	// Create a card with 2 chunks
	originalChunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n chunk1"),
		diff.NewChunk("file.go", 30, 2, 31, 3, "@@ -30,2 +31,3 @@\n chunk2"),
	}
	streamer.ProcessChunks(originalChunks)

	// Get the card ID
	allCards, err := store.ListAll()
	if err != nil {
		t.Fatalf("failed to list cards: %v", err)
	}
	cardID := allCards[0].ID

	// Verify 2 chunks were saved
	savedChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get chunks: %v", err)
	}
	if len(savedChunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(savedChunks))
	}

	// Now reconcile with 3 chunks (count mismatch)
	newChunkInfos := []ChunkInfo{
		{Index: 0, Content: "@@ -10,3 +10,4 @@\n new chunk1", ContentHash: hashContent("@@ -10,3 +10,4 @@\n new chunk1")},
		{Index: 1, Content: "@@ -30,2 +31,3 @@\n new chunk2", ContentHash: hashContent("@@ -30,2 +31,3 @@\n new chunk2")},
		{Index: 2, Content: "@@ -50,1 +52,2 @@\n new chunk3", ContentHash: hashContent("@@ -50,1 +52,2 @@\n new chunk3")},
	}

	streamer.ReconcileChunksForCard(cardID, newChunkInfos)

	// Verify chunks were rebuilt with new count
	rebuiltChunks, err := store.GetChunks(cardID)
	if err != nil {
		t.Fatalf("failed to get rebuilt chunks: %v", err)
	}
	if len(rebuiltChunks) != 3 {
		t.Errorf("expected 3 rebuilt chunks after count mismatch, got %d", len(rebuiltChunks))
	}
}

// =============================================================================
// Phase 2.2: Grouping Algorithm Tests
// =============================================================================

// TestChunkGroupInfo_Fields verifies the ChunkGroupInfo struct has required fields.
func TestChunkGroupInfo_Fields(t *testing.T) {
	group := ChunkGroupInfo{
		GroupIndex: 0,
		LineStart:  10,
		LineEnd:    25,
		ChunkCount: 3,
	}

	if group.GroupIndex != 0 {
		t.Errorf("expected GroupIndex 0, got %d", group.GroupIndex)
	}
	if group.LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", group.LineStart)
	}
	if group.LineEnd != 25 {
		t.Errorf("expected LineEnd 25, got %d", group.LineEnd)
	}
	if group.ChunkCount != 3 {
		t.Errorf("expected ChunkCount 3, got %d", group.ChunkCount)
	}
}

// TestGroupChunksByProximity_SingleChunk tests grouping with one chunk.
func TestGroupChunksByProximity_SingleChunk(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 10, OldCount: 3, NewStart: 10, NewCount: 5},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].GroupIndex != 0 {
		t.Errorf("expected group index 0, got %d", groups[0].GroupIndex)
	}
	if groups[0].LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", groups[0].LineStart)
	}
	// LineEnd = line_ref + line_span - 1 = 10 + 5 - 1 = 14
	if groups[0].LineEnd != 14 {
		t.Errorf("expected LineEnd 14, got %d", groups[0].LineEnd)
	}
	if groups[0].ChunkCount != 1 {
		t.Errorf("expected ChunkCount 1, got %d", groups[0].ChunkCount)
	}

	// Verify indexed chunks have GroupIndex set
	if len(indexed) != 1 {
		t.Fatalf("expected 1 indexed chunk, got %d", len(indexed))
	}
	if indexed[0].GroupIndex != 0 {
		t.Errorf("expected chunk GroupIndex 0, got %d", indexed[0].GroupIndex)
	}
}

// TestGroupChunksByProximity_MultipleChunks_SameGroup tests chunks within proximity.
func TestGroupChunksByProximity_MultipleChunks_SameGroup(t *testing.T) {
	// Chunk 1: lines 10-14 (new_start=10, new_count=5)
	// Chunk 2: lines 30-32 (new_start=30, new_count=3)
	// With proximity 20: 30 <= 14 + 20 = 34, so same group
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 10, OldCount: 3, NewStart: 10, NewCount: 5},
		{Index: 1, OldStart: 30, OldCount: 2, NewStart: 30, NewCount: 3},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (chunks within proximity), got %d", len(groups))
	}
	if groups[0].LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", groups[0].LineStart)
	}
	// LineEnd = max of both chunks = max(14, 32) = 32
	if groups[0].LineEnd != 32 {
		t.Errorf("expected LineEnd 32, got %d", groups[0].LineEnd)
	}
	if groups[0].ChunkCount != 2 {
		t.Errorf("expected ChunkCount 2, got %d", groups[0].ChunkCount)
	}

	// All chunks in same group
	for i, chunk := range indexed {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0, got %d", i, chunk.GroupIndex)
		}
	}
}

// TestGroupChunksByProximity_MultipleChunks_SeparateGroups tests chunks beyond proximity.
func TestGroupChunksByProximity_MultipleChunks_SeparateGroups(t *testing.T) {
	// Chunk 1: lines 10-14 (new_start=10, new_count=5)
	// Chunk 2: lines 50-52 (new_start=50, new_count=3)
	// With proximity 20: 50 > 14 + 20 = 34, so separate groups
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 10, OldCount: 3, NewStart: 10, NewCount: 5},
		{Index: 1, OldStart: 50, OldCount: 2, NewStart: 50, NewCount: 3},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (chunks beyond proximity), got %d", len(groups))
	}

	// First group
	if groups[0].GroupIndex != 0 {
		t.Errorf("group 0: expected index 0, got %d", groups[0].GroupIndex)
	}
	if groups[0].LineStart != 10 || groups[0].LineEnd != 14 {
		t.Errorf("group 0: expected 10-14, got %d-%d", groups[0].LineStart, groups[0].LineEnd)
	}
	if groups[0].ChunkCount != 1 {
		t.Errorf("group 0: expected count 1, got %d", groups[0].ChunkCount)
	}

	// Second group
	if groups[1].GroupIndex != 1 {
		t.Errorf("group 1: expected index 1, got %d", groups[1].GroupIndex)
	}
	if groups[1].LineStart != 50 || groups[1].LineEnd != 52 {
		t.Errorf("group 1: expected 50-52, got %d-%d", groups[1].LineStart, groups[1].LineEnd)
	}
	if groups[1].ChunkCount != 1 {
		t.Errorf("group 1: expected count 1, got %d", groups[1].ChunkCount)
	}

	// Verify chunk group indices
	if indexed[0].GroupIndex != 0 {
		t.Errorf("chunk 0: expected GroupIndex 0, got %d", indexed[0].GroupIndex)
	}
	if indexed[1].GroupIndex != 1 {
		t.Errorf("chunk 1: expected GroupIndex 1, got %d", indexed[1].GroupIndex)
	}
}

// TestGroupChunksByProximity_ThreeChunks_TwoGroups tests complex grouping.
func TestGroupChunksByProximity_ThreeChunks_TwoGroups(t *testing.T) {
	// Chunk 1: lines 10-14
	// Chunk 2: lines 25-27 (within 20 of chunk 1's end at 14)
	// Chunk 3: lines 60-64 (beyond 20 of chunk 2's end at 27)
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 5},
		{Index: 1, NewStart: 25, NewCount: 3},
		{Index: 2, NewStart: 60, NewCount: 5},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First group contains chunks 0 and 1
	if groups[0].ChunkCount != 2 {
		t.Errorf("group 0: expected 2 chunks, got %d", groups[0].ChunkCount)
	}
	if groups[0].LineStart != 10 {
		t.Errorf("group 0: expected LineStart 10, got %d", groups[0].LineStart)
	}
	if groups[0].LineEnd != 27 {
		t.Errorf("group 0: expected LineEnd 27 (25+3-1), got %d", groups[0].LineEnd)
	}

	// Second group contains chunk 2
	if groups[1].ChunkCount != 1 {
		t.Errorf("group 1: expected 1 chunk, got %d", groups[1].ChunkCount)
	}
	if groups[1].LineStart != 60 {
		t.Errorf("group 1: expected LineStart 60, got %d", groups[1].LineStart)
	}

	// Verify chunk assignments
	if indexed[0].GroupIndex != 0 || indexed[1].GroupIndex != 0 {
		t.Error("chunks 0 and 1 should be in group 0")
	}
	if indexed[2].GroupIndex != 1 {
		t.Errorf("chunk 2: expected GroupIndex 1, got %d", indexed[2].GroupIndex)
	}
}

// TestGroupChunksByProximity_EmptyChunks tests empty input.
func TestGroupChunksByProximity_EmptyChunks(t *testing.T) {
	groups, indexed := GroupChunksByProximity(nil, 20)

	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil input, got %d", len(groups))
	}
	if len(indexed) != 0 {
		t.Errorf("expected 0 indexed chunks for nil input, got %d", len(indexed))
	}

	groups, indexed = GroupChunksByProximity([]ChunkInfo{}, 20)

	if len(groups) != 0 {
		t.Errorf("expected 0 groups for empty input, got %d", len(groups))
	}
	if len(indexed) != 0 {
		t.Errorf("expected 0 indexed chunks for empty input, got %d", len(indexed))
	}
}

// TestGroupChunksByProximity_DeletionOnly_UsesOldStart tests the adversarial case:
// chunk.new_count = 0 -> line_ref uses old_start for grouping
func TestGroupChunksByProximity_DeletionOnly_UsesOldStart(t *testing.T) {
	// Chunk is deletion-only: new_count = 0, so use old_start
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 100, OldCount: 5, NewStart: 100, NewCount: 0}, // deletion at old line 100
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	// line_ref should be old_start (100) since new_count = 0
	// line_span should be old_count (5) since new_count = 0
	// LineEnd = 100 + 5 - 1 = 104
	if groups[0].LineStart != 100 {
		t.Errorf("deletion-only chunk: expected LineStart 100 (old_start), got %d", groups[0].LineStart)
	}
	if groups[0].LineEnd != 104 {
		t.Errorf("deletion-only chunk: expected LineEnd 104, got %d", groups[0].LineEnd)
	}
	if indexed[0].GroupIndex != 0 {
		t.Errorf("expected chunk GroupIndex 0, got %d", indexed[0].GroupIndex)
	}
}

// TestGroupChunksByProximity_IdenticalLineRef_SameGroup tests the adversarial case:
// chunks with identical line_ref -> same group (line_end >= line_start)
func TestGroupChunksByProximity_IdenticalLineRef_SameGroup(t *testing.T) {
	// Two chunks at exactly the same line
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 50, NewCount: 2},
		{Index: 1, NewStart: 50, NewCount: 3},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group for identical line_ref, got %d", len(groups))
	}

	if groups[0].ChunkCount != 2 {
		t.Errorf("expected 2 chunks in group, got %d", groups[0].ChunkCount)
	}
	if groups[0].LineStart != 50 {
		t.Errorf("expected LineStart 50, got %d", groups[0].LineStart)
	}
	// LineEnd should be max(50+2-1, 50+3-1) = max(51, 52) = 52
	if groups[0].LineEnd != 52 {
		t.Errorf("expected LineEnd 52, got %d", groups[0].LineEnd)
	}

	// Both chunks in same group
	if indexed[0].GroupIndex != indexed[1].GroupIndex {
		t.Error("identical line_ref chunks should be in the same group")
	}
}

// TestGroupChunksByProximity_ProximityBoundary tests exact proximity boundary.
func TestGroupChunksByProximity_ProximityBoundary(t *testing.T) {
	// Chunk 1 ends at line 14 (10 + 5 - 1)
	// Chunk 2 starts at line 34 (exactly 14 + 20)
	// Should be in the SAME group (<=, not <)
	chunksWithin := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 5}, // ends at 14
		{Index: 1, NewStart: 34, NewCount: 2}, // starts at 34 = 14 + 20
	}

	groups, _ := GroupChunksByProximity(chunksWithin, 20)

	if len(groups) != 1 {
		t.Errorf("expected 1 group at exact boundary (34 <= 14+20), got %d", len(groups))
	}

	// Chunk 2 starts at line 35 (14 + 20 + 1 = 35)
	// Should be in DIFFERENT group (35 > 34)
	chunksBeyond := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 5}, // ends at 14
		{Index: 1, NewStart: 35, NewCount: 2}, // starts at 35 > 14 + 20
	}

	groups, _ = GroupChunksByProximity(chunksBeyond, 20)

	if len(groups) != 2 {
		t.Errorf("expected 2 groups beyond boundary (35 > 14+20), got %d", len(groups))
	}
}

// TestGroupChunksByProximity_MinimumProximity tests proximity = 1.
func TestGroupChunksByProximity_MinimumProximity(t *testing.T) {
	// With proximity 1, only immediately adjacent chunks are grouped
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 5}, // ends at 14
		{Index: 1, NewStart: 15, NewCount: 3}, // starts at 15 = 14 + 1, within proximity
		{Index: 2, NewStart: 20, NewCount: 2}, // starts at 20 > 17 + 1 = 18, separate group
	}

	groups, _ := GroupChunksByProximity(chunks, 1)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups with proximity 1, got %d", len(groups))
	}

	// First group: chunks 0 and 1 (15 <= 14 + 1)
	if groups[0].ChunkCount != 2 {
		t.Errorf("group 0: expected 2 chunks, got %d", groups[0].ChunkCount)
	}

	// Second group: chunk 2 (20 > 17 + 1)
	if groups[1].ChunkCount != 1 {
		t.Errorf("group 1: expected 1 chunk, got %d", groups[1].ChunkCount)
	}
}

// TestGroupChunksByProximity_UnsortedInput tests that input is sorted by line_ref.
func TestGroupChunksByProximity_UnsortedInput(t *testing.T) {
	// Input chunks are NOT sorted by line number
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 50, NewCount: 2}, // line 50
		{Index: 1, NewStart: 10, NewCount: 3}, // line 10 (should come first)
		{Index: 2, NewStart: 30, NewCount: 2}, // line 30
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	// After sorting by line_ref: 10, 30, 50
	// 10-12, 30-31 (30 <= 12+20=32, same group)
	// 50-51 (50 > 31+20=51? No, 50 <= 51, same group)
	// Actually: 30 <= 12+20=32 ✓, 50 <= 31+20=51 ✓
	// All in one group
	if len(groups) != 1 {
		t.Fatalf("expected 1 group after sorting, got %d", len(groups))
	}

	// All chunks should have GroupIndex 0
	for i, chunk := range indexed {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0, got %d", i, chunk.GroupIndex)
		}
	}
}

// TestGroupChunksByProximity_LargeGap tests chunks with very large gap.
func TestGroupChunksByProximity_LargeGap(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 5},
		{Index: 1, NewStart: 1000, NewCount: 5},
	}

	groups, _ := GroupChunksByProximity(chunks, 20)

	if len(groups) != 2 {
		t.Errorf("expected 2 groups for large gap, got %d", len(groups))
	}
}

// TestGroupChunksByProximity_MixedDeletionAndAddition tests mixed chunk types.
func TestGroupChunksByProximity_MixedDeletionAndAddition(t *testing.T) {
	chunks := []ChunkInfo{
		// Addition chunk at new line 10
		{Index: 0, OldStart: 10, OldCount: 0, NewStart: 10, NewCount: 5},
		// Deletion-only chunk at old line 20 (new_count = 0)
		{Index: 1, OldStart: 20, OldCount: 3, NewStart: 15, NewCount: 0},
		// Regular edit at new line 30
		{Index: 2, OldStart: 28, OldCount: 2, NewStart: 30, NewCount: 3},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	// line_ref values:
	// chunk 0: new_count > 0, so line_ref = 10
	// chunk 1: new_count = 0, so line_ref = 20 (old_start)
	// chunk 2: new_count > 0, so line_ref = 30
	//
	// After sorting: 10, 20, 30
	// 10-14, 20 <= 14+20=34 ✓ (same group)
	// 30 <= 22+20=42 ✓ (same group, line_end of chunk 1 is 20+3-1=22)
	// Wait: chunk 1 has old_count=3, new_count=0, so line_span = old_count = 3
	// line_end = 20 + 3 - 1 = 22
	// 30 <= 22 + 20 = 42 ✓

	if len(groups) != 1 {
		t.Fatalf("expected 1 group for mixed chunks within proximity, got %d", len(groups))
	}

	if groups[0].ChunkCount != 3 {
		t.Errorf("expected 3 chunks in group, got %d", groups[0].ChunkCount)
	}

	// Verify all chunks assigned to group 0
	for i, chunk := range indexed {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0, got %d", i, chunk.GroupIndex)
		}
	}
}

// TestGroupChunksByProximity_PreservesOtherFields tests that other ChunkInfo fields are preserved.
func TestGroupChunksByProximity_PreservesOtherFields(t *testing.T) {
	chunks := []ChunkInfo{
		{
			Index:       0,
			OldStart:    10,
			OldCount:    3,
			NewStart:    10,
			NewCount:    5,
			Offset:      0,
			Length:      100,
			Content:     "@@ -10,3 +10,5 @@\n context",
			ContentHash: "abc123",
		},
	}

	_, indexed := GroupChunksByProximity(chunks, 20)

	if len(indexed) != 1 {
		t.Fatalf("expected 1 indexed chunk, got %d", len(indexed))
	}

	// All original fields should be preserved
	if indexed[0].Index != 0 {
		t.Errorf("Index not preserved: expected 0, got %d", indexed[0].Index)
	}
	if indexed[0].OldStart != 10 {
		t.Errorf("OldStart not preserved: expected 10, got %d", indexed[0].OldStart)
	}
	if indexed[0].OldCount != 3 {
		t.Errorf("OldCount not preserved: expected 3, got %d", indexed[0].OldCount)
	}
	if indexed[0].NewStart != 10 {
		t.Errorf("NewStart not preserved: expected 10, got %d", indexed[0].NewStart)
	}
	if indexed[0].NewCount != 5 {
		t.Errorf("NewCount not preserved: expected 5, got %d", indexed[0].NewCount)
	}
	if indexed[0].Offset != 0 {
		t.Errorf("Offset not preserved: expected 0, got %d", indexed[0].Offset)
	}
	if indexed[0].Length != 100 {
		t.Errorf("Length not preserved: expected 100, got %d", indexed[0].Length)
	}
	if indexed[0].Content != "@@ -10,3 +10,5 @@\n context" {
		t.Errorf("Content not preserved")
	}
	if indexed[0].ContentHash != "abc123" {
		t.Errorf("ContentHash not preserved: expected abc123, got %s", indexed[0].ContentHash)
	}
	// GroupIndex should be set
	if indexed[0].GroupIndex != 0 {
		t.Errorf("GroupIndex should be 0, got %d", indexed[0].GroupIndex)
	}
}

// TestGroupChunksByProximity_ZeroLineSpan tests chunks with 0 line span.
func TestGroupChunksByProximity_ZeroLineSpan(t *testing.T) {
	// Edge case: both counts are 0 (unusual but possible)
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 10, OldCount: 0, NewStart: 10, NewCount: 0},
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	// line_span = 0, so line_end = line_start - 1 = 9
	// But we should handle this gracefully - line_end should be at least line_start
	if groups[0].LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", groups[0].LineStart)
	}
	// When span is 0, line_end should equal line_start (treat as 1 line minimum)
	if groups[0].LineEnd < groups[0].LineStart {
		t.Errorf("LineEnd (%d) should be >= LineStart (%d)", groups[0].LineEnd, groups[0].LineStart)
	}

	if len(indexed) != 1 || indexed[0].GroupIndex != 0 {
		t.Error("chunk should be assigned to group 0")
	}
}

// TestGroupChunksByProximity_ManyChunks tests performance with many chunks.
func TestGroupChunksByProximity_ManyChunks(t *testing.T) {
	// Create 100 chunks, each 10 lines apart
	chunks := make([]ChunkInfo, 100)
	for i := 0; i < 100; i++ {
		chunks[i] = ChunkInfo{
			Index:    i,
			NewStart: i*10 + 1, // 1, 11, 21, 31, ...
			NewCount: 3,
		}
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	// With proximity 20, each chunk ends at i*10+3 and next starts at (i+1)*10+1
	// Gap = (i+1)*10+1 - (i*10+3) = 10+1-3 = 8
	// 8 <= 20, so all should be in one group
	if len(groups) != 1 {
		t.Errorf("expected 1 group for closely spaced chunks, got %d", len(groups))
	}

	if groups[0].ChunkCount != 100 {
		t.Errorf("expected 100 chunks in group, got %d", groups[0].ChunkCount)
	}

	// Verify all have same group index
	for i, chunk := range indexed {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0, got %d", i, chunk.GroupIndex)
		}
	}
}

// =============================================================================
// Phase 2.4: Wiring Integration Tests
// =============================================================================
// These tests verify that ChunkGroupingEnabled and ChunkGroupingProximity config
// fields are correctly wired to CardStreamer's ProcessChunksRaw and StreamPendingCards.

// TestCardStreamer_ProcessChunksRaw_GroupingEnabled_PopulatesGroups verifies that
// when ChunkGroupingEnabled=true, broadcasts include chunk_groups and chunks have GroupIndex set.
func TestCardStreamer_ProcessChunksRaw_GroupingEnabled_PopulatesGroups(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   true,
		ChunkGroupingProximity: 20,
	})

	// Create 3 chunks within proximity (all should be in one group)
	// Chunk 1: lines 10-14, Chunk 2: lines 25-27, Chunk 3: lines 35-38
	// With proximity 20: 25 <= 14+20=34 ✓, 35 <= 27+20=47 ✓
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 5, 10, 5, "@@ -10,5 +10,5 @@\n context\n+added1"),
		diff.NewChunk("file.go", 25, 3, 25, 3, "@@ -25,3 +25,3 @@\n more\n+added2"),
		diff.NewChunk("file.go", 35, 4, 35, 4, "@@ -35,4 +35,4 @@\n final\n+added3"),
	}

	streamer.ProcessChunks(chunks)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	card := cards[0]

	// Verify chunk_groups is populated
	if len(card.ChunkGroups) == 0 {
		t.Fatal("expected chunk_groups to be populated when grouping enabled")
	}

	// All 3 chunks within proximity should be in 1 group
	if len(card.ChunkGroups) != 1 {
		t.Errorf("expected 1 group for chunks within proximity, got %d", len(card.ChunkGroups))
	}

	if card.ChunkGroups[0].ChunkCount != 3 {
		t.Errorf("expected group to have 3 chunks, got %d", card.ChunkGroups[0].ChunkCount)
	}

	// Verify all chunks have GroupIndex set to 0
	if len(card.Chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(card.Chunks))
	}
	for i, chunk := range card.Chunks {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0, got %d", i, chunk.GroupIndex)
		}
	}
}

// TestCardStreamer_ProcessChunksRaw_GroupingDisabled_OmitsGroups verifies that
// when ChunkGroupingEnabled=false, broadcasts have nil chunk_groups.
// This is the adversarial case: chunk_groups missing or empty -> render flat list.
func TestCardStreamer_ProcessChunksRaw_GroupingDisabled_OmitsGroups(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   false, // Disabled
		ChunkGroupingProximity: 20,
	})

	// Create multiple chunks
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 3, 10, 4, "@@ -10,3 +10,4 @@\n context\n+added1"),
		diff.NewChunk("file.go", 50, 2, 51, 3, "@@ -50,2 +51,3 @@\n more\n+added2"),
	}

	streamer.ProcessChunks(chunks)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	card := cards[0]

	// Verify chunk_groups is nil/empty when grouping is disabled
	if len(card.ChunkGroups) != 0 {
		t.Errorf("expected chunk_groups to be empty when grouping disabled, got %d groups", len(card.ChunkGroups))
	}

	// Chunks should still be present (without grouping metadata)
	if len(card.Chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(card.Chunks))
	}

	// GroupIndex should be 0 (unset/default) for all chunks when grouping disabled
	for i, chunk := range card.Chunks {
		if chunk.GroupIndex != 0 {
			t.Errorf("chunk %d: expected GroupIndex 0 (unset) when grouping disabled, got %d", i, chunk.GroupIndex)
		}
	}
}

// TestCardStreamer_ProcessChunksRaw_GroupingEnabled_UsesConfigProximity verifies that
// a custom ChunkGroupingProximity value is respected.
func TestCardStreamer_ProcessChunksRaw_GroupingEnabled_UsesConfigProximity(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	// Use small proximity of 5 lines
	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   true,
		ChunkGroupingProximity: 5, // Small proximity
	})

	// Chunk 1: ends at line 14 (10+5-1)
	// Chunk 2: starts at line 25
	// With proximity 5: 25 > 14+5=19, so should be in SEPARATE groups
	// With default proximity 20: 25 <= 14+20=34, would be same group
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 5, 10, 5, "@@ -10,5 +10,5 @@\n context\n+added1"),
		diff.NewChunk("file.go", 25, 3, 25, 3, "@@ -25,3 +25,3 @@\n more\n+added2"),
	}

	streamer.ProcessChunks(chunks)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	card := cards[0]

	// With small proximity, chunks should be in separate groups
	if len(card.ChunkGroups) != 2 {
		t.Errorf("expected 2 groups with proximity 5, got %d (custom proximity not respected)", len(card.ChunkGroups))
	}

	// Verify each group has 1 chunk
	for i, group := range card.ChunkGroups {
		if group.ChunkCount != 1 {
			t.Errorf("group %d: expected 1 chunk, got %d", i, group.ChunkCount)
		}
	}

	// Verify chunks have different GroupIndex values
	if len(card.Chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(card.Chunks))
	}
	if card.Chunks[0].GroupIndex == card.Chunks[1].GroupIndex {
		t.Error("chunks should have different GroupIndex values with small proximity")
	}
}

// TestCardStreamer_ProcessChunksRaw_GroupingEnabled_DefaultProximity verifies that
// when ChunkGroupingProximity=0 (unset), the default proximity of 20 is used.
func TestCardStreamer_ProcessChunksRaw_GroupingEnabled_DefaultProximity(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   true,
		ChunkGroupingProximity: 0, // Unset - should use default of 20
	})

	// Chunk 1: ends at line 14 (10+5-1)
	// Chunk 2: starts at line 30
	// With default proximity 20: 30 <= 14+20=34, should be in SAME group
	chunks := []*diff.Chunk{
		diff.NewChunk("file.go", 10, 5, 10, 5, "@@ -10,5 +10,5 @@\n context\n+added1"),
		diff.NewChunk("file.go", 30, 3, 30, 3, "@@ -30,3 +30,3 @@\n more\n+added2"),
	}

	streamer.ProcessChunks(chunks)

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	card := cards[0]

	// With default proximity 20, chunks should be in same group
	if len(card.ChunkGroups) != 1 {
		t.Errorf("expected 1 group with default proximity 20, got %d", len(card.ChunkGroups))
	}

	if card.ChunkGroups[0].ChunkCount != 2 {
		t.Errorf("expected group to have 2 chunks, got %d", card.ChunkGroups[0].ChunkCount)
	}
}

// TestCardStreamer_StreamPendingCards_GroupingEnabled verifies that grouping
// is applied when streaming pending cards on reconnect.
func TestCardStreamer_StreamPendingCards_GroupingEnabled(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Pre-populate store with a pending card that has multiple chunks
	diffContent := "@@ -10,3 +10,4 @@\n context\n+added1\n@@ -30,2 +31,3 @@\n more\n+added2"
	card := &storage.ReviewCard{
		ID:        "card-for-reconnect",
		SessionID: "test-session",
		File:      "test.go",
		Diff:      diffContent,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("failed to save card: %v", err)
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   true,
		ChunkGroupingProximity: 30, // Enough to group both chunks (lines 10 and 30)
	})

	// Stream pending cards (simulates reconnect)
	if err := streamer.StreamPendingCards(); err != nil {
		t.Fatalf("StreamPendingCards() error: %v", err)
	}

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	streamedCard := cards[0]

	// Verify chunk_groups is populated on reconnect
	if len(streamedCard.ChunkGroups) == 0 {
		t.Error("expected chunk_groups to be populated on reconnect when grouping enabled")
	}

	// Verify chunks have GroupIndex set
	if len(streamedCard.Chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(streamedCard.Chunks))
	}

	// With proximity 30, both chunks (at lines 10 and 30) should be in same group
	// 30 <= (10 + 4 - 1) + 30 = 13 + 30 = 43 ✓
	if len(streamedCard.ChunkGroups) != 1 {
		t.Errorf("expected 1 group on reconnect, got %d", len(streamedCard.ChunkGroups))
	}
}

// TestCardStreamer_StreamPendingCards_GroupingDisabled verifies that grouping
// is NOT applied when streaming pending cards if disabled.
func TestCardStreamer_StreamPendingCards_GroupingDisabled(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Pre-populate store with a pending card
	diffContent := "@@ -10,3 +10,4 @@\n context\n+added1\n@@ -50,2 +51,3 @@\n more\n+added2"
	card := &storage.ReviewCard{
		ID:        "card-for-reconnect-disabled",
		SessionID: "test-session",
		File:      "test.go",
		Diff:      diffContent,
		Status:    storage.CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("failed to save card: %v", err)
	}

	broadcaster := newMockBroadcaster()

	streamer := NewCardStreamer(CardStreamerConfig{
		Store:                  store,
		ChunkStore:             store,
		Broadcaster:            broadcaster,
		SessionID:              "test-session",
		ChunkGroupingEnabled:   false, // Disabled
		ChunkGroupingProximity: 20,
	})

	// Stream pending cards
	if err := streamer.StreamPendingCards(); err != nil {
		t.Fatalf("StreamPendingCards() error: %v", err)
	}

	cards := broadcaster.getCards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 broadcast card, got %d", len(cards))
	}

	streamedCard := cards[0]

	// Verify chunk_groups is nil/empty when grouping disabled
	if len(streamedCard.ChunkGroups) != 0 {
		t.Errorf("expected chunk_groups to be empty on reconnect when grouping disabled, got %d", len(streamedCard.ChunkGroups))
	}

	// Chunks should still be present
	if len(streamedCard.Chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(streamedCard.Chunks))
	}
}
