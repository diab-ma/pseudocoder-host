package main

import (
	"bytes"
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
	if !strings.Contains(output, "-pair") {
		t.Errorf("Help output missing -pair flag, got: %s", output)
	}
	if !strings.Contains(output, "-qr") {
		t.Errorf("Help output missing -qr flag, got: %s", output)
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
	if cfg.Addr != "0.0.0.0:7070" {
		t.Errorf("Addr = %q, want %q (LAN-ready default)", cfg.Addr, "0.0.0.0:7070")
	}
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
