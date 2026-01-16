package diff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// IncludeUntracked includes untracked files (git ls-files --others --exclude-standard).
	// These are presented as new files in the diff.
	IncludeUntracked bool

	// ExcludePaths contains repo-relative paths to skip when generating
	// diffs (useful for internal files like host storage).
	ExcludePaths []string

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

	// Optionally include untracked files
	if p.config.IncludeUntracked {
		untrackedDiff, err := p.getUntrackedDiff()
		if err != nil {
			// If we fail to get untracked files, we shouldn't fail the whole poll,
			// as the main diff might still be valid. But we should report it.
			// For now, return error to be consistent with other parts.
			return "", err
		}
		if untrackedDiff != "" {
			if result != "" {
				result += "\n"
			}
			result += untrackedDiff
		}
	}

	return result, nil
}

// getUntrackedDiff returns synthetic diffs for untracked files.
func (p *Poller) getUntrackedDiff() (string, error) {
	cmd := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	cmd.Dir = p.config.RepoPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(files) == 0 || (len(files) == 1 && files[0] == "") {
		return "", nil
	}

	var sb strings.Builder
	for _, file := range files {
		if file == "" {
			continue
		}
		if p.shouldExclude(file) {
			continue
		}
		// Generate synthetic diff
		diff, err := p.generateSyntheticDiff(file)
		if err != nil {
			// If we can't read a file (e.g. permission denied, or deleted during race), just skip it
			continue
		}
		sb.WriteString(diff)
	}
	return sb.String(), nil
}

// shouldExclude returns true if the path should be skipped.
// Paths are compared using repo-relative, slash-separated values.
func (p *Poller) shouldExclude(relPath string) bool {
	if relPath == "" {
		return false
	}

	normalizedPath := filepath.ToSlash(filepath.Clean(relPath))

	for _, exclude := range p.config.ExcludePaths {
		if exclude == "" {
			continue
		}
		normalizedExclude := filepath.ToSlash(filepath.Clean(exclude))
		normalizedExclude = strings.TrimPrefix(normalizedExclude, "./")
		if normalizedExclude == "." {
			continue
		}
		if normalizedPath == normalizedExclude || strings.HasPrefix(normalizedPath, normalizedExclude+"/") {
			return true
		}
	}

	return false
}

// generateSyntheticDiff generates a git-compatible diff for a new untracked file.
func (p *Poller) generateSyntheticDiff(relPath string) (string, error) {
	fullPath := filepath.Join(p.config.RepoPath, relPath)

	// Use Lstat to get file info without following symlinks.
	// This is critical for proper symlink handling - os.Stat would follow the link
	// and return the target's info (or error if target is missing).
	info, err := os.Lstat(fullPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", nil
	}

	// Check if this is a symbolic link - git represents these specially
	// with mode 120000 and the link target path as content.
	if info.Mode()&os.ModeSymlink != 0 {
		return p.generateSymlinkDiff(relPath, fullPath)
	}

	// Read file content
	f, err := os.Open(fullPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Read first 8KB to check for binary
	buf := make([]byte, 8000)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	head := buf[:n]

	isBinary := bytes.IndexByte(head, 0) != -1

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("diff --git a/%s b/%s\n", relPath, relPath))
	sb.WriteString(fmt.Sprintf("new file mode %o\n", info.Mode().Perm()))

	if isBinary {
		// Read rest of file to compute content hash for change detection.
		// Without this hash, CardStreamer cannot detect when binary file contents change
		// because ExtractFileDiffSection would return identical output.
		rest, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		content := append(head, rest...)

		// Compute content hash (7 chars like git's abbreviated blob hashes).
		// Format: index 0000000..<hash> where 0000000 represents /dev/null (new file).
		hash := sha256.Sum256(content)
		contentHash := hex.EncodeToString(hash[:])[:7]

		sb.WriteString(fmt.Sprintf("index 0000000..%s\n", contentHash))
		sb.WriteString(fmt.Sprintf("Binary files /dev/null and b/%s differ\n", relPath))
	} else {
		// Read the rest of the file if needed
		var content []byte
		if err == io.EOF {
			content = head
		} else {
			rest, err := io.ReadAll(f)
			if err != nil {
				return "", err
			}
			content = append(head, rest...)
		}

		// Check if file is empty - still emit the diff header so the file is visible
		if len(content) == 0 {
			// Empty files have no content lines, but we emit ---/+++ and an empty hunk
			// so the file appears in the review flow. The @@ -0,0 +0,0 @@ hunk indicates
			// zero lines changed (consistent with git's format for empty additions).
			sb.WriteString("--- /dev/null\n")
			sb.WriteString(fmt.Sprintf("+++ b/%s\n", relPath))
			sb.WriteString("@@ -0,0 +0,0 @@\n")
			return sb.String(), nil
		}

		// Count lines
		lc := bytes.Count(content, []byte{'\n'})
		if len(content) > 0 && content[len(content)-1] != '\n' {
			lc++
		}

		sb.WriteString("--- /dev/null\n")
		sb.WriteString(fmt.Sprintf("+++ b/%s\n", relPath))
		sb.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", lc))

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				break
			}
			sb.WriteString("+" + line + "\n")
		}

		if len(content) > 0 && content[len(content)-1] != '\n' {
			sb.WriteString("\\ No newline at end of file\n")
		}
	}

	return sb.String(), nil
}

// generateSymlinkDiff generates a git-compatible diff for a symbolic link.
// Git represents symlinks with mode 120000 and the link target path as content
// (without a trailing newline). This matches git's behavior exactly.
func (p *Poller) generateSymlinkDiff(relPath, fullPath string) (string, error) {
	// Read the symlink target - this is what git uses as the "content"
	target, err := os.Readlink(fullPath)
	if err != nil {
		return "", err
	}

	// Compute hash for change detection (same logic as binary files)
	hash := sha256.Sum256([]byte(target))
	contentHash := hex.EncodeToString(hash[:])[:7]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("diff --git a/%s b/%s\n", relPath, relPath))
	// 120000 is git's mode for symbolic links
	sb.WriteString("new file mode 120000\n")
	sb.WriteString(fmt.Sprintf("index 0000000..%s\n", contentHash))
	sb.WriteString("--- /dev/null\n")
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", relPath))
	sb.WriteString("@@ -0,0 +1 @@\n")
	sb.WriteString("+" + target + "\n")
	// Git always emits "No newline at end of file" for symlinks since
	// the target path is stored without a trailing newline
	sb.WriteString("\\ No newline at end of file\n")

	return sb.String(), nil
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

// SnapshotRaw returns the current diff without updating the poller's hash.
// Use this for read-only checks that should not suppress the next poll tick.
func (p *Poller) SnapshotRaw() ([]*Chunk, string, error) {
	diffOutput, err := p.runGitDiff()
	if err != nil {
		return nil, "", err
	}

	return p.parser.Parse(diffOutput), diffOutput, nil
}
