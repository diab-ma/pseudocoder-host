package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunSession_MissingSubcommand tests that session without subcommand shows usage.
func TestRunSession_MissingSubcommand(t *testing.T) {
	code, out, _ := runWithArgs([]string{"pseudocoder", "session"})
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(out, "Usage: pseudocoder session") {
		t.Fatalf("expected session usage, got %q", out)
	}
}

// TestRunSession_UnknownSubcommand tests that unknown subcommand is rejected.
func TestRunSession_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSession([]string{"nope"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Unknown session command") {
		t.Fatalf("expected unknown command message, got %q", stdout.String())
	}
}

// TestSessionNewHelp tests that session new --help shows usage.
func TestSessionNewHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionNew([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session new") {
		t.Fatalf("expected session new usage, got %q", stderr.String())
	}
}

// TestSessionListHelp tests that session list --help shows usage.
func TestSessionListHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionList([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session list") {
		t.Fatalf("expected session list usage, got %q", stderr.String())
	}
}

// TestSessionKillHelp tests that session kill --help shows usage.
func TestSessionKillHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionKill([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session kill") {
		t.Fatalf("expected session kill usage, got %q", stderr.String())
	}
}

// TestSessionRenameHelp tests that session rename --help shows usage.
func TestSessionRenameHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionRename([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session rename") {
		t.Fatalf("expected session rename usage, got %q", stderr.String())
	}
}

// TestSessionKillMissingID tests that session kill without ID shows error.
func TestSessionKillMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionKill([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "session-id is required") {
		t.Fatalf("expected session-id error, got %q", stderr.String())
	}
}

// TestSessionRenameMissingArgs tests that session rename without args shows error.
func TestSessionRenameMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionRename([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "session-id and new-name are required") {
		t.Fatalf("expected args error, got %q", stderr.String())
	}
}

// TestSessionRenameMissingName tests that session rename with only ID shows error.
func TestSessionRenameMissingName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionRename([]string{"session-id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "session-id and new-name are required") {
		t.Fatalf("expected args error, got %q", stderr.String())
	}
}

// TestSessionNewNoHost tests that session new fails gracefully when host is not running.
func TestSessionNewNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Use a port that's unlikely to have a host running
	code := runSessionNew([]string{"--addr", "127.0.0.1:19999"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionListNoHost tests that session list fails gracefully when host is not running.
func TestSessionListNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionList([]string{"--addr", "127.0.0.1:19999"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionKillNoHost tests that session kill fails gracefully when host is not running.
func TestSessionKillNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionKill([]string{"--addr", "127.0.0.1:19999", "some-id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionRenameNoHost tests that session rename fails gracefully when host is not running.
func TestSessionRenameNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionRename([]string{"--addr", "127.0.0.1:19999", "some-id", "new-name"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// === Unit 12.9: tmux CLI command tests ===

// TestSessionListTmuxHelp tests that session list-tmux --help shows usage.
func TestSessionListTmuxHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionListTmux([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session list-tmux") {
		t.Fatalf("expected session list-tmux usage, got %q", stderr.String())
	}
}

// TestSessionAttachTmuxHelp tests that session attach-tmux --help shows usage.
func TestSessionAttachTmuxHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionAttachTmux([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session attach-tmux") {
		t.Fatalf("expected session attach-tmux usage, got %q", stderr.String())
	}
}

// TestSessionDetachHelp tests that session detach --help shows usage.
func TestSessionDetachHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionDetach([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder session detach") {
		t.Fatalf("expected session detach usage, got %q", stderr.String())
	}
}

// TestSessionAttachTmuxMissingSession tests that attach-tmux without session name shows error.
func TestSessionAttachTmuxMissingSession(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionAttachTmux([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "tmux-session name is required") {
		t.Fatalf("expected tmux-session error, got %q", stderr.String())
	}
}

// TestSessionDetachMissingID tests that detach without session ID shows error.
func TestSessionDetachMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionDetach([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "session-id is required") {
		t.Fatalf("expected session-id error, got %q", stderr.String())
	}
}

// TestSessionListTmuxNoHost tests that list-tmux fails gracefully when host is not running.
func TestSessionListTmuxNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionListTmux([]string{"--addr", "127.0.0.1:19999"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionAttachTmuxNoHost tests that attach-tmux fails gracefully when host is not running.
func TestSessionAttachTmuxNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionAttachTmux([]string{"--addr", "127.0.0.1:19999", "my-session"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionDetachNoHost tests that detach fails gracefully when host is not running.
func TestSessionDetachNoHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionDetach([]string{"--addr", "127.0.0.1:19999", "some-id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

// TestSessionDetachWithKillFlag tests that --kill flag is accepted.
func TestSessionDetachWithKillFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// This will fail because no host, but we're just testing flag parsing
	code := runSessionDetach([]string{"--addr", "127.0.0.1:19999", "--kill", "some-id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	// Should fail with connection error, not flag error
	if !strings.Contains(stderr.String(), "Error:") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
	// Should NOT contain flag parsing error
	if strings.Contains(stderr.String(), "flag") {
		t.Fatalf("unexpected flag error, got %q", stderr.String())
	}
}

// TestSessionUsageIncludesTmuxCommands tests that session usage shows tmux commands.
func TestSessionUsageIncludesTmuxCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSession([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	output := stdout.String()

	// Verify all tmux commands are listed
	if !strings.Contains(output, "list-tmux") {
		t.Errorf("usage should include list-tmux, got %q", output)
	}
	if !strings.Contains(output, "attach-tmux") {
		t.Errorf("usage should include attach-tmux, got %q", output)
	}
	if !strings.Contains(output, "detach") {
		t.Errorf("usage should include detach, got %q", output)
	}
}
