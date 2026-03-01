package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pseudocoder/host/internal/server"
)

// =============================================================================
// Helpers
// =============================================================================

// runDoctorWithArgs is a test helper that invokes runDoctor and captures output.
func runDoctorWithArgs(args []string) (exitCode int, stdout, stderr string) {
	var outBuf, errBuf bytes.Buffer
	code := runDoctor(args, &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

// stubDoctor overrides all function-variable seams with deterministic stubs.
// Returns a cleanup function that restores originals.
func stubDoctor(t *testing.T, opts stubOpts) {
	t.Helper()

	origQueryStatus := doctorQueryHostStatus
	origLoadCert := doctorLoadCertificate
	origProbeIPC := doctorProbeIPC
	origResolveAddr := doctorResolveAddrCandidates

	t.Cleanup(func() {
		doctorQueryHostStatus = origQueryStatus
		doctorLoadCertificate = origLoadCert
		doctorProbeIPC = origProbeIPC
		doctorResolveAddrCandidates = origResolveAddr
	})

	if opts.statusResp != nil || opts.statusErr != nil {
		doctorQueryHostStatus = func(addr string) (*server.StatusResponse, error) {
			return opts.statusResp, opts.statusErr
		}
	}
	if opts.certErr != nil || opts.certOK {
		doctorLoadCertificate = func(path string) error {
			return opts.certErr
		}
	}
	if opts.ipcErr != nil || opts.ipcOK {
		doctorProbeIPC = func(socketPath string) error {
			return opts.ipcErr
		}
	}
	// Always override address candidates for deterministic tests.
	doctorResolveAddrCandidates = func(addr string, port int, explicitPort bool, stderr io.Writer) []string {
		if addr != "" {
			return []string{addr}
		}
		return []string{"127.0.0.1:7070"}
	}
}

// stubOpts configures the behavior of stubbed seams for doctor tests.
type stubOpts struct {
	statusResp *server.StatusResponse
	statusErr  error
	certErr    error
	certOK     bool // when true, cert loads successfully (certErr == nil)
	ipcErr     error
	ipcOK      bool // when true, IPC succeeds (ipcErr == nil)
}

// =============================================================================
// AC1: Command is discoverable and routed from top-level CLI
// =============================================================================

func TestRunDoctor_Help(t *testing.T) {
	code, _, stderr := runDoctorWithArgs([]string{"--help"})
	if code != 0 {
		t.Fatalf("expected exit code 0 for --help, got %d", code)
	}
	if !strings.Contains(stderr, "Usage: pseudocoder doctor") {
		t.Fatalf("expected doctor usage, got %q", stderr)
	}
	if !strings.Contains(stderr, "-json") {
		t.Fatalf("expected -json flag in usage, got %q", stderr)
	}
}

// =============================================================================
// AC2: --json output follows the required minimum contract
// =============================================================================

func TestRunDoctorJSON_AllPass(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	code, stdout, _ := runDoctorWithArgs([]string{"--json"})
	if code != 0 {
		t.Fatalf("expected exit code 0 for all-pass, got %d", code)
	}

	var result DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, stdout)
	}

	// Version must be "1".
	if result.Version != "1" {
		t.Errorf("expected version %q, got %q", "1", result.Version)
	}

	// Must have exactly 4 checks.
	if len(result.Checks) != 4 {
		t.Fatalf("expected 4 checks, got %d", len(result.Checks))
	}

	// All checks must have required fields.
	for i, c := range result.Checks {
		if c.ID == "" {
			t.Errorf("check[%d]: missing id", i)
		}
		if c.Status == "" {
			t.Errorf("check[%d]: missing status", i)
		}
		if c.Message == "" {
			t.Errorf("check[%d]: missing message", i)
		}
		if c.NextAction == "" {
			t.Errorf("check[%d]: missing next_action", i)
		}
	}

	// Summary counts must match.
	if result.Summary.Pass != 4 {
		t.Errorf("expected 4 pass, got %d", result.Summary.Pass)
	}
	if result.Summary.Warn != 0 {
		t.Errorf("expected 0 warn, got %d", result.Summary.Warn)
	}
	if result.Summary.Fail != 0 {
		t.Errorf("expected 0 fail, got %d", result.Summary.Fail)
	}
}

func TestRunDoctorJSON_StdoutIsJSONOnly(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	_, stdout, _ := runDoctorWithArgs([]string{"--json"})

	// Stdout must be valid JSON and nothing else.
	trimmed := strings.TrimSpace(stdout)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Errorf("stdout should contain only JSON, got: %s", stdout)
	}
	var js json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &js); err != nil {
		t.Errorf("stdout is not valid JSON: %v", err)
	}
}

func TestRunDoctorJSON_WarningsToStderr(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	_, stdout, _ := runDoctorWithArgs([]string{"--json"})

	// No non-JSON content in stdout.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Every line should be part of valid JSON (starts with JSON chars).
		first := trimmed[0]
		if first != '{' && first != '}' && first != '"' && first != '[' && first != ']' &&
			!(first >= '0' && first <= '9') {
			t.Errorf("non-JSON line in stdout: %q", line)
		}
	}
}

// =============================================================================
// AC3: Stable check IDs and status values are enforced and deterministic
// =============================================================================

func TestRunDoctorJSON_CheckIDsAndOrder(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	_, stdout, _ := runDoctorWithArgs([]string{"--json"})

	var result DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	expectedIDs := []string{
		"trust.certificate",
		"network.reachability",
		"pairing.ipc",
		"host.readiness",
	}

	for i, expected := range expectedIDs {
		if result.Checks[i].ID != expected {
			t.Errorf("check[%d]: expected ID %q, got %q", i, expected, result.Checks[i].ID)
		}
	}
}

func TestRunDoctorJSON_StatusEnumConstrained(t *testing.T) {
	validStatuses := map[string]bool{"pass": true, "warn": true, "fail": true}

	tests := []struct {
		name string
		opts stubOpts
	}{
		{
			name: "all pass",
			opts: stubOpts{
				statusResp: &server.StatusResponse{
					ListeningAddress: "192.168.1.10:7070",
					TLSEnabled:       true,
					RequireAuth:      true,
				},
				certOK: true,
				ipcOK:  true,
			},
		},
		{
			name: "all warn",
			opts: stubOpts{
				statusResp: &server.StatusResponse{
					ListeningAddress: "127.0.0.1:7070",
					TLSEnabled:       false,
					RequireAuth:      false,
				},
				certOK: true,
				ipcErr: errPairSocketNotFound,
			},
		},
		{
			name: "all fail",
			opts: stubOpts{
				statusErr: errors.New("unreachable"),
				certErr:   errors.New("missing cert"),
				ipcErr:    errPairSocketPermission,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDoctor(t, tt.opts)
			_, stdout, _ := runDoctorWithArgs([]string{"--json"})

			var result DoctorResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}

			for i, c := range result.Checks {
				if !validStatuses[c.Status] {
					t.Errorf("check[%d] %s: invalid status %q", i, c.ID, c.Status)
				}
			}
		})
	}
}

// =============================================================================
// AC3: Decision matrix table tests for all four checks
// =============================================================================

func TestDoctorDecisionMatrix_TrustCertificate(t *testing.T) {
	tests := []struct {
		name       string
		status     *server.StatusResponse
		certErr    error
		wantStatus string
		wantAction string
	}{
		{
			name:       "TLS disabled on host -> warn",
			status:     &server.StatusResponse{TLSEnabled: false},
			wantStatus: "warn",
			wantAction: "Restart host without `--no-tls`",
		},
		{
			name:       "TLS required, cert loads OK -> pass",
			status:     &server.StatusResponse{TLSEnabled: true},
			certErr:    nil,
			wantStatus: "pass",
			wantAction: "No action required.",
		},
		{
			name:       "TLS required, cert missing -> fail",
			status:     &server.StatusResponse{TLSEnabled: true},
			certErr:    errors.New("file not found"),
			wantStatus: "fail",
			wantAction: "Provide a valid cert",
		},
		{
			name:       "host unreachable, cert loads OK -> pass (secure default)",
			status:     nil,
			certErr:    nil,
			wantStatus: "pass",
			wantAction: "No action required.",
		},
		{
			name:       "host unreachable, cert missing -> fail (secure default)",
			status:     nil,
			certErr:    errors.New("no cert"),
			wantStatus: "fail",
			wantAction: "Provide a valid cert",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origLoadCert := doctorLoadCertificate
			t.Cleanup(func() { doctorLoadCertificate = origLoadCert })
			doctorLoadCertificate = func(path string) error { return tt.certErr }

			check := evalTrustCertificate(tt.status, "/test/cert.crt")
			if check.ID != checkIDTrustCert {
				t.Errorf("expected ID %q, got %q", checkIDTrustCert, check.ID)
			}
			if check.Status != tt.wantStatus {
				t.Errorf("expected status %q, got %q", tt.wantStatus, check.Status)
			}
			if !strings.Contains(check.NextAction, tt.wantAction) {
				t.Errorf("expected next_action to contain %q, got %q", tt.wantAction, check.NextAction)
			}
		})
	}
}

func TestDoctorDecisionMatrix_NetworkReachability(t *testing.T) {
	tests := []struct {
		name       string
		status     *server.StatusResponse
		wantStatus string
		wantAction string
	}{
		{
			name:       "host unreachable -> fail",
			status:     nil,
			wantStatus: "fail",
			wantAction: "Start host",
		},
		{
			name:       "loopback 127.0.0.1 -> warn",
			status:     &server.StatusResponse{ListeningAddress: "127.0.0.1:7070"},
			wantStatus: "warn",
			wantAction: "Bind host to LAN/Tailscale",
		},
		{
			name:       "loopback localhost -> warn",
			status:     &server.StatusResponse{ListeningAddress: "localhost:7070"},
			wantStatus: "warn",
			wantAction: "Bind host to LAN/Tailscale",
		},
		{
			name:       "loopback ::1 -> warn",
			status:     &server.StatusResponse{ListeningAddress: "[::1]:7070"},
			wantStatus: "warn",
			wantAction: "Bind host to LAN/Tailscale",
		},
		{
			name:       "non-loopback LAN -> pass",
			status:     &server.StatusResponse{ListeningAddress: "192.168.1.10:7070"},
			wantStatus: "pass",
			wantAction: "No action required.",
		},
		{
			name:       "non-loopback Tailscale -> pass",
			status:     &server.StatusResponse{ListeningAddress: "100.64.1.5:7070"},
			wantStatus: "pass",
			wantAction: "No action required.",
		},
		{
			name:       "unparseable address -> warn",
			status:     &server.StatusResponse{ListeningAddress: "bad-address"},
			wantStatus: "warn",
			wantAction: "Check host bind address format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := evalNetworkReachability(tt.status)
			if check.ID != checkIDNetReach {
				t.Errorf("expected ID %q, got %q", checkIDNetReach, check.ID)
			}
			if check.Status != tt.wantStatus {
				t.Errorf("expected status %q, got %q", tt.wantStatus, check.Status)
			}
			if !strings.Contains(check.NextAction, tt.wantAction) {
				t.Errorf("expected next_action to contain %q, got %q", tt.wantAction, check.NextAction)
			}
		})
	}
}

func TestDoctorDecisionMatrix_PairingIPC(t *testing.T) {
	tests := []struct {
		name       string
		ipcErr     error
		wantStatus string
		wantAction string
	}{
		{
			name:       "IPC succeeds -> pass",
			ipcErr:     nil,
			wantStatus: "pass",
			wantAction: "No action required.",
		},
		{
			name:       "socket not found -> warn",
			ipcErr:     errPairSocketNotFound,
			wantStatus: "warn",
			wantAction: "Start/update host IPC pairing socket",
		},
		{
			name:       "socket permission denied -> fail",
			ipcErr:     errPairSocketPermission,
			wantStatus: "fail",
			wantAction: "Run host and CLI as same user",
		},
		{
			name:       "socket unavailable/stale -> fail",
			ipcErr:     errPairSocketUnavailable,
			wantStatus: "fail",
			wantAction: "Remove stale socket and restart host.",
		},
		{
			name:       "other IPC error -> fail",
			ipcErr:     errors.New("socket path is not a socket"),
			wantStatus: "fail",
			wantAction: "Fix `--pair-socket` path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origProbeIPC := doctorProbeIPC
			t.Cleanup(func() { doctorProbeIPC = origProbeIPC })
			doctorProbeIPC = func(socketPath string) error { return tt.ipcErr }

			check := evalPairingIPC("/test/pair.sock")
			if check.ID != checkIDPairingIPC {
				t.Errorf("expected ID %q, got %q", checkIDPairingIPC, check.ID)
			}
			if check.Status != tt.wantStatus {
				t.Errorf("expected status %q, got %q", tt.wantStatus, check.Status)
			}
			if !strings.Contains(check.NextAction, tt.wantAction) {
				t.Errorf("expected next_action to contain %q, got %q", tt.wantAction, check.NextAction)
			}
		})
	}
}

func TestDoctorDecisionMatrix_HostReadiness(t *testing.T) {
	tests := []struct {
		name       string
		status     *server.StatusResponse
		wantStatus string
		wantAction string
	}{
		{
			name:       "host unreachable -> fail",
			status:     nil,
			wantStatus: "fail",
			wantAction: "start host and re-run doctor",
		},
		{
			name:       "auth disabled -> warn",
			status:     &server.StatusResponse{RequireAuth: false},
			wantStatus: "warn",
			wantAction: "Restart host with authentication",
		},
		{
			name:       "auth enabled -> pass",
			status:     &server.StatusResponse{RequireAuth: true},
			wantStatus: "pass",
			wantAction: "No action required.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := evalHostReadiness(tt.status)
			if check.ID != checkIDHostReady {
				t.Errorf("expected ID %q, got %q", checkIDHostReady, check.ID)
			}
			if check.Status != tt.wantStatus {
				t.Errorf("expected status %q, got %q", tt.wantStatus, check.Status)
			}
			if !strings.Contains(check.NextAction, tt.wantAction) {
				t.Errorf("expected next_action to contain %q, got %q", tt.wantAction, check.NextAction)
			}
		})
	}
}

// =============================================================================
// AC5: Command exit code supports automation gating
// =============================================================================

func TestRunDoctorExitCodes_NoFails(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	code, _, _ := runDoctorWithArgs([]string{})
	if code != 0 {
		t.Fatalf("expected exit code 0 (no fails), got %d", code)
	}
}

func TestRunDoctorExitCodes_WarnsOnly(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "127.0.0.1:7070",
			TLSEnabled:       false,
			RequireAuth:      false,
		},
		certOK: true,
		ipcErr: errPairSocketNotFound,
	})

	code, _, _ := runDoctorWithArgs([]string{})
	if code != 0 {
		t.Fatalf("expected exit code 0 (warns only), got %d", code)
	}
}

func TestRunDoctorExitCodes_AtLeastOneFail(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusErr: errors.New("unreachable"),
		certErr:   errors.New("missing cert"),
		ipcErr:    errPairSocketPermission,
	})

	code, _, _ := runDoctorWithArgs([]string{})
	if code != 1 {
		t.Fatalf("expected exit code 1 (has fails), got %d", code)
	}
}

func TestRunDoctorExitCodes_JSONModeWithFails(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusErr: errors.New("unreachable"),
		certErr:   errors.New("missing cert"),
		ipcErr:    errPairSocketPermission,
	})

	code, stdout, _ := runDoctorWithArgs([]string{"--json"})
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}

	// JSON output must still be valid even when exiting with code 1.
	var result DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON on exit code 1: %v", err)
	}
	if result.Summary.Fail == 0 {
		t.Error("expected at least one fail in summary")
	}
}

// =============================================================================
// AC6: Flag precedence/default resolution is deterministic and test-locked
// =============================================================================

func TestRunDoctorFlags_InvalidPort(t *testing.T) {
	code, _, stderr := runDoctorWithArgs([]string{"--port", "0"})
	if code != 1 {
		t.Fatalf("expected exit code 1 for port 0, got %d", code)
	}
	if !strings.Contains(stderr, "port must be between") {
		t.Fatalf("expected port validation error, got %q", stderr)
	}
}

func TestRunDoctorFlags_InvalidPortNegative(t *testing.T) {
	code, _, stderr := runDoctorWithArgs([]string{"--port", "-1"})
	if code != 1 {
		t.Fatalf("expected exit code 1 for port -1, got %d", code)
	}
	if !strings.Contains(stderr, "port must be between") {
		t.Fatalf("expected port validation error, got %q", stderr)
	}
}

func TestRunDoctorFlags_InvalidPortTooHigh(t *testing.T) {
	code, _, stderr := runDoctorWithArgs([]string{"--port", "70000"})
	if code != 1 {
		t.Fatalf("expected exit code 1 for port 70000, got %d", code)
	}
	if !strings.Contains(stderr, "port must be between") {
		t.Fatalf("expected port validation error, got %q", stderr)
	}
}

func TestRunDoctorFlags_AddrOverridesPort(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "10.0.0.5:8080",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	// Override resolveAddrCandidates to capture what was passed.
	var capturedAddr string
	origResolve := doctorResolveAddrCandidates
	t.Cleanup(func() { doctorResolveAddrCandidates = origResolve })
	doctorResolveAddrCandidates = func(addr string, port int, explicitPort bool, stderr io.Writer) []string {
		capturedAddr = addr
		if addr != "" {
			return []string{addr}
		}
		return []string{"127.0.0.1:7070"}
	}

	runDoctorWithArgs([]string{"--addr", "10.0.0.5:8080", "--port", "9999"})
	if capturedAddr != "10.0.0.5:8080" {
		t.Errorf("expected addr to be passed to resolve, got %q", capturedAddr)
	}
}

func TestRunDoctorFlags_DefaultPairSocket(t *testing.T) {
	// When --pair-socket is not provided, it should resolve to DefaultPairSocketPath.
	var capturedSocketPath string
	origProbe := doctorProbeIPC
	origResolve := doctorResolveAddrCandidates
	origQuery := doctorQueryHostStatus
	origCert := doctorLoadCertificate
	t.Cleanup(func() {
		doctorProbeIPC = origProbe
		doctorResolveAddrCandidates = origResolve
		doctorQueryHostStatus = origQuery
		doctorLoadCertificate = origCert
	})
	doctorProbeIPC = func(socketPath string) error {
		capturedSocketPath = socketPath
		return nil
	}
	doctorResolveAddrCandidates = func(addr string, port int, explicitPort bool, stderr io.Writer) []string {
		return []string{"127.0.0.1:7070"}
	}
	doctorQueryHostStatus = func(addr string) (*server.StatusResponse, error) {
		return &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		}, nil
	}
	doctorLoadCertificate = func(path string) error { return nil }

	runDoctorWithArgs([]string{})
	if !strings.Contains(capturedSocketPath, ".pseudocoder") || !strings.Contains(capturedSocketPath, "pair.sock") {
		t.Errorf("expected default pair socket path, got %q", capturedSocketPath)
	}
}

func TestRunDoctorFlags_DefaultTLSCert(t *testing.T) {
	// When --tls-cert is not provided, it should resolve to ~/.pseudocoder/certs/host.crt.
	var capturedCertPath string
	origCert := doctorLoadCertificate
	origProbe := doctorProbeIPC
	origResolve := doctorResolveAddrCandidates
	origQuery := doctorQueryHostStatus
	t.Cleanup(func() {
		doctorLoadCertificate = origCert
		doctorProbeIPC = origProbe
		doctorResolveAddrCandidates = origResolve
		doctorQueryHostStatus = origQuery
	})
	doctorLoadCertificate = func(path string) error {
		capturedCertPath = path
		return nil
	}
	doctorProbeIPC = func(socketPath string) error { return nil }
	doctorResolveAddrCandidates = func(addr string, port int, explicitPort bool, stderr io.Writer) []string {
		return []string{"127.0.0.1:7070"}
	}
	doctorQueryHostStatus = func(addr string) (*server.StatusResponse, error) {
		return &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		}, nil
	}

	runDoctorWithArgs([]string{})
	if !strings.Contains(capturedCertPath, ".pseudocoder") || !strings.Contains(capturedCertPath, "host.crt") {
		t.Errorf("expected default cert path, got %q", capturedCertPath)
	}
}

// =============================================================================
// Regression: non-socket pair path must not pass (gate blocker fix)
// =============================================================================

func TestRunDoctor_NonSocketPairPath_Fails(t *testing.T) {
	// Create a regular file (not a Unix socket) to use as --pair-socket.
	tmpFile, err := os.CreateTemp(t.TempDir(), "not-a-socket-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	// Stub everything EXCEPT doctorProbeIPC so the real defaultProbeIPC runs.
	origQuery := doctorQueryHostStatus
	origCert := doctorLoadCertificate
	origResolve := doctorResolveAddrCandidates
	t.Cleanup(func() {
		doctorQueryHostStatus = origQuery
		doctorLoadCertificate = origCert
		doctorResolveAddrCandidates = origResolve
	})
	doctorQueryHostStatus = func(addr string) (*server.StatusResponse, error) {
		return &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		}, nil
	}
	doctorLoadCertificate = func(path string) error { return nil }
	doctorResolveAddrCandidates = func(addr string, port int, explicitPort bool, stderr io.Writer) []string {
		return []string{"127.0.0.1:7070"}
	}

	code, stdout, _ := runDoctorWithArgs([]string{
		"--json",
		"--pair-socket", tmpFile.Name(),
	})

	var result DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, stdout)
	}

	// Find the pairing.ipc check.
	var ipcCheck *DoctorCheck
	for i := range result.Checks {
		if result.Checks[i].ID == checkIDPairingIPC {
			ipcCheck = &result.Checks[i]
			break
		}
	}
	if ipcCheck == nil {
		t.Fatal("pairing.ipc check not found in output")
	}

	// Before the fix this was "pass"; after the fix it must be "fail".
	if ipcCheck.Status != statusFail {
		t.Errorf("expected pairing.ipc status %q for non-socket path, got %q (message: %s)",
			statusFail, ipcCheck.Status, ipcCheck.Message)
	}

	// Exit code must be 1 because there is at least one failure.
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestDefaultProbeIPC_NonSocketPath(t *testing.T) {
	// A regular file must produce a non-nil error from defaultProbeIPC.
	tmpFile, err := os.CreateTemp(t.TempDir(), "not-a-socket-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	err = defaultProbeIPC(tmpFile.Name())
	if err == nil {
		t.Fatal("expected error for non-socket path, got nil (pass)")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("expected 'not a socket' in error, got: %v", err)
	}
}

// =============================================================================
// AC4: Human-readable output renders correctly
// =============================================================================

func TestRunDoctor_HumanReadableOutput(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "192.168.1.10:7070",
			TLSEnabled:       true,
			RequireAuth:      true,
		},
		certOK: true,
		ipcOK:  true,
	})

	code, stdout, _ := runDoctorWithArgs([]string{})
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	// Check key elements of human output.
	if !strings.Contains(stdout, "Pseudocoder Doctor") {
		t.Error("expected header in human output")
	}
	if !strings.Contains(stdout, "[PASS]") {
		t.Error("expected [PASS] markers in human output")
	}
	if !strings.Contains(stdout, "trust.certificate") {
		t.Error("expected check ID in human output")
	}
	if !strings.Contains(stdout, "Summary:") {
		t.Error("expected summary in human output")
	}
	if !strings.Contains(stdout, "4 passed") {
		t.Error("expected pass count in summary")
	}
}

func TestRunDoctor_HumanReadableShowsWarnings(t *testing.T) {
	stubDoctor(t, stubOpts{
		statusResp: &server.StatusResponse{
			ListeningAddress: "127.0.0.1:7070",
			TLSEnabled:       false,
			RequireAuth:      false,
		},
		certOK: true,
		ipcErr: errPairSocketNotFound,
	})

	_, stdout, _ := runDoctorWithArgs([]string{})

	if !strings.Contains(stdout, "[WARN]") {
		t.Error("expected [WARN] markers")
	}
	if !strings.Contains(stdout, "->") {
		t.Error("expected next_action guidance for warnings")
	}
}

// =============================================================================
// Summary count verification
// =============================================================================

func TestRunDoctorJSON_SummaryCounts(t *testing.T) {
	tests := []struct {
		name     string
		opts     stubOpts
		wantPass int
		wantWarn int
		wantFail int
	}{
		{
			name: "all pass",
			opts: stubOpts{
				statusResp: &server.StatusResponse{
					ListeningAddress: "192.168.1.10:7070",
					TLSEnabled:       true,
					RequireAuth:      true,
				},
				certOK: true,
				ipcOK:  true,
			},
			wantPass: 4, wantWarn: 0, wantFail: 0,
		},
		{
			name: "mixed warn and pass",
			opts: stubOpts{
				statusResp: &server.StatusResponse{
					ListeningAddress: "127.0.0.1:7070",
					TLSEnabled:       false,
					RequireAuth:      false,
				},
				certOK: true,
				ipcErr: errPairSocketNotFound,
			},
			wantPass: 0, wantWarn: 4, wantFail: 0,
		},
		{
			name: "host unreachable",
			opts: stubOpts{
				statusErr: errors.New("unreachable"),
				certErr:   errors.New("missing cert"),
				ipcErr:    errPairSocketPermission,
			},
			wantPass: 0, wantWarn: 0, wantFail: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDoctor(t, tt.opts)
			_, stdout, _ := runDoctorWithArgs([]string{"--json"})

			var result DoctorResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}

			if result.Summary.Pass != tt.wantPass {
				t.Errorf("expected %d pass, got %d", tt.wantPass, result.Summary.Pass)
			}
			if result.Summary.Warn != tt.wantWarn {
				t.Errorf("expected %d warn, got %d", tt.wantWarn, result.Summary.Warn)
			}
			if result.Summary.Fail != tt.wantFail {
				t.Errorf("expected %d fail, got %d", tt.wantFail, result.Summary.Fail)
			}
		})
	}
}
