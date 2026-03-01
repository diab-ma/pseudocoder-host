package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/keepawake"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/tmux"
)

// SessionID returns the current session identifier.
func (s *Server) SessionID() string {
	return s.sessionID
}

// Addr returns the server's listening address.
// This is used by the status handler to report the configured address.
func (s *Server) Addr() string {
	return s.addr
}

// ClientCount returns the number of connected clients.
func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// CloseDeviceConnections closes all active WebSocket connections for the given device.
// This is called when a device is revoked to immediately terminate access.
// Returns the number of connections that were closed. Thread-safe.
func (s *Server) CloseDeviceConnections(deviceID string) int {
	if deviceID == "" {
		return 0
	}

	// Keep-awake leases for a revoked device are expired immediately with no grace.
	s.revokeKeepAwakeLeasesForDevice(deviceID)

	s.mu.Lock()
	defer s.mu.Unlock()

	var closed int
	for client := range s.clients {
		if client.deviceID == deviceID {
			client.closeSend()
			closed++
			log.Printf("Closed connection for revoked device %s", deviceID)
		}
	}

	return closed
}

// KeepAwakeSummary returns a snapshot of the current keep-awake status.
// It uses the same prune/runtime reconciliation path as websocket keep-awake
// flows so /status cannot drift from host-authoritative runtime state.
func (s *Server) KeepAwakeSummary() KeepAwakeStatusPayload {
	s.keepAwake.mu.Lock()
	now := time.Now()
	if s.keepAwake.now != nil {
		now = s.keepAwake.now()
	}

	changed := s.keepAwakePruneExpiredLocked(now)
	thresholdRevoked := s.keepAwakeCheckBatteryThresholdLocked(now)
	if changed || thresholdRevoked {
		if changed && !thresholdRevoked {
			s.keepAwake.statusRevision++
		}
		s.keepAwakeSetRuntimeStateLocked(len(s.keepAwake.leases) > 0)
	}
	status := s.keepAwakeStatusPayloadLocked(now)
	s.keepAwake.mu.Unlock()

	if changed || thresholdRevoked {
		go s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
	}
	return status
}

// SetKeepAwakeRuntimeManager wires the host keep-awake runtime seam.
func (s *Server) SetKeepAwakeRuntimeManager(manager KeepAwakeRuntimeManager) {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	s.keepAwake.runtime = manager
}

// SetKeepAwakeAuditWriter wires the process-local keep-awake audit seam.
func (s *Server) SetKeepAwakeAuditWriter(writer KeepAwakeAuditWriter) {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	s.keepAwake.auditWriter = writer
}

// SetKeepAwakePolicy updates keep-awake authorization policy flags.
// If remote becomes disabled, all existing leases are revoked.
func (s *Server) SetKeepAwakePolicy(policy KeepAwakePolicyConfig) {
	s.keepAwake.mu.Lock()
	// Bootstrap policy seed should not emit a policy-change revision/audit event.
	// This preserves status_revision baseline semantics across server boot IDs.
	if !s.keepAwake.policySet {
		s.keepAwake.policy = policy
		s.keepAwake.policySet = true
		s.keepAwake.mu.Unlock()
		return
	}
	oldPolicy := s.keepAwake.policy
	policyChanged := keepAwakePolicyChanged(oldPolicy, policy)
	s.keepAwake.policy = policy
	now := s.keepAwake.now()

	// If remote was just disabled, revoke all leases.
	if oldPolicy.RemoteEnabled && !policy.RemoteEnabled && len(s.keepAwake.leases) > 0 {
		for key, lease := range s.keepAwake.leases {
			if err := s.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
				Operation:      "policy_revoke",
				ActorDeviceID:  "system",
				TargetDeviceID: lease.DeviceID,
				SessionID:      lease.SessionID,
				LeaseID:        lease.LeaseID,
				Reason:         "remote_disabled",
				At:             now,
			}); err != nil {
				status := s.keepAwakeFailClosedOnPolicyAuditErrorLocked(now, err)
				s.keepAwake.mu.Unlock()
				s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
				return
			}
			if timer := s.keepAwake.timers[key]; timer != nil {
				timer.Stop()
				delete(s.keepAwake.timers, key)
			}
			delete(s.keepAwake.leases, key)
		}
		s.keepAwakeSetRuntimeStateLocked(false)
	}

	if policyChanged {
		if err := s.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
			Operation:     "policy_change",
			ActorDeviceID: "system",
			Reason:        keepAwakePolicyChangeReason(oldPolicy, policy),
			At:            now,
		}); err != nil {
			status := s.keepAwakeFailClosedOnPolicyAuditErrorLocked(now, err)
			s.keepAwake.mu.Unlock()
			s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
			return
		}
		s.keepAwake.statusRevision++
		status := s.keepAwakeStatusPayloadLocked(now)
		s.keepAwake.mu.Unlock()
		s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
		return
	}

	s.keepAwake.mu.Unlock()
}

// keepAwakeFailClosedOnPolicyAuditErrorLocked enforces a fail-closed keep-awake
// posture when policy audit persistence fails.
// Must be called with keepAwake mutex held.
func (s *Server) keepAwakeFailClosedOnPolicyAuditErrorLocked(now time.Time, auditErr error) KeepAwakeStatusPayload {
	log.Printf("keep-awake policy audit write failed; forcing fail-closed policy state: %v", auditErr)
	s.keepAwake.migrationFailed = true
	s.keepAwake.policy.RemoteEnabled = false

	for key := range s.keepAwake.timers {
		if timer := s.keepAwake.timers[key]; timer != nil {
			timer.Stop()
		}
		delete(s.keepAwake.timers, key)
	}
	for key := range s.keepAwake.leases {
		delete(s.keepAwake.leases, key)
	}

	s.keepAwakeSetRuntimeStateLocked(false)
	s.keepAwake.statusRevision++
	return s.keepAwakeStatusPayloadLocked(now)
}

func keepAwakePolicyChanged(oldPolicy, newPolicy KeepAwakePolicyConfig) bool {
	if oldPolicy.RemoteEnabled != newPolicy.RemoteEnabled {
		return true
	}
	if oldPolicy.AllowAdminRevoke != newPolicy.AllowAdminRevoke {
		return true
	}
	if oldPolicy.AllowOnBattery != newPolicy.AllowOnBattery {
		return true
	}
	if oldPolicy.AutoDisableBatteryPercent != newPolicy.AutoDisableBatteryPercent {
		return true
	}
	if len(oldPolicy.AdminDeviceIDs) != len(newPolicy.AdminDeviceIDs) {
		return true
	}
	for i := range oldPolicy.AdminDeviceIDs {
		if oldPolicy.AdminDeviceIDs[i] != newPolicy.AdminDeviceIDs[i] {
			return true
		}
	}
	return false
}

func keepAwakePolicyChangeReason(oldPolicy, newPolicy KeepAwakePolicyConfig) string {
	return strings.Join([]string{
		fmt.Sprintf("remote_enabled:%t->%t", oldPolicy.RemoteEnabled, newPolicy.RemoteEnabled),
		fmt.Sprintf("allow_admin_revoke:%t->%t", oldPolicy.AllowAdminRevoke, newPolicy.AllowAdminRevoke),
		fmt.Sprintf("allow_on_battery:%t->%t", oldPolicy.AllowOnBattery, newPolicy.AllowOnBattery),
		fmt.Sprintf("auto_disable_battery_percent:%d->%d", oldPolicy.AutoDisableBatteryPercent, newPolicy.AutoDisableBatteryPercent),
		fmt.Sprintf("admin_device_ids:%d->%d", len(oldPolicy.AdminDeviceIDs), len(newPolicy.AdminDeviceIDs)),
	}, ",")
}

// SetKeepAwakePowerProvider wires the host power state seam.
func (s *Server) SetKeepAwakePowerProvider(provider keepawake.PowerProvider) {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	s.keepAwake.powerProvider = provider
}

// SetKeepAwakeMigrationFailed sets the migration-failed flag for fail-closed audit.
func (s *Server) SetKeepAwakeMigrationFailed(failed bool) {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	s.keepAwake.migrationFailed = failed
}

// SetKeepAwakeDisconnectGrace configures the disconnect grace duration.
// Valid range is 0ms to 300000ms inclusive.
func (s *Server) SetKeepAwakeDisconnectGrace(grace time.Duration) error {
	if grace < 0 || grace.Milliseconds() > maxKeepAwakeGraceMs {
		return fmt.Errorf("keep-awake grace out of range: %dms", grace.Milliseconds())
	}
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	s.keepAwake.grace = grace
	return nil
}

// SetDecisionHandler sets the callback for processing review decisions.
// This should be called before any clients connect. The handler is called
// when a client sends a review.decision message with action "accept" or "reject".
func (s *Server) SetDecisionHandler(handler DecisionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisionHandler = handler
}

// SetChunkDecisionHandler sets the callback for processing per-chunk decisions.
// This should be called before any clients connect. The handler is called
// when a client sends a chunk.decision message for a specific chunk within a card.
func (s *Server) SetChunkDecisionHandler(handler ChunkDecisionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkDecisionHandler = handler
}

// SetDeleteHandler sets the callback for processing file deletion requests.
// This should be called before any clients connect. The handler is called
// when a client sends a review.delete message for an untracked file.
func (s *Server) SetDeleteHandler(handler DeleteHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteHandler = handler
}

// SetUndoHandler sets the callback for processing file-level undo requests.
// The handler is called when a client sends a review.undo message.
// It should reverse the decision and return the restored card info.
// Phase 20.2: Enables undo flow from mobile.
func (s *Server) SetUndoHandler(handler UndoHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.undoHandler = handler
}

// SetChunkUndoHandler sets the callback for processing chunk-level undo requests.
// The handler is called when a client sends a chunk.undo message.
// It should reverse the chunk decision and return the restored chunk info.
// Phase 20.2: Enables per-chunk undo flow from mobile.
func (s *Server) SetChunkUndoHandler(handler ChunkUndoHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkUndoHandler = handler
}

// SetReconnectHandler sets the callback for replaying history on client connect.
// This handler is called each time a new client connects, allowing the server
// to send terminal history and pending review cards to bring the client up to date.
// The handler receives a function to send messages directly to the connecting client.
func (s *Server) SetReconnectHandler(handler ReconnectHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnectHandler = handler
}

// SetTokenValidator sets the token validation function for WebSocket auth.
// When requireAuth is true, connections without valid tokens are rejected.
func (s *Server) SetTokenValidator(validator TokenValidator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenValidator = validator
}

// SetRequireAuth controls whether authentication is required.
// When true, all WebSocket connections must provide a valid token.
func (s *Server) SetRequireAuth(require bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireAuth = require
}

// SetPairHandler sets the HTTP handler for the /pair endpoint.
// This must be called before Start() or StartAsync().
func (s *Server) SetPairHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairHandler = handler
}

// SetGenerateCodeHandler sets the HTTP handler for the /pair/generate endpoint.
// This must be called before Start() or StartAsync().
func (s *Server) SetGenerateCodeHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generateCodeHandler = handler
}

// SetRevokeDeviceHandler sets the HTTP handler for the /devices/{id}/revoke endpoint.
// This endpoint allows the CLI to signal the running host to close connections
// for revoked devices. Must be called before Start() or StartAsync().
func (s *Server) SetRevokeDeviceHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeDeviceHandler = handler
}

// SetDeviceActivityTracker sets the callback for tracking device activity.
// This is called when a message is received from an authenticated client,
// allowing the application to update last_seen timestamps.
func (s *Server) SetDeviceActivityTracker(tracker DeviceActivityTracker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceActivityTracker = tracker
}

// SetPTYWriter sets the PTY session for writing terminal input.
// This enables bidirectional terminal: mobile clients can send input
// to the PTY running on the host. Phase 5.5.
func (s *Server) SetPTYWriter(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptyWriter = w
}

// SetApprovalQueue sets the approval queue for CLI command approval.
// This enables mobile clients to approve or deny commands requested
// by CLI tools through the approval broker. Phase 6.
func (s *Server) SetApprovalQueue(queue *ApprovalQueue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvalQueue = queue
}

// GetApprovalQueue returns the approval queue for external access.
// This allows the HTTP endpoint to submit approval requests.
func (s *Server) GetApprovalQueue() *ApprovalQueue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.approvalQueue
}

// SetApproveHandler sets the HTTP handler for the /approve endpoint.
// This endpoint allows CLI tools to request command approval via HTTP.
// Phase 6.1b: HTTP endpoint for CLI command approval broker.
func (s *Server) SetApproveHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approveHandler = handler
}

// SetGitOperations sets the git operations handler for commit workflows.
// This enables mobile clients to query repository status and create commits.
// Phase 6.4: Enables commit workflow from mobile.
func (s *Server) SetGitOperations(ops *GitOperations) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gitOps = ops
}

// SetDecidedStore sets the decided card store for commit association.
// After a successful commit, accepted cards are marked as committed using this store.
// Phase 20.2: Enables commit association for undo support.
func (s *Server) SetDecidedStore(store storage.DecidedCardStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decidedStore = store
}

// SetStatusHandler sets the HTTP handler for the /status endpoint.
// This endpoint allows the CLI to query host status (uptime, clients, etc).
// Phase 7.5: Enables "pseudocoder host status" CLI command.
func (s *Server) SetStatusHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusHandler = handler
}

// SetSessionManager sets the PTY session manager for multi-session support.
// This enables mobile clients to create, close, and switch between multiple
// terminal sessions. Phase 9.3: Multi-session PTY management.
func (s *Server) SetSessionManager(mgr *pty.SessionManager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionManager = mgr
}

// SetSessionStore sets the persistent session history store.
// This enables handlers that read/mutate archived session history.
func (s *Server) SetSessionStore(store storage.SessionStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionStore = store
}

// GetSessionManager returns the session manager for external access.
// This allows the host command to manage sessions directly.
func (s *Server) GetSessionManager() *pty.SessionManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionManager
}

// NextCreatedSessionSequence increments and returns the global sequence
// number for successful session.create operations.
//
// The sequence is server-local and monotonic for the host process lifetime.
// It is intentionally independent from SessionManager.Count() so that:
// - pre-registered default sessions do not offset auto names,
// - closed sessions do not cause name reuse,
// - named creates still advance numbering for later unnamed creates.
func (s *Server) NextCreatedSessionSequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createdSessionSequence++
	return s.createdSessionSequence
}

// SetSessionAPIHandler sets the HTTP handler for the /api/session/ endpoints.
// This enables CLI commands for session management (new, list, kill, rename).
// Phase 9.5: Enables CLI session commands.
func (s *Server) SetSessionAPIHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionAPIHandler = handler
}

// SetTmuxManager sets the tmux manager for session integration.
// This enables mobile clients to list and attach to tmux sessions.
// Phase 12: tmux session integration.
func (s *Server) SetTmuxManager(mgr *tmux.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tmuxManager = mgr
}

// GetTmuxManager returns the tmux manager for external access.
func (s *Server) GetTmuxManager() *tmux.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tmuxManager
}

// SetTmuxAPIHandler sets the HTTP handler for the /api/tmux/ endpoints.
// This enables CLI commands for tmux session management (list-tmux, attach-tmux, detach).
// Phase 12.9: Enables CLI tmux commands.
func (s *Server) SetTmuxAPIHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tmuxAPIHandler = handler
}

// SetKeepAwakePolicyHandler sets the HTTP handler for the /api/keep-awake/policy endpoint.
// This enables CLI-driven keep-awake policy mutation.
// Phase 18: Enables "pseudocoder keep-awake enable/disable" commands.
func (s *Server) SetKeepAwakePolicyHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keepAwakePolicyHandler = handler
}

// SetFlagsAPIHandler sets the HTTP handler for the /api/flags endpoint.
// Phase 7: Enables CLI flag management commands.
func (s *Server) SetFlagsAPIHandler(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flagsAPIHandler = handler
}

// SetFileOperations sets the file operations handler for the file explorer.
// This enables mobile clients to list directories and read files within the repo.
// Phase 3: Enables file explorer protocol.
func (s *Server) SetFileOperations(ops FileOperations) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileOps = ops
}

// SetMetricsStore sets the metrics store for rollout metrics collection.
// Phase 7: Enables recording of crash, latency, and pairing metrics.
func (s *Server) SetMetricsStore(store storage.MetricsStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metricsStore = store
}

// GetMetricsStore returns the metrics store for external access.
func (s *Server) GetMetricsStore() storage.MetricsStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metricsStore
}
