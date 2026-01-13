package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultApprovalTokenPath verifies the default token path format.
func TestDefaultApprovalTokenPath(t *testing.T) {
	path, err := DefaultApprovalTokenPath()
	if err != nil {
		t.Fatalf("DefaultApprovalTokenPath() error: %v", err)
	}

	// Should contain .pseudocoder
	if !strings.Contains(path, ".pseudocoder") {
		t.Errorf("expected path to contain '.pseudocoder', got %q", path)
	}

	// Should end with approval.token
	if !strings.HasSuffix(path, "approval.token") {
		t.Errorf("expected path to end with 'approval.token', got %q", path)
	}
}

// TestEnsureToken_CreatesNewToken verifies that a new token is created when none exists.
func TestEnsureToken_CreatesNewToken(t *testing.T) {
	// Create a temp directory for the test
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, ".pseudocoder", "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	token, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	// Token should be 64 hex characters (32 bytes)
	if len(token) != 64 {
		t.Errorf("expected token length 64, got %d", len(token))
	}

	// Token should be valid hex
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("token contains invalid character: %c", c)
		}
	}

	// File should exist
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		t.Error("token file was not created")
	}
}

// TestEnsureToken_LoadsExisting verifies that an existing token is loaded without regenerating.
func TestEnsureToken_LoadsExisting(t *testing.T) {
	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	// Create the directory and write an existing token
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	existingToken := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := os.WriteFile(tokenPath, []byte(existingToken), 0600); err != nil {
		t.Fatalf("failed to write existing token: %v", err)
	}

	manager := NewApprovalTokenManager(tokenPath)
	token, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	// Should load the existing token, not generate a new one
	if token != existingToken {
		t.Errorf("expected %q, got %q", existingToken, token)
	}
}

// TestEnsureToken_HandlesWhitespace verifies that whitespace in token file is trimmed.
func TestEnsureToken_HandlesWhitespace(t *testing.T) {
	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	// Create the directory and write a token with whitespace
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	existingToken := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := os.WriteFile(tokenPath, []byte(existingToken+"\n"), 0600); err != nil {
		t.Fatalf("failed to write existing token: %v", err)
	}

	manager := NewApprovalTokenManager(tokenPath)
	token, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	// Should trim whitespace
	if token != existingToken {
		t.Errorf("expected %q, got %q", existingToken, token)
	}
}

// TestEnsureToken_RegeneratesEmpty verifies that an empty token file triggers regeneration.
func TestEnsureToken_RegeneratesEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	// Create the directory and write an empty file
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte(""), 0600); err != nil {
		t.Fatalf("failed to write empty token: %v", err)
	}

	manager := NewApprovalTokenManager(tokenPath)
	token, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	// Should generate a new token (64 hex chars)
	if len(token) != 64 {
		t.Errorf("expected token length 64, got %d", len(token))
	}
}

// TestEnsureToken_FilePermissions verifies that the token file has correct permissions.
func TestEnsureToken_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, ".pseudocoder", "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("failed to stat token file: %v", err)
	}

	// File permissions should be 0600 (owner read/write only)
	// On some systems, umask may affect this, so we check the permissions
	// are at most 0600 (no group or other permissions)
	mode := info.Mode().Perm()
	if mode&0077 != 0 {
		t.Errorf("expected no group/other permissions, got %o", mode)
	}
}

// TestEnsureToken_DirectoryPermissions verifies that the parent directory has correct permissions.
func TestEnsureToken_DirectoryPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	info, err := os.Stat(tokenDir)
	if err != nil {
		t.Fatalf("failed to stat token directory: %v", err)
	}

	// Directory permissions should be 0700 (owner only)
	mode := info.Mode().Perm()
	if mode&0077 != 0 {
		t.Errorf("expected no group/other permissions on directory, got %o", mode)
	}
}

// TestValidateToken_Success verifies that the correct token validates.
func TestValidateToken_Success(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	token, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	if !manager.ValidateToken(token) {
		t.Error("ValidateToken() returned false for correct token")
	}
}

// TestValidateToken_Failure verifies that an incorrect token is rejected.
func TestValidateToken_Failure(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	if manager.ValidateToken("wrong_token") {
		t.Error("ValidateToken() returned true for incorrect token")
	}
}

// TestValidateToken_EmptyToken verifies that an empty token is rejected.
func TestValidateToken_EmptyToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}

	if manager.ValidateToken("") {
		t.Error("ValidateToken() returned true for empty token")
	}
}

// TestValidateToken_BeforeEnsure verifies that ValidateToken returns false before EnsureToken.
func TestValidateToken_BeforeEnsure(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)

	// Don't call EnsureToken()
	if manager.ValidateToken("any_token") {
		t.Error("ValidateToken() returned true before EnsureToken()")
	}
}

// TestTokenPath verifies that TokenPath returns the correct path.
func TestTokenPath(t *testing.T) {
	path := "/test/path/approval.token"
	manager := NewApprovalTokenManager(path)

	if manager.TokenPath() != path {
		t.Errorf("expected %q, got %q", path, manager.TokenPath())
	}
}

// TestEnsureToken_MultipleCalls verifies that calling EnsureToken multiple times returns the same token.
func TestEnsureToken_MultipleCalls(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "approval.token")

	manager := NewApprovalTokenManager(tokenPath)
	token1, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("first EnsureToken() error: %v", err)
	}

	token2, err := manager.EnsureToken()
	if err != nil {
		t.Fatalf("second EnsureToken() error: %v", err)
	}

	if token1 != token2 {
		t.Errorf("multiple EnsureToken() calls returned different tokens: %q vs %q", token1, token2)
	}
}

// TestEnsureToken_UnreadableFile verifies that EnsureToken returns an error for unreadable token files.
func TestEnsureToken_UnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping permission test on Windows")
	}

	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	// Create the directory and write a token file
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	existingToken := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := os.WriteFile(tokenPath, []byte(existingToken), 0600); err != nil {
		t.Fatalf("failed to write token: %v", err)
	}

	// Make the file unreadable (write-only)
	if err := os.Chmod(tokenPath, 0200); err != nil {
		t.Fatalf("failed to chmod token file: %v", err)
	}
	// Restore permissions on cleanup so t.TempDir() can remove it
	t.Cleanup(func() {
		os.Chmod(tokenPath, 0600)
	})

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()

	// Should return an error because file is unreadable
	if err == nil {
		t.Error("EnsureToken() should return error for unreadable file")
	}
}

// TestEnsureToken_UnwritableDirectory verifies that EnsureToken returns an error when directory is not writable.
func TestEnsureToken_UnwritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping permission test on Windows")
	}

	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, ".pseudocoder")
	tokenPath := filepath.Join(tokenDir, "approval.token")

	// Create the parent directory but make it unwritable
	if err := os.MkdirAll(tokenDir, 0500); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	// Restore permissions on cleanup
	t.Cleanup(func() {
		os.Chmod(tokenDir, 0700)
	})

	manager := NewApprovalTokenManager(tokenPath)
	_, err := manager.EnsureToken()

	// Should return an error because directory is not writable
	if err == nil {
		t.Error("EnsureToken() should return error for unwritable directory")
	}
}
