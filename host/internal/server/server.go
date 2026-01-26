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
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "forbidden",
			"message": "Device revocation is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "method_not_allowed",
			"message": "Only POST is allowed",
		})
		return
	}

	// Extract device ID from URL path: /devices/{id}/revoke
	// Path format: /devices/UUID/revoke
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) != 3 || pathParts[0] != "devices" || pathParts[2] != "revoke" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "invalid_path",
			"message": "Expected path format: /devices/{id}/revoke",
		})
		return
	}
	deviceID := pathParts[1]

	if deviceID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "missing_device_id",
			"message": "Device ID is required",
		})
		return
	}

	// Check if device exists
	device, err := h.deviceStore.GetDevice(deviceID)
	if err != nil {
		log.Printf("server: failed to lookup device %s: %v", deviceID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "lookup_failed",
			"message": "Failed to lookup device",
		})
		return
	}
	if device == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "delete_failed",
			"message": "Failed to delete device",
		})
		return
	}

	log.Printf("server: revoked device %s (%s), closed %d connection(s)", deviceID, device.Name, closedCount)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"device_id":          deviceID,
		"device_name":        device.Name,
		"connections_closed": closedCount,
	})
}

// isLoopbackRequest checks if the HTTP request came from the local machine.
// This is used to restrict sensitive endpoints to local-only access.
// Returns true for loopback or local interface addresses.
// Note: This duplicates the function in auth/handler.go to avoid circular imports.
func isLoopbackRequest(r *http.Request) bool {
	// Extract the host part from RemoteAddr (format is "host:port" or "[host]:port" for IPv6)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// If we can't parse the address, be conservative and reject
		log.Printf("server: failed to parse RemoteAddr %q: %v", r.RemoteAddr, err)
		return false
	}

	// Parse the IP address
	ip := net.ParseIP(host)
	if ip == nil {
		// If we can't parse the IP, be conservative and reject
		log.Printf("server: failed to parse IP from host %q", host)
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	return isLocalInterfaceIP(ip)
}

func isLocalInterfaceIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("server: failed to list interfaces: %v", err)
		return false
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var localIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				localIP = v.IP
			case *net.IPAddr:
				localIP = v.IP
			}
			if localIP == nil {
				continue
			}
			if localIP4 := localIP.To4(); localIP4 != nil {
				localIP = localIP4
			}
			if localIP.Equal(ip) {
				return true
			}
		}
	}

	return false
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "forbidden",
			Message:  "Approval endpoint is only available from localhost",
		})
		return
	}

	// Only accept POST
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "method_not_allowed",
			Message:  "Only POST is allowed",
		})
		return
	}

	// Extract and validate Bearer token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Authorization header required",
		})
		return
	}

	// Parse Bearer token
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "approval.invalid_token",
			Message:  "Invalid approval token",
		})
		return
	}

	// Parse request body
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(approveResponse{
			Approved: false,
			Error:    "invalid_request",
			Message:  fmt.Sprintf("Invalid JSON body: %v", err),
		})
		return
	}

	// Validate required fields
	if req.Command == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(approveResponse{
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(approveResponse{
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}
