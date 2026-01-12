// Package tmux provides integration with the tmux terminal multiplexer.
//
// It allows listing, attaching to, and managing existing tmux sessions
// from the pseudocoder host. This enables mobile clients to connect to
// tmux sessions running on the Mac for seamless workflow continuity.
//
// The package uses a PTY wrapper approach: when attaching to a tmux session,
// we spawn `tmux attach-session -t <name>` inside a pseudocoder PTY session.
// This reuses existing PTY infrastructure and works with multiple clients
// (tmux handles concurrent attachments natively).
//
// See ADR 0030 for architectural decisions.
package tmux

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/pty"
)

// DefaultScrollbackLines is the default number of lines to capture from tmux scrollback.
const DefaultScrollbackLines = 1000

// TmuxSessionInfo contains metadata about a tmux session.
// This information is retrieved via `tmux list-sessions` and sent
// to mobile clients to help them discover available sessions.
type TmuxSessionInfo struct {
	// Name is the tmux session name (e.g., "main", "dev", "0").
	Name string `json:"name"`

	// Windows is the number of windows in this session.
	Windows int `json:"windows"`

	// Attached indicates whether another client is currently attached.
	// When true, the mobile client can still attach (tmux supports multiple clients).
	Attached bool `json:"attached"`

	// CreatedAt is when this tmux session was created.
	CreatedAt time.Time `json:"created_at"`
}

// Manager handles interaction with tmux.
// It provides methods to list available sessions and will later support
// attaching to sessions and capturing scrollback.
type Manager struct {
	// execCommand is a function that creates exec.Cmd instances.
	// This allows tests to inject mock command execution.
	// In production, this is exec.Command.
	execCommand func(name string, arg ...string) *exec.Cmd
}

// NewManager creates a new tmux Manager using the real exec.Command.
func NewManager() *Manager {
	return &Manager{
		execCommand: exec.Command,
	}
}

// ListSessions returns all available tmux sessions.
//
// It runs `tmux list-sessions -F` with a tab-delimited format to retrieve
// session metadata. The tab delimiter avoids parsing issues with session
// names that contain colons (the default tmux delimiter).
//
// Error handling:
//   - If tmux is not installed, returns errors.TmuxNotInstalled().
//   - If no tmux server is running (no sessions), returns an empty slice
//     with nil error. This is a UX decision: "no sessions" is a normal state,
//     not an error condition.
//   - Parse errors for individual sessions are logged but don't fail the
//     entire operation - malformed lines are skipped.
func (m *Manager) ListSessions() ([]TmuxSessionInfo, error) {
	// Format string for tmux list-sessions:
	// - #{session_name}: The session name
	// - #{session_windows}: Number of windows in the session
	// - #{session_attached}: 1 if attached, 0 if not
	// - #{session_created}: Unix timestamp when session was created
	//
	// We use tab (\t) as delimiter because session names can contain colons.
	format := "#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}"

	cmd := m.execCommand("tmux", "list-sessions", "-F", format)
	output, err := cmd.CombinedOutput()

	if err != nil {
		// Check if tmux is not installed
		if isCommandNotFound(err) {
			return nil, errors.TmuxNotInstalled()
		}

		// Check if no tmux server is running (exit code 1 with specific message)
		// tmux returns exit code 1 with "no server running" or similar message
		outputStr := string(output)
		if isNoServerRunning(outputStr, err) {
			// Return empty list, not an error - this is a normal state
			return []TmuxSessionInfo{}, nil
		}

		// Unknown error
		return nil, errors.Wrap(errors.CodeInternal, "failed to list tmux sessions", err)
	}

	// Parse the output
	return parseTmuxListOutput(string(output))
}

// AttachToTmux creates a PTY session attached to an existing tmux session.
//
// It first verifies the tmux session exists using `tmux has-session -t <name>`,
// then creates a PTY running `tmux attach-session -t <name>`.
//
// The returned session is already started and ready for I/O. The caller is
// responsible for registering it with SessionManager if needed.
//
// Parameters:
//   - name: The tmux session name to attach to
//   - sessionConfig: Configuration for the PTY session (ID, callbacks, etc.)
//
// Error handling:
//   - If tmux is not installed, returns errors.TmuxNotInstalled()
//   - If the session doesn't exist, returns errors.TmuxSessionNotFound(name)
//   - If attach fails, returns errors.TmuxAttachFailed(name, cause)
func (m *Manager) AttachToTmux(name string, sessionConfig pty.SessionConfig) (*pty.Session, error) {
	// Validate session name
	if name == "" {
		return nil, errors.TmuxSessionNotFound("")
	}

	// Step 1: Verify session exists using `tmux has-session -t <name>`
	cmd := m.execCommand("tmux", "has-session", "-t", name)
	output, err := cmd.CombinedOutput()

	if err != nil {
		// Check if tmux is not installed
		if isCommandNotFound(err) {
			return nil, errors.TmuxNotInstalled()
		}

		// Check if session doesn't exist or no server running
		// tmux has-session returns exit code 1 if session not found
		outputStr := strings.ToLower(string(output))
		if strings.Contains(outputStr, "can't find session") ||
			strings.Contains(outputStr, "session not found") ||
			strings.Contains(outputStr, "no server running") ||
			strings.Contains(outputStr, "error connecting to") {
			return nil, errors.TmuxSessionNotFound(name)
		}

		// Unknown error during session check - treat as session not found
		return nil, errors.TmuxSessionNotFound(name)
	}

	// Step 2: Create PTY session with tmux attach command
	// Generate a UUID if no ID was provided in the config
	if sessionConfig.ID == "" {
		sessionConfig.ID = uuid.New().String()
	}
	session := pty.NewSession(sessionConfig)
	session.TmuxSession = name // Mark as tmux session

	// Step 3: Start the PTY with tmux attach-session command
	if err := session.Start("tmux", "attach-session", "-t", name); err != nil {
		cause := errors.Wrap(errors.CodeInternal, "PTY start failed", err)
		return nil, errors.TmuxAttachFailed(name, cause)
	}

	return session, nil
}

// KillSession terminates a tmux session.
// It runs `tmux kill-session -t <name>` to destroy the session.
//
// Error handling:
//   - If tmux is not installed, returns errors.TmuxNotInstalled()
//   - If the session doesn't exist, returns errors.TmuxSessionNotFound(name)
//   - Other errors return wrapped internal errors
func (m *Manager) KillSession(name string) error {
	if name == "" {
		return errors.TmuxSessionNotFound("")
	}

	cmd := m.execCommand("tmux", "kill-session", "-t", name)
	output, err := cmd.CombinedOutput()

	if err != nil {
		if isCommandNotFound(err) {
			return errors.TmuxNotInstalled()
		}

		outputStr := strings.ToLower(string(output))
		if strings.Contains(outputStr, "can't find session") ||
			strings.Contains(outputStr, "session not found") ||
			strings.Contains(outputStr, "no server running") ||
			strings.Contains(outputStr, "error connecting to") {
			return errors.TmuxSessionNotFound(name)
		}

		return errors.Wrap(errors.CodeInternal, "failed to kill tmux session", err)
	}

	return nil
}

// CaptureScrollback retrieves scrollback history from a tmux session.
// It runs `tmux capture-pane -t <session> -p -S -<lines>` to capture output.
//
// Parameters:
//   - name: The tmux session name to capture from
//   - lines: Number of lines to capture (use DefaultScrollbackLines if 0)
//
// Returns the captured output as a string (may contain multiple lines with \n).
// Returns empty string if capture fails (e.g., session not found, no scrollback).
//
// Error handling:
//   - If tmux is not installed, returns errors.TmuxNotInstalled()
//   - If the session doesn't exist, returns empty string (not an error - graceful)
//   - Parse/exec errors return empty string (scrollback is optional)
func (m *Manager) CaptureScrollback(name string, lines int) (string, error) {
	if name == "" {
		return "", nil
	}

	if lines <= 0 {
		lines = DefaultScrollbackLines
	}

	// -p: print to stdout instead of to a paste buffer
	// -S: start line (negative = from scrollback, -1000 = last 1000 lines)
	// -t: target session
	cmd := m.execCommand("tmux", "capture-pane", "-t", name, "-p", "-S", fmt.Sprintf("-%d", lines))
	output, err := cmd.CombinedOutput()

	if err != nil {
		if isCommandNotFound(err) {
			return "", errors.TmuxNotInstalled()
		}
		// Session not found or no server - return empty (graceful degradation)
		return "", nil
	}

	return string(output), nil
}

// parseTmuxListOutput parses the tab-delimited output from tmux list-sessions.
// Each line contains: session_name\twindows\tattached\tcreated_at
func parseTmuxListOutput(output string) ([]TmuxSessionInfo, error) {
	var sessions []TmuxSessionInfo

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		info, err := parseTmuxSessionLine(line)
		if err != nil {
			// Skip malformed lines rather than failing entirely
			// This is more robust if tmux output format varies slightly
			continue
		}
		sessions = append(sessions, info)
	}

	if err := scanner.Err(); err != nil {
		return nil, errors.Wrap(errors.CodeInternal, "error reading tmux output", err)
	}

	return sessions, nil
}

// parseTmuxSessionLine parses a single line of tmux list-sessions output.
// Expected format: session_name\twindows\tattached\tcreated_at
func parseTmuxSessionLine(line string) (TmuxSessionInfo, error) {
	parts := strings.Split(line, "\t")
	if len(parts) != 4 {
		return TmuxSessionInfo{}, errors.New(errors.CodeInternal, "invalid tmux session line format")
	}

	name := parts[0]

	windows, err := strconv.Atoi(parts[1])
	if err != nil {
		return TmuxSessionInfo{}, errors.Wrap(errors.CodeInternal, "invalid window count", err)
	}

	// tmux outputs 1 for attached, 0 for not attached
	attached := parts[2] == "1"

	createdAt, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return TmuxSessionInfo{}, errors.Wrap(errors.CodeInternal, "invalid created_at timestamp", err)
	}

	return TmuxSessionInfo{
		Name:      name,
		Windows:   windows,
		Attached:  attached,
		CreatedAt: time.Unix(createdAt, 0),
	}, nil
}

// isCommandNotFound checks if the error indicates the command was not found.
// This typically happens when tmux is not installed.
func isCommandNotFound(err error) bool {
	if err == nil {
		return false
	}

	// Check for exec.ErrNotFound (Go 1.19+)
	if err == exec.ErrNotFound {
		return true
	}

	// Check the error message for common "not found" patterns
	errMsg := err.Error()
	return strings.Contains(errMsg, "executable file not found") ||
		strings.Contains(errMsg, "no such file or directory")
}

// isNoServerRunning checks if the tmux output indicates no server is running.
// When there are no tmux sessions, tmux exits with code 1 and prints a message.
func isNoServerRunning(output string, err error) bool {
	if err == nil {
		return false
	}

	// Check for the common "no server running" message
	// tmux may output slightly different messages depending on version:
	// - "no server running on ..."
	// - "error connecting to ..."
	// - "no current session"
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting to") ||
		strings.Contains(lower, "no sessions")
}
