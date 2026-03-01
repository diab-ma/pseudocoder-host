package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/storage"
)

// testMonitorSetup creates a monitor with an in-memory metrics store
// and a temp config file for testing.
type testMonitorSetup struct {
	monitor      *RolloutMonitor
	metricsStore *storage.SQLiteMetricsStore
	configPath   string
	t            *testing.T
}

func newTestMonitorSetup(t *testing.T) *testMonitorSetup {
	t.Helper()
	ms, err := storage.NewSQLiteMetricsStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteMetricsStore: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	initial := `repo = "/test"
v2_phase_0_enabled = true
v2_phase_5_enabled = true
v2_kill_switch = false
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loadConfig := func() (*config.Config, error) {
		return config.Load(cfgPath)
	}

	m := NewRolloutMonitor(ms, cfgPath, loadConfig)
	return &testMonitorSetup{
		monitor:      m,
		metricsStore: ms,
		configPath:   cfgPath,
		t:            t,
	}
}

func (s *testMonitorSetup) loadConfig() *config.Config {
	s.t.Helper()
	cfg, err := config.Load(s.configPath)
	if err != nil {
		s.t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestRolloutMonitor_ComputeWindowStats_Empty(t *testing.T) {
	setup := newTestMonitorSetup(t)

	stats, err := setup.monitor.ComputeWindowStats()
	if err != nil {
		t.Fatalf("ComputeWindowStats: %v", err)
	}

	if stats.CrashFreeRate != 100.0 {
		t.Errorf("CrashFreeRate = %f, want 100.0", stats.CrashFreeRate)
	}
	if stats.TerminalP95Ms != 0 {
		t.Errorf("TerminalP95Ms = %d, want 0", stats.TerminalP95Ms)
	}
	if stats.PairingSuccessRate != 100.0 {
		t.Errorf("PairingSuccessRate = %f, want 100.0", stats.PairingSuccessRate)
	}
}

func TestRolloutMonitor_CrashBreach_SustainedDisables(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// Inject crashes to breach threshold (need > 1 crash for <99% rate)
	for i := 0; i < 5; i++ {
		if err := setup.metricsStore.RecordCrash("test", "test crash"); err != nil {
			t.Fatalf("RecordCrash: %v", err)
		}
	}

	// Set breach start to 31 minutes ago (simulating sustained breach).
	now := time.Now()
	setup.monitor.now = func() time.Time { return now }
	setup.monitor.breachStart[BreachCrashFree] = now.Add(-31 * time.Minute)

	var mu sync.Mutex
	var lastDisabled bool
	setup.monitor.onCheck = func(stats WindowStats, breaches []BreachType, disabled bool) {
		mu.Lock()
		lastDisabled = disabled
		mu.Unlock()
	}

	// Run a single check cycle.
	setup.monitor.check()

	mu.Lock()
	wasDisabled := lastDisabled
	mu.Unlock()

	if !wasDisabled {
		t.Error("expected auto-disable to trigger after sustained crash breach")
	}

	// Verify phases are now disabled in config.
	cfg := setup.loadConfig()
	if cfg.V2Phase0Enabled {
		t.Error("V2Phase0Enabled should be false after auto-disable")
	}
	if cfg.V2Phase5Enabled {
		t.Error("V2Phase5Enabled should be false after auto-disable")
	}
}

func TestRolloutMonitor_LatencyBreach_SustainedDisables(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// Inject high-latency samples.
	for i := 0; i < 20; i++ {
		if err := setup.metricsStore.RecordLatency("terminal", 500); err != nil {
			t.Fatalf("RecordLatency: %v", err)
		}
	}

	now := time.Now()
	setup.monitor.now = func() time.Time { return now }
	setup.monitor.breachStart[BreachTerminalP95] = now.Add(-31 * time.Minute)

	var disabled bool
	setup.monitor.onCheck = func(stats WindowStats, breaches []BreachType, d bool) {
		disabled = d
	}

	setup.monitor.check()

	if !disabled {
		t.Error("expected auto-disable to trigger after sustained latency breach")
	}

	cfg := setup.loadConfig()
	if cfg.V2Phase0Enabled {
		t.Error("V2Phase0Enabled should be false after auto-disable")
	}
}

func TestRolloutMonitor_PairingBreach_SustainedDisables(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// Inject pairing failures to breach 95% threshold.
	for i := 0; i < 10; i++ {
		if err := setup.metricsStore.RecordPairingAttempt("dev", false); err != nil {
			t.Fatalf("RecordPairingAttempt: %v", err)
		}
	}
	// Only 1 success out of 11 = ~9% success rate.
	if err := setup.metricsStore.RecordPairingAttempt("dev", true); err != nil {
		t.Fatalf("RecordPairingAttempt: %v", err)
	}

	now := time.Now()
	setup.monitor.now = func() time.Time { return now }
	setup.monitor.breachStart[BreachPairingRate] = now.Add(-31 * time.Minute)

	var disabled bool
	setup.monitor.onCheck = func(stats WindowStats, breaches []BreachType, d bool) {
		disabled = d
	}

	setup.monitor.check()

	if !disabled {
		t.Error("expected auto-disable to trigger after sustained pairing breach")
	}
}

func TestRolloutMonitor_BreachNotSustained_NoDisable(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// Inject crashes.
	for i := 0; i < 5; i++ {
		if err := setup.metricsStore.RecordCrash("test", "crash"); err != nil {
			t.Fatalf("RecordCrash: %v", err)
		}
	}

	now := time.Now()
	setup.monitor.now = func() time.Time { return now }
	// Breach started only 10 minutes ago - not yet sustained.
	setup.monitor.breachStart[BreachCrashFree] = now.Add(-10 * time.Minute)

	var disabled bool
	setup.monitor.onCheck = func(stats WindowStats, breaches []BreachType, d bool) {
		disabled = d
	}

	setup.monitor.check()

	if disabled {
		t.Error("should not auto-disable before MonitorWindow elapsed")
	}

	// Phases should still be enabled.
	cfg := setup.loadConfig()
	if !cfg.V2Phase0Enabled {
		t.Error("V2Phase0Enabled should still be true")
	}
}

func TestRolloutMonitor_BreachClears_WhenMetricsRecover(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// No crashes = no breach
	now := time.Now()
	setup.monitor.now = func() time.Time { return now }

	// Simulate a previous breach that existed
	setup.monitor.breachStart[BreachCrashFree] = now.Add(-5 * time.Minute)

	var breaches []BreachType
	setup.monitor.onCheck = func(stats WindowStats, b []BreachType, d bool) {
		breaches = b
	}

	setup.monitor.check()

	// With no crashes, the breach should have cleared.
	if len(breaches) != 0 {
		t.Errorf("expected no breaches, got %v", breaches)
	}

	// Verify the breach tracking was cleared.
	setup.monitor.mu.Lock()
	_, exists := setup.monitor.breachStart[BreachCrashFree]
	setup.monitor.mu.Unlock()

	if exists {
		t.Error("breach should have been cleared from tracking")
	}
}

func TestRolloutMonitor_KillSwitch_SkipsAutoDisable(t *testing.T) {
	setup := newTestMonitorSetup(t)

	// Enable kill switch in config.
	content, _ := os.ReadFile(setup.configPath)
	newContent := strings.Replace(string(content), "v2_kill_switch = false", "v2_kill_switch = true", 1)
	os.WriteFile(setup.configPath, []byte(newContent), 0600)

	// Inject crashes and set sustained breach.
	for i := 0; i < 5; i++ {
		setup.metricsStore.RecordCrash("test", "crash")
	}
	now := time.Now()
	setup.monitor.now = func() time.Time { return now }
	setup.monitor.breachStart[BreachCrashFree] = now.Add(-31 * time.Minute)

	var disabled bool
	setup.monitor.onCheck = func(stats WindowStats, b []BreachType, d bool) {
		disabled = d
	}

	setup.monitor.check()

	// Auto-disable should fire (breach is sustained), but autoDisable()
	// should skip because kill switch is already on.
	if !disabled {
		t.Error("check should detect the sustained breach")
	}
}

func TestRolloutMonitor_StartStop(t *testing.T) {
	setup := newTestMonitorSetup(t)
	setup.monitor.Start()
	// Give it a moment to start.
	time.Sleep(50 * time.Millisecond)
	setup.monitor.Stop()
	// Stop again should be idempotent.
	setup.monitor.Stop()
}
