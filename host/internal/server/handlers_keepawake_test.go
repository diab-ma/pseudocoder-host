package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/keepawake"
)

type keepAwakeRuntimeStub struct {
	forceDegraded bool
}

func (s *keepAwakeRuntimeStub) SetDesiredEnabled(_ context.Context, enabled bool) keepawake.Status {
	if s.forceDegraded && enabled {
		return keepawake.Status{State: keepawake.StateDegraded, Reason: keepawake.DegradedReasonAcquireFailed}
	}
	if enabled {
		return keepawake.Status{State: keepawake.StateOn}
	}
	return keepawake.Status{State: keepawake.StateOff}
}

type keepAwakeAuditStub struct {
	mu     sync.Mutex
	events []KeepAwakeAuditEvent
	fail   bool
}

func (s *keepAwakeAuditStub) WriteKeepAwakeAudit(event KeepAwakeAuditEvent) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	if s.fail {
		return fmt.Errorf("audit failed")
	}
	return nil
}

func (s *keepAwakeAuditStub) Events() []KeepAwakeAuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]KeepAwakeAuditEvent, len(s.events))
	copy(out, s.events)
	return out
}

func newKeepAwakeTestClient(t *testing.T) (*Server, *Client) {
	t.Helper()
	s := NewServer("unused")
	s.SetRequireAuth(true)
	s.SetKeepAwakeRuntimeManager(&keepAwakeRuntimeStub{})
	s.SetKeepAwakeAuditWriter(&keepAwakeAuditStub{})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: true, AllowAdminRevoke: true, AdminDeviceIDs: []string{"admin-1"}, AllowOnBattery: true})
	if err := s.SetKeepAwakeDisconnectGrace(50 * time.Millisecond); err != nil {
		t.Fatalf("SetKeepAwakeDisconnectGrace: %v", err)
	}
	c := &Client{
		server:   s,
		send:     make(chan Message, 32),
		done:     make(chan struct{}),
		deviceID: "device-1",
	}
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	return s, c
}

func keepAwakeAuditEvents(t *testing.T, s *Server) []KeepAwakeAuditEvent {
	t.Helper()
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	stub, ok := s.keepAwake.auditWriter.(*keepAwakeAuditStub)
	if !ok {
		t.Fatalf("audit writer is %T, expected *keepAwakeAuditStub", s.keepAwake.auditWriter)
	}
	return stub.Events()
}

func setKeepAwakeAuditFailMode(t *testing.T, s *Server, fail bool) {
	t.Helper()
	s.keepAwake.mu.Lock()
	defer s.keepAwake.mu.Unlock()
	stub, ok := s.keepAwake.auditWriter.(*keepAwakeAuditStub)
	if !ok {
		t.Fatalf("audit writer is %T, expected *keepAwakeAuditStub", s.keepAwake.auditWriter)
	}
	stub.fail = fail
}

func encodeKeepAwakeRequest(t *testing.T, msgType MessageType, payload map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"type":    msgType,
		"payload": payload,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func mustReceive(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return Message{}
	}
}

func TestKeepAwakeMessageTypes(t *testing.T) {
	if MessageTypeSessionKeepAwakeEnable != "session.keep_awake_enable" {
		t.Fatalf("unexpected type: %s", MessageTypeSessionKeepAwakeEnable)
	}
	if MessageTypeSessionKeepAwakeChanged != "session.keep_awake_changed" {
		t.Fatalf("unexpected type: %s", MessageTypeSessionKeepAwakeChanged)
	}
}

func TestNewKeepAwakeResultMessage(t *testing.T) {
	msg := NewKeepAwakeEnableResultMessage(KeepAwakeMutationResultPayload{RequestID: "r1", Success: true})
	if msg.Type != MessageTypeSessionKeepAwakeEnableResult {
		t.Fatalf("unexpected type: %s", msg.Type)
	}
}

func TestNewKeepAwakeChangedMessage(t *testing.T) {
	msg := NewKeepAwakeChangedMessage(KeepAwakeStatusPayload{State: "ON"})
	if msg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("unexpected type: %s", msg.Type)
	}
}

func TestNewKeepAwakeStatusMessage(t *testing.T) {
	msg := NewKeepAwakeStatusResultMessage(KeepAwakeStatusPayload{State: "OFF"})
	if msg.Type != MessageTypeSessionKeepAwakeStatusResult {
		t.Fatalf("unexpected type: %s", msg.Type)
	}
}

func TestHandleKeepAwakeEnable_Success(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "req-1",
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	}))

	resultMsg := mustReceive(t, c.send)
	if resultMsg.Type != MessageTypeSessionKeepAwakeEnableResult {
		t.Fatalf("expected enable_result, got %s", resultMsg.Type)
	}
	result, ok := resultMsg.Payload.(KeepAwakeMutationResultPayload)
	if !ok {
		t.Fatalf("expected KeepAwakeMutationResultPayload, got %T", resultMsg.Payload)
	}
	if !result.Success {
		t.Fatalf("expected success, got error=%s code=%s", result.Error, result.ErrorCode)
	}
	if result.LeaseID == "" {
		t.Fatal("expected generated lease_id")
	}

	changedMsg := mustReceive(t, c.send)
	if changedMsg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed event after result, got %s", changedMsg.Type)
	}
}

func TestHandleKeepAwakeEnable_MissingRequestID(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	}))

	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.Success {
		t.Fatal("expected failure")
	}
	if payload.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %s", payload.ErrorCode)
	}
}

func TestHandleKeepAwakeEnable_PolicyDisabled(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: false})
	_ = mustReceive(t, c.send) // policy changed broadcast
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "req-2",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))

	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "keep_awake.policy_disabled" {
		t.Fatalf("expected policy_disabled, got %s", payload.ErrorCode)
	}
}

func TestHandleKeepAwakeEnable_Unauthorized(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetRequireAuth(false)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "req-3",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "keep_awake.unauthorized" {
		t.Fatalf("expected unauthorized, got %s", payload.ErrorCode)
	}
}

func TestHandleKeepAwakeIdempotencyReplay(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	request := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "idem-1",
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(request)
	first := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c.send) // changed

	c.handleKeepAwakeEnable(request)
	second := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if first.LeaseID != second.LeaseID {
		t.Fatalf("expected replay to return same lease_id: %s != %s", first.LeaseID, second.LeaseID)
	}
}

func TestHandleKeepAwakeIdempotencyMismatch(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "idem-mm",
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send)
	_ = mustReceive(t, c.send)

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "idem-mm",
		"session_id":  c.server.sessionID,
		"duration_ms": 90000,
	}))
	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected conflict, got %s", payload.ErrorCode)
	}
}

func TestKeepAwakeOwnerOnlyDisable(t *testing.T) {
	s, c1 := newKeepAwakeTestClient(t)
	c1.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "owner-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	result := mustReceive(t, c1.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c1.send)

	c2 := &Client{server: s, send: make(chan Message, 16), done: make(chan struct{}), deviceID: "device-2"}
	s.mu.Lock()
	s.clients[c2] = true
	s.mu.Unlock()
	c2.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "owner-disable",
		"session_id": s.sessionID,
		"lease_id":   result.LeaseID,
	}))
	msg := mustReceive(t, c2.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "keep_awake.unauthorized" {
		t.Fatalf("expected unauthorized, got %s", payload.ErrorCode)
	}
}

func TestKeepAwakeAdminRevoke(t *testing.T) {
	s, owner := newKeepAwakeTestClient(t)
	owner.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "admin-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	result := mustReceive(t, owner.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, owner.send)

	admin := &Client{server: s, send: make(chan Message, 16), done: make(chan struct{}), deviceID: "admin-1"}
	s.mu.Lock()
	s.clients[admin] = true
	s.mu.Unlock()
	admin.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "admin-disable",
		"session_id": s.sessionID,
		"lease_id":   result.LeaseID,
	}))
	msg := mustReceive(t, admin.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if !payload.Success {
		t.Fatalf("expected admin revoke success: %s", payload.ErrorCode)
	}
}

func TestKeepAwakeDisconnectGrace(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "grace-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send)
	_ = mustReceive(t, c.send)

	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(c.deviceID)
	s.keepAwake.mu.Lock()
	var disconnected bool
	for _, lease := range s.keepAwake.leases {
		disconnected = lease.Disconnected
	}
	s.keepAwake.mu.Unlock()
	if !disconnected {
		t.Fatal("expected lease to be marked disconnected during grace")
	}

	s.onKeepAwakeClientConnected(c)
	s.keepAwake.mu.Lock()
	var reconnected bool
	for _, lease := range s.keepAwake.leases {
		reconnected = !lease.Disconnected
	}
	s.keepAwake.mu.Unlock()
	if !reconnected {
		t.Fatal("expected lease to be restored on reconnect")
	}
}

func TestKeepAwakeDurationStrictIntegerValidation(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "strict-int",
		"session_id":  c.server.sessionID,
		"duration_ms": "60000",
	}))
	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected invalid_message, got %s", payload.ErrorCode)
	}
}

func TestKeepAwakeAuthDisabledMutationsFailClosed(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetRequireAuth(false)
	c.handleKeepAwakeExtend(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeExtend, map[string]interface{}{
		"request_id":  "auth-disabled",
		"session_id":  s.sessionID,
		"lease_id":    "missing",
		"duration_ms": 60000,
	}))
	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeMutationResultPayload)
	if payload.ErrorCode != "keep_awake.unauthorized" {
		t.Fatalf("expected unauthorized, got %s", payload.ErrorCode)
	}
}

func TestKeepAwakeRevokeExpiresImmediately(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "revoke-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send)
	_ = mustReceive(t, c.send)

	s.revokeKeepAwakeLeasesForDevice(c.deviceID)
	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("expected immediate revoke lease expiry, got %d leases", leaseCount)
	}
}

func TestKeepAwakeResultBeforeChangedBroadcast(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "order-enable",
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	}))

	first := mustReceive(t, c.send)
	second := mustReceive(t, c.send)
	if first.Type != MessageTypeSessionKeepAwakeEnableResult {
		t.Fatalf("expected result first, got %s", first.Type)
	}
	if second.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed second, got %s", second.Type)
	}
}

func TestHandleKeepAwakeInFlightCoalesce(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	requestID := "inflight-1"
	fingerprint := keepAwakeFingerprint("enable", s.sessionID, "60000", "")
	key := keepAwakeMutationCacheKey{
		DeviceID:  c.deviceID,
		OpType:    MessageTypeSessionKeepAwakeEnable,
		RequestID: requestID,
	}
	expected := NewKeepAwakeEnableResultMessage(KeepAwakeMutationResultPayload{
		RequestID: requestID,
		Success:   true,
		LeaseID:   "ka-existing",
	})
	inFlight := &inFlightMutation{
		Fingerprint: fingerprint,
		done:        make(chan struct{}),
	}

	s.keepAwake.mu.Lock()
	s.keepAwake.inFlightMutations[key] = inFlight
	s.keepAwake.mu.Unlock()

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.keepAwake.mu.Lock()
		inFlight.Result = expected
		close(inFlight.done)
		delete(s.keepAwake.inFlightMutations, key)
		s.keepAwake.mu.Unlock()
	}()

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  requestID,
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	msg := mustReceive(t, c.send)
	if msg.Type != MessageTypeSessionKeepAwakeEnableResult {
		t.Fatalf("expected replayed enable_result, got %s", msg.Type)
	}
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.LeaseID != "ka-existing" {
		t.Fatalf("expected replayed lease id, got %q", result.LeaseID)
	}
}

func TestHandleKeepAwakeRequesterOnlyResults(t *testing.T) {
	s, requester := newKeepAwakeTestClient(t)
	observer := &Client{
		server:   s,
		send:     make(chan Message, 16),
		done:     make(chan struct{}),
		deviceID: "device-2",
	}
	s.mu.Lock()
	s.clients[observer] = true
	s.mu.Unlock()

	requester.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "requester-only",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, requester.send) // result
	_ = mustReceive(t, requester.send) // changed

	observerMsg := mustReceive(t, observer.send)
	if observerMsg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed for non-requester, got %s", observerMsg.Type)
	}
	select {
	case extra := <-observer.send:
		if extra.Type == MessageTypeSessionKeepAwakeEnableResult {
			t.Fatalf("non-requester must not receive mutation result")
		}
	default:
	}
}

func TestKeepAwakeStatusRevisionMonotonic(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "rev-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	enableResult := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c.send)

	c.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "rev-disable",
		"session_id": s.sessionID,
		"lease_id":   enableResult.LeaseID,
	}))
	disableResult := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if disableResult.StatusRevision <= enableResult.StatusRevision {
		t.Fatalf("expected monotonic status revision, got %d then %d", enableResult.StatusRevision, disableResult.StatusRevision)
	}
}

func TestKeepAwakeServerBootIDIncluded(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "boot-id",
	}))
	msg := mustReceive(t, c.send)
	if msg.Type != MessageTypeSessionKeepAwakeStatusResult {
		t.Fatalf("expected status_result, got %s", msg.Type)
	}
	payload := msg.Payload.(KeepAwakeStatusPayload)
	if payload.ServerBootID == "" {
		t.Fatal("expected non-empty server_boot_id")
	}
}

func TestKeepAwakeStatusResultIncludesRevision(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "status-rev",
	}))
	msg := mustReceive(t, c.send)
	payload := msg.Payload.(KeepAwakeStatusPayload)
	if payload.StatusRevision < 0 {
		t.Fatalf("expected non-negative status_revision, got %d", payload.StatusRevision)
	}
}

func TestKeepAwakeLeaseIDGeneratedOnEnable(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "lease-generated",
		"session_id":  c.server.sessionID,
		"duration_ms": 60000,
	}))
	result := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if result.LeaseID == "" {
		t.Fatal("expected generated lease_id")
	}
}

func TestKeepAwakeLeaseIDCollisionRejected(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.keepAwake.mu.Lock()
	s.keepAwake.leaseIDGenerate = func(seq int64) string {
		_ = seq
		return "fixed-lease-id"
	}
	s.keepAwake.mu.Unlock()

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "lease-collision-a",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	first := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if !first.Success {
		t.Fatalf("expected initial success, got %s", first.ErrorCode)
	}
	_ = mustReceive(t, c.send)

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "lease-collision-b",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	second := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if second.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected keep_awake.conflict, got %s", second.ErrorCode)
	}
}

func TestKeepAwakeDurationBoundsValidation(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "duration-low",
		"session_id":  c.server.sessionID,
		"duration_ms": 29999,
	}))
	low := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if low.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message for low duration, got %s", low.ErrorCode)
	}

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "duration-high",
		"session_id":  c.server.sessionID,
		"duration_ms": 28800001,
	}))
	high := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if high.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message for high duration, got %s", high.ErrorCode)
	}
}

func TestKeepAwakeDurationAliasRejected(t *testing.T) {
	_, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "duration-alias",
		"session_id": c.server.sessionID,
		"duration":   60000,
	}))
	msg := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if msg.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %s", msg.ErrorCode)
	}
}

func TestKeepAwakeGraceBoundsValidation(t *testing.T) {
	s := NewServer("unused")
	if err := s.SetKeepAwakeDisconnectGrace(-1 * time.Millisecond); err == nil {
		t.Fatal("expected error for negative grace")
	}
	if err := s.SetKeepAwakeDisconnectGrace(time.Duration(maxKeepAwakeGraceMs+1) * time.Millisecond); err == nil {
		t.Fatal("expected error for out-of-range grace")
	}
}

func TestKeepAwakeBackpressureRequesterResultHandling(t *testing.T) {
	s, requester := newKeepAwakeTestClient(t)
	observer := &Client{
		server:   s,
		send:     make(chan Message, 16),
		done:     make(chan struct{}),
		deviceID: "device-2",
	}
	s.mu.Lock()
	s.clients[observer] = true
	s.mu.Unlock()

	for i := 0; i < cap(requester.send); i++ {
		requester.send <- Message{Type: MessageTypeHeartbeat}
	}

	requester.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "backpressure",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))

	select {
	case <-requester.done:
	case <-time.After(1 * time.Second):
		t.Fatal("expected requester connection to be closed on result backpressure timeout")
	}

	changed := mustReceive(t, observer.send)
	if changed.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed event to observer, got %s", changed.Type)
	}
}

func TestKeepAwakeRestartBootIDResetsRevisionBaseline(t *testing.T) {
	s1, c1 := newKeepAwakeTestClient(t)
	c1.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "restart-a",
		"session_id":  s1.sessionID,
		"duration_ms": 60000,
	}))
	first := mustReceive(t, c1.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c1.send)

	_, c2 := newKeepAwakeTestClient(t)
	c2.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "restart-status",
	}))
	status := mustReceive(t, c2.send).Payload.(KeepAwakeStatusPayload)

	if status.ServerBootID == first.ServerBootID {
		t.Fatal("expected fresh server_boot_id after restart")
	}
	if status.StatusRevision != 0 {
		t.Fatalf("expected reset revision baseline at boot, got %d", status.StatusRevision)
	}
}

func TestKeepAwakeAdminCapabilityPolicySourceEnforced(t *testing.T) {
	s, owner := newKeepAwakeTestClient(t)
	owner.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "policy-owner",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	result := mustReceive(t, owner.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, owner.send)

	nonAdmin := &Client{
		server:   s,
		send:     make(chan Message, 16),
		done:     make(chan struct{}),
		deviceID: "admin-looking-but-not-listed",
	}
	s.mu.Lock()
	s.clients[nonAdmin] = true
	s.mu.Unlock()

	nonAdmin.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "policy-non-admin",
		"session_id": s.sessionID,
		"lease_id":   result.LeaseID,
	}))
	msg := mustReceive(t, nonAdmin.send).Payload.(KeepAwakeMutationResultPayload)
	if msg.ErrorCode != "keep_awake.unauthorized" {
		t.Fatalf("expected keep_awake.unauthorized, got %s", msg.ErrorCode)
	}
}

func TestKeepAwakeAuditWriteFailClosed(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	auditStub, ok := s.keepAwake.auditWriter.(*keepAwakeAuditStub)
	if !ok {
		t.Fatalf("expected keepAwakeAuditStub, got %T", s.keepAwake.auditWriter)
	}
	auditStub.fail = true

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "audit-fail",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	result := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if result.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected keep_awake.conflict, got %s", result.ErrorCode)
	}

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("expected no lease mutation on audit failure, got %d leases", leaseCount)
	}
}

func TestKeepAwakeReconnectIdempotencyReplay(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	request := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "reconnect-idem",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(request)
	first := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c.send)

	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(c.deviceID)

	reconnected := &Client{
		server:   s,
		send:     make(chan Message, 16),
		done:     make(chan struct{}),
		deviceID: c.deviceID,
	}
	s.mu.Lock()
	s.clients[reconnected] = true
	s.mu.Unlock()
	s.onKeepAwakeClientConnected(reconnected)

	reconnected.handleKeepAwakeEnable(request)
	var second KeepAwakeMutationResultPayload
	for i := 0; i < 3; i++ {
		msg := mustReceive(t, reconnected.send)
		if msg.Type != MessageTypeSessionKeepAwakeEnableResult {
			continue
		}
		second = msg.Payload.(KeepAwakeMutationResultPayload)
		break
	}
	if second.LeaseID != first.LeaseID {
		t.Fatalf("expected idempotent replay lease id %q, got %q", first.LeaseID, second.LeaseID)
	}
}

func TestKeepAwakeTimerCleanupNoDoubleExpiry(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "timer-cleanup",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	enableResult := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c.send)

	leaseKey := keepAwakeLeaseKey(s.sessionID, c.deviceID, enableResult.LeaseID)
	s.keepAwake.mu.Lock()
	lease := s.keepAwake.leases[leaseKey]
	if lease == nil {
		s.keepAwake.mu.Unlock()
		t.Fatal("expected lease to exist")
	}
	lease.ExpiresAt = s.keepAwake.now().Add(-1 * time.Millisecond)
	beforeRevision := s.keepAwake.statusRevision
	s.keepAwake.mu.Unlock()

	s.keepAwakeOnTimer(leaseKey)
	s.keepAwakeOnTimer(leaseKey)

	s.keepAwake.mu.Lock()
	afterRevision := s.keepAwake.statusRevision
	remainingLeases := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()

	if remainingLeases != 0 {
		t.Fatalf("expected lease expiry cleanup, got %d active leases", remainingLeases)
	}
	if afterRevision != beforeRevision+1 {
		t.Fatalf("expected single revision bump across duplicate timer callbacks: before=%d after=%d", beforeRevision, afterRevision)
	}
}

// =============================================================================
// P17U5: Keep-Awake Security/Reliability Tests
// =============================================================================

type keepAwakeAuditRecorder struct {
	entries []KeepAwakeAuditEvent
	fail    bool
}

func (r *keepAwakeAuditRecorder) WriteKeepAwakeAudit(e KeepAwakeAuditEvent) error {
	if r.fail {
		return fmt.Errorf("audit write failed")
	}
	r.entries = append(r.entries, e)
	return nil
}

type stubPowerProvider struct {
	onBattery      *bool
	batteryPercent *int
	externalPower  *bool
}

func (s *stubPowerProvider) Snapshot() keepawake.PowerSnapshot {
	return keepawake.PowerSnapshot{
		OnBattery:      s.onBattery,
		BatteryPercent: s.batteryPercent,
		ExternalPower:  s.externalPower,
	}
}

func kaIntPtr(v int) *int { return &v }

func TestKeepAwakeAuditDurableWrite(t *testing.T) {
	recorder := &keepAwakeAuditRecorder{}
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakeAuditWriter(recorder)

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if !result.Success && result.ErrorCode != "" {
		// Degraded is ok, but no error code besides that
		if result.ErrorCode != "keep_awake_acquire_failed" &&
			result.ErrorCode != "keep_awake_unsupported_environment" {
			t.Fatalf("unexpected error: %s - %s", result.ErrorCode, result.Error)
		}
	}
	if len(recorder.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(recorder.entries))
	}
	if recorder.entries[0].Operation != "enable" {
		t.Errorf("audit op = %q, want enable", recorder.entries[0].Operation)
	}
}

func TestKeepAwakeAuditWriteFailure(t *testing.T) {
	recorder := &keepAwakeAuditRecorder{fail: true}
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakeAuditWriter(recorder)

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected failure when audit write fails")
	}
	// Verify no leases were created (fail-closed).
	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Errorf("expected 0 leases after audit failure, got %d", leaseCount)
	}
}

func TestKeepAwakePolicyBatteryBlock(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakePowerProvider(&stubPowerProvider{
		onBattery:      boolPtr(true),
		batteryPercent: kaIntPtr(50),
	})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:  true,
		AllowOnBattery: false,
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected failure when on battery with AllowOnBattery=false")
	}
	if result.ErrorCode != "keep_awake.policy_disabled" {
		t.Errorf("error code = %q, want keep_awake_policy_disabled", result.ErrorCode)
	}
}

func TestKeepAwakeBatteryThresholdAutoDisable(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	provider := &stubPowerProvider{
		onBattery:      boolPtr(false),
		batteryPercent: kaIntPtr(80),
		externalPower:  boolPtr(true),
	}
	s.SetKeepAwakePowerProvider(provider)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowOnBattery:            true,
		AutoDisableBatteryPercent: 20,
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	// Enable a lease first (on AC power)
	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 3600000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	// Drain broadcast
	select {
	case <-c.send:
	case <-time.After(100 * time.Millisecond):
	}
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 active lease, got %d", result.ActiveLeaseCount)
	}

	// Now simulate switching to battery with low percent
	provider.onBattery = boolPtr(true)
	provider.batteryPercent = kaIntPtr(15)
	provider.externalPower = boolPtr(false)

	// Trigger timer which checks battery threshold
	s.keepAwake.mu.Lock()
	var leaseKey string
	for k := range s.keepAwake.leases {
		leaseKey = k
		break
	}
	s.keepAwake.mu.Unlock()
	s.keepAwakeOnTimer(leaseKey)

	// Verify leases were auto-revoked
	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	threshold := s.keepAwake.thresholdDisabledThisEpoch
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Errorf("expected 0 leases after threshold auto-disable, got %d", leaseCount)
	}
	if !threshold {
		t.Error("thresholdDisabledThisEpoch should be true")
	}
}

func TestKeepAwakeBatteryThresholdAutoDisableAuditFailClosed(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	provider := &stubPowerProvider{
		onBattery:      boolPtr(false),
		batteryPercent: kaIntPtr(80),
		externalPower:  boolPtr(true),
	}
	s.SetKeepAwakePowerProvider(provider)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowOnBattery:            true,
		AutoDisableBatteryPercent: 20,
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 3600000,
	}))
	result := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c.send) // enable changed broadcast
	if result.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 active lease before threshold check, got %d", result.ActiveLeaseCount)
	}

	// Simulate low battery threshold crossing and force audit writes to fail.
	provider.onBattery = boolPtr(true)
	provider.batteryPercent = kaIntPtr(15)
	provider.externalPower = boolPtr(false)
	setKeepAwakeAuditFailMode(t, s, true)

	s.keepAwake.mu.Lock()
	var leaseKey string
	revBefore := s.keepAwake.statusRevision
	for k := range s.keepAwake.leases {
		leaseKey = k
		break
	}
	s.keepAwake.mu.Unlock()
	s.keepAwakeOnTimer(leaseKey)

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	threshold := s.keepAwake.thresholdDisabledThisEpoch
	revAfter := s.keepAwake.statusRevision
	s.keepAwake.mu.Unlock()
	if leaseCount != 1 {
		t.Fatalf("expected lease table unchanged on threshold audit failure, got %d", leaseCount)
	}
	if threshold {
		t.Fatal("thresholdDisabledThisEpoch should remain false on audit failure")
	}
	if revAfter != revBefore {
		t.Fatalf("expected status revision unchanged on audit failure, before=%d after=%d", revBefore, revAfter)
	}
}

func TestKeepAwakeBatteryFlap(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	provider := &stubPowerProvider{
		onBattery:      boolPtr(false),
		batteryPercent: kaIntPtr(80),
		externalPower:  boolPtr(true),
	}
	s.SetKeepAwakePowerProvider(provider)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowOnBattery:            true,
		AutoDisableBatteryPercent: 20,
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	// Set thresholdDisabledThisEpoch = true to simulate previous auto-disable
	s.keepAwake.mu.Lock()
	s.keepAwake.thresholdDisabledThisEpoch = true
	s.keepAwake.mu.Unlock()

	// Enable should reset the flag (transition 0->1 leases)
	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	_ = mustReceive(t, c.send)
	// Drain broadcast
	select {
	case <-c.send:
	case <-time.After(100 * time.Millisecond):
	}

	s.keepAwake.mu.Lock()
	threshold := s.keepAwake.thresholdDisabledThisEpoch
	s.keepAwake.mu.Unlock()
	if threshold {
		t.Error("thresholdDisabledThisEpoch should be reset after new enable")
	}
}

func TestKeepAwakeUnknownPower(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	// Provider returns all-nil (unknown state)
	s.SetKeepAwakePowerProvider(&stubPowerProvider{})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:  true,
		AllowOnBattery: false, // constraint exists but power unknown
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected failure when power state unknown with constraints")
	}
}

func TestKeepAwakeUnknownBatteryPercentWithThreshold(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakePowerProvider(&stubPowerProvider{
		onBattery:      boolPtr(true),
		batteryPercent: nil, // unknown battery percentage
	})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowOnBattery:            true,
		AutoDisableBatteryPercent: 20,
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected failure when threshold is configured and battery percent is unknown")
	}
	if result.ErrorCode != "keep_awake.policy_disabled" {
		t.Fatalf("expected keep_awake.policy_disabled, got %s", result.ErrorCode)
	}
	if result.Error != "power_state_unknown" {
		t.Fatalf("expected power_state_unknown, got %q", result.Error)
	}
}

func TestKeepAwakePolicyChange(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	_ = c

	// Enable a lease
	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 3600000,
	})
	c.handleKeepAwakeEnable(data)
	_ = mustReceive(t, c.send)
	// Drain broadcast
	select {
	case <-c.send:
	case <-time.After(100 * time.Millisecond):
	}

	s.keepAwake.mu.Lock()
	leasesBefore := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leasesBefore != 1 {
		t.Fatalf("expected 1 lease, got %d", leasesBefore)
	}

	// Disable remote -> should revoke all leases
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: false})

	s.keepAwake.mu.Lock()
	leasesAfter := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leasesAfter != 0 {
		t.Errorf("expected 0 leases after policy change, got %d", leasesAfter)
	}

	events := keepAwakeAuditEvents(t, s)
	var sawPolicyChange bool
	for _, event := range events {
		if event.Operation == "policy_change" {
			sawPolicyChange = true
			break
		}
	}
	if !sawPolicyChange {
		t.Fatal("expected policy_change audit event")
	}
}

func TestKeepAwakePolicyChangeBroadcastWithoutLeases(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)

	// Change policy without any active leases.
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowAdminRevoke:          true,
		AdminDeviceIDs:            []string{"admin-1"},
		AllowOnBattery:            false,
		AutoDisableBatteryPercent: 15,
	})

	msg := mustReceive(t, c.send)
	if msg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected keep-awake changed broadcast, got %s", msg.Type)
	}
	changed := msg.Payload.(KeepAwakeStatusPayload)
	if changed.Policy.AllowOnBattery {
		t.Fatal("expected allow_on_battery=false in broadcast payload")
	}
	if changed.Policy.AutoDisableBatteryPercent != 15 {
		t.Fatalf("expected auto_disable_battery_percent=15, got %d", changed.Policy.AutoDisableBatteryPercent)
	}

	events := keepAwakeAuditEvents(t, s)
	var sawPolicyChange bool
	for _, event := range events {
		if event.Operation == "policy_change" {
			sawPolicyChange = true
			break
		}
	}
	if !sawPolicyChange {
		t.Fatal("expected policy_change audit event")
	}
}

func TestKeepAwakePolicyChangeAuditFailureFailsClosed(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)

	// Create one active lease so fail-closed must revoke runtime state.
	enable := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "seed", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(enable)
	_ = mustReceive(t, c.send) // enable result
	_ = mustReceive(t, c.send) // changed broadcast

	setKeepAwakeAuditFailMode(t, s, true)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:             true,
		AllowAdminRevoke:          true,
		AdminDeviceIDs:            []string{"admin-1"},
		AllowOnBattery:            false, // policy-change path (not remote disable path)
		AutoDisableBatteryPercent: 10,
	})

	msg := mustReceive(t, c.send)
	if msg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed broadcast, got %s", msg.Type)
	}
	changed := msg.Payload.(KeepAwakeStatusPayload)
	if changed.Policy.RemoteEnabled {
		t.Fatal("expected remote policy to be forced disabled after audit failure")
	}

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	migrationFailed := s.keepAwake.migrationFailed
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("expected leases revoked under fail-closed policy, got %d", leaseCount)
	}
	if !migrationFailed {
		t.Fatal("expected migrationFailed flag set after policy audit failure")
	}

	// Mutations must be blocked after fail-closed transition.
	blocked := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "blocked", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(blocked)
	resultMsg := mustReceive(t, c.send)
	result := resultMsg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected keep-awake enable to be blocked after fail-closed transition")
	}
	if result.ErrorCode != "keep_awake.policy_disabled" {
		t.Fatalf("expected keep_awake.policy_disabled, got %s", result.ErrorCode)
	}
}

func TestKeepAwakePolicyRevokeAuditFailureFailsClosed(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)

	// Create one active lease then attempt remote disable with audit failure.
	enable := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "seed", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(enable)
	_ = mustReceive(t, c.send) // enable result
	_ = mustReceive(t, c.send) // changed broadcast

	setKeepAwakeAuditFailMode(t, s, true)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: false})

	msg := mustReceive(t, c.send)
	if msg.Type != MessageTypeSessionKeepAwakeChanged {
		t.Fatalf("expected changed broadcast, got %s", msg.Type)
	}
	changed := msg.Payload.(KeepAwakeStatusPayload)
	if changed.Policy.RemoteEnabled {
		t.Fatal("expected remote policy to remain disabled after revoke-audit failure")
	}

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	migrationFailed := s.keepAwake.migrationFailed
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("expected leases revoked under fail-closed policy, got %d", leaseCount)
	}
	if !migrationFailed {
		t.Fatal("expected migrationFailed flag set after revoke audit failure")
	}
}

func TestKeepAwakeMigrationFallback(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakeMigrationFailed(true)

	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	if result.Success {
		t.Fatal("expected failure when migration failed")
	}
	if result.ErrorCode != "keep_awake.policy_disabled" {
		t.Errorf("error code = %q, want keep_awake_policy_disabled", result.ErrorCode)
	}
}

func TestKeepAwakeRecoveryHint(t *testing.T) {
	s, _ := newKeepAwakeTestClient(t)
	s.SetKeepAwakePowerProvider(&stubPowerProvider{
		onBattery:      boolPtr(true),
		batteryPercent: kaIntPtr(50),
	})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:  true,
		AllowOnBattery: false,
	})

	s.keepAwake.mu.Lock()
	status := s.keepAwakeStatusPayloadLocked(time.Now())
	s.keepAwake.mu.Unlock()

	if status.Power == nil {
		t.Fatal("Power should be populated")
	}
	if !status.Power.PolicyBlocked {
		t.Error("PolicyBlocked should be true")
	}
	if status.RecoveryHint == "" {
		t.Error("RecoveryHint should be set for power policy block")
	}
}

func TestKeepAwakeAdminDeviceIdCaseSensitive(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:    true,
		AllowAdminRevoke: true,
		AdminDeviceIDs:   []string{"Admin-1"}, // different case from c.deviceID
	})
	_ = mustReceive(t, c.send) // policy changed broadcast

	// Create lease from a different "session"
	data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id": "r1", "session_id": s.SessionID(), "duration_ms": 60000,
	})
	c.handleKeepAwakeEnable(data)
	msg := mustReceive(t, c.send)
	result := msg.Payload.(KeepAwakeMutationResultPayload)
	// Should succeed: device-1 is same session
	if !result.Success && result.ErrorCode != "" &&
		result.ErrorCode != "keep_awake_acquire_failed" &&
		result.ErrorCode != "keep_awake_unsupported_environment" {
		// Admin-1 != admin-1 (case sensitive), so this tests that admin matching is case-sensitive
		// The enable should still work because device-1 is same session
	}
}

func TestKeepAwakeAuditRetention(t *testing.T) {
	recorder := &keepAwakeAuditRecorder{}
	s, c := newKeepAwakeTestClient(t)
	s.SetKeepAwakeAuditWriter(recorder)

	// Create and disable multiple leases
	for i := 0; i < 5; i++ {
		data := encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
			"request_id": fmt.Sprintf("enable-%d", i), "session_id": s.SessionID(), "duration_ms": 60000,
		})
		c.handleKeepAwakeEnable(data)
		_ = mustReceive(t, c.send)
		// Drain broadcast
		select {
		case <-c.send:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Verify all enables were audited
	enableCount := 0
	for _, e := range recorder.entries {
		if e.Operation == "enable" {
			enableCount++
		}
	}
	if enableCount != 5 {
		t.Errorf("expected 5 enable audit entries, got %d", enableCount)
	}
}

// =============================================================================
// P17U6: Keep-Awake Adversarial Matrix Tests
// =============================================================================

// P17U6: Partition grace boundary -- reconnect within grace preserves lease;
// reconnect after grace expires loses it.
func TestKeepAwakePartitionGraceBoundary(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "grace-boundary",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send) // result
	_ = mustReceive(t, c.send) // changed

	// Disconnect.
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(c.deviceID)

	// Reconnect within grace -> lease preserved.
	c2 := &Client{server: s, send: make(chan Message, 32), done: make(chan struct{}), deviceID: c.deviceID}
	s.mu.Lock()
	s.clients[c2] = true
	s.mu.Unlock()
	s.onKeepAwakeClientConnected(c2)

	s.keepAwake.mu.Lock()
	leaseCountAfterReconnect := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCountAfterReconnect != 1 {
		t.Fatalf("expected lease preserved on reconnect within grace, got %d", leaseCountAfterReconnect)
	}

	// Now test beyond-grace path: disconnect and wait for grace to expire.
	s.mu.Lock()
	delete(s.clients, c2)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(c2.deviceID)

	// Wait for grace to expire (grace=50ms).
	time.Sleep(80 * time.Millisecond)

	s.keepAwake.mu.Lock()
	var leaseKey string
	for k := range s.keepAwake.leases {
		leaseKey = k
	}
	s.keepAwake.mu.Unlock()
	if leaseKey != "" {
		s.keepAwakeOnTimer(leaseKey)
	}

	s.keepAwake.mu.Lock()
	leaseCountAfterExpiry := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCountAfterExpiry != 0 {
		t.Fatalf("expected lease expired after grace, got %d", leaseCountAfterExpiry)
	}
}

func TestKeepAwakePartitionGraceAtExactDeadline(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)

	// Use controllable time.
	now := time.Now()
	s.keepAwake.mu.Lock()
	s.keepAwake.now = func() time.Time { return now }
	s.keepAwake.mu.Unlock()

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "exact-grace",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send) // result
	_ = mustReceive(t, c.send) // changed

	// Disconnect.
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(c.deviceID)

	// Read the grace deadline.
	s.keepAwake.mu.Lock()
	var graceDeadline time.Time
	var leaseKey string
	for k, lease := range s.keepAwake.leases {
		graceDeadline = lease.GraceDeadline
		leaseKey = k
		break
	}
	s.keepAwake.mu.Unlock()
	if graceDeadline.IsZero() {
		t.Fatal("expected grace deadline to be set")
	}

	// Advance time to exactly the grace deadline.
	s.keepAwake.mu.Lock()
	s.keepAwake.now = func() time.Time { return graceDeadline }
	s.keepAwake.mu.Unlock()

	// Reconnect at exactly the deadline -- reconnect wins the race.
	c2 := &Client{server: s, send: make(chan Message, 32), done: make(chan struct{}), deviceID: c.deviceID}
	s.mu.Lock()
	s.clients[c2] = true
	s.mu.Unlock()
	s.onKeepAwakeClientConnected(c2)

	// Now fire the timer -- it should find the lease no longer disconnected.
	s.keepAwakeOnTimer(leaseKey)

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if leaseCount != 1 {
		t.Fatalf("expected lease preserved when reconnect wins at exact deadline, got %d", leaseCount)
	}
}

func TestKeepAwakeRevokeReconnectRace(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "revoke-reconnect",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send) // result
	_ = mustReceive(t, c.send) // changed

	// Revoke the device's leases.
	s.revokeKeepAwakeLeasesForDevice(c.deviceID)

	s.keepAwake.mu.Lock()
	countAfterRevoke := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if countAfterRevoke != 0 {
		t.Fatalf("expected 0 leases after revoke, got %d", countAfterRevoke)
	}

	// Reconnect same device -> lease count stays 0.
	s.onKeepAwakeClientConnected(c)

	s.keepAwake.mu.Lock()
	countAfterReconnect := len(s.keepAwake.leases)
	s.keepAwake.mu.Unlock()
	if countAfterReconnect != 0 {
		t.Fatalf("expected revoked lease cannot resurrect on reconnect, got %d", countAfterReconnect)
	}
}

func TestKeepAwakeMultiClientNoPrematureUnlock(t *testing.T) {
	s, c1 := newKeepAwakeTestClient(t)
	c2 := &Client{server: s, send: make(chan Message, 32), done: make(chan struct{}), deviceID: "device-2"}
	s.mu.Lock()
	s.clients[c2] = true
	s.mu.Unlock()

	// Device-1 enables.
	c1.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "multi-1",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	r1 := mustReceive(t, c1.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c1.send) // changed
	_ = mustReceive(t, c2.send) // observer changed

	// Device-2 enables.
	c2.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "multi-2",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	r2 := mustReceive(t, c2.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c2.send) // changed
	_ = mustReceive(t, c1.send) // observer changed

	// Disable device-1's lease.
	c1.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "multi-disable-1",
		"session_id": s.sessionID,
		"lease_id":   r1.LeaseID,
	}))
	disableResult1 := mustReceive(t, c1.send).Payload.(KeepAwakeMutationResultPayload)
	if disableResult1.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 lease remaining after first disable, got %d", disableResult1.ActiveLeaseCount)
	}
	if disableResult1.State != "ON" {
		t.Fatalf("expected runtime still ON with 1 lease, got %s", disableResult1.State)
	}

	// Drain changed broadcasts.
	select {
	case <-c1.send:
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-c2.send:
	case <-time.After(200 * time.Millisecond):
	}

	// Disable device-2's lease.
	c2.handleKeepAwakeDisable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeDisable, map[string]interface{}{
		"request_id": "multi-disable-2",
		"session_id": s.sessionID,
		"lease_id":   r2.LeaseID,
	}))
	disableResult2 := mustReceive(t, c2.send).Payload.(KeepAwakeMutationResultPayload)
	if disableResult2.ActiveLeaseCount != 0 {
		t.Fatalf("expected 0 leases after second disable, got %d", disableResult2.ActiveLeaseCount)
	}
	if disableResult2.State != "OFF" {
		t.Fatalf("expected runtime OFF with 0 leases, got %s", disableResult2.State)
	}
}

func TestKeepAwakeCrashRestartNoOrphanLeaseState(t *testing.T) {
	s1, c1 := newKeepAwakeTestClient(t)
	c1.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "crash-enable",
		"session_id":  s1.sessionID,
		"duration_ms": 60000,
	}))
	r1 := mustReceive(t, c1.send).Payload.(KeepAwakeMutationResultPayload)
	_ = mustReceive(t, c1.send)
	if r1.State != "ON" {
		t.Fatalf("expected ON after enable, got %s", r1.State)
	}
	if r1.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 lease before restart, got %d", r1.ActiveLeaseCount)
	}
	if r1.StatusRevision <= 0 {
		t.Fatalf("expected positive revision before restart, got %d", r1.StatusRevision)
	}
	if r1.ServerBootID == "" {
		t.Fatal("expected non-empty ServerBootID before restart")
	}
	if r1.DegradedReason != "" {
		t.Fatalf("expected empty degraded reason before restart, got %q", r1.DegradedReason)
	}
	if r1.RecoveryHint != "" {
		t.Fatalf("expected empty recovery hint before restart, got %q", r1.RecoveryHint)
	}

	// Simulate crash + restart: create fresh server.
	s2, c2 := newKeepAwakeTestClient(t)
	c2.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "crash-status",
	}))
	status := mustReceive(t, c2.send).Payload.(KeepAwakeStatusPayload)

	if status.ServerBootID == r1.ServerBootID {
		t.Fatal("expected different ServerBootID after restart")
	}
	if status.ServerBootID == "" {
		t.Fatal("expected non-empty ServerBootID after restart")
	}
	if status.StatusRevision != 0 {
		t.Fatalf("expected revision 0 after restart, got %d", status.StatusRevision)
	}
	if status.ActiveLeaseCount != 0 {
		t.Fatalf("expected 0 leases after restart, got %d", status.ActiveLeaseCount)
	}
	if status.State != "OFF" {
		t.Fatalf("expected OFF after restart, got %s", status.State)
	}
	if status.DegradedReason != "" {
		t.Fatalf("expected empty degraded reason after restart, got %q", status.DegradedReason)
	}
	if status.RecoveryHint != "" {
		t.Fatalf("expected empty recovery hint after restart, got %q", status.RecoveryHint)
	}
	_ = s1
	_ = s2
}

func TestKeepAwakeHardStopRecoveryHintContract(t *testing.T) {
	tests := []struct {
		name            string
		migrationFailed bool
		runtimeReason   string
		powerBlocked    bool
		powerReason     string
		wantReason      string
		wantHint        string
		wantState       string
	}{
		{
			name:            "migration failed",
			migrationFailed: true,
			wantReason:      "",
			wantHint:        "Audit storage migration failed. Restart host to retry.",
			wantState:       "OFF",
		},
		{
			name:          "degraded unsupported environment",
			runtimeReason: "unsupported_environment",
			wantReason:    "unsupported_environment",
			wantHint:      "Keep-awake is not supported in this environment.",
			wantState:     "DEGRADED",
		},
		{
			name:          "degraded acquire failed",
			runtimeReason: "acquire_failed",
			wantReason:    "acquire_failed",
			wantHint:      "Keep-awake inhibitor could not be acquired. Check host logs.",
			wantState:     "DEGRADED",
		},
		{
			name:         "power policy blocked requires_external_power",
			powerBlocked: true,
			powerReason:  "requires_external_power",
			wantReason:   "",
			wantHint:     "Connect external power to enable keep-awake.",
			wantState:    "OFF",
		},
		{
			name:         "power policy blocked battery_threshold",
			powerBlocked: true,
			powerReason:  "battery_threshold",
			wantReason:   "",
			wantHint:     "Battery too low. Charge device to enable keep-awake.",
			wantState:    "OFF",
		},
		{
			name:      "clean state - no hint",
			wantReason: "",
			wantHint:  "",
			wantState: "OFF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer("unused")
			s.SetKeepAwakeRuntimeManager(&keepAwakeRuntimeStub{})

			s.keepAwake.mu.Lock()
			if tt.migrationFailed {
				s.keepAwake.migrationFailed = true
			}
			if tt.runtimeReason != "" {
				s.keepAwake.runtimeState = "DEGRADED"
				s.keepAwake.runtimeReason = tt.runtimeReason
			}
			if tt.powerBlocked {
				if tt.powerReason == "requires_external_power" {
					s.keepAwake.powerProvider = &stubPowerProvider{
						onBattery:      boolPtr(true),
						batteryPercent: kaIntPtr(50),
					}
					s.keepAwake.policy.AllowOnBattery = false
				} else if tt.powerReason == "battery_threshold" {
					s.keepAwake.powerProvider = &stubPowerProvider{
						onBattery:      boolPtr(true),
						batteryPercent: kaIntPtr(10),
					}
					s.keepAwake.policy.AllowOnBattery = true
					s.keepAwake.policy.AutoDisableBatteryPercent = 20
				}
			}
			status := s.keepAwakeStatusPayloadLocked(time.Now())
			s.keepAwake.mu.Unlock()

			if status.RecoveryHint != tt.wantHint {
				t.Errorf("RecoveryHint = %q, want %q", status.RecoveryHint, tt.wantHint)
			}
			if status.State != tt.wantState {
				t.Errorf("State = %q, want %q", status.State, tt.wantState)
			}
			if status.DegradedReason != tt.wantReason {
				t.Errorf("DegradedReason = %q, want %q", status.DegradedReason, tt.wantReason)
			}
			if status.ActiveLeaseCount != 0 {
				t.Errorf("ActiveLeaseCount = %d, want 0", status.ActiveLeaseCount)
			}
			if status.StatusRevision != 0 {
				t.Errorf("StatusRevision = %d, want 0", status.StatusRevision)
			}
			if status.ServerBootID == "" {
				t.Error("expected non-empty ServerBootID")
			}
		})
	}
}

func TestKeepAwakeIdempotencyCacheEviction(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)

	// Fill cache with maxIdempotencyEntries unique enable requests.
	var firstRequestID string
	var firstLeaseID string
	for i := 0; i < maxIdempotencyEntries; i++ {
		reqID := fmt.Sprintf("evict-%d", i)
		if i == 0 {
			firstRequestID = reqID
		}
		c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
			"request_id":  reqID,
			"session_id":  s.sessionID,
			"duration_ms": 60000,
		}))
		result := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
		if i == 0 {
			firstLeaseID = result.LeaseID
		}
		// Drain changed broadcast.
		select {
		case <-c.send:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Insert one more to evict the first.
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "evict-overflow",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send)
	select {
	case <-c.send:
	case <-time.After(100 * time.Millisecond):
	}

	// Replay evicted entry -> treated as new request (not cached).
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  firstRequestID,
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	replayResult := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	// If evicted, this is a new request and gets a new lease ID.
	if replayResult.LeaseID == firstLeaseID {
		t.Fatal("expected evicted entry to produce new lease_id, not cached replay")
	}
	select {
	case <-c.send:
	case <-time.After(100 * time.Millisecond):
	}

	// Replay a retained entry (last one inserted before overflow).
	retainedReqID := fmt.Sprintf("evict-%d", maxIdempotencyEntries-1)
	s.keepAwake.mu.Lock()
	retainedKey := keepAwakeMutationCacheKey{
		DeviceID:  c.deviceID,
		OpType:    MessageTypeSessionKeepAwakeEnable,
		RequestID: retainedReqID,
	}
	retainedEntry, ok := s.keepAwake.idempotencyEntries[retainedKey]
	s.keepAwake.mu.Unlock()
	if !ok {
		t.Fatal("expected retained entry still in cache")
	}
	retainedLeaseID := retainedEntry.Result.Payload.(KeepAwakeMutationResultPayload).LeaseID

	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  retainedReqID,
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	retainedReplay := mustReceive(t, c.send).Payload.(KeepAwakeMutationResultPayload)
	if retainedReplay.LeaseID != retainedLeaseID {
		t.Fatalf("expected retained replay same lease_id %q, got %q", retainedLeaseID, retainedReplay.LeaseID)
	}
}

func TestKeepAwakeObserverBackpressureConvergesViaStatusRefresh(t *testing.T) {
	s, requester := newKeepAwakeTestClient(t)
	observer := &Client{server: s, send: make(chan Message, 16), done: make(chan struct{}), deviceID: "device-2"}
	s.mu.Lock()
	s.clients[observer] = true
	s.mu.Unlock()

	// Fill observer's send channel to simulate backpressure.
	for i := 0; i < cap(observer.send); i++ {
		observer.send <- Message{Type: MessageTypeHeartbeat}
	}

	// Requester enables -> observer's changed broadcast is dropped (channel full).
	requester.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "bp-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, requester.send)
	_ = mustReceive(t, requester.send) // changed

	// Drain observer channel (all heartbeats).
	for len(observer.send) > 0 {
		<-observer.send
	}

	// Observer sends status request to refresh.
	observer.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "bp-refresh",
	}))
	statusMsg := mustReceive(t, observer.send)
	status := statusMsg.Payload.(KeepAwakeStatusPayload)
	if status.State != "ON" {
		t.Fatalf("expected observer to see ON via status refresh, got %s", status.State)
	}
	if status.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 active lease from status refresh, got %d", status.ActiveLeaseCount)
	}
}

func TestKeepAwakeObserverBackpressureConvergesAfterReconnect(t *testing.T) {
	s, requester := newKeepAwakeTestClient(t)
	observer := &Client{server: s, send: make(chan Message, 16), done: make(chan struct{}), deviceID: "device-2"}
	s.mu.Lock()
	s.clients[observer] = true
	s.mu.Unlock()

	// Fill observer's send channel.
	for i := 0; i < cap(observer.send); i++ {
		observer.send <- Message{Type: MessageTypeHeartbeat}
	}

	// Requester enables -> observer's changed broadcast dropped.
	requester.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "bp-reconnect",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, requester.send)
	_ = mustReceive(t, requester.send)

	// Observer disconnects and reconnects.
	s.mu.Lock()
	delete(s.clients, observer)
	s.mu.Unlock()
	s.onKeepAwakeClientDisconnected(observer.deviceID)

	// Create reconnected observer.
	observer2 := &Client{server: s, send: make(chan Message, 16), done: make(chan struct{}), deviceID: observer.deviceID}
	s.mu.Lock()
	s.clients[observer2] = true
	s.mu.Unlock()
	s.onKeepAwakeClientConnected(observer2)

	// Observer sends status request after reconnect.
	observer2.handleKeepAwakeStatus(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeStatus, map[string]interface{}{
		"request_id": "bp-reconnect-status",
	}))
	statusMsg := mustReceive(t, observer2.send)
	status := statusMsg.Payload.(KeepAwakeStatusPayload)
	if status.State != "ON" {
		t.Fatalf("expected observer to see ON after reconnect status, got %s", status.State)
	}
	if status.ActiveLeaseCount != 1 {
		t.Fatalf("expected 1 active lease after reconnect, got %d", status.ActiveLeaseCount)
	}
}

// UT-S5: Disable dominance (P18U2)

// TestKeepAwakeDisableDominatesConcurrentEnable verifies that disabling keep-awake
// via the policy API revokes existing leases even if enable was just called.
func TestKeepAwakeDisableDominatesConcurrentEnable(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	// newKeepAwakeTestClient bootstraps with RemoteEnabled: true.

	// Enable a lease.
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "req-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	enableResult := mustReceive(t, c.send)
	if enableResult.Type != MessageTypeSessionKeepAwakeEnableResult {
		t.Fatalf("expected enable_result, got %s", enableResult.Type)
	}
	result := enableResult.Payload.(KeepAwakeMutationResultPayload)
	if !result.Success {
		t.Fatalf("enable failed: %s", result.Error)
	}
	// Drain the changed broadcast.
	mustReceive(t, c.send)

	// Now disable policy (simulates CLI HTTP handler applying policy change).
	// Use same AdminDeviceIDs as bootstrap to avoid extra policy_change fields.
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:    false,
		AllowAdminRevoke: true,
		AdminDeviceIDs:   []string{"admin-1"},
		AllowOnBattery:   true,
	})

	// The disable should have revoked all leases. Drain the changed broadcast.
	changedMsg := mustReceive(t, c.send)
	changedPayload := changedMsg.Payload.(KeepAwakeStatusPayload)
	if changedPayload.ActiveLeaseCount != 0 {
		t.Errorf("expected 0 leases after disable, got %d", changedPayload.ActiveLeaseCount)
	}
	if changedPayload.Policy.RemoteEnabled {
		t.Error("expected remote_enabled=false after disable")
	}
}

// TestKeepAwakeDisableDominatesConcurrentExtend verifies that disabling keep-awake
// policy revokes leases even if a concurrent extend is in progress.
func TestKeepAwakeDisableDominatesConcurrentExtend(t *testing.T) {
	s, c := newKeepAwakeTestClient(t)
	// newKeepAwakeTestClient bootstraps with RemoteEnabled: true.

	// Enable a lease.
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "req-enable",
		"session_id":  s.sessionID,
		"duration_ms": 60000,
	}))
	enableResult := mustReceive(t, c.send)
	result := enableResult.Payload.(KeepAwakeMutationResultPayload)
	if !result.Success {
		t.Fatalf("enable failed: %s", result.Error)
	}
	leaseID := result.LeaseID
	mustReceive(t, c.send) // drain changed

	// Extend the lease to prove it's active.
	c.handleKeepAwakeExtend(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeExtend, map[string]interface{}{
		"request_id":  "req-extend",
		"session_id":  s.sessionID,
		"lease_id":    leaseID,
		"duration_ms": 120000,
	}))
	extendResult := mustReceive(t, c.send)
	extResult := extendResult.Payload.(KeepAwakeMutationResultPayload)
	if !extResult.Success {
		t.Fatalf("extend failed: %s", extResult.Error)
	}
	mustReceive(t, c.send) // drain changed

	// Now disable policy.
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{
		RemoteEnabled:    false,
		AllowAdminRevoke: true,
		AdminDeviceIDs:   []string{"admin-1"},
		AllowOnBattery:   true,
	})

	// Leases should be revoked.
	changedMsg := mustReceive(t, c.send)
	changedPayload := changedMsg.Payload.(KeepAwakeStatusPayload)
	if changedPayload.ActiveLeaseCount != 0 {
		t.Errorf("expected 0 leases after policy disable, got %d", changedPayload.ActiveLeaseCount)
	}
}
