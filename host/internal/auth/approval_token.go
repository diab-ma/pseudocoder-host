// Package auth provides authentication and authorization for the host application.
// This file implements the approval token manager for CLI command approval.
// The token is used to authenticate requests to the /approve HTTP endpoint.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ApprovalTokenManager handles the CLI approval token.
// The token is a random 32-byte hex string (64 characters) stored in a file.
// This token is used to authenticate CLI tools making approval requests
// to the /approve HTTP endpoint.
//
// Thread safety: After EnsureToken() is called, the token is immutable
// and ValidateToken is safe for concurrent use.
//
// Security notes (accepted risks):
// - Token file race: If multiple instances start simultaneously before the
//   token file exists, they could generate different tokens. This is acceptable
//   as the host is designed to run as a single-instance daemon.
// - Token in memory: The token is stored in plaintext in process memory.
//   This is acceptable as the daemon must validate tokens at runtime, and
//   any process with memory access already has sufficient privileges.
// - File permissions: The token file is created with 0600 and directory with
//   0700 permissions, restricting access to the owner only.
type ApprovalTokenManager struct {
	// tokenPath is the path to the token file.
	tokenPath string

	// token is the loaded/generated token.
	// Empty until EnsureToken() is called.
	token string
}

// NewApprovalTokenManager creates a new token manager with the given token path.
// The path should be an absolute path. Use DefaultApprovalTokenPath() to get
// the standard path (~/.pseudocoder/approval.token).
//
// The token is not loaded or generated until EnsureToken() is called.
func NewApprovalTokenManager(tokenPath string) *ApprovalTokenManager {
	return &ApprovalTokenManager{
		tokenPath: tokenPath,
	}
}

// DefaultApprovalTokenPath returns the default path for the approval token file.
// This is ~/.pseudocoder/approval.token on all platforms.
func DefaultApprovalTokenPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pseudocoder", "approval.token"), nil
}

// EnsureToken loads the token from the file, or generates a new one if it doesn't exist.
// The parent directory is created if needed with 0700 permissions.
// The token file is created with 0600 permissions (owner read/write only).
//
// Returns the token string on success. The token is also cached in memory
// for subsequent ValidateToken calls.
func (m *ApprovalTokenManager) EnsureToken() (string, error) {
	// Check if token file exists
	data, err := os.ReadFile(m.tokenPath)
	if err == nil {
		// File exists, load the token
		m.token = strings.TrimSpace(string(data))
		if m.token == "" {
			// File is empty, regenerate
			return m.generateNewToken()
		}
		log.Printf("auth: loaded approval token from %s", m.tokenPath)
		return m.token, nil
	}

	if !os.IsNotExist(err) {
		// Some other error (permissions, etc.)
		return "", fmt.Errorf("failed to read token file %s: %w", m.tokenPath, err)
	}

	// File doesn't exist, generate a new token
	return m.generateNewToken()
}

// generateNewToken creates a new random token and saves it to the file.
func (m *ApprovalTokenManager) generateNewToken() (string, error) {
	// Generate 32 random bytes (256 bits of entropy)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate random token: %w", err)
	}

	// Hex-encode to 64 characters
	m.token = hex.EncodeToString(tokenBytes)

	// Ensure parent directory exists with 0700 permissions
	dir := filepath.Dir(m.tokenPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create token directory %s: %w", dir, err)
	}

	// Write token to file with 0600 permissions (owner read/write only)
	if err := os.WriteFile(m.tokenPath, []byte(m.token), 0600); err != nil {
		return "", fmt.Errorf("failed to write token file %s: %w", m.tokenPath, err)
	}

	log.Printf("auth: generated new approval token at %s", m.tokenPath)
	return m.token, nil
}

// ValidateToken checks if the provided token matches the stored token.
// Returns true if the token is valid, false otherwise.
// Uses constant-time comparison to prevent timing attacks.
//
// Note: EnsureToken() must be called before ValidateToken(),
// otherwise this always returns false.
func (m *ApprovalTokenManager) ValidateToken(token string) bool {
	if m.token == "" {
		return false
	}
	if token == "" {
		return false
	}
	// Use constant-time comparison to prevent timing attacks
	return subtle.ConstantTimeCompare([]byte(m.token), []byte(token)) == 1
}

// TokenPath returns the path to the token file.
func (m *ApprovalTokenManager) TokenPath() string {
	return m.tokenPath
}
