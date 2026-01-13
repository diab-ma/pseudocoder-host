package auth

import (
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// mockDeviceStore is a simple in-memory device store for testing.
type mockDeviceStore struct {
	mu      sync.RWMutex
	devices map[string]*storage.Device
}

func newMockDeviceStore() *mockDeviceStore {
	return &mockDeviceStore{
		devices: make(map[string]*storage.Device),
	}
}

func (s *mockDeviceStore) SaveDevice(device *storage.Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[device.ID] = device
	return nil
}

func (s *mockDeviceStore) GetDevice(id string) (*storage.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devices[id], nil
}

func (s *mockDeviceStore) GetDeviceByTokenHash(hash string) (*storage.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.devices {
		if d.TokenHash == hash {
			return d, nil
		}
	}
	return nil, nil
}

func (s *mockDeviceStore) ListDevices() ([]*storage.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*storage.Device
	for _, d := range s.devices {
		result = append(result, d)
	}
	return result, nil
}

func (s *mockDeviceStore) DeleteDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
	return nil
}

func (s *mockDeviceStore) UpdateLastSeen(id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[id]; ok {
		d.LastSeen = t
		return nil
	}
	return storage.ErrDeviceNotFound
}

// TestGenerateCode verifies that pairing codes are generated correctly.
func TestGenerateCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Code should be 6 digits
	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %d digits", len(code))
	}

	// All characters should be digits
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("expected digits only, found %c", c)
		}
	}
}

// TestCodeRandomness verifies that generated codes are different.
func TestCodeRandomness(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	codes := make(map[string]bool)
	for i := 0; i < 100; i++ {
		code, err := pm.GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode failed: %v", err)
		}
		codes[code] = true
	}

	// We should have mostly unique codes (allow some collisions)
	if len(codes) < 90 {
		t.Errorf("expected mostly unique codes, got only %d unique out of 100", len(codes))
	}
}

// TestHasActiveCode checks the active code detection.
func TestHasActiveCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	// No active code initially
	if pm.HasActiveCode() {
		t.Error("expected no active code initially")
	}

	// Generate a code
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Now there should be an active code
	if !pm.HasActiveCode() {
		t.Error("expected active code after generation")
	}
}

// TestCodeExpiry verifies that codes expire correctly.
func TestCodeExpiry(t *testing.T) {
	store := newMockDeviceStore()

	// Use a very short expiry for testing
	currentTime := time.Now()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
		CodeExpiry:  100 * time.Millisecond,
		TimeNow: func() time.Time {
			return currentTime
		},
	})

	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Code should be valid immediately
	if !pm.HasActiveCode() {
		t.Error("expected active code immediately after generation")
	}

	// Advance time past expiry
	currentTime = currentTime.Add(200 * time.Millisecond)

	// Code should now be expired
	if pm.HasActiveCode() {
		t.Error("expected code to be expired")
	}

	// Validation should fail with ErrCodeExpired
	_, _, err = pm.ValidateCode(code, "Test Device")
	if err != ErrCodeExpired {
		t.Errorf("expected ErrCodeExpired, got %v", err)
	}
}

// TestValidateCode verifies successful code validation and device creation.
func TestValidateCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	deviceID, token, err := pm.ValidateCode(code, "Test iPhone")
	if err != nil {
		t.Fatalf("ValidateCode failed: %v", err)
	}

	// Should have a device ID
	if deviceID == "" {
		t.Error("expected non-empty device ID")
	}

	// Should have a token
	if token == "" {
		t.Error("expected non-empty token")
	}

	// Token should be long enough for security
	if len(token) < 32 {
		t.Errorf("expected token length >= 32, got %d", len(token))
	}

	// Device should be stored
	device, err := store.GetDevice(deviceID)
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if device == nil {
		t.Fatal("expected device to be stored")
	}
	if device.Name != "Test iPhone" {
		t.Errorf("expected device name 'Test iPhone', got '%s'", device.Name)
	}
}

// TestReplayPrevention verifies that codes cannot be reused.
func TestReplayPrevention(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// First use should succeed
	_, _, err = pm.ValidateCode(code, "Device 1")
	if err != nil {
		t.Fatalf("first ValidateCode failed: %v", err)
	}

	// Second use should fail with ErrCodeUsed
	_, _, err = pm.ValidateCode(code, "Device 2")
	if err != ErrCodeUsed {
		t.Errorf("expected ErrCodeUsed on replay, got %v", err)
	}
}

// TestInvalidCode verifies that wrong codes are rejected.
func TestInvalidCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	// Generate a code but try to validate a different one
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	_, _, err = pm.ValidateCode("000000", "Test Device")
	if err != ErrCodeInvalid {
		t.Errorf("expected ErrCodeInvalid, got %v", err)
	}
}

// TestNoActiveCode verifies error when no code exists.
func TestNoActiveCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	// Don't generate a code, try to validate
	_, _, err := pm.ValidateCode("123456", "Test Device")
	if err != ErrCodeInvalid {
		t.Errorf("expected ErrCodeInvalid with no active code, got %v", err)
	}
}

// TestRateLimiting verifies that rate limiting works.
func TestRateLimiting(t *testing.T) {
	store := newMockDeviceStore()

	currentTime := time.Now()
	pm := NewPairingManager(PairingConfig{
		DeviceStore:          store,
		MaxAttemptsPerMinute: 3, // Low limit for testing
		TimeNow: func() time.Time {
			return currentTime
		},
	})

	// Generate a code
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// First 3 attempts should work (but fail due to wrong code)
	for i := 0; i < 3; i++ {
		_, _, err = pm.ValidateCode("000000", "Device")
		if err == ErrRateLimited {
			t.Errorf("attempt %d was rate limited too early", i+1)
		}
	}

	// 4th attempt should be rate limited
	_, _, err = pm.ValidateCode("000000", "Device")
	if err != ErrRateLimited {
		t.Errorf("expected ErrRateLimited after exceeding limit, got %v", err)
	}

	// Advance time by 1 minute to clear rate limit
	currentTime = currentTime.Add(61 * time.Second)

	// Generate a new code (old one was consumed by attempts)
	_, err = pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode after rate limit reset failed: %v", err)
	}

	// Should be able to try again
	_, _, err = pm.ValidateCode("000000", "Device")
	if err == ErrRateLimited {
		t.Error("rate limit should have reset after 1 minute")
	}
}

// TestNewCodeInvalidatesOld verifies that generating a new code invalidates the old one.
func TestNewCodeInvalidatesOld(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	code1, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode 1 failed: %v", err)
	}

	code2, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode 2 failed: %v", err)
	}

	// Old code should be invalid
	_, _, err = pm.ValidateCode(code1, "Device")
	if err != ErrCodeInvalid {
		t.Errorf("expected ErrCodeInvalid for old code, got %v", err)
	}

	// New code should be valid
	_, _, err = pm.ValidateCode(code2, "Device")
	if err != nil {
		t.Errorf("new code should be valid, got %v", err)
	}
}

// TestGetCodeExpiry verifies the expiry time is returned correctly.
func TestGetCodeExpiry(t *testing.T) {
	store := newMockDeviceStore()

	currentTime := time.Now()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
		CodeExpiry:  5 * time.Minute,
		TimeNow: func() time.Time {
			return currentTime
		},
	})

	// No active code initially
	expiry := pm.GetCodeExpiry()
	if !expiry.IsZero() {
		t.Error("expected zero time when no active code")
	}

	// Generate a code
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Expiry should be set
	expiry = pm.GetCodeExpiry()
	expected := currentTime.Add(5 * time.Minute)
	if !expiry.Equal(expected) {
		t.Errorf("expected expiry %v, got %v", expected, expiry)
	}
}

// =============================================================================
// Unit 7.11: Pairing and Auth Edge Cases
// =============================================================================

// TestDeviceNameEdgeCases_Pairing tests edge cases for device names during pairing.
// This complements the storage-level tests in sqlite_test.go by exercising the full
// pairing flow with unusual device names.
func TestDeviceNameEdgeCases_Pairing(t *testing.T) {
	testCases := []struct {
		name       string
		deviceName string
		wantErr    bool
	}{
		{"empty name", "", false},
		{"very long name 10KB", string(make([]byte, 10000)), false},
		{"unicode emoji", "\U0001F4F1\U0001F525\U0001F4BB", false}, // 📱🔥💻
		{"chinese characters", "\u6211\u7684\u624B\u673A", false}, // 我的手机
		{"newlines and tabs", "Device\n\tName", false},
		{"null bytes", "Device\x00Name", false},
		{"mixed unicode", "iPhone\u2019s Pro \u2014 Max", false}, // iPhone's Pro — Max
		{"only whitespace", "   \t\n   ", false},
		{"control characters", "Device\x01\x02\x03", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockDeviceStore()
			pm := NewPairingManager(PairingConfig{
				DeviceStore: store,
			})

			code, err := pm.GenerateCode()
			if err != nil {
				t.Fatalf("GenerateCode failed: %v", err)
			}

			deviceID, token, err := pm.ValidateCode(code, tc.deviceName)

			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("ValidateCode failed: %v", err)
			}

			if deviceID == "" || token == "" {
				t.Error("expected non-empty deviceID and token")
			}

			// Verify device was stored with the name
			device, err := store.GetDevice(deviceID)
			if err != nil {
				t.Fatalf("GetDevice failed: %v", err)
			}
			if device == nil {
				t.Fatal("expected device to be stored")
			}

			// For empty name, it stays empty (handler.go defaults "Unknown Device" at HTTP layer)
			if device.Name != tc.deviceName {
				t.Errorf("device name mismatch: got %q, want %q", device.Name, tc.deviceName)
			}
		})
	}
}

// TestConcurrentPairingAttempts tests multiple clients trying to pair with the same code.
// This verifies that replay prevention works correctly under concurrent access.
func TestConcurrentPairingAttempts(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore:          store,
		MaxAttemptsPerMinute: 100, // High limit to not trigger rate limiting
	})

	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	type result struct {
		deviceID string
		token    string
		err      error
	}
	results := make(chan result, numGoroutines)

	// All goroutines try to validate the same code
	for i := 0; i < numGoroutines; i++ {
		go func(n int) {
			defer wg.Done()
			deviceID, token, err := pm.ValidateCode(code, "Device "+string(rune('A'+n)))
			results <- result{deviceID, token, err}
		}(i)
	}

	wg.Wait()
	close(results)

	// Count successes and failures
	successCount := 0
	codeUsedCount := 0
	for r := range results {
		if r.err == nil {
			successCount++
		} else if r.err == ErrCodeUsed {
			codeUsedCount++
		} else {
			// Other errors are unexpected
			t.Errorf("unexpected error: %v", r.err)
		}
	}

	// Exactly one should succeed
	if successCount != 1 {
		t.Errorf("expected exactly 1 success, got %d", successCount)
	}

	// Rest should get ErrCodeUsed
	if codeUsedCount != numGoroutines-1 {
		t.Errorf("expected %d ErrCodeUsed, got %d", numGoroutines-1, codeUsedCount)
	}

	// Only one device should be stored
	devices, err := store.ListDevices()
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 1 {
		t.Errorf("expected 1 device stored, got %d", len(devices))
	}
}

// TestConcurrentCodeGeneration tests rapid code generation from multiple goroutines.
// This verifies that only one active code exists and no race conditions occur.
func TestConcurrentCodeGeneration(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})

	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	codes := make(chan string, numGoroutines)

	// All goroutines generate codes simultaneously
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			code, err := pm.GenerateCode()
			if err != nil {
				t.Errorf("GenerateCode failed: %v", err)
				return
			}
			codes <- code
		}()
	}

	wg.Wait()
	close(codes)

	// Collect all generated codes
	var allCodes []string
	for code := range codes {
		allCodes = append(allCodes, code)
	}

	if len(allCodes) != numGoroutines {
		t.Fatalf("expected %d codes, got %d", numGoroutines, len(allCodes))
	}

	// Only one code should be valid (the last one to complete)
	// But we can't know which one that is, so just verify exactly one works
	if !pm.HasActiveCode() {
		t.Error("expected an active code after concurrent generation")
	}

	// Try to validate each code - only one should work
	validCount := 0
	for _, code := range allCodes {
		// Generate fresh PM to reset state for each test
		pmTest := NewPairingManager(PairingConfig{
			DeviceStore: newMockDeviceStore(),
		})
		pmTest.GenerateCode() // Generate our own code

		// Now try the original PM
		_, _, err := pm.ValidateCode(code, "Device")
		if err == nil {
			validCount++
		}
	}

	// At most one should have succeeded (might be 0 if all overwrite each other)
	if validCount > 1 {
		t.Errorf("expected at most 1 valid code, got %d", validCount)
	}
}

// TestConcurrentGenerateAndValidate tests concurrent code generation and validation.
// This is a stress test for the pairing manager's mutex protection.
func TestConcurrentGenerateAndValidate(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore:          store,
		MaxAttemptsPerMinute: 1000, // High limit for stress test
	})

	const numGoroutines = 50
	const iterations = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines * 2) // Half generators, half validators

	// Error channel for collecting errors
	errors := make(chan error, numGoroutines*iterations)

	// Generators
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, err := pm.GenerateCode()
				if err != nil {
					errors <- err
				}
			}
		}()
	}

	// Validators
	for i := 0; i < numGoroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Try a random code - most will fail, that's expected
				code := "123456"
				_, _, err := pm.ValidateCode(code, "Device")
				// We only care about unexpected errors (panics would crash)
				// ErrCodeInvalid, ErrCodeUsed, ErrCodeExpired, ErrRateLimited are all OK
				if err != nil && err != ErrCodeInvalid && err != ErrCodeUsed &&
					err != ErrCodeExpired && err != ErrRateLimited {
					errors <- err
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for unexpected errors
	for err := range errors {
		t.Errorf("unexpected error: %v", err)
	}
}
