package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	testTailPollInterval = 10 * time.Millisecond
	testTailWait         = 150 * time.Millisecond
)

// writeJSONLLine marshals v to JSON and writes it as a newline-terminated line.
func writeJSONLLine(t *testing.T, f *os.File, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLTailReader_FullReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write 3 lines before starting the reader.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONLLine(t, f, map[string]int{"id": 1})
	writeJSONLLine(t, f, map[string]int{"id": 2})
	writeJSONLLine(t, f, map[string]int{"id": 3})
	f.Close()

	var mu sync.Mutex
	var lines []string

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var m map[string]int
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d: unmarshal error: %v", i, err)
		}
		if m["id"] != i+1 {
			t.Fatalf("line %d: id=%d, want %d", i, m["id"], i+1)
		}
	}
}

func TestJSONLTailReader_IncrementalLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write initial content.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONLLine(t, f, map[string]int{"id": 1})

	var mu sync.Mutex
	var lines []string

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)

	// Append more content while reader is running.
	writeJSONLLine(t, f, map[string]int{"id": 2})
	writeJSONLLine(t, f, map[string]int{"id": 3})
	f.Close()

	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
}

func TestJSONLTailReader_IncompleteLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write a partial line (no trailing newline).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte(`{"id":1}`))
	f.Close()

	var mu sync.Mutex
	var lines []string

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)

	// No lines delivered yet — the line has no terminating newline.
	mu.Lock()
	if len(lines) != 0 {
		t.Fatalf("got %d lines before newline, want 0", len(lines))
	}
	mu.Unlock()

	// Complete the first line and add a second one.
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("\n{\"id\":2}\n"))
	f.Close()

	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
}

func TestJSONLTailReader_MalformedLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	content := "{\"id\":1}\nnot valid json\n{\"id\":2}\n{truncated\n{\"id\":3}\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	// Only 3 valid JSON lines should be delivered; 2 malformed lines skipped.
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (malformed lines should be skipped)", len(lines))
	}
}

func TestJSONLTailReader_TruncationDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write initial content (2 lines).
	if err := os.WriteFile(path, []byte("{\"id\":1}\n{\"id\":2}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	var resetCount int

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)

	// Truncate and write smaller content (same inode, smaller size).
	if err := os.WriteFile(path, []byte("{\"id\":10}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	if resetCount < 1 {
		t.Fatalf("resetCount=%d, want >= 1", resetCount)
	}

	// Initial: 2 lines + after reset re-read: 1 line = at least 3.
	if len(lines) < 3 {
		t.Fatalf("got %d lines, want >= 3 (initial + post-reset)", len(lines))
	}
}

func TestJSONLTailReader_InodeChangeDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	if err := os.WriteFile(path, []byte("{\"id\":1}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	var resetCount int

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)

	// Replace the file via rename — creates a new inode at the same path.
	tmpPath := filepath.Join(dir, "session.jsonl.tmp")
	if err := os.WriteFile(tmpPath, []byte("{\"id\":20}\n{\"id\":21}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}

	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	if resetCount < 1 {
		t.Fatalf("resetCount=%d, want >= 1", resetCount)
	}

	// Initial: 1 line + after inode change re-read: 2 lines = at least 3.
	if len(lines) < 3 {
		t.Fatalf("got %d lines, want >= 3 (initial + post-reset)", len(lines))
	}
}

func TestJSONLTailReader_CleanShutdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	if err := os.WriteFile(path, []byte("{\"id\":1}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
	})
	reader.Start()
	time.Sleep(testTailWait)

	// Stop should return without hanging.
	done := make(chan struct{})
	go func() {
		reader.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success: Stop returned cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within timeout — goroutine leak")
	}

	// Double-stop must be safe.
	reader.Stop()
}

func TestJSONLTailReader_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.jsonl")

	var mu sync.Mutex
	var errors []error

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     path,
		PollInterval: testTailPollInterval,
		OnError: func(err error) {
			mu.Lock()
			errors = append(errors, err)
			mu.Unlock()
		},
	})
	reader.Start()
	time.Sleep(testTailWait)
	reader.Stop()

	mu.Lock()
	defer mu.Unlock()

	// Should have reported errors (file not found), not panicked.
	if len(errors) == 0 {
		t.Fatal("expected errors for nonexistent file, got none")
	}
}
