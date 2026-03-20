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
	testWatcherPollInterval    = 10 * time.Millisecond
	testWatcherDiscoveryTimeout = 200 * time.Millisecond
	testWatcherMtimeWindow     = 5 * time.Second
	testWatcherWait            = 500 * time.Millisecond
)

// writeJSONLFile writes a JSONL file with the given lines and sets its mtime.
func writeJSONLFile(t *testing.T, dir, name string, lines []string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var content string
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write JSONL file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

// makeJSONLEvent creates a minimal JSONL event line with the given timestamp.
func makeJSONLEvent(t *testing.T, ts time.Time) string {
	t.Helper()
	event := map[string]interface{}{
		"uuid":      "test-uuid-" + fmt.Sprintf("%d", ts.UnixMilli()),
		"type":      "human",
		"timestamp": ts.Format(time.RFC3339Nano),
		"message": map[string]interface{}{
			"role":    "user",
			"content": "hello",
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(data)
}

func TestSessionWatcher_SingleCandidateBinds(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Create a single valid JSONL file: mtime within window, first event >= launch.
	eventTime := launchTime.Add(100 * time.Millisecond)
	writeJSONLFile(t, dir, "session-abc.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, launchTime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				boundMu.Lock()
				boundPath = path
				boundMu.Unlock()
				boundCh <- struct{}{}
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade")
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		expected := filepath.Join(dir, "session-abc.jsonl")
		if got != expected {
			t.Errorf("bound path = %q, want %q", got, expected)
		}
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestSessionWatcher_ZeroCandidatesDowngrades(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// No JSONL files in the directory.
	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind")
			},
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_MultipleCandidatesDowngrades(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Create two valid JSONL files — should cause ambiguity downgrade.
	eventTime := launchTime.Add(100 * time.Millisecond)
	writeJSONLFile(t, dir, "session-1.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, launchTime)
	writeJSONLFile(t, dir, "session-2.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind")
			},
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_StaleTimestampRejected(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// File mtime is within window but first event timestamp predates launch.
	staleTime := launchTime.Add(-10 * time.Second)
	writeJSONLFile(t, dir, "stale.jsonl",
		[]string{makeJSONLEvent(t, staleTime)}, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind: stale timestamp should be rejected")
			},
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — stale file is the only candidate but rejected.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_MtimeOutOfWindowRejected(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// File has a valid first-event timestamp but mtime is far BEFORE launch
	// (beyond backward tolerance). The forward-looking filter rejects it.
	eventTime := launchTime.Add(100 * time.Millisecond)
	oldMtime := launchTime.Add(-1 * time.Hour)
	writeJSONLFile(t, dir, "old-mtime.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, oldMtime)

	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind: mtime out of window")
			},
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_PartialFilePolling(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Create a file with content but no newline (partial first line).
	partialPath := filepath.Join(dir, "partial.jsonl")
	eventTime := launchTime.Add(100 * time.Millisecond)
	eventLine := makeJSONLEvent(t, eventTime)
	// Write without trailing newline — scanner won't produce a complete line.
	if err := os.WriteFile(partialPath, []byte(eventLine), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partialPath, launchTime, launchTime); err != nil {
		t.Fatal(err)
	}

	boundCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: 2 * time.Second, // long enough for us to complete the file
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				boundCh <- struct{}{}
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade")
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	// Wait a bit to let the watcher see the partial file.
	time.Sleep(50 * time.Millisecond)

	// Now complete the file by appending a newline.
	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	// Update mtime to be within window.
	if err := os.Chtimes(partialPath, launchTime, launchTime); err != nil {
		t.Fatal(err)
	}

	select {
	case <-boundCh:
		// Expected — partial file became parseable and was bound.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for bind after completing partial file")
	}
}

func TestSessionWatcher_SessionCloseStopsWatcher(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// No candidates — watcher would normally poll until timeout.
	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: 10 * time.Second, // long timeout
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind")
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade during stop")
			},
		},
		jsonlDir: dir,
	}

	w.Start()

	// Stop should return promptly without waiting for the discovery timeout.
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Expected — Stop returned promptly.
	case <-time.After(testWatcherWait):
		t.Fatal("Stop() did not return promptly")
	}
}

func TestSessionWatcher_DoubleStopSafe(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: 10 * time.Second,
			MtimeWindow:      testWatcherMtimeWindow,
		},
		jsonlDir: dir,
	}

	w.Start()
	w.Stop()
	w.Stop() // Should not panic or block.
}

func TestSessionWatcher_NonJSONLFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Create a .txt file and a directory — neither should be considered.
	eventTime := launchTime.Add(100 * time.Millisecond)
	writeJSONLFile(t, dir, "notes.txt",
		[]string{makeJSONLEvent(t, eventTime)}, launchTime)
	if err := os.Mkdir(filepath.Join(dir, "subdir.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}

	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				t.Error("unexpected bind: non-JSONL files should be ignored")
			},
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — no valid .jsonl files.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_DirectoryNotExistYet(t *testing.T) {
	// Point at a non-existent directory. The watcher should poll without error
	// and eventually downgrade when the timeout expires.
	dir := filepath.Join(t.TempDir(), "nonexistent-subdir")
	launchTime := time.Now()

	downgradeCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnDowngrade: func() {
				downgradeCh <- struct{}{}
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestSessionWatcher_MtimeAfterLaunchAccepted(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// File with mtime 10s AFTER launch should bind. The old symmetric filter
	// (abs(mtime - launch) <= 5s) would reject this — this is the bug scenario.
	eventTime := launchTime.Add(100 * time.Millisecond)
	futureMtime := launchTime.Add(10 * time.Second)
	writeJSONLFile(t, dir, "future-mtime.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, futureMtime)

	var boundPath string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: testWatcherDiscoveryTimeout,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				boundMu.Lock()
				boundPath = path
				boundMu.Unlock()
				boundCh <- struct{}{}
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade: mtime after launch should be accepted")
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundPath
		boundMu.Unlock()
		expected := filepath.Join(dir, "future-mtime.jsonl")
		if got != expected {
			t.Errorf("bound path = %q, want %q", got, expected)
		}
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestSessionWatcher_FileAppearsLateAfterLaunch(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Directory starts empty; file appears 100ms later.
	// Simulates Claude's delayed file creation after receiving a prompt.
	boundCh := make(chan struct{}, 1)

	w := &JSONLSessionWatcher{
		config: JSONLSessionWatcherConfig{
			LaunchTime:       launchTime.UnixMilli(),
			PollInterval:     testWatcherPollInterval,
			DiscoveryTimeout: 2 * time.Second,
			MtimeWindow:      testWatcherMtimeWindow,
			OnBound: func(path string) {
				boundCh <- struct{}{}
			},
			OnDowngrade: func() {
				t.Error("unexpected downgrade: late-appearing file should bind")
			},
		},
		jsonlDir: dir,
	}

	w.Start()
	defer w.Stop()

	// Wait then create the file.
	time.Sleep(100 * time.Millisecond)
	eventTime := time.Now()
	writeJSONLFile(t, dir, "late.jsonl",
		[]string{makeJSONLEvent(t, eventTime)}, eventTime)

	select {
	case <-boundCh:
		// Expected — late-appearing file was discovered and bound.
	case <-time.After(testWatcherWait):
		t.Fatal("timed out waiting for bind of late-appearing file")
	}
}

func TestDeriveProjectSlug(t *testing.T) {
	// Use a real temp directory to ensure EvalSymlinks works.
	dir := t.TempDir()
	slug, err := deriveProjectSlug(dir)
	if err != nil {
		t.Fatalf("deriveProjectSlug(%q): %v", dir, err)
	}

	// Slug should start with "-" (leading "/" becomes "-", matching Claude Code behavior).
	if len(slug) == 0 || slug[0] != '-' {
		t.Errorf("slug %q should start with '-'", slug)
	}
	// Slug should not contain "/".
	if idx := len(slug) - len(slug); idx >= 0 {
		for _, c := range slug {
			if c == '/' {
				t.Errorf("slug %q contains '/'", slug)
				break
			}
		}
	}
}

func TestReadFirstEventTimestamp(t *testing.T) {
	t.Run("valid_rfc3339", func(t *testing.T) {
		dir := t.TempDir()
		ts := time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC)
		path := writeJSONLFile(t, dir, "valid.jsonl",
			[]string{makeJSONLEvent(t, ts)}, time.Now())

		got, partial, err := readFirstEventTimestamp(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatal("expected non-partial")
		}
		if got != ts.UnixMilli() {
			t.Errorf("timestamp = %d, want %d", got, ts.UnixMilli())
		}
	})

	t.Run("numeric_timestamp", func(t *testing.T) {
		dir := t.TempDir()
		tsMillis := int64(1710417600000) // 2024-03-14T12:00:00Z
		event := fmt.Sprintf(`{"uuid":"test","type":"human","timestamp":%d,"message":{"role":"user","content":"hi"}}`, tsMillis)
		path := writeJSONLFile(t, dir, "numeric.jsonl", []string{event}, time.Now())

		got, partial, err := readFirstEventTimestamp(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatal("expected non-partial")
		}
		if got != tsMillis {
			t.Errorf("timestamp = %d, want %d", got, tsMillis)
		}
	})

	t.Run("partial_no_newline", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "partial.jsonl")
		// Write content without trailing newline.
		if err := os.WriteFile(path, []byte(`{"uuid":"x","type":"human","timestamp":"2025-01-01T00:00:00Z"}`), 0644); err != nil {
			t.Fatal(err)
		}

		_, partial, err := readFirstEventTimestamp(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !partial {
			t.Fatal("expected partial=true for file without newline")
		}
	})

	t.Run("empty_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.jsonl")
		if err := os.WriteFile(path, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}

		_, partial, err := readFirstEventTimestamp(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !partial {
			t.Fatal("expected partial=true for empty file")
		}
	})

	t.Run("null_timestamp", func(t *testing.T) {
		dir := t.TempDir()
		event := `{"uuid":"test","type":"human","timestamp":null,"message":{"role":"user","content":"hi"}}`
		path := writeJSONLFile(t, dir, "null-ts.jsonl", []string{event}, time.Now())

		_, _, err := readFirstEventTimestamp(path)
		if err == nil {
			t.Fatal("expected error for null timestamp")
		}
	})

	t.Run("skips_malformed_lines", func(t *testing.T) {
		dir := t.TempDir()
		ts := time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC)
		lines := []string{
			"not valid json at all",
			makeJSONLEvent(t, ts),
		}
		path := writeJSONLFile(t, dir, "malformed-first.jsonl", lines, time.Now())

		got, partial, err := readFirstEventTimestamp(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatal("expected non-partial")
		}
		if got != ts.UnixMilli() {
			t.Errorf("timestamp = %d, want %d", got, ts.UnixMilli())
		}
	})
}

func TestParseJSONLTimestamp(t *testing.T) {
	t.Run("rfc3339", func(t *testing.T) {
		raw := json.RawMessage(`"2025-03-14T12:00:00Z"`)
		got := parseJSONLTimestamp(raw)
		want := time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC).UnixMilli()
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})

	t.Run("rfc3339nano", func(t *testing.T) {
		raw := json.RawMessage(`"2025-03-14T12:00:00.123456789Z"`)
		got := parseJSONLTimestamp(raw)
		want := time.Date(2025, 3, 14, 12, 0, 0, 123456789, time.UTC).UnixMilli()
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})

	t.Run("numeric", func(t *testing.T) {
		raw := json.RawMessage(`1710417600000`)
		got := parseJSONLTimestamp(raw)
		if got != 1710417600000 {
			t.Errorf("got %d, want 1710417600000", got)
		}
	})

	t.Run("null", func(t *testing.T) {
		raw := json.RawMessage(`null`)
		got := parseJSONLTimestamp(raw)
		if got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		got := parseJSONLTimestamp(nil)
		if got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
}
