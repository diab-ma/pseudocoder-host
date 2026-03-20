package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CodexSessionWatcherConfig configures Codex session file discovery.
type CodexSessionWatcherConfig struct {
	// WorkingDir is the project working directory, used for CWD matching
	// against the session_meta payload.
	WorkingDir string
	// LaunchTime is the PTY launch time in Unix milliseconds.
	LaunchTime int64
	// PollInterval is how often to scan for candidates. Defaults to 500ms.
	PollInterval time.Duration
	// DiscoveryTimeout is the maximum time to search before downgrading. Defaults to 60s.
	DiscoveryTimeout time.Duration
	// MtimeWindow is the backward tolerance: candidates with mtime before
	// (LaunchTime - MtimeWindow) are rejected. Defaults to 5s.
	MtimeWindow time.Duration
	// OnBound is called when a single valid JSONL file is bound.
	// The argument is the absolute path to the bound file.
	OnBound func(filePath string)
	// OnDowngrade is called when discovery fails (zero or ambiguous candidates).
	OnDowngrade func()
	// OnError is called for non-fatal discovery errors.
	OnError func(error)
}

// CodexSessionWatcher discovers and binds the correct JSONL file for a Codex
// session by polling the ~/.codex/sessions/ directory tree. It scans date
// subdirectories (YYYY/MM/DD/) for rollout-*.jsonl files whose session_meta
// event matches the working directory and launch time.
type CodexSessionWatcher struct {
	config  CodexSessionWatcherConfig
	baseDir string // resolved ~/.codex/sessions/

	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	running  bool
	stopping bool
}

// codexCandidate represents a valid Codex session file candidate.
type codexCandidate struct {
	path      string
	timestamp int64 // session_meta.payload.timestamp in millis
}

// codexSessionMeta holds the parsed payload from a session_meta event.
type codexSessionMeta struct {
	CWD       string  `json:"cwd"`
	Timestamp float64 `json:"-"`
}

func (m *codexSessionMeta) UnmarshalJSON(data []byte) error {
	var raw struct {
		CWD       string          `json:"cwd"`
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	timestamp, err := parseFlexibleTimestampSeconds(raw.Timestamp)
	if err != nil {
		return err
	}

	m.CWD = raw.CWD
	m.Timestamp = timestamp
	return nil
}

// NewCodexSessionWatcher creates a watcher (not started). Returns an error
// if the base directory cannot be determined (e.g., home dir unavailable).
func NewCodexSessionWatcher(config CodexSessionWatcherConfig) (*CodexSessionWatcher, error) {
	if config.PollInterval == 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.DiscoveryTimeout == 0 {
		config.DiscoveryTimeout = 60 * time.Second
	}
	if config.MtimeWindow == 0 {
		config.MtimeWindow = 5 * time.Second
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	baseDir := filepath.Join(home, ".codex", "sessions")

	return &CodexSessionWatcher{
		config:  config,
		baseDir: baseDir,
	}, nil
}

// Start begins the discovery loop in a background goroutine.
func (w *CodexSessionWatcher) Start() {
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
func (w *CodexSessionWatcher) Stop() {
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

// discoveryLoop polls for Codex session candidates until one is bound, the
// timeout expires, or the watcher is stopped.
func (w *CodexSessionWatcher) discoveryLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
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
func (w *CodexSessionWatcher) tryDiscover(deadline time.Time) bool {
	now := time.Now()

	candidates, hasPartial := w.scanCandidates()

	switch {
	case len(candidates) == 1:
		log.Printf("codex_session_watcher: bound %s", candidates[0].path)
		if w.config.OnBound != nil {
			w.config.OnBound(candidates[0].path)
		}
		return true

	case len(candidates) > 1:
		// Tie-break: sort by smallest absolute distance to LaunchTime.
		winner, ambiguous := codexTieBreak(candidates, w.config.LaunchTime)
		if ambiguous {
			log.Printf("codex_session_watcher: %d candidates with same distance, downgrading", len(candidates))
			if w.config.OnDowngrade != nil {
				w.config.OnDowngrade()
			}
			return true
		}
		log.Printf("codex_session_watcher: tie-break bound %s", winner.path)
		if w.config.OnBound != nil {
			w.config.OnBound(winner.path)
		}
		return true

	default:
		// Zero valid candidates — check if we've exceeded the deadline.
		if now.After(deadline) {
			if hasPartial {
				log.Printf("codex_session_watcher: partial file never became parseable, downgrading")
			} else {
				log.Printf("codex_session_watcher: no candidates found within discovery window, downgrading")
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

// codexTieBreak selects the best candidate from multiple matches.
// Returns the winner and whether the result is ambiguous (multiple candidates
// with the same smallest distance to launchTime).
func codexTieBreak(candidates []codexCandidate, launchTime int64) (codexCandidate, bool) {
	// Sort by abs(timestamp - launchTime) ascending, then by timestamp descending.
	sort.Slice(candidates, func(i, j int) bool {
		di := int64(math.Abs(float64(candidates[i].timestamp - launchTime)))
		dj := int64(math.Abs(float64(candidates[j].timestamp - launchTime)))
		if di != dj {
			return di < dj
		}
		return candidates[i].timestamp > candidates[j].timestamp
	})

	// Check if first two have the same distance — ambiguous.
	d0 := int64(math.Abs(float64(candidates[0].timestamp - launchTime)))
	d1 := int64(math.Abs(float64(candidates[1].timestamp - launchTime)))
	if d0 == d1 {
		return codexCandidate{}, true
	}

	return candidates[0], false
}

// scanCandidates scans today's and yesterday's date directories for valid
// Codex session file candidates. A file qualifies when:
//  1. It matches the rollout-*.jsonl pattern
//  2. Its mtime is at or after (LaunchTime - MtimeWindow)
//  3. Its session_meta event CWD matches WorkingDir (both resolved)
//  4. Its session_meta timestamp >= LaunchTime
//
// Returns the list of valid candidates and whether any partial files were seen.
func (w *CodexSessionWatcher) scanCandidates() (valid []codexCandidate, hasPartial bool) {
	launchTime := time.UnixMilli(w.config.LaunchTime)
	dirs := codexDateDirs(w.baseDir, launchTime)

	resolvedWorkDir, err := resolveDir(w.config.WorkingDir)
	if err != nil {
		w.reportError(fmt.Errorf("resolve working dir %s: %w", w.config.WorkingDir, err))
		return nil, false
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// Directory may not exist yet — not an error worth reporting.
			if !os.IsNotExist(err) {
				w.reportError(fmt.Errorf("scan codex dir %s: %w", dir, err))
			}
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}

			absPath := filepath.Join(dir, name)
			info, err := entry.Info()
			if err != nil {
				w.reportError(fmt.Errorf("stat %s: %w", absPath, err))
				continue
			}

			// Filter by mtime: reject files modified before (LaunchTime - MtimeWindow).
			earliestMtime := launchTime.Add(-w.config.MtimeWindow)
			if info.ModTime().Before(earliestMtime) {
				continue
			}

			// Read and validate session_meta event.
			meta, partial, err := readSessionMeta(absPath)
			if err != nil {
				w.reportError(fmt.Errorf("read session_meta %s: %w", absPath, err))
				continue
			}
			if partial {
				hasPartial = true
				continue
			}

			// Match CWD.
			resolvedCWD, err := resolveDir(meta.CWD)
			if err != nil {
				w.reportError(fmt.Errorf("resolve cwd %s: %w", meta.CWD, err))
				continue
			}
			if resolvedCWD != resolvedWorkDir {
				continue
			}

			// Convert session_meta timestamp (Unix seconds float) to millis.
			tsMillis := int64(meta.Timestamp * 1000)

			// Reject if timestamp predates PTY launch.
			if tsMillis < w.config.LaunchTime {
				continue
			}

			valid = append(valid, codexCandidate{
				path:      absPath,
				timestamp: tsMillis,
			})
		}
	}

	return valid, hasPartial
}

// readSessionMeta reads the first session_meta event from a Codex JSONL file.
//
// Returns (*codexSessionMeta, partial, error):
//   - Found valid session_meta: (meta, false, nil)
//   - File exists but no complete session_meta line yet: (nil, true, nil)
//   - Read or parse error: (nil, false, err)
func readSessionMeta(path string) (*codexSessionMeta, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	foundAnyComplete := false

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// EOF without newline — content exists but line is incomplete.
			if len(line) > 0 {
				return nil, true, nil
			}
			break
		}

		trimmed := strings.TrimSpace(string(line))
		if len(trimmed) == 0 {
			continue
		}
		if !json.Valid([]byte(trimmed)) {
			continue
		}

		foundAnyComplete = true

		// Parse outer event to check type.
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
			continue
		}

		if event.Type != "session_meta" {
			continue
		}

		// Parse payload.
		var meta codexSessionMeta
		if err := json.Unmarshal(event.Payload, &meta); err != nil {
			return nil, false, fmt.Errorf("unmarshal session_meta payload: %w", err)
		}

		return &meta, false, nil
	}

	// No session_meta found. Check if file has content.
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if info.Size() > 0 && !foundAnyComplete {
		// File has content but no complete lines — partial.
		return nil, true, nil
	}
	if info.Size() == 0 {
		// Empty file — treated as partial.
		return nil, true, nil
	}
	// File has complete lines but none is session_meta — treat as partial
	// (the session_meta line may not have been written yet).
	return nil, true, nil
}

// codexDateDirs returns the date directories to scan: today and yesterday
// (for midnight edge cases).
func codexDateDirs(baseDir string, launchTime time.Time) []string {
	today := launchTime.Format("2006/01/02")
	yesterday := launchTime.AddDate(0, 0, -1).Format("2006/01/02")
	dirs := []string{filepath.Join(baseDir, today)}
	if yesterday != today {
		dirs = append(dirs, filepath.Join(baseDir, yesterday))
	}
	return dirs
}

// resolveDir resolves a directory path through symlinks and cleans it.
func resolveDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func (w *CodexSessionWatcher) reportError(err error) {
	if w.config.OnError != nil {
		w.config.OnError(err)
	}
}
