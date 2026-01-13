//go:build perf
// +build perf

package auth

// Token validation performance tests.
// These tests verify the O(n) bcrypt validation behavior documented in docs/PLANS.md.
//
// The current implementation does a linear scan of all devices and calls
// bcrypt.CompareHashAndPassword for each device until a match is found.
// This is acceptable for the single-user MVP scale but should be monitored.
//
// Run these tests with: go test -v -run 'TokenValidation' ./internal/auth/

import (
	"fmt"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

// Performance thresholds for token validation.
// These define acceptable limits for single-user MVP scale.
const (
	// Token validation with 100 devices should complete within 10 seconds.
	// This is generous: 100 devices x ~100ms bcrypt = ~10s worst case.
	maxTokenValidation100Devices = 10 * time.Second

	// Individual bcrypt compare should be under 200ms on typical hardware.
	// bcrypt with default cost is designed to be slow for security.
	maxBcryptCompareTime = 200 * time.Millisecond
)

// TestTokenValidation100Devices verifies performance with 100 paired devices.
// This tests the O(n) behavior: worst case is validating the last device's token.
func TestTokenValidation100Devices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow bcrypt test in short mode")
	}
	store := newMockDeviceStore()

	const numDevices = 100
	tokens := make([]string, numDevices)
	deviceIDs := make([]string, numDevices)

	// Pre-generate all devices with bcrypt hashes.
	// This is slow by design (bcrypt), so do it before timing measurements.
	t.Log("creating 100 devices with bcrypt hashes (this may take a minute)...")
	setupStart := time.Now()

	for i := 0; i < numDevices; i++ {
		token := fmt.Sprintf("token-%03d-secret", i)
		tokens[i] = token
		deviceIDs[i] = fmt.Sprintf("device-%03d", i)

		hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt hash failed for device %d: %v", i, err)
		}

		device := &storage.Device{
			ID:        deviceIDs[i],
			Name:      fmt.Sprintf("Device %d", i),
			TokenHash: string(hash),
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}
		store.SaveDevice(device)
	}
	t.Logf("setup completed in %v", time.Since(setupStart))

	validator := NewTokenValidator(store)

	// Test cases: first device (best case), middle device, last device (worst case)
	// Note: bcrypt takes ~50-100ms per compare depending on hardware and system load.
	// Thresholds are set with generous margins for CI/CD variability.
	testCases := []struct {
		name      string
		tokenIdx  int
		maxTime   time.Duration
		expectOps int // Expected number of bcrypt compares (worst case)
	}{
		{"first device (best case)", 0, 5 * time.Second, 1},   // Single compare + overhead + CI margin
		{"middle device (average)", 50, 8 * time.Second, 51},  // ~51 compares + CI margin
		{"last device (worst case)", 99, maxTokenValidation100Devices, 100}, // 100 compares
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			token := tokens[tc.tokenIdx]
			expectedID := deviceIDs[tc.tokenIdx]

			start := time.Now()
			result, err := validator.ValidateToken(token)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("ValidateToken failed: %v", err)
			}
			if result.ID != expectedID {
				t.Errorf("expected device ID %q, got %q", expectedID, result.ID)
			}

			if elapsed > tc.maxTime {
				t.Errorf("validation took %v, want < %v", elapsed, tc.maxTime)
			}
			t.Logf("validated %s in %v (expected up to %d bcrypt compares)",
				tc.name, elapsed, tc.expectOps)
		})
	}
}

// TestTokenValidationScaling measures how validation time scales with device count.
// Documents the O(n) behavior for future optimization decisions.
func TestTokenValidationScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow bcrypt test in short mode")
	}
	testCases := []struct {
		numDevices int
		maxTime    time.Duration
	}{
		{10, 2 * time.Second},
		{50, 6 * time.Second},
		{100, maxTokenValidation100Devices},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%d devices", tc.numDevices), func(t *testing.T) {
			store := newMockDeviceStore()
			tokens := make([]string, tc.numDevices)

			// Generate devices
			for i := 0; i < tc.numDevices; i++ {
				token := fmt.Sprintf("token-%03d-secret", i)
				tokens[i] = token

				hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
				if err != nil {
					t.Fatalf("bcrypt hash failed: %v", err)
				}

				device := &storage.Device{
					ID:        fmt.Sprintf("device-%03d", i),
					Name:      fmt.Sprintf("Device %d", i),
					TokenHash: string(hash),
					CreatedAt: time.Now(),
					LastSeen:  time.Now(),
				}
				store.SaveDevice(device)
			}

			validator := NewTokenValidator(store)

			// Measure worst-case: validate the last device's token
			lastToken := tokens[tc.numDevices-1]
			start := time.Now()
			_, err := validator.ValidateToken(lastToken)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("ValidateToken failed: %v", err)
			}

			if elapsed > tc.maxTime {
				t.Errorf("worst-case validation took %v, want < %v", elapsed, tc.maxTime)
			}

			// Calculate average time per bcrypt compare
			avgPerCompare := elapsed / time.Duration(tc.numDevices)
			t.Logf("%d devices: worst-case %v (~%v per bcrypt compare)",
				tc.numDevices, elapsed, avgPerCompare)
		})
	}
}

// TestTokenValidationInvalidTokenScaling measures performance for invalid tokens.
// Invalid tokens must compare against ALL devices, so this is always O(n).
func TestTokenValidationInvalidTokenScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow bcrypt test in short mode")
	}
	store := newMockDeviceStore()

	const numDevices = 50 // Use 50 to keep test time reasonable

	// Generate devices
	for i := 0; i < numDevices; i++ {
		hash, err := bcrypt.GenerateFromPassword([]byte(fmt.Sprintf("token-%03d", i)), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt hash failed: %v", err)
		}

		device := &storage.Device{
			ID:        fmt.Sprintf("device-%03d", i),
			Name:      fmt.Sprintf("Device %d", i),
			TokenHash: string(hash),
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}
		store.SaveDevice(device)
	}

	validator := NewTokenValidator(store)

	// Invalid token must check ALL devices
	start := time.Now()
	_, err := validator.ValidateToken("invalid-token-that-wont-match")
	elapsed := time.Since(start)

	if err != ErrDeviceNotFound {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}

	// Should take roughly numDevices * bcryptTime
	// Allow generous margin for CI variance
	maxExpected := 8 * time.Second // ~50 devices x ~100ms + margin
	if elapsed > maxExpected {
		t.Errorf("invalid token check took %v, want < %v", elapsed, maxExpected)
	}

	avgPerCompare := elapsed / numDevices
	t.Logf("invalid token with %d devices: %v total (~%v per bcrypt compare)",
		numDevices, elapsed, avgPerCompare)
}

// TestBcryptCompareBaseline measures baseline bcrypt compare time.
// This helps calibrate expectations for the O(n) scaling tests.
func TestBcryptCompareBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow bcrypt test in short mode")
	}
	// Generate a hash with default cost
	token := "test-token-for-baseline"
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash failed: %v", err)
	}

	// Measure successful compare
	start := time.Now()
	err = bcrypt.CompareHashAndPassword(hash, []byte(token))
	successElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("bcrypt compare failed: %v", err)
	}

	// Measure failed compare (slightly faster in some implementations)
	start = time.Now()
	_ = bcrypt.CompareHashAndPassword(hash, []byte("wrong-token"))
	failedElapsed := time.Since(start)

	if successElapsed > maxBcryptCompareTime {
		t.Errorf("successful bcrypt compare took %v, want < %v", successElapsed, maxBcryptCompareTime)
	}

	t.Logf("bcrypt baseline: success=%v, failed=%v", successElapsed, failedElapsed)
}

// TestTokenValidationConcurrentAccess verifies thread safety under load.
// Multiple goroutines validate tokens simultaneously.
func TestTokenValidationConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow bcrypt test in short mode")
	}
	store := newMockDeviceStore()

	const numDevices = 10
	tokens := make([]string, numDevices)

	// Generate devices
	for i := 0; i < numDevices; i++ {
		token := fmt.Sprintf("token-%03d-secret", i)
		tokens[i] = token

		hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt hash failed: %v", err)
		}

		device := &storage.Device{
			ID:        fmt.Sprintf("device-%03d", i),
			Name:      fmt.Sprintf("Device %d", i),
			TokenHash: string(hash),
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}
		store.SaveDevice(device)
	}

	validator := NewTokenValidator(store)

	// Launch concurrent validation requests
	const numGoroutines = 20
	done := make(chan error, numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(idx int) {
			tokenIdx := idx % numDevices
			_, err := validator.ValidateToken(tokens[tokenIdx])
			done <- err
		}(g)
	}

	// Collect results
	var errors int
	for i := 0; i < numGoroutines; i++ {
		if err := <-done; err != nil {
			t.Logf("goroutine error: %v", err)
			errors++
		}
	}

	if errors > 0 {
		t.Errorf("%d errors during concurrent validation", errors)
	}
	t.Logf("completed %d concurrent validations successfully", numGoroutines)
}
