package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

// failingSaveDeviceStore wraps mockDeviceStore but returns an error from SaveDevice.
// This triggers the internal_error (default) branch in the pair handler.
type failingSaveDeviceStore struct {
	*mockDeviceStore
}

func (s *failingSaveDeviceStore) SaveDevice(device *Device) error {
	return fmt.Errorf("simulated storage failure")
}

// assertTaxonomyResponse verifies error, error_code, and next_action fields
// in a non-200 handler response.
func assertTaxonomyResponse(t *testing.T, w *httptest.ResponseRecorder, expectedStatus int, expectedError, expectedCode string) {
	t.Helper()
	if w.Code != expectedStatus {
		t.Errorf("status: got %d, want %d (%s)", w.Code, expectedStatus, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != expectedError {
		t.Errorf("error: got %q, want %q", resp.Error, expectedError)
	}
	if resp.ErrorCode != expectedCode {
		t.Errorf("error_code: got %q, want %q", resp.ErrorCode, expectedCode)
	}
	if resp.NextAction == "" {
		t.Error("next_action: expected non-empty, got empty")
	}
	if expected := hostErrors.GetNextAction(expectedCode); resp.NextAction != expected {
		t.Errorf("next_action: got %q, want %q", resp.NextAction, expected)
	}
}

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

	// Test various non-local addresses that should be rejected
	nonLoopbackAddrs := []string{
		"192.0.2.10:54321",    // TEST-NET-1
		"198.51.100.5:54321",  // TEST-NET-2
		"203.0.113.20:54321",  // TEST-NET-3
		"8.8.8.8:54321",       // Public IPv4
		"[2001:db8::1]:54321", // Documentation IPv6
		"[2001:db8::2]:54321", // Documentation IPv6
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
		"127.0.0.1:54321",       // Standard IPv4 loopback
		"127.0.0.2:54321",       // Other 127.x.x.x address
		"127.255.255.1:54321",   // Another 127.x.x.x address
		"[::1]:54321",           // IPv6 loopback
		"/tmp/pseudocoder.sock", // Unix socket
		"@pseudocoder.sock",     // Abstract Unix socket
		"",                      // Unnamed Unix socket
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
		{"/tmp/pseudocoder.sock", true},
		{"@pseudocoder.sock", true},
		{"", true},

		// Non-local addresses (should return false)
		{"8.8.8.8:54321", false},
		{"0.0.0.0:54321", false},
		{"[2001:db8::1]:54321", false},
		{"[::]:54321", false},

		// Local interface addresses (should return false)

		// Edge cases (should return false)
		{"invalid", false},
		{"no-port", false},
	}

	if ip := findLocalNonLoopbackIP(); ip != "" {
		tests = append(tests, struct {
			remoteAddr string
			expected   bool
		}{remoteAddr: net.JoinHostPort(ip, "54321"), expected: false})
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

func findLocalNonLoopbackIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
			return ip.String()
		}
	}

	return ""
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

// TestPairHandlerTaxonomyMapping verifies that every /pair non-200 response
// includes the correct error, error_code, and next_action fields per the A3
// taxonomy table.
func TestPairHandlerTaxonomyMapping(t *testing.T) {
	tests := []struct {
		name             string
		method           string
		body             any
		setupCode        bool // whether to generate a code first
		useCorrectCode   bool
		maxAttempts      int // 0 = default
		exhaustAttempts  int // how many wrong-code attempts to make first
		expectedStatus   int
		expectedError    string
		expectedCode     string
		expectedHasNext  bool
	}{
		{
			name:            "method not allowed",
			method:          http.MethodGet,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedError:   "method_not_allowed",
			expectedCode:    hostErrors.CodeAuthPairMethodNotAllowed,
			expectedHasNext: true,
		},
		{
			name:            "missing code",
			method:          http.MethodPost,
			body:            PairRequest{DeviceName: "Test"},
			expectedStatus:  http.StatusBadRequest,
			expectedError:   "missing_code",
			expectedCode:    hostErrors.CodeAuthPairMissingCode,
			expectedHasNext: true,
		},
		{
			name:            "invalid JSON",
			method:          http.MethodPost,
			body:            "not json",
			expectedStatus:  http.StatusBadRequest,
			expectedError:   "invalid_request",
			expectedCode:    hostErrors.CodeAuthPairInvalidRequest,
			expectedHasNext: true,
		},
		{
			name:            "invalid code",
			method:          http.MethodPost,
			body:            PairRequest{Code: "000000"},
			setupCode:       true,
			expectedStatus:  http.StatusUnauthorized,
			expectedError:   "invalid_code",
			expectedCode:    hostErrors.CodeAuthPairInvalidCode,
			expectedHasNext: true,
		},
		{
			name:            "rate limited",
			method:          http.MethodPost,
			body:            PairRequest{Code: "000000"},
			setupCode:       true,
			maxAttempts:     2,
			exhaustAttempts: 2,
			expectedStatus:  http.StatusTooManyRequests,
			expectedError:   "rate_limited",
			expectedCode:    hostErrors.CodeAuthPairRateLimited,
			expectedHasNext: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockDeviceStore()
			cfg := PairingConfig{DeviceStore: store}
			if tt.maxAttempts > 0 {
				cfg.MaxAttemptsPerMinute = tt.maxAttempts
			}
			pm := NewPairingManager(cfg)
			handler := NewPairHandler(pm)

			if tt.setupCode {
				if _, err := pm.GenerateCode(); err != nil {
					t.Fatalf("GenerateCode: %v", err)
				}
			}

			// Exhaust rate limit if needed
			for i := 0; i < tt.exhaustAttempts; i++ {
				b, _ := json.Marshal(PairRequest{Code: "000000"})
				r := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(b))
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
			}

			// Make the actual request
			var bodyBytes []byte
			switch v := tt.body.(type) {
			case string:
				bodyBytes = []byte(v)
			case PairRequest:
				bodyBytes, _ = json.Marshal(v)
			case nil:
				bodyBytes = nil
			}

			req := httptest.NewRequest(tt.method, "/pair", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("status: got %d, want %d (%s)", w.Code, tt.expectedStatus, w.Body.String())
			}

			var resp ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if resp.Error != tt.expectedError {
				t.Errorf("error: got %q, want %q", resp.Error, tt.expectedError)
			}
			if resp.ErrorCode != tt.expectedCode {
				t.Errorf("error_code: got %q, want %q", resp.ErrorCode, tt.expectedCode)
			}
			if tt.expectedHasNext && resp.NextAction == "" {
				t.Errorf("next_action: expected non-empty, got empty")
			}
			// Verify next_action matches the taxonomy table
			if expected := hostErrors.GetNextAction(tt.expectedCode); resp.NextAction != expected {
				t.Errorf("next_action: got %q, want %q", resp.NextAction, expected)
			}
		})
	}

	// Cases requiring custom setup beyond table-driven parameters.

	t.Run("expired code", func(t *testing.T) {
		store := newMockDeviceStore()
		currentTime := time.Now()
		pm := NewPairingManager(PairingConfig{
			DeviceStore: store,
			CodeExpiry:  100 * time.Millisecond,
			TimeNow: func() time.Time {
				return currentTime
			},
		})
		handler := NewPairHandler(pm)

		code, err := pm.GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}

		// Advance time past expiry
		currentTime = currentTime.Add(200 * time.Millisecond)

		body, _ := json.Marshal(PairRequest{Code: code})
		req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assertTaxonomyResponse(t, w, http.StatusUnauthorized, "expired_code",
			hostErrors.CodeAuthPairExpiredCode)

		// P6U2 regression guard: verify TTL says "2 minutes" not "5 minutes"
		nextAction := hostErrors.GetNextAction(hostErrors.CodeAuthPairExpiredCode)
		if !strings.Contains(nextAction, "2 minutes") {
			t.Errorf("expired code next_action should say '2 minutes', got: %s", nextAction)
		}
	})

	t.Run("used code", func(t *testing.T) {
		store := newMockDeviceStore()
		pm := NewPairingManager(PairingConfig{DeviceStore: store})
		handler := NewPairHandler(pm)

		code, err := pm.GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}

		// Use the code once (should succeed)
		body, _ := json.Marshal(PairRequest{Code: code, DeviceName: "First"})
		req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("first use: expected 200, got %d: %s", w.Code, w.Body.String())
		}

		// Second use should return used_code
		body, _ = json.Marshal(PairRequest{Code: code, DeviceName: "Second"})
		req = httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assertTaxonomyResponse(t, w, http.StatusUnauthorized, "used_code",
			hostErrors.CodeAuthPairUsedCode)
	})

	t.Run("internal error", func(t *testing.T) {
		// Use a device store that fails on SaveDevice to trigger the default
		// error branch (internal_error) in the handler.
		store := &failingSaveDeviceStore{mockDeviceStore: newMockDeviceStore()}
		pm := NewPairingManager(PairingConfig{DeviceStore: store})
		handler := NewPairHandler(pm)

		code, err := pm.GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}

		body, _ := json.Marshal(PairRequest{Code: code, DeviceName: "FailTest"})
		req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assertTaxonomyResponse(t, w, http.StatusInternalServerError, "internal_error",
			hostErrors.CodeAuthPairInternal)
	})
}

// TestGenerateCodeHandlerTaxonomyMapping verifies that /pair/generate non-200
// responses include the correct error_code and next_action per A3 taxonomy.
func TestGenerateCodeHandlerTaxonomyMapping(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		remoteAddr     string
		expectedStatus int
		expectedError  string
		expectedCode   string
	}{
		{
			name:           "forbidden - non-loopback",
			method:         http.MethodPost,
			remoteAddr:     "192.0.2.10:54321",
			expectedStatus: http.StatusForbidden,
			expectedError:  "forbidden",
			expectedCode:   hostErrors.CodeAuthPairGenerateForbidden,
		},
		{
			name:           "method not allowed",
			method:         http.MethodGet,
			remoteAddr:     "127.0.0.1:54321",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedError:  "method_not_allowed",
			expectedCode:   hostErrors.CodeAuthPairGenerateMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockDeviceStore()
			pm := NewPairingManager(PairingConfig{DeviceStore: store})
			handler := NewGenerateCodeHandler(pm)

			req := httptest.NewRequest(tt.method, "/pair/generate", nil)
			req.RemoteAddr = tt.remoteAddr
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("status: got %d, want %d (%s)", w.Code, tt.expectedStatus, w.Body.String())
			}

			var resp ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if resp.Error != tt.expectedError {
				t.Errorf("error: got %q, want %q", resp.Error, tt.expectedError)
			}
			if resp.ErrorCode != tt.expectedCode {
				t.Errorf("error_code: got %q, want %q", resp.ErrorCode, tt.expectedCode)
			}
			if resp.NextAction == "" {
				t.Error("next_action: expected non-empty, got empty")
			}
			if expected := hostErrors.GetNextAction(tt.expectedCode); resp.NextAction != expected {
				t.Errorf("next_action: got %q, want %q", resp.NextAction, expected)
			}
		})
	}

	// internal_error: GenerateCode only fails on crypto/rand failure which
	// cannot be injected at the unit level. Verify the handler's writeError
	// output produces the correct taxonomy mapping for this code path.
	t.Run("internal error (writeError path)", func(t *testing.T) {
		pm := NewPairingManager(PairingConfig{DeviceStore: newMockDeviceStore()})
		handler := NewGenerateCodeHandler(pm)
		w := httptest.NewRecorder()
		handler.writeError(w, http.StatusInternalServerError, "internal_error",
			hostErrors.CodeAuthPairGenerateInternal, "Failed to generate pairing code")

		assertTaxonomyResponse(t, w, http.StatusInternalServerError, "internal_error",
			hostErrors.CodeAuthPairGenerateInternal)
	})
}
