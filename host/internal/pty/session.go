package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	// This is a third-party library for creating PTYs (pseudo-terminals).
	// PTYs let us run a command as if it were in a real terminal, which means
	// we get proper output formatting, colors, and the command behaves normally.
	"github.com/creack/pty"
)

// Session manages a PTY session for a command.
//
// A PTY (pseudo-terminal) is a pair of virtual devices: a "master" (ptmx) and
// a "slave" (pts). The command runs attached to the slave (thinking it's a
// real terminal), while we read/write to the master to capture output and
// send input.
//
// This is how terminal emulators work: they create a PTY, run your shell on
// the slave side, and display the master's output in a GUI window.
type Session struct {
	// ID is a unique identifier for this session (e.g., UUID).
	// Used by SessionManager to track multiple concurrent sessions.
	// Empty string if not managed by SessionManager (backward compatibility).
	ID string

	// Name is a human-readable name for the session (e.g., "main", "dev").
	// Optional; if empty, the ID is used for display purposes.
	Name string

	// Command is the command being executed (e.g., "/bin/bash").
	// Stored for reporting in SessionInfo.
	Command string

	// Args are the command arguments.
	// Stored for reporting in SessionInfo.
	Args []string

	// CreatedAt is when the session was created.
	// Set during Start() to track session age.
	CreatedAt time.Time

	// TmuxSession is the name of the tmux session this PTY is attached to.
	// Empty string if this is not a tmux session.
	// When set, the PTY runs `tmux attach-session -t <TmuxSession>`.
	TmuxSession string

	// cmd is the command being run (e.g., "/bin/bash" or "claude").
	// exec.Cmd is Go's way of representing an external command.
	cmd *exec.Cmd

	// ptmx is the master side of the PTY. We read command output from here
	// and write user input to here. The name comes from "PTY multiplexer".
	// It's an *os.File, meaning it acts like a file we can read/write.
	ptmx *os.File

	// buffer stores the last N lines of output for replay on reconnect.
	buffer *RingBuffer

	// done is a channel that gets closed when the session ends.
	// Channels are Go's way of communicating between goroutines.
	// A closed channel is useful for signaling "this event happened" to
	// anyone listening, because reads from a closed channel return immediately.
	done chan struct{}

	// outputDone is closed when output capture finishes.
	outputDone chan struct{}

	// err stores any error that occurred during output capture.
	err error

	// mu protects concurrent access to the session's mutable fields.
	// Without this, multiple goroutines could corrupt the data.
	mu sync.Mutex

	// running tracks whether the command is currently executing.
	running bool

	// OnOutput is an optional callback invoked for each line of output.
	// This lets the caller (e.g., the host command) react to output in
	// real-time, such as printing it or streaming it over WebSocket.
	OnOutput func(line string)

	// OnOutputWithID is like OnOutput but includes the session ID.
	// Used by SessionManager to route output to the correct session.
	// If set, this is called instead of OnOutput.
	OnOutputWithID func(sessionID, line string)
}

// SessionConfig holds configuration for a PTY session.
// Using a config struct instead of many parameters makes the API cleaner
// and allows adding new options without breaking existing code.
type SessionConfig struct {
	ID             string                         // Unique session identifier (optional, for SessionManager)
	Name           string                         // Human-readable name (optional)
	Command        string                         // The command to run (not used directly; see Start())
	Args           []string                       // Command arguments (not used directly; see Start())
	HistoryLines   int                            // How many lines to keep in the ring buffer
	OnOutput       func(line string)              // Callback for each output line
	OnOutputWithID func(sessionID, line string)   // Callback with session ID (takes precedence over OnOutput)
}

// NewSession creates a new PTY session with the given configuration.
// This only allocates the Session struct; call Start() to actually run a command.
func NewSession(cfg SessionConfig) *Session {
	historyLines := cfg.HistoryLines
	if historyLines <= 0 {
		historyLines = 5000 // Default: keep last 5000 lines
	}

	return &Session{
		ID:             cfg.ID,
		Name:           cfg.Name,
		buffer:         NewRingBuffer(historyLines),
		done:           make(chan struct{}), // Create an unbuffered channel
		outputDone:     make(chan struct{}),
		OnOutput:       cfg.OnOutput,
		OnOutputWithID: cfg.OnOutputWithID,
	}
}

// Start spawns the PTY session with the specified command and arguments.
//
// This method:
// 1. Creates the command with exec.Command()
// 2. Starts it in a PTY using the creack/pty library
// 3. Launches two goroutines: one to capture output, one to wait for exit
//
// The "..." in "args ...string" is Go's variadic parameter syntax, meaning
// you can pass any number of string arguments.
func (s *Session) Start(command string, args ...string) error {
	// Lock to prevent concurrent Start() calls
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("session already running")
	}

	// Store command info for reporting (e.g., in SessionInfo)
	s.Command = command
	s.Args = args
	s.CreatedAt = time.Now()

	// exec.Command creates a Cmd struct representing the command to run.
	// It doesn't actually start the command yet.
	s.cmd = exec.Command(command, args...)

	// pty.Start() does three things:
	// 1. Creates a new PTY (master + slave pair)
	// 2. Attaches the command's stdin/stdout/stderr to the slave
	// 3. Starts the command
	// It returns the master side (ptmx) which we use to read/write.
	ptmx, err := pty.Start(s.cmd)
	if err != nil {
		// %w wraps the original error so callers can unwrap it later
		return fmt.Errorf("failed to start PTY: %w", err)
	}

	s.ptmx = ptmx
	s.running = true

	// "go" starts a goroutine - a lightweight thread managed by Go's runtime.
	// These run concurrently with the main code. We use two goroutines:
	// 1. captureOutput: reads from PTY and stores in ring buffer
	// 2. waitForExit: waits for command to finish and signals done

	// Start capturing output in a goroutine
	go s.captureOutput()

	// Wait for the command to finish in a goroutine
	go s.waitForExit()

	return nil
}

// captureOutput reads from the PTY and processes output.
// This runs in its own goroutine so it doesn't block the main thread.
//
// Output is read in chunks (up to 4KB) and immediately forwarded to callbacks
// for real-time updates (control sequences, line edits like arrow keys and
// backspace). The ring buffer receives only complete newline-terminated lines
// for history replay on reconnect.
//
// Previously this used ReadString('\n') which blocked until a newline arrived,
// causing interactive line editing to be invisible until Enter was pressed.
func (s *Session) captureOutput() {
	defer close(s.outputDone)

	// Get a local copy of ptmx under the mutex to avoid race with waitForExit
	// which sets s.ptmx = nil after the process exits.
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()

	if ptmx == nil {
		return
	}

	// Read in chunks up to 4KB at a time. This allows control sequences
	// (arrow keys, cursor movement, etc.) to be forwarded immediately
	// without waiting for a newline.
	buf := make([]byte, 4096)

	// pendingLine accumulates partial lines (data without a trailing newline)
	// between chunks. When a newline arrives in a later chunk, we prepend
	// the pending data to form a complete line for the ring buffer.
	var pendingLine strings.Builder

	for {
		// Read returns immediately when data is available, not waiting for
		// a specific delimiter. This is key for real-time terminal updates.
		n, err := ptmx.Read(buf)

		if n > 0 {
			chunk := string(buf[:n])

			// Terminal output might contain invalid UTF-8 bytes (e.g., from
			// binary data or encoding issues). We sanitize it to prevent
			// problems when sending over JSON/WebSocket later.
			sanitized := sanitizeUTF8(chunk)

			// Forward raw chunk immediately for live updates.
			// This lets the mobile client see cursor movements, line edits,
			// and other interactive changes in real-time.
			if s.OnOutputWithID != nil {
				s.OnOutputWithID(s.ID, sanitized)
			} else if s.OnOutput != nil {
				s.OnOutput(sanitized)
			}

			// Extract complete lines for the ring buffer (used for history
			// replay on reconnect). Partial lines are accumulated in pendingLine.
			s.extractAndBufferLines(sanitized, &pendingLine)
		}

		if err != nil {
			// Flush any remaining partial line to buffer before exit.
			// This ensures the last output (even without a trailing newline)
			// is preserved in history.
			if pendingLine.Len() > 0 {
				s.buffer.Write(pendingLine.String())
			}

			// io.EOF means we reached the end of input (command exited).
			// Any other error is unexpected and we should record it.
			if err != io.EOF {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
			}
			return // Exit the goroutine
		}
	}
}

// extractAndBufferLines splits a chunk into complete lines (for the ring buffer)
// and stores any trailing partial line in pendingLine for the next chunk.
//
// The ring buffer needs complete lines for clean history replay, but we receive
// arbitrary byte chunks from the PTY. This function bridges the gap:
// 1. Prepends any pending partial line from the previous chunk
// 2. Extracts all complete lines (ending with \n) and writes to ring buffer
// 3. Stores any remaining partial line for the next chunk
func (s *Session) extractAndBufferLines(chunk string, pendingLine *strings.Builder) {
	// Prepend any pending partial line from the previous chunk
	if pendingLine.Len() > 0 {
		chunk = pendingLine.String() + chunk
		pendingLine.Reset()
	}

	// Find and buffer complete lines
	for {
		idx := strings.Index(chunk, "\n")
		if idx == -1 {
			// No more newlines - store remainder as pending for next chunk
			pendingLine.WriteString(chunk)
			return
		}

		// Extract complete line (including the newline) and write to buffer
		line := chunk[:idx+1]
		s.buffer.Write(line)
		chunk = chunk[idx+1:]
	}
}

// waitForExit waits for the command to finish and performs cleanup.
// This runs in its own goroutine.
func (s *Session) waitForExit() {
	// Wait() blocks until the command exits. The underscore (_) discards
	// the return value (exit status) since we don't need it here.
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Wait()
	}

	<-s.outputDone

	s.mu.Lock()
	s.running = false

	// Close PTY file descriptor to avoid resource leak.
	// If we don't close this, each session would leak a file descriptor,
	// and eventually we'd hit the OS limit (usually 1024 or so).
	if s.ptmx != nil {
		s.ptmx.Close()
		s.ptmx = nil // Set to nil to prevent double-close in Stop()
	}
	s.mu.Unlock()

	// close() on a channel signals all listeners that this channel is done.
	// Any goroutine waiting on <-s.done will immediately unblock.
	close(s.done)
}

// sanitizeUTF8 ensures the string is valid UTF-8, replacing invalid bytes
// with the Unicode replacement character (U+FFFD, displayed as �).
//
// This is important because:
// 1. JSON requires valid UTF-8, and we'll send output over WebSocket as JSON
// 2. Invalid bytes could cause display issues or security problems
func sanitizeUTF8(s string) string {
	// Fast path: if it's already valid, just return it
	if utf8.ValidString(s) {
		return s
	}

	// Slow path: decode rune by rune, replacing invalid sequences
	// A "rune" in Go is a Unicode code point (like a character).
	result := make([]rune, 0, len(s))
	for len(s) > 0 {
		// DecodeRuneInString returns the first rune and its byte length.
		// If the bytes are invalid, it returns RuneError (�) and size 1.
		r, size := utf8.DecodeRuneInString(s)
		result = append(result, r)
		s = s[size:] // Move past the bytes we just decoded
	}
	return string(result)
}

// Write sends input to the PTY (and thus to the running command).
// For example, you could send keystrokes or commands this way.
func (s *Session) Write(p []byte) (int, error) {
	// Get ptmx under lock to ensure it's not being closed concurrently
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()

	if ptmx == nil {
		return 0, fmt.Errorf("session not started")
	}
	return ptmx.Write(p)
}

// Resize changes the terminal dimensions of the PTY.
//
// This sends a SIGWINCH signal to the running process, which tells it
// to re-query its terminal size. TUI applications (like Claude, vim, htop)
// will redraw themselves to fit the new dimensions.
//
// Cols and rows must be greater than 0. Returns an error if the session
// is not running or the resize fails.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid dimensions: cols=%d, rows=%d", cols, rows)
	}

	// Hold lock during entire Setsize operation to prevent Stop() from
	// closing the FD while we're using it (race condition fix).
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return fmt.Errorf("session not running")
	}

	if s.ptmx == nil {
		return fmt.Errorf("session not started")
	}

	// Setsize changes the terminal size associated with the PTY.
	// This triggers SIGWINCH to be sent to the foreground process group,
	// which causes TUI applications to redraw themselves.
	size := &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	}
	if err := pty.Setsize(s.ptmx, size); err != nil {
		return fmt.Errorf("resize failed: %w", err)
	}

	return nil
}

// Lines returns all captured lines from the ring buffer.
// This is useful for replaying terminal history when a client reconnects.
func (s *Session) Lines() []string {
	return s.buffer.Lines()
}

// Buffer returns the underlying ring buffer.
// This gives direct access for advanced use cases.
func (s *Session) Buffer() *RingBuffer {
	return s.buffer
}

// PrependToBuffer adds lines to the session's ring buffer.
// This is used to inject scrollback history before the session starts producing output.
//
// Lines are expected to be newline-terminated (or the raw output from tmux capture-pane).
// The method splits the content by newlines and adds each line to the buffer.
// Legitimate blank lines in the middle of content are preserved; only the trailing
// empty element from Split (artifact of trailing newline) is skipped.
func (s *Session) PrependToBuffer(content string) {
	if content == "" {
		return
	}

	// Split content into lines and add each to the buffer
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		// Skip only the last element if it's empty (trailing newline artifact)
		// Preserve legitimate blank lines in the middle of content
		if i == len(lines)-1 && line == "" {
			continue
		}
		// Add newline back since buffer stores complete lines
		s.buffer.Write(line + "\n")
	}
}

// Done returns a channel that is closed when the session exits.
// Use this to wait for the session to finish:
//
//	<-session.Done()  // Blocks until session exits
//
// Or in a select statement to handle multiple events:
//
//	select {
//	case <-session.Done():
//	    fmt.Println("Session ended")
//	case <-ctx.Done():
//	    fmt.Println("Context cancelled")
//	}
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// IsRunning returns true if the session is currently running.
func (s *Session) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// GetCommand returns the command being executed.
// Thread-safe: acquires the session lock.
func (s *Session) GetCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Command
}

// GetName returns the session's human-readable name.
// Thread-safe: acquires the session lock.
func (s *Session) GetName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Name
}

// SetName sets the session's human-readable name.
// Thread-safe: acquires the session lock.
func (s *Session) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Name = name
}

// GetArgs returns a copy of the command arguments.
// Thread-safe: acquires the session lock and returns a copy to prevent mutation.
func (s *Session) GetArgs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Return a copy to prevent callers from mutating the slice
	if s.Args == nil {
		return nil
	}
	args := make([]string, len(s.Args))
	copy(args, s.Args)
	return args
}

// GetCreatedAt returns when the session was started.
// Thread-safe: acquires the session lock.
func (s *Session) GetCreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CreatedAt
}

// GetTmuxSession returns the tmux session name if this is a tmux session.
// Returns empty string for regular PTY sessions.
// Thread-safe: acquires the session lock.
func (s *Session) GetTmuxSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TmuxSession
}

// IsTmuxSession returns true if this session is attached to a tmux session.
// Thread-safe: acquires the session lock.
func (s *Session) IsTmuxSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TmuxSession != ""
}

// Error returns any error that occurred during the session.
func (s *Session) Error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stop terminates the PTY session forcefully.
// This is used when we want to kill the command (e.g., user pressed Ctrl+C).
func (s *Session) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already stopped? Nothing to do.
	if !s.running {
		return nil
	}

	// Close the PTY master. This will cause reads to fail, which will
	// make captureOutput() exit. It also signals EOF to the command.
	if s.ptmx != nil {
		s.ptmx.Close()
		s.ptmx = nil
	}

	// Kill the process if it's still running. Kill() sends SIGKILL on Unix,
	// which immediately terminates the process (it cannot be caught or ignored).
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}

	return nil
}
