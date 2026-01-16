package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupTestRepo creates a temporary git repository for testing.
func setupTestRepo(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "diff-poller-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("failed to git init: %v", err)
	}

	// Configure git user for commits
	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = dir
	cmd.Run()

	// Create initial file and commit
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\n"), 0644); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("failed to write test file: %v", err)
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = dir
	cmd.Run()

	cmd = exec.Command("git", "commit", "-m", "initial commit")
	cmd.Dir = dir
	cmd.Run()

	return dir
}

func TestPoller_NoDiff(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
	})

	// Poll once with no changes
	chunks, err := p.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for clean repo, got %d", len(chunks))
	}
}

func TestPoller_DetectsChange(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Modify the file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nadded line\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
	})

	chunks, err := p.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	if chunks[0].File != "test.txt" {
		t.Errorf("expected file 'test.txt', got '%s'", chunks[0].File)
	}
}

func TestPoller_ChangeDetection(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	callCount := 0
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 30 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			mu.Unlock()
		},
	})

	// Modify the file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("modified content\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	p.Start()
	defer p.Stop()

	// Wait for a few poll cycles
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Should only be called once since diff doesn't change between polls
	if count != 1 {
		t.Errorf("expected OnChunks called 1 time (change detection), got %d", count)
	}
}

func TestPoller_MultipleChanges(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	var allChunks [][]*Chunk
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 30 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			allChunks = append(allChunks, chunks)
			mu.Unlock()
		},
	})

	testFile := filepath.Join(dir, "test.txt")

	// First change
	if err := os.WriteFile(testFile, []byte("change 1\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	p.Start()

	// Wait for first poll
	time.Sleep(50 * time.Millisecond)

	// Second change
	if err := os.WriteFile(testFile, []byte("change 2\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// Wait for second poll
	time.Sleep(50 * time.Millisecond)

	p.Stop()

	mu.Lock()
	chunkSets := len(allChunks)
	mu.Unlock()

	// Should have been called twice (once per change)
	if chunkSets < 2 {
		t.Errorf("expected at least 2 OnChunks calls for 2 changes, got %d", chunkSets)
	}
}

func TestPoller_StartStop(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 10 * time.Millisecond,
	})

	p.Start()

	// Starting again should be a no-op
	p.Start()

	time.Sleep(30 * time.Millisecond)

	p.Stop()

	// Stopping again should be a no-op
	p.Stop()

	// Verify done channel is closed
	select {
	case <-p.Done():
		// Good
	default:
		t.Error("Done channel should be closed after Stop")
	}
}

func TestPoller_StartDuringStopBlocked(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("change 1\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	var mu sync.Mutex
	callCount := 0
	firstCall := make(chan struct{})
	release := make(chan struct{})

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 10 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			if callCount == 1 {
				close(firstCall)
				mu.Unlock()
				<-release
				return
			}
			mu.Unlock()
		},
	})

	p.Start()

	select {
	case <-firstCall:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected OnChunks to be called before Stop")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("expected Stop to block until polling loop exits")
	case <-time.After(20 * time.Millisecond):
	}

	// Start should no-op while Stop is in progress.
	p.Start()

	close(release)

	select {
	case <-stopDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected Stop to return after release")
	}

	mu.Lock()
	initialCalls := callCount
	mu.Unlock()

	if err := os.WriteFile(testFile, []byte("change 2\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	afterStopCalls := callCount
	mu.Unlock()

	if afterStopCalls != initialCalls {
		t.Errorf("expected no OnChunks calls after Stop, got %d -> %d", initialCalls, afterStopCalls)
	}
}

func TestPoller_RestartAfterStop(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	callCount := 0
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 20 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			mu.Unlock()
		},
	})

	// Modify file to trigger OnChunks
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("change 1\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// First run
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	mu.Lock()
	firstRunCount := callCount
	mu.Unlock()

	if firstRunCount == 0 {
		t.Error("expected OnChunks to be called in first run")
	}

	// Modify file again before restart
	if err := os.WriteFile(testFile, []byte("change 2\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// Second run - should NOT panic
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	mu.Lock()
	secondRunCount := callCount
	mu.Unlock()

	// OnChunks should have been called again in the second run
	if secondRunCount <= firstRunCount {
		t.Errorf("expected OnChunks to be called in second run: first=%d, total=%d",
			firstRunCount, secondRunCount)
	}

	// Verify Done() returns a closed channel after second stop
	select {
	case <-p.Done():
		// Good
	default:
		t.Error("Done channel should be closed after second Stop")
	}
}

func TestChunk_IDGeneration(t *testing.T) {
	h1 := NewChunk("file.go", 1, 3, 1, 4, "+new line")
	h2 := NewChunk("file.go", 1, 3, 1, 4, "+new line")
	h3 := NewChunk("file.go", 1, 3, 1, 4, "+different content")

	// Same content should produce same ID
	if h1.ID != h2.ID {
		t.Errorf("identical chunks should have same ID: %s != %s", h1.ID, h2.ID)
	}

	// Different content should produce different ID
	if h1.ID == h3.ID {
		t.Errorf("different chunks should have different IDs")
	}

	// ID should be 12 chars (truncated hash)
	if len(h1.ID) != 12 {
		t.Errorf("expected ID length 12, got %d", len(h1.ID))
	}
}

func TestPoller_GitFailure_NonGitDirectory(t *testing.T) {
	// Create a temp directory that is NOT a git repo
	dir, err := os.MkdirTemp("", "not-a-git-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	var receivedError error
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
		OnError: func(err error) {
			mu.Lock()
			receivedError = err
			mu.Unlock()
		},
	})

	// PollOnce should return an error
	chunks, err := p.PollOnce()
	if err == nil {
		t.Error("expected error for non-git directory, got nil")
	}
	if chunks != nil {
		t.Errorf("expected nil chunks on error, got %d", len(chunks))
	}

	// Start the poller and verify OnError is called
	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	mu.Lock()
	gotError := receivedError
	mu.Unlock()

	if gotError == nil {
		t.Error("expected OnError to be called for non-git directory")
	}
}

func TestPoller_GitFailure_NoOnErrorCallback(t *testing.T) {
	// Create a temp directory that is NOT a git repo
	dir, err := os.MkdirTemp("", "not-a-git-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	// No OnError callback - should not panic
	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
	})

	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()
	// Test passes if no panic occurred
}

func TestPoller_ZeroPollInterval(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Zero interval should use a sensible default (not panic or spin)
	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 0,
	})

	// Modify the file to have something to detect
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("modified\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// PollOnce should still work
	chunks, err := p.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed with zero interval: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestPoller_NegativePollInterval(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Negative interval should be treated as zero/default
	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: -100 * time.Millisecond,
	})

	// Modify the file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("modified\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// PollOnce should still work
	chunks, err := p.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed with negative interval: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestPoller_EmptyRepoPath(t *testing.T) {
	// Empty repo path should use current directory or error gracefully
	p := NewPoller(PollerConfig{
		RepoPath:     "",
		PollInterval: 50 * time.Millisecond,
	})

	// PollOnce will either succeed (if CWD is a git repo) or fail gracefully
	_, err := p.PollOnce()
	// We don't assert success or failure - just verify no panic
	t.Logf("PollOnce with empty path result: %v", err)
}

func TestPoller_NonExistentRepoPath(t *testing.T) {
	p := NewPoller(PollerConfig{
		RepoPath:     "/path/that/does/not/exist/anywhere",
		PollInterval: 50 * time.Millisecond,
	})

	// Should fail gracefully
	_, err := p.PollOnce()
	if err == nil {
		t.Error("expected error for non-existent path, got nil")
	}
}

func TestPoller_IncludeStaged(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Create a new file and stage it (but don't commit)
	stagedFile := filepath.Join(dir, "staged.txt")
	if err := os.WriteFile(stagedFile, []byte("staged content\n"), 0644); err != nil {
		t.Fatalf("failed to write staged file: %v", err)
	}

	cmd := exec.Command("git", "add", "staged.txt")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}

	// Also modify an existing file but don't stage it
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("modified unstaged\n"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// Test WITHOUT IncludeStaged - should only see unstaged change
	p1 := NewPoller(PollerConfig{
		RepoPath:      dir,
		PollInterval:  50 * time.Millisecond,
		IncludeStaged: false,
	})

	chunks1, err := p1.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	// Should only see test.txt (unstaged)
	if len(chunks1) != 1 {
		t.Errorf("expected 1 chunk without IncludeStaged, got %d", len(chunks1))
	}
	if len(chunks1) > 0 && chunks1[0].File != "test.txt" {
		t.Errorf("expected file 'test.txt', got '%s'", chunks1[0].File)
	}

	// Test WITH IncludeStaged - should see both changes
	p2 := NewPoller(PollerConfig{
		RepoPath:      dir,
		PollInterval:  50 * time.Millisecond,
		IncludeStaged: true,
	})

	chunks2, err := p2.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	// Should see both test.txt (unstaged) and staged.txt (staged)
	if len(chunks2) != 2 {
		t.Errorf("expected 2 chunks with IncludeStaged, got %d", len(chunks2))
	}

	// Verify we have both files
	files := make(map[string]bool)
	for _, h := range chunks2 {
		files[h.File] = true
	}
	if !files["test.txt"] {
		t.Error("expected to find test.txt in chunks")
	}
	if !files["staged.txt"] {
		t.Error("expected to find staged.txt in chunks")
	}
}

func TestPoller_ExcludePaths_SkipsUntracked(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	untrackedFile := filepath.Join(dir, "ignored.db")
	if err := os.WriteFile(untrackedFile, []byte("ignored content\n"), 0644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
		ExcludePaths:     []string{"ignored.db"},
	})

	chunks, rawDiff, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	if strings.Contains(rawDiff, "ignored.db") {
		t.Errorf("expected excluded file to be omitted, got raw diff: %s", rawDiff)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks when only excluded files exist, got %d", len(chunks))
	}
}

// TestPoller_OnChunksRaw_Precedence tests that OnChunksRaw is called instead of
// OnChunks when both are configured.
func TestPoller_OnChunksRaw_Precedence(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Modify a file to create a diff
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nmodified line\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	var onChunksCalled bool
	var onChunksRawCalled bool
	var receivedRawDiff string

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			onChunksCalled = true
		},
		OnChunksRaw: func(chunks []*Chunk, rawDiff string) {
			onChunksRawCalled = true
			receivedRawDiff = rawDiff
		},
	})

	// Start and let it poll once
	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	// OnChunksRaw should be called, NOT OnChunks
	if onChunksCalled {
		t.Error("OnChunks should NOT be called when OnChunksRaw is configured")
	}
	if !onChunksRawCalled {
		t.Error("OnChunksRaw should be called")
	}
	if receivedRawDiff == "" {
		t.Error("expected raw diff to be non-empty")
	}
	if !strings.Contains(receivedRawDiff, "test.txt") {
		t.Error("expected raw diff to contain test.txt")
	}
}

// TestPoller_OnChunksRaw_IncludesRawOutput tests that OnChunksRaw receives
// the complete raw git diff output for binary file detection.
func TestPoller_OnChunksRaw_IncludesRawOutput(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Modify a file to create a diff
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nnew line added\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	var receivedChunks []*Chunk
	var receivedRawDiff string
	var callCount int
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
		OnChunksRaw: func(chunks []*Chunk, rawDiff string) {
			mu.Lock()
			callCount++
			receivedChunks = chunks
			receivedRawDiff = rawDiff
			mu.Unlock()
		},
	})

	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	if callCount == 0 {
		t.Fatal("OnChunksRaw was never called")
	}

	if len(receivedChunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(receivedChunks))
	}

	// Raw diff should contain git diff headers
	if !strings.Contains(receivedRawDiff, "diff --git") {
		t.Error("expected raw diff to contain 'diff --git' header")
	}
	if !strings.Contains(receivedRawDiff, "@@") {
		t.Error("expected raw diff to contain @@ chunk header")
	}
}

// TestPollOnceRaw_ReturnsRawDiff tests that PollOnceRaw returns both
// chunks and raw diff output.
func TestPollOnceRaw_ReturnsRawDiff(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Modify a file
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\nmodified\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 50 * time.Millisecond,
	})

	chunks, rawDiff, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}

	if rawDiff == "" {
		t.Error("expected non-empty raw diff")
	}

	if !strings.Contains(rawDiff, "diff --git") {
		t.Error("expected raw diff to contain git header")
	}
}

// TestPoller_SnapshotRaw_DoesNotUpdateHash ensures SnapshotRaw does not
// suppress the next polling tick when changes already exist.
func TestPoller_SnapshotRaw_DoesNotUpdateHash(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Create an untracked file to generate a diff
	untrackedFile := filepath.Join(dir, "snapshot.txt")
	if err := os.WriteFile(untrackedFile, []byte("snapshot content\n"), 0644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	var once sync.Once
	handlerCalled := make(chan struct{})

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
		OnChunks: func(chunks []*Chunk) {
			if len(chunks) == 0 {
				return
			}
			once.Do(func() {
				close(handlerCalled)
			})
		},
	})

	// Snapshot should not update the poller's hash.
	if _, _, err := p.SnapshotRaw(); err != nil {
		t.Fatalf("SnapshotRaw failed: %v", err)
	}

	p.Start()
	defer p.Stop()

	select {
	case <-handlerCalled:
		// Good - initial poll still emitted chunks.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected poller to emit chunks after SnapshotRaw")
	}
}

// =============================================================================
// Unit 7.10: Concurrency Stress Tests - Diff Poller
// =============================================================================

// TestPoller_SlowHandlerBlocking verifies the poller doesn't deadlock when the
// handler takes longer than the poll interval.
func TestPoller_SlowHandlerBlocking(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Modify file to create a diff
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("slow handler test\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	const pollInterval = 50 * time.Millisecond
	const handlerDelay = 200 * time.Millisecond // 4x the poll interval

	var callCount int
	var mu sync.Mutex
	handlerDone := make(chan struct{})

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: pollInterval,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			count := callCount
			mu.Unlock()

			// First call blocks for longer than poll interval
			if count == 1 {
				time.Sleep(handlerDelay)
				close(handlerDone)
			}
		},
	})

	p.Start()

	// Wait for the slow handler to complete
	select {
	case <-handlerDone:
		// Good - handler completed
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not complete in time")
	}

	// Give some time for any queued polls
	time.Sleep(100 * time.Millisecond)

	p.Stop()

	mu.Lock()
	finalCount := callCount
	mu.Unlock()

	// Should have been called at least once (the blocking call)
	if finalCount < 1 {
		t.Errorf("expected at least 1 handler call, got %d", finalCount)
	}

	// Verify poller stopped cleanly
	select {
	case <-p.Done():
		// Good
	case <-time.After(time.Second):
		t.Error("poller did not stop cleanly after slow handler")
	}
}

// TestPoller_SlowHandlerExceedsInterval tests that polls don't queue up when
// the handler takes longer than the poll interval.
func TestPoller_SlowHandlerExceedsInterval(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.txt")

	const pollInterval = 30 * time.Millisecond
	const handlerDelay = 100 * time.Millisecond // > 3x poll interval

	var callCount int
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: pollInterval,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			mu.Unlock()
			time.Sleep(handlerDelay)
		},
	})

	// Create initial change
	if err := os.WriteFile(testFile, []byte("change 1\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	p.Start()

	// Run for a fixed duration
	time.Sleep(500 * time.Millisecond)

	p.Stop()

	mu.Lock()
	finalCount := callCount
	mu.Unlock()

	// With 500ms runtime and 100ms handler, max ~5 calls is reasonable
	// If polls were queuing up, we'd see many more calls
	if finalCount > 10 {
		t.Errorf("too many handler calls (%d) - polls may be queuing up", finalCount)
	}

	t.Logf("handler called %d times in 500ms with 100ms handler delay", finalCount)
}

// TestPoller_ConcurrentRapidStartStop tests rapid lifecycle changes don't cause
// race conditions or goroutine leaks.
func TestPoller_ConcurrentRapidStartStop(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("rapid test\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	var callCount int
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 5 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			callCount++
			mu.Unlock()
		},
	})

	// Rapid start/stop cycles
	const cycles = 20
	for i := 0; i < cycles; i++ {
		p.Start()
		time.Sleep(10 * time.Millisecond)
		p.Stop()
	}

	// Final verification - should be stopped
	select {
	case <-p.Done():
		// Good - poller is stopped
	case <-time.After(time.Second):
		t.Error("poller not stopped after rapid cycles")
	}

	// One more start/stop to verify poller is still usable
	if err := os.WriteFile(testFile, []byte("final change\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	p.Start()
	time.Sleep(30 * time.Millisecond)
	p.Stop()

	mu.Lock()
	finalCount := callCount
	mu.Unlock()

	if finalCount == 0 {
		t.Error("expected at least some handler calls during rapid cycles")
	}

	t.Logf("completed %d rapid start/stop cycles with %d handler calls", cycles, finalCount)
}

// TestPoller_HandlerWithError tests that the poller continues operating when
// OnError is provided but OnChunks processing doesn't panic.
func TestPoller_HandlerWithError(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.txt")

	var onChunksCount int
	var onErrorCount int
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 30 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			mu.Lock()
			onChunksCount++
			mu.Unlock()
		},
		OnError: func(err error) {
			mu.Lock()
			onErrorCount++
			mu.Unlock()
		},
	})

	// Create a valid change
	if err := os.WriteFile(testFile, []byte("test error handling\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	mu.Lock()
	chunksCount := onChunksCount
	errCount := onErrorCount
	mu.Unlock()

	// OnChunks should have been called at least once
	if chunksCount == 0 {
		t.Error("expected OnChunks to be called at least once")
	}

	// OnError should not have been called (valid git repo)
	if errCount > 0 {
		t.Errorf("unexpected OnError calls: %d", errCount)
	}

	t.Logf("handler called %d times, error handler called %d times", chunksCount, errCount)
}

// TestPoller_StopDuringSlowHandler verifies Stop() completes even when handler
// is blocked.
func TestPoller_StopDuringSlowHandler(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("stop during slow\n"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	handlerStarted := make(chan struct{})
	handlerRelease := make(chan struct{})
	var handlerCompleted bool
	var mu sync.Mutex

	p := NewPoller(PollerConfig{
		RepoPath:     dir,
		PollInterval: 20 * time.Millisecond,
		OnChunks: func(chunks []*Chunk) {
			close(handlerStarted)
			<-handlerRelease
			mu.Lock()
			handlerCompleted = true
			mu.Unlock()
		},
	})

	p.Start()

	// Wait for handler to start
	select {
	case <-handlerStarted:
		// Good
	case <-time.After(time.Second):
		t.Fatal("handler did not start in time")
	}

	// Start stop in goroutine (will block)
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	// Stop should be waiting
	select {
	case <-stopDone:
		t.Fatal("Stop returned before handler completed")
	case <-time.After(50 * time.Millisecond):
		// Expected - Stop is blocking
	}

	// Release the handler
	close(handlerRelease)

	// Now Stop should complete
	select {
	case <-stopDone:
		// Good
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after handler released")
	}

	mu.Lock()
	completed := handlerCompleted
	mu.Unlock()

	if !completed {
		t.Error("handler did not complete")
	}
}
