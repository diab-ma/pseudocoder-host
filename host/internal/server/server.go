// Package server provides the WebSocket server for client connections.
// It handles streaming terminal output, review cards, and other messages
// between the host and mobile clients.
//
// The package is split across multiple files:
//   - server_types.go: Constants, Server/Client structs, handler types
//   - server_lifecycle.go: Start, StartAsync, StartAsyncTLS, Stop
//   - server_http.go: HTTP routing, WebSocket upgrade, token extraction
//   - server_broadcast.go: Broadcast methods and runBroadcaster
//   - server_setters.go: All Set*/Get* methods
//   - client_io.go: Client I/O (writePump, readPump, closeSend)
//   - handlers_helpers.go: Common send helpers (sendDecisionResult, sendError, etc.)
//   - handlers_decision.go: Review/chunk decision + delete handlers
//   - handlers_undo.go: Undo + chunk undo handlers
//   - handlers_terminal.go: Terminal input + resize handlers
//   - handlers_approval.go: Approval decision handler
//   - handlers_repo.go: Repo status/commit/push handlers
//   - handlers_sessions.go: Session create/close/switch/rename handlers
//   - handlers_tmux.go: Tmux list/attach/detach handlers
//   - server.go: HTTP handlers for non-WebSocket endpoints (this file)
//   - message.go: Message types and payloads
//   - approval.go: Approval queue for CLI command approval
//   - git.go: Git operations for commit/push workflows
//   - session_api.go: Session management API handlers
//   - tmux_api.go: Tmux API handlers
//   - status_handler.go: Status endpoint handler
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/config"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/netutil"
)

// DeviceStore is the interface for device storage operations.
// This allows the server to delete devices without importing the storage package.
type DeviceStore interface {
	GetDevice(id string) (*DeviceInfo, error)
	DeleteDevice(id string) error
}

// DeviceInfo represents a paired device (minimal interface for server package).
type DeviceInfo struct {
	ID   string
	Name string
}

// RevokeDeviceHandler handles the HTTP endpoint for device revocation.
// It closes active connections for the device and removes it from storage.
type RevokeDeviceHandler struct {
	server      *Server
	deviceStore DeviceStore
}

// NewRevokeDeviceHandler creates a handler for the /devices/{id}/revoke endpoint.
// The server is used to close active WebSocket connections for the revoked device.
// The store is used to delete the device from persistent storage.
func NewRevokeDeviceHandler(server *Server, store DeviceStore) *RevokeDeviceHandler {
	return &RevokeDeviceHandler{server: server, deviceStore: store}
}

// ServeHTTP handles POST /devices/{id}/revoke requests.
// This endpoint is restricted to loopback (localhost) requests only for security.
// The handler first closes any active WebSocket connections for the device,
// then removes the device from storage.
func (h *RevokeDeviceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses
	if !isLoopbackRequest(r) {
		log.Printf("server: rejected device revoke from non-loopback address: %s", r.RemoteAddr)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "Device revocation is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error":   "method_not_allowed",
			"message": "Only POST is allowed",
		})
		return
	}

	// Extract device ID from URL path: /devices/{id}/revoke
	// Path format: /devices/UUID/revoke
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) != 3 || pathParts[0] != "devices" || pathParts[2] != "revoke" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_path",
			"message": "Expected path format: /devices/{id}/revoke",
		})
		return
	}
	deviceID := pathParts[1]

	if deviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_device_id",
			"message": "Device ID is required",
		})
		return
	}

	// Check if device exists
	device, err := h.deviceStore.GetDevice(deviceID)
	if err != nil {
		log.Printf("server: failed to lookup device %s: %v", deviceID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "lookup_failed",
			"message": "Failed to lookup device",
		})
		return
	}
	if device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": "Device not found",
		})
		return
	}

	// Close active connections first (before deleting from storage)
	// This ensures the device cannot use an existing connection after revocation
	closedCount := h.server.CloseDeviceConnections(deviceID)

	// Delete from storage
	if err := h.deviceStore.DeleteDevice(deviceID); err != nil {
		log.Printf("server: failed to delete device %s: %v", deviceID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "delete_failed",
			"message": "Failed to delete device",
		})
		return
	}

	log.Printf("server: revoked device %s (%s), closed %d connection(s)", deviceID, device.Name, closedCount)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"device_id":          deviceID,
		"device_name":        device.Name,
		"connections_closed": closedCount,
	})
}

// isLoopbackRequest checks if the HTTP request came from the local machine.
// Delegates to the shared netutil.IsLoopbackRequest implementation.
func isLoopbackRequest(r *http.Request) bool {
	return netutil.IsLoopbackRequest(r)
}

// ApprovalTokenValidator validates approval tokens for the /approve endpoint.
// This interface allows mocking the token validation in tests.
type ApprovalTokenValidator interface {
	ValidateToken(token string) bool
}

// ApproveHandler handles the HTTP endpoint for CLI command approval.
// It validates the request, extracts the bearer token, and calls the approval queue.
// Phase 6.1b: HTTP endpoint for CLI command approval broker.
//
// Security notes:
//   - Loopback only: Requests from non-loopback addresses are rejected with 403.
//   - Token auth: Bearer token is validated before processing the request.
//   - Context cancellation: If the HTTP request context is cancelled (client
//     disconnect), the approval queue handles cleanup and returns context.Canceled.
//
// Accepted risks:
//   - No request body size limit: Large request bodies are accepted. This is
//     acceptable as the endpoint is localhost-only and authenticated.
//   - Response always 200: Approval decisions (denied/timeout) return 200 with
//     approved=false. This simplifies client handling and matches REST conventions
//     where the request itself succeeded even if the business logic result is negative.
type ApproveHandler struct {
	server         *Server
	tokenValidator ApprovalTokenValidator
}

// NewApproveHandler creates a handler for the /approve endpoint.
// The server provides access to the approval queue.
// The tokenValidator is used to authenticate requests.
func NewApproveHandler(server *Server, tokenValidator ApprovalTokenValidator) *ApproveHandler {
	return &ApproveHandler{
		server:         server,
		tokenValidator: tokenValidator,
	}
}

// approveRequest is the JSON request body for POST /approve.
type approveRequest struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
	Repo      string `json:"repo"`
	Rationale string `json:"rationale"`
}

// approveResponse is the JSON response for POST /approve.
type approveResponse struct {
	Approved            bool    `json:"approved"`
	Error               string  `json:"error,omitempty"`
	Message             string  `json:"message,omitempty"`
	TemporaryAllowUntil *string `json:"temporary_allow_until,omitempty"`
}

// ServeHTTP handles POST /approve requests from CLI tools.
// This endpoint is restricted to loopback (localhost) requests and requires
// a valid Bearer token in the Authorization header.
func (h *ApproveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback addresses
	if !isLoopbackRequest(r) {
		log.Printf("server: rejected /approve from non-loopback address: %s", r.RemoteAddr)
		writeJSON(w, http.StatusForbidden, approveResponse{
			Approved: false,
			Error:    "forbidden",
			Message:  "Approval endpoint is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, approveResponse{
			Approved: false,
			Error:    "method_not_allowed",
			Message:  "Only POST is allowed",
		})
		return
	}

	// Extract and validate Bearer token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		writeJSON(w, http.StatusUnauthorized, approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Authorization header required",
		})
		return
	}

	// Parse Bearer token
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		writeJSON(w, http.StatusUnauthorized, approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Invalid authorization format (expected Bearer token)",
		})
		return
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)

	// Validate token
	if !h.tokenValidator.ValidateToken(token) {
		log.Printf("server: rejected /approve with invalid token")
		writeJSON(w, http.StatusUnauthorized, approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Invalid approval token",
		})
		return
	}

	// Parse request body
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, approveResponse{
			Approved: false,
			Error:    "invalid_request",
			Message:  fmt.Sprintf("Invalid JSON body: %v", err),
		})
		return
	}

	// Validate required fields
	if req.Command == "" {
		writeJSON(w, http.StatusBadRequest, approveResponse{
			Approved: false,
			Error:    "invalid_request",
			Message:  "Command is required",
		})
		return
	}

	// Get approval queue
	queue := h.server.GetApprovalQueue()
	if queue == nil {
		log.Printf("server: /approve called but no approval queue configured")
		writeJSON(w, http.StatusServiceUnavailable, approveResponse{
			Approved: false,
			Error:    "service_unavailable",
			Message:  "Approval queue not configured",
		})
		return
	}

	// Submit approval request and wait for decision
	approvalReq := ApprovalRequest{
		Command:   req.Command,
		Cwd:       req.Cwd,
		Repo:      req.Repo,
		Rationale: req.Rationale,
	}

	log.Printf("server: /approve request for command %q", req.Command)
	response, err := queue.Queue(r.Context(), approvalReq)

	// Build response
	result := approveResponse{
		Approved: err == nil && response.Decision == "approve",
	}

	if err != nil {
		// Extract error code and message
		result.Error = apperrors.GetCode(err)
		result.Message = apperrors.GetMessage(err)
	}

	if response.TemporaryAllowUntil != nil {
		ts := response.TemporaryAllowUntil.Format(time.RFC3339)
		result.TemporaryAllowUntil = &ts
	}

	writeJSON(w, http.StatusOK, result)
}

// KeepAwakePolicyHandler handles POST /api/keep-awake/policy for CLI-driven
// keep-awake policy mutation with atomic validate -> persist -> apply -> broadcast.
type KeepAwakePolicyHandler struct {
	server         *Server
	tokenValidator ApprovalTokenValidator
	configPath     string
	persistPolicy  func(string, bool) error
}

// NewKeepAwakePolicyHandler creates a handler for the /api/keep-awake/policy endpoint.
func NewKeepAwakePolicyHandler(server *Server, tokenValidator ApprovalTokenValidator, configPath string) *KeepAwakePolicyHandler {
	return &KeepAwakePolicyHandler{
		server:         server,
		tokenValidator: tokenValidator,
		configPath:     configPath,
		persistPolicy:  config.PersistKeepAwakePolicy,
	}
}

// keepAwakePolicyRequest is the JSON request body for POST /api/keep-awake/policy.
type keepAwakePolicyRequest struct {
	RequestID     string `json:"request_id"`
	RemoteEnabled *bool  `json:"remote_enabled"`
	Reason        string `json:"reason,omitempty"`
}

func (h *KeepAwakePolicyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. POST-only.
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, KeepAwakePolicyMutationResponse{
			ErrorCode: "keep_awake.method_not_allowed",
			Error:     "Only POST is allowed",
		})
		return
	}

	// 2. Loopback enforcement.
	if !isLoopbackRequest(r) {
		log.Printf("server: rejected /api/keep-awake/policy from non-loopback: %s", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnauthorized,
			Error:     "Policy endpoint is only available from localhost",
		})
		return
	}

	// 3. Token validator unavailable -> 503.
	if h.tokenValidator == nil {
		writeJSON(w, http.StatusServiceUnavailable, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnavailable,
			Error:     "Authentication system unavailable",
		})
		return
	}

	// 4. Bearer token extraction and validation.
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		writeJSON(w, http.StatusUnauthorized, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnauthorized,
			Error:     "Authorization header required",
		})
		return
	}
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		writeJSON(w, http.StatusUnauthorized, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnauthorized,
			Error:     "Invalid authorization format (expected Bearer token)",
		})
		return
	}
	tokenRaw := strings.TrimPrefix(authHeader, bearerPrefix)
	tokenRaw = strings.TrimSpace(tokenRaw)
	if tokenRaw == "" {
		writeJSON(w, http.StatusUnauthorized, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnauthorized,
			Error:     "Authorization token is required",
		})
		return
	}
	if !h.tokenValidator.ValidateToken(tokenRaw) {
		log.Printf("server: rejected /api/keep-awake/policy with invalid token")
		writeJSON(w, http.StatusUnauthorized, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeKeepAwakeUnauthorized,
			Error:     "Invalid approval token",
		})
		return
	}

	// 5. Caller identity.
	callerIdentity := sha256Hex(tokenRaw)

	// 6. Parse request body with strict JSON.
	var req keepAwakePolicyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     fmt.Sprintf("Invalid JSON: %v", err),
		})
		return
	}
	if req.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, KeepAwakePolicyMutationResponse{
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     "request_id is required",
		})
		return
	}
	if req.RemoteEnabled == nil {
		writeJSON(w, http.StatusBadRequest, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     "remote_enabled is required",
		})
		return
	}

	// 7. Normalize reason.
	reason, err := normalizeKeepAwakePolicyReason(req.Reason)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     err.Error(),
		})
		return
	}

	remoteEnabled := *req.RemoteEnabled
	fingerprint := computeFingerprint("policy_mutation", fmt.Sprintf("%t", remoteEnabled), reason)

	// 8. Lock -> idempotency check -> no-op detect -> reserve.
	idKey := policyIdempotencyKey{CallerIdentity: callerIdentity, RequestID: req.RequestID}
	h.server.keepAwake.mu.Lock()
	cached, replay, idErr := h.server.policyIdempotencyCheckLocked(idKey, fingerprint)
	if replay {
		h.server.keepAwake.mu.Unlock()
		writeJSON(w, http.StatusOK, cached)
		return
	}
	if idErr != nil {
		h.server.keepAwake.mu.Unlock()
		writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeKeepAwakeConflict,
			Error:     idErr.Error(),
		})
		return
	}

	// No-op detection: if policy already matches, return success without persist.
	currentEnabled := h.server.keepAwake.policy.RemoteEnabled
	if currentEnabled == remoteEnabled {
		now := h.server.keepAwake.now()
		status := h.server.keepAwakeStatusPayloadLocked(now)
		h.server.policyIdempotencyReserveLocked(idKey)
		resp := KeepAwakePolicyMutationResponse{
			RequestID:      req.RequestID,
			Success:        true,
			StatusRevision: status.StatusRevision,
			KeepAwake:      status,
			HotApplied:     true,
			Persisted:      false,
		}
		h.server.policyIdempotencyCompleteLocked(idKey, fingerprint, resp)
		h.server.keepAwake.mu.Unlock()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Reserve in-flight.
	h.server.policyIdempotencyReserveLocked(idKey)
	oldPolicy := h.server.keepAwake.policy

	// Unlock before persist I/O.
	h.server.keepAwake.mu.Unlock()

	// 9. Persist to disk (no lock held, flock guards file contention).
	if h.configPath == "" {
		h.server.keepAwake.mu.Lock()
		h.server.policyIdempotencyCancelLocked(idKey)
		h.server.keepAwake.mu.Unlock()
		writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeKeepAwakeConflict,
			Error:     "config path is unavailable; cannot persist keep-awake policy",
		})
		return
	}
	if err := h.persistPolicy(h.configPath, remoteEnabled); err != nil {
		log.Printf("keep-awake policy persist failed: %v", err)
		h.server.keepAwake.mu.Lock()
		h.server.policyIdempotencyCancelLocked(idKey)
		h.server.keepAwake.mu.Unlock()
		writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeKeepAwakeConflict,
			Error:     "policy persistence failed: " + err.Error(),
		})
		return
	}
	persisted := true

	// 10. Re-lock -> apply policy -> revoke leases if disabling -> audit -> revision++ -> snapshot.
	h.server.keepAwake.mu.Lock()
	newPolicy := h.server.keepAwake.policy
	newPolicy.RemoteEnabled = remoteEnabled
	h.server.keepAwake.policy = newPolicy

	now := h.server.keepAwake.now()

	// Write audit.
	auditReason := reason
	if auditReason == "" {
		if remoteEnabled {
			auditReason = "cli_enable"
		} else {
			auditReason = "cli_disable"
		}
	}
	if err := h.server.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
		Operation:     "policy_change",
		RequestID:     req.RequestID,
		ActorDeviceID: "cli:" + callerIdentity[:8],
		Reason:        auditReason,
		At:            now,
	}); err != nil {
		// Audit write failed -> rollback.
		h.server.keepAwake.policy = oldPolicy
		h.server.policyIdempotencyCancelLocked(idKey)
		h.server.keepAwake.mu.Unlock()

		// Rollback persist.
		if persisted && h.configPath != "" {
			if rbErr := h.persistPolicy(h.configPath, oldPolicy.RemoteEnabled); rbErr != nil {
				log.Printf("keep-awake policy rollback persist failed: %v", rbErr)
				writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
					RequestID: req.RequestID,
					ErrorCode: apperrors.CodeKeepAwakeConflict,
					Error:     "policy apply failed and rollback failed; manually restore keep_awake_remote_enabled in config: " + rbErr.Error(),
				})
				return
			}
		}

		writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
			RequestID: req.RequestID,
			ErrorCode: apperrors.CodeKeepAwakeConflict,
			Error:     "policy apply failed; rollback completed: " + apperrors.GetMessage(err),
		})
		return
	}

	// Disable dominance: revoke all leases when disabling.
	if oldPolicy.RemoteEnabled && !remoteEnabled && len(h.server.keepAwake.leases) > 0 {
		for key, lease := range h.server.keepAwake.leases {
			if err := h.server.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
				Operation:      "policy_revoke",
				ActorDeviceID:  "cli:" + callerIdentity[:8],
				TargetDeviceID: lease.DeviceID,
				SessionID:      lease.SessionID,
				LeaseID:        lease.LeaseID,
				Reason:         "remote_disabled_by_cli",
				At:             now,
			}); err != nil {
				status := h.server.keepAwakeFailClosedOnPolicyAuditErrorLocked(now, err)
				h.server.policyIdempotencyCancelLocked(idKey)
				h.server.keepAwake.mu.Unlock()
				changedMsg := NewKeepAwakeChangedMessage(status)
				go h.server.broadcastKeepAwakeChanged(changedMsg, nil, true)
				writeJSON(w, http.StatusConflict, KeepAwakePolicyMutationResponse{
					RequestID: req.RequestID,
					ErrorCode: apperrors.CodeKeepAwakeConflict,
					Error:     "policy apply failed; keep-awake forced fail-closed after revoke audit error",
				})
				return
			}
			if timer := h.server.keepAwake.timers[key]; timer != nil {
				timer.Stop()
				delete(h.server.keepAwake.timers, key)
			}
			delete(h.server.keepAwake.leases, key)
		}
		h.server.keepAwakeSetRuntimeStateLocked(false)
	}

	h.server.keepAwake.statusRevision++
	revision := h.server.keepAwake.statusRevision
	status := h.server.keepAwakeStatusPayloadLocked(now)

	resp := KeepAwakePolicyMutationResponse{
		RequestID:      req.RequestID,
		Success:        true,
		StatusRevision: revision,
		KeepAwake:      status,
		HotApplied:     true,
		Persisted:      persisted,
	}
	h.server.policyIdempotencyCompleteLocked(idKey, fingerprint, resp)
	h.server.keepAwake.mu.Unlock()

	// 11. Broadcast (non-blocking, failure preserves committed truth).
	changedMsg := NewKeepAwakeChangedMessage(status)
	go h.server.broadcastKeepAwakeChanged(changedMsg, nil, true)

	// 12. Return 200.
	writeJSON(w, http.StatusOK, resp)
}

// sha256Hex returns the hex-encoded SHA256 digest of s.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
