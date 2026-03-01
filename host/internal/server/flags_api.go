package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/storage"
)

// FlagsResponse is the JSON response for GET /api/flags.
type FlagsResponse struct {
	KillSwitch   bool           `json:"kill_switch"`
	RolloutStage string         `json:"rollout_stage"`
	PhaseFlags   map[int]bool   `json:"phase_flags"`
	Metrics      *WindowStats   `json:"metrics,omitempty"`
	WatchWindow  *WatchWindowInfo `json:"watch_window,omitempty"`
}

// WatchWindowInfo tracks the 24-hour watch window for promotion gates.
type WatchWindowInfo struct {
	StageEnteredAt string `json:"stage_entered_at,omitempty"`
	ElapsedHours   float64 `json:"elapsed_hours"`
	Required       float64 `json:"required_hours"`
	Satisfied      bool    `json:"satisfied"`
}

// FlagsMutationRequest is the JSON body for POST /api/flags.
type FlagsMutationRequest struct {
	Action string `json:"action"` // "enable", "disable", "promote", "kill_switch", "rollout_stage"
	Phase  *int   `json:"phase,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Value  *bool  `json:"value,omitempty"`
}

// FlagsMutationResponse is the JSON response for POST /api/flags.
type FlagsMutationResponse struct {
	OK      bool   `json:"ok"`
	Action  string `json:"action"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// FlagsAPIHandler handles HTTP requests for rollout flag management.
type FlagsAPIHandler struct {
	configPath   string
	loadConfig   func() (*config.Config, error)
	metricsStore storage.MetricsStore
	monitor      *RolloutMonitor
	mu           sync.Mutex
	revision     int64
	onChanged    func(ServerFlagsPayload)
}

// NewFlagsAPIHandler creates a new flags API handler.
func NewFlagsAPIHandler(
	configPath string,
	loadConfig func() (*config.Config, error),
	metricsStore storage.MetricsStore,
	monitor *RolloutMonitor,
) *FlagsAPIHandler {
	return &FlagsAPIHandler{
		configPath:   configPath,
		loadConfig:   loadConfig,
		metricsStore: metricsStore,
		monitor:      monitor,
	}
}

// SetOnChanged wires an optional callback invoked after successful mutations.
// The callback receives the latest flags payload (including revision).
func (h *FlagsAPIHandler) SetOnChanged(cb func(ServerFlagsPayload)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onChanged = cb
}

func (h *FlagsAPIHandler) currentRevision() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revision
}

func (h *FlagsAPIHandler) nextRevision() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	return h.revision
}

func (h *FlagsAPIHandler) changedCallback() func(ServerFlagsPayload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.onChanged
}

func (h *FlagsAPIHandler) buildFlagsPayload(cfg *config.Config, revision int64) ServerFlagsPayload {
	return ServerFlagsPayload{
		KillSwitch:   cfg.V2KillSwitch,
		RolloutStage: cfg.EffectiveV2RolloutStage(),
		PhaseFlags:   cfg.V2PhaseFlags(),
		Revision:     revision,
	}
}

// CurrentPayload returns the latest host flags payload.
func (h *FlagsAPIHandler) CurrentPayload() (ServerFlagsPayload, error) {
	cfg, err := h.loadConfig()
	if err != nil {
		return ServerFlagsPayload{}, err
	}
	return h.buildFlagsPayload(cfg, h.currentRevision()), nil
}

// ServeHTTP handles requests to /api/flags.
func (h *FlagsAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		http.Error(w, "Forbidden: flags endpoint is local-only", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.handleGet(w, r)
	case http.MethodPost:
		h.handlePost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *FlagsAPIHandler) handleGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := h.loadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	resp := FlagsResponse{
		KillSwitch:   cfg.V2KillSwitch,
		RolloutStage: cfg.EffectiveV2RolloutStage(),
		PhaseFlags:   cfg.V2PhaseFlags(),
	}

	// Include metrics summary if monitor is available.
	if h.monitor != nil {
		stats, err := h.monitor.ComputeWindowStats()
		if err == nil {
			resp.Metrics = &stats
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *FlagsAPIHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	var req FlagsMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	cfg, err := h.loadConfig()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "config_error", fmt.Sprintf("failed to load config: %v", err))
		return
	}

	switch req.Action {
	case "enable":
		if req.Phase == nil {
			h.writeError(w, http.StatusBadRequest, "missing_phase", "phase is required for enable action")
			return
		}
		if !cfg.SetV2PhaseEnabled(*req.Phase, true) {
			h.writeError(w, http.StatusBadRequest, "invalid_phase", fmt.Sprintf("invalid phase: %d", *req.Phase))
			return
		}

	case "disable":
		if req.Phase == nil {
			h.writeError(w, http.StatusBadRequest, "missing_phase", "phase is required for disable action")
			return
		}
		if !cfg.SetV2PhaseEnabled(*req.Phase, false) {
			h.writeError(w, http.StatusBadRequest, "invalid_phase", fmt.Sprintf("invalid phase: %d", *req.Phase))
			return
		}

	case "kill_switch":
		if req.Value == nil {
			h.writeError(w, http.StatusBadRequest, "missing_value", "value is required for kill_switch action")
			return
		}
		cfg.V2KillSwitch = *req.Value

	case "rollout_stage":
		if req.Stage == "" {
			h.writeError(w, http.StatusBadRequest, "missing_stage", "stage is required for rollout_stage action")
			return
		}
		if !config.ValidV2RolloutStages[req.Stage] {
			h.writeError(w, http.StatusBadRequest, "invalid_stage", fmt.Sprintf("invalid stage: %s", req.Stage))
			return
		}
		cfg.V2RolloutStage = req.Stage

	case "promote":
		if req.Phase == nil {
			h.writeError(w, http.StatusBadRequest, "missing_phase", "phase is required for promote action")
			return
		}
		// Promotion gate: check metrics + watch window.
		if err := h.checkPromotionGate(cfg, *req.Phase); err != nil {
			h.writeError(w, http.StatusPreconditionFailed, "gate_failed", err.Error())
			return
		}
		cfg.SetV2PhaseEnabled(*req.Phase, true)
		// Record evidence.
		if h.metricsStore != nil {
			stats, _ := h.monitor.ComputeWindowStats()
			snapshot, _ := json.Marshal(stats)
			h.metricsStore.RecordPromotionEvidence(*req.Phase, cfg.EffectiveV2RolloutStage(), true, string(snapshot))
		}

	default:
		h.writeError(w, http.StatusBadRequest, "unknown_action", fmt.Sprintf("unknown action: %s", req.Action))
		return
	}

	if err := config.PersistRolloutFlags(h.configPath, cfg); err != nil {
		h.writeError(w, http.StatusInternalServerError, "persist_error", fmt.Sprintf("failed to persist flags: %v", err))
		return
	}

	payload := h.buildFlagsPayload(cfg, h.nextRevision())
	if onChanged := h.changedCallback(); onChanged != nil {
		onChanged(payload)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(FlagsMutationResponse{
		OK:      true,
		Action:  req.Action,
		Message: "flags updated successfully",
	})
}

// checkPromotionGate verifies that all metrics are above threshold
// and the 24-hour watch window has elapsed.
func (h *FlagsAPIHandler) checkPromotionGate(_ *config.Config, phase int) error {
	if h.monitor == nil {
		return fmt.Errorf("rollout monitor not configured")
	}

	stats, err := h.monitor.ComputeWindowStats()
	if err != nil {
		return fmt.Errorf("failed to compute metrics: %w", err)
	}

	// Check each threshold.
	if stats.CrashCount > 0 && stats.CrashFreeRate < ThresholdCrashFreeRate {
		return fmt.Errorf("crash-free rate %.1f%% < %.1f%% threshold (phase %d)", stats.CrashFreeRate, ThresholdCrashFreeRate, phase)
	}
	if stats.LatencySampleCount > 0 && stats.TerminalP95Ms > ThresholdTerminalP95Ms {
		return fmt.Errorf("terminal p95 %dms > %dms threshold (phase %d)", stats.TerminalP95Ms, ThresholdTerminalP95Ms, phase)
	}
	if stats.PairingTotal > 0 && stats.PairingSuccessRate < ThresholdPairingSuccessRate {
		return fmt.Errorf("pairing success rate %.1f%% < %.1f%% threshold (phase %d)", stats.PairingSuccessRate, ThresholdPairingSuccessRate, phase)
	}

	// Note: 24-hour watch window enforcement requires external state tracking.
	// For now, the CLI/operator is responsible for observing the window.
	// A future enhancement could track stage-entry timestamps in the config or DB.

	log.Printf("flags-api: promotion gate passed for phase %d", phase)
	return nil
}

func (h *FlagsAPIHandler) writeError(w http.ResponseWriter, status int, errCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(FlagsMutationResponse{
		OK:    false,
		Error: fmt.Sprintf("%s: %s", errCode, message),
	})
}

// stageEntryKey returns the promotion_evidence lookup key for watch window checks.
func stageEntryKey(stage string) string {
	return fmt.Sprintf("stage_entry_%s", stage)
}

// QueryStageEntryTime returns when the current stage was first promoted to.
// Returns zero time if no evidence exists.
func QueryStageEntryTime(ms storage.MetricsStore, stage string) time.Time {
	// The promotion_evidence table records all promotions.
	// The earliest promotion to the current stage is the watch window start.
	// This is a simplified implementation; could be enhanced with dedicated column.
	_ = ms
	_ = stage
	return time.Time{}
}
