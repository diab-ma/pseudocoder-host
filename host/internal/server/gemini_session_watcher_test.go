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
	testGeminiWatcherPollInterval     = 10 * time.Millisecond
	testGeminiWatcherDiscoveryTimeout = 200 * time.Millisecond
	testGeminiWatcherMtimeWindow      = 5 * time.Second
	testGeminiWatcherWait             = 500 * time.Millisecond
)

// setupGeminiHashDir creates a hash subdirectory with a .project_root file.
func setupGeminiHashDir(t *testing.T, baseDir, hash, projectRoot string) string {
	t.Helper()
	hashDir := filepath.Join(baseDir, hash)
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		t.Fatalf("create hash dir: %v", err)
	}
	projectRootPath := filepath.Join(hashDir, ".project_root")
	if err := os.WriteFile(projectRootPath, []byte(projectRoot), 0644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}
	return hashDir
}

// writeGeminiSessionFile writes a Gemini session JSON file with given fields and mtime.
func writeGeminiSessionFile(t *testing.T, dir, name, sessionID string, startTime, lastUpdated float64, mtime time.Time) string {
	t.Helper()
	header := map[string]interface{}{
		"sessionId":   sessionID,
		"startTime":   startTime,
		"lastUpdated": lastUpdated,
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

func writeGeminiSessionFileWithStringTimestamps(t *testing.T, dir, name, sessionID, startTime, lastUpdated string, mtime time.Time) string {
	t.Helper()
	header := map[string]interface{}{
		"sessionId":   sessionID,
		"startTime":   startTime,
		"lastUpdated": lastUpdated,
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

// newTestGeminiWatcher creates a GeminiSessionWatcher with the baseDir overridden
// for testing (bypasses NewGeminiSessionWatcher which resolves ~/.gemini/tmp/).
func newTestGeminiWatcher(config GeminiSessionWatcherConfig, baseDir string) *GeminiSessionWatcher {
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
	return &GeminiSessionWatcher{
		config:  config,
		baseDir: baseDir,
	}
}

func TestGeminiWatcher_SingleCandidateBinds(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	hashDir := setupGeminiHashDir(t, baseDir, "abc123", workDir)
	writeGeminiSessionFile(t, hashDir, "session_1.json", "sess-001",
		startTimeSec, lastUpdatedSec, launchTime)

	var boundPath string
	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundPath = filePath
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		gotPath := boundPath
		gotSession := boundSession
		boundMu.Unlock()
		expectedPath := filepath.Join(hashDir, "session_1.json")
		if gotPath != expectedPath {
			t.Errorf("bound path = %q, want %q", gotPath, expectedPath)
		}
		if gotSession != "sess-001" {
			t.Errorf("bound sessionID = %q, want %q", gotSession, "sess-001")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_BindsChatSubdirFileWithStringTimestamps(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	hashDir := setupGeminiHashDir(t, baseDir, "abc123", workDir)
	expectedPath := writeGeminiSessionFileWithStringTimestamps(
		t,
		filepath.Join(hashDir, "chats"),
		"session_1.json",
		"sess-001",
		launchTime.Add(100*time.Millisecond).UTC().Format(time.RFC3339Nano),
		launchTime.Add(200*time.Millisecond).UTC().Format(time.RFC3339Nano),
		launchTime,
	)

	var boundPath string
	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundPath = filePath
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		gotPath := boundPath
		gotSession := boundSession
		boundMu.Unlock()
		if gotPath != expectedPath {
			t.Errorf("bound path = %q, want %q", gotPath, expectedPath)
		}
		if gotSession != "sess-001" {
			t.Errorf("bound sessionID = %q, want %q", gotSession, "sess-001")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_ZeroCandidatesDowngrades(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()

	// Create hash dir with matching project root but no session files.
	setupGeminiHashDir(t, baseDir, "abc123", workDir)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchTime.UnixMilli(),
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_ProjectRootMatching(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// Hash dir with a different project root — should be skipped.
	otherHashDir := setupGeminiHashDir(t, baseDir, "other123", otherDir)
	writeGeminiSessionFile(t, otherHashDir, "session_other.json", "sess-other",
		startTimeSec, lastUpdatedSec, launchTime)

	// Hash dir matching our working dir.
	matchHashDir := setupGeminiHashDir(t, baseDir, "match123", workDir)
	writeGeminiSessionFile(t, matchHashDir, "session_match.json", "sess-match",
		startTimeSec, lastUpdatedSec, launchTime)

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-match" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-match")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_ProjectRootMissing(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// Create hash dir without .project_root.
	hashDir := filepath.Join(baseDir, "noprootdir")
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeGeminiSessionFile(t, hashDir, "session.json", "sess-orphan",
		startTimeSec, lastUpdatedSec, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind: dir without .project_root should be skipped")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — dir without .project_root is skipped.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_MultipleHashDirsOnlyOneMatches(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	otherDir1 := t.TempDir()
	otherDir2 := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// Two non-matching hash dirs with session files.
	h1 := setupGeminiHashDir(t, baseDir, "hash1", otherDir1)
	writeGeminiSessionFile(t, h1, "s1.json", "other-1", startTimeSec, lastUpdatedSec, launchTime)
	h2 := setupGeminiHashDir(t, baseDir, "hash2", otherDir2)
	writeGeminiSessionFile(t, h2, "s2.json", "other-2", startTimeSec, lastUpdatedSec, launchTime)

	// One matching hash dir.
	hMatch := setupGeminiHashDir(t, baseDir, "hash_match", workDir)
	writeGeminiSessionFile(t, hMatch, "s_match.json", "match-1", startTimeSec, lastUpdatedSec, launchTime)

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "match-1" {
			t.Errorf("bound sessionID = %q, want %q", got, "match-1")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_ResumedSession(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// Resumed session: startTime well before launch, but mtime is fresh.
	startTimeSec := float64(launchMillis-30000) / 1000.0 // 30s before launch
	lastUpdatedSec := float64(launchMillis+100) / 1000.0

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "resumed.json", "sess-resumed",
		startTimeSec, lastUpdatedSec, launchTime.Add(100*time.Millisecond))

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: resumed session should bind")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-resumed" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-resumed")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_StaleSessionRejected(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// Stale session: startTime before launch AND mtime is stale (before launch - mtimeWindow).
	startTimeSec := float64(launchMillis-30000) / 1000.0
	lastUpdatedSec := float64(launchMillis-20000) / 1000.0
	staleMtime := launchTime.Add(-10 * time.Second) // Beyond the 5s window

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "stale.json", "sess-stale",
		startTimeSec, lastUpdatedSec, staleMtime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind: stale session should be rejected")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — stale session rejected.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_FreshSession(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// Fresh session: startTime >= (LaunchTime - MtimeWindow)
	startTimeSec := float64(launchMillis+500) / 1000.0
	lastUpdatedSec := float64(launchMillis+600) / 1000.0

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "fresh.json", "sess-fresh",
		startTimeSec, lastUpdatedSec, launchTime.Add(500*time.Millisecond))

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: fresh session should bind")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-fresh" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-fresh")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_TieBreakPicksClosest(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// Two candidates with different distances to launch time.
	// Candidate A: lastUpdated close to launch (distance=100ms)
	startA := float64(launchMillis+50) / 1000.0
	lastUpdatedA := float64(launchMillis+100) / 1000.0

	// Candidate B: lastUpdated farther from launch (distance=5000ms)
	startB := float64(launchMillis+50) / 1000.0
	lastUpdatedB := float64(launchMillis+5000) / 1000.0

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "close.json", "sess-close",
		startA, lastUpdatedA, launchTime.Add(100*time.Millisecond))
	writeGeminiSessionFile(t, hashDir, "far.json", "sess-far",
		startB, lastUpdatedB, launchTime.Add(5*time.Second))

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: tie-break should pick closest")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-close" {
			t.Errorf("bound sessionID = %q, want %q (closest to launch)", got, "sess-close")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_MultipleTiedCandidatesDowngrades(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// Two candidates with identical distance and startTime → tie → downgrade.
	startSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "tied_1.json", "sess-tied-1",
		startSec, lastUpdatedSec, launchTime)
	writeGeminiSessionFile(t, hashDir, "tied_2.json", "sess-tied-2",
		startSec, lastUpdatedSec, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind: tied candidates should downgrade")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — tied candidates cause downgrade.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_MtimePreFilterRejectsOld(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	// File has fresh startTime but very old mtime (beyond mtime window).
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0
	oldMtime := launchTime.Add(-1 * time.Hour)

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)
	writeGeminiSessionFile(t, hashDir, "old_mtime.json", "sess-old",
		startTimeSec, lastUpdatedSec, oldMtime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind: old mtime should be pre-filtered")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — old mtime pre-filtered.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_SessionCloseStopsWatcher(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()

	// No candidates — watcher would normally poll until timeout.
	setupGeminiHashDir(t, baseDir, "abc", workDir)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchTime.UnixMilli(),
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: 10 * time.Second, // long timeout
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind")
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade during stop")
		},
	}, baseDir)

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
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("Stop() did not return promptly")
	}
}

func TestGeminiWatcher_DoubleStopSafe(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchTime.UnixMilli(),
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: 10 * time.Second,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
	}, baseDir)

	w.Start()
	w.Stop()
	w.Stop() // Should not panic or block.
}

func TestGeminiWatcher_DirectoryNotExist(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "nonexistent")
	workDir := t.TempDir()
	launchTime := time.Now()

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchTime.UnixMilli(),
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestGeminiWatcher_SubdirectoryProjectRoot(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// .project_root points to workDir + "/subdir" (agent ran from subdirectory).
	// Create the subdir so EvalSymlinks can resolve it (macOS /var -> /private/var).
	subDir := filepath.Join(workDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	hashDir := setupGeminiHashDir(t, baseDir, "abc123", subDir)
	writeGeminiSessionFile(t, hashDir, "session.json", "sess-sub",
		startTimeSec, lastUpdatedSec, launchTime)

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: subdirectory project root should match")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-sub" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-sub")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_ParentProjectRoot(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// .project_root points to parent of workDir.
	hashDir := setupGeminiHashDir(t, baseDir, "abc123", filepath.Dir(workDir))
	writeGeminiSessionFile(t, hashDir, "session.json", "sess-parent",
		startTimeSec, lastUpdatedSec, launchTime)

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: parent project root should match")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-parent" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-parent")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_UnrelatedProjectRoot(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	unrelatedDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// .project_root points to a completely unrelated directory.
	hashDir := setupGeminiHashDir(t, baseDir, "abc123", unrelatedDir)
	writeGeminiSessionFile(t, hashDir, "session.json", "sess-unrelated",
		startTimeSec, lastUpdatedSec, launchTime)

	downgradeCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       workDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			t.Error("unexpected bind: unrelated project root should not match")
		},
		OnDowngrade: func() {
			downgradeCh <- struct{}{}
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-downgradeCh:
		// Expected — unrelated project root should not match.
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for downgrade")
	}
}

func TestDirMatchesOrContains(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"exact match", "/foo/bar", "/foo/bar", true},
		{"a is child of b", "/foo/bar/baz", "/foo/bar", true},
		{"b is child of a", "/foo/bar", "/foo/bar/baz", true},
		{"unrelated", "/foo/bar", "/qux/baz", false},
		{"prefix but not subdir", "/foo/bar-baz", "/foo/bar", false},
		{"prefix but not subdir reverse", "/foo/bar", "/foo/bar-baz", false},
		{"deeply nested child", "/foo/bar/baz/qux", "/foo/bar", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dirMatchesOrContains(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("dirMatchesOrContains(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestGeminiWatcher_FreshPreferenceSkipsStaleFile(t *testing.T) {
	// Simulates the race condition: Gemini CLI touches an OLD session file
	// (updating its mtime) before creating the actual new session file.
	// The watcher should wait for the fresh file rather than binding the stale one.
	baseDir := t.TempDir()
	workDir := t.TempDir()
	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()

	hashDir := setupGeminiHashDir(t, baseDir, "abc", workDir)

	// Stale session: startTime 17 minutes before launch, but mtime is fresh
	// (simulates Gemini CLI touching/re-saving an old session after PTY launch).
	staleStartSec := float64(launchMillis-17*60*1000) / 1000.0
	staleLastUpdatedSec := float64(launchMillis+8000) / 1000.0
	writeGeminiSessionFile(t, hashDir, "session-old.json", "sess-stale",
		staleStartSec, staleLastUpdatedSec, launchTime.Add(8*time.Second))

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:            workDir,
		LaunchTime:            launchMillis,
		PollInterval:          testGeminiWatcherPollInterval,
		DiscoveryTimeout:      2 * time.Second,
		MtimeWindow:           testGeminiWatcherMtimeWindow,
		FreshPreferenceWindow: 500 * time.Millisecond,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	// After a small delay, create the fresh session file (simulates Gemini CLI
	// creating the actual new session ~27s after launch, compressed to 50ms here).
	time.Sleep(50 * time.Millisecond)
	freshStartSec := float64(launchMillis+50) / 1000.0
	freshLastUpdatedSec := float64(launchMillis+100) / 1000.0
	writeGeminiSessionFile(t, hashDir, "session-new.json", "sess-fresh",
		freshStartSec, freshLastUpdatedSec, launchTime.Add(50*time.Millisecond))

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-fresh" {
			t.Errorf("bound sessionID = %q, want %q (should skip stale, bind fresh)", got, "sess-fresh")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for bind")
	}
}

func TestGeminiWatcher_CWDNormalization(t *testing.T) {
	baseDir := t.TempDir()
	// Create a real directory and a symlink to it.
	realDir := filepath.Join(t.TempDir(), "realproject")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(t.TempDir(), "linkproject")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatal(err)
	}

	launchTime := time.Now()
	launchMillis := launchTime.UnixMilli()
	startTimeSec := float64(launchMillis+100) / 1000.0
	lastUpdatedSec := float64(launchMillis+200) / 1000.0

	// .project_root points to the real path.
	hashDir := setupGeminiHashDir(t, baseDir, "abc", realDir)
	writeGeminiSessionFile(t, hashDir, "session.json", "sess-sym",
		startTimeSec, lastUpdatedSec, launchTime)

	var boundSession string
	var boundMu sync.Mutex
	boundCh := make(chan struct{}, 1)

	// WorkingDir is the symlink — should resolve and match.
	w := newTestGeminiWatcher(GeminiSessionWatcherConfig{
		WorkingDir:       symlinkDir,
		LaunchTime:       launchMillis,
		PollInterval:     testGeminiWatcherPollInterval,
		DiscoveryTimeout: testGeminiWatcherDiscoveryTimeout,
		MtimeWindow:      testGeminiWatcherMtimeWindow,
		OnBound: func(filePath string, sessionID string) {
			boundMu.Lock()
			boundSession = sessionID
			boundMu.Unlock()
			boundCh <- struct{}{}
		},
		OnDowngrade: func() {
			t.Error("unexpected downgrade: symlink should resolve and match")
		},
	}, baseDir)

	w.Start()
	defer w.Stop()

	select {
	case <-boundCh:
		boundMu.Lock()
		got := boundSession
		boundMu.Unlock()
		if got != "sess-sym" {
			t.Errorf("bound sessionID = %q, want %q", got, "sess-sym")
		}
	case <-time.After(testGeminiWatcherWait):
		t.Fatal("timed out waiting for bind")
	}
}
