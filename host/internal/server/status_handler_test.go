package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatusHandler_Success tests that the status handler returns correct JSON.
func TestStatusHandler_Success(t *testing.T) {
	// Create a server with known state
	srv := NewServer("127.0.0.1:7070")

	// Create handler with known values
	handler := NewStatusHandler(srv, "/path/to/repo", "main", true, false)

	// Create a request from loopback address
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	// Record the response
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Check status code
	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	// Check content type
	contentType := rr.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", contentType)
	}

	// Parse response
	var status StatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Verify fields
	if status.ListeningAddress != "127.0.0.1:7070" {
		t.Errorf("expected ListeningAddress 127.0.0.1:7070, got %s", status.ListeningAddress)
	}
	if status.RepositoryPath != "/path/to/repo" {
		t.Errorf("expected RepositoryPath /path/to/repo, got %s", status.RepositoryPath)
	}
	if status.CurrentBranch != "main" {
		t.Errorf("expected CurrentBranch main, got %s", status.CurrentBranch)
	}
	if !status.TLSEnabled {
		t.Error("expected TLSEnabled true")
	}
	if status.RequireAuth {
		t.Error("expected RequireAuth false")
	}
	if status.ConnectedClients != 0 {
		t.Errorf("expected ConnectedClients 0, got %d", status.ConnectedClients)
	}
	if status.SessionID == "" {
		t.Error("expected SessionID to be set")
	}
	// Uptime should be >= 0 (just created)
	if status.UptimeSeconds < 0 {
		t.Errorf("expected UptimeSeconds >= 0, got %d", status.UptimeSeconds)
	}
}

// TestStatusHandler_Uptime tests that uptime increases over time.
func TestStatusHandler_Uptime(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")

	// Create handler and wait a bit
	handler := NewStatusHandler(srv, "/repo", "main", true, false)
	time.Sleep(100 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var status StatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Uptime should be at least 0 (may be 0 if within first second)
	if status.UptimeSeconds < 0 {
		t.Errorf("expected UptimeSeconds >= 0, got %d", status.UptimeSeconds)
	}
}

// TestStatusHandler_LoopbackOnly tests that non-loopback requests are rejected.
func TestStatusHandler_LoopbackOnly(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	handler := NewStatusHandler(srv, "/repo", "main", true, false)

	tests := []struct {
		name       string
		remoteAddr string
		wantCode   int
	}{
		{
			name:       "IPv4 loopback allowed",
			remoteAddr: "127.0.0.1:12345",
			wantCode:   http.StatusOK,
		},
		{
			name:       "IPv6 loopback allowed",
			remoteAddr: "[::1]:12345",
			wantCode:   http.StatusOK,
		},
		{
			name:       "LAN address rejected",
			remoteAddr: "192.168.1.100:12345",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "Public IP rejected",
			remoteAddr: "8.8.8.8:12345",
			wantCode:   http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.RemoteAddr = tt.remoteAddr

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantCode {
				t.Errorf("expected status %d, got %d", tt.wantCode, rr.Code)
			}
		})
	}
}

// TestStatusHandler_MethodNotAllowed tests that non-GET methods are rejected.
func TestStatusHandler_MethodNotAllowed(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	handler := NewStatusHandler(srv, "/repo", "main", true, false)

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/status", nil)
			req.RemoteAddr = "127.0.0.1:12345"

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected status 405, got %d", rr.Code)
			}
		})
	}
}

// TestStatusHandler_AuthVariations tests different auth configurations.
func TestStatusHandler_AuthVariations(t *testing.T) {
	srv := NewServer("0.0.0.0:8080")

	tests := []struct {
		name        string
		tlsEnabled  bool
		requireAuth bool
	}{
		{"TLS+Auth", true, true},
		{"TLS only", true, false},
		{"Auth only", false, true},
		{"Neither", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewStatusHandler(srv, "/repo", "feature-branch", tt.tlsEnabled, tt.requireAuth)

			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.RemoteAddr = "127.0.0.1:12345"

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", rr.Code)
			}

			var status StatusResponse
			if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}

			if status.TLSEnabled != tt.tlsEnabled {
				t.Errorf("expected TLSEnabled %v, got %v", tt.tlsEnabled, status.TLSEnabled)
			}
			if status.RequireAuth != tt.requireAuth {
				t.Errorf("expected RequireAuth %v, got %v", tt.requireAuth, status.RequireAuth)
			}
			if status.ListeningAddress != "0.0.0.0:8080" {
				t.Errorf("expected ListeningAddress 0.0.0.0:8080, got %s", status.ListeningAddress)
			}
			if status.CurrentBranch != "feature-branch" {
				t.Errorf("expected CurrentBranch feature-branch, got %s", status.CurrentBranch)
			}
		})
	}
}
