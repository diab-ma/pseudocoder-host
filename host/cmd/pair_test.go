package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hostTLS "github.com/pseudocoder/host/internal/tls"
)

// ---------------------------------------------------------------------------
// A4: Recovery scenario classifier and guidance tests
// ---------------------------------------------------------------------------

func TestClassifyPairError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want pairRecoveryScenario
	}{
		// Stale socket
		{
			name: "stale socket sentinel",
			err:  errPairSocketUnavailable,
			want: pairScenarioStaleSocket,
		},
		{
			name: "stale socket wrapped",
			err:  fmt.Errorf("pairing socket at /tmp/pair.sock is not accepting connections (restart the host): %w", errPairSocketUnavailable),
			want: pairScenarioStaleSocket,
		},

		// Wrong-port positive tokens
		{
			name: "connection refused",
			err:  fmt.Errorf("could not connect to host: dial tcp: Connection refused"),
			want: pairScenarioWrongPort,
		},
		{
			name: "econnrefused",
			err:  fmt.Errorf("post https://...: ECONNREFUSED"),
			want: pairScenarioWrongPort,
		},
		{
			name: "errno = 61",
			err:  fmt.Errorf("dial tcp: errno = 61"),
			want: pairScenarioWrongPort,
		},
		{
			name: "errno = 111",
			err:  fmt.Errorf("dial tcp: errno = 111"),
			want: pairScenarioWrongPort,
		},

		// Unreachable-host positive tokens
		{
			name: "i/o timeout",
			err:  fmt.Errorf("dial tcp: i/o timeout"),
			want: pairScenarioUnreachableHost,
		},
		{
			name: "no such host",
			err:  fmt.Errorf("dial tcp: lookup badhost: no such host"),
			want: pairScenarioUnreachableHost,
		},
		{
			name: "network is unreachable",
			err:  fmt.Errorf("connect: network is unreachable"),
			want: pairScenarioUnreachableHost,
		},
		{
			name: "host is unreachable",
			err:  fmt.Errorf("connect: host is unreachable"),
			want: pairScenarioUnreachableHost,
		},

		// Negative: unreachable tokens should NOT map to wrong port
		{
			name: "i/o timeout is not wrong port",
			err:  fmt.Errorf("i/o timeout"),
			want: pairScenarioUnreachableHost,
		},

		// Precedence: mixed tokens resolve to wrong port
		{
			name: "mixed connection refused + i/o timeout -> wrong port wins",
			err:  fmt.Errorf("connection refused after i/o timeout"),
			want: pairScenarioWrongPort,
		},

		// Permission denied sentinel
		{
			name: "permission denied sentinel",
			err:  errPairSocketPermission,
			want: pairScenarioPermissionDenied,
		},
		{
			name: "permission denied wrapped",
			err:  fmt.Errorf("access error: %w", errPairSocketPermission),
			want: pairScenarioPermissionDenied,
		},

		// Non-socket path sentinel
		{
			name: "non-socket path sentinel",
			err:  errPairSocketNonSocket,
			want: pairScenarioNonSocketPath,
		},

		// Cert failure tokens
		{
			name: "cert failure - failed to load",
			err:  fmt.Errorf("failed to load host certificate: file not found"),
			want: pairScenarioCertFailure,
		},
		{
			name: "cert failure - certificate not found",
			err:  fmt.Errorf("certificate not found at /path/to/cert"),
			want: pairScenarioCertFailure,
		},

		// Structured host next_action bypass
		{
			name: "structured host guidance bypass",
			err:  fmt.Errorf("Code gen failed\nNext: Run pseudocoder doctor"),
			want: pairScenarioNone,
		},
		{
			name: "structured host guidance bypass even with wrong-port token",
			err:  fmt.Errorf("connection refused\nNext: Check host port"),
			want: pairScenarioNone,
		},

		// No match
		{
			name: "unrecognized error",
			err:  fmt.Errorf("something unexpected happened"),
			want: pairScenarioNone,
		},

		// Socket not found is NOT a recovery scenario (it falls back)
		{
			name: "socket not found is not classified",
			err:  errPairSocketNotFound,
			want: pairScenarioNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPairError(tt.err)
			if got != tt.want {
				t.Errorf("classifyPairError(%q) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestNormalizeErrorText(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"  Connection  Refused  ", "connection refused"},
		{"ECONNREFUSED", "econnrefused"},
		{"No Such Host", "no such host"},
		{"", ""},
		{"   multiple   spaces   here   ", "multiple spaces here"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeErrorText(tt.input)
			if got != tt.want {
				t.Errorf("normalizeErrorText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPrintRecoveryGuidance_StaleSocket(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioStaleSocket, "/tmp/pair.sock")
	output := buf.String()

	if !strings.Contains(output, "Stale Pairing Socket") {
		t.Error("output should contain 'Stale Pairing Socket'")
	}
	if !strings.Contains(output, "/tmp/pair.sock") {
		t.Error("output should contain the socket path")
	}
	if !strings.Contains(output, "1.") {
		t.Error("output should contain numbered steps")
	}
}

func TestPrintRecoveryGuidance_WrongPort(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioWrongPort, "")
	output := buf.String()

	if !strings.Contains(output, "Wrong Port") {
		t.Error("output should contain 'Wrong Port'")
	}
	if !strings.Contains(output, "pseudocoder host status") {
		t.Error("output should mention host status command")
	}
}

func TestPrintRecoveryGuidance_UnreachableHost(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioUnreachableHost, "")
	output := buf.String()

	if !strings.Contains(output, "Host Unreachable") {
		t.Error("output should contain 'Host Unreachable'")
	}
	if !strings.Contains(output, "pseudocoder doctor") {
		t.Error("output should mention doctor command")
	}
}

func TestPrintRecoveryGuidance_None(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioNone, "")
	if buf.Len() != 0 {
		t.Errorf("pairScenarioNone should produce no output, got: %q", buf.String())
	}
}

func TestRunPair_StaleSocketRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketUnavailable
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Stale Pairing Socket") {
		t.Errorf("stderr should contain recovery guidance, got: %s", stderr.String())
	}
	// Should NOT contain the generic fallback text
	if strings.Contains(stderr.String(), "The host must be running") {
		t.Error("stale socket should show specific guidance, not generic fallback")
	}
}

func TestRunPair_WrongPortRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("dial tcp 127.0.0.1:7070: connection refused")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Wrong Port") {
		t.Errorf("stderr should contain wrong port guidance, got: %s", stderr.String())
	}
}

func TestRunPair_UnreachableHostRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("dial tcp: i/o timeout")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Host Unreachable") {
		t.Errorf("stderr should contain host unreachable guidance, got: %s", stderr.String())
	}
}

// TestDisplayQRCode verifies that displayQRCode produces correct output format
// with QR code and plain-text fallback containing all required fields.
func TestDisplayQRCode(t *testing.T) {
	var buf bytes.Buffer
	code := "123456"
	expiry := time.Now().Add(2 * time.Minute)
	addr := "192.168.1.10:7070"
	fingerprint := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"

	DisplayQRCode(&buf, code, expiry, addr, fingerprint)

	output := buf.String()

	// Verify header
	if !strings.Contains(output, "SCAN TO PAIR") {
		t.Error("output should contain 'SCAN TO PAIR' header")
	}

	// Verify plain-text fallback section
	if !strings.Contains(output, "Plain-text fallback") {
		t.Error("output should contain 'Plain-text fallback' section")
	}

	// Verify code is formatted with spaces
	if !strings.Contains(output, "1 2 3 4 5 6") {
		t.Error("output should contain formatted code '1 2 3 4 5 6'")
	}

	// Verify host address is present
	if !strings.Contains(output, addr) {
		t.Errorf("output should contain host address %q", addr)
	}

	// Verify fingerprint is present
	if !strings.Contains(output, fingerprint) {
		t.Errorf("output should contain fingerprint %q", fingerprint)
	}

	// Verify expiry time format (HH:MM:SS)
	expiryStr := expiry.Format("15:04:05")
	if !strings.Contains(output, expiryStr) {
		t.Errorf("output should contain expiry time %q", expiryStr)
	}

	// Verify QR code block characters are present (half-block chars used by ToSmallString)
	// The QR code uses Unicode block characters like █ ▄ ▀
	if !strings.ContainsAny(output, "█▄▀") {
		t.Error("output should contain QR code block characters")
	}
}

// TestDisplayQRCodeEmptyFingerprint verifies behavior when fingerprint is empty
// (e.g., when using --no-tls mode).
func TestDisplayQRCodeEmptyFingerprint(t *testing.T) {
	var buf bytes.Buffer
	code := "654321"
	expiry := time.Now().Add(2 * time.Minute)
	addr := "127.0.0.1:7070"
	fingerprint := "" // Empty fingerprint for --no-tls mode

	DisplayQRCode(&buf, code, expiry, addr, fingerprint)

	output := buf.String()

	// Should still produce valid output
	if !strings.Contains(output, "SCAN TO PAIR") {
		t.Error("output should contain 'SCAN TO PAIR' header even with empty fingerprint")
	}

	// Code should still be present
	if !strings.Contains(output, "6 5 4 3 2 1") {
		t.Error("output should contain formatted code")
	}

	// Host should still be present
	if !strings.Contains(output, addr) {
		t.Error("output should contain host address")
	}
}

// TestQRPayloadRoundTrip verifies that the QR payload can be parsed back into
// the original field values. This ensures mobile clients can extract the data.
func TestQRPayloadRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		addr        string
		code        string
		fingerprint string
	}{
		{
			name:        "standard LAN address",
			addr:        "192.168.1.10:7070",
			code:        "123456",
			fingerprint: "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
		},
		{
			name:        "localhost",
			addr:        "127.0.0.1:7070",
			code:        "999999",
			fingerprint: "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00",
		},
		{
			name:        "empty fingerprint (no-tls mode)",
			addr:        "localhost:7070",
			code:        "000000",
			fingerprint: "",
		},
		{
			name:        "IPv6 localhost",
			addr:        "[::1]:7070",
			code:        "111111",
			fingerprint: "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build payload the same way displayQRCode does
			payload := buildQRPayload(tt.addr, tt.code, tt.fingerprint)

			// Parse the payload as a URL
			parsed, err := url.Parse(payload)
			if err != nil {
				t.Fatalf("failed to parse payload URL: %v", err)
			}

			// Verify scheme
			if parsed.Scheme != "pseudocoder" {
				t.Errorf("scheme = %q, want %q", parsed.Scheme, "pseudocoder")
			}

			// Verify path (opaque part after scheme)
			if parsed.Opaque != "//pair" && parsed.Host != "pair" {
				// URL parsing varies; just check the payload contains "pair"
				if !strings.Contains(payload, "://pair?") {
					t.Error("payload should contain '://pair?' path")
				}
			}

			// Extract query parameters
			query := parsed.Query()

			// Verify host param
			gotHost := query.Get("host")
			if gotHost != tt.addr {
				t.Errorf("host = %q, want %q", gotHost, tt.addr)
			}

			// Verify code param
			gotCode := query.Get("code")
			if gotCode != tt.code {
				t.Errorf("code = %q, want %q", gotCode, tt.code)
			}

			// Verify fingerprint param
			gotFP := query.Get("fp")
			if gotFP != tt.fingerprint {
				t.Errorf("fp = %q, want %q", gotFP, tt.fingerprint)
			}
		})
	}
}

// buildQRPayload constructs the QR payload URL (extracted for testing).
// This mirrors the logic in displayQRCode.
func buildQRPayload(addr, code, fingerprint string) string {
	return "pseudocoder://pair?host=" + url.QueryEscape(addr) +
		"&code=" + code +
		"&fp=" + url.QueryEscape(fingerprint)
}

// TestDisplayPairingCode verifies the non-QR display format.
func TestDisplayPairingCode(t *testing.T) {
	var buf bytes.Buffer
	code := "123456"
	expiry := time.Now().Add(2 * time.Minute)
	addr := "192.168.1.10:7070"

	DisplayPairingCode(&buf, code, expiry, addr)

	output := buf.String()

	// Verify header
	if !strings.Contains(output, "PAIRING CODE") {
		t.Error("output should contain 'PAIRING CODE' header")
	}

	// Verify code is formatted with spaces
	if !strings.Contains(output, "1 2 3 4 5 6") {
		t.Error("output should contain formatted code '1 2 3 4 5 6'")
	}

	// Verify host address
	if !strings.Contains(output, addr) {
		t.Errorf("output should contain host address %q", addr)
	}

	// Verify instructions
	if !strings.Contains(output, "Enter this code in the mobile app") {
		t.Error("output should contain pairing instructions")
	}

	// Verify no QR-specific content
	if strings.Contains(output, "SCAN TO PAIR") {
		t.Error("non-QR output should not contain 'SCAN TO PAIR'")
	}
}

// TestFormatCodeWithSpaces verifies code formatting.
func TestFormatCodeWithSpaces(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"123456", "1 2 3 4 5 6"},
		{"000000", "0 0 0 0 0 0"},
		{"1", "1"},
		{"12", "1 2"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := FormatCodeWithSpaces(tt.input)
			if got != tt.want {
				t.Errorf("formatCodeWithSpaces(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRunPair_InvalidPort(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--port", "0"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("runPair(--port 0) = %d, want 1", code)
	}
}

func TestRequestPairingCode_IPCFallback(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "123456", time.Unix(0, 0), nil
	}

	var stderr bytes.Buffer
	code, expiry, _, err := requestPairingCode(&PairConfig{Addrs: []string{"127.0.0.1:7070"}, NoTLS: true, PairSocket: "/tmp/missing.sock"}, &stderr)
	if err != nil {
		t.Fatalf("requestPairingCode() error: %v", err)
	}
	if code != "123456" {
		t.Fatalf("code = %q, want %q", code, "123456")
	}
	if expiry != time.Unix(0, 0) {
		t.Fatalf("expiry = %v, want %v", expiry, time.Unix(0, 0))
	}
	if !strings.Contains(stderr.String(), "falling back to localhost HTTP") {
		t.Fatalf("stderr missing fallback warning, got: %s", stderr.String())
	}
}

func TestRequestPairingCode_IPCPermissionDenied(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketPermission
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stderr bytes.Buffer
	_, _, _, err := requestPairingCode(&PairConfig{Addrs: []string{"127.0.0.1:7070"}, NoTLS: true, PairSocket: "/tmp/denied.sock"}, &stderr)
	if err == nil {
		t.Fatal("requestPairingCode() expected error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want permission denied", err)
	}
}

func TestRequestPairingCode_IPCUnavailable(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketUnavailable
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stderr bytes.Buffer
	_, _, _, err := requestPairingCode(&PairConfig{Addrs: []string{"127.0.0.1:7070"}, NoTLS: true, PairSocket: "/tmp/unavailable.sock"}, &stderr)
	if err == nil {
		t.Fatal("requestPairingCode() expected error")
	}
	if !strings.Contains(err.Error(), "not accepting connections") {
		t.Fatalf("error = %v, want not accepting connections", err)
	}
}

// TestParsePairingCodeResponse tests parsePairingCodeResponse for all code paths:
// structured non-200 with error_code+next_action, legacy 403, generic non-200, and success.
func TestParsePairingCodeResponse(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantCode    string
		wantErr     bool
		errContains string
	}{
		{
			name:       "success path",
			statusCode: 200,
			body:       `{"code":"123456","expiry":"2025-01-01T12:00:00Z"}`,
			wantCode:   "123456",
		},
		{
			name:        "structured non-200 with error_code and next_action",
			statusCode:  403,
			body:        `{"error":"forbidden","error_code":"auth.pair_generate_forbidden","message":"Only from localhost","next_action":"Run pseudocoder pair on host"}`,
			wantErr:     true,
			errContains: "Only from localhost",
		},
		{
			name:        "structured non-200 includes next_action in error",
			statusCode:  500,
			body:        `{"error":"internal_error","error_code":"auth.pair_generate_internal","message":"Code gen failed","next_action":"Run pseudocoder doctor"}`,
			wantErr:     true,
			errContains: "Next: Run pseudocoder doctor",
		},
		{
			name:        "legacy 403 fallback text",
			statusCode:  403,
			body:        `not json`,
			wantErr:     true,
			errContains: "restricted to localhost",
		},
		{
			name:        "generic non-200 fallback status",
			statusCode:  502,
			body:        `not json`,
			wantErr:     true,
			errContains: "status 502",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}

			code, _, err := parsePairingCodeResponse(resp)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.errContains)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// P6U2: IPC-first reordering tests
// ---------------------------------------------------------------------------

func TestRequestPairingCode_IPCSuccessNoCert(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "654321", time.Unix(1000, 0), nil
	}
	httpCalled := false
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		httpCalled = true
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stderr bytes.Buffer
	// NoTLS=false but with a cert path that doesn't exist — IPC success should still work.
	code, expiry, fingerprint, err := requestPairingCode(&PairConfig{
		Addrs:      []string{"127.0.0.1:7070"},
		NoTLS:      false,
		TLSCert:    "/tmp/nonexistent-cert.pem",
		PairSocket: "/tmp/test.sock",
	}, &stderr)
	if err != nil {
		t.Fatalf("requestPairingCode() error: %v", err)
	}
	if code != "654321" {
		t.Errorf("code = %q, want %q", code, "654321")
	}
	if expiry != time.Unix(1000, 0) {
		t.Errorf("expiry = %v, want %v", expiry, time.Unix(1000, 0))
	}
	// Fingerprint should be empty since cert doesn't exist
	if fingerprint != "" {
		t.Errorf("fingerprint = %q, want empty", fingerprint)
	}
	if httpCalled {
		t.Error("HTTP fallback should not have been called")
	}
}

func TestRequestPairingCode_ENOENTCertFailureHardStop(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	httpCalled := false
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		httpCalled = true
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stderr bytes.Buffer
	_, _, _, err := requestPairingCode(&PairConfig{
		Addrs:      []string{"127.0.0.1:7070"},
		NoTLS:      false,
		TLSCert:    "/tmp/nonexistent-cert-for-test.pem",
		PairSocket: "/tmp/missing.sock",
	}, &stderr)
	if err == nil {
		t.Fatal("expected cert failure error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to load host certificate") {
		t.Errorf("error = %v, want containing 'failed to load host certificate'", err)
	}
	if httpCalled {
		t.Error("HTTP fallback should not have been called after cert failure")
	}
}

func TestRequestPairingCode_NonSocketPathHardStop(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("%w: %s", errPairSocketNonSocket, socketPath)
	}
	httpCalled := false
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		httpCalled = true
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stderr bytes.Buffer
	_, _, _, err := requestPairingCode(&PairConfig{
		Addrs:      []string{"127.0.0.1:7070"},
		NoTLS:      true,
		PairSocket: "/tmp/not-a-socket",
	}, &stderr)
	if err == nil {
		t.Fatal("expected non-socket error, got nil")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("error = %v, want containing 'not a socket'", err)
	}
	if httpCalled {
		t.Error("HTTP fallback should not have been called for non-socket error")
	}
}

func TestRequestPairingCode_ENOENTFallbackSuccess(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "987654", time.Unix(2000, 0), nil
	}

	var stderr bytes.Buffer
	code, expiry, _, err := requestPairingCode(&PairConfig{
		Addrs:      []string{"127.0.0.1:7070"},
		NoTLS:      true,
		PairSocket: "/tmp/missing.sock",
	}, &stderr)
	if err != nil {
		t.Fatalf("requestPairingCode() error: %v", err)
	}
	if code != "987654" {
		t.Errorf("code = %q, want %q", code, "987654")
	}
	if expiry != time.Unix(2000, 0) {
		t.Errorf("expiry = %v, want %v", expiry, time.Unix(2000, 0))
	}
	if !strings.Contains(stderr.String(), "falling back to localhost HTTP") {
		t.Errorf("stderr missing fallback warning, got: %s", stderr.String())
	}
}

func TestRequestPairingCode_ENOENTTLSFallbackSuccess(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "host.crt")
	keyPath := filepath.Join(tmpDir, "host.key")
	certInfo, err := hostTLS.GenerateCertificate(hostTLS.CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
		Hosts:    []string{"127.0.0.1", "localhost"},
	})
	if err != nil {
		t.Fatalf("GenerateCertificate() error: %v", err)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected generated cert at %s: %v", certPath, err)
	}

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}

	httpCalled := false
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		httpCalled = true
		if noTLS {
			t.Fatal("expected TLS fallback, got noTLS=true")
		}
		if tlsConfig == nil {
			t.Fatal("expected TLS config for HTTPS fallback")
		}
		if tlsConfig.RootCAs == nil {
			t.Fatal("expected RootCAs to be populated")
		}
		return "987654", time.Unix(3000, 0), nil
	}

	var stderr bytes.Buffer
	code, expiry, fingerprint, err := requestPairingCode(&PairConfig{
		Addrs:      []string{"127.0.0.1:7070"},
		NoTLS:      false,
		TLSCert:    certPath,
		PairSocket: "/tmp/missing.sock",
	}, &stderr)
	if err != nil {
		t.Fatalf("requestPairingCode() error: %v", err)
	}
	if !httpCalled {
		t.Fatal("expected HTTPS fallback to be called once")
	}
	if code != "987654" {
		t.Errorf("code = %q, want %q", code, "987654")
	}
	if expiry != time.Unix(3000, 0) {
		t.Errorf("expiry = %v, want %v", expiry, time.Unix(3000, 0))
	}
	if fingerprint != certInfo.Fingerprint {
		t.Errorf("fingerprint = %q, want %q", fingerprint, certInfo.Fingerprint)
	}
	if !strings.Contains(stderr.String(), "falling back to localhost HTTP") {
		t.Errorf("stderr missing fallback warning, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Using certificate: "+certPath) {
		t.Errorf("stderr missing cert path, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Fingerprint: "+certInfo.Fingerprint) {
		t.Errorf("stderr missing fingerprint, got: %s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// P6U2: Recovery guidance tests for new scenarios
// ---------------------------------------------------------------------------

func TestPrintRecoveryGuidance_PermissionDenied(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioPermissionDenied, "/tmp/pair.sock")
	output := buf.String()

	if !strings.Contains(output, "Permission Denied") {
		t.Error("output should contain 'Permission Denied'")
	}
	if !strings.Contains(output, "/tmp/pair.sock") {
		t.Error("output should contain the socket path")
	}
}

func TestPrintRecoveryGuidance_NonSocketPath(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioNonSocketPath, "/tmp/pair.sock")
	output := buf.String()

	if !strings.Contains(output, "Non-Socket Path") {
		t.Error("output should contain 'Non-Socket Path'")
	}
	if !strings.Contains(output, "/tmp/pair.sock") {
		t.Error("output should contain the socket path")
	}
}

func TestPrintRecoveryGuidance_CertFailure(t *testing.T) {
	var buf bytes.Buffer
	printRecoveryGuidance(&buf, pairScenarioCertFailure, "")
	output := buf.String()

	if !strings.Contains(output, "Certificate Failure") {
		t.Error("output should contain 'Certificate Failure'")
	}
	if !strings.Contains(output, "--no-tls") {
		t.Error("output should mention --no-tls fallback")
	}
}

// ---------------------------------------------------------------------------
// P6U2: runPair integration tests for new recovery guidance
// ---------------------------------------------------------------------------

func TestRunPair_PermissionDeniedRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketPermission
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Permission Denied") {
		t.Errorf("stderr should contain 'Permission Denied', got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "The host must be running") {
		t.Error("permission denied should show specific guidance, not generic fallback")
	}
}

func TestRunPair_NonSocketPathRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("%w: %s", errPairSocketNonSocket, socketPath)
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Non-Socket Path") {
		t.Errorf("stderr should contain 'Non-Socket Path', got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "The host must be running") {
		t.Error("non-socket path should show specific guidance, not generic fallback")
	}
}

func TestRunPair_CertFailureRecoveryGuidance(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	// ENOENT from IPC, then cert failure on fallback path
	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("unexpected HTTP fallback")
	}

	var stdout, stderr bytes.Buffer
	// Use a cert path that doesn't exist — will fail on cert load before HTTP
	code := runPair([]string{"--tls-cert", "/tmp/nonexistent-p6u2-cert.pem"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Certificate Failure") {
		t.Errorf("stderr should contain 'Certificate Failure', got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "The host must be running") {
		t.Error("cert failure should show specific guidance, not generic fallback")
	}
}

func TestRunPair_StructuredHostGuidancePrecedence(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("Code gen failed\nNext: Run pseudocoder doctor")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	// Should NOT print generic guidance or any recovery scenario
	if strings.Contains(stderr.String(), "The host must be running") {
		t.Error("structured host guidance should suppress generic text")
	}
	if strings.Contains(stderr.String(), "Recovery:") {
		t.Error("structured host guidance should suppress local recovery guidance")
	}
}

func TestRunPair_GenericTextOnlyForUnclassifiedErrors(t *testing.T) {
	originalIPC := requestPairingCodeIPCFunc
	originalHTTP := requestPairingCodeHTTPFunc
	t.Cleanup(func() {
		requestPairingCodeIPCFunc = originalIPC
		requestPairingCodeHTTPFunc = originalHTTP
	})

	requestPairingCodeIPCFunc = func(socketPath string) (string, time.Time, error) {
		return "", time.Time{}, errPairSocketNotFound
	}
	requestPairingCodeHTTPFunc = func(addrs []string, noTLS bool, tlsConfig *tls.Config) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("something completely unknown")
	}

	var stdout, stderr bytes.Buffer
	code := runPair([]string{"--no-tls"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runPair() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "The host must be running") {
		t.Errorf("unclassified errors should show generic text, got: %s", stderr.String())
	}
}
