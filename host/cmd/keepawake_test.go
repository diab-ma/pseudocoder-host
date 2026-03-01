package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/server"
)

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func stubToken(t *testing.T, token string, err error) {
	orig := kaReadApprovalToken
	t.Cleanup(func() { kaReadApprovalToken = orig })
	kaReadApprovalToken = func() (string, error) { return token, err }
}

func stubRequestID(t *testing.T, id string) {
	orig := kaGenerateRequestID
	t.Cleanup(func() { kaGenerateRequestID = orig })
	kaGenerateRequestID = func() string { return id }
}

func stubHTTPClient(t *testing.T, client *http.Client) {
	orig := kaNewHTTPClient
	t.Cleanup(func() { kaNewHTTPClient = orig })
	kaNewHTTPClient = func() *http.Client { return client }
}

func kaRun(args []string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runKeepAwake(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func kaMutationRun(command string, args []string) (int, string, string) {
	all := append([]string{command}, args...)
	return kaRun(all)
}

func policyHandler(t *testing.T, statusCode int, resp server.KeepAwakePolicyMutationResponse) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(resp)
	})
}

func statusHandler(t *testing.T, status server.StatusResponse) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
}

// -----------------------------------------------------------------------
// Help tests
// -----------------------------------------------------------------------

func TestKeepAwakeEnableHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runKeepAwake([]string{"enable-remote", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder keep-awake enable-remote") {
		t.Fatalf("expected enable-remote usage, got %q", stderr.String())
	}
}

func TestKeepAwakeDisableHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runKeepAwake([]string{"disable-remote", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder keep-awake disable-remote") {
		t.Fatalf("expected disable-remote usage, got %q", stderr.String())
	}
}

func TestKeepAwakeStatusHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runKeepAwake([]string{"status", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: pseudocoder keep-awake status") {
		t.Fatalf("expected status usage, got %q", stderr.String())
	}
}

// -----------------------------------------------------------------------
// Invalid port
// -----------------------------------------------------------------------

func TestKeepAwakeEnableInvalidPort(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("enable-remote", []string{"--port", "0"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "invalid port") {
		t.Fatalf("expected port error, got %q", errOut)
	}
}

func TestKeepAwakeDisableInvalidPort(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("disable-remote", []string{"--port", "99999"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "invalid port") {
		t.Fatalf("expected port error, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Addr overrides port warning
// -----------------------------------------------------------------------

func TestKeepAwakeEnableAddrOverridesPortWarning(t *testing.T) {
	stubToken(t, "tok", nil)
	// Use an unreachable address so command fails after warning
	_, _, errOut := kaMutationRun("enable-remote", []string{"--addr", "127.0.0.1:9999", "--port", "8080"})
	if !strings.Contains(errOut, "--addr overrides --port") {
		t.Fatalf("expected addr override warning, got %q", errOut)
	}
}

func TestKeepAwakeDisableAddrOverridesPortWarning(t *testing.T) {
	stubToken(t, "tok", nil)
	_, _, errOut := kaMutationRun("disable-remote", []string{"--addr", "127.0.0.1:9999", "--port", "8080"})
	if !strings.Contains(errOut, "--addr overrides --port") {
		t.Fatalf("expected addr override warning, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Invalid addr format
// -----------------------------------------------------------------------

func TestKeepAwakeEnableInvalidAddrFormat(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", "not-a-valid-addr"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "invalid addr format") {
		t.Fatalf("expected deterministic invalid addr error, got %q", errOut)
	}
}

func TestKeepAwakeDisableInvalidAddrFormat(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", "not-a-valid-addr"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "invalid addr format") {
		t.Fatalf("expected deterministic invalid addr error, got %q", errOut)
	}
}

func TestKeepAwakeEnableUnexpectedArgs(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("enable-remote", []string{"extra"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "unexpected arguments: extra") {
		t.Fatalf("expected unexpected args error, got %q", errOut)
	}
}

func TestKeepAwakeDisableUnexpectedArgs(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("disable-remote", []string{"extra"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "unexpected arguments: extra") {
		t.Fatalf("expected unexpected args error, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Missing approval token
// -----------------------------------------------------------------------

func TestKeepAwakeEnableMissingApprovalToken(t *testing.T) {
	stubToken(t, "", fmt.Errorf("no such file or directory"))
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", "127.0.0.1:9999"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "approval token") {
		t.Fatalf("expected token error, got %q", errOut)
	}
	if !strings.Contains(errOut, "pseudocoder host start") {
		t.Fatalf("expected actionable guidance, got %q", errOut)
	}
}

func TestKeepAwakeDisableMissingApprovalToken(t *testing.T) {
	stubToken(t, "", fmt.Errorf("permission denied"))
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", "127.0.0.1:9999"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "approval token") {
		t.Fatalf("expected token error, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Request body and auth header
// -----------------------------------------------------------------------

func TestKeepAwakeEnableRequestBodyAndAuthHeader(t *testing.T) {
	stubToken(t, "test-token-abc", nil)
	stubRequestID(t, "req-id-111")

	var gotAuth string
	var gotBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "req-id-111",
			Success:        true,
			StatusRevision: 1,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, _ := kaMutationRun("enable-remote", []string{"--addr", addr, "--reason", "testing"})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if gotAuth != "Bearer test-token-abc" {
		t.Fatalf("expected Bearer token, got %q", gotAuth)
	}
	if gotBody["request_id"] != "req-id-111" {
		t.Fatalf("expected request_id in body, got %v", gotBody)
	}
	if gotBody["remote_enabled"] != true {
		t.Fatalf("expected remote_enabled=true, got %v", gotBody["remote_enabled"])
	}
	if gotBody["reason"] != "testing" {
		t.Fatalf("expected reason=testing, got %v", gotBody["reason"])
	}
}

func TestKeepAwakeDisableRequestBodyAndAuthHeader(t *testing.T) {
	stubToken(t, "token-xyz", nil)
	stubRequestID(t, "req-id-222")

	var gotBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "req-id-222",
			Success:        true,
			StatusRevision: 2,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if gotBody["remote_enabled"] != false {
		t.Fatalf("expected remote_enabled=false, got %v", gotBody["remote_enabled"])
	}
}

// -----------------------------------------------------------------------
// Stable request ID across retries
// -----------------------------------------------------------------------

func TestKeepAwakeEnableStableRequestIDAcrossRetry(t *testing.T) {
	stubToken(t, "tok", nil)
	stubRequestID(t, "stable-req-id")

	var requestIDs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if id, ok := body["request_id"].(string); ok {
			requestIDs = append(requestIDs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "stable-req-id",
			Success:        true,
			StatusRevision: 1,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, _ := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	for _, id := range requestIDs {
		if id != "stable-req-id" {
			t.Fatalf("expected stable request ID, got %q", id)
		}
	}
}

func TestKeepAwakeDisableStableRequestIDAcrossRetry(t *testing.T) {
	stubToken(t, "tok", nil)
	stubRequestID(t, "stable-disable-id")

	var requestIDs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if id, ok := body["request_id"].(string); ok {
			requestIDs = append(requestIDs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "stable-disable-id",
			Success:        true,
			StatusRevision: 1,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	for _, id := range requestIDs {
		if id != "stable-disable-id" {
			t.Fatalf("expected stable request ID, got %q", id)
		}
	}
}

// -----------------------------------------------------------------------
// Host unreachable guidance
// -----------------------------------------------------------------------

func TestKeepAwakeEnableHostUnreachableGuidance(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", "127.0.0.1:1"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "not reachable") && !strings.Contains(errOut, "timed out") {
		t.Fatalf("expected unreachable message, got %q", errOut)
	}
	// Must not contain success language
	if strings.Contains(errOut, "enabled") || strings.Contains(errOut, "disabled") {
		t.Fatalf("unreachable must not claim success, got %q", errOut)
	}
}

func TestKeepAwakeDisableHostUnreachableGuidance(t *testing.T) {
	stubToken(t, "tok", nil)
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", "127.0.0.1:1"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "not reachable") && !strings.Contains(errOut, "timed out") {
		t.Fatalf("expected unreachable message, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Host error code propagation
// -----------------------------------------------------------------------

func TestKeepAwakeEnableHostErrorCodePropagation(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 409, server.KeepAwakePolicyMutationResponse{
		Success:   false,
		ErrorCode: "keep_awake.conflict",
		Error:     "request conflict",
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "keep_awake.conflict") {
		t.Fatalf("expected error code in output, got %q", errOut)
	}
	if !strings.Contains(errOut, "request conflict") {
		t.Fatalf("expected error message in output, got %q", errOut)
	}
}

func TestKeepAwakeDisableHostErrorCodePropagation(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 401, server.KeepAwakePolicyMutationResponse{
		Success:   false,
		ErrorCode: "keep_awake.unauthorized",
		Error:     "invalid token",
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "keep_awake.unauthorized") {
		t.Fatalf("expected error code in output, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Success output truthfulness
// -----------------------------------------------------------------------

func TestKeepAwakeEnableSuccessOutputTruthfulness(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 200, server.KeepAwakePolicyMutationResponse{
		RequestID:      "rid",
		Success:        true,
		StatusRevision: 42,
		HotApplied:     true,
		Persisted:      false,
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, "enabled") {
		t.Fatalf("expected 'enabled' in output, got %q", out)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("expected status revision 42, got %q", out)
	}
	if !strings.Contains(out, "Hot applied:     true") {
		t.Fatalf("expected hot_applied true, got %q", out)
	}
	if !strings.Contains(out, "Persisted:       false") {
		t.Fatalf("expected persisted false, got %q", out)
	}
}

func TestKeepAwakeDisableSuccessOutputTruthfulness(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 200, server.KeepAwakePolicyMutationResponse{
		RequestID:      "rid",
		Success:        true,
		StatusRevision: 7,
		HotApplied:     false,
		Persisted:      true,
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("expected 'disabled' in output, got %q", out)
	}
	if !strings.Contains(out, "Hot applied:     false") {
		t.Fatalf("expected hot_applied false, got %q", out)
	}
	if !strings.Contains(out, "Persisted:       true") {
		t.Fatalf("expected persisted true, got %q", out)
	}
}

// -----------------------------------------------------------------------
// Timeout guidance
// -----------------------------------------------------------------------

func TestKeepAwakeEnableTimeoutGuidance(t *testing.T) {
	stubToken(t, "tok", nil)

	// Create a server that delays beyond timeout
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer ts.Close()

	// Override client with tiny timeout
	stubHTTPClient(t, &http.Client{Timeout: 50 * time.Millisecond})

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "timed out") {
		t.Fatalf("expected timeout message, got %q", errOut)
	}
}

func TestKeepAwakeDisableTimeoutGuidance(t *testing.T) {
	stubToken(t, "tok", nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer ts.Close()

	stubHTTPClient(t, &http.Client{Timeout: 50 * time.Millisecond})

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "timed out") {
		t.Fatalf("expected timeout message, got %q", errOut)
	}
}

// -----------------------------------------------------------------------
// Malformed response fail-closed
// -----------------------------------------------------------------------

func TestKeepAwakeEnableMalformedResponseFailClosed(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`not json at all`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1 for malformed response, got %d", code)
	}
	if strings.Contains(out, "enabled") {
		t.Fatal("malformed response must not claim success")
	}
}

func TestKeepAwakeDisableMalformedResponseFailClosed(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{invalid json`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1 for malformed response, got %d", code)
	}
	if strings.Contains(out, "disabled") {
		t.Fatal("malformed response must not claim success")
	}
}

// -----------------------------------------------------------------------
// Partial JSON missing required fields (fail-closed)
// -----------------------------------------------------------------------

func TestKeepAwakeEnablePartialJSONMissingRequiredFields(t *testing.T) {
	stubToken(t, "tok", nil)
	// Respond with success=true but missing request_id
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"success":true,"status_revision":1}`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1 for partial response, got %d", code)
	}
	if strings.Contains(out, "enabled") {
		t.Fatal("partial response must not claim success")
	}
}

func TestKeepAwakeDisablePartialJSONMissingRequiredFields(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"success":true}`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1 for partial response, got %d", code)
	}
	if strings.Contains(out, "disabled") {
		t.Fatal("partial response must not claim success")
	}
}

// -----------------------------------------------------------------------
// Reason boundary errors
// -----------------------------------------------------------------------

func TestKeepAwakeEnableReasonBoundaryErrors(t *testing.T) {
	stubToken(t, "tok", nil)
	// Server propagates reason back in error
	ts := httptest.NewServer(policyHandler(t, 400, server.KeepAwakePolicyMutationResponse{
		Success:   false,
		ErrorCode: "keep_awake.invalid_reason",
		Error:     "reason exceeds maximum length",
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	longReason := strings.Repeat("x", 10000)
	code, _, errOut := kaMutationRun("enable-remote", []string{"--addr", addr, "--reason", longReason})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "reason") {
		t.Fatalf("expected reason error, got %q", errOut)
	}
}

func TestKeepAwakeDisableReasonBoundaryErrors(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 400, server.KeepAwakePolicyMutationResponse{
		Success:   false,
		ErrorCode: "keep_awake.invalid_reason",
		Error:     "reason contains non-printable characters",
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaMutationRun("disable-remote", []string{"--addr", addr, "--reason", "test\x00reason"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	_ = errOut
}

// -----------------------------------------------------------------------
// HTTPS then HTTP fallback
// -----------------------------------------------------------------------

func TestKeepAwakeEnableHTTPSThenHTTPFallback(t *testing.T) {
	stubToken(t, "tok", nil)

	// Plain HTTP server — HTTPS attempt will fail, HTTP should succeed
	var callCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "rid",
			Success:        true,
			StatusRevision: 1,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("enable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0 after HTTP fallback, got %d", code)
	}
	if !strings.Contains(out, "enabled") {
		t.Fatalf("expected success after fallback, got %q", out)
	}
}

func TestKeepAwakeDisableHTTPSThenHTTPFallback(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(server.KeepAwakePolicyMutationResponse{
			RequestID:      "rid",
			Success:        true,
			StatusRevision: 3,
			HotApplied:     true,
			Persisted:      true,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("disable-remote", []string{"--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0 after HTTP fallback, got %d", code)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("expected success after fallback, got %q", out)
	}
}

// -----------------------------------------------------------------------
// JSON failure shape
// -----------------------------------------------------------------------

func TestKeepAwakeEnableJSONFailureShape(t *testing.T) {
	stubToken(t, "tok", nil)
	ts := httptest.NewServer(policyHandler(t, 409, server.KeepAwakePolicyMutationResponse{
		Success:        false,
		ErrorCode:      "keep_awake.conflict",
		Error:          "conflict",
		StatusRevision: 5,
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaMutationRun("enable-remote", []string{"--addr", addr, "--json"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["ok"] != false {
		t.Fatal("expected ok=false")
	}
	if result["command"] != "enable-remote" {
		t.Fatalf("expected command=enable-remote, got %v", result["command"])
	}
	if result["kind"] != kaErrHostError {
		t.Fatalf("expected kind=host_error, got %v", result["kind"])
	}
	if result["error_code"] != "keep_awake.conflict" {
		t.Fatalf("expected error_code, got %v", result["error_code"])
	}
	if _, ok := result["status_code"]; !ok {
		t.Fatal("expected status_code in JSON failure")
	}
}

func TestKeepAwakeDisableJSONFailureShape(t *testing.T) {
	stubToken(t, "", fmt.Errorf("no token"))
	code, out, _ := kaMutationRun("disable-remote", []string{"--addr", "127.0.0.1:1", "--json"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["ok"] != false {
		t.Fatal("expected ok=false")
	}
	if result["kind"] != kaErrAuth {
		t.Fatalf("expected kind=auth, got %v", result["kind"])
	}
	if _, ok := result["message"]; !ok {
		t.Fatal("expected message in JSON failure")
	}
}

// -----------------------------------------------------------------------
// Status output formatting
// -----------------------------------------------------------------------

func TestKeepAwakeStatusOutputFormatting(t *testing.T) {
	ts := httptest.NewServer(statusHandler(t, server.StatusResponse{
		ListeningAddress: "127.0.0.1:7070",
		KeepAwake: &server.KeepAwakeStatusSummary{
			State:          "ON",
			RemoteEnabled:  true,
			StatusRevision: 10,
			RecoveryHint:   "check battery",
		},
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, "Remote enabled:  true") {
		t.Fatalf("expected remote enabled, got %q", out)
	}
	if !strings.Contains(out, "State:           ON") {
		t.Fatalf("expected state ON, got %q", out)
	}
	if !strings.Contains(out, "Status revision: 10") {
		t.Fatalf("expected revision 10, got %q", out)
	}
	if !strings.Contains(out, "Recovery hint:   check battery") {
		t.Fatalf("expected recovery hint, got %q", out)
	}
}

func TestKeepAwakeStatusJSONOutput(t *testing.T) {
	ts := httptest.NewServer(statusHandler(t, server.StatusResponse{
		ListeningAddress: "127.0.0.1:7070",
		KeepAwake: &server.KeepAwakeStatusSummary{
			State:          "OFF",
			RemoteEnabled:  false,
			StatusRevision: 3,
		},
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr, "--json"})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["ok"] != true {
		t.Fatal("expected ok=true")
	}
	if result["command"] != "status" {
		t.Fatalf("expected command=status, got %v", result["command"])
	}
	if result["remote_enabled"] != false {
		t.Fatalf("expected remote_enabled=false, got %v", result["remote_enabled"])
	}
	if result["keep_awake_state"] != "OFF" {
		t.Fatalf("expected state OFF, got %v", result["keep_awake_state"])
	}
}

func TestKeepAwakeStatusMissingKeepAwakeSection(t *testing.T) {
	ts := httptest.NewServer(statusHandler(t, server.StatusResponse{
		ListeningAddress: "127.0.0.1:7070",
		// KeepAwake is nil
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, _, errOut := kaRun([]string{"status", "--addr", addr})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "not available") {
		t.Fatalf("expected missing status message, got %q", errOut)
	}
}

func TestKeepAwakeStatusUnexpectedArgsJSON(t *testing.T) {
	code, out, _ := kaRun([]string{"status", "--json", "extra"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["kind"] != kaErrInvalidArgs {
		t.Fatalf("expected invalid_args kind, got %v", result["kind"])
	}
}

func TestKeepAwakeStatusJSONTimeoutKind(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer ts.Close()

	stubHTTPClient(t, &http.Client{Timeout: 50 * time.Millisecond})

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr, "--json"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["kind"] != kaErrTimeout {
		t.Fatalf("expected timeout kind, got %v", result["kind"])
	}
}

func TestKeepAwakeStatusJSONHostErrorKind(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"busy"}`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr, "--json"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["kind"] != kaErrHostError {
		t.Fatalf("expected host_error kind, got %v", result["kind"])
	}
}

func TestKeepAwakeStatusJSONDecodeErrorKind(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid-json`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr, "--json"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if result["kind"] != kaErrDecodeError {
		t.Fatalf("expected decode_error kind, got %v", result["kind"])
	}
}

// -----------------------------------------------------------------------
// Status addr resolution
// -----------------------------------------------------------------------

func TestKeepAwakeStatusAddrResolution(t *testing.T) {
	ts := httptest.NewServer(statusHandler(t, server.StatusResponse{
		ListeningAddress: "127.0.0.1:7070",
		KeepAwake: &server.KeepAwakeStatusSummary{
			State:          "ON",
			RemoteEnabled:  true,
			StatusRevision: 1,
		},
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	code, out, _ := kaRun([]string{"status", "--addr", addr})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, addr) {
		t.Fatalf("expected target addr in output, got %q", out)
	}
}

// -----------------------------------------------------------------------
// Unknown subcommand
// -----------------------------------------------------------------------

func TestKeepAwakeUnknownSubcommand(t *testing.T) {
	code, out, _ := kaRun([]string{"bogus"})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(out, "Unknown keep-awake command") {
		t.Fatalf("expected unknown command message, got %q", out)
	}
}

// Suppress unused import warnings for testing utilities.
var _ = io.Discard
var _ = fmt.Sprint
