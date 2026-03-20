package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	testCodexWatcherPollInterval     = 10 * time.Millisecond
	testCodexWatcherDiscoveryTimeout = 200 * time.Millisecond
	testCodexWatcherMtimeWindow      = 5 * time.Second
	testCodexWatcherWait             = 500 * time.Millisecond
)

// writeCodexSessionFile writes a Codex session JSONL file with a session_meta
// event and sets its mtime. Returns the absolute path.
func writeCodexSessionFile(t *testing.T, dir, name, cwd string, timestamp float64, mtime time.Time) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create dir %s: %v", dir, err)
	}

	event := map[string]interface{}{
		"timestamp": timestamp,
		"type":      "session_meta",
		"payload": map[string]interface{}{
			"cwd":       cwd,
			"timestamp": timestamp,
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal session_meta: %v", err)
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		t.Fatalf("write codex session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

func writeCodexSessionFileWithStringTimestamp(t *testing.T, dir, name, cwd, timestamp string, mtime time.Time) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create dir %s: %v", dir, err)
	}

	event := map[string]interface{}{
		"timestamp": timestamp,
		"type":      "session_meta",
		"payload": map[string]interface{}{
			"cwd":       cwd,
			"timestamp": timestamp,
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal session_meta: %v", err)
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		t.Fatalf("write codex session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

// codexDateDir returns the date subdirectory for a given time, under baseDir.
func codexDateDir(baseDir string, t time.Time) string {
	return filepath.Join(baseDir, t.Format("2006/01/02"))
}

// newTestCodexWatcher creates a CodexSessionWatcher with the given callbacks
// and test defaults, pointing at the provided baseDir instead of ~/.codex/sessions/.
func newTestCodexWatcher(
	t *testing.T,
	baseDir, workingDir string,
	launchTime time.Time,
	onBound func(string),
	onDowngrade func(),
) *CodexSessionWatcher {
	t.Helper()
	return &CodexSessionWatcher{
		config: CodexSessionWatcherConfig{
			WorkingDir:       workingDir,
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testCodexWatcherPollInterval,
			DiscoveryTimeout: testCodexWatcherDiscoveryTimeout,
			MtimeWindow:      testCodexWatcherMtimeWindow,
			OnBound:          onBound,
			OnDowngrade:      onDowngrade,
		},
		baseDir: baseDir,
	}
}

func TestCodexWatcher_SingleCandidateBinds(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	// Create a single valid session file.
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	dateDir := codexDateDir(baseDir, launchTime)
	expectedPath := writeCodexSessionFile(t, dateDir, "rollout-abc.jsonl",
		workingDir, tsFloat, launchTime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			boundMu.Lock()
			boundPath = path
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		func() {
			t.Error("unexpected downgrade")
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		if got != expectedPath {
			t.Errorf("bound path = %q, want %q", got, expectedPath)
		}
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestCodexWatcher_StringTimestampCandidateBinds(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	tsString := launchTime.Add(100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	dateDir := codexDateDir(baseDir, launchTime)
	expectedPath := writeCodexSessionFileWithStringTimestamp(
		t,
		dateDir,
		"rollout-string.jsonl",
		workingDir,
		tsString,
		launchTime,
	)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			boundMu.Lock()
			boundPath = path
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		func() {
			t.Error("unexpected downgrade")
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		if got != expectedPath {
			t.Errorf("bound path = %q, want %q", got, expectedPath)
		}
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestCodexWatcher_ZeroCandidatesDowngrades(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	// No session files — should downgrade on timeout.
	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind")
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_TieBreakPicksClosestTimestamp(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// Candidate 1: timestamp 50ms after launch (closer).
	ts1 := float64(launchTime.UnixMilli()+50) / 1000.0
	closerPath := writeCodexSessionFile(t, dateDir, "rollout-closer.jsonl",
		workingDir, ts1, launchTime)

	// Candidate 2: timestamp 500ms after launch (farther).
	ts2 := float64(launchTime.UnixMilli()+500) / 1000.0
	writeCodexSessionFile(t, dateDir, "rollout-farther.jsonl",
		workingDir, ts2, launchTime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			boundMu.Lock()
			boundPath = path
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		func() {
			t.Error("unexpected downgrade: tie-break should pick closer candidate")
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		if got != closerPath {
			t.Errorf("bound path = %q, want %q (closer candidate)", got, closerPath)
		}
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestCodexWatcher_TieBreakSameDistanceDowngrades(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// Two candidates with the exact same timestamp — same distance, ambiguous.
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	writeCodexSessionFile(t, dateDir, "rollout-a.jsonl",
		workingDir, tsFloat, launchTime)
	writeCodexSessionFile(t, dateDir, "rollout-b.jsonl",
		workingDir, tsFloat, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind: same-distance candidates should downgrade")
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_CWDMismatchNotCandidate(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	otherDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// File has a different CWD — should not match.
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	writeCodexSessionFile(t, dateDir, "rollout-other.jsonl",
		otherDir, tsFloat, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind: CWD mismatch should not match")
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_YesterdayDirectoryWorks(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	// Put the file in yesterday's date directory.
	yesterday := launchTime.AddDate(0, 0, -1)
	dateDir := codexDateDir(baseDir, yesterday)

	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	expectedPath := writeCodexSessionFile(t, dateDir, "rollout-yesterday.jsonl",
		workingDir, tsFloat, launchTime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			boundMu.Lock()
			boundPath = path
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		func() {
			t.Error("unexpected downgrade: yesterday's dir should be scanned")
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		if got != expectedPath {
			t.Errorf("bound path = %q, want %q", got, expectedPath)
		}
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestCodexWatcher_NonRolloutFilesIgnored(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// Create files that don't match the rollout-*.jsonl pattern.
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	writeCodexSessionFile(t, dateDir, "session-abc.jsonl",
		workingDir, tsFloat, launchTime)
	writeCodexSessionFile(t, dateDir, "rollout-abc.txt",
		workingDir, tsFloat, launchTime)
	writeCodexSessionFile(t, dateDir, "other.jsonl",
		workingDir, tsFloat, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Errorf("unexpected bind: non-rollout file %q should be ignored", path)
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_StaleTimestampRejected(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// Timestamp before launch — should be rejected.
	staleTs := float64(launchTime.UnixMilli()-10000) / 1000.0
	writeCodexSessionFile(t, dateDir, "rollout-stale.jsonl",
		workingDir, staleTs, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind: stale timestamp should be rejected")
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_MtimeOutOfWindowRejected(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)

	// Valid timestamp but mtime far before launch (beyond backward tolerance).
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	oldMtime := launchTime.Add(-1 * time.Hour)
	writeCodexSessionFile(t, dateDir, "rollout-old-mtime.jsonl",
		workingDir, tsFloat, oldMtime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind: mtime out of window should be rejected")
		},
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_PartialFileKeepsPolling(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	dateDir := codexDateDir(baseDir, launchTime)
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a partial file (no trailing newline).
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0
	event := map[string]interface{}{
		"timestamp": tsFloat,
		"type":      "session_meta",
		"payload": map[string]interface{}{
			"cwd":       workingDir,
			"timestamp": tsFloat,
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	partialPath := filepath.Join(dateDir, "rollout-partial.jsonl")
	if err := os.WriteFile(partialPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partialPath, launchTime, launchTime); err != nil {
		t.Fatal(err)
	}

	boundCh := make(chan struct{}, 1)

	w := &CodexSessionWatcher{
		config: CodexSessionWatcherConfig{
			WorkingDir:       workingDir,
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testCodexWatcherPollInterval,
			DiscoveryTimeout: 2 * time.Second,
			MtimeWindow:      testCodexWatcherMtimeWindow,
			OnBound: func(path string) {
				boundCh <- struct{}{}
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade")
			},
		},
		baseDir: baseDir,
	}

	w.Start()
	defer w.Stop()

	// Let watcher see partial file.
	time.Sleep(50 * time.Millisecond)

	// Complete the file by appending a newline.
	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chtimes(partialPath, launchTime, launchTime); err != nil {
		t.Fatal(err)
	}

	select {
	case <-boundCh:
		// Expected — partial file became parseable and was bound.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind after completing partial file")
	}
}

func TestCodexWatcher_SessionCloseStopsWatcher(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	// No candidates — watcher would poll until timeout.
	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		func(path string) {
			t.Error("unexpected bind")
		},
		func() {
			t.Error("unexpected downgrade during stop")
		},
	)
	// Use long timeout to prove Stop() exits early.
	w.config.DiscoveryTimeout = 10 * time.Second

	w.Start()

	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Expected — Stop returned promptly.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("Stop() did not return promptly")
	}
}

func TestCodexWatcher_DoubleStopSafe(t *testing.T) {
	baseDir := t.TempDir()
	workingDir := t.TempDir()
	launchTime := time.Now()

	w := &CodexSessionWatcher{
		config: CodexSessionWatcherConfig{
			WorkingDir:       workingDir,
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testCodexWatcherPollInterval,
			DiscoveryTimeout: 10 * time.Second,
			MtimeWindow:      testCodexWatcherMtimeWindow,
		},
		baseDir: baseDir,
	}

	w.Start()
	w.Stop()
	w.Stop() // Should not panic or block.
}

func TestCodexWatcher_DirectoryNotExistYet(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "nonexistent-codex")
	workingDir := t.TempDir()
	launchTime := time.Now()

	downgradeCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, workingDir, launchTime,
		nil,
		func() {
			downgradeCh <- struct{}{}
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — directory doesn't exist, polls without error, downgrades on timeout.
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestCodexWatcher_CWDNormalization(t *testing.T) {
	baseDir := t.TempDir()
	launchTime := time.Now()

	// Create a real directory and a symlink to it.
	realDir := filepath.Join(t.TempDir(), "real-project")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(t.TempDir(), "symlink-project")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatal(err)
	}

	dateDir := codexDateDir(baseDir, launchTime)
	tsFloat := float64(launchTime.UnixMilli()+100) / 1000.0

	// File records the real path, watcher config uses the symlink.
	expectedPath := writeCodexSessionFile(t, dateDir, "rollout-sym.jsonl",
		realDir, tsFloat, launchTime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestCodexWatcher(t, baseDir, symlinkDir, launchTime,
		func(path string) {
			boundMu.Lock()
			boundPath = path
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		func() {
			t.Error("unexpected downgrade: symlink CWD should resolve and match")
		},
	)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		if got != expectedPath {
			t.Errorf("bound path = %q, want %q", got, expectedPath)
		}
	case <-time.After(testCodexWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestCodexDateDirs(t *testing.T) {
	baseDir := "/home/user/.codex/sessions"

	t.Run("normal_day", func(t *testing.T) {
		lt := time.Date(2026, 3, 15, 14, 0, 0, 0, time.UTC)
		dirs := codexDateDirs(baseDir, lt)
		if len(dirs) != 2 {
			t.Fatalf("expected 2 dirs, got %d", len(dirs))
		}
		if dirs[0] != filepath.Join(baseDir, "2026/03/15") {
			t.Errorf("today dir = %q", dirs[0])
		}
		if dirs[1] != filepath.Join(baseDir, "2026/03/14") {
			t.Errorf("yesterday dir = %q", dirs[1])
		}
	})

	t.Run("month_boundary", func(t *testing.T) {
		lt := time.Date(2026, 3, 1, 0, 5, 0, 0, time.UTC)
		dirs := codexDateDirs(baseDir, lt)
		if len(dirs) != 2 {
			t.Fatalf("expected 2 dirs, got %d", len(dirs))
		}
		if dirs[0] != filepath.Join(baseDir, "2026/03/01") {
			t.Errorf("today dir = %q", dirs[0])
		}
		if dirs[1] != filepath.Join(baseDir, "2026/02/28") {
			t.Errorf("yesterday dir = %q", dirs[1])
		}
	})
}

func TestReadSessionMeta(t *testing.T) {
	t.Run("valid_session_meta", func(t *testing.T) {
		dir := t.TempDir()
		tsFloat := 1710417600.123
		event := fmt.Sprintf(`{"timestamp":%f,"type":"session_meta","payload":{"cwd":"/tmp/project","timestamp":%f}}`, tsFloat, tsFloat)
		path := filepath.Join(dir, "test.jsonl")
		if err := os.WriteFile(path, []byte(event+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		meta, partial, err := readSessionMeta(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatal("expected non-partial")
		}
		if meta.CWD != "/tmp/project" {
			t.Errorf("CWD = %q, want /tmp/project", meta.CWD)
		}
		if meta.Timestamp != tsFloat {
			t.Errorf("Timestamp = %f, want %f", meta.Timestamp, tsFloat)
		}
	})

	t.Run("no_session_meta_type", func(t *testing.T) {
		dir := t.TempDir()
		event := `{"timestamp":1710417600.0,"type":"message","payload":{"text":"hello"}}`
		path := filepath.Join(dir, "no-meta.jsonl")
		if err := os.WriteFile(path, []byte(event+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		_, partial, err := readSessionMeta(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !partial {
			t.Fatal("expected partial=true when no session_meta found")
		}
	})

	t.Run("empty_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.jsonl")
		if err := os.WriteFile(path, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}

		_, partial, err := readSessionMeta(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !partial {
			t.Fatal("expected partial=true for empty file")
		}
	})

	t.Run("partial_no_newline", func(t *testing.T) {
		dir := t.TempDir()
		event := `{"timestamp":1710417600.0,"type":"session_meta","payload":{"cwd":"/tmp","timestamp":1710417600.0}}`
		path := filepath.Join(dir, "partial.jsonl")
		// Write without trailing newline.
		if err := os.WriteFile(path, []byte(event), 0644); err != nil {
			t.Fatal(err)
		}

		_, partial, err := readSessionMeta(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !partial {
			t.Fatal("expected partial=true for file without newline")
		}
	})

	t.Run("session_meta_after_other_lines", func(t *testing.T) {
		dir := t.TempDir()
		lines := `{"timestamp":1710417600.0,"type":"init","payload":{}}
{"timestamp":1710417600.5,"type":"session_meta","payload":{"cwd":"/tmp/project2","timestamp":1710417600.5}}
`
		path := filepath.Join(dir, "multi.jsonl")
		if err := os.WriteFile(path, []byte(lines), 0644); err != nil {
			t.Fatal(err)
		}

		meta, partial, err := readSessionMeta(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatal("expected non-partial")
		}
		if meta.CWD != "/tmp/project2" {
			t.Errorf("CWD = %q, want /tmp/project2", meta.CWD)
		}
	})
}

func TestCodexTieBreak(t *testing.T) {
	t.Run("different_distances", func(t *testing.T) {
		launch := int64(1000)
		candidates := []codexCandidate{
			{path: "/far", timestamp: 1500},
			{path: "/close", timestamp: 1050},
		}
		winner, ambiguous := codexTieBreak(candidates, launch)
		if ambiguous {
			t.Fatal("expected non-ambiguous")
		}
		if winner.path != "/close" {
			t.Errorf("winner = %q, want /close", winner.path)
		}
	})

	t.Run("same_distance_ambiguous", func(t *testing.T) {
		launch := int64(1000)
		candidates := []codexCandidate{
			{path: "/a", timestamp: 1100},
			{path: "/b", timestamp: 1100},
		}
		_, ambiguous := codexTieBreak(candidates, launch)
		if !ambiguous {
			t.Fatal("expected ambiguous for same-distance candidates")
		}
	})
}
