package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GeminiSessionWatcherConfig configures Gemini session file discovery.
type GeminiSessionWatcherConfig struct {
	// WorkingDir is the project working directory, used to match against
	// .project_root files inside ~/.gemini/tmp/<hash>/ directories.
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
	// FreshPreferenceWindow is how long to prefer fresh sessions before
	// accepting resumed ones. Defaults to 30s. During this window, candidates
	// with startTime < (LaunchTime - MtimeWindow) are skipped, giving the
	// CLI time to create the actual fresh session file.
	FreshPreferenceWindow time.Duration
	// OnBound is called when a single valid session file is bound.
	// filePath is the absolute path; sessionID is the parsed sessionId from JSON.
	OnBound func(filePath string, sessionID string)
	// OnDowngrade is called when discovery fails (zero or multiple candidates).
	OnDowngrade func()
	// OnError is called for non-fatal discovery errors.
	OnError func(error)
}

// geminiCandidate represents a discovered Gemini session file.
type geminiCandidate struct {
	path      string
	sessionID string // from JSON sessionId field
	startTime int64  // millis
	mtime     int64  // millis
}

// geminiSessionHeader is the minimal JSON structure parsed from session files
// for discovery purposes. Uses the same field names as geminiSessionFile but
// only the top-level scalars (no Messages array needed).
type geminiSessionHeader struct {
	SessionID   string  `json:"sessionId"`
	StartTime   float64 `json:"-"`
	LastUpdated float64 `json:"-"`
}

func (h *geminiSessionHeader) UnmarshalJSON(data []byte) error {
	var raw struct {
		SessionID   string          `json:"sessionId"`
		StartTime   json.RawMessage `json:"startTime"`
		LastUpdated json.RawMessage `json:"lastUpdated"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	startTime, err := parseFlexibleTimestampSeconds(raw.StartTime)
	if err != nil {
		return err
	}
	lastUpdated, err := parseFlexibleTimestampSeconds(raw.LastUpdated)
	if err != nil {
		return err
	}

	h.SessionID = raw.SessionID
	h.StartTime = startTime
	h.LastUpdated = lastUpdated
	return nil
}

// GeminiSessionWatcher discovers and binds the correct Gemini session JSON file
// by polling ~/.gemini/tmp/ for hash directories whose .project_root matches
// the configured WorkingDir. It scans for .json files, filters by mtime and
// startTime, and uses a tie-break algorithm to select the best candidate.
type GeminiSessionWatcher struct {
	config  GeminiSessionWatcherConfig
	baseDir string // resolved ~/.gemini/tmp/

	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	running  bool
	stopping bool
}

// NewGeminiSessionWatcher creates a watcher (not started). Returns an error
// if the base directory cannot be determined (e.g., home dir unavailable).
func NewGeminiSessionWatcher(config GeminiSessionWatcherConfig) (*GeminiSessionWatcher, error) {
	if config.PollInterval == 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.DiscoveryTimeout == 0 {
		config.DiscoveryTimeout = 60 * time.Second
	}
	if config.MtimeWindow == 0 {
		config.MtimeWindow = 5 * time.Second
	}
	if config.FreshPreferenceWindow == 0 {
		config.FreshPreferenceWindow = 30 * time.Second
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	baseDir := filepath.Join(home, ".gemini", "tmp")

	return &GeminiSessionWatcher{
		config:  config,
		baseDir: baseDir,
	}, nil
}

// Start begins the discovery loop in a background goroutine.
func (w *GeminiSessionWatcher) Start() {
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
func (w *GeminiSessionWatcher) Stop() {
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

// discoveryLoop polls for Gemini session candidates until one is bound, the
// timeout expires, or the watcher is stopped.
func (w *GeminiSessionWatcher) discoveryLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
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
func (w *GeminiSessionWatcher) tryDiscover(deadline time.Time) bool {
	now := time.Now()

	candidates, _ := w.scanCandidates()

	if len(candidates) == 0 {
		if now.After(deadline) {
			log.Printf("gemini_session_watcher: no candidates found within discovery window, downgrading")
			if w.config.OnDowngrade != nil {
				w.config.OnDowngrade()
			}
			return true
		}
		return false
	}

	// Separate fresh vs resumed candidates.
	// Fresh: startTime >= (LaunchTime - MtimeWindow) — created around launch time.
	// Resumed: older startTime but recent mtime (CLI touched/re-saved the file).
	launchMillis := w.config.LaunchTime
	mtimeWindowMillis := w.config.MtimeWindow.Milliseconds()
	var fresh, resumed []geminiCandidate
	for _, c := range candidates {
		if c.startTime >= (launchMillis - mtimeWindowMillis) {
			fresh = append(fresh, c)
		} else {
			resumed = append(resumed, c)
		}
	}

	// During the fresh-preference window, prefer fresh candidates to avoid
	// binding to stale files that the CLI touched/re-saved before creating
	// the actual new session.
	freshDeadline := time.UnixMilli(launchMillis).Add(w.config.FreshPreferenceWindow)
	inFreshWindow := now.Before(freshDeadline)

	switch {
	case len(fresh) > 0:
		return w.resolveCandidate(fresh)
	case len(resumed) > 0 && !inFreshWindow:
		return w.resolveCandidate(resumed)
	case len(resumed) > 0 && now.After(deadline):
		// Discovery timeout with only resumed candidates — fall back.
		log.Printf("gemini_session_watcher: discovery timeout, falling back to resumed candidate")
		return w.resolveCandidate(resumed)
	case now.After(deadline):
		log.Printf("gemini_session_watcher: no candidates found within discovery window, downgrading")
		if w.config.OnDowngrade != nil {
			w.config.OnDowngrade()
		}
		return true
	default:
		return false // Keep looking for fresh candidates
	}
}

// resolveCandidate applies the tie-break algorithm to select a single winner.
// Returns true (discovery complete) in all cases.
func (w *GeminiSessionWatcher) resolveCandidate(candidates []geminiCandidate) bool {
	launchTime := w.config.LaunchTime

	// Compute distance for each candidate.
	type scored struct {
		candidate geminiCandidate
		distance  int64
	}

	var scored_list []scored
	for _, c := range candidates {
		// distance = (lastUpdated*1000 or mtime) - LaunchTime
		var effectiveTime int64
		// We stored startTime in millis already; we need lastUpdated in millis.
		// Re-read lastUpdated from the candidate. We used mtime as fallback at scan time,
		// but for tie-break we need the actual lastUpdated. Since we don't store it
		// separately, we use the heuristic: parse the file again or use what we have.
		// For simplicity, we store an "effectiveLastUpdated" during scanning.
		// Actually, let's just re-parse. But that's wasteful.
		// Looking at the spec: distance = (lastUpdated*1000 or mtime) - LaunchTime
		// We'll store lastUpdated during scanning. For now, use mtime as the fallback.
		effectiveTime = c.mtime // Already in millis, represents lastUpdated*1000 or mtime
		dist := effectiveTime - launchTime
		scored_list = append(scored_list, scored{candidate: c, distance: dist})
	}

	// Sort by smallest non-negative distance, then by startTime descending.
	sort.Slice(scored_list, func(i, j int) bool {
		di, dj := scored_list[i].distance, scored_list[j].distance
		iNonNeg := di >= 0
		jNonNeg := dj >= 0

		// Non-negative distances come before negative ones.
		if iNonNeg != jNonNeg {
			return iNonNeg
		}

		// Both non-negative: smaller distance wins.
		// Both negative: larger (closer to zero) distance wins.
		if di != dj {
			if iNonNeg {
				return di < dj
			}
			return di > dj
		}

		// Tied distance: newer startTime first.
		return scored_list[i].candidate.startTime > scored_list[j].candidate.startTime
	})

	// Check if top candidate is unique (no tie with #2).
	if len(scored_list) == 1 {
		winner := scored_list[0].candidate
		log.Printf("gemini_session_watcher: bound %s (session %s)", winner.path, winner.sessionID)
		if w.config.OnBound != nil {
			w.config.OnBound(winner.path, winner.sessionID)
		}
		return true
	}

	// Check for tie between top two.
	top := scored_list[0]
	second := scored_list[1]
	if top.distance == second.distance && top.candidate.startTime == second.candidate.startTime {
		log.Printf("gemini_session_watcher: %d candidates tied, downgrading", len(candidates))
		if w.config.OnDowngrade != nil {
			w.config.OnDowngrade()
		}
		return true
	}

	// Single winner.
	winner := top.candidate
	log.Printf("gemini_session_watcher: bound %s (session %s)", winner.path, winner.sessionID)
	if w.config.OnBound != nil {
		w.config.OnBound(winner.path, winner.sessionID)
	}
	return true
}

// scanCandidates scans matching hash directories for valid session file candidates.
// A file qualifies when:
//   - Its mtime >= (LaunchTime - MtimeWindow)
//   - Fresh session: startTime >= (LaunchTime - MtimeWindow)
//   - Resumed session: startTime < LaunchTime AND mtime >= (LaunchTime - MtimeWindow)
//
// Returns the list of valid candidates and whether any partial candidates were seen.
func (w *GeminiSessionWatcher) scanCandidates() (valid []geminiCandidate, hasPartial bool) {
	hashDirs := w.matchingHashDirs()

	launchMillis := w.config.LaunchTime
	mtimeWindowMillis := w.config.MtimeWindow.Milliseconds()
	earliestMtimeMillis := launchMillis - mtimeWindowMillis

	for _, hashDir := range hashDirs {
		for _, candidateDir := range []string{hashDir, filepath.Join(hashDir, "chats")} {
			entries, err := os.ReadDir(candidateDir)
			if err != nil {
				if !os.IsNotExist(err) {
					w.reportError(fmt.Errorf("scan hash dir %s: %w", candidateDir, err))
				}
				continue
			}

			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}

				absPath := filepath.Join(candidateDir, entry.Name())
				info, err := entry.Info()
				if err != nil {
					w.reportError(fmt.Errorf("stat %s: %w", absPath, err))
					continue
				}

				mtimeMillis := info.ModTime().UnixMilli()

				// Pre-filter by mtime: reject files modified before (LaunchTime - MtimeWindow).
				if mtimeMillis < earliestMtimeMillis {
					continue
				}

				// Parse session header.
				header, err := readGeminiSessionHeader(absPath)
				if err != nil {
					w.reportError(fmt.Errorf("parse session %s: %w", absPath, err))
					continue
				}

				startTimeMillis := int64(header.StartTime * 1000)

				// Check candidate qualification:
				// Fresh session: startTime >= (LaunchTime - MtimeWindow)
				// Resumed session: startTime < LaunchTime AND mtime >= (LaunchTime - MtimeWindow)
				// Both cases already pass the mtime pre-filter above.
				isFresh := startTimeMillis >= (launchMillis - mtimeWindowMillis)
				isResumed := startTimeMillis < launchMillis && mtimeMillis >= earliestMtimeMillis

				if !isFresh && !isResumed {
					continue
				}

				// For tie-break: effectiveTime = lastUpdated*1000 or mtime
				var effectiveTime int64
				if header.LastUpdated > 0 {
					effectiveTime = int64(header.LastUpdated * 1000)
				} else {
					effectiveTime = mtimeMillis
				}

				valid = append(valid, geminiCandidate{
					path:      absPath,
					sessionID: header.SessionID,
					startTime: startTimeMillis,
					mtime:     effectiveTime,
				})
			}
		}
	}

	return valid, hasPartial
}

// matchingHashDirs returns subdirectories of ~/.gemini/tmp/ whose .project_root
// file matches the configured WorkingDir (both resolved via EvalSymlinks).
func (w *GeminiSessionWatcher) matchingHashDirs() []string {
	entries, err := os.ReadDir(w.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			w.reportError(fmt.Errorf("scan base dir %s: %w", w.baseDir, err))
		}
		return nil
	}

	resolvedWorkDir := w.config.WorkingDir
	if resolved, err := filepath.EvalSymlinks(w.config.WorkingDir); err == nil {
		resolvedWorkDir = resolved
	}

	var matched []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		hashDir := filepath.Join(w.baseDir, entry.Name())
		projectRootPath := filepath.Join(hashDir, ".project_root")

		data, err := os.ReadFile(projectRootPath)
		if err != nil {
			// .project_root missing or unreadable — skip this dir.
			continue
		}

		projectRoot := strings.TrimSpace(string(data))
		if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil {
			projectRoot = resolved
		}

		if dirMatchesOrContains(projectRoot, resolvedWorkDir) {
			matched = append(matched, hashDir)
		}
	}

	return matched
}

// dirMatchesOrContains returns true if a == b, or one is an ancestor of the other.
// Both paths should already be cleaned/resolved.
func dirMatchesOrContains(a, b string) bool {
	if a == b {
		return true
	}
	// a is under b (user ran agent from subdirectory of host cwd)
	if strings.HasPrefix(a, b+string(filepath.Separator)) {
		return true
	}
	// b is under a (host cwd is a subdirectory of project root)
	if strings.HasPrefix(b, a+string(filepath.Separator)) {
		return true
	}
	return false
}

// readGeminiSessionHeader reads and parses the top-level fields from a Gemini
// session JSON file.
func readGeminiSessionHeader(path string) (*geminiSessionHeader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var header geminiSessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("unmarshal session header: %w", err)
	}

	return &header, nil
}

func (w *GeminiSessionWatcher) reportError(err error) {
	if w.config.OnError != nil {
		w.config.OnError(err)
	}
}
