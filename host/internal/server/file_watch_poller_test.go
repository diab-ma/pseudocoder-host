package server

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// pollInterval and timing margins for live-poller tests.
// Kept generous so tests are deterministic on slow CI runners
// even when running alongside the full server test suite.
const (
	testPollInterval  = 50 * time.Millisecond
	testBaselineWait  = 250 * time.Millisecond // > 4 poll intervals — baseline + first tick
	testDetectionWait = 500 * time.Millisecond // > 8 poll intervals — ample for change pickup
)

func TestFileWatchPoller_BaselineNoEvents(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0o644)

	var mu sync.Mutex
	var received []FileWatchEvent

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()

	// Wait for baseline + at least one tick.
	time.Sleep(testBaselineWait + testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Fatalf("expected no events on stable baseline, got %d: %v", len(received), received)
	}
}

func TestFileWatchPoller_DetectCreate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("hi"), 0o644)

	var mu sync.Mutex
	var received []FileWatchEvent

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()

	time.Sleep(testBaselineWait)

	// Create a new file.
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644)

	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, ev := range received {
		if ev.Path == "new.txt" && ev.Change == "created" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected created event for new.txt, got: %v", received)
	}
}

func TestFileWatchPoller_DetectModify(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "mod.txt")
	os.WriteFile(filePath, []byte("original"), 0o644)

	var mu sync.Mutex
	var received []FileWatchEvent

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()

	time.Sleep(testBaselineWait)

	// Modify the file (change size ensures detection even if modtime granularity is coarse).
	os.WriteFile(filePath, []byte("modified content now"), 0o644)

	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, ev := range received {
		if ev.Path == "mod.txt" && ev.Change == "modified" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected modified event for mod.txt, got: %v", received)
	}
}

func TestFileWatchPoller_DetectDelete(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "doomed.txt")
	os.WriteFile(filePath, []byte("bye"), 0o644)

	var mu sync.Mutex
	var received []FileWatchEvent

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()

	time.Sleep(testBaselineWait)

	os.Remove(filePath)

	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, ev := range received {
		if ev.Path == "doomed.txt" && ev.Change == "deleted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected deleted event for doomed.txt, got: %v", received)
	}
}

func TestFileWatchPoller_RenameAsDeleteCreate(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.txt")
	newPath := filepath.Join(dir, "new.txt")
	os.WriteFile(oldPath, []byte("content"), 0o644)

	var mu sync.Mutex
	var received []FileWatchEvent

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()
	time.Sleep(testBaselineWait)

	os.Rename(oldPath, newPath)

	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	hasDelete, hasCreate := false, false
	for _, ev := range received {
		if ev.Path == "old.txt" && ev.Change == "deleted" {
			hasDelete = true
		}
		if ev.Path == "new.txt" && ev.Change == "created" {
			hasCreate = true
		}
	}
	if !hasDelete || !hasCreate {
		t.Fatalf("expected delete+create for rename, got: %v", received)
	}
}

func TestFileWatchPoller_DeterministicOrdering(t *testing.T) {
	// Test diffSnapshots directly for ordering guarantees.
	old := map[string]snapshotEntry{
		"b.txt":     {size: 10, modTime: time.Unix(1000, 0)},
		"a.txt":     {size: 10, modTime: time.Unix(1000, 0)},
		"mod-z.txt": {size: 10, modTime: time.Unix(1000, 0)},
		"mod-a.txt": {size: 10, modTime: time.Unix(1000, 0)},
	}
	new := map[string]snapshotEntry{
		// a.txt deleted, b.txt deleted
		"c.txt":     {size: 5, modTime: time.Unix(2000, 0)},
		"d.txt":     {size: 5, modTime: time.Unix(2000, 0)},
		"mod-z.txt": {size: 20, modTime: time.Unix(2000, 0)}, // modified
		"mod-a.txt": {size: 20, modTime: time.Unix(2000, 0)}, // modified
	}

	events := diffSnapshots(old, new, nil)

	// Expected order: deleted(a.txt, b.txt), created(c.txt, d.txt), modified(mod-a.txt, mod-z.txt)
	expected := []FileWatchEvent{
		{Path: "a.txt", Change: "deleted"},
		{Path: "b.txt", Change: "deleted"},
		{Path: "c.txt", Change: "created"},
		{Path: "d.txt", Change: "created"},
		{Path: "mod-a.txt", Change: "modified"},
		{Path: "mod-z.txt", Change: "modified"},
	}

	if len(events) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(events), events)
	}
	for i, ev := range events {
		if ev.Path != expected[i].Path || ev.Change != expected[i].Change {
			t.Errorf("event[%d]: expected {%s %s}, got {%s %s}", i, expected[i].Path, expected[i].Change, ev.Path, ev.Change)
		}
	}
}

func TestFileWatchPoller_GitExclusion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o644)
	os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("yes"), 0o644)

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: time.Hour, // won't tick
	})

	snap, _ := p.scan()

	if _, exists := snap[".git"]; exists {
		t.Error("snapshot should not include .git directory")
	}
	if _, exists := snap[filepath.ToSlash(".git/HEAD")]; exists {
		t.Error("snapshot should not include .git/HEAD")
	}
	if _, exists := snap["visible.txt"]; !exists {
		t.Error("snapshot should include visible.txt")
	}
}

func TestFileWatchPoller_GitFileExclusion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /tmp/shared.git\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatalf("write visible file: %v", err)
	}

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: time.Hour, // won't tick
	})

	snap, _ := p.scan()

	if _, exists := snap[".git"]; exists {
		t.Error("snapshot should not include .git file")
	}
	if _, exists := snap["visible.txt"]; !exists {
		t.Error("snapshot should include visible.txt")
	}
}

func TestFileWatchPoller_TempArtifactExclusion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".pseudocoder-write-abc123"), []byte("temp"), 0o644)
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real"), 0o644)

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: time.Hour,
	})

	snap, _ := p.scan()

	if _, exists := snap[".pseudocoder-write-abc123"]; exists {
		t.Error("snapshot should not include .pseudocoder-write-* temp files")
	}
	if _, exists := snap["real.txt"]; !exists {
		t.Error("snapshot should include real.txt")
	}
}

func TestFileWatchPoller_DirCreatedDeleted(t *testing.T) {
	// Directory create/delete should emit events.
	old := map[string]snapshotEntry{
		"olddir": {isDir: true, modTime: time.Unix(1000, 0)},
	}
	new := map[string]snapshotEntry{
		"newdir": {isDir: true, modTime: time.Unix(2000, 0)},
	}

	events := diffSnapshots(old, new, nil)

	hasDelete, hasCreate := false, false
	for _, ev := range events {
		if ev.Path == "olddir" && ev.Change == "deleted" {
			hasDelete = true
		}
		if ev.Path == "newdir" && ev.Change == "created" {
			hasCreate = true
		}
	}
	if !hasDelete {
		t.Error("expected deleted event for olddir")
	}
	if !hasCreate {
		t.Error("expected created event for newdir")
	}
}

func TestFileWatchPoller_DirModifiedSuppressed(t *testing.T) {
	// Directory modification (modtime change) should NOT produce a modified event.
	old := map[string]snapshotEntry{
		"mydir": {isDir: true, modTime: time.Unix(1000, 0)},
	}
	new := map[string]snapshotEntry{
		"mydir": {isDir: true, modTime: time.Unix(2000, 0)},
	}

	events := diffSnapshots(old, new, nil)

	for _, ev := range events {
		if ev.Path == "mydir" && ev.Change == "modified" {
			t.Fatal("directory modified events should be suppressed")
		}
	}
}

func TestFileWatchPoller_PartialScanSuppressesDescendantDeletes(t *testing.T) {
	// If a directory scan errors (e.g. permission denied), its children vanish
	// from the new snapshot.  Deletes must be suppressed for the errored
	// directory AND all descendants to avoid false inference.
	old := map[string]snapshotEntry{
		"ok.txt":         {size: 5, modTime: time.Unix(1000, 0)},
		"sub":            {isDir: true, modTime: time.Unix(1000, 0)},
		"sub/a.txt":      {size: 5, modTime: time.Unix(1000, 0)},
		"sub/deep":       {isDir: true, modTime: time.Unix(1000, 0)},
		"sub/deep/b.txt": {size: 5, modTime: time.Unix(1000, 0)},
		"other":          {isDir: true, modTime: time.Unix(1000, 0)},
		"other/c.txt":    {size: 5, modTime: time.Unix(1000, 0)},
		"gone.txt":       {size: 5, modTime: time.Unix(1000, 0)},
	}
	// "sub" directory errored → its children and itself vanish from new snapshot.
	// "gone.txt" was genuinely deleted (no error).
	new := map[string]snapshotEntry{
		"ok.txt":      {size: 5, modTime: time.Unix(1000, 0)},
		"other":       {isDir: true, modTime: time.Unix(1000, 0)},
		"other/c.txt": {size: 5, modTime: time.Unix(1000, 0)},
	}
	errPaths := map[string]bool{"sub": true}

	events := diffSnapshots(old, new, errPaths)

	// Only "gone.txt" should appear as deleted; sub/** must be suppressed.
	for _, ev := range events {
		if ev.Change == "deleted" && ev.Path != "gone.txt" {
			t.Errorf("unexpected delete for %q (should be suppressed by errored parent 'sub')", ev.Path)
		}
	}
	foundGone := false
	for _, ev := range events {
		if ev.Path == "gone.txt" && ev.Change == "deleted" {
			foundGone = true
		}
	}
	if !foundGone {
		t.Error("expected genuine delete event for gone.txt")
	}
}

func TestFileWatchPoller_OverlapSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644)

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: time.Hour,
	})

	// Simulate: set scanning=true, then call tick() — should be a no-op.
	p.mu.Lock()
	p.scanning = true
	p.snapshot = map[string]snapshotEntry{} // empty baseline
	p.mu.Unlock()

	// tick should return immediately without scanning.
	p.tick()

	p.mu.Lock()
	defer p.mu.Unlock()
	// scanning should still be true (tick didn't clear it because it skipped).
	if !p.scanning {
		t.Error("expected scanning to remain true when overlap guard fires")
	}
}

func TestFileWatchPoller_StopIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
	})
	p.Start()
	time.Sleep(testBaselineWait)
	p.Stop()

	// Calling Stop again must be a safe no-op and return promptly.
	stoppedAgain := make(chan struct{})
	go func() {
		p.Stop()
		close(stoppedAgain)
	}()

	select {
	case <-stoppedAgain:
	case <-time.After(time.Second):
		t.Fatal("second Stop call did not return promptly")
	}
}

func TestFileWatchPoller_RestartAfterStop(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var received []FileWatchEvent
	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})

	p.Start()
	time.Sleep(testBaselineWait)
	p.Stop()

	p.Start()
	time.Sleep(testBaselineWait)
	if err := os.WriteFile(filepath.Join(dir, "after-restart.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write after-restart.txt: %v", err)
	}
	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, ev := range received {
		if ev.Path == "after-restart.txt" && ev.Change == "created" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected created event after restart, got %v", received)
	}
}

func TestFileWatchPoller_RootScanErrorSuppressesAllDeletes(t *testing.T) {
	// If the repo root itself errors (err path "."), all delete inference must
	// be suppressed for that cycle.
	old := map[string]snapshotEntry{
		"a.txt":     {size: 5, modTime: time.Unix(1000, 0)},
		"sub":       {isDir: true, modTime: time.Unix(1000, 0)},
		"sub/b.txt": {size: 5, modTime: time.Unix(1000, 0)},
	}
	new := map[string]snapshotEntry{}
	errPaths := map[string]bool{".": true}

	events := diffSnapshots(old, new, errPaths)
	if len(events) != 0 {
		t.Fatalf("expected no events when root scan errors, got %v", events)
	}
}

func TestFileWatchPoller_OnErrorReportsRootScanFailure(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "missing-repo")

	var mu sync.Mutex
	var got []error
	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     repoPath,
		PollInterval: time.Hour,
		OnError: func(err error) {
			mu.Lock()
			got = append(got, err)
			mu.Unlock()
		},
	})

	p.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 OnError callback, got %d", len(got))
	}
	if !strings.Contains(got[0].Error(), "scan had errors") {
		t.Fatalf("expected scan error summary, got %q", got[0].Error())
	}
	if !strings.Contains(got[0].Error(), ".") {
		t.Fatalf("expected root path marker in error detail, got %q", got[0].Error())
	}
}

func TestFileWatchPoller_OnErrorDeduplicatesAndResetsAfterRecovery(t *testing.T) {
	baseDir := t.TempDir()
	repoPath := filepath.Join(baseDir, "repo")

	var mu sync.Mutex
	calls := 0
	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     repoPath, // Start missing to force root scan error.
		PollInterval: time.Hour,
		OnError: func(err error) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})

	// Same failing signature should only report once.
	p.tick()
	p.tick()

	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 deduplicated callback for repeated identical failure, got %d", calls)
	}
	mu.Unlock()

	// Recovery (clean scan) should reset dedupe state.
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write ok.txt: %v", err)
	}
	p.tick()

	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("expected no new callback after clean scan, got %d", calls)
	}
	mu.Unlock()

	// Reintroducing a failure after recovery should report again.
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove repo: %v", err)
	}
	p.tick()

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected second callback after failure recurs, got %d", calls)
	}
}

func TestFileWatchPoller_OnErrorNotCalledForHealthyScan(t *testing.T) {
	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write ok.txt: %v", err)
	}

	var mu sync.Mutex
	calls := 0
	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     repoPath,
		PollInterval: time.Hour,
		OnError: func(err error) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})

	p.tick()

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("expected no OnError callback for healthy scan, got %d", calls)
	}
}

func TestFileWatchPoller_SpecialFilesExcluded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo/unix socket behavior is unix-only")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	fifoPath := filepath.Join(dir, "pipe.fifo")
	fifoSupported := true
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		fifoSupported = false
		t.Logf("mkfifo unsupported in this environment: %v", err)
	}

	socketPath := filepath.Join(dir, "watch.sock")
	ln, err := net.Listen("unix", socketPath)
	socketSupported := true
	if err != nil {
		socketSupported = false
		t.Logf("unix socket unsupported in this environment: %v", err)
	} else {
		defer ln.Close()
	}

	if !fifoSupported && !socketSupported {
		t.Skip("no unix special file primitives supported in this environment")
	}

	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: time.Hour,
	})
	snap, _ := p.scan()

	if _, ok := snap["regular.txt"]; !ok {
		t.Fatal("expected regular file in snapshot")
	}
	if fifoSupported {
		if _, ok := snap["pipe.fifo"]; ok {
			t.Fatal("expected FIFO to be excluded from snapshot")
		}
	}
	if socketSupported {
		if _, ok := snap["watch.sock"]; ok {
			t.Fatal("expected unix socket to be excluded from snapshot")
		}
	}
	if !fifoSupported && socketSupported {
		t.Log("validated special-file exclusion using unix socket only")
	}
	if fifoSupported && !socketSupported {
		t.Log("validated special-file exclusion using FIFO only")
	}
	if fifoSupported && socketSupported {
		t.Log("validated special-file exclusion for FIFO and unix socket")
	}
}

func TestFileWatchPoller_EmitsRepoRelativePath(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "nested")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}

	var mu sync.Mutex
	var received []FileWatchEvent
	p := NewFileWatchPoller(FileWatchPollerConfig{
		RepoPath:     dir,
		PollInterval: testPollInterval,
		OnEvents: func(events []FileWatchEvent) {
			mu.Lock()
			received = append(received, events...)
			mu.Unlock()
		},
	})
	p.Start()
	time.Sleep(testBaselineWait)

	if err := os.WriteFile(filepath.Join(subDir, "child.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	time.Sleep(testDetectionWait)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, ev := range received {
		if ev.Path == "nested/child.txt" && ev.Change == "created" {
			found = true
		}
		if strings.Contains(ev.Path, dir) {
			t.Fatalf("expected repo-relative path, got absolute-like value: %q", ev.Path)
		}
		if strings.Contains(ev.Path, `\`) {
			t.Fatalf("expected slash-normalized path, got %q", ev.Path)
		}
	}
	if !found {
		t.Fatalf("expected created event for nested/child.txt, got %v", received)
	}
}
