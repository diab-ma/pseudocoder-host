package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/storage"
)

func newTestFlagsHandler(t *testing.T) (*FlagsAPIHandler, string) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	initial := `repo = "/test"
v2_phase_0_enabled = true
v2_phase_5_enabled = true
v2_kill_switch = false
v2_rollout_stage = "internal"
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ms, err := storage.NewSQLiteMetricsStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteMetricsStore: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	loadConfig := func() (*config.Config, error) {
		return config.Load(cfgPath)
	}

	monitor := NewRolloutMonitor(ms, cfgPath, loadConfig)

	handler := NewFlagsAPIHandler(cfgPath, loadConfig, ms, monitor)
	return handler, cfgPath
}

func TestFlagsAPI_GET(t *testing.T) {
	handler, _ := newTestFlagsHandler(t)

	req := httptest.NewRequest("GET", "/api/flags", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp FlagsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.KillSwitch {
		t.Error("KillSwitch should be false")
	}
	if resp.RolloutStage != "internal" {
		t.Errorf("RolloutStage = %q, want internal", resp.RolloutStage)
	}
	if !resp.PhaseFlags[0] {
		t.Error("Phase 0 should be enabled")
	}
	if !resp.PhaseFlags[5] {
		t.Error("Phase 5 should be enabled")
	}
	if resp.PhaseFlags[1] {
		t.Error("Phase 1 should be disabled")
	}
}

func TestFlagsAPI_POST_Enable(t *testing.T) {
	handler, cfgPath := newTestFlagsHandler(t)

	phase := 3
	body, _ := json.Marshal(FlagsMutationRequest{Action: "enable", Phase: &phase})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	var resp FlagsMutationResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Errorf("OK = false, error: %s", resp.Error)
	}

	cfg, _ := config.Load(cfgPath)
	if !cfg.V2Phase3Enabled {
		t.Error("V2Phase3Enabled should be true after enable")
	}
}

func TestFlagsAPI_POST_Disable(t *testing.T) {
	handler, cfgPath := newTestFlagsHandler(t)

	phase := 0
	body, _ := json.Marshal(FlagsMutationRequest{Action: "disable", Phase: &phase})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	cfg, _ := config.Load(cfgPath)
	if cfg.V2Phase0Enabled {
		t.Error("V2Phase0Enabled should be false after disable")
	}
}

func TestFlagsAPI_POST_KillSwitch(t *testing.T) {
	handler, cfgPath := newTestFlagsHandler(t)

	v := true
	body, _ := json.Marshal(FlagsMutationRequest{Action: "kill_switch", Value: &v})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	cfg, _ := config.Load(cfgPath)
	if !cfg.V2KillSwitch {
		t.Error("V2KillSwitch should be true after kill_switch action")
	}
}

func TestFlagsAPI_POST_RolloutStage(t *testing.T) {
	handler, cfgPath := newTestFlagsHandler(t)

	body, _ := json.Marshal(FlagsMutationRequest{Action: "rollout_stage", Stage: "beta"})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	cfg, _ := config.Load(cfgPath)
	if cfg.V2RolloutStage != "beta" {
		t.Errorf("V2RolloutStage = %q, want beta", cfg.V2RolloutStage)
	}
}

func TestFlagsAPI_POST_RolloutStage_Invalid(t *testing.T) {
	handler, _ := newTestFlagsHandler(t)

	body, _ := json.Marshal(FlagsMutationRequest{Action: "rollout_stage", Stage: "invalid"})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestFlagsAPI_POST_Promote_GateFailed(t *testing.T) {
	handler, _ := newTestFlagsHandler(t)

	// Inject crashes to fail gate.
	ms := handler.metricsStore.(*storage.SQLiteMetricsStore)
	for i := 0; i < 5; i++ {
		ms.RecordCrash("test", "crash")
	}

	phase := 3
	body, _ := json.Marshal(FlagsMutationRequest{Action: "promote", Phase: &phase})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", w.Code)
	}
}

func TestFlagsAPI_POST_Promote_GatePassed(t *testing.T) {
	handler, cfgPath := newTestFlagsHandler(t)

	// No crashes, no latency issues, no pairing failures -> gate should pass.
	phase := 3
	body, _ := json.Marshal(FlagsMutationRequest{Action: "promote", Phase: &phase})
	req := httptest.NewRequest("POST", "/api/flags", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	cfg, _ := config.Load(cfgPath)
	if !cfg.V2Phase3Enabled {
		t.Error("V2Phase3Enabled should be true after promote")
	}
}

func TestFlagsAPI_NonLoopback_Rejected(t *testing.T) {
	handler, _ := newTestFlagsHandler(t)

	req := httptest.NewRequest("GET", "/api/flags", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
