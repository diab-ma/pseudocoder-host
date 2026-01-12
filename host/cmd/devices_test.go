package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// TestFormatDuration verifies the human-readable duration formatting.
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "just now"},
		{30 * time.Second, "just now"},
		{59 * time.Second, "just now"},
		{60 * time.Second, "1m ago"},
		{5 * time.Minute, "5m ago"},
		{59 * time.Minute, "59m ago"},
		{60 * time.Minute, "1h ago"},
		{2 * time.Hour, "2h ago"},
		{23 * time.Hour, "23h ago"},
		{24 * time.Hour, "1d ago"},
		{48 * time.Hour, "2d ago"},
		{-5 * time.Minute, "in the future"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestDevicesListWithDevices verifies listing devices from a database.
func TestDevicesListWithDevices(t *testing.T) {
	// Create a temp database with devices
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// Add test devices
	now := time.Now()
	device1 := &storage.Device{
		ID:        "device-001",
		Name:      "Test iPhone",
		TokenHash: "hash1",
		CreatedAt: now.Add(-24 * time.Hour),
		LastSeen:  now.Add(-5 * time.Minute),
	}
	device2 := &storage.Device{
		ID:        "device-002",
		Name:      "Test iPad",
		TokenHash: "hash2",
		CreatedAt: now.Add(-48 * time.Hour),
		LastSeen:  now.Add(-2 * time.Hour),
	}

	if err := store.SaveDevice(device1); err != nil {
		t.Fatalf("failed to save device1: %v", err)
	}
	if err := store.SaveDevice(device2); err != nil {
		t.Fatalf("failed to save device2: %v", err)
	}
	store.Close()

	// Run devices list
	var stdout, stderr bytes.Buffer
	code := runDevicesList([]string{"--token-store", dbPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d. stderr: %s", code, stderr.String())
	}

	output := stdout.String()

	// Verify output contains expected device info
	if !strings.Contains(output, "device-001") {
		t.Errorf("output should contain device-001, got %q", output)
	}
	if !strings.Contains(output, "device-002") {
		t.Errorf("output should contain device-002, got %q", output)
	}
	if !strings.Contains(output, "Test iPhone") {
		t.Errorf("output should contain 'Test iPhone', got %q", output)
	}
	if !strings.Contains(output, "Test iPad") {
		t.Errorf("output should contain 'Test iPad', got %q", output)
	}
	if !strings.Contains(output, "DEVICE ID") {
		t.Errorf("output should contain header 'DEVICE ID', got %q", output)
	}
}

// TestDevicesListEmptyDatabase verifies listing when database exists but is empty.
func TestDevicesListEmptyDatabase(t *testing.T) {
	// Create an empty database
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "empty.db")

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runDevicesList([]string{"--token-store", dbPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d. stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "No paired devices found") {
		t.Errorf("expected 'No paired devices found', got %q", stdout.String())
	}
}

// TestDevicesRevokeExistingDevice verifies revoking an existing device.
func TestDevicesRevokeExistingDevice(t *testing.T) {
	// Create a database with a device
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	device := &storage.Device{
		ID:        "device-to-revoke",
		Name:      "Revokable Device",
		TokenHash: "hash123",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("failed to save device: %v", err)
	}
	store.Close()

	// Revoke the device
	var stdout, stderr bytes.Buffer
	code := runDevicesRevoke([]string{"--token-store", dbPath, "device-to-revoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d. stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Revoked device") {
		t.Errorf("expected 'Revoked device' in output, got %q", output)
	}
	if !strings.Contains(output, "device-to-revoke") {
		t.Errorf("expected device ID in output, got %q", output)
	}
	if !strings.Contains(output, "Revokable Device") {
		t.Errorf("expected device name in output, got %q", output)
	}

	// Verify device is gone
	store2, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer store2.Close()

	device, err = store2.GetDevice("device-to-revoke")
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if device != nil {
		t.Error("device should be deleted after revoke")
	}
}

// TestDevicesRevokeNonexistentDevice verifies error when device doesn't exist.
func TestDevicesRevokeNonexistentDevice(t *testing.T) {
	// Create an empty database
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runDevicesRevoke([]string{"--token-store", dbPath, "nonexistent-device"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}

	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("expected 'not found' error, got %q", stderr.String())
	}
}

// TestGetDefaultTokenStorePath verifies the default path construction.
func TestGetDefaultTokenStorePath(t *testing.T) {
	path, err := getDefaultTokenStorePath()
	if err != nil {
		t.Fatalf("getDefaultTokenStorePath failed: %v", err)
	}

	if path == "" {
		t.Error("expected non-empty path")
	}
	if !strings.Contains(path, ".pseudocoder") {
		t.Errorf("expected path to contain '.pseudocoder', got %q", path)
	}
	if !strings.Contains(path, "pseudocoder.db") {
		t.Errorf("expected path to contain 'pseudocoder.db', got %q", path)
	}
}
