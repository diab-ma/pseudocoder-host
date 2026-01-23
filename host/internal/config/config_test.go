package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_AllFields verifies that all config fields are parsed correctly from TOML.
func TestLoad_AllFields(t *testing.T) {
	// Create a temporary config file with all fields set
	content := `
repo = "/path/to/repo"
addr = "0.0.0.0:8080"
tls_cert = "/path/to/cert.crt"
tls_key = "/path/to/key.key"
token_store = "/path/to/store.db"
log_level = "debug"
history_lines = 10000
diff_poll_ms = 2000
session_cmd = "zsh"
require_auth = true
commit_allow_no_verify = true
commit_allow_no_gpg_sign = true
push_allow_force_with_lease = true
daemon = true
local_terminal = true
pid_file = "/var/run/pseudocoder.pid"
log_file = "/var/log/pseudocoder.log"
pair = true
qr = true
pair_socket = "/tmp/pseudocoder-pair.sock"
chunk_grouping_enabled = true
chunk_grouping_proximity = 25
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify all fields
	if cfg.Repo != "/path/to/repo" {
		t.Errorf("Repo = %q, want %q", cfg.Repo, "/path/to/repo")
	}
	if cfg.Addr != "0.0.0.0:8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "0.0.0.0:8080")
	}
	if cfg.TLSCert != "/path/to/cert.crt" {
		t.Errorf("TLSCert = %q, want %q", cfg.TLSCert, "/path/to/cert.crt")
	}
	if cfg.TLSKey != "/path/to/key.key" {
		t.Errorf("TLSKey = %q, want %q", cfg.TLSKey, "/path/to/key.key")
	}
	if cfg.TokenStore != "/path/to/store.db" {
		t.Errorf("TokenStore = %q, want %q", cfg.TokenStore, "/path/to/store.db")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.HistoryLines != 10000 {
		t.Errorf("HistoryLines = %d, want %d", cfg.HistoryLines, 10000)
	}
	if cfg.DiffPollMs != 2000 {
		t.Errorf("DiffPollMs = %d, want %d", cfg.DiffPollMs, 2000)
	}
	if cfg.SessionCmd != "zsh" {
		t.Errorf("SessionCmd = %q, want %q", cfg.SessionCmd, "zsh")
	}
	if !cfg.RequireAuth {
		t.Error("RequireAuth = false, want true")
	}
	if !cfg.CommitAllowNoVerify {
		t.Error("CommitAllowNoVerify = false, want true")
	}
	if !cfg.CommitAllowNoGpgSign {
		t.Error("CommitAllowNoGpgSign = false, want true")
	}
	if !cfg.PushAllowForceWithLease {
		t.Error("PushAllowForceWithLease = false, want true")
	}
	// Unit 9.4: Daemon mode fields
	if !cfg.Daemon {
		t.Error("Daemon = false, want true")
	}
	if !cfg.LocalTerminal {
		t.Error("LocalTerminal = false, want true")
	}
	if cfg.PIDFile != "/var/run/pseudocoder.pid" {
		t.Errorf("PIDFile = %q, want %q", cfg.PIDFile, "/var/run/pseudocoder.pid")
	}
	if cfg.LogFile != "/var/log/pseudocoder.log" {
		t.Errorf("LogFile = %q, want %q", cfg.LogFile, "/var/log/pseudocoder.log")
	}
	// Unit 18.1: Integrated pairing fields
	if !cfg.Pair {
		t.Error("Pair = false, want true")
	}
	if !cfg.QR {
		t.Error("QR = false, want true")
	}
	if cfg.PairSocket != "/tmp/pseudocoder-pair.sock" {
		t.Errorf("PairSocket = %q, want %q", cfg.PairSocket, "/tmp/pseudocoder-pair.sock")
	}
	// Unit 2.1: Chunk grouping fields
	if !cfg.ChunkGroupingEnabled {
		t.Error("ChunkGroupingEnabled = false, want true")
	}
	if cfg.ChunkGroupingProximity != 25 {
		t.Errorf("ChunkGroupingProximity = %d, want 25", cfg.ChunkGroupingProximity)
	}
}

// TestLoad_PartialConfig verifies that a config with only some fields set
// leaves other fields at their zero values.
func TestLoad_PartialConfig(t *testing.T) {
	content := `
addr = "0.0.0.0:9090"
history_lines = 3000
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Specified fields should be set
	if cfg.Addr != "0.0.0.0:9090" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "0.0.0.0:9090")
	}
	if cfg.HistoryLines != 3000 {
		t.Errorf("HistoryLines = %d, want %d", cfg.HistoryLines, 3000)
	}

	// Unspecified fields should be zero values
	if cfg.Repo != "" {
		t.Errorf("Repo = %q, want empty", cfg.Repo)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty", cfg.LogLevel)
	}
	if cfg.DiffPollMs != 0 {
		t.Errorf("DiffPollMs = %d, want 0", cfg.DiffPollMs)
	}
	if cfg.RequireAuth {
		t.Error("RequireAuth = true, want false")
	}
	// Unit 9.4: Daemon mode fields should default to zero values
	if cfg.Daemon {
		t.Error("Daemon = true, want false")
	}
	if cfg.LocalTerminal {
		t.Error("LocalTerminal = true, want false")
	}
	if cfg.PIDFile != "" {
		t.Errorf("PIDFile = %q, want empty", cfg.PIDFile)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty", cfg.LogFile)
	}
	// Unit 18.1: Pairing fields should default to false
	if cfg.Pair {
		t.Error("Pair = true, want false")
	}
	if cfg.QR {
		t.Error("QR = true, want false")
	}
	if cfg.PairSocket != "" {
		t.Errorf("PairSocket = %q, want empty", cfg.PairSocket)
	}
	// Unit 2.1: Chunk grouping fields should default to zero values
	if cfg.ChunkGroupingEnabled {
		t.Error("ChunkGroupingEnabled = true, want false (zero value)")
	}
	if cfg.ChunkGroupingProximity != 0 {
		t.Errorf("ChunkGroupingProximity = %d, want 0 (zero value)", cfg.ChunkGroupingProximity)
	}
}

// TestLoad_ExplicitPath_NotFound verifies that an error is returned when
// an explicit config path is provided but the file doesn't exist.
func TestLoad_ExplicitPath_NotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	if err == nil {
		t.Error("Load() expected error for missing file, got nil")
	}
}

// TestLoad_EmptyPath_NoDefaultFile verifies that an empty path returns
// an empty Config without error when no default file exists.
func TestLoad_EmptyPath_NoDefaultFile(t *testing.T) {
	// Set HOME to a temp dir without config.toml
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)
	os.Setenv("HOME", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	// Should return empty config
	if cfg.Addr != "" {
		t.Errorf("Addr = %q, want empty", cfg.Addr)
	}
	if cfg.HistoryLines != 0 {
		t.Errorf("HistoryLines = %d, want 0", cfg.HistoryLines)
	}
}

// TestLoad_EmptyPath_DefaultFileExists verifies that an empty path loads
// from the default location when the file exists.
func TestLoad_EmptyPath_DefaultFileExists(t *testing.T) {
	// Set HOME to a temp dir and create config.toml there
	tmpHome := t.TempDir()
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)
	os.Setenv("HOME", tmpHome)

	// Create .pseudocoder directory and config.toml
	configDir := filepath.Join(tmpHome, ".pseudocoder")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatalf("Failed to create config dir: %v", err)
	}

	content := `addr = "localhost:7777"`
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	if cfg.Addr != "localhost:7777" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "localhost:7777")
	}
}

// TestLoad_InvalidTOML verifies that a parse error is returned for invalid TOML.
func TestLoad_InvalidTOML(t *testing.T) {
	content := `
addr = "missing quote
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	_, err := Load(tmpFile)
	if err == nil {
		t.Error("Load() expected error for invalid TOML, got nil")
	}
}

// TestDefaultConfigPath verifies the default config path format.
func TestDefaultConfigPath(t *testing.T) {
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error: %v", err)
	}

	// Should end with .pseudocoder/config.toml
	if filepath.Base(path) != "config.toml" {
		t.Errorf("DefaultConfigPath() = %q, want filename config.toml", path)
	}
	if filepath.Base(filepath.Dir(path)) != ".pseudocoder" {
		t.Errorf("DefaultConfigPath() = %q, want parent dir .pseudocoder", path)
	}
}

// TestDefaultPairSocketPath verifies the default pairing socket path format.
func TestDefaultPairSocketPath(t *testing.T) {
	path, err := DefaultPairSocketPath()
	if err != nil {
		t.Fatalf("DefaultPairSocketPath() error: %v", err)
	}

	if filepath.Base(path) != "pair.sock" {
		t.Errorf("DefaultPairSocketPath() = %q, want filename pair.sock", path)
	}
	if filepath.Base(filepath.Dir(path)) != ".pseudocoder" {
		t.Errorf("DefaultPairSocketPath() = %q, want parent dir .pseudocoder", path)
	}
}

// TestWriteDefault_CreatesFile verifies that WriteDefault creates a config file
// with mobile-ready defaults.
func TestWriteDefault_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, ".pseudocoder", "config.toml")

	err := WriteDefault(configPath, "/path/to/repo")
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("Config file was not created")
	}

	// Verify file permissions (0600)
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("File permissions = %o, want 0600", info.Mode().Perm())
	}

	// Load the config and verify defaults
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.RequireAuth {
		t.Error("RequireAuth = false, want true")
	}
	if cfg.Repo != "/path/to/repo" {
		t.Errorf("Repo = %q, want %q", cfg.Repo, "/path/to/repo")
	}
}

// TestWriteDefault_NoOverwrite verifies that WriteDefault does not overwrite
// an existing config file.
func TestWriteDefault_NoOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Create an existing config with different values
	existingContent := `addr = "127.0.0.1:9999"
require_auth = false
repo = "/existing/repo"
`
	if err := os.WriteFile(configPath, []byte(existingContent), 0600); err != nil {
		t.Fatalf("Failed to write existing config: %v", err)
	}

	// Call WriteDefault - should not overwrite
	err := WriteDefault(configPath, "/new/repo")
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	// Verify original content is preserved
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want %q (original should be preserved)", cfg.Addr, "127.0.0.1:9999")
	}
	if cfg.RequireAuth {
		t.Error("RequireAuth = true, want false (original should be preserved)")
	}
	if cfg.Repo != "/existing/repo" {
		t.Errorf("Repo = %q, want %q (original should be preserved)", cfg.Repo, "/existing/repo")
	}
}

// TestWriteDefault_CreatesDirectory verifies that WriteDefault creates the
// parent directory if it doesn't exist.
func TestWriteDefault_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	// Use nested directory that doesn't exist
	configPath := filepath.Join(tmpDir, "nested", "deep", "config.toml")

	err := WriteDefault(configPath, "/some/repo")
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("Config file was not created")
	}

	// Verify directory permissions (0700)
	dirInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		t.Fatalf("Stat(dir) error: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("Dir permissions = %o, want 0700", dirInfo.Mode().Perm())
	}
}

// TestWriteDefault_RepoWithSpecialChars verifies that repo paths with special
// characters are properly quoted in the TOML output.
func TestWriteDefault_RepoWithSpecialChars(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Repo path with quotes and backslashes
	repoPath := `/path/with "quotes" and\backslashes`

	err := WriteDefault(configPath, repoPath)
	if err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	// Load and verify the repo path is correctly parsed
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Repo != repoPath {
		t.Errorf("Repo = %q, want %q", cfg.Repo, repoPath)
	}
}

// =============================================================================
// Phase 2 Unit 2.1: Chunk Grouping Config Fields
// =============================================================================

// TestLoad_ChunkGroupingFields verifies that chunk grouping config fields
// are parsed correctly from TOML.
func TestLoad_ChunkGroupingFields(t *testing.T) {
	content := `
chunk_grouping_enabled = true
chunk_grouping_proximity = 30
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.ChunkGroupingEnabled {
		t.Error("ChunkGroupingEnabled = false, want true")
	}
	if cfg.ChunkGroupingProximity != 30 {
		t.Errorf("ChunkGroupingProximity = %d, want 30", cfg.ChunkGroupingProximity)
	}
}

// TestLoad_ChunkGroupingEnabled_False verifies that explicit false value parses.
func TestLoad_ChunkGroupingEnabled_False(t *testing.T) {
	content := `
chunk_grouping_enabled = false
chunk_grouping_proximity = 20
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.ChunkGroupingEnabled {
		t.Error("ChunkGroupingEnabled = true, want false")
	}
	if cfg.ChunkGroupingProximity != 20 {
		t.Errorf("ChunkGroupingProximity = %d, want 20", cfg.ChunkGroupingProximity)
	}
}

// TestValidate_ChunkGroupingProximity uses table-driven tests to verify
// proximity validation for boundary and adversarial cases.
func TestValidate_ChunkGroupingProximity(t *testing.T) {
	tests := []struct {
		name      string
		proximity int
		wantErr   bool
	}{
		// Valid cases
		{"valid_default", 20, false},
		{"valid_boundary", 1, false},
		{"valid_large", 1000, false},
		{"valid_zero_means_unset", 0, false}, // 0 means "use default"

		// Adversarial cases (from PLANS.md)
		{"invalid_negative", -1, true},
		{"invalid_negative_large", -100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ChunkGroupingProximity: tt.proximity,
			}
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_Valid_EmptyConfig verifies that an empty config passes validation.
// Zero values indicate "use default" and are valid.
func TestValidate_Valid_EmptyConfig(t *testing.T) {
	cfg := &Config{}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("Validate() error = %v, want nil for empty config", err)
	}
}

// TestValidate_ErrorMessage verifies that validation errors include helpful details.
func TestValidate_ErrorMessage(t *testing.T) {
	cfg := &Config{
		ChunkGroupingProximity: -5,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error, got nil")
	}

	// Error should mention the field name and the invalid value
	errMsg := err.Error()
	if !contains(errMsg, "chunk_grouping_proximity") {
		t.Errorf("Error message should mention field name, got: %s", errMsg)
	}
	if !contains(errMsg, "-5") {
		t.Errorf("Error message should mention invalid value, got: %s", errMsg)
	}
}

// contains is a helper to check if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
