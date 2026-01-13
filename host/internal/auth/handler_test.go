package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPairHandlerSuccess tests successful pairing.
func TestPairHandlerSuccess(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	// Generate a pairing code first
	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Create request
	body, _ := json.Marshal(PairRequest{
		Code:       code,
		DeviceName: "Test iPhone",
	})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Should succeed
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Parse response
	var resp PairResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.DeviceID == "" {
		t.Error("expected non-empty device ID")
	}
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
}

// TestPairHandlerInvalidCode tests pairing with wrong code.
func TestPairHandlerInvalidCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	// Generate a pairing code but use a different one
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Create request with wrong code
	body, _ := json.Marshal(PairRequest{
		Code:       "000000",
		DeviceName: "Test iPhone",
	})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Should fail with 401
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d: %s", w.Code, w.Body.String())
	}

	// Parse error response
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "invalid_code" {
		t.Errorf("expected error code 'invalid_code', got '%s'", resp.Error)
	}
}

// TestPairHandlerMissingCode tests pairing without a code.
func TestPairHandlerMissingCode(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	// Create request without code
	body, _ := json.Marshal(PairRequest{
		DeviceName: "Test iPhone",
	})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Should fail with 400
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "missing_code" {
		t.Errorf("expected error code 'missing_code', got '%s'", resp.Error)
	}
}

// TestPairHandlerMethodNotAllowed tests that only POST is accepted.
func TestPairHandlerMethodNotAllowed(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete}
	for _, method := range methods {
		req := httptest.NewRequest(method, "/pair", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected status 405, got %d", method, w.Code)
		}
	}
}

// TestPairHandlerInvalidJSON tests pairing with malformed JSON.
func TestPairHandlerInvalidJSON(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "invalid_request" {
		t.Errorf("expected error code 'invalid_request', got '%s'", resp.Error)
	}
}

// TestPairHandlerRateLimited tests rate limiting response.
func TestPairHandlerRateLimited(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore:          store,
		MaxAttemptsPerMinute: 2,
	})
	handler := NewPairHandler(pm)

	// Generate a code
	_, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Use up the rate limit with wrong codes
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(PairRequest{Code: "000000"})
		req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// Next request should be rate limited
	body, _ := json.Marshal(PairRequest{Code: "000000"})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", w.Code)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "rate_limited" {
		t.Errorf("expected error code 'rate_limited', got '%s'", resp.Error)
	}
}

// TestGenerateCodeHandler tests the /pair/generate endpoint from loopback.
func TestGenerateCodeHandler(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewGenerateCodeHandler(pm)

	req := httptest.NewRequest(http.MethodPost, "/pair/generate", nil)
	// Simulate request from loopback address
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp GenerateCodeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Code) != 6 {
		t.Errorf("expected 6-digit code, got %d digits", len(resp.Code))
	}

	if resp.Expiry.IsZero() {
		t.Error("expected non-zero expiry time")
	}
}

// TestGenerateCodeHandlerMethodNotAllowed tests that only POST is accepted.
func TestGenerateCodeHandlerMethodNotAllowed(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewGenerateCodeHandler(pm)

	req := httptest.NewRequest(http.MethodGet, "/pair/generate", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

// TestGenerateCodeHandlerLoopbackOnly tests that /pair/generate rejects non-loopback requests.
func TestGenerateCodeHandlerLoopbackOnly(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewGenerateCodeHandler(pm)

	// Test various non-loopback addresses that should be rejected
	nonLoopbackAddrs := []string{
		"192.168.1.100:54321",  // Private IPv4
		"10.0.0.5:54321",       // Private IPv4
		"172.16.0.1:54321",     // Private IPv4
		"8.8.8.8:54321",        // Public IPv4
		"[2001:db8::1]:54321",  // Public IPv6
		"[fe80::1]:54321",      // Link-local IPv6
	}

	for _, addr := range nonLoopbackAddrs {
		req := httptest.NewRequest(http.MethodPost, "/pair/generate", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("RemoteAddr=%s: expected status 403, got %d", addr, w.Code)
		}

		var resp ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("RemoteAddr=%s: failed to decode response: %v", addr, err)
		}

		if resp.Error != "forbidden" {
			t.Errorf("RemoteAddr=%s: expected error 'forbidden', got '%s'", addr, resp.Error)
		}
	}
}

// TestGenerateCodeHandlerLoopbackAccepted tests that loopback addresses are accepted.
func TestGenerateCodeHandlerLoopbackAccepted(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewGenerateCodeHandler(pm)

	// Test various loopback addresses that should be accepted
	loopbackAddrs := []string{
		"127.0.0.1:54321",      // Standard IPv4 loopback
		"127.0.0.2:54321",      // Other 127.x.x.x address
		"127.255.255.1:54321",  // Another 127.x.x.x address
		"[::1]:54321",          // IPv6 loopback
	}

	for _, addr := range loopbackAddrs {
		req := httptest.NewRequest(http.MethodPost, "/pair/generate", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("RemoteAddr=%s: expected status 200, got %d: %s", addr, w.Code, w.Body.String())
		}
	}
}

// TestIsLoopbackRequest tests the loopback detection function.
func TestIsLoopbackRequest(t *testing.T) {
	tests := []struct {
		remoteAddr string
		expected   bool
	}{
		// Loopback addresses (should return true)
		{"127.0.0.1:54321", true},
		{"127.0.0.2:54321", true},
		{"127.255.255.255:54321", true},
		{"[::1]:54321", true},

		// Non-loopback addresses (should return false)
		{"192.168.1.1:54321", false},
		{"10.0.0.1:54321", false},
		{"172.16.0.1:54321", false},
		{"8.8.8.8:54321", false},
		{"0.0.0.0:54321", false},
		{"[2001:db8::1]:54321", false},
		{"[fe80::1]:54321", false},
		{"[::]:54321", false},

		// Edge cases (should return false)
		{"", false},
		{"invalid", false},
		{"no-port", false},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.RemoteAddr = tt.remoteAddr

		result := isLoopbackRequest(req)
		if result != tt.expected {
			t.Errorf("isLoopbackRequest(%q) = %v, expected %v", tt.remoteAddr, result, tt.expected)
		}
	}
}

// TestPairHandlerDefaultDeviceName tests default device name.
func TestPairHandlerDefaultDeviceName(t *testing.T) {
	store := newMockDeviceStore()
	pm := NewPairingManager(PairingConfig{
		DeviceStore: store,
	})
	handler := NewPairHandler(pm)

	// Generate a pairing code
	code, err := pm.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	// Create request without device name
	body, _ := json.Marshal(PairRequest{
		Code: code,
	})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Check that device has default name
	devices, _ := store.ListDevices()
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].Name != "Unknown Device" {
		t.Errorf("expected default name 'Unknown Device', got '%s'", devices[0].Name)
	}
}
