package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileWatchEvent represents a single filesystem change event.
type FileWatchEvent struct {
	Path   string // Canonical repo-relative slash-separated path
	Change string // "created", "modified", or "deleted"
}

// FileWatchPollerConfig configures the filesystem change poller.
type FileWatchPollerConfig struct {
	RepoPath     string
	PollInterval time.Duration
	OnEvents     func([]FileWatchEvent)
	OnError      func(error)
}

// snapshotEntry holds metadata for a single filesystem entry.
type snapshotEntry struct {
	isDir      bool
	size       int64
	modTime    time.Time
	symlinkTgt string
}

// FileWatchPoller detects external filesystem changes by periodic scanning.
type FileWatchPoller struct {
	config             FileWatchPollerConfig
	snapshot           map[string]snapshotEntry
	stopCh             chan struct{}
	doneCh             chan struct{}
	mu                 sync.Mutex
	running            bool
	stopping           bool
	scanning           bool
	lastErrorSignature string
}

// NewFileWatchPoller creates a new poller (not started).
func NewFileWatchPoller(config FileWatchPollerConfig) *FileWatchPoller {
	return &FileWatchPoller{
		config: config,
	}
}

// Start begins the poll loop in a background goroutine.
func (p *FileWatchPoller) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.running = true
	p.stopping = false
	stopCh := p.stopCh
	doneCh := p.doneCh
	p.mu.Unlock()

	go p.pollLoop(stopCh, doneCh)
}

// Stop signals the poll loop to exit and waits for it to finish.
func (p *FileWatchPoller) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	if p.stopping {
		doneCh := p.doneCh
		p.mu.Unlock()
		<-doneCh
		return
	}

	p.stopping = true
	stopCh := p.stopCh
	doneCh := p.doneCh
	p.mu.Unlock()

	close(stopCh)
	<-doneCh

	p.mu.Lock()
	p.running = false
	p.stopping = false
	p.scanning = false
	p.lastErrorSignature = ""
	p.mu.Unlock()
}

// pollLoop builds the baseline snapshot, then periodically scans for changes.
func (p *FileWatchPoller) pollLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
	defer close(doneCh)

	// Build baseline snapshot (no events emitted).
	baseline, errPaths := p.scan()
	p.mu.Lock()
	p.snapshot = baseline
	p.mu.Unlock()
	p.reportScanErrors(errPaths)

	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick performs a single poll cycle with overlap protection.
func (p *FileWatchPoller) tick() {
	p.mu.Lock()
	if p.scanning {
		p.mu.Unlock()
		return
	}
	p.scanning = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.scanning = false
		p.mu.Unlock()
	}()

	newSnap, errPaths := p.scan()
	p.reportScanErrors(errPaths)

	p.mu.Lock()
	oldSnap := p.snapshot
	p.snapshot = newSnap
	p.mu.Unlock()

	events := diffSnapshots(oldSnap, newSnap, errPaths)
	if len(events) > 0 && p.config.OnEvents != nil {
		p.config.OnEvents(events)
	}
}

// scan walks the repo tree and returns a snapshot of all entries.
// It skips .git, .pseudocoder-write-* temp files, and special files.
// Returns the snapshot and a set of paths that had errors (excluded from diff).
func (p *FileWatchPoller) scan() (map[string]snapshotEntry, map[string]bool) {
	snap := make(map[string]snapshotEntry)
	errPaths := make(map[string]bool)

	_ = filepath.Walk(p.config.RepoPath, func(absPath string, info os.FileInfo, err error) error {
		if err != nil {
			relPath, relErr := filepath.Rel(p.config.RepoPath, absPath)
			if relErr != nil {
				return nil
			}
			relPath = filepath.ToSlash(relPath)

			// If a non-root path disappears during the scan, treat it as a real
			// filesystem delta (delete/rename), not an error that suppresses delete
			// inference. This keeps rename emission deterministic as deleted+created.
			if os.IsNotExist(err) && relPath != "." {
				return nil
			}

			errPaths[relPath] = true
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Compute repo-relative path.
		relPath, relErr := filepath.Rel(p.config.RepoPath, absPath)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		// Skip repo root itself.
		if relPath == "." {
			return nil
		}

		name := info.Name()

		// Skip .git for both regular repos (.git dir) and worktree repos
		// (.git file that points to the shared gitdir).
		if name == ".git" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip .pseudocoder-write-* temp files.
		if strings.HasPrefix(name, ".pseudocoder-write-") {
			return nil
		}

		// Skip special files (sockets, FIFOs, devices).
		mode := info.Mode()
		if mode&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice) != 0 {
			return nil
		}

		entry := snapshotEntry{
			isDir:   info.IsDir(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}

		// Capture symlink target if it's a symlink.
		if mode&os.ModeSymlink != 0 {
			tgt, err := os.Readlink(absPath)
			if err == nil {
				entry.symlinkTgt = tgt
			}
		}

		snap[relPath] = entry
		return nil
	})

	return snap, errPaths
}

// reportScanErrors emits scan diagnostics through OnError when scan failures are
// observed. Duplicate path sets are deduplicated to avoid repeated log spam.
// A clean scan resets the dedupe signature so future distinct failures are
// reported again.
func (p *FileWatchPoller) reportScanErrors(errPaths map[string]bool) {
	if len(errPaths) == 0 {
		p.mu.Lock()
		p.lastErrorSignature = ""
		p.mu.Unlock()
		return
	}

	paths := make([]string, 0, len(errPaths))
	for path := range errPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	signature := strings.Join(paths, "\x1f")

	p.mu.Lock()
	if signature == p.lastErrorSignature {
		p.mu.Unlock()
		return
	}
	p.lastErrorSignature = signature
	onError := p.config.OnError
	p.mu.Unlock()

	if onError == nil {
		return
	}

	const previewLimit = 5
	preview := paths
	if len(preview) > previewLimit {
		preview = preview[:previewLimit]
	}

	detail := strings.Join(preview, ", ")
	if len(paths) > previewLimit {
		detail = fmt.Sprintf("%s (+%d more)", detail, len(paths)-previewLimit)
	}

	onError(fmt.Errorf("file watch poll scan had errors on %d path(s): %s", len(paths), detail))
}

// isUnderErrPath reports whether path is an exact match or a descendant of any
// errored directory.  This prevents false delete inference when a directory
// scan fails and its children are missing from the new snapshot.
func isUnderErrPath(path string, errPaths map[string]bool) bool {
	if len(errPaths) == 0 {
		return false
	}
	if errPaths[path] {
		return true
	}
	for ep := range errPaths {
		// "." means the repo root path itself failed to scan, so we must
		// suppress delete inference for the entire tree.
		if ep == "." {
			return true
		}
		if strings.HasPrefix(path, ep+"/") {
			return true
		}
	}
	return false
}

// diffSnapshots computes events between old and new snapshots with deterministic ordering.
// Deleted paths come first, then created, then modified — each group sorted ascending.
func diffSnapshots(old, new map[string]snapshotEntry, errPaths map[string]bool) []FileWatchEvent {
	var events []FileWatchEvent

	// Deleted: in old, not in new, not at or under any errored path.
	var deleted []string
	for path := range old {
		if _, exists := new[path]; !exists && !isUnderErrPath(path, errPaths) {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(deleted)
	for _, path := range deleted {
		events = append(events, FileWatchEvent{Path: path, Change: "deleted"})
	}

	// Created: in new, not in old.
	var created []string
	for path := range new {
		if _, exists := old[path]; !exists {
			created = append(created, path)
		}
	}
	sort.Strings(created)
	for _, path := range created {
		events = append(events, FileWatchEvent{Path: path, Change: "created"})
	}

	// Modified: in both, metadata changed, not a directory.
	var modified []string
	for path, newEntry := range new {
		oldEntry, exists := old[path]
		if !exists {
			continue
		}
		// Suppress directory modified events.
		if newEntry.isDir {
			continue
		}
		if oldEntry.size != newEntry.size ||
			!oldEntry.modTime.Equal(newEntry.modTime) ||
			oldEntry.symlinkTgt != newEntry.symlinkTgt {
			modified = append(modified, path)
		}
	}
	sort.Strings(modified)
	for _, path := range modified {
		events = append(events, FileWatchEvent{Path: path, Change: "modified"})
	}

	return events
}
