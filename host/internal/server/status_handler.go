// Package server provides WebSocket server functionality for the pseudocoder host.
// This file implements the status HTTP endpoint for CLI queries (Unit 7.5).
package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// StatusResponse contains host status information returned by the /status endpoint.
// This structure is used by the CLI to display host status to the user.
type StatusResponse struct {
	// ListeningAddress is the address the host is listening on (e.g., "127.0.0.1:7070").
	ListeningAddress string `json:"listening_address"`

	// ConnectedClients is the number of currently connected WebSocket clients.
	ConnectedClients int `json:"connected_clients"`

	// SessionID is the unique identifier for the current PTY session.
	SessionID string `json:"session_id"`

	// RepositoryPath is the path to the git repository being supervised.
	RepositoryPath string `json:"repository_path"`

	// CurrentBranch is the git branch the repository is on.
	CurrentBranch string `json:"current_branch"`

	// UptimeSeconds is how long the host has been running, in seconds.
	UptimeSeconds int64 `json:"uptime_seconds"`

	// TLSEnabled indicates whether the host is using TLS encryption.
	TLSEnabled bool `json:"tls_enabled"`

	// RequireAuth indicates whether authentication is required for WebSocket connections.
	RequireAuth bool `json:"require_auth"`

	// PairSocketPath is the path to the pairing IPC socket, or empty if unavailable.
	PairSocketPath string `json:"pair_socket_path,omitempty"`

	// KeepAwake is the keep-awake status summary (nil when not available).
	KeepAwake *KeepAwakeStatusSummary `json:"keep_awake,omitempty"`
}

// KeepAwakeStatusSummary contains keep-awake status for the /status endpoint.
type KeepAwakeStatusSummary struct {
	State                     string `json:"state"`
	ActiveLeaseCount          int    `json:"active_lease_count"`
	NextExpiryMs              int64  `json:"next_expiry_ms,omitempty"`
	RemoteEnabled             bool   `json:"remote_enabled"`
	AllowAdminRevoke          bool   `json:"allow_admin_revoke"`
	AllowOnBattery            bool   `json:"allow_on_battery"`
	AutoDisableBatteryPercent int    `json:"auto_disable_battery_percent,omitempty"`
	DegradedReason            string `json:"degraded_reason,omitempty"`
	StatusRevision            int64  `json:"status_revision"`
	ServerBootID              string `json:"server_boot_id"`
	OnBattery                 *bool  `json:"on_battery,omitempty"`
	BatteryPercent            *int   `json:"battery_percent,omitempty"`
	PolicyBlocked             bool   `json:"policy_blocked,omitempty"`
	PolicyReason              string `json:"policy_reason,omitempty"`
	RecoveryHint              string `json:"recovery_hint,omitempty"`
}

// StatusHandler handles HTTP requests for host status.
// This endpoint is restricted to local machine addresses for security.
// It provides information needed by the "pseudocoder host status" CLI command.
type StatusHandler struct {
	server      *Server
	startTime   time.Time
	repoPath    string
	branch      string
	tlsEnabled  bool
	requireAuth bool
	pairSocket  string
}

// NewStatusHandler creates a new StatusHandler.
// The handler captures the current time as the server start time for uptime calculation.
//
// Parameters:
//   - s: The WebSocket server instance (for client count and session ID)
//   - repoPath: Path to the repository being supervised
//   - branch: Current git branch
//   - tlsEnabled: Whether TLS is enabled
//   - requireAuth: Whether authentication is required
//   - pairSocketPath: Path to the pairing IPC socket (empty if unavailable)
func NewStatusHandler(s *Server, repoPath, branch string, tlsEnabled, requireAuth bool, pairSocketPath string) *StatusHandler {
	return &StatusHandler{
		server:      s,
		startTime:   time.Now(),
		repoPath:    repoPath,
		branch:      branch,
		tlsEnabled:  tlsEnabled,
		requireAuth: requireAuth,
		pairSocket:  pairSocketPath,
	}
}

// ServeHTTP handles HTTP GET requests to the /status endpoint.
// It returns a JSON StatusResponse with current host information.
//
// Security: This endpoint only responds to local machine requests.
// Non-local requests receive HTTP 403 Forbidden.
//
// Only GET method is allowed; other methods receive HTTP 405 Method Not Allowed.
func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from local machine addresses.
	// This prevents exposure of status information over the network.
	if !isLoopbackRequest(r) {
		http.Error(w, "Forbidden: status endpoint is local-only", http.StatusForbidden)
		return
	}

	// Only accept GET requests for status queries.
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Build the status response from current server state.
	resp := StatusResponse{
		ListeningAddress: h.server.Addr(),
		ConnectedClients: h.server.ClientCount(),
		SessionID:        h.server.SessionID(),
		RepositoryPath:   h.repoPath,
		CurrentBranch:    h.branch,
		UptimeSeconds:    int64(time.Since(h.startTime).Seconds()),
		TLSEnabled:       h.tlsEnabled,
		RequireAuth:      h.requireAuth,
		PairSocketPath:   h.pairSocket,
	}

	// Include keep-awake summary.
	kaStatus := h.server.KeepAwakeSummary()
	summary := &KeepAwakeStatusSummary{
		State:                     kaStatus.State,
		ActiveLeaseCount:          kaStatus.ActiveLeaseCount,
		NextExpiryMs:              kaStatus.NextExpiryMs,
		RemoteEnabled:             kaStatus.Policy.RemoteEnabled,
		AllowAdminRevoke:          kaStatus.Policy.AllowAdminRevoke,
		AllowOnBattery:            kaStatus.Policy.AllowOnBattery,
		AutoDisableBatteryPercent: kaStatus.Policy.AutoDisableBatteryPercent,
		DegradedReason:            kaStatus.DegradedReason,
		StatusRevision:            kaStatus.StatusRevision,
		ServerBootID:              kaStatus.ServerBootID,
		RecoveryHint:              kaStatus.RecoveryHint,
	}
	if kaStatus.Power != nil {
		summary.OnBattery = kaStatus.Power.OnBattery
		summary.BatteryPercent = kaStatus.Power.BatteryPercent
		summary.PolicyBlocked = kaStatus.Power.PolicyBlocked
		summary.PolicyReason = kaStatus.Power.PolicyReason
	}
	resp.KeepAwake = summary

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Log error but response is already partially sent
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}
