package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pseudocoder/host/internal/config"
)

// TestRunStart_Help verifies that --help returns usage information.
func TestRunStart_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStart([]string{"--help"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("runStart(--help) = %d, want 0", code)
	}

	// Help should be in stderr (flag package default)
	output := stderr.String()
	if !strings.Contains(output, "Usage: pseudocoder start") {
		t.Errorf("Help output missing usage line, got: %s", output)
	}
	if !strings.Contains(output, "-repo") {
		t.Errorf("Help output missing -repo flag, got: %s", output)
	}
	if !strings.Contains(output, "-addr") {
		t.Errorf("Help output missing -addr flag, got: %s", output)
	}
	if !strings.Contains(output, "-pair") {
		t.Errorf("Help output missing -pair flag, got: %s", output)
	}
	if !strings.Contains(output, "-qr") {
		t.Errorf("Help output missing -qr flag, got: %s", output)
	}
	if !strings.Contains(output, "-port") {
		t.Errorf("Help output missing -port flag, got: %s", output)
	}
	if !strings.Contains(output, "-pair-socket") {
		t.Errorf("Help output missing -pair-socket flag, got: %s", output)
	}
}

// TestRunStart_InvalidFlag verifies that invalid flags return an error.
func TestRunStart_InvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStart([]string{"--invalid-flag"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("runStart(--invalid-flag) = %d, want 1", code)
	}
}

func TestRunStart_InvalidPort(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStart([]string{"--port", "0"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("runStart(--port 0) = %d, want 1", code)
	}
}

// TestWriteDefaultIntegration verifies that WriteDefault creates proper mobile-ready config.
// This supplements config_test.go by verifying the exact defaults used by runStart.
func TestWriteDefaultIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	repoPath := "/test/repo"

	err := config.WriteDefault(configPath, repoPath)
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify mobile-ready defaults match what runStart expects
	if !cfg.RequireAuth {
		t.Error("RequireAuth = false, want true (security default)")
	}
	if cfg.Repo != repoPath {
		t.Errorf("Repo = %q, want %q", cfg.Repo, repoPath)
	}
}

// TestDefaultConfigPath verifies config path used by runStart.
func TestDefaultConfigPath(t *testing.T) {
	path, err := config.DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error: %v", err)
	}

	// Should end with .pseudocoder/config.toml
	if !strings.HasSuffix(path, ".pseudocoder/config.toml") {
		t.Errorf("DefaultConfigPath() = %q, want suffix .pseudocoder/config.toml", path)
	}
}

// TestWriteDefaultNoOverwrite verifies existing config is preserved.
// This is critical for runStart's "don't overwrite" behavior.
func TestWriteDefaultNoOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Create existing config with different values
	existingContent := `addr = "127.0.0.1:9999"
require_auth = false
`
	if err := os.WriteFile(configPath, []byte(existingContent), 0600); err != nil {
		t.Fatalf("Failed to write existing config: %v", err)
	}

	// WriteDefault should not overwrite
	err := config.WriteDefault(configPath, "/new/repo")
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	// Verify original content preserved
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want %q (original should be preserved)", cfg.Addr, "127.0.0.1:9999")
	}
	if cfg.RequireAuth {
		t.Error("RequireAuth = true, want false (original should be preserved)")
	}
}

func TestSelectBindAddr_PrioritizesTailscale(t *testing.T) {
	originalTailscale := getTailscaleIP
	originalOutbound := getPreferredOutboundIP
	getTailscaleIP = func() string { return "100.64.0.5" }
	getPreferredOutboundIP = func() string { return "192.168.1.10" }
	t.Cleanup(func() {
		getTailscaleIP = originalTailscale
		getPreferredOutboundIP = originalOutbound
	})

	var stderr bytes.Buffer
	addr := selectBindAddr("", 7070, false, &stderr)

	if addr != "100.64.0.5:7070" {
		t.Errorf("selectBindAddr() = %q, want %q", addr, "100.64.0.5:7070")
	}
	if stderr.Len() != 0 {
		t.Errorf("selectBindAddr() unexpected warning: %s", stderr.String())
	}
}

func TestSelectBindAddr_FallsBackToLAN(t *testing.T) {
	originalTailscale := getTailscaleIP
	originalOutbound := getPreferredOutboundIP
	getTailscaleIP = func() string { return "" }
	getPreferredOutboundIP = func() string { return "192.168.1.10" }
	t.Cleanup(func() {
		getTailscaleIP = originalTailscale
		getPreferredOutboundIP = originalOutbound
	})

	var stderr bytes.Buffer
	addr := selectBindAddr("", 7071, false, &stderr)

	if addr != "192.168.1.10:7071" {
		t.Errorf("selectBindAddr() = %q, want %q", addr, "192.168.1.10:7071")
	}
	if stderr.Len() != 0 {
		t.Errorf("selectBindAddr() unexpected warning: %s", stderr.String())
	}
}

func TestSelectBindAddr_WarnsOnLocalhostFallback(t *testing.T) {
	originalTailscale := getTailscaleIP
	originalOutbound := getPreferredOutboundIP
	getTailscaleIP = func() string { return "" }
	getPreferredOutboundIP = func() string { return "" }
	t.Cleanup(func() {
		getTailscaleIP = originalTailscale
		getPreferredOutboundIP = originalOutbound
	})

	var stderr bytes.Buffer
	addr := selectBindAddr("", 7072, false, &stderr)

	if addr != "127.0.0.1:7072" {
		t.Errorf("selectBindAddr() = %q, want %q", addr, "127.0.0.1:7072")
	}
	if !strings.Contains(stderr.String(), "using localhost") {
		t.Errorf("selectBindAddr() missing warning, got: %s", stderr.String())
	}
}

func TestSelectBindAddr_AddrOverridesPort(t *testing.T) {
	var stderr bytes.Buffer
	addr := selectBindAddr("192.168.1.20:7073", 7074, true, &stderr)

	if addr != "192.168.1.20:7073" {
		t.Errorf("selectBindAddr() = %q, want %q", addr, "192.168.1.20:7073")
	}
	if !strings.Contains(stderr.String(), "overrides --port") {
		t.Errorf("selectBindAddr() missing override warning, got: %s", stderr.String())
	}
}

// Note: Full integration testing of runStart requires manual verification
// because runStart calls runHostStart which blocks. See docs/TESTING-ARCHIVE.md Phase 13
// for manual test procedures covering:
// - Config creation on first run
// - Existing config preservation
// - Connection summary banner output
// - Interaction with host start defaults
//
// Unit 18.1 additions (manual verification):
// - `pseudocoder start --pair` shows pairing code during startup
// - `pseudocoder start --pair --qr` shows QR code during startup
// - `pseudocoder start --qr` auto-enables --pair (QR implies pairing code)
// - Port extraction from --addr is used in display address (not hardcoded 7070)

// -----------------------------------------------------------------------
// P18U4 test helpers
// -----------------------------------------------------------------------

func stubIsTerminal(t *testing.T, isTTY bool) {
	orig := startIsTerminal
	t.Cleanup(func() { startIsTerminal = orig })
	startIsTerminal = func() bool { return isTTY }
}

func stubReadLine(t *testing.T, lines ...string) {
	orig := startReadLine
	t.Cleanup(func() { startReadLine = orig })
	idx := 0
	startReadLine = func() (string, error) {
		if idx >= len(lines) {
			return "", io.EOF
		}
		line := lines[idx]
		idx++
		return line, nil
	}
}

func stubPersistPolicy(t *testing.T, retErr error) *bool {
	orig := startPersistPolicy
	t.Cleanup(func() { startPersistPolicy = orig })
	called := false
	startPersistPolicy = func(path string, enabled bool) error {
		called = true
		return retErr
	}
	return &called
}

func stubHostDispatch(t *testing.T, exitCode int) *bool {
	orig := startHostDispatch
	t.Cleanup(func() { startHostDispatch = orig })
	called := false
	startHostDispatch = func(args []string, stdout, stderr io.Writer) int {
		called = true
		return exitCode
	}
	return &called
}

func stubConfigPath(t *testing.T, path string) {
	orig := startConfigPath
	t.Cleanup(func() { startConfigPath = orig })
	startConfigPath = func() (string, error) { return path, nil }
}

// stubNetwork stubs network detection to avoid real lookups in tests.
func stubNetwork(t *testing.T) {
	origTS := getTailscaleIP
	origOB := getPreferredOutboundIP
	t.Cleanup(func() {
		getTailscaleIP = origTS
		getPreferredOutboundIP = origOB
	})
	getTailscaleIP = func() string { return "" }
	getPreferredOutboundIP = func() string { return "192.168.1.100" }
}

// writeConfig writes a TOML config to a temp dir and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	return p
}

// runStartTest runs runStart with seams stubbed and returns exit code + output.
func runStartTest(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runStart(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// -----------------------------------------------------------------------
// P18U4 unit tests
// -----------------------------------------------------------------------

// AC1: Help includes both new flags.
func TestRunStart_HelpIncludesKeepAwakeFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStart([]string{"--help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	output := stderr.String()
	if !strings.Contains(output, "enable-remote-keep-awake") {
		t.Error("help missing -enable-remote-keep-awake flag")
	}
	if !strings.Contains(output, "no-prompt") {
		t.Error("help missing -no-prompt flag")
	}
}

// AC2: Prompt shown only when TTY + policy disabled + no flags.
func TestRunStart_PromptShownOnlyWhenTTYAndPolicyDisabled(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "n")
	stubNetwork(t)
	stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt not shown when expected (TTY + disabled + no flags)")
	}
}

// AC2 negative: prompt suppressed when policy already enabled.
func TestRunStart_PromptSuppressedWhenPolicyAlreadyEnabled(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = true
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown when policy already enabled")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy: enabled.") {
		t.Error("expected enabled status line")
	}
}

// AC3: Enable flag enables without prompt.
func TestRunStart_EnableRemoteKeepAwakeFlagForcesEnableWithoutPrompt(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, []string{"--enable-remote-keep-awake"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown despite --enable-remote-keep-awake flag")
	}
	if !*persistCalled {
		t.Error("persist not called with --enable-remote-keep-awake")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy enabled.") {
		t.Error("expected enabled confirmation")
	}
}

// AC4: --no-prompt suppresses and preserves.
func TestRunStart_NoPromptSuppressesPromptAndPreservesPolicy(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, []string{"--no-prompt"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown despite --no-prompt")
	}
	if *persistCalled {
		t.Error("persist called despite --no-prompt (should preserve)")
	}
	if !strings.Contains(stdout, "Prompt suppressed") {
		t.Error("expected suppression message")
	}
}

// AC5: Enable flag wins over no-prompt (TTY).
func TestRunStart_EnableFlagPrecedenceOverNoPromptTTY(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, []string{"--enable-remote-keep-awake", "--no-prompt"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !*persistCalled {
		t.Error("persist not called: enable flag should win over no-prompt")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy enabled.") {
		t.Error("expected enabled confirmation when enable flag wins")
	}
}

// AC5/AC16: Enable flag wins over no-prompt (non-TTY).
func TestRunStart_EnableFlagPrecedenceOverNoPromptNonTTY(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, []string{"--enable-remote-keep-awake", "--no-prompt"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !*persistCalled {
		t.Error("persist not called: enable flag should win over no-prompt in non-TTY")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy enabled.") {
		t.Error("expected enabled confirmation")
	}
}

// AC6: Empty input defaults to No.
func TestRunStart_PromptEmptyDefaultsNo(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "")
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called on empty input (should default to No)")
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Error("expected unchanged message on empty input")
	}
}

// AC6: EOF defaults to No.
func TestRunStart_PromptEOFDefaultsNo(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t) // no lines => EOF on first read
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, _, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called on EOF (should default to No)")
	}
}

// AC7: Invalid input then yes on retry.
func TestRunStart_PromptInvalidThenYes(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "maybe", "y")
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !*persistCalled {
		t.Error("persist not called after invalid-then-yes")
	}
	if !strings.Contains(stdout, "Unrecognized input. Please enter") {
		t.Error("expected retry prompt on first invalid input")
	}
}

// AC7: Two invalid inputs default to No.
func TestRunStart_PromptInvalidTwiceDefaultsNo(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "maybe", "dunno")
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called after two invalid inputs")
	}
	if !strings.Contains(stdout, "Unrecognized input. Defaulting to No.") {
		t.Error("expected final defaulting message after two invalid inputs")
	}
}

// AC8: Non-TTY, no flags skips prompt and preserves policy.
func TestRunStart_NonTTYNoFlagsSkipsPromptPreservesPolicy(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown in non-TTY mode")
	}
	if *persistCalled {
		t.Error("persist called in non-TTY no-flag mode")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy: disabled.") {
		t.Error("expected disabled status line in non-TTY")
	}
}

// AC9: Enable path persist failure exits non-zero.
func TestRunStart_EnablePathPersistFailureFailsClosed(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	stubPersistPolicy(t, fmt.Errorf("disk full"))
	stubHostDispatch(t, 0)

	code, _, stderr := runStartTest(t, []string{"--enable-remote-keep-awake"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on persist failure", code)
	}
	if !strings.Contains(stderr, "failed to persist") {
		t.Error("expected persist failure message in stderr")
	}
}

// AC10: Existing flags pass through unchanged.
func TestRunStart_PreservesExistingStartFlagPassThrough(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = true
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	stubPersistPolicy(t, nil)

	var capturedArgs []string
	orig := startHostDispatch
	t.Cleanup(func() { startHostDispatch = orig })
	startHostDispatch = func(args []string, stdout, stderr io.Writer) int {
		capturedArgs = args
		return 0
	}

	code, _, _ := runStartTest(t, []string{"--repo", "/my/repo", "--mdns", "--pair", "--qr", "--pair-socket", "/tmp/p.sock"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{"--require-auth", "--repo /my/repo", "--mdns", "--pair", "--qr", "--pair-socket /tmp/p.sock"} {
		if !strings.Contains(joined, want) {
			t.Errorf("host args missing %q, got: %s", want, joined)
		}
	}
}

// AC12: Malformed config fails closed before prompt.
func TestRunStart_MalformedConfigFailsClosedBeforePrompt(t *testing.T) {
	cfgPath := writeConfig(t, `this is not valid TOML [[[`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	dispatchCalled := stubHostDispatch(t, 0)

	code, _, stderr := runStartTest(t, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on malformed config", code)
	}
	if !strings.Contains(stderr, "failed to parse config") {
		t.Error("expected parse error message")
	}
	if *dispatchCalled {
		t.Error("host dispatch called despite malformed config")
	}
}

// AC13: Unset policy is prompt-eligible.
func TestRunStart_PromptShownWhenPolicyUnset(t *testing.T) {
	// Config with no keep_awake_remote_enabled key at all.
	cfgPath := writeConfig(t, `require_auth = true
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "n")
	stubNetwork(t)
	stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt not shown when policy key is unset (should be prompt-eligible)")
	}
}

// AC14: Persist failure blocks dispatch.
func TestRunStart_EnablePathPersistFailureDoesNotDispatchHostStart(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	stubPersistPolicy(t, fmt.Errorf("permission denied"))
	dispatchCalled := stubHostDispatch(t, 0)

	code, _, _ := runStartTest(t, []string{"--enable-remote-keep-awake"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if *dispatchCalled {
		t.Error("host dispatch called despite persist failure")
	}
}

// AC15: EOF message is deterministic.
func TestRunStart_PromptEOFDefaultsNoMessageDeterministic(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t) // EOF
	stubNetwork(t)
	stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "No input received. Defaulting to No.") {
		t.Errorf("expected deterministic EOF message, got stdout:\n%s", stdout)
	}
}

// AC17: First-run bootstrap + enable flag persists policy.
func TestRunStart_FirstRunBootstrapEnableFlagPersistsPolicy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Config does not exist yet — first run.
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, []string{"--enable-remote-keep-awake"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Created config") {
		t.Error("expected config creation message on first run")
	}
	if !*persistCalled {
		t.Error("persist not called on first-run + enable flag")
	}
}

// Prompt input yes enables policy.
func TestRunStart_PromptInputYesEnablesPolicy(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "yes")
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !*persistCalled {
		t.Error("persist not called on 'yes' input")
	}
	if !strings.Contains(stdout, "Remote keep-awake policy enabled.") {
		t.Error("expected enabled confirmation")
	}
}

// Prompt input no preserves policy.
func TestRunStart_PromptInputNoPreservesPolicy(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubReadLine(t, "no")
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called on 'no' input (should preserve)")
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Error("expected unchanged message on 'no' input")
	}
}

// No-prompt path does not persist.
func TestRunStart_NoPromptPathDoesNotPersist(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	stubHostDispatch(t, 0)

	code, _, _ := runStartTest(t, []string{"--no-prompt"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called with --no-prompt (should not mutate)")
	}
}

// -----------------------------------------------------------------------
// P18U4 integration tests
// -----------------------------------------------------------------------

func TestRunStart_Integration_EnableRemoteKeepAwakeBeforeHostStartDispatch(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
keep_awake_remote_enabled = false
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)

	var order []string
	origPersist := startPersistPolicy
	t.Cleanup(func() { startPersistPolicy = origPersist })
	startPersistPolicy = func(path string, enabled bool) error {
		order = append(order, "persist")
		return nil
	}

	origDispatch := startHostDispatch
	t.Cleanup(func() { startHostDispatch = origDispatch })
	startHostDispatch = func(args []string, stdout, stderr io.Writer) int {
		order = append(order, "dispatch")
		return 0
	}

	code, _, _ := runStartTest(t, []string{"--enable-remote-keep-awake"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(order) != 2 || order[0] != "persist" || order[1] != "dispatch" {
		t.Errorf("expected [persist, dispatch], got %v", order)
	}
}

func TestRunStart_Integration_NonInteractiveScriptSafeDefaults(t *testing.T) {
	cfgPath := writeConfig(t, `require_auth = true
`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, false)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	dispatchCalled := stubHostDispatch(t, 0)

	code, stdout, _ := runStartTest(t, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if *persistCalled {
		t.Error("persist called in non-interactive mode without flags")
	}
	if !*dispatchCalled {
		t.Error("host dispatch not called")
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown in non-interactive context")
	}
}

func TestRunStart_Integration_MalformedConfigFailClosed(t *testing.T) {
	cfgPath := writeConfig(t, `[broken
key = "unclosed`)
	stubConfigPath(t, cfgPath)
	stubIsTerminal(t, true)
	stubNetwork(t)
	persistCalled := stubPersistPolicy(t, nil)
	dispatchCalled := stubHostDispatch(t, 0)

	code, stdout, stderr := runStartTest(t, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on malformed config", code)
	}
	if *persistCalled {
		t.Error("persist called despite malformed config")
	}
	if *dispatchCalled {
		t.Error("host dispatch called despite malformed config")
	}
	if strings.Contains(stdout, "Enable remote keep-awake?") {
		t.Error("prompt shown despite malformed config")
	}
	if !strings.Contains(stderr, "failed to parse config") {
		t.Error("expected actionable parse error in stderr")
	}
}

// -----------------------------------------------------------------------
// parsePromptAnswer unit tests
// -----------------------------------------------------------------------

func TestParsePromptAnswer(t *testing.T) {
	tests := []struct {
		input     string
		wantEn    bool
		wantValid bool
	}{
		{"y", true, true},
		{"Y", true, true},
		{"yes", true, true},
		{"YES", true, true},
		{"  yes  ", true, true},
		{"n", false, true},
		{"N", false, true},
		{"no", false, true},
		{"NO", false, true},
		{"", false, true},
		{"  ", false, true},
		{"maybe", false, false},
		{"yep", false, false},
		{"nah", false, false},
	}
	for _, tt := range tests {
		en, valid := parsePromptAnswer(tt.input)
		if en != tt.wantEn || valid != tt.wantValid {
			t.Errorf("parsePromptAnswer(%q) = (%v, %v), want (%v, %v)",
				tt.input, en, valid, tt.wantEn, tt.wantValid)
		}
	}
}

// -----------------------------------------------------------------------
// shouldShowKeepAwakePrompt unit tests
// -----------------------------------------------------------------------

func TestShouldShowKeepAwakePrompt(t *testing.T) {
	tests := []struct {
		name           string
		isTTY          bool
		policyEnabled  bool
		enableFlagSet  bool
		noPromptSet    bool
		want           bool
	}{
		{"all conditions met", true, false, false, false, true},
		{"non-TTY", false, false, false, false, false},
		{"policy enabled", true, true, false, false, false},
		{"enable flag set", true, false, true, false, false},
		{"no-prompt set", true, false, false, true, false},
		{"all false except TTY", true, true, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldShowKeepAwakePrompt(tt.isTTY, tt.policyEnabled, tt.enableFlagSet, tt.noPromptSet)
			if got != tt.want {
				t.Errorf("shouldShowKeepAwakePrompt(%v, %v, %v, %v) = %v, want %v",
					tt.isTTY, tt.policyEnabled, tt.enableFlagSet, tt.noPromptSet, got, tt.want)
			}
		})
	}
}
