package auth

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

// PairRequest is the JSON body for the /pair endpoint.
type PairRequest struct {
	// Code is the 6-digit pairing code displayed by `pseudocoder pair`.
	Code string `json:"code"`

	// DeviceName is a friendly name for the device (e.g., "iPhone 15 Pro").
	DeviceName string `json:"device_name"`
}

// PairResponse is the JSON response from the /pair endpoint on success.
type PairResponse struct {
	// DeviceID is the unique identifier for the paired device.
	DeviceID string `json:"device_id"`

	// Token is the bearer token for authentication.
	// This is only returned once and should be stored securely by the client.
	Token string `json:"token"`
}

// ErrorResponse is the JSON response for error conditions.
type ErrorResponse struct {
	// Error is a machine-readable legacy error code (e.g., "invalid_code").
	Error string `json:"error"`

	// ErrorCode is the stable dotted taxonomy code (e.g., "auth.pair_invalid_code").
	ErrorCode string `json:"error_code"`

	// Message is a human-readable description.
	Message string `json:"message"`

	// NextAction is the single primary recovery action for the operator.
	NextAction string `json:"next_action"`
}

// PairingMetricsRecorder is an optional callback for recording pairing metrics.
// Phase 7: Rollout metrics collection.
type PairingMetricsRecorder func(deviceID string, success bool)

// PairHandler handles the /pair HTTP endpoint for code-to-token exchange.
// It validates the pairing code and returns a device token on success.
type PairHandler struct {
	pairingManager  *PairingManager
	metricsRecorder PairingMetricsRecorder
}

// NewPairHandler creates a new pair handler.
func NewPairHandler(pm *PairingManager) *PairHandler {
	return &PairHandler{pairingManager: pm}
}

// SetMetricsRecorder sets an optional callback for recording pairing metrics.
func (h *PairHandler) SetMetricsRecorder(recorder PairingMetricsRecorder) {
	h.metricsRecorder = recorder
}

// ServeHTTP handles POST /pair requests.
func (h *PairHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only accept POST requests
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", hostErrors.CodeAuthPairMethodNotAllowed, "Only POST is allowed")
		return
	}

	// Parse the request body
	var req PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("auth: failed to parse pair request: %v", err)
		h.writeError(w, http.StatusBadRequest, "invalid_request", hostErrors.CodeAuthPairInvalidRequest, "Invalid JSON body")
		return
	}

	// Validate required fields
	if req.Code == "" {
		h.writeError(w, http.StatusBadRequest, "missing_code", hostErrors.CodeAuthPairMissingCode, "Pairing code is required")
		return
	}

	// Default device name if not provided
	deviceName := req.DeviceName
	if deviceName == "" {
		deviceName = "Unknown Device"
	}

	// Validate the code and get device token
	deviceID, token, err := h.pairingManager.ValidateCode(req.Code, deviceName)
	if err != nil {
		if h.metricsRecorder != nil {
			h.metricsRecorder("", false)
		}
		switch err {
		case ErrCodeInvalid:
			h.writeError(w, http.StatusUnauthorized, "invalid_code", hostErrors.CodeAuthPairInvalidCode, "Invalid pairing code")
		case ErrCodeExpired:
			h.writeError(w, http.StatusUnauthorized, "expired_code", hostErrors.CodeAuthPairExpiredCode, "Pairing code has expired")
		case ErrCodeUsed:
			h.writeError(w, http.StatusUnauthorized, "used_code", hostErrors.CodeAuthPairUsedCode, "Pairing code has already been used")
		case ErrRateLimited:
			h.writeError(w, http.StatusTooManyRequests, "rate_limited", hostErrors.CodeAuthPairRateLimited, "Too many pairing attempts, please wait")
		default:
			log.Printf("auth: unexpected error during pairing: %v", err)
			h.writeError(w, http.StatusInternalServerError, "internal_error", hostErrors.CodeAuthPairInternal, "Failed to complete pairing")
		}
		return
	}

	// Success - return device ID and token
	if h.metricsRecorder != nil {
		h.metricsRecorder(deviceID, true)
	}
	log.Printf("auth: device paired successfully: %s (%s)", deviceID, deviceName)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(PairResponse{
		DeviceID: deviceID,
		Token:    token,
	})
}

// writeError sends a JSON error response with taxonomy code and next action.
func (h *PairHandler) writeError(w http.ResponseWriter, status int, legacyCode, taxonomyCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error:      legacyCode,
		ErrorCode:  taxonomyCode,
		Message:    message,
		NextAction: hostErrors.GetNextAction(taxonomyCode),
	})
}

// GenerateCodeResponse is the JSON response for /pair/generate.
type GenerateCodeResponse struct {
	Code   string    `json:"code"`
	Expiry time.Time `json:"expiry"`
}

// GenerateCodeHandler handles the /pair/generate endpoint.
// This is called by the `pseudocoder pair` CLI command to generate a code.
type GenerateCodeHandler struct {
	pairingManager *PairingManager
}

// NewGenerateCodeHandler creates a new generate code handler.
func NewGenerateCodeHandler(pm *PairingManager) *GenerateCodeHandler {
	return &GenerateCodeHandler{pairingManager: pm}
}

// isLoopbackRequest checks if the request originates from the local machine.
// This is used to restrict sensitive endpoints like /pair/generate to local access only.
// Returns true for loopback or unix socket addresses.
func isLoopbackRequest(r *http.Request) bool {
	// Extract the host part from RemoteAddr (format is "host:port" or "[host]:port" for IPv6)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		if isUnixSocketRemoteAddr(r.RemoteAddr) {
			return true
		}
		// If we can't parse the address, be conservative and reject
		log.Printf("auth: failed to parse RemoteAddr %q: %v", r.RemoteAddr, err)
		return false
	}

	// Parse the IP address
	ip := net.ParseIP(host)
	if ip == nil {
		// If we can't parse the IP, be conservative and reject
		log.Printf("auth: failed to parse IP from host %q", host)
		return false
	}

	return ip.IsLoopback()
}

func isUnixSocketRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return true
	}
	if strings.HasPrefix(remoteAddr, "/") || strings.HasPrefix(remoteAddr, "@") {
		return true
	}
	return false
}

// writeError sends a JSON error response with taxonomy code and next action.
func (h *GenerateCodeHandler) writeError(w http.ResponseWriter, status int, legacyCode, taxonomyCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error:      legacyCode,
		ErrorCode:  taxonomyCode,
		Message:    message,
		NextAction: hostErrors.GetNextAction(taxonomyCode),
	})
}

// ServeHTTP handles POST /pair/generate requests.
// This endpoint is restricted to loopback (localhost) or unix socket requests only for security.
// Remote access to pairing code generation would allow attackers to generate codes
// and potentially race legitimate users to complete pairing.
func (h *GenerateCodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: Only allow requests from loopback or unix socket addresses.
	// This ensures that pairing codes can only be generated by someone with
	// local access to the host machine (e.g., via SSH or direct terminal).
	if !isLoopbackRequest(r) {
		log.Printf("auth: rejected /pair/generate from non-loopback address: %s", r.RemoteAddr)
		h.writeError(w, http.StatusForbidden, "forbidden", hostErrors.CodeAuthPairGenerateForbidden, "Pairing code generation is only available from localhost")
		return
	}

	// Only accept POST requests
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", hostErrors.CodeAuthPairGenerateMethodNotAllowed, "Only POST is allowed")
		return
	}

	// Generate a new pairing code
	code, err := h.pairingManager.GenerateCode()
	if err != nil {
		log.Printf("auth: failed to generate pairing code: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", hostErrors.CodeAuthPairGenerateInternal, "Failed to generate pairing code")
		return
	}

	expiry := h.pairingManager.GetCodeExpiry()

	log.Printf("auth: generated pairing code via /pair/generate endpoint")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(GenerateCodeResponse{
		Code:   code,
		Expiry: expiry,
	})
}
