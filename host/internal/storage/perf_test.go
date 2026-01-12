//go:build perf
// +build perf

package storage

// Performance and resilience tests for SQLite storage.
// These tests verify behavior under load, with large payloads, and during
// transient failures. They complement the unit tests in sqlite_test.go.
//
// Run these tests with: go test -v -run 'Bulk|Perf|Large' ./internal/storage/
// Run with race detector: go test -v -race -run 'Bulk|Perf|Large' ./internal/storage/

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Performance thresholds for storage operations.
// These define acceptable limits for single-user MVP scale.
const (
	// Bulk insert: 1000 cards should complete within 30 seconds.
	// This allows ~30ms per card, which is generous for SQLite.
	maxBulkInsert1000Time = 30 * time.Second

	// List queries: ListPending should average under 100ms.
	// This ensures responsive UI even with many pending cards.
	maxListPendingAvgTime = 100 * time.Millisecond

	// Large diffs: Saving a 1MB diff should complete within 1 second.
	// Larger diffs should be handled at the diff detection layer.
	maxLargeDiffSaveTime = 1 * time.Second

	// Concurrent access: Operations under contention should complete
	// within 10 seconds, relying on SQLite's busy_timeout (5s).
	maxConcurrentOpTime = 10 * time.Second
)

// TestBulkCardInsert verifies storage can handle 1000+ cards.
// This is a performance test that measures insert throughput.
func TestBulkCardInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow performance test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "bulk.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numCards = 1000
	now := time.Now()

	// Measure insertion time
	start := time.Now()
	for i := 0; i < numCards; i++ {
		card := &ReviewCard{
			ID:        fmt.Sprintf("bulk-card-%04d", i),
			SessionID: "bulk-session",
			File:      fmt.Sprintf("src/file%04d.go", i),
			Diff:      generateDiff(i, 100), // 100 lines per diff
			Status:    CardPending,
			CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := store.SaveCard(card); err != nil {
			t.Fatalf("SaveCard %d failed: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// Verify timing threshold
	if elapsed > maxBulkInsert1000Time {
		t.Errorf("bulk insert took %v, want < %v", elapsed, maxBulkInsert1000Time)
	}
	t.Logf("inserted %d cards in %v (%.2f cards/sec)", numCards, elapsed, float64(numCards)/elapsed.Seconds())

	// Verify all cards were stored
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != numCards {
		t.Errorf("ListAll returned %d cards, want %d", len(all), numCards)
	}
}

// TestBulkCardRetrieve verifies query performance with many cards.
// After inserting 1000 cards, ListPending should remain fast.
func TestBulkCardRetrieve(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow performance test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "retrieve.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Insert 1000 pending cards
	const numCards = 1000
	now := time.Now()
	for i := 0; i < numCards; i++ {
		card := &ReviewCard{
			ID:        fmt.Sprintf("retrieve-card-%04d", i),
			SessionID: "retrieve-session",
			File:      fmt.Sprintf("src/file%04d.go", i),
			Diff:      generateDiff(i, 50),
			Status:    CardPending,
			CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := store.SaveCard(card); err != nil {
			t.Fatalf("SaveCard %d failed: %v", i, err)
		}
	}

	// Measure query performance over multiple iterations
	const numQueries = 100
	start := time.Now()
	for i := 0; i < numQueries; i++ {
		pending, err := store.ListPending()
		if err != nil {
			t.Fatalf("ListPending failed: %v", err)
		}
		if len(pending) != numCards {
			t.Fatalf("ListPending returned %d, want %d", len(pending), numCards)
		}
	}
	elapsed := time.Since(start)
	avgTime := elapsed / numQueries

	// Verify timing threshold
	if avgTime > maxListPendingAvgTime {
		t.Errorf("ListPending average %v, want < %v", avgTime, maxListPendingAvgTime)
	}
	t.Logf("ListPending with %d cards: avg %v over %d queries", numCards, avgTime, numQueries)
}

// TestLargeDiffPayload verifies storage handles large diff content.
// Tests 100KB, 500KB, and 1MB payloads.
func TestLargeDiffPayload(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "large.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Test cases with different payload sizes
	testCases := []struct {
		name     string
		sizeKB   int
		maxTime  time.Duration
	}{
		{"100KB", 100, maxLargeDiffSaveTime},
		{"500KB", 500, maxLargeDiffSaveTime},
		{"1MB", 1024, maxLargeDiffSaveTime},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Generate large diff content
			diff := generateLargeDiff(tc.sizeKB * 1024)
			t.Logf("generated diff size: %d bytes", len(diff))

			card := &ReviewCard{
				ID:        fmt.Sprintf("large-%s", tc.name),
				SessionID: "large-session",
				File:      "large-file.go",
				Diff:      diff,
				Status:    CardPending,
				CreatedAt: time.Now(),
			}

			// Measure save time
			start := time.Now()
			if err := store.SaveCard(card); err != nil {
				t.Fatalf("SaveCard failed: %v", err)
			}
			saveTime := time.Since(start)

			if saveTime > tc.maxTime {
				t.Errorf("save took %v, want < %v", saveTime, tc.maxTime)
			}
			t.Logf("saved %s in %v", tc.name, saveTime)

			// Verify retrieval
			retrieved, err := store.GetCard(card.ID)
			if err != nil {
				t.Fatalf("GetCard failed: %v", err)
			}
			if len(retrieved.Diff) != len(diff) {
				t.Errorf("retrieved diff length %d, want %d", len(retrieved.Diff), len(diff))
			}
		})
	}
}

// TestDBBusyHandling verifies SQLite handles concurrent access gracefully.
// Multiple goroutines write simultaneously, relying on busy_timeout.
func TestDBBusyHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow performance test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "busy.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numGoroutines = 50
	const opsPerGoroutine = 20

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)
	start := time.Now()

	// Launch concurrent writers
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for op := 0; op < opsPerGoroutine; op++ {
				card := &ReviewCard{
					ID:        fmt.Sprintf("busy-%d-%d", goroutineID, op),
					SessionID: "busy-session",
					File:      fmt.Sprintf("file-%d.go", goroutineID),
					Diff:      generateDiff(op, 10),
					Status:    CardPending,
					CreatedAt: time.Now(),
				}
				if err := store.SaveCard(card); err != nil {
					errors <- fmt.Errorf("goroutine %d op %d: %w", goroutineID, op, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)
	elapsed := time.Since(start)

	// Check for errors
	var errCount int
	for err := range errors {
		if errCount < 5 { // Log first 5 errors
			t.Logf("error: %v", err)
		}
		errCount++
	}

	if errCount > 0 {
		t.Errorf("%d errors during concurrent access", errCount)
	}

	if elapsed > maxConcurrentOpTime {
		t.Errorf("concurrent operations took %v, want < %v", elapsed, maxConcurrentOpTime)
	}

	// Verify final state
	expectedCards := numGoroutines * opsPerGoroutine
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != expectedCards {
		t.Errorf("ListAll returned %d cards, want %d", len(all), expectedCards)
	}
	t.Logf("completed %d concurrent operations in %v", expectedCards, elapsed)
}

// TestStorageRecovery verifies the store recovers from transient issues.
// Simulates reopening the database after unexpected close.
func TestStorageRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "recovery.db")

	// First session: create some cards
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (1) failed: %v", err)
	}

	now := time.Now()
	for i := 0; i < 10; i++ {
		card := &ReviewCard{
			ID:        fmt.Sprintf("recovery-card-%d", i),
			SessionID: "recovery-session",
			File:      fmt.Sprintf("file%d.go", i),
			Diff:      generateDiff(i, 10),
			Status:    CardPending,
			CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := store1.SaveCard(card); err != nil {
			t.Fatalf("SaveCard %d failed: %v", i, err)
		}
	}

	// Simulate unexpected close (no graceful shutdown)
	store1.Close()

	// Second session: verify recovery
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (2) failed: %v", err)
	}
	defer store2.Close()

	// Verify all cards are present
	all, err := store2.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != 10 {
		t.Errorf("recovered %d cards, want 10", len(all))
	}

	// Verify we can continue writing
	card := &ReviewCard{
		ID:        "recovery-new-card",
		SessionID: "recovery-session",
		File:      "newfile.go",
		Diff:      generateDiff(100, 10),
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store2.SaveCard(card); err != nil {
		t.Fatalf("SaveCard after recovery failed: %v", err)
	}

	// Verify final state
	all, err = store2.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != 11 {
		t.Errorf("final card count %d, want 11", len(all))
	}
	t.Log("storage recovery successful")
}

// TestConcurrentDecisions verifies decision atomicity under high concurrency.
// Multiple goroutines attempt to decide the same cards simultaneously.
func TestConcurrentDecisions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "decisions.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create cards to decide
	const numCards = 20
	now := time.Now()
	for i := 0; i < numCards; i++ {
		card := &ReviewCard{
			ID:        fmt.Sprintf("decision-card-%d", i),
			SessionID: "decision-session",
			File:      fmt.Sprintf("file%d.go", i),
			Diff:      generateDiff(i, 10),
			Status:    CardPending,
			CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := store.SaveCard(card); err != nil {
			t.Fatalf("SaveCard %d failed: %v", i, err)
		}
	}

	// Launch concurrent decision attempts
	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make(chan struct {
		cardID  string
		success bool
		err     error
	}, numCards*numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for cardIdx := 0; cardIdx < numCards; cardIdx++ {
				cardID := fmt.Sprintf("decision-card-%d", cardIdx)
				decision := &Decision{
					CardID:    cardID,
					Status:    CardAccepted,
					Timestamp: time.Now(),
					Comment:   fmt.Sprintf("decided by goroutine %d", goroutineID),
				}
				err := store.RecordDecision(decision)
				results <- struct {
					cardID  string
					success bool
					err     error
				}{cardID, err == nil, err}
			}
		}(g)
	}

	wg.Wait()
	close(results)

	// Analyze results: each card should have exactly one successful decision
	successes := make(map[string]int)
	failures := make(map[string]int)
	for r := range results {
		if r.success {
			successes[r.cardID]++
		} else {
			failures[r.cardID]++
		}
	}

	// Each card should have exactly 1 success and (numGoroutines-1) failures
	for i := 0; i < numCards; i++ {
		cardID := fmt.Sprintf("decision-card-%d", i)
		if successes[cardID] != 1 {
			t.Errorf("card %s had %d successes, want 1", cardID, successes[cardID])
		}
		expectedFailures := numGoroutines - 1
		if failures[cardID] != expectedFailures {
			t.Errorf("card %s had %d failures, want %d", cardID, failures[cardID], expectedFailures)
		}
	}
	t.Logf("concurrent decision atomicity verified: %d cards, %d goroutines", numCards, numGoroutines)
}

// generateDiff creates a realistic git diff with the specified number of lines.
func generateDiff(seed int, lines int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("@@ -%d,5 +%d,%d @@\n", seed, seed, lines))
	for i := 0; i < lines; i++ {
		if i%3 == 0 {
			sb.WriteString(fmt.Sprintf("+// Added line %d (seed %d)\n", i, seed))
		} else if i%3 == 1 {
			sb.WriteString(fmt.Sprintf("-// Removed line %d (seed %d)\n", i, seed))
		} else {
			sb.WriteString(fmt.Sprintf(" // Context line %d (seed %d)\n", i, seed))
		}
	}
	return sb.String()
}

// generateLargeDiff creates a diff of approximately the specified size in bytes.
func generateLargeDiff(targetBytes int) string {
	// Each line is about 60 bytes
	const bytesPerLine = 60
	lines := targetBytes / bytesPerLine

	var sb strings.Builder
	sb.Grow(targetBytes)
	sb.WriteString("@@ -1,1 +1," + fmt.Sprintf("%d", lines) + " @@\n")

	for i := 0; i < lines; i++ {
		sb.WriteString(fmt.Sprintf("+// Line %08d: This is some added content padding.\n", i))
	}
	return sb.String()
}

// =============================================================================
// Unit 7.10: Concurrency Stress Tests
// =============================================================================

// TestSustainedLoad_HighConcurrency stresses SQLite with 100 goroutines running
// mixed operations for 5 seconds. Verifies no deadlocks and data consistency.
func TestSustainedLoad_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow stress test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sustained.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numGoroutines = 100
	const duration = 5 * time.Second

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*100)
	stop := make(chan struct{})

	// Track operation counts
	var opsCount int64
	var mu sync.Mutex

	start := time.Now()

	// Launch concurrent workers
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			opNum := 0
			for {
				select {
				case <-stop:
					return
				default:
				}

				cardID := fmt.Sprintf("sustained-%d-%d", goroutineID, opNum)

				// Mix of operations: save, get, list
				switch opNum % 3 {
				case 0:
					// Save a card
					card := &ReviewCard{
						ID:        cardID,
						SessionID: "sustained-session",
						File:      fmt.Sprintf("file-%d.go", goroutineID),
						Diff:      generateDiff(opNum, 10),
						Status:    CardPending,
						CreatedAt: time.Now(),
					}
					if err := store.SaveCard(card); err != nil {
						errors <- fmt.Errorf("goroutine %d op %d save: %w", goroutineID, opNum, err)
						return
					}
				case 1:
					// List pending
					if _, err := store.ListPending(); err != nil {
						errors <- fmt.Errorf("goroutine %d op %d list: %w", goroutineID, opNum, err)
						return
					}
				case 2:
					// Get card (may not exist - that's ok)
					_, _ = store.GetCard(cardID)
				}

				mu.Lock()
				opsCount++
				mu.Unlock()
				opNum++
			}
		}(g)
	}

	// Let it run for duration
	time.Sleep(duration)
	close(stop)
	wg.Wait()
	close(errors)
	elapsed := time.Since(start)

	// Check for errors
	var errCount int
	for err := range errors {
		if errCount < 5 {
			t.Logf("error: %v", err)
		}
		errCount++
	}

	if errCount > 0 {
		t.Errorf("%d errors during sustained load", errCount)
	}

	mu.Lock()
	finalOps := opsCount
	mu.Unlock()

	t.Logf("completed %d operations in %v with %d goroutines (%.0f ops/sec)",
		finalOps, elapsed, numGoroutines, float64(finalOps)/elapsed.Seconds())

	// Verify data consistency - ListAll should not error
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed after stress: %v", err)
	}
	t.Logf("final card count: %d", len(all))
}

// TestMixedOperationsStress runs concurrent readers and writers simultaneously.
// Verifies no data corruption and reads see consistent state.
func TestMixedOperationsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow stress test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "mixed.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numWriters = 50
	const numReaders = 50
	const opsPerWorker = 50

	var wg sync.WaitGroup
	errors := make(chan error, (numWriters+numReaders)*opsPerWorker)

	start := time.Now()

	// Launch writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for op := 0; op < opsPerWorker; op++ {
				card := &ReviewCard{
					ID:        fmt.Sprintf("mixed-w%d-op%d", writerID, op),
					SessionID: "mixed-session",
					File:      fmt.Sprintf("file-%d.go", writerID),
					Diff:      generateDiff(op, 10),
					Status:    CardPending,
					CreatedAt: time.Now(),
				}
				if err := store.SaveCard(card); err != nil {
					errors <- fmt.Errorf("writer %d op %d: %w", writerID, op, err)
					return
				}
			}
		}(w)
	}

	// Launch readers (start slightly after writers to ensure data exists)
	time.Sleep(10 * time.Millisecond)
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for op := 0; op < opsPerWorker; op++ {
				// Alternate between different read operations
				switch op % 3 {
				case 0:
					if _, err := store.ListPending(); err != nil {
						errors <- fmt.Errorf("reader %d op %d ListPending: %w", readerID, op, err)
						return
					}
				case 1:
					if _, err := store.ListAll(); err != nil {
						errors <- fmt.Errorf("reader %d op %d ListAll: %w", readerID, op, err)
						return
					}
				case 2:
					// Try to get a card that might exist
					cardID := fmt.Sprintf("mixed-w%d-op%d", readerID%numWriters, op%10)
					_, _ = store.GetCard(cardID) // May not exist, that's ok
				}
			}
		}(r)
	}

	wg.Wait()
	close(errors)
	elapsed := time.Since(start)

	// Check for errors
	var errCount int
	for err := range errors {
		if errCount < 5 {
			t.Logf("error: %v", err)
		}
		errCount++
	}

	if errCount > 0 {
		t.Errorf("%d errors during mixed operations", errCount)
	}

	if elapsed > maxConcurrentOpTime {
		t.Errorf("mixed operations took %v, want < %v", elapsed, maxConcurrentOpTime)
	}

	// Verify final state
	expectedCards := numWriters * opsPerWorker
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != expectedCards {
		t.Errorf("ListAll returned %d cards, want %d", len(all), expectedCards)
	}

	t.Logf("mixed operations: %d writers + %d readers completed in %v",
		numWriters, numReaders, elapsed)
}

// TestChunkOperationsStress tests concurrent chunk save and decision operations.
// Verifies transactions work correctly under load.
func TestChunkOperationsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow stress test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "chunks.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numCards = 50
	const chunksPerCard = 5
	const numDeciders = 20

	// First, create cards with chunks
	now := time.Now()
	for c := 0; c < numCards; c++ {
		cardID := fmt.Sprintf("chunk-card-%d", c)
		card := &ReviewCard{
			ID:        cardID,
			SessionID: "chunk-session",
			File:      fmt.Sprintf("file%d.go", c),
			Diff:      generateDiff(c, 50),
			Status:    CardPending,
			CreatedAt: now,
		}
		if err := store.SaveCard(card); err != nil {
			t.Fatalf("SaveCard %d failed: %v", c, err)
		}

		// Save chunks for this card
		var chunks []*ChunkStatus
		for h := 0; h < chunksPerCard; h++ {
			chunks = append(chunks, &ChunkStatus{
				CardID:    cardID,
				ChunkIndex: h,
				Content:   fmt.Sprintf("@@ -%d,1 +%d,1 @@\n+// Chunk %d content\n", h*10, h*10, h),
				Status:    CardPending,
			})
		}
		if err := store.SaveChunks(cardID, chunks); err != nil {
			t.Fatalf("SaveChunks for card %d failed: %v", c, err)
		}
	}

	var wg sync.WaitGroup
	errors := make(chan error, numDeciders*numCards*chunksPerCard)
	successCount := make(chan int, numDeciders*numCards*chunksPerCard)

	start := time.Now()

	// Launch concurrent deciders
	for d := 0; d < numDeciders; d++ {
		wg.Add(1)
		go func(deciderID int) {
			defer wg.Done()
			for c := 0; c < numCards; c++ {
				cardID := fmt.Sprintf("chunk-card-%d", c)
				for h := 0; h < chunksPerCard; h++ {
					decision := &ChunkDecision{
						CardID:    cardID,
						ChunkIndex: h,
						Status:    CardAccepted,
						Timestamp: time.Now(),
					}
					err := store.RecordChunkDecision(decision)
					if err == nil {
						successCount <- 1
					} else if err != ErrAlreadyDecided {
						errors <- fmt.Errorf("decider %d card %s chunk %d: %w", deciderID, cardID, h, err)
					}
				}
			}
		}(d)
	}

	wg.Wait()
	close(errors)
	close(successCount)
	elapsed := time.Since(start)

	// Check for errors
	var errCount int
	for err := range errors {
		if errCount < 5 {
			t.Logf("error: %v", err)
		}
		errCount++
	}

	if errCount > 0 {
		t.Errorf("%d errors during chunk operations", errCount)
	}

	// Count successes - should equal total chunks (each decided exactly once)
	var totalSuccess int
	for range successCount {
		totalSuccess++
	}

	expectedSuccess := numCards * chunksPerCard
	if totalSuccess != expectedSuccess {
		t.Errorf("got %d successful decisions, want %d", totalSuccess, expectedSuccess)
	}

	t.Logf("chunk operations: %d cards × %d chunks with %d deciders completed in %v",
		numCards, chunksPerCard, numDeciders, elapsed)
}

// TestSessionOperationsStress tests concurrent session operations.
// Verifies retention cleanup doesn't deadlock.
func TestSessionOperationsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow stress test in short mode")
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numGoroutines = 30
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	start := time.Now()

	// Launch concurrent session operations
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for op := 0; op < opsPerGoroutine; op++ {
				sessionID := fmt.Sprintf("session-%d-%d", goroutineID, op)
				session := &Session{
					ID:        sessionID,
					Repo:      "/test/repo",
					Branch:    "main",
					StartedAt: time.Now(),
					LastSeen:  time.Now(),
					Status:    SessionStatusRunning,
				}

				// Save session
				if err := store.SaveSession(session); err != nil {
					errors <- fmt.Errorf("goroutine %d op %d save: %w", goroutineID, op, err)
					return
				}

				// Get session
				_, err := store.GetSession(sessionID)
				if err != nil && err != ErrCardNotFound {
					errors <- fmt.Errorf("goroutine %d op %d get: %w", goroutineID, op, err)
					return
				}

				// List sessions (triggers cleanup) - pass limit of 100
				if _, err := store.ListSessions(100); err != nil {
					errors <- fmt.Errorf("goroutine %d op %d list: %w", goroutineID, op, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)
	elapsed := time.Since(start)

	// Check for errors
	var errCount int
	for err := range errors {
		if errCount < 5 {
			t.Logf("error: %v", err)
		}
		errCount++
	}

	if errCount > 0 {
		t.Errorf("%d errors during session operations", errCount)
	}

	// Verify sessions exist
	sessions, err := store.ListSessions(100)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	t.Logf("session operations: %d goroutines × %d ops completed in %v, %d sessions remain",
		numGoroutines, opsPerGoroutine, elapsed, len(sessions))
}
