package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/keepawake"
)

type keepAwakeSummaryRuntimeStub struct {
	calls []bool
}

func (s *keepAwakeSummaryRuntimeStub) SetDesiredEnabled(_ context.Context, enabled bool) keepawake.Status {
	s.calls = append(s.calls, enabled)
	if enabled {
		return keepawake.Status{State: keepawake.StateOn}
	}
	return keepawake.Status{State: keepawake.StateOff}
}

// TestStatusHandler_Success tests that the status handler returns correct JSON.
func TestStatusHandler_Success(t *testing.T) {
	// Create a server with known state
	srv := NewServer("127.0.0.1:7070")

	// Create handler with known values
	handler := NewStatusHandler(srv, "/path/to/repo", "main", true, false, "/tmp/pseudocoder.sock")

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
	if status.PairSocketPath != "/tmp/pseudocoder.sock" {
		t.Errorf("expected PairSocketPath /tmp/pseudocoder.sock, got %s", status.PairSocketPath)
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
	handler := NewStatusHandler(srv, "/repo", "main", true, false, "")
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
	handler := NewStatusHandler(srv, "/repo", "main", true, false, "")

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
	handler := NewStatusHandler(srv, "/repo", "main", true, false, "")

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

// TestStatusHandler_KeepAwakeDefaults tests that the keep_awake block is present with defaults.
func TestStatusHandler_KeepAwakeDefaults(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	handler := NewStatusHandler(srv, "/repo", "main", true, false, "")

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

	if status.KeepAwake == nil {
		t.Fatal("expected KeepAwake to be present")
	}

	ka := status.KeepAwake
	if ka.State != "OFF" {
		t.Errorf("expected State OFF, got %s", ka.State)
	}
	if ka.ActiveLeaseCount != 0 {
		t.Errorf("expected ActiveLeaseCount 0, got %d", ka.ActiveLeaseCount)
	}
	if ka.RemoteEnabled {
		t.Error("expected RemoteEnabled false")
	}
	if ka.AllowAdminRevoke {
		t.Error("expected AllowAdminRevoke false")
	}
	if ka.ServerBootID == "" {
		t.Error("expected ServerBootID to be set")
	}
	if ka.NextExpiryMs != 0 {
		t.Errorf("expected NextExpiryMs 0, got %d", ka.NextExpiryMs)
	}
	if ka.DegradedReason != "" {
		t.Errorf("expected DegradedReason empty, got %s", ka.DegradedReason)
	}
}

func TestKeepAwakeSummary_ReconcilesRuntimeViaCanonicalPath(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	runtimeStub := &keepAwakeSummaryRuntimeStub{}
	srv.SetKeepAwakeRuntimeManager(runtimeStub)

	now := time.Unix(2000, 0)
	srv.keepAwake.mu.Lock()
	srv.keepAwake.now = func() time.Time { return now }
	srv.keepAwake.runtimeState = "ON"
	srv.keepAwake.statusRevision = 7
	srv.keepAwake.leases["lease-expired"] = &keepAwakeLease{
		LeaseID:   "lease-expired",
		SessionID: "session-1",
		DeviceID:  "device-1",
		ExpiresAt: now.Add(-time.Second),
	}
	srv.keepAwake.mu.Unlock()

	status := srv.KeepAwakeSummary()

	if status.ActiveLeaseCount != 0 {
		t.Fatalf("expected expired lease to be pruned, got %d", status.ActiveLeaseCount)
	}
	if status.State != "OFF" {
		t.Fatalf("expected reconciled runtime state OFF, got %s", status.State)
	}
	if len(runtimeStub.calls) != 1 || runtimeStub.calls[0] {
		t.Fatalf("expected runtime to be reconciled once with desiredEnabled=false, got %#v", runtimeStub.calls)
	}
	if status.StatusRevision != 8 {
		t.Fatalf("expected status revision increment to 8, got %d", status.StatusRevision)
	}
}

func TestKeepAwakeSummary_AppliesBatteryThresholdAutoDisable(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	runtimeStub := &keepAwakeSummaryRuntimeStub{}
	srv.SetKeepAwakeRuntimeManager(runtimeStub)

	onBattery := true
	batteryPercent := 10
	srv.SetKeepAwakePowerProvider(&stubPowerProvider{
		onBattery:      &onBattery,
		batteryPercent: &batteryPercent,
	})
	srv.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowOnBattery:            true,
		AutoDisableBatteryPercent: 20,
	})

	now := time.Unix(3000, 0)
	srv.keepAwake.mu.Lock()
	srv.keepAwake.now = func() time.Time { return now }
	srv.keepAwake.runtimeState = "ON"
	srv.keepAwake.statusRevision = 4
	srv.keepAwake.leases["lease-active"] = &keepAwakeLease{
		LeaseID:   "lease-active",
		SessionID: "session-1",
		DeviceID:  "device-1",
		ExpiresAt: now.Add(time.Hour),
	}
	srv.keepAwake.mu.Unlock()

	status := srv.KeepAwakeSummary()
	if status.ActiveLeaseCount != 0 {
		t.Fatalf("expected threshold revoke to clear leases, got %d", status.ActiveLeaseCount)
	}
	if status.State != "OFF" {
		t.Fatalf("expected runtime state OFF after threshold revoke, got %s", status.State)
	}
	if status.StatusRevision != 5 {
		t.Fatalf("expected status revision increment to 5, got %d", status.StatusRevision)
	}
	if len(runtimeStub.calls) != 1 || runtimeStub.calls[0] {
		t.Fatalf("expected runtime to be reconciled with desiredEnabled=false, got %#v", runtimeStub.calls)
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
			handler := NewStatusHandler(srv, "/repo", "feature-branch", tt.tlsEnabled, tt.requireAuth, "")

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

// TestStatusHandler_KeepAwakePolicyPowerSummary verifies that power/policy fields
// are populated in the /status response.
func TestStatusHandler_KeepAwakePolicyPowerSummary(t *testing.T) {
	srv := NewServer("127.0.0.1:7070")
	srv.SetKeepAwakeRuntimeManager(&keepAwakeSummaryRuntimeStub{})
	srv.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowAdminRevoke:          true,
		AllowOnBattery:            false,
		AutoDisableBatteryPercent: 20,
	})

	onBatt := true
	pct := 50
	srv.SetKeepAwakePowerProvider(&stubPowerProvider{
		onBattery:      &onBatt,
		batteryPercent: &pct,
	})

	handler := NewStatusHandler(srv, "/test/repo", "main", true, true, "")
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var status StatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ka := status.KeepAwake
	if ka == nil {
		t.Fatal("KeepAwake should not be nil")
	}
	if ka.AllowOnBattery {
		t.Error("AllowOnBattery should be false")
	}
	if ka.AutoDisableBatteryPercent != 20 {
		t.Errorf("AutoDisableBatteryPercent = %d, want 20", ka.AutoDisableBatteryPercent)
	}
	if ka.OnBattery == nil || !*ka.OnBattery {
		t.Error("OnBattery should be true")
	}
	if ka.BatteryPercent == nil || *ka.BatteryPercent != 50 {
		t.Errorf("BatteryPercent = %v, want 50", ka.BatteryPercent)
	}
	if !ka.PolicyBlocked {
		t.Error("PolicyBlocked should be true")
	}
	if ka.PolicyReason != "requires_external_power" {
		t.Errorf("PolicyReason = %q, want requires_external_power", ka.PolicyReason)
	}
	if ka.RecoveryHint == "" {
		t.Error("RecoveryHint should be populated")
	}
}

// =============================================================================
// P17U6: Keep-Awake Adversarial Matrix Tests
// =============================================================================

func TestStatusHandler_KeepAwakeHardStopRecoveryHintDeterministic(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(srv *Server)
		wantHint  string
		wantState string
	}{
		{
			name: "migration failed",
			setup: func(srv *Server) {
				srv.SetKeepAwakeMigrationFailed(true)
			},
			wantHint:  "Audit storage migration failed. Restart host to retry.",
			wantState: "OFF",
		},
		{
			name: "degraded unsupported",
			setup: func(srv *Server) {
				srv.keepAwake.mu.Lock()
				srv.keepAwake.runtimeState = "DEGRADED"
				srv.keepAwake.runtimeReason = "unsupported_environment"
				srv.keepAwake.mu.Unlock()
			},
			wantHint:  "Keep-awake is not supported in this environment.",
			wantState: "DEGRADED",
		},
		{
			name: "power policy blocked",
			setup: func(srv *Server) {
				srv.SetKeepAwakeRuntimeManager(&keepAwakeSummaryRuntimeStub{})
				srv.SetKeepAwakePowerProvider(&stubPowerProvider{
					onBattery:      boolPtr(true),
					batteryPercent: kaIntPtr(50),
				})
				srv.SetKeepAwakePolicy(KeepAwakePolicyConfig{
					RemoteEnabled:  true,
					AllowOnBattery: false,
				})
			},
			wantHint:  "Connect external power to enable keep-awake.",
			wantState: "OFF",
		},
		{
			name: "clean state",
			setup: func(srv *Server) {
				srv.SetKeepAwakeRuntimeManager(&keepAwakeSummaryRuntimeStub{})
			},
			wantHint:  "",
			wantState: "OFF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer("127.0.0.1:7070")
			tt.setup(srv)
			handler := NewStatusHandler(srv, "/repo", "main", true, false, "")

			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rr.Code)
			}

			var status StatusResponse
			if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
				t.Fatalf("decode: %v", err)
			}

			ka := status.KeepAwake
			if ka == nil {
				t.Fatal("KeepAwake should not be nil")
			}
			if ka.RecoveryHint != tt.wantHint {
				t.Errorf("RecoveryHint = %q, want %q", ka.RecoveryHint, tt.wantHint)
			}
			if ka.State != tt.wantState {
				t.Errorf("State = %q, want %q", ka.State, tt.wantState)
			}
		})
	}
}
