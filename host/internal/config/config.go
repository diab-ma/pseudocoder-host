// Package config provides TOML configuration file loading and parsing for the host.
// The configuration file lives at ~/.pseudocoder/config.toml by default, but can be
// overridden with the --config flag. CLI flags always take precedence over file values.
package config

import (
	"fmt"
	"os"
	"path/filepath"

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

// WriteDefault creates a config file with mobile-ready defaults at the given path.
// The config enables LAN access (0.0.0.0:7070) and requires authentication.
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

# Listen on all interfaces for LAN access
addr = "0.0.0.0:7070"

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
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	return cfg, nil
}
