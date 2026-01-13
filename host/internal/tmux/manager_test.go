package tmux

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/pty"
)

// mockExecCommand creates a function that returns a mock exec.Cmd.
// The mock runs a helper process that outputs the given data or returns an error.
// This is a standard Go testing pattern for mocking exec.Command.
func mockExecCommand(output string, exitCode int) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		// Create a command that runs the test binary with a special flag
		// This causes TestHelperProcess to be run, which outputs our mock data
		cs := []string{"-test.run=TestHelperProcess", "--", name}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = []string{
			"GO_WANT_HELPER_PROCESS=1",
			"MOCK_OUTPUT=" + output,
			"MOCK_EXIT_CODE=" + string(rune('0'+exitCode)),
		}
		return cmd
	}
}

// mockExecCommandNotFound creates a function that returns an exec.Cmd for a non-existent binary.
// When Run() or CombinedOutput() is called, it will return an exec.ErrNotFound error.
func mockExecCommandNotFound() func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		// Return a command for a binary that doesn't exist
		// This will cause exec.ErrNotFound when trying to run it
		return exec.Command("/nonexistent/path/to/binary/that/does/not/exist")
	}
}

// TestHelperProcess is not a real test. It is used as a helper process
// by mockExecCommand to simulate tmux command output.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Output the mock data
	output := os.Getenv("MOCK_OUTPUT")
	_, _ = os.Stdout.WriteString(output)

	// Exit with the specified code
	exitCode := os.Getenv("MOCK_EXIT_CODE")
	if exitCode == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestListSessions_Success(t *testing.T) {
	// Mock output with two sessions
	// Format: name\twindows\tattached\tcreated_at
	now := time.Now().Unix()
	earlier := now - 3600 // 1 hour ago
	mockOutput := "main\t3\t1\t" + itoa(now) + "\n" +
		"dev\t2\t0\t" + itoa(earlier) + "\n"

	m := &Manager{
		execCommand: mockExecCommand(mockOutput, 0),
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// Check first session
	if sessions[0].Name != "main" {
		t.Errorf("expected name 'main', got '%s'", sessions[0].Name)
	}
	if sessions[0].Windows != 3 {
		t.Errorf("expected 3 windows, got %d", sessions[0].Windows)
	}
	if !sessions[0].Attached {
		t.Errorf("expected attached=true, got false")
	}
	if sessions[0].CreatedAt.Unix() != now {
		t.Errorf("expected createdAt %d, got %d", now, sessions[0].CreatedAt.Unix())
	}

	// Check second session
	if sessions[1].Name != "dev" {
		t.Errorf("expected name 'dev', got '%s'", sessions[1].Name)
	}
	if sessions[1].Windows != 2 {
		t.Errorf("expected 2 windows, got %d", sessions[1].Windows)
	}
	if sessions[1].Attached {
		t.Errorf("expected attached=false, got true")
	}
	if sessions[1].CreatedAt.Unix() != earlier {
		t.Errorf("expected createdAt %d, got %d", earlier, sessions[1].CreatedAt.Unix())
	}
}

func TestListSessions_EmptyOutput(t *testing.T) {
	// Server is running but no sessions (empty output)
	m := &Manager{
		execCommand: mockExecCommand("", 0),
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestListSessions_NoServer(t *testing.T) {
	// tmux server not running - should return empty list, not error
	m := &Manager{
		execCommand: mockExecCommand("no server running on /private/tmp/tmux-501/default", 1),
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("expected no error for 'no server running', got: %v", err)
	}

	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions for 'no server running', got %d", len(sessions))
	}
}

func TestListSessions_SessionWithColonInName(t *testing.T) {
	// Session name containing a colon (this is why we use tab delimiter)
	now := time.Now().Unix()
	mockOutput := "project:feature\t1\t0\t" + itoa(now) + "\n"

	m := &Manager{
		execCommand: mockExecCommand(mockOutput, 0),
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	if sessions[0].Name != "project:feature" {
		t.Errorf("expected name 'project:feature', got '%s'", sessions[0].Name)
	}
}

func TestListSessions_UnicodeSessionName(t *testing.T) {
	// Session name containing Unicode characters (emoji, non-ASCII)
	now := time.Now().Unix()
	mockOutput := "dev-日本語\t2\t1\t" + itoa(now) + "\n"

	m := &Manager{
		execCommand: mockExecCommand(mockOutput, 0),
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	if sessions[0].Name != "dev-日本語" {
		t.Errorf("expected name 'dev-日本語', got '%s'", sessions[0].Name)
	}
	if sessions[0].Windows != 2 {
		t.Errorf("expected 2 windows, got %d", sessions[0].Windows)
	}
	if !sessions[0].Attached {
		t.Errorf("expected attached=true, got false")
	}
}

func TestParseTmuxListOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantLen  int
		wantName string
	}{
		{
			name:     "single session",
			output:   "test\t1\t0\t1704067200\n",
			wantLen:  1,
			wantName: "test",
		},
		{
			name:     "multiple sessions",
			output:   "one\t1\t0\t1704067200\ntwo\t2\t1\t1704067200\n",
			wantLen:  2,
			wantName: "one",
		},
		{
			name:    "empty output",
			output:  "",
			wantLen: 0,
		},
		{
			name:    "whitespace only",
			output:  "  \n  \n",
			wantLen: 0,
		},
		{
			name:     "trailing newlines",
			output:   "test\t1\t0\t1704067200\n\n\n",
			wantLen:  1,
			wantName: "test",
		},
		{
			name:     "malformed line skipped",
			output:   "bad-line-no-tabs\ntest\t1\t0\t1704067200\n",
			wantLen:  1,
			wantName: "test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, err := parseTmuxListOutput(tt.output)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(sessions) != tt.wantLen {
				t.Errorf("expected %d sessions, got %d", tt.wantLen, len(sessions))
			}

			if tt.wantLen > 0 && sessions[0].Name != tt.wantName {
				t.Errorf("expected name '%s', got '%s'", tt.wantName, sessions[0].Name)
			}
		})
	}
}

func TestParseTmuxSessionLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr bool
	}{
		{
			name:    "valid line",
			line:    "session\t3\t1\t1704067200",
			wantErr: false,
		},
		{
			name:    "too few parts",
			line:    "session\t3\t1",
			wantErr: true,
		},
		{
			name:    "too many parts",
			line:    "session\t3\t1\t1704067200\textra",
			wantErr: true,
		},
		{
			name:    "invalid window count",
			line:    "session\tnotanumber\t1\t1704067200",
			wantErr: true,
		},
		{
			name:    "invalid timestamp",
			line:    "session\t3\t1\tnotanumber",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTmuxSessionLine(tt.line)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTmuxSessionLine() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsCommandNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "exec.ErrNotFound",
			err:  exec.ErrNotFound,
			want: true,
		},
		{
			name: "no such file or directory",
			err:  &exec.Error{Name: "tmux", Err: exec.ErrNotFound},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCommandNotFound(tt.err)
			if got != tt.want {
				t.Errorf("isCommandNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNoServerRunning(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{
			name:   "no server running message",
			output: "no server running on /private/tmp/tmux-501/default",
			err:    &exec.ExitError{},
			want:   true,
		},
		{
			name:   "error connecting message",
			output: "error connecting to /private/tmp/tmux-501/default",
			err:    &exec.ExitError{},
			want:   true,
		},
		{
			name:   "no sessions message",
			output: "no sessions",
			err:    &exec.ExitError{},
			want:   true,
		},
		{
			name:   "nil error",
			output: "no server running",
			err:    nil,
			want:   false,
		},
		{
			name:   "other error",
			output: "some other error",
			err:    &exec.ExitError{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNoServerRunning(tt.output, tt.err)
			if got != tt.want {
				t.Errorf("isNoServerRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTmuxErrorCodes(t *testing.T) {
	// Verify error constructors return correct codes
	t.Run("TmuxNotInstalled", func(t *testing.T) {
		err := errors.TmuxNotInstalled()
		if err.Code != errors.CodeTmuxNotInstalled {
			t.Errorf("expected code %s, got %s", errors.CodeTmuxNotInstalled, err.Code)
		}
	})

	t.Run("TmuxNoServer", func(t *testing.T) {
		err := errors.TmuxNoServer()
		if err.Code != errors.CodeTmuxNoServer {
			t.Errorf("expected code %s, got %s", errors.CodeTmuxNoServer, err.Code)
		}
	})

	t.Run("TmuxSessionNotFound", func(t *testing.T) {
		err := errors.TmuxSessionNotFound("test")
		if err.Code != errors.CodeTmuxSessionNotFound {
			t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, err.Code)
		}
	})

	t.Run("TmuxAttachFailed", func(t *testing.T) {
		cause := errors.New(errors.CodeUnknown, "cause")
		err := errors.TmuxAttachFailed("test", cause)
		if err.Code != errors.CodeTmuxAttachFailed {
			t.Errorf("expected code %s, got %s", errors.CodeTmuxAttachFailed, err.Code)
		}
	})

	t.Run("TmuxKillFailed", func(t *testing.T) {
		cause := errors.New(errors.CodeUnknown, "cause")
		err := errors.TmuxKillFailed("test", cause)
		if err.Code != errors.CodeTmuxKillFailed {
			t.Errorf("expected code %s, got %s", errors.CodeTmuxKillFailed, err.Code)
		}
	})
}

// itoa is a helper to convert int64 to string for test fixtures.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// =============================================================================
// Unit 12.3: AttachToTmux Tests
// =============================================================================

// TestAttachToTmux_EmptyName verifies error when session name is empty.
func TestAttachToTmux_EmptyName(t *testing.T) {
	m := NewManager()

	_, err := m.AttachToTmux("", pty.SessionConfig{})
	if err == nil {
		t.Fatal("expected error for empty session name")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, code)
	}
}

// TestAttachToTmux_SessionNotFound verifies error when tmux session doesn't exist.
func TestAttachToTmux_SessionNotFound(t *testing.T) {
	// Mock has-session returning "can't find session" error
	m := &Manager{
		execCommand: mockExecCommand("can't find session: nonexistent", 1),
	}

	_, err := m.AttachToTmux("nonexistent", pty.SessionConfig{})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, code)
	}
}

// TestAttachToTmux_NoServer verifies error when tmux server not running.
func TestAttachToTmux_NoServer(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("no server running on /private/tmp/tmux-501/default", 1),
	}

	_, err := m.AttachToTmux("main", pty.SessionConfig{})
	if err == nil {
		t.Fatal("expected error when no server running")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, code)
	}
}

// TestAttachToTmux_ErrorConnecting verifies error when can't connect to server.
func TestAttachToTmux_ErrorConnecting(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("error connecting to /private/tmp/tmux-501/default", 1),
	}

	_, err := m.AttachToTmux("main", pty.SessionConfig{})
	if err == nil {
		t.Fatal("expected error when can't connect to server")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, code)
	}
}

// TestAttachToTmux_UnknownError verifies handling of unknown errors from has-session.
func TestAttachToTmux_UnknownError(t *testing.T) {
	// Mock has-session returning some unknown error
	m := &Manager{
		execCommand: mockExecCommand("unknown error", 1),
	}

	_, err := m.AttachToTmux("main", pty.SessionConfig{})
	if err == nil {
		t.Fatal("expected error for unknown has-session error")
	}

	// Unknown errors should also be treated as session not found
	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s", errors.CodeTmuxSessionNotFound, code)
	}
}

// TestAttachToTmux_SpecialCharactersInName verifies session names with special chars are handled.
func TestAttachToTmux_SpecialCharactersInName(t *testing.T) {
	tests := []struct {
		name        string
		sessionName string
	}{
		{"colon in name", "project:feature"},
		{"unicode in name", "dev-日本語"},
		{"spaces in name", "my session"},
		{"dots in name", "v1.2.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock has-session succeeding for these names
			m := &Manager{
				execCommand: mockExecCommand("", 0),
			}

			_, err := m.AttachToTmux(tt.sessionName, pty.SessionConfig{})

			// In test environment, attach will fail (no real tmux)
			// but we verify has-session passed (not TmuxSessionNotFound)
			if err == nil {
				return // Somehow succeeded, that's fine
			}

			code := errors.GetCode(err)
			// Should be AttachFailed, not SessionNotFound
			// (proving the name was passed correctly to has-session)
			if code != errors.CodeTmuxAttachFailed {
				t.Errorf("expected code %s for name %q, got %s",
					errors.CodeTmuxAttachFailed, tt.sessionName, code)
			}
		})
	}
}

// TestAttachToTmux_ConfigPassthrough verifies session config is applied.
// Note: We can only test that TmuxSession field is set since full PTY start
// would require actual tmux. Error tests above verify has-session validation.
func TestAttachToTmux_ConfigPassthrough(t *testing.T) {
	// For this test we need has-session to succeed (exit 0),
	// but then the actual PTY start will fail because tmux isn't available.
	// We verify TmuxSession is set by catching the TmuxAttachFailed error.
	m := &Manager{
		execCommand: mockExecCommand("", 0), // has-session succeeds
	}

	sessionConfig := pty.SessionConfig{
		ID:           "test-session-id",
		Name:         "test-session-name",
		HistoryLines: 1000,
	}

	session, err := m.AttachToTmux("main", sessionConfig)

	// In test environment, tmux attach-session will fail
	// because we're not actually running in a tmux-capable environment
	if err != nil {
		// Expected: TmuxAttachFailed because PTY start of tmux attach fails
		code := errors.GetCode(err)
		if code != errors.CodeTmuxAttachFailed {
			t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxAttachFailed, code, err)
		}
		return // Test passes - we verified has-session check passed
	}

	// If somehow it succeeded (tmux actually available), verify the session
	if session.TmuxSession != "main" {
		t.Errorf("expected TmuxSession 'main', got %q", session.TmuxSession)
	}

	// Clean up
	session.Stop()
	<-session.Done()
}

// TestAttachToTmux_GeneratesUUID verifies a UUID is generated when no ID is provided.
// This is critical: without a UUID, multiple tmux attaches would fail with duplicate ID.
func TestAttachToTmux_GeneratesUUID(t *testing.T) {
	// Mock has-session to succeed
	m := &Manager{
		execCommand: mockExecCommand("", 0),
	}

	// Empty ID in config - should generate UUID
	sessionConfig := pty.SessionConfig{
		ID:           "", // Explicitly empty
		HistoryLines: 100,
	}

	session, err := m.AttachToTmux("main", sessionConfig)

	// In test environment, tmux attach-session will fail
	if err != nil {
		code := errors.GetCode(err)
		if code != errors.CodeTmuxAttachFailed {
			t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxAttachFailed, code, err)
		}
		// Can't verify ID in this case since session isn't returned
		return
	}

	// If somehow it succeeded, verify UUID was generated
	if session.ID == "" {
		t.Error("expected non-empty session ID (UUID should be generated)")
	}

	// UUID format: 8-4-4-4-12 = 36 characters with dashes
	if len(session.ID) != 36 {
		t.Errorf("expected UUID format (36 chars), got %d chars: %q", len(session.ID), session.ID)
	}

	// Verify it's actually a valid UUID pattern
	// UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	parts := strings.Split(session.ID, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 UUID parts separated by dashes, got %d: %q", len(parts), session.ID)
	}

	session.Stop()
	<-session.Done()
}

// TestAttachToTmux_PreservesProvidedID verifies provided ID is not overwritten.
func TestAttachToTmux_PreservesProvidedID(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("", 0),
	}

	customID := "my-custom-session-id"
	sessionConfig := pty.SessionConfig{
		ID: customID,
	}

	session, err := m.AttachToTmux("main", sessionConfig)

	if err != nil {
		code := errors.GetCode(err)
		if code != errors.CodeTmuxAttachFailed {
			t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxAttachFailed, code, err)
		}
		return
	}

	// Verify custom ID was preserved
	if session.ID != customID {
		t.Errorf("expected ID %q, got %q", customID, session.ID)
	}

	session.Stop()
	<-session.Done()
}

// TestKillSession_Success verifies successful tmux session killing.
func TestKillSession_Success(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("", 0), // Success, no output
	}

	err := m.KillSession("main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestKillSession_TmuxNotInstalled verifies error when tmux is not installed.
func TestKillSession_TmuxNotInstalled(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommandNotFound(),
	}

	err := m.KillSession("main")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxNotInstalled {
		t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxNotInstalled, code, err)
	}
}

// TestKillSession_SessionNotFound verifies error when session doesn't exist.
func TestKillSession_SessionNotFound(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"can't find session", "can't find session: nonexistent"},
		{"session not found", "session not found: nonexistent"},
		{"no server running", "no server running on /tmp/tmux-501/default"},
		{"error connecting", "error connecting to /tmp/tmux-501/default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{
				execCommand: mockExecCommand(tt.output, 1),
			}

			err := m.KillSession("nonexistent")
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			code := errors.GetCode(err)
			if code != errors.CodeTmuxSessionNotFound {
				t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxSessionNotFound, code, err)
			}
		})
	}
}

// TestKillSession_EmptyName verifies error for empty session name.
func TestKillSession_EmptyName(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("", 0),
	}

	err := m.KillSession("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxSessionNotFound {
		t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxSessionNotFound, code, err)
	}
}

// =============================================================================
// Unit 12.6: CaptureScrollback Tests
// =============================================================================

// TestCaptureScrollback_Success verifies successful scrollback capture.
func TestCaptureScrollback_Success(t *testing.T) {
	mockScrollback := "line1\nline2\nline3\n"
	m := &Manager{
		execCommand: mockExecCommand(mockScrollback, 0),
	}

	output, err := m.CaptureScrollback("main", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output != mockScrollback {
		t.Errorf("expected %q, got %q", mockScrollback, output)
	}
}

// TestCaptureScrollback_TmuxNotInstalled verifies error when tmux is not installed.
func TestCaptureScrollback_TmuxNotInstalled(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommandNotFound(),
	}

	_, err := m.CaptureScrollback("main", 100)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	code := errors.GetCode(err)
	if code != errors.CodeTmuxNotInstalled {
		t.Errorf("expected code %s, got %s: %v", errors.CodeTmuxNotInstalled, code, err)
	}
}

// TestCaptureScrollback_SessionNotFound verifies graceful handling when session doesn't exist.
func TestCaptureScrollback_SessionNotFound(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"can't find session", "can't find session: nonexistent"},
		{"no server running", "no server running on /tmp/tmux-501/default"},
		{"error connecting", "error connecting to /tmp/tmux-501/default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{
				execCommand: mockExecCommand(tt.output, 1),
			}

			output, err := m.CaptureScrollback("nonexistent", 100)
			if err != nil {
				t.Fatalf("expected no error (graceful degradation), got: %v", err)
			}

			if output != "" {
				t.Errorf("expected empty output, got %q", output)
			}
		})
	}
}

// TestCaptureScrollback_EmptyName verifies graceful handling for empty session name.
func TestCaptureScrollback_EmptyName(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("should not be called", 0),
	}

	output, err := m.CaptureScrollback("", 100)
	if err != nil {
		t.Fatalf("expected no error for empty name, got: %v", err)
	}

	if output != "" {
		t.Errorf("expected empty output, got %q", output)
	}
}

// TestCaptureScrollback_DefaultLines verifies default lines when 0 is passed.
func TestCaptureScrollback_DefaultLines(t *testing.T) {
	mockScrollback := "scrollback content\n"
	var capturedArgs []string

	m := &Manager{
		execCommand: func(name string, args ...string) *exec.Cmd {
			capturedArgs = args
			return mockExecCommand(mockScrollback, 0)(name, args...)
		},
	}

	_, err := m.CaptureScrollback("main", 0) // 0 should use default
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the -S argument and check its value
	for i, arg := range capturedArgs {
		if arg == "-S" && i+1 < len(capturedArgs) {
			expected := "-1000" // DefaultScrollbackLines
			if capturedArgs[i+1] != expected {
				t.Errorf("expected -S %s, got -S %s", expected, capturedArgs[i+1])
			}
			return
		}
	}
	t.Error("expected -S argument in tmux command")
}

// TestCaptureScrollback_CustomLines verifies custom line count is used.
func TestCaptureScrollback_CustomLines(t *testing.T) {
	mockScrollback := "scrollback content\n"
	var capturedArgs []string

	m := &Manager{
		execCommand: func(name string, args ...string) *exec.Cmd {
			capturedArgs = args
			return mockExecCommand(mockScrollback, 0)(name, args...)
		},
	}

	_, err := m.CaptureScrollback("main", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the -S argument and check its value
	for i, arg := range capturedArgs {
		if arg == "-S" && i+1 < len(capturedArgs) {
			expected := "-500"
			if capturedArgs[i+1] != expected {
				t.Errorf("expected -S %s, got -S %s", expected, capturedArgs[i+1])
			}
			return
		}
	}
	t.Error("expected -S argument in tmux command")
}

// TestCaptureScrollback_EmptyOutput verifies handling of empty scrollback.
func TestCaptureScrollback_EmptyOutput(t *testing.T) {
	m := &Manager{
		execCommand: mockExecCommand("", 0),
	}

	output, err := m.CaptureScrollback("main", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output != "" {
		t.Errorf("expected empty output, got %q", output)
	}
}
