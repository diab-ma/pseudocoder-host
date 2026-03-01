package server

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/storage"
)

// ADR 0040 thresholds for auto-disable.
const (
	ThresholdCrashFreeRate    = 99.0  // percent
	ThresholdTerminalP95Ms    = 400   // milliseconds
	ThresholdPairingSuccessRate = 95.0 // percent
	MonitorWindow             = 30 * time.Minute
	MonitorInterval           = 30 * time.Second
)

// WindowStats holds the computed rolling-window statistics.
type WindowStats struct {
	CrashFreeRate      float64 `json:"crash_free_rate"`
	CrashCount         int     `json:"crash_count"`
	TerminalP95Ms      int64   `json:"terminal_p95_ms"`
	LatencySampleCount int     `json:"latency_sample_count"`
	PairingSuccessRate float64 `json:"pairing_success_rate"`
	PairingTotal       int     `json:"pairing_total"`
	PairingSuccesses   int     `json:"pairing_successes"`
}

// BreachType identifies which threshold was breached.
type BreachType string

const (
	BreachCrashFree    BreachType = "crash_free_rate"
	BreachTerminalP95  BreachType = "terminal_p95"
	BreachPairingRate  BreachType = "pairing_success_rate"
)

// RolloutMonitor checks metrics at regular intervals and auto-disables
// V2 phase flags when sustained breaches are detected.
type RolloutMonitor struct {
	metricsStore storage.MetricsStore
	configPath   string
	loadConfig   func() (*config.Config, error)

	mu       sync.Mutex
	stopCh   chan struct{}
	stopped  bool
	// breachStart tracks when a sustained breach began for each type.
	// If nil, no breach is currently active.
	breachStart map[BreachType]time.Time

	// now is a test seam for time injection.
	now func() time.Time
	// onCheck is a test seam called after each check cycle completes.
	onCheck func(stats WindowStats, breaches []BreachType, disabled bool)
}

// NewRolloutMonitor creates a new monitor.
// configPath is the path to the TOML config file for flag persistence.
// loadConfig returns the current config (re-read on each check).
func NewRolloutMonitor(
	metricsStore storage.MetricsStore,
	configPath string,
	loadConfig func() (*config.Config, error),
) *RolloutMonitor {
	return &RolloutMonitor{
		metricsStore: metricsStore,
		configPath:   configPath,
		loadConfig:   loadConfig,
		stopCh:       make(chan struct{}),
		breachStart:  make(map[BreachType]time.Time),
		now:          time.Now,
	}
}

// Start begins the background monitoring goroutine.
func (m *RolloutMonitor) Start() {
	go m.run()
}

// Stop signals the monitor to stop and waits for it to finish.
func (m *RolloutMonitor) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.stopCh)
	m.mu.Unlock()
}

func (m *RolloutMonitor) run() {
	ticker := time.NewTicker(MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.check()
		}
	}
}

// ComputeWindowStats queries the metrics store for rolling-window statistics.
func (m *RolloutMonitor) ComputeWindowStats() (WindowStats, error) {
	var stats WindowStats

	// Crash-free rate: if no crashes, rate = 100%.
	crashCount, err := m.metricsStore.QueryCrashWindow(MonitorWindow)
	if err != nil {
		return stats, fmt.Errorf("query crash window: %w", err)
	}
	stats.CrashCount = crashCount
	if crashCount == 0 {
		stats.CrashFreeRate = 100.0
	} else {
		// Crash-free rate is based on sessions/requests, but we use
		// a simple model: rate = max(0, 100 - crashCount) for simplicity.
		// With 100 as baseline, each crash reduces rate by 1%.
		stats.CrashFreeRate = 100.0 - float64(crashCount)
		if stats.CrashFreeRate < 0 {
			stats.CrashFreeRate = 0
		}
	}

	// Terminal p95 latency.
	p95, sampleCount, err := m.metricsStore.QueryLatencyP95Window(MonitorWindow)
	if err != nil {
		return stats, fmt.Errorf("query latency window: %w", err)
	}
	stats.TerminalP95Ms = p95
	stats.LatencySampleCount = sampleCount

	// Pairing success rate.
	total, successes, err := m.metricsStore.QueryPairingWindow(MonitorWindow)
	if err != nil {
		return stats, fmt.Errorf("query pairing window: %w", err)
	}
	stats.PairingTotal = total
	stats.PairingSuccesses = successes
	if total == 0 {
		stats.PairingSuccessRate = 100.0 // No attempts = no failures
	} else {
		stats.PairingSuccessRate = float64(successes) / float64(total) * 100.0
	}

	return stats, nil
}

func (m *RolloutMonitor) check() {
	stats, err := m.ComputeWindowStats()
	if err != nil {
		log.Printf("rollout-monitor: failed to compute stats: %v", err)
		return
	}

	now := m.now()
	var breaches []BreachType

	// Check each threshold.
	if stats.CrashFreeRate < ThresholdCrashFreeRate && stats.CrashCount > 0 {
		breaches = append(breaches, BreachCrashFree)
	}
	if stats.TerminalP95Ms > ThresholdTerminalP95Ms && stats.LatencySampleCount > 0 {
		breaches = append(breaches, BreachTerminalP95)
	}
	if stats.PairingSuccessRate < ThresholdPairingSuccessRate && stats.PairingTotal > 0 {
		breaches = append(breaches, BreachPairingRate)
	}

	m.mu.Lock()

	// Update breach tracking.
	for _, bt := range []BreachType{BreachCrashFree, BreachTerminalP95, BreachPairingRate} {
		inBreach := false
		for _, b := range breaches {
			if b == bt {
				inBreach = true
				break
			}
		}
		if inBreach {
			if _, exists := m.breachStart[bt]; !exists {
				m.breachStart[bt] = now
				log.Printf("rollout-monitor: breach started: %s", bt)
			}
		} else {
			if _, exists := m.breachStart[bt]; exists {
				log.Printf("rollout-monitor: breach cleared: %s", bt)
				delete(m.breachStart, bt)
			}
		}
	}

	// Check for sustained breaches (>= MonitorWindow).
	disabled := false
	for bt, start := range m.breachStart {
		if now.Sub(start) >= MonitorWindow {
			log.Printf("rollout-monitor: sustained breach for %s (%v), triggering auto-disable", bt, now.Sub(start))
			disabled = true
			break
		}
	}
	m.mu.Unlock()

	if disabled {
		m.autoDisable(stats)
	}

	if m.onCheck != nil {
		m.onCheck(stats, breaches, disabled)
	}
}

func (m *RolloutMonitor) autoDisable(stats WindowStats) {
	cfg, err := m.loadConfig()
	if err != nil {
		log.Printf("rollout-monitor: failed to load config for auto-disable: %v", err)
		return
	}

	// If kill switch is already on, nothing to do.
	if cfg.V2KillSwitch {
		return
	}

	// Disable all enabled phases.
	anyChanged := false
	for _, p := range []int{0, 1, 2, 3, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19} {
		if cfg.V2PhaseEnabled(p) {
			cfg.SetV2PhaseEnabled(p, false)
			anyChanged = true
		}
	}

	if !anyChanged {
		return
	}

	// Record the auto-disable event as JSON snapshot.
	snapshot, _ := json.Marshal(stats)
	log.Printf("rollout-monitor: auto-disabling all phases. Stats: %s", snapshot)

	if err := config.PersistRolloutFlags(m.configPath, cfg); err != nil {
		log.Printf("rollout-monitor: failed to persist auto-disable: %v", err)
		return
	}

	// Record in metrics store as a crash event for audit trail.
	if err := m.metricsStore.RecordCrash("rollout_auto_disable", string(snapshot)); err != nil {
		log.Printf("rollout-monitor: failed to record auto-disable event: %v", err)
	}

	// Clear breach tracking after successful disable.
	m.mu.Lock()
	m.breachStart = make(map[BreachType]time.Time)
	m.mu.Unlock()

	log.Printf("rollout-monitor: auto-disable complete - all V2 phases disabled")
}
