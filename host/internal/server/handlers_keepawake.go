package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/keepawake"
)

const (
	minKeepAwakeDurationMs = int64(30000)
	maxKeepAwakeDurationMs = int64(28800000)
	maxKeepAwakeGraceMs    = int64(300000)

	keepAwakeResultSendTimeout = 250 * time.Millisecond
)

// KeepAwakeRuntimeManager abstracts host runtime keep-awake state transitions.
type KeepAwakeRuntimeManager interface {
	SetDesiredEnabled(ctx context.Context, enabled bool) keepawake.Status
}

// KeepAwakeAuditEvent is the process-local keep-awake audit record.
type KeepAwakeAuditEvent struct {
	Operation      string
	RequestID      string
	ActorDeviceID  string
	TargetDeviceID string
	SessionID      string
	LeaseID        string
	Reason         string
	At             time.Time
}

// KeepAwakeAuditWriter persists process-local keep-awake audit records.
type KeepAwakeAuditWriter interface {
	WriteKeepAwakeAudit(event KeepAwakeAuditEvent) error
}

// KeepAwakePolicyConfig controls keep-awake remote mutation authorization.
type KeepAwakePolicyConfig struct {
	RemoteEnabled              bool
	AllowAdminRevoke           bool
	AdminDeviceIDs             []string
	AllowOnBattery             bool
	AutoDisableBatteryPercent  int
}

type keepAwakeLease struct {
	SessionID      string
	DeviceID       string
	LeaseID        string
	Reason         string
	ExpiresAt      time.Time
	Disconnected   bool
	GraceDeadline  time.Time
	DisconnectedAt time.Time
}

type keepAwakeMutationCacheKey struct {
	DeviceID  string
	OpType    MessageType
	RequestID string
}

type keepAwakeControlPlane struct {
	mu sync.Mutex

	runtime     KeepAwakeRuntimeManager
	auditWriter KeepAwakeAuditWriter
	policy      KeepAwakePolicyConfig
	policySet   bool
	grace       time.Duration

	powerProvider             keepawake.PowerProvider
	thresholdDisabledThisEpoch bool
	migrationFailed           bool

	serverBootID   string
	statusRevision int64
	runtimeState   string
	runtimeReason  string

	leases map[string]*keepAwakeLease
	timers map[string]*time.Timer

	nextLeaseSeq    int64
	leaseIDGenerate func(seq int64) string
	now             func() time.Time

	idempotencyEntries map[keepAwakeMutationCacheKey]*idempotencyCacheEntry
	idempotencyOrder   []keepAwakeMutationCacheKey
	inFlightMutations  map[keepAwakeMutationCacheKey]*inFlightMutation

	// Policy mutation idempotency (HTTP-only, separate key space from lease mutations).
	// Keyed by (callerIdentity=sha256(bearer_token), requestID).
	policyIdempotencyEntries map[policyIdempotencyKey]*policyIdempotencyEntry
	policyIdempotencyOrder   []policyIdempotencyKey
	policyInFlight           map[policyIdempotencyKey]struct{}
}

// policyIdempotencyKey uniquely identifies a policy mutation request.
type policyIdempotencyKey struct {
	CallerIdentity string
	RequestID      string
}

// policyIdempotencyEntry stores a cached policy mutation result.
type policyIdempotencyEntry struct {
	Fingerprint string
	Response    KeepAwakePolicyMutationResponse
}

func newKeepAwakeControlPlane() *keepAwakeControlPlane {
	boot := fmt.Sprintf("boot-%d", time.Now().UnixNano())
	return &keepAwakeControlPlane{
		policy: KeepAwakePolicyConfig{RemoteEnabled: false},
		grace:  45 * time.Second,

		serverBootID:   boot,
		statusRevision: 0,
		runtimeState:   string(keepawake.StateOff),

		leases: make(map[string]*keepAwakeLease),
		timers: make(map[string]*time.Timer),

		leaseIDGenerate: func(seq int64) string {
			return fmt.Sprintf("ka-%d", seq)
		},
		now: time.Now,

		idempotencyEntries: make(map[keepAwakeMutationCacheKey]*idempotencyCacheEntry),
		inFlightMutations:  make(map[keepAwakeMutationCacheKey]*inFlightMutation),

		policyIdempotencyEntries: make(map[policyIdempotencyKey]*policyIdempotencyEntry),
		policyInFlight:           make(map[policyIdempotencyKey]struct{}),
	}
}

// normalizeKeepAwakePolicyReason sanitizes a user-provided reason string:
// trim whitespace, collapse internal whitespace, printable ASCII only, <=256 bytes.
func normalizeKeepAwakePolicyReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", nil
	}
	// Collapse internal whitespace while validating allowed characters.
	var b strings.Builder
	prevSpace := false
	for _, r := range reason {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		if r < 0x20 || r > 0x7E {
			return "", fmt.Errorf("reason must contain printable ASCII characters only")
		}
		prevSpace = false
		b.WriteRune(r)
	}
	result := strings.TrimSpace(b.String())
	if len(result) > 256 {
		return "", fmt.Errorf("reason must be 256 bytes or fewer after normalization")
	}
	return result, nil
}

func keepAwakeLeaseKey(sessionID, deviceID, leaseID string) string {
	return sessionID + "\x00" + deviceID + "\x00" + leaseID
}

func keepAwakeFingerprint(fields ...string) string {
	return computeFingerprint(fields...)
}

func parseKeepAwakeEnvelope(data []byte) (map[string]json.RawMessage, error) {
	var envelope struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Payload == nil {
		return map[string]json.RawMessage{}, nil
	}
	return envelope.Payload, nil
}

func rejectUnexpectedPayloadKeys(payload map[string]json.RawMessage, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range payload {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("unexpected payload field: %s", key)
		}
	}
	return nil
}

func parseRequiredStringField(payload map[string]json.RawMessage, field string) (string, error) {
	raw, ok := payload[field]
	if !ok {
		return "", fmt.Errorf("%s is required", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", field)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func parseOptionalStringField(payload map[string]json.RawMessage, field string) (string, error) {
	raw, ok := payload[field]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return strings.TrimSpace(value), nil
}

func parseStrictDurationMs(payload map[string]json.RawMessage, required bool) (int64, error) {
	if _, alias := payload["duration"]; alias {
		return 0, fmt.Errorf("duration_ms is required")
	}
	if _, alias := payload["lease_duration_ms"]; alias {
		return 0, fmt.Errorf("duration_ms is required")
	}
	raw, ok := payload["duration_ms"]
	if !ok {
		if required {
			return 0, fmt.Errorf("duration_ms is required")
		}
		return 0, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, fmt.Errorf("duration_ms must be an integer")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return 0, fmt.Errorf("duration_ms must be an integer")
	}
	numStr := num.String()
	if strings.ContainsAny(numStr, ".eE") {
		return 0, fmt.Errorf("duration_ms must be an integer")
	}
	parsed, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("duration_ms must be an integer")
	}
	if parsed < minKeepAwakeDurationMs || parsed > maxKeepAwakeDurationMs {
		return 0, fmt.Errorf("duration_ms out of range")
	}
	return parsed, nil
}

func (s *Server) keepAwakeStatusPayloadLocked(now time.Time) KeepAwakeStatusPayload {
	ka := s.keepAwake
	leases := make([]KeepAwakeLeasePayload, 0, len(ka.leases))
	var nextExpiry int64
	for _, lease := range ka.leases {
		remaining := lease.ExpiresAt.Sub(now).Milliseconds()
		if remaining < 0 {
			remaining = 0
		}
		entry := KeepAwakeLeasePayload{
			SessionID:     lease.SessionID,
			LeaseID:       lease.LeaseID,
			OwnerDeviceID: lease.DeviceID,
			ExpiresAtMs:   lease.ExpiresAt.UnixMilli(),
			RemainingMs:   remaining,
			Disconnected:  lease.Disconnected,
		}
		if lease.Disconnected {
			entry.GraceDeadlineMs = lease.GraceDeadline.UnixMilli()
		}
		leases = append(leases, entry)

		deadlineMs := lease.ExpiresAt.UnixMilli()
		if lease.Disconnected && !lease.GraceDeadline.IsZero() && lease.GraceDeadline.UnixMilli() < deadlineMs {
			deadlineMs = lease.GraceDeadline.UnixMilli()
		}
		if nextExpiry == 0 || deadlineMs < nextExpiry {
			nextExpiry = deadlineMs
		}
	}
	sort.Slice(leases, func(i, j int) bool {
		if leases[i].SessionID != leases[j].SessionID {
			return leases[i].SessionID < leases[j].SessionID
		}
		if leases[i].OwnerDeviceID != leases[j].OwnerDeviceID {
			return leases[i].OwnerDeviceID < leases[j].OwnerDeviceID
		}
		return leases[i].LeaseID < leases[j].LeaseID
	})

	payload := KeepAwakeStatusPayload{
		State:            ka.runtimeState,
		StatusRevision:   ka.statusRevision,
		ServerBootID:     ka.serverBootID,
		ActiveLeaseCount: len(ka.leases),
		Leases:           leases,
		Policy: KeepAwakePolicyPayload{
			RemoteEnabled:             ka.policy.RemoteEnabled,
			AllowAdminRevoke:          ka.policy.AllowAdminRevoke,
			AllowOnBattery:            ka.policy.AllowOnBattery,
			AutoDisableBatteryPercent: ka.policy.AutoDisableBatteryPercent,
		},
	}
	if ka.runtimeReason != "" {
		payload.DegradedReason = ka.runtimeReason
	}
	if nextExpiry > 0 {
		payload.NextExpiryMs = nextExpiry
	}

	// Populate power payload from provider + policy check.
	if ka.powerProvider != nil {
		snap := ka.powerProvider.Snapshot()
		powerAllowed, powerReason := s.keepAwakeCheckPowerPolicyLocked()
		payload.Power = &KeepAwakePowerPayload{
			OnBattery:      snap.OnBattery,
			BatteryPercent: snap.BatteryPercent,
			ExternalPower:  snap.ExternalPower,
			PolicyBlocked:  !powerAllowed,
			PolicyReason:   powerReason,
		}
	}

	// Recovery hint for degraded or migration failure states.
	if ka.migrationFailed {
		payload.RecoveryHint = "Audit storage migration failed. Restart host to retry."
	} else if ka.runtimeReason == string(keepawake.DegradedReasonUnsupportedEnvironment) {
		payload.RecoveryHint = "Keep-awake is not supported in this environment."
	} else if ka.runtimeReason == string(keepawake.DegradedReasonAcquireFailed) {
		payload.RecoveryHint = "Keep-awake inhibitor could not be acquired. Check host logs."
	} else if payload.Power != nil && payload.Power.PolicyBlocked {
		switch payload.Power.PolicyReason {
		case "requires_external_power":
			payload.RecoveryHint = "Connect external power to enable keep-awake."
		case "battery_threshold":
			payload.RecoveryHint = "Battery too low. Charge device to enable keep-awake."
		case "power_state_unknown":
			payload.RecoveryHint = "Host power state unknown. Check system configuration."
		}
	}

	return payload
}

func (s *Server) keepAwakeMutationResultFromStatusLocked(requestID, leaseID, errCode, errMsg string, success bool, now time.Time) KeepAwakeMutationResultPayload {
	status := s.keepAwakeStatusPayloadLocked(now)
	return KeepAwakeMutationResultPayload{
		RequestID:        requestID,
		Success:          success,
		LeaseID:          leaseID,
		ErrorCode:        errCode,
		Error:            errMsg,
		State:            status.State,
		StatusRevision:   status.StatusRevision,
		ServerBootID:     status.ServerBootID,
		ActiveLeaseCount: status.ActiveLeaseCount,
		Leases:           status.Leases,
		Policy:           status.Policy,
		DegradedReason:   status.DegradedReason,
		NextExpiryMs:     status.NextExpiryMs,
		Power:            status.Power,
		RecoveryHint:     status.RecoveryHint,
	}
}

func (s *Server) keepAwakeSetRuntimeStateLocked(desiredEnabled bool) {
	ka := s.keepAwake
	if ka.runtime == nil {
		if desiredEnabled {
			ka.runtimeState = string(keepawake.StateDegraded)
			ka.runtimeReason = string(keepawake.DegradedReasonUnsupportedEnvironment)
			return
		}
		ka.runtimeState = string(keepawake.StateOff)
		ka.runtimeReason = ""
		return
	}
	st := ka.runtime.SetDesiredEnabled(context.Background(), desiredEnabled)
	ka.runtimeState = string(st.State)
	if st.Reason != "" {
		ka.runtimeReason = string(st.Reason)
	} else {
		ka.runtimeReason = ""
	}
}

func (s *Server) keepAwakePruneExpiredLocked(now time.Time) bool {
	ka := s.keepAwake
	changed := false
	for key, lease := range ka.leases {
		expired := !lease.ExpiresAt.After(now)
		graceElapsed := lease.Disconnected && !lease.GraceDeadline.After(now)
		if expired || graceElapsed {
			if timer := ka.timers[key]; timer != nil {
				timer.Stop()
				delete(ka.timers, key)
			}
			delete(ka.leases, key)
			changed = true
		}
	}
	return changed
}

func (s *Server) keepAwakeScheduleTimerLocked(leaseKey string, lease *keepAwakeLease) {
	ka := s.keepAwake
	if timer := ka.timers[leaseKey]; timer != nil {
		timer.Stop()
	}
	now := ka.now()
	deadline := lease.ExpiresAt
	if lease.Disconnected && !lease.GraceDeadline.IsZero() && lease.GraceDeadline.Before(deadline) {
		deadline = lease.GraceDeadline
	}
	delay := deadline.Sub(now)
	if delay <= 0 {
		go s.keepAwakeOnTimer(leaseKey)
		return
	}
	ka.timers[leaseKey] = time.AfterFunc(delay, func() {
		s.keepAwakeOnTimer(leaseKey)
	})
}

func (s *Server) keepAwakeOnTimer(leaseKey string) {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()

	now := s.keepAwake.now()
	changed := s.keepAwakePruneExpiredLocked(now)
	thresholdRevoked := s.keepAwakeCheckBatteryThresholdLocked(now)
	if changed || thresholdRevoked {
		if changed && !thresholdRevoked {
			s.keepAwake.statusRevision++
		}
		s.keepAwakeSetRuntimeStateLocked(len(s.keepAwake.leases) > 0)
		status := s.keepAwakeStatusPayloadLocked(now)
		go s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
	}
}

// keepAwakeCheckPowerPolicyLocked checks whether power constraints allow mutation.
// Must be called with ka.mu held.
func (s *Server) keepAwakeCheckPowerPolicyLocked() (allowed bool, reason string) {
	ka := s.keepAwake
	hasConstraints := !ka.policy.AllowOnBattery || ka.policy.AutoDisableBatteryPercent > 0
	if !hasConstraints {
		return true, ""
	}

	if ka.powerProvider == nil {
		return false, "power_state_unknown"
	}
	snap := ka.powerProvider.Snapshot()
	if snap.OnBattery == nil {
		return false, "power_state_unknown"
	}

	if *snap.OnBattery && !ka.policy.AllowOnBattery {
		return false, "requires_external_power"
	}

	if *snap.OnBattery && ka.policy.AutoDisableBatteryPercent > 0 {
		if snap.BatteryPercent == nil {
			return false, "power_state_unknown"
		}
		if *snap.BatteryPercent <= ka.policy.AutoDisableBatteryPercent {
			return false, "battery_threshold"
		}
	}

	return true, ""
}

// keepAwakeCheckBatteryThresholdLocked checks if battery has dropped below threshold
// and auto-revokes all leases if so. Returns true if leases were revoked.
// Must be called with ka.mu held.
func (s *Server) keepAwakeCheckBatteryThresholdLocked(now time.Time) bool {
	ka := s.keepAwake
	if ka.policy.AutoDisableBatteryPercent == 0 {
		return false
	}
	if ka.thresholdDisabledThisEpoch {
		return false
	}
	if len(ka.leases) == 0 {
		return false
	}
	if ka.powerProvider == nil {
		return false
	}

	snap := ka.powerProvider.Snapshot()
	if snap.OnBattery == nil || !*snap.OnBattery {
		return false
	}
	if snap.BatteryPercent == nil || *snap.BatteryPercent > ka.policy.AutoDisableBatteryPercent {
		return false
	}

	type leaseRevokeCandidate struct {
		key   string
		lease *keepAwakeLease
	}
	candidates := make([]leaseRevokeCandidate, 0, len(ka.leases))
	for key, lease := range ka.leases {
		candidates = append(candidates, leaseRevokeCandidate{key: key, lease: lease})
	}

	// Fail closed: do not mutate lease/runtime state if durable audit cannot be written.
	for _, candidate := range candidates {
		if err := s.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
			Operation:      "auto_disable",
			ActorDeviceID:  "system",
			TargetDeviceID: candidate.lease.DeviceID,
			SessionID:      candidate.lease.SessionID,
			LeaseID:        candidate.lease.LeaseID,
			Reason:         fmt.Sprintf("battery_threshold_%d%%", *snap.BatteryPercent),
			At:             now,
		}); err != nil {
			log.Printf("keep-awake threshold auto-disable aborted: audit write failed: %v", err)
			return false
		}
	}

	// Revoke all leases once audit persistence has succeeded for each revoke event.
	for _, candidate := range candidates {
		key := candidate.key
		if timer := ka.timers[key]; timer != nil {
			timer.Stop()
			delete(ka.timers, key)
		}
		delete(ka.leases, key)
	}
	ka.thresholdDisabledThisEpoch = true
	ka.statusRevision++
	return true
}

func (s *Server) keepAwakeValidateMutationAuthorization(c *Client, sessionID string, requireAuth bool, activeSession string) (bool, error) {
	ka := s.keepAwake
	if ka.migrationFailed {
		return false, apperrors.New(apperrors.CodeKeepAwakePolicyDisabled, "keep-awake audit storage unavailable")
	}
	if !ka.policy.RemoteEnabled {
		return false, apperrors.New(apperrors.CodeKeepAwakePolicyDisabled, "keep-awake policy is disabled")
	}
	if !requireAuth || strings.TrimSpace(c.deviceID) == "" {
		return false, apperrors.New(apperrors.CodeKeepAwakeUnauthorized, "keep-awake mutation requires authenticated device")
	}
	isAdmin := false
	if ka.policy.AllowAdminRevoke {
		for _, adminID := range ka.policy.AdminDeviceIDs {
			if adminID == c.deviceID {
				isAdmin = true
				break
			}
		}
	}
	if strings.TrimSpace(sessionID) != strings.TrimSpace(activeSession) && !isAdmin {
		return false, apperrors.New(apperrors.CodeKeepAwakeUnauthorized, "unauthorized session scope")
	}
	return isAdmin, nil
}

func (s *Server) keepAwakeCheckOrBeginIdempotencyLocked(key keepAwakeMutationCacheKey, fingerprint string) (Message, bool, *inFlightMutation, error) {
	ka := s.keepAwake
	if entry, ok := ka.idempotencyEntries[key]; ok {
		if entry.Fingerprint != fingerprint {
			return Message{}, false, nil, &idempotencyMismatchError{requestID: key.RequestID}
		}
		return entry.Result, true, nil, nil
	}
	if inFlight, ok := ka.inFlightMutations[key]; ok {
		if inFlight.Fingerprint != fingerprint {
			return Message{}, false, nil, &idempotencyMismatchError{requestID: key.RequestID}
		}
		return Message{}, false, inFlight, nil
	}
	ka.inFlightMutations[key] = &inFlightMutation{Fingerprint: fingerprint, done: make(chan struct{})}
	return Message{}, false, nil, nil
}

func (s *Server) keepAwakeIdempotencyCompleteLocked(key keepAwakeMutationCacheKey, fingerprint string, result Message) {
	ka := s.keepAwake
	if _, exists := ka.idempotencyEntries[key]; !exists {
		if len(ka.idempotencyOrder) >= maxIdempotencyEntries {
			oldest := ka.idempotencyOrder[0]
			ka.idempotencyOrder = ka.idempotencyOrder[1:]
			delete(ka.idempotencyEntries, oldest)
		}
		ka.idempotencyOrder = append(ka.idempotencyOrder, key)
	}
	ka.idempotencyEntries[key] = &idempotencyCacheEntry{Fingerprint: fingerprint, Result: result}
	if inFlight, ok := ka.inFlightMutations[key]; ok {
		inFlight.Result = result
		delete(ka.inFlightMutations, key)
		close(inFlight.done)
	}
}

// maxPolicyIdempotencyEntries caps the policy mutation idempotency cache.
const maxPolicyIdempotencyEntries = 64

// policyIdempotencyCheckLocked checks the policy idempotency cache.
// Returns (response, true) for replay, (_, false) for miss.
// Returns error for fingerprint mismatch or in-flight reservation.
func (s *Server) policyIdempotencyCheckLocked(key policyIdempotencyKey, fingerprint string) (KeepAwakePolicyMutationResponse, bool, error) {
	ka := s.keepAwake
	if entry, ok := ka.policyIdempotencyEntries[key]; ok {
		if entry.Fingerprint != fingerprint {
			return KeepAwakePolicyMutationResponse{}, false, fmt.Errorf("request_id %s already used with different parameters", key.RequestID)
		}
		return entry.Response, true, nil
	}
	if _, ok := ka.policyInFlight[key]; ok {
		return KeepAwakePolicyMutationResponse{}, false, fmt.Errorf("request_id %s is already in-flight", key.RequestID)
	}
	return KeepAwakePolicyMutationResponse{}, false, nil
}

// policyIdempotencyReserveLocked marks a policy mutation as in-flight.
func (s *Server) policyIdempotencyReserveLocked(key policyIdempotencyKey) {
	s.keepAwake.policyInFlight[key] = struct{}{}
}

// policyIdempotencyCompleteLocked stores the result and clears in-flight.
func (s *Server) policyIdempotencyCompleteLocked(key policyIdempotencyKey, fingerprint string, response KeepAwakePolicyMutationResponse) {
	ka := s.keepAwake
	delete(ka.policyInFlight, key)
	if _, exists := ka.policyIdempotencyEntries[key]; !exists {
		if len(ka.policyIdempotencyOrder) >= maxPolicyIdempotencyEntries {
			oldest := ka.policyIdempotencyOrder[0]
			ka.policyIdempotencyOrder = ka.policyIdempotencyOrder[1:]
			delete(ka.policyIdempotencyEntries, oldest)
		}
		ka.policyIdempotencyOrder = append(ka.policyIdempotencyOrder, key)
	}
	ka.policyIdempotencyEntries[key] = &policyIdempotencyEntry{Fingerprint: fingerprint, Response: response}
}

// policyIdempotencyCancelLocked removes an in-flight reservation without caching.
func (s *Server) policyIdempotencyCancelLocked(key policyIdempotencyKey) {
	delete(s.keepAwake.policyInFlight, key)
}

func (s *Server) keepAwakeWriteAuditLocked(event KeepAwakeAuditEvent) error {
	if s.keepAwake.auditWriter == nil {
		return nil
	}
	if err := s.keepAwake.auditWriter.WriteKeepAwakeAudit(event); err != nil {
		return apperrors.Wrap(apperrors.CodeKeepAwakeConflict, "keep-awake audit write failed", err)
	}
	return nil
}

func (c *Client) sendKeepAwakeResultWithTimeout(msg Message) bool {
	select {
	case <-c.done:
		return false
	case c.send <- msg:
		return true
	case <-time.After(keepAwakeResultSendTimeout):
		c.closeSend()
		return false
	}
}

func (s *Server) broadcastKeepAwakeChanged(msg Message, requester *Client, includeRequester bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for client := range s.clients {
		if client == requester && !includeRequester {
			continue
		}
		select {
		case <-client.done:
		case client.send <- msg:
		default:
			log.Printf("Warning: client send buffer full, dropping keep-awake changed event")
		}
	}
}

func (s *Server) keepAwakeDegradedErrorCode(state, reason string) string {
	if state != string(keepawake.StateDegraded) {
		return ""
	}
	if reason == string(keepawake.DegradedReasonUnsupportedEnvironment) {
		return apperrors.CodeKeepAwakeUnsupportedEnvironment
	}
	if reason == string(keepawake.DegradedReasonAcquireFailed) {
		return apperrors.CodeKeepAwakeAcquireFailed
	}
	return ""
}

func (c *Client) handleKeepAwakeEnable(data []byte) {
	payload, err := parseKeepAwakeEnvelope(data)
	if err != nil {
		rawID := extractRawRequestIDFromPayload(data)
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(KeepAwakeMutationResultPayload{
			RequestID: rawID,
			Success:   false,
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     "invalid JSON",
		}))
		return
	}
	if err := rejectUnexpectedPayloadKeys(payload, "request_id", "session_id", "duration_ms", "reason"); err != nil {
		requestID := extractRawRequestIDFromPayload(data)
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}

	requestID, err := parseRequiredStringField(payload, "request_id")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked("", "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}
	sessionID, err := parseRequiredStringField(payload, "session_id")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}
	durationMs, err := parseStrictDurationMs(payload, true)
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}
	reason, err := parseOptionalStringField(payload, "reason")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}
	if _, suppliedLease := payload["lease_id"]; suppliedLease {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, "lease_id is not allowed for enable", false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}

	fingerprint := keepAwakeFingerprint("enable", sessionID, strconv.FormatInt(durationMs, 10), reason)
	key := keepAwakeMutationCacheKey{DeviceID: c.deviceID, OpType: MessageTypeSessionKeepAwakeEnable, RequestID: requestID}
	c.server.mu.RLock()
	requireAuth := c.server.requireAuth
	activeSession := c.server.sessionID
	c.server.mu.RUnlock()

	c.server.keepAwake.mu.Lock()
	cached, replay, inFlight, idErr := c.server.keepAwakeCheckOrBeginIdempotencyLocked(key, fingerprint)
	if replay {
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(cached)
		return
	}
	if idErr != nil {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeKeepAwakeConflict, idErr.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(NewKeepAwakeEnableResultMessage(result))
		return
	}
	if inFlight != nil {
		c.server.keepAwake.mu.Unlock()
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendKeepAwakeResultWithTimeout(inFlight.Result)
			return
		}
	}

	isAdmin, authErr := c.server.keepAwakeValidateMutationAuthorization(c, sessionID, requireAuth, activeSession)
	_ = isAdmin
	if authErr != nil {
		code, msg := apperrors.ToCodeAndMessage(authErr)
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", code, msg, false, c.server.keepAwake.now())
		message := NewKeepAwakeEnableResultMessage(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	// Power policy check for enable.
	if powerAllowed, powerReason := c.server.keepAwakeCheckPowerPolicyLocked(); !powerAllowed {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeKeepAwakePolicyDisabled, powerReason, false, c.server.keepAwake.now())
		message := NewKeepAwakeEnableResultMessage(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	c.server.keepAwake.nextLeaseSeq++
	leaseID := c.server.keepAwake.leaseIDGenerate(c.server.keepAwake.nextLeaseSeq)
	leaseKey := keepAwakeLeaseKey(sessionID, c.deviceID, leaseID)
	if _, exists := c.server.keepAwake.leases[leaseKey]; exists {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeKeepAwakeConflict, "lease id collision", false, c.server.keepAwake.now())
		message := NewKeepAwakeEnableResultMessage(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	now := c.server.keepAwake.now()
	auditErr := c.server.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
		Operation:     "enable",
		RequestID:     requestID,
		ActorDeviceID: c.deviceID,
		SessionID:     sessionID,
		LeaseID:       leaseID,
		Reason:        reason,
		At:            now,
	})
	if auditErr != nil {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeKeepAwakeConflict, apperrors.GetMessage(auditErr), false, now)
		message := NewKeepAwakeEnableResultMessage(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	// Anti-flap: reset threshold flag when transitioning from 0->1 leases.
	if len(c.server.keepAwake.leases) == 0 {
		c.server.keepAwake.thresholdDisabledThisEpoch = false
	}

	lease := &keepAwakeLease{
		SessionID: sessionID,
		DeviceID:  c.deviceID,
		LeaseID:   leaseID,
		Reason:    reason,
		ExpiresAt: now.Add(time.Duration(durationMs) * time.Millisecond),
	}
	c.server.keepAwake.leases[leaseKey] = lease
	c.server.keepAwakeScheduleTimerLocked(leaseKey, lease)
	c.server.keepAwake.statusRevision++
	c.server.keepAwakeSetRuntimeStateLocked(true)
	status := c.server.keepAwakeStatusPayloadLocked(now)
	errCode := c.server.keepAwakeDegradedErrorCode(status.State, status.DegradedReason)
	success := errCode == ""
	errMsg := ""
	if errCode != "" {
		errMsg = "keep-awake runtime degraded"
	}
	result := KeepAwakeMutationResultPayload{
		RequestID:        requestID,
		Success:          success,
		LeaseID:          leaseID,
		ErrorCode:        errCode,
		Error:            errMsg,
		State:            status.State,
		StatusRevision:   status.StatusRevision,
		ServerBootID:     status.ServerBootID,
		ActiveLeaseCount: status.ActiveLeaseCount,
		Leases:           status.Leases,
		Policy:           status.Policy,
		DegradedReason:   status.DegradedReason,
		NextExpiryMs:     status.NextExpiryMs,
		Power:            status.Power,
		RecoveryHint:     status.RecoveryHint,
	}
	resultMsg := NewKeepAwakeEnableResultMessage(result)
	changedMsg := NewKeepAwakeChangedMessage(status)
	c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, resultMsg)
	c.server.keepAwake.mu.Unlock()

	if c.sendKeepAwakeResultWithTimeout(resultMsg) {
		c.server.broadcastKeepAwakeChanged(changedMsg, c, true)
	} else {
		c.server.broadcastKeepAwakeChanged(changedMsg, c, false)
	}
}

func (c *Client) handleKeepAwakeDisable(data []byte) {
	c.handleKeepAwakeDisableOrExtend(data, false)
}

func (c *Client) handleKeepAwakeExtend(data []byte) {
	c.handleKeepAwakeDisableOrExtend(data, true)
}

func (c *Client) handleKeepAwakeDisableOrExtend(data []byte, isExtend bool) {
	opType := MessageTypeSessionKeepAwakeDisable
	resultType := NewKeepAwakeDisableResultMessage
	opName := "disable"
	if isExtend {
		opType = MessageTypeSessionKeepAwakeExtend
		resultType = NewKeepAwakeExtendResultMessage
		opName = "extend"
	}

	payload, err := parseKeepAwakeEnvelope(data)
	if err != nil {
		rawID := extractRawRequestIDFromPayload(data)
		c.sendKeepAwakeResultWithTimeout(resultType(KeepAwakeMutationResultPayload{
			RequestID: rawID,
			Success:   false,
			ErrorCode: apperrors.CodeServerInvalidMessage,
			Error:     "invalid JSON",
		}))
		return
	}
	allowed := []string{"request_id", "session_id", "lease_id", "reason"}
	if isExtend {
		allowed = append(allowed, "duration_ms")
	}
	if err := rejectUnexpectedPayloadKeys(payload, allowed...); err != nil {
		requestID := extractRawRequestIDFromPayload(data)
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	requestID, err := parseRequiredStringField(payload, "request_id")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked("", "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	sessionID, err := parseRequiredStringField(payload, "session_id")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	leaseID, err := parseRequiredStringField(payload, "lease_id")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	reason, err := parseOptionalStringField(payload, "reason")
	if err != nil {
		c.server.keepAwake.mu.Lock()
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	var durationMs int64
	if isExtend {
		durationMs, err = parseStrictDurationMs(payload, true)
		if err != nil {
			c.server.keepAwake.mu.Lock()
			result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeServerInvalidMessage, err.Error(), false, c.server.keepAwake.now())
			c.server.keepAwake.mu.Unlock()
			c.sendKeepAwakeResultWithTimeout(resultType(result))
			return
		}
	}

	fingerprintFields := []string{opName, sessionID, leaseID, reason}
	if isExtend {
		fingerprintFields = append(fingerprintFields, strconv.FormatInt(durationMs, 10))
	}
	fingerprint := keepAwakeFingerprint(fingerprintFields...)
	key := keepAwakeMutationCacheKey{DeviceID: c.deviceID, OpType: opType, RequestID: requestID}
	c.server.mu.RLock()
	requireAuth := c.server.requireAuth
	activeSession := c.server.sessionID
	c.server.mu.RUnlock()

	c.server.keepAwake.mu.Lock()
	cached, replay, inFlight, idErr := c.server.keepAwakeCheckOrBeginIdempotencyLocked(key, fingerprint)
	if replay {
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(cached)
		return
	}
	if idErr != nil {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", apperrors.CodeKeepAwakeConflict, idErr.Error(), false, c.server.keepAwake.now())
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(resultType(result))
		return
	}
	if inFlight != nil {
		c.server.keepAwake.mu.Unlock()
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendKeepAwakeResultWithTimeout(inFlight.Result)
			return
		}
	}

	isAdmin, authErr := c.server.keepAwakeValidateMutationAuthorization(c, sessionID, requireAuth, activeSession)
	if authErr != nil {
		code, msg := apperrors.ToCodeAndMessage(authErr)
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, "", code, msg, false, c.server.keepAwake.now())
		message := resultType(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	// Power policy check for extend only (disable is always allowed).
	if isExtend {
		if powerAllowed, powerReason := c.server.keepAwakeCheckPowerPolicyLocked(); !powerAllowed {
			result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, leaseID, apperrors.CodeKeepAwakePolicyDisabled, powerReason, false, c.server.keepAwake.now())
			message := resultType(result)
			c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
			c.server.keepAwake.mu.Unlock()
			c.sendKeepAwakeResultWithTimeout(message)
			return
		}
	}

	leaseKey := keepAwakeLeaseKey(sessionID, c.deviceID, leaseID)
	lease, owned := c.server.keepAwake.leases[leaseKey]
	if !owned {
		for keyIter, candidate := range c.server.keepAwake.leases {
			if candidate.SessionID == sessionID && candidate.LeaseID == leaseID {
				lease = candidate
				leaseKey = keyIter
				break
			}
		}
	}
	if lease == nil {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, leaseID, apperrors.CodeKeepAwakeExpired, "lease not found", false, c.server.keepAwake.now())
		message := resultType(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}
	if lease.DeviceID != c.deviceID && !isAdmin {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, leaseID, apperrors.CodeKeepAwakeUnauthorized, "only lease owner can mutate", false, c.server.keepAwake.now())
		message := resultType(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	now := c.server.keepAwake.now()
	auditErr := c.server.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
		Operation:      opName,
		RequestID:      requestID,
		ActorDeviceID:  c.deviceID,
		TargetDeviceID: lease.DeviceID,
		SessionID:      lease.SessionID,
		LeaseID:        lease.LeaseID,
		Reason:         reason,
		At:             now,
	})
	if auditErr != nil {
		result := c.server.keepAwakeMutationResultFromStatusLocked(requestID, leaseID, apperrors.CodeKeepAwakeConflict, apperrors.GetMessage(auditErr), false, now)
		message := resultType(result)
		c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, message)
		c.server.keepAwake.mu.Unlock()
		c.sendKeepAwakeResultWithTimeout(message)
		return
	}

	if isExtend {
		lease.ExpiresAt = now.Add(time.Duration(durationMs) * time.Millisecond)
		lease.Disconnected = false
		lease.GraceDeadline = time.Time{}
		c.server.keepAwakeScheduleTimerLocked(leaseKey, lease)
	} else {
		if timer := c.server.keepAwake.timers[leaseKey]; timer != nil {
			timer.Stop()
			delete(c.server.keepAwake.timers, leaseKey)
		}
		delete(c.server.keepAwake.leases, leaseKey)
	}

	c.server.keepAwake.statusRevision++
	c.server.keepAwakeSetRuntimeStateLocked(len(c.server.keepAwake.leases) > 0)
	status := c.server.keepAwakeStatusPayloadLocked(now)
	errCode := c.server.keepAwakeDegradedErrorCode(status.State, status.DegradedReason)
	success := errCode == ""
	errMsg := ""
	if errCode != "" {
		errMsg = "keep-awake runtime degraded"
	}
	result := KeepAwakeMutationResultPayload{
		RequestID:        requestID,
		Success:          success,
		LeaseID:          leaseID,
		ErrorCode:        errCode,
		Error:            errMsg,
		State:            status.State,
		StatusRevision:   status.StatusRevision,
		ServerBootID:     status.ServerBootID,
		ActiveLeaseCount: status.ActiveLeaseCount,
		Leases:           status.Leases,
		Policy:           status.Policy,
		DegradedReason:   status.DegradedReason,
		NextExpiryMs:     status.NextExpiryMs,
		Power:            status.Power,
		RecoveryHint:     status.RecoveryHint,
	}
	resultMsg := resultType(result)
	changedMsg := NewKeepAwakeChangedMessage(status)
	c.server.keepAwakeIdempotencyCompleteLocked(key, fingerprint, resultMsg)
	c.server.keepAwake.mu.Unlock()

	if c.sendKeepAwakeResultWithTimeout(resultMsg) {
		c.server.broadcastKeepAwakeChanged(changedMsg, c, true)
	} else {
		c.server.broadcastKeepAwakeChanged(changedMsg, c, false)
	}
}

func (c *Client) handleKeepAwakeStatus(data []byte) {
	payload, err := parseKeepAwakeEnvelope(data)
	if err != nil {
		status := KeepAwakeStatusPayload{}
		_ = c.sendKeepAwakeResultWithTimeout(NewKeepAwakeStatusResultMessage(status))
		return
	}
	if err := rejectUnexpectedPayloadKeys(payload, "request_id", "session_id"); err != nil {
		c.sendError(apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	sessionID, err := parseOptionalStringField(payload, "session_id")
	if err != nil {
		c.sendError(apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	if sessionID != "" {
		c.server.mu.RLock()
		activeSession := c.server.sessionID
		c.server.mu.RUnlock()
		if sessionID != activeSession {
			c.sendError(apperrors.CodeKeepAwakeUnauthorized, "unauthorized session scope")
			return
		}
	}

	c.server.keepAwake.mu.Lock()
	now := c.server.keepAwake.now()
	if c.server.keepAwakePruneExpiredLocked(now) {
		c.server.keepAwake.statusRevision++
		c.server.keepAwakeSetRuntimeStateLocked(len(c.server.keepAwake.leases) > 0)
	}
	status := c.server.keepAwakeStatusPayloadLocked(now)
	c.server.keepAwake.mu.Unlock()

	c.sendKeepAwakeResultWithTimeout(NewKeepAwakeStatusResultMessage(status))
}

func (s *Server) onKeepAwakeClientConnected(client *Client) {
	if client == nil || strings.TrimSpace(client.deviceID) == "" {
		return
	}
	s.keepAwake.mu.Lock()
	now := s.keepAwake.now()
	changed := false
	for key, lease := range s.keepAwake.leases {
		if lease.DeviceID != client.deviceID {
			continue
		}
		if lease.Disconnected {
			lease.Disconnected = false
			lease.GraceDeadline = time.Time{}
			s.keepAwakeScheduleTimerLocked(key, lease)
			changed = true
		}
	}
	if changed {
		s.keepAwake.statusRevision++
		s.keepAwakeSetRuntimeStateLocked(len(s.keepAwake.leases) > 0)
		status := s.keepAwakeStatusPayloadLocked(now)
		s.keepAwake.mu.Unlock()
		s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
		return
	}
	s.keepAwake.mu.Unlock()
}

func (s *Server) onKeepAwakeClientDisconnected(deviceID string) {
	if strings.TrimSpace(deviceID) == "" {
		return
	}
	s.mu.RLock()
	for client := range s.clients {
		if client.deviceID == deviceID {
			s.mu.RUnlock()
			return
		}
	}
	s.mu.RUnlock()

	s.keepAwake.mu.Lock()
	now := s.keepAwake.now()
	changed := false
	for key, lease := range s.keepAwake.leases {
		if lease.DeviceID != deviceID || lease.Disconnected {
			continue
		}
		lease.Disconnected = true
		lease.DisconnectedAt = now
		lease.GraceDeadline = now.Add(s.keepAwake.grace)
		s.keepAwakeScheduleTimerLocked(key, lease)
		changed = true
	}
	if changed {
		s.keepAwake.statusRevision++
		s.keepAwakeSetRuntimeStateLocked(len(s.keepAwake.leases) > 0)
		status := s.keepAwakeStatusPayloadLocked(now)
		s.keepAwake.mu.Unlock()
		s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
		return
	}
	s.keepAwake.mu.Unlock()
}

func (s *Server) revokeKeepAwakeLeasesForDevice(deviceID string) {
	if strings.TrimSpace(deviceID) == "" {
		return
	}
	s.keepAwake.mu.Lock()
	now := s.keepAwake.now()
	removed := false
	for key, lease := range s.keepAwake.leases {
		if lease.DeviceID != deviceID {
			continue
		}
		if err := s.keepAwakeWriteAuditLocked(KeepAwakeAuditEvent{
			Operation:      "revoke",
			ActorDeviceID:  "host",
			TargetDeviceID: lease.DeviceID,
			SessionID:      lease.SessionID,
			LeaseID:        lease.LeaseID,
			Reason:         "device revoked",
			At:             now,
		}); err != nil {
			log.Printf("Warning: keep-awake revoke audit failed for lease %s: %v", lease.LeaseID, err)
			continue
		}
		if timer := s.keepAwake.timers[key]; timer != nil {
			timer.Stop()
			delete(s.keepAwake.timers, key)
		}
		delete(s.keepAwake.leases, key)
		removed = true
	}
	if removed {
		s.keepAwake.statusRevision++
		s.keepAwakeSetRuntimeStateLocked(len(s.keepAwake.leases) > 0)
		status := s.keepAwakeStatusPayloadLocked(now)
		s.keepAwake.mu.Unlock()
		s.broadcastKeepAwakeChanged(NewKeepAwakeChangedMessage(status), nil, true)
		return
	}
	s.keepAwake.mu.Unlock()
}

func (s *Server) stopKeepAwake() {
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	for key, timer := range s.keepAwake.timers {
		timer.Stop()
		delete(s.keepAwake.timers, key)
	}
}
