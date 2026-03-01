package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/config"
)

// policyTestTokenValidator implements ApprovalTokenValidator for policy tests.
type policyTestTokenValidator struct {
	validToken string
}

func (v *policyTestTokenValidator) ValidateToken(token string) bool {
	return v.validToken != "" && token == v.validToken
}

func newPolicyTestHandler(t *testing.T, configPath string) (*KeepAwakePolicyHandler, *Server) {
	t.Helper()
	s := NewServer("unused")
	s.SetRequireAuth(true)
	s.SetKeepAwakeRuntimeManager(&keepAwakeRuntimeStub{})
	s.SetKeepAwakeAuditWriter(&keepAwakeAuditStub{})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: false, AllowOnBattery: true})

	validator := &policyTestTokenValidator{validToken: "test-token-123"}
	h := NewKeepAwakePolicyHandler(s, validator, configPath)
	return h, s
}

func policyRequest(t *testing.T, h http.Handler, method, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		bodyReader = bytes.NewBuffer(b)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, "/api/keep-awake/policy", bodyReader)
	req.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodePolicyResponse(t *testing.T, rr *httptest.ResponseRecorder) KeepAwakePolicyMutationResponse {
	t.Helper()
	var resp KeepAwakePolicyMutationResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	return resp
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// UT-S1: Transport + Auth

func TestKeepAwakePolicyAPI_LoopbackEnforcement(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	body := map[string]interface{}{"request_id": "r1", "remote_enabled": true}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/keep-awake/policy", bytes.NewBuffer(b))
	req.RemoteAddr = "192.168.1.100:12345"
	req.Header.Set("Authorization", "Bearer test-token-123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	resp := decodePolicyResponse(t, rr)
	if resp.ErrorCode != "keep_awake.unauthorized" {
		t.Errorf("error_code = %q, want keep_awake.unauthorized", resp.ErrorCode)
	}
}

func TestKeepAwakePolicyAPI_BearerAuth(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, _ := newPolicyTestHandler(t, cfgPath)
	enable := true
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": enable,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Success {
		t.Errorf("expected success, got error=%q code=%q", resp.Error, resp.ErrorCode)
	}
}

func TestKeepAwakePolicyAPI_InvalidToken(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "POST", "wrong-token", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestKeepAwakePolicyAPI_InvalidPayload(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	// Unknown field.
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true, "unknown_field": "bad",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestKeepAwakePolicyAPI_MethodNotAllowed(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "GET", "test-token-123", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestKeepAwakePolicyAPI_MissingAuthHeader(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "POST", "", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// UT-S2: Core mutations

func TestKeepAwakePolicyAPI_SuccessEnable(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Success {
		t.Fatalf("expected success, got error=%q", resp.Error)
	}
	if !resp.KeepAwake.Policy.RemoteEnabled {
		t.Error("expected keep_awake=true")
	}
	if !resp.HotApplied {
		t.Error("expected hot_applied=true")
	}
	if !resp.Persisted {
		t.Error("expected persisted=true")
	}
	if resp.StatusRevision < 1 {
		t.Errorf("expected status_revision >= 1, got %d", resp.StatusRevision)
	}

	// Verify in-memory policy changed.
	s.keepAwake.mu.Lock()
	enabled := s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if !enabled {
		t.Error("in-memory policy should be enabled")
	}
}

func TestKeepAwakePolicyAPI_SuccessDisable(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = true\n")
	h, s := newPolicyTestHandler(t, cfgPath)
	// Start with policy enabled.
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: true, AllowOnBattery: true})

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": false,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Success {
		t.Fatalf("expected success, got error=%q", resp.Error)
	}
	if resp.KeepAwake.Policy.RemoteEnabled {
		t.Error("expected keep_awake=false")
	}
}

func TestKeepAwakePolicyAPI_IdempotentReplay(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, _ := newPolicyTestHandler(t, cfgPath)

	body := map[string]interface{}{"request_id": "r1", "remote_enabled": true}
	rr1 := policyRequest(t, h, "POST", "test-token-123", body)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rr1.Code)
	}
	resp1 := decodePolicyResponse(t, rr1)

	// Same request_id, same parameters -> replay.
	rr2 := policyRequest(t, h, "POST", "test-token-123", body)
	if rr2.Code != http.StatusOK {
		t.Fatalf("replay: expected 200, got %d", rr2.Code)
	}
	resp2 := decodePolicyResponse(t, rr2)

	if resp1.StatusRevision != resp2.StatusRevision {
		t.Errorf("replay revision mismatch: %d vs %d", resp1.StatusRevision, resp2.StatusRevision)
	}
}

func TestKeepAwakePolicyAPI_IdempotencyMismatch(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, _ := newPolicyTestHandler(t, cfgPath)

	rr1 := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}

	// Same request_id, different parameters -> conflict.
	rr2 := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": false,
	})
	if rr2.Code != http.StatusConflict {
		t.Fatalf("mismatch: expected 409, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// UT-S3: Edge cases

func TestKeepAwakePolicyAPI_AuthValidatorUnavailable(t *testing.T) {
	s := NewServer("unused")
	h := NewKeepAwakePolicyHandler(s, nil, "")
	rr := policyRequest(t, h, "POST", "any-token", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}

func TestKeepAwakePolicyAPI_CallerScopedIdempotency(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	s := NewServer("unused")
	s.SetKeepAwakeRuntimeManager(&keepAwakeRuntimeStub{})
	s.SetKeepAwakeAuditWriter(&keepAwakeAuditStub{})
	s.SetKeepAwakePolicy(KeepAwakePolicyConfig{RemoteEnabled: false, AllowOnBattery: true})

	// Two different tokens = two different callers.
	validator := &multiTokenValidator{tokens: map[string]bool{"token-a": true, "token-b": true}}
	h := NewKeepAwakePolicyHandler(s, validator, cfgPath)

	// Caller A enables.
	rrA := policyRequest(t, h, "POST", "token-a", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rrA.Code != http.StatusOK {
		t.Fatalf("caller A: expected 200, got %d: %s", rrA.Code, rrA.Body.String())
	}

	// Caller B uses same request_id but different enable value.
	// Different caller identity -> different idempotency key -> not a mismatch.
	cfgPath2 := writeTempConfig(t, "keep_awake_remote_enabled = true\n")
	h2 := NewKeepAwakePolicyHandler(s, validator, cfgPath2)
	rrB := policyRequest(t, h2, "POST", "token-b", map[string]interface{}{
		"request_id": "r1", "remote_enabled": false,
	})
	if rrB.Code != http.StatusOK {
		t.Fatalf("caller B: expected 200, got %d: %s", rrB.Code, rrB.Body.String())
	}
}

func TestKeepAwakePolicyAPI_NoOpSemantics(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)

	// Policy is already disabled. Disable again -> no-op.
	s.keepAwake.mu.Lock()
	revBefore := s.keepAwake.statusRevision
	s.keepAwake.mu.Unlock()

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": false,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Success {
		t.Fatalf("expected success, got error=%q", resp.Error)
	}
	// Revision should not have changed for a no-op.
	if resp.StatusRevision != revBefore {
		t.Errorf("revision changed for no-op: %d -> %d", revBefore, resp.StatusRevision)
	}
	if resp.Persisted {
		t.Error("expected persisted=false for no-op mutation")
	}
}

func TestKeepAwakePolicyAPI_ReasonBounds(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, _ := newPolicyTestHandler(t, cfgPath)

	// 256-byte reason should work.
	longReason := strings.Repeat("x", 256)
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true, "reason": longReason,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestKeepAwakePolicyAPI_ReasonNormalization(t *testing.T) {
	result, err := normalizeKeepAwakePolicyReason("  hello   world\t\n  ")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if result != "hello world" {
		t.Errorf("normalized = %q, want %q", result, "hello world")
	}

	// Non-printable ASCII is rejected.
	_, err = normalizeKeepAwakePolicyReason("hello\x01world")
	if err == nil {
		t.Fatal("expected printable ASCII validation error")
	}

	// Reasons above 256 bytes are rejected.
	long := strings.Repeat("a", 300)
	_, err = normalizeKeepAwakePolicyReason(long)
	if err == nil {
		t.Fatal("expected max-length validation error")
	}
}

// UT-S4: Failure modes

func TestKeepAwakePolicyAPI_PersistFailureNoMutation(t *testing.T) {
	// No config path -> persist fails and mutation is fail-closed.
	h, s := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if resp.Success {
		t.Fatalf("expected failure, got success")
	}
	if resp.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected keep_awake.conflict, got %q", resp.ErrorCode)
	}

	// In-memory policy should not change when persist fails.
	s.keepAwake.mu.Lock()
	enabled := s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if enabled {
		t.Error("in-memory policy should remain disabled when persist fails")
	}
}

func TestKeepAwakePolicyAPI_ApplyFailureRollback(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)

	// Make audit fail.
	setKeepAwakeAuditFailMode(t, s, true)

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	// Policy should have been rolled back.
	s.keepAwake.mu.Lock()
	enabled := s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if enabled {
		t.Error("policy should be rolled back to false after audit failure")
	}
}

func TestKeepAwakePolicyAPI_ApplyFailureRollbackFailureGuidance(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)
	setKeepAwakeAuditFailMode(t, s, true)

	persistCalls := 0
	h.persistPolicy = func(path string, remoteEnabled bool) error {
		persistCalls++
		if persistCalls == 1 {
			return config.PersistKeepAwakePolicy(path, remoteEnabled)
		}
		return fmt.Errorf("simulated rollback write failure")
	}

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r-rollback-fail", "remote_enabled": true,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if resp.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected keep_awake.conflict, got %q", resp.ErrorCode)
	}
	if !strings.Contains(resp.Error, "rollback failed") {
		t.Fatalf("expected rollback-failed guidance, got %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "manually restore keep_awake_remote_enabled") {
		t.Fatalf("expected manual recovery guidance, got %q", resp.Error)
	}

	s.keepAwake.mu.Lock()
	enabled := s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if enabled {
		t.Error("runtime policy should be fail-closed after rollback failure")
	}
}

func TestKeepAwakePolicyAPI_DisableRevokeAuditFailureFailsClosed(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = true\n")

	s, c := newKeepAwakeTestClient(t)
	validator := &policyTestTokenValidator{validToken: "test-token-123"}
	h := NewKeepAwakePolicyHandler(s, validator, cfgPath)

	// Seed one active lease so disable path must emit policy_revoke audits.
	c.handleKeepAwakeEnable(encodeKeepAwakeRequest(t, MessageTypeSessionKeepAwakeEnable, map[string]interface{}{
		"request_id":  "seed-lease",
		"session_id":  s.SessionID(),
		"duration_ms": 60000,
	}))
	_ = mustReceive(t, c.send) // enable result
	_ = mustReceive(t, c.send) // changed broadcast

	s.SetKeepAwakeAuditWriter(&failOnOperationAuditWriter{failOperation: "policy_revoke"})

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r-disable", "remote_enabled": false,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if resp.ErrorCode != "keep_awake.conflict" {
		t.Fatalf("expected keep_awake.conflict, got %q", resp.ErrorCode)
	}

	s.keepAwake.mu.Lock()
	leaseCount := len(s.keepAwake.leases)
	migrationFailed := s.keepAwake.migrationFailed
	policyDisabled := !s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("expected leases revoked under fail-closed policy, got %d", leaseCount)
	}
	if !migrationFailed {
		t.Fatal("expected migrationFailed flag set after revoke audit failure")
	}
	if !policyDisabled {
		t.Fatal("expected remote policy to be disabled after fail-closed transition")
	}
}

func TestKeepAwakePolicyAPI_BroadcastFailurePreservation(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)

	// No clients registered, so broadcast is effectively a no-op.
	// The response should still indicate success.
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Success {
		t.Fatalf("expected success despite no connected clients")
	}

	// Policy should be applied regardless of broadcast.
	s.keepAwake.mu.Lock()
	enabled := s.keepAwake.policy.RemoteEnabled
	s.keepAwake.mu.Unlock()
	if !enabled {
		t.Error("policy should be enabled despite no broadcast targets")
	}
}

func TestKeepAwakePolicyAPI_StatusRevisionRules(t *testing.T) {
	cfgPath := writeTempConfig(t, "keep_awake_remote_enabled = false\n")
	h, s := newPolicyTestHandler(t, cfgPath)

	s.keepAwake.mu.Lock()
	revBefore := s.keepAwake.statusRevision
	s.keepAwake.mu.Unlock()

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	resp := decodePolicyResponse(t, rr)
	if resp.StatusRevision <= revBefore {
		t.Errorf("revision should increase: %d -> %d", revBefore, resp.StatusRevision)
	}
}

func TestKeepAwakePolicyAPI_CallerIdentityFingerprint(t *testing.T) {
	// Verify that sha256Hex produces consistent fingerprints.
	fp1 := sha256Hex("test-token-123")
	fp2 := sha256Hex("test-token-123")
	if fp1 != fp2 {
		t.Errorf("inconsistent fingerprint: %s vs %s", fp1, fp2)
	}
	fp3 := sha256Hex("different-token")
	if fp1 == fp3 {
		t.Error("different tokens should produce different fingerprints")
	}
	if len(fp1) != 64 {
		t.Errorf("expected 64-char hex fingerprint, got %d chars", len(fp1))
	}
}

func TestKeepAwakePolicyAPI_MissingRequestID(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"remote_enabled": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestKeepAwakePolicyAPI_MissingRemoteEnabled(t *testing.T) {
	h, _ := newPolicyTestHandler(t, "")
	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// multiTokenValidator accepts multiple tokens.
type multiTokenValidator struct {
	tokens map[string]bool
}

func (v *multiTokenValidator) ValidateToken(token string) bool {
	return v.tokens[token]
}

type failOnOperationAuditWriter struct {
	failOperation string
}

func (w *failOnOperationAuditWriter) WriteKeepAwakeAudit(e KeepAwakeAuditEvent) error {
	if e.Operation == w.failOperation {
		return fmt.Errorf("forced audit failure for operation %s", e.Operation)
	}
	return nil
}

// UT-S5: Disable dominance tests are in handlers_keepawake_test.go

// TestKeepAwakePolicyAPI_EnableAppendsMissingKey tests that PersistKeepAwakePolicy
// can add the key when it's missing from the config file.
func TestKeepAwakePolicyAPI_EnableAppendsMissingKey(t *testing.T) {
	cfgPath := writeTempConfig(t, "repo = \"/myrepo\"\n")
	h, _ := newPolicyTestHandler(t, cfgPath)

	rr := policyRequest(t, h, "POST", "test-token-123", map[string]interface{}{
		"request_id": "r1", "remote_enabled": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePolicyResponse(t, rr)
	if !resp.Persisted {
		t.Error("expected persisted=true when key is appended")
	}

	// Verify the file now has the key.
	content, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(content), "keep_awake_remote_enabled = true") {
		t.Errorf("expected key appended to config, got:\n%s", content)
	}
}

// Wait a bit for broadcast goroutines to settle.
func init() {
	_ = time.Millisecond // used in other test files
}
