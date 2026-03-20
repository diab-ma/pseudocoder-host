package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// JSONLSessionWatcherConfig configures JSONL session file discovery.
type JSONLSessionWatcherConfig struct {
	// WorkingDir is the PTY session's working directory, used to derive
	// the Claude project slug (~/.claude/projects/<slug>/).
	WorkingDir string
	// LaunchTime is the PTY launch time in Unix milliseconds.
	LaunchTime int64
	// PollInterval is how often to scan for candidates. Defaults to 500ms.
	PollInterval time.Duration
	// DiscoveryTimeout is the maximum time to search before downgrading. Defaults to 60s.
	DiscoveryTimeout time.Duration
	// MtimeWindow is the backward tolerance: candidates with mtime before
	// (LaunchTime - MtimeWindow) are rejected. Files modified at or after
	// that threshold always pass. Defaults to 5s.
	MtimeWindow time.Duration
	// OnBound is called when a single valid JSONL file is bound.
	// The argument is the absolute path to the bound file.
	OnBound func(filePath string)
	// OnDowngrade is called when discovery fails (zero or multiple candidates).
	OnDowngrade func()
	// OnError is called for non-fatal discovery errors.
	OnError func(error)
}

// JSONLSessionWatcher discovers and binds the correct JSONL file for a Claude
// PTY session by polling the Claude projects directory. It scans for .jsonl
// files whose mtime is within 5s of PTY launch and whose first event timestamp
// is at or after launch time. Once a single candidate is found, OnBound is
// called. If zero or multiple candidates are found after the 30s discovery
// window, OnDowngrade is called.
type JSONLSessionWatcher struct {
	config   JSONLSessionWatcherConfig
	jsonlDir string // ~/.claude/projects/<slug>/

	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	running  bool
	stopping bool
}

// NewJSONLSessionWatcher creates a watcher (not started). Returns an error
// if the JSONL directory cannot be determined (e.g., home dir unavailable).
func NewJSONLSessionWatcher(config JSONLSessionWatcherConfig) (*JSONLSessionWatcher, error) {
	if config.PollInterval == 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.DiscoveryTimeout == 0 {
		config.DiscoveryTimeout = 60 * time.Second
	}
	if config.MtimeWindow == 0 {
		config.MtimeWindow = 5 * time.Second
	}

	dir, err := jsonlProjectDir(config.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("derive JSONL project dir: %w", err)
	}

	return &JSONLSessionWatcher{
		config:   config,
		jsonlDir: dir,
	}, nil
}

// Start begins the discovery loop in a background goroutine.
func (w *JSONLSessionWatcher) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	w.running = true
	w.stopping = false
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.mu.Unlock()

	go w.discoveryLoop(stopCh, doneCh)
}

// Stop signals the watcher to exit and waits for it to finish.
func (w *JSONLSessionWatcher) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	if w.stopping {
		doneCh := w.doneCh
		w.mu.Unlock()
		<-doneCh
		return
	}
	w.stopping = true
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.mu.Unlock()

	close(stopCh)
	<-doneCh

	w.mu.Lock()
	w.running = false
	w.stopping = false
	w.mu.Unlock()
}

// discoveryLoop polls for JSONL candidates until one is bound, the timeout
// expires, or the watcher is stopped.
func (w *JSONLSessionWatcher) discoveryLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
	defer close(doneCh)

	deadline := time.UnixMilli(w.config.LaunchTime).Add(w.config.DiscoveryTimeout)
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()

	// Perform first scan immediately.
	if w.tryDiscover(deadline) {
		return
	}

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if w.tryDiscover(deadline) {
				return
			}
		}
	}
}

// tryDiscover performs a single discovery scan. Returns true if discovery is
// complete (either bound or downgraded) and the loop should exit.
func (w *JSONLSessionWatcher) tryDiscover(deadline time.Time) bool {
	now := time.Now()

	candidates, hasPartial := w.scanCandidates()

	switch {
	case len(candidates) == 1:
		log.Printf("jsonl_session_watcher: bound %s", candidates[0])
		if w.config.OnBound != nil {
			w.config.OnBound(candidates[0])
		}
		return true

	case len(candidates) > 1:
		// Multiple candidates — ambiguous, downgrade.
		log.Printf("jsonl_session_watcher: %d candidates found, downgrading", len(candidates))
		if w.config.OnDowngrade != nil {
			w.config.OnDowngrade()
		}
		return true

	default:
		// Zero valid candidates — check if we've exceeded the deadline.
		if now.After(deadline) {
			if hasPartial {
				log.Printf("jsonl_session_watcher: partial file never became parseable, downgrading")
			} else {
				log.Printf("jsonl_session_watcher: no candidates found within discovery window, downgrading")
			}
			if w.config.OnDowngrade != nil {
				w.config.OnDowngrade()
			}
			return true
		}
		// Keep polling — within the discovery window.
		return false
	}
}

// scanCandidates scans the JSONL directory for valid binding candidates.
// A file qualifies when its mtime is at or after (LaunchTime - MtimeWindow)
// AND its first parseable JSONL event timestamp >= LaunchTime.
// Returns the list of valid candidate paths and whether any partial
// (incomplete first line) candidates were seen.
func (w *JSONLSessionWatcher) scanCandidates() (valid []string, hasPartial bool) {
	entries, err := os.ReadDir(w.jsonlDir)
	if err != nil {
		// Directory may not exist yet — not an error worth reporting.
		if !os.IsNotExist(err) {
			w.reportError(fmt.Errorf("scan JSONL dir: %w", err))
		}
		return nil, false
	}

	launchTime := time.UnixMilli(w.config.LaunchTime)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		absPath := filepath.Join(w.jsonlDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			w.reportError(fmt.Errorf("stat %s: %w", absPath, err))
			continue
		}

		// Filter by mtime: reject files modified before (LaunchTime - MtimeWindow).
		// Files modified at or after that threshold always pass — the first-event
		// timestamp check below is the primary discriminator.
		earliestMtime := launchTime.Add(-w.config.MtimeWindow)
		if info.ModTime().Before(earliestMtime) {
			continue
		}

		// Check first parseable JSONL event timestamp.
		ts, partial, err := readFirstEventTimestamp(absPath)
		if err != nil {
			w.reportError(fmt.Errorf("read first event %s: %w", absPath, err))
			continue
		}
		if partial {
			// File exists but first line is incomplete — keep polling.
			hasPartial = true
			continue
		}

		// Reject if first-event timestamp predates PTY launch.
		if ts < w.config.LaunchTime {
			continue
		}

		valid = append(valid, absPath)
	}

	return valid, hasPartial
}

func (w *JSONLSessionWatcher) reportError(err error) {
	if w.config.OnError != nil {
		w.config.OnError(err)
	}
}

// jsonlProjectDir returns the Claude JSONL projects directory for the given
// working directory: ~/.claude/projects/<slug>/
func jsonlProjectDir(workingDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}

	slug, err := deriveProjectSlug(workingDir)
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".claude", "projects", slug), nil
}

// deriveProjectSlug computes the Claude project slug from a working directory.
// Claude Code replaces all "/" with "-" (including the leading slash), producing
// slugs like "-Users-diab-pseudocoder".
func deriveProjectSlug(workingDir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return "", fmt.Errorf("eval symlinks %s: %w", workingDir, err)
	}
	slug := strings.ReplaceAll(resolved, "/", "-")
	return slug, nil
}

// readFirstEventTimestamp reads the first complete JSONL line from the file
// and extracts its timestamp (Unix milliseconds).
//
// Returns (timestamp, partial, error):
//   - Complete first line with valid timestamp: (ts, false, nil)
//   - File exists but no complete first line yet: (0, true, nil)
//   - Read or parse error: (0, false, err)
func readFirstEventTimestamp(path string) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// EOF without newline — content exists but line is incomplete.
			if len(line) > 0 {
				return 0, true, nil
			}
			// True EOF with no content remaining.
			break
		}

		trimmed := strings.TrimSpace(string(line))
		if len(trimmed) == 0 {
			continue
		}
		if !json.Valid([]byte(trimmed)) {
			// Skip malformed lines, try the next one.
			continue
		}

		// Parse minimal event structure to extract timestamp.
		var event struct {
			Timestamp json.RawMessage `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
			return 0, false, fmt.Errorf("unmarshal first event: %w", err)
		}

		ts := parseJSONLTimestamp(event.Timestamp)
		if ts == 0 {
			return 0, false, fmt.Errorf("no valid timestamp in first event")
		}
		return ts, false, nil
	}

	// No complete lines found. Check if file has any content at all.
	info, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	if info.Size() > 0 {
		return 0, true, nil
	}
	// Empty file — treated as partial (may get content soon).
	return 0, true, nil
}

// parseJSONLTimestamp extracts a Unix-millis timestamp from a JSON value.
// Handles RFC3339 strings and numeric values. Returns 0 if unparseable.
func parseJSONLTimestamp(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}

	// Try as RFC3339 string (most common in Claude Code JSONL).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli()
		}
	}

	// Try as numeric Unix milliseconds.
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return int64(n)
	}

	return 0
}
