// Package config provides TOML configuration file loading and parsing for the host.
// The configuration file lives at ~/.pseudocoder/config.toml by default, but can be
// overridden with the --config flag. CLI flags always take precedence over file values.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

// Config represents the host configuration file structure.
// Field names use Go camelCase internally but map to snake_case in TOML files
// via struct tags. This matches the convention documented in docs/TDD.md.
type Config struct {
	// Repo is the path to the repository to supervise.
	// If empty, defaults to the current working directory.
	Repo string `toml:"repo"`

	// Addr is the host:port for the WebSocket server.
	// Default: 127.0.0.1:7070
	Addr string `toml:"addr"`

	// TLSCert is the path to the TLS certificate file.
	// Default: ~/.pseudocoder/certs/host.crt (auto-generated if missing)
	TLSCert string `toml:"tls_cert"`

	// TLSKey is the path to the TLS key file.
	// Default: ~/.pseudocoder/certs/host.key (auto-generated if missing)
	TLSKey string `toml:"tls_key"`

	// TokenStore is the path to the SQLite database for tokens and cards.
	// Default: ~/.pseudocoder/pseudocoder.db
	TokenStore string `toml:"token_store"`

	// LogLevel controls logging verbosity: debug, info, warn, error.
	// Default: info
	LogLevel string `toml:"log_level"`

	// HistoryLines is the number of terminal lines to retain in the ring buffer.
	// Default: 5000
	HistoryLines int `toml:"history_lines"`

	// DiffPollMs is the interval for git diff polling in milliseconds.
	// Default: 1000
	DiffPollMs int `toml:"diff_poll_ms"`

	// SessionCmd is the command to run in the PTY session.
	// If empty, defaults to the user's shell ($SHELL or /bin/sh).
	SessionCmd string `toml:"session_cmd"`

	// RequireAuth enables token-based authentication for WebSocket connections.
	// Default: false
	RequireAuth bool `toml:"require_auth"`

	// CommitAllowNoVerify allows clients to skip pre-commit hooks.
	// Not recommended for production use. Default: false
	CommitAllowNoVerify bool `toml:"commit_allow_no_verify"`

	// CommitAllowNoGpgSign allows clients to skip GPG signing.
	// Default: false
	CommitAllowNoGpgSign bool `toml:"commit_allow_no_gpg_sign"`

	// PushAllowForceWithLease allows clients to use force-with-lease.
	// Not recommended for production use. Default: false
	PushAllowForceWithLease bool `toml:"push_allow_force_with_lease"`

	// Daemon runs the host as a background daemon.
	// Default: false
	Daemon bool `toml:"daemon"`

	// LocalTerminal shows PTY output locally (in addition to streaming).
	// Default: false (headless mode - only server logs shown locally)
	LocalTerminal bool `toml:"local_terminal"`

	// PIDFile is the path to write the daemon PID file.
	// Default: ~/.pseudocoder/host.pid
	PIDFile string `toml:"pid_file"`

	// LogFile is the path for daemon log output.
	// Default: ~/.pseudocoder/host.log
	LogFile string `toml:"log_file"`

	// MdnsEnabled enables mDNS/Bonjour service advertisement.
	// When true, the host advertises itself on the local network,
	// allowing mobile apps to discover it without manual IP entry.
	// Discovery only reveals presence; pairing codes are still required.
	// Default: false (disabled for security - must be explicitly enabled)
	MdnsEnabled bool `toml:"mdns_enabled"`

	// Pair generates and displays a pairing code during startup.
	// When true, eliminates the need to run 'pseudocoder pair' separately.
	// Default: false
	Pair bool `toml:"pair"`

	// QR displays the pairing code as a QR code (requires Pair to be true).
	// Default: false
	QR bool `toml:"qr"`

	// PairSocket is the Unix socket path for pairing IPC.
	// Default: ~/.pseudocoder/pair.sock
	PairSocket string `toml:"pair_socket"`

	// ChunkGroupingEnabled enables proximity-based chunk grouping in diff cards.
	// When true, chunks within ChunkGroupingProximity lines are grouped together.
	// Default: true (but Go zero value is false; caller must apply defaults)
	ChunkGroupingEnabled bool `toml:"chunk_grouping_enabled"`

	// ChunkGroupingProximity is the maximum line distance for grouping chunks.
	// Chunks within this many lines of each other are grouped together.
	// Default: 20, must be >= 1 when set. Zero means "use default".
	ChunkGroupingProximity int `toml:"chunk_grouping_proximity"`

	// KeepAwakeRemoteEnabled enables remote keep-awake mutations from mobile.
	// Default: false
	KeepAwakeRemoteEnabled bool `toml:"keep_awake_remote_enabled"`

	// KeepAwakeAllowAdminRevoke allows admin devices to revoke any lease.
	// Requires KeepAwakeAdminDeviceIDs to be non-empty.
	// Default: false
	KeepAwakeAllowAdminRevoke bool `toml:"keep_awake_allow_admin_revoke"`

	// KeepAwakeAdminDeviceIDs lists device IDs with admin privileges.
	KeepAwakeAdminDeviceIDs []string `toml:"keep_awake_admin_device_ids"`

	// KeepAwakeAllowOnBattery permits keep-awake when on battery power.
	// Default: true (Go zero false; host.go transforms to semantic default)
	KeepAwakeAllowOnBattery bool `toml:"keep_awake_allow_on_battery"`

	// KeepAwakeAllowOnBatterySet tracks whether keep_awake_allow_on_battery
	// was explicitly defined in the config file. This preserves the ability to
	// distinguish "unset" (semantic default true) from explicit false.
	KeepAwakeAllowOnBatterySet bool `toml:"-"`

	// KeepAwakeAutoDisableBatteryPercent is the battery % threshold for auto-disable.
	// 0 means disabled. When set, must be 1-100.
	KeepAwakeAutoDisableBatteryPercent int `toml:"keep_awake_auto_disable_battery_percent"`

	// KeepAwakeAuditMaxRows caps the number of durable audit rows retained.
	// 0 means use default (1000). When set, must be 100-50000.
	KeepAwakeAuditMaxRows int `toml:"keep_awake_audit_max_rows"`

}

// DefaultConfigPath returns the default config file location: ~/.pseudocoder/config.toml.
// Returns an error only if the user's home directory cannot be determined.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".pseudocoder", "config.toml"), nil
}

// DefaultPairSocketPath returns the default pairing IPC socket path: ~/.pseudocoder/pair.sock.
// Returns an error only if the user's home directory cannot be determined.
func DefaultPairSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".pseudocoder", "pair.sock"), nil
}

// WriteDefault creates a config file with mobile-ready defaults at the given path.
// The config requires authentication and leaves the bind address unset so
// CLI defaults can decide the runtime listen address.
//
// Behavior:
//   - If the file already exists, returns without error (does not overwrite).
//   - Creates the parent directory if it doesn't exist.
//   - Returns an error if the file cannot be written.
func WriteDefault(path string, repo string) error {
	// Check if file already exists - never overwrite
	if _, err := os.Stat(path); err == nil {
		return nil // File exists, nothing to do
	}

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Build minimal TOML config with mobile-ready defaults
	// Using raw string to control formatting exactly
	content := fmt.Sprintf(`# Pseudocoder configuration
# Created by 'pseudocoder start' for mobile-ready defaults

# Require authentication for security
require_auth = true

# Repository to supervise
repo = %q
`, repo)

	// Write with restrictive permissions (owner read/write only)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// Load reads a TOML config file from the given path and returns a Config.
//
// Behavior:
//   - If path is empty, attempts to load from the default location (~/.pseudocoder/config.toml).
//     Returns an empty Config without error if the default file doesn't exist.
//   - If path is specified, returns an error if the file doesn't exist.
//   - Returns an error if the file exists but cannot be parsed.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path == "" {
		// No explicit path: try default location, but don't error if missing.
		// This allows the host to start without any config file.
		defaultPath, err := DefaultConfigPath()
		if err != nil {
			// Can't determine home dir, return empty config
			return cfg, nil
		}
		if _, err := os.Stat(defaultPath); os.IsNotExist(err) {
			// Default config doesn't exist, that's fine
			return cfg, nil
		}
		path = defaultPath
	} else {
		// Explicit path provided: error if file doesn't exist.
		// This matches user expectation: if they specify a config file, it should exist.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
	}

	// Parse the TOML file. Any parse error is fatal since the user expects
	// the config to be applied.
	meta, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	cfg.KeepAwakeAllowOnBatterySet = meta.IsDefined("keep_awake_allow_on_battery")

	return cfg, nil
}

// EffectiveKeepAwakeAllowOnBattery resolves the semantic keep-awake battery policy
// default. When the config key is omitted, default to true.
func (c *Config) EffectiveKeepAwakeAllowOnBattery() bool {
	if !c.KeepAwakeAllowOnBatterySet {
		return true
	}
	return c.KeepAwakeAllowOnBattery
}

// Validate checks config values for semantic correctness.
// Returns an error if any value is invalid.
//
// Validation rules:
//   - ChunkGroupingProximity must be >= 1 when set (non-zero).
//     Zero indicates "use default" and is valid.
//
// This method does not apply defaults; the caller is responsible for that.
// This separation allows Load() to be a pure parser and Validate() to be
// a semantic checker, matching the existing pattern in this codebase.
func (c *Config) Validate() error {
	// ChunkGroupingProximity: 0 means "use default", any positive value is valid,
	// but negative values are always invalid.
	if c.ChunkGroupingProximity < 0 {
		return fmt.Errorf("chunk_grouping_proximity must be >= 1, got %d", c.ChunkGroupingProximity)
	}

	// KeepAwakeAutoDisableBatteryPercent: 0 means disabled, 1-100 valid.
	if c.KeepAwakeAutoDisableBatteryPercent != 0 &&
		(c.KeepAwakeAutoDisableBatteryPercent < 1 || c.KeepAwakeAutoDisableBatteryPercent > 100) {
		return fmt.Errorf("keep_awake_auto_disable_battery_percent must be 0 (disabled) or 1-100, got %d", c.KeepAwakeAutoDisableBatteryPercent)
	}

	// KeepAwakeAuditMaxRows: 0 means use default, 100-50000 valid.
	if c.KeepAwakeAuditMaxRows != 0 &&
		(c.KeepAwakeAuditMaxRows < 100 || c.KeepAwakeAuditMaxRows > 50000) {
		return fmt.Errorf("keep_awake_audit_max_rows must be 0 (default) or 100-50000, got %d", c.KeepAwakeAuditMaxRows)
	}

	// KeepAwakeAllowAdminRevoke requires non-empty admin device IDs.
	if c.KeepAwakeAllowAdminRevoke {
		normalized := NormalizeKeepAwakeAdminDeviceIDs(c.KeepAwakeAdminDeviceIDs)
		if len(normalized) == 0 {
			return fmt.Errorf("keep_awake_allow_admin_revoke requires non-empty keep_awake_admin_device_ids")
		}
	}

	return nil
}

// PersistKeepAwakePolicy atomically updates keep_awake_remote_enabled in the
// config file. Uses line-by-line text replacement to preserve comments/formatting.
// The file is protected by flock to prevent TOCTOU races.
func PersistKeepAwakePolicy(configPath string, remoteEnabled bool) error {
	// Resolve canonical path, rejecting unsafe symlinks.
	canonical, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config file not found: %s", configPath)
		}
		return fmt.Errorf("unsafe symlink or unresolvable path %s: %w", configPath, err)
	}
	// Reject if symlink resolves outside the original parent directory.
	origDir := filepath.Dir(configPath)
	canonDir := filepath.Dir(canonical)
	origDirCanon, err := filepath.EvalSymlinks(origDir)
	if err != nil {
		return fmt.Errorf("cannot resolve parent directory %s: %w", origDir, err)
	}
	if canonDir != origDirCanon {
		return fmt.Errorf("unsafe symlink: %s resolves outside expected directory", configPath)
	}

	// Check directory is writable before acquiring lock.
	dirInfo, err := os.Stat(canonDir)
	if err != nil {
		return fmt.Errorf("cannot stat config directory %s: %w", canonDir, err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("config parent is not a directory: %s", canonDir)
	}
	// Try creating a temp file to test directory writability.
	testFile, err := os.CreateTemp(canonDir, ".pseudocoder-probe-*")
	if err != nil {
		return fmt.Errorf("config directory is read-only: %s (create a writable config directory or run with appropriate permissions)", canonDir)
	}
	testFile.Close()
	os.Remove(testFile.Name())

	// Open file for reading + lock.
	f, err := os.OpenFile(canonical, os.O_RDWR, 0)
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("config file is read-only: %s (run 'chmod u+w %s' to fix)", canonical, canonical)
		}
		return fmt.Errorf("cannot open config file %s: %w", canonical, err)
	}
	defer f.Close()

	// Acquire exclusive flock (non-blocking).
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("config file is locked by another process (retry in a moment): %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Re-read under lock to prevent TOCTOU.
	content, err := os.ReadFile(canonical)
	if err != nil {
		return fmt.Errorf("cannot read config file under lock: %w", err)
	}

	// Validate TOML is parseable.
	var probe Config
	if _, err := toml.Decode(string(content), &probe); err != nil {
		return fmt.Errorf("config file has malformed TOML (fix syntax errors before retrying): %w", err)
	}

	// Line-by-line replacement of keep_awake_remote_enabled.
	newValue := fmt.Sprintf("keep_awake_remote_enabled = %t", remoteEnabled)
	lines := strings.Split(string(content), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "keep_awake_remote_enabled") && !strings.HasPrefix(trimmed, "#") {
			// Preserve leading whitespace.
			leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = leading + newValue
			found = true
			break
		}
	}
	if !found {
		// Append the key. Find the right place: after last keep_awake_ key or at end.
		insertIdx := len(lines)
		for i := len(lines) - 1; i >= 0; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "keep_awake_") && !strings.HasPrefix(trimmed, "#") {
				insertIdx = i + 1
				break
			}
		}
		// Insert the new line.
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:insertIdx]...)
		newLines = append(newLines, newValue)
		newLines = append(newLines, lines[insertIdx:]...)
		lines = newLines
	}

	newContent := strings.Join(lines, "\n")

	// Atomic write: temp file + rename.
	tmpFile, err := os.CreateTemp(canonDir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("cannot create temp file for atomic write: %w", err)
	}
	tmpName := tmpFile.Name()

	w := bufio.NewWriter(tmpFile)
	if _, err := w.WriteString(newContent); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cannot write temp config: %w", err)
	}
	if err := w.Flush(); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cannot flush temp config: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cannot sync temp config: %w", err)
	}

	// Preserve original file permissions.
	origInfo, err := os.Stat(canonical)
	if err == nil {
		os.Chmod(tmpName, origInfo.Mode().Perm())
	}

	tmpFile.Close()

	if err := os.Rename(tmpName, canonical); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic rename failed: %w", err)
	}

	// Post-write identity check: verify symlink still resolves to same path.
	postCanonical, err := filepath.EvalSymlinks(configPath)
	if err != nil || postCanonical != canonical {
		// The symlink target changed during write. The write landed on the
		// original canonical path, which is still correct, but log a warning.
		// We don't roll back because the data was written to the intended file.
	}

	return nil
}

// NormalizeKeepAwakeAdminDeviceIDs trims whitespace, removes empty entries,
// and deduplicates while preserving case and order.
func NormalizeKeepAwakeAdminDeviceIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	var result []string
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
