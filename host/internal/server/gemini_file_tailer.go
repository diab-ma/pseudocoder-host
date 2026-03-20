package server

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// GeminiFileTailer
// ---------------------------------------------------------------------------

// GeminiFileTailerConfig configures a Gemini JSON file tailer.
type GeminiFileTailerConfig struct {
	// FilePath is the absolute path to the Gemini session JSON file.
	FilePath string
	// PollInterval is how often to check for changes. Defaults to 200ms.
	PollInterval time.Duration
	// OnMessages is called when valid messages are parsed from the file.
	OnMessages func(messages []geminiMessage, sessionID string)
	// OnReset is called when a session identity change, inode change, or
	// truncation is detected, signaling that downstream state should be cleared.
	OnReset func()
	// OnError is called for non-fatal errors (stat failures, read errors).
	OnError func(error)
}

// GeminiFileTailer polls a Gemini session JSON file for changes and delivers
// parsed messages. Unlike JSONL tailers, Gemini overwrites the complete file
// on each update, so the tailer reads the entire file on every change.
type GeminiFileTailer struct {
	config        GeminiFileTailerConfig
	lastMtime     int64       // last known mtime in nanos
	lastSize      int64       // last known file size
	fileInfo      os.FileInfo // for inode comparison
	lastSessionID string      // for identity validation (FR24)

	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	running  bool
	stopping bool
}

// NewGeminiFileTailer creates a new Gemini file tailer (not started).
func NewGeminiFileTailer(config GeminiFileTailerConfig) *GeminiFileTailer {
	if config.PollInterval == 0 {
		config.PollInterval = 200 * time.Millisecond
	}
	return &GeminiFileTailer{config: config}
}

// Start begins polling the file in a background goroutine.
// An initial read is performed immediately on start.
func (t *GeminiFileTailer) Start() {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	t.stopCh = make(chan struct{})
	t.doneCh = make(chan struct{})
	t.running = true
	t.stopping = false
	stopCh := t.stopCh
	doneCh := t.doneCh
	t.mu.Unlock()

	go t.pollLoop(stopCh, doneCh)
}

// Stop signals the tailer to exit and waits for it to finish.
func (t *GeminiFileTailer) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	if t.stopping {
		doneCh := t.doneCh
		t.mu.Unlock()
		<-doneCh
		return
	}
	t.stopping = true
	stopCh := t.stopCh
	doneCh := t.doneCh
	t.mu.Unlock()

	close(stopCh)
	<-doneCh

	t.mu.Lock()
	t.running = false
	t.stopping = false
	t.mu.Unlock()
}

// pollLoop performs an initial read, then polls for changes at the configured interval.
func (t *GeminiFileTailer) pollLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
	defer close(doneCh)

	// Initial read on start.
	t.readAndDeliver()

	ticker := time.NewTicker(t.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			t.poll()
		}
	}
}

// poll checks for file changes (mtime, size, inode) and reads if changed.
func (t *GeminiFileTailer) poll() {
	f, err := os.Open(t.config.FilePath)
	if err != nil {
		t.reportError(err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.reportError(err)
		return
	}

	// Detect inode change (file replaced).
	if t.fileInfo != nil && !os.SameFile(t.fileInfo, info) {
		log.Printf("gemini_file_tailer: inode change detected for %s", t.config.FilePath)
		if t.config.OnReset != nil {
			t.config.OnReset()
		}
		t.resetState()
		t.readAndDeliver()
		return
	}

	// Check for any change in mtime or size.
	mtime := info.ModTime().UnixNano()
	size := info.Size()
	if mtime == t.lastMtime && size == t.lastSize {
		return // No change.
	}

	// Note: truncation (size < lastSize) is handled inside readAndDeliver()
	// after JSON validity is confirmed. This avoids firing OnReset for
	// partial writes that happen to be smaller than the previous content.
	t.readAndDeliver()
}

// readAndDeliver reads the entire file, parses it, and delivers messages.
// On invalid JSON (partial write), it logs a warning and retries next cycle.
func (t *GeminiFileTailer) readAndDeliver() {
	data, info, err := t.readFile()
	if err != nil {
		t.reportError(err)
		return
	}

	var session geminiSessionFile
	if err := json.Unmarshal(data, &session); err != nil {
		// Invalid JSON — likely a partial write. Do NOT call OnReset.
		// Retain last state and retry on the next poll cycle (FR10).
		log.Printf("gemini_file_tailer: invalid JSON in %s (partial write?), will retry: %v",
			t.config.FilePath, err)
		return
	}

	// Truncation detection: if the file size shrank and the JSON is valid,
	// this is a real content change (not a partial write). Fire OnReset so
	// downstream consumers can clear stale state before receiving new data.
	newSize := info.Size()
	if t.lastSize > 0 && newSize < t.lastSize {
		log.Printf("gemini_file_tailer: truncation detected for %s (size %d < lastSize %d)",
			t.config.FilePath, newSize, t.lastSize)
		if t.config.OnReset != nil {
			t.config.OnReset()
		}
		// Clear lastSessionID so the identity check below doesn't double-fire.
		t.lastSessionID = ""
	}

	// Update file tracking state only after successful parse.
	t.fileInfo = info
	t.lastMtime = info.ModTime().UnixNano()
	t.lastSize = newSize

	// Identity mismatch detection (FR24): if we had a previous session ID
	// and it differs from the new one, call OnReset before delivering.
	if t.lastSessionID != "" && session.SessionID != t.lastSessionID {
		log.Printf("gemini_file_tailer: session identity changed from %q to %q",
			t.lastSessionID, session.SessionID)
		if t.config.OnReset != nil {
			t.config.OnReset()
		}
	}
	t.lastSessionID = session.SessionID

	// Deliver messages (even if the array is empty).
	if t.config.OnMessages != nil {
		t.config.OnMessages(session.Messages, session.SessionID)
	}
}

// readFile opens, stats, and reads the entire file contents.
func (t *GeminiFileTailer) readFile() ([]byte, os.FileInfo, error) {
	f, err := os.Open(t.config.FilePath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}

	data, err := os.ReadFile(t.config.FilePath)
	if err != nil {
		return nil, nil, err
	}

	return data, info, nil
}

// resetState clears mtime/size/fileInfo/sessionID so the next read is treated
// as an initial read (but does NOT call OnReset — the caller is responsible).
func (t *GeminiFileTailer) resetState() {
	t.lastMtime = 0
	t.lastSize = 0
	t.fileInfo = nil
	t.lastSessionID = ""
}

func (t *GeminiFileTailer) reportError(err error) {
	if t.config.OnError != nil {
		t.config.OnError(err)
	}
}
