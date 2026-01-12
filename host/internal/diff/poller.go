package diff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"sync"
	"time"
)

// PollerConfig holds configuration for the diff poller.
type PollerConfig struct {
	// RepoPath is the path to the git repository to monitor.
	RepoPath string

	// PollInterval is how often to check for changes.
	PollInterval time.Duration

	// IncludeStaged includes staged changes (git diff --cached) in addition
	// to unstaged changes. When false, only unstaged changes are detected.
	IncludeStaged bool

	// OnChunks is called whenever new or changed chunks are detected.
	// It receives the full list of current chunks.
	OnChunks func(chunks []*Chunk)

	// OnChunksRaw is an alternative to OnChunks that includes the raw diff output.
	// If set, OnChunks is ignored. The raw output allows detecting binary files
	// and calculating diff statistics.
	OnChunksRaw func(chunks []*Chunk, rawDiff string)

	// OnError is called when git diff fails (e.g., not a git repo, git not installed).
	// If nil, errors are silently ignored.
	OnError func(err error)
}

// Poller monitors a git repository for changes and emits chunks.
// It tracks the previous diff state to avoid duplicate notifications.
type Poller struct {
	config   PollerConfig  // Immutable config for polling behavior.
	parser   *Parser       // Parses diff output into chunks.
	stopCh   chan struct{} // Signals the polling loop to stop.
	doneCh   chan struct{} // Closes when the polling loop exits.
	mu       sync.Mutex    // Guards lifecycle state and lastHash updates.
	lastHash string        // Tracks last diff hash to suppress duplicates.
	running  bool          // True while a pollLoop goroutine is active.
	stopping bool          // True while Stop is waiting for pollLoop to exit.
}

// NewPoller creates a new diff poller with the given configuration.
func NewPoller(config PollerConfig) *Poller {
	return &Poller{
		config: config,
		parser: NewParser(),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start begins polling for git diff changes.
// It runs in a goroutine and can be stopped with Stop().
// Start is safe to call after Stop - the poller will restart with fresh channels.
func (p *Poller) Start() {
	p.mu.Lock()
	if p.running || p.stopping {
		p.mu.Unlock()
		return
	}
	p.running = true
	// Recreate channels to allow restart after Stop
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go p.pollLoop()
}

// Stop halts the polling loop and waits for it to finish.
func (p *Poller) Stop() {
	p.mu.Lock()
	if !p.running || p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	// Capture channels under lock before closing
	stopCh := p.stopCh
	doneCh := p.doneCh
	p.mu.Unlock()

	close(stopCh)
	<-doneCh

	p.mu.Lock()
	p.running = false
	p.stopping = false
	p.mu.Unlock()
}

// Done returns a channel that closes when the poller has stopped.
// Note: The channel is recreated on each Start(), so callers should
// call Done() after Start() to get the correct channel for that run.
func (p *Poller) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doneCh
}

// pollLoop runs the main polling loop.
func (p *Poller) pollLoop() {
	defer close(p.doneCh)

	// Do an initial poll immediately
	p.poll()

	// Use a sensible default if poll interval is zero or negative.
	// This prevents spinning and provides consistent behavior.
	interval := p.config.PollInterval
	if interval <= 0 {
		interval = 1 * time.Second // Default to 1 second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

// poll runs git diff and processes the output.
func (p *Poller) poll() {
	diffOutput, err := p.runGitDiff()
	if err != nil {
		if p.config.OnError != nil {
			p.config.OnError(err)
		}
		return
	}

	// Compute hash of current diff state
	currentHash := p.hashDiff(diffOutput)

	p.mu.Lock()
	lastHash := p.lastHash
	p.lastHash = currentHash
	p.mu.Unlock()

	// Only process if diff has changed
	if currentHash == lastHash {
		return
	}

	// Parse into chunks
	chunks := p.parser.Parse(diffOutput)

	// Notify callback if we have chunks (or if diff cleared)
	// Prefer OnChunksRaw if set, as it provides more information
	if p.config.OnChunksRaw != nil {
		p.config.OnChunksRaw(chunks, diffOutput)
	} else if p.config.OnChunks != nil {
		p.config.OnChunks(chunks)
	}
}

// runGitDiff executes git diff and returns the output.
// If IncludeStaged is true, it also includes staged changes (git diff --cached).
func (p *Poller) runGitDiff() (string, error) {
	// Get unstaged changes
	cmd := exec.Command("git", "diff")
	cmd.Dir = p.config.RepoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}

	result := stdout.String()

	// Optionally include staged changes
	if p.config.IncludeStaged {
		cmd = exec.Command("git", "diff", "--cached")
		cmd.Dir = p.config.RepoPath

		stdout.Reset()
		stderr.Reset()
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return "", err
		}

		stagedDiff := stdout.String()
		if stagedDiff != "" {
			if result != "" {
				result += "\n"
			}
			result += stagedDiff
		}
	}

	return result, nil
}

// hashDiff computes a hash of the diff output for change detection.
func (p *Poller) hashDiff(diffOutput string) string {
	if diffOutput == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(diffOutput))
	return hex.EncodeToString(hash[:])
}

// PollOnce performs a single poll and returns the current chunks.
// Useful for testing and initial state fetching.
func (p *Poller) PollOnce() ([]*Chunk, error) {
	diffOutput, err := p.runGitDiff()
	if err != nil {
		return nil, err
	}

	// Update hash
	currentHash := p.hashDiff(diffOutput)
	p.mu.Lock()
	p.lastHash = currentHash
	p.mu.Unlock()

	return p.parser.Parse(diffOutput), nil
}

// PollOnceRaw performs a single poll and returns both chunks and raw diff output.
// This allows callers to detect binary files and calculate diff stats.
func (p *Poller) PollOnceRaw() ([]*Chunk, string, error) {
	diffOutput, err := p.runGitDiff()
	if err != nil {
		return nil, "", err
	}

	// Update hash
	currentHash := p.hashDiff(diffOutput)
	p.mu.Lock()
	p.lastHash = currentHash
	p.mu.Unlock()

	return p.parser.Parse(diffOutput), diffOutput, nil
}
