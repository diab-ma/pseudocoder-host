package auth

import (
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TestTokenValidator verifies token validation works correctly.
func TestTokenValidator(t *testing.T) {
	store := newMockDeviceStore()

	// Create a device with a known token
	token := "test-token-12345"
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash failed: %v", err)
	}

	device := &Device{
		ID:        "device-1",
		Name:      "Test Device",
		TokenHash: string(hash),
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	store.SaveDevice(device)

	validator := NewTokenValidator(store)

	// Valid token should work
	result, err := validator.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken failed: %v", err)
	}
	if result.ID != "device-1" {
		t.Errorf("expected device ID 'device-1', got '%s'", result.ID)
	}
	if result.Name != "Test Device" {
		t.Errorf("expected device name 'Test Device', got '%s'", result.Name)
	}
}

// TestTokenValidatorInvalidToken verifies invalid tokens are rejected.
func TestTokenValidatorInvalidToken(t *testing.T) {
	store := newMockDeviceStore()

	// Create a device with a known token
	token := "correct-token"
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash failed: %v", err)
	}

	device := &Device{
		ID:        "device-1",
		Name:      "Test Device",
		TokenHash: string(hash),
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	store.SaveDevice(device)

	validator := NewTokenValidator(store)

	// Wrong token should fail
	_, err = validator.ValidateToken("wrong-token")
	if err != ErrDeviceNotFound {
		t.Errorf("expected ErrDeviceNotFound for invalid token, got %v", err)
	}
}

// TestTokenValidatorNoDevices verifies behavior with empty store.
func TestTokenValidatorNoDevices(t *testing.T) {
	store := newMockDeviceStore()
	validator := NewTokenValidator(store)

	_, err := validator.ValidateToken("any-token")
	if err != ErrDeviceNotFound {
		t.Errorf("expected ErrDeviceNotFound with no devices, got %v", err)
	}
}

// TestTokenValidatorMultipleDevices verifies correct device is found.
func TestTokenValidatorMultipleDevices(t *testing.T) {
	store := newMockDeviceStore()

	// Create multiple devices
	tokens := []string{"token-a", "token-b", "token-c"}
	deviceIDs := []string{"device-a", "device-b", "device-c"}
	deviceNames := []string{"Device A", "Device B", "Device C"}

	for i, token := range tokens {
		hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt hash failed: %v", err)
		}

		device := &Device{
			ID:        deviceIDs[i],
			Name:      deviceNames[i],
			TokenHash: string(hash),
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}
		store.SaveDevice(device)
	}

	validator := NewTokenValidator(store)

	// Each token should find the correct device
	for i, token := range tokens {
		result, err := validator.ValidateToken(token)
		if err != nil {
			t.Fatalf("ValidateToken for token %d failed: %v", i, err)
		}
		if result.ID != deviceIDs[i] {
			t.Errorf("token %d: expected device '%s', got '%s'", i, deviceIDs[i], result.ID)
		}
	}
}

// TestValidateDeviceID verifies device ID lookup.
func TestValidateDeviceID(t *testing.T) {
	store := newMockDeviceStore()

	device := &Device{
		ID:        "device-1",
		Name:      "Test Device",
		TokenHash: "hash",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	store.SaveDevice(device)

	validator := NewTokenValidator(store)

	// Existing device should be found
	result, err := validator.ValidateDeviceID("device-1")
	if err != nil {
		t.Fatalf("ValidateDeviceID failed: %v", err)
	}
	if result.ID != "device-1" {
		t.Errorf("expected device ID 'device-1', got '%s'", result.ID)
	}

	// Non-existent device should return error
	_, err = validator.ValidateDeviceID("device-999")
	if err != ErrDeviceNotFound {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}
}
