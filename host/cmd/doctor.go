// Package main provides CLI commands for the pseudocoder host.
// This file implements the `pseudocoder doctor` diagnostic command (Unit A2).
//
// The doctor command runs a sequence of preflight checks against the local
// host environment and reports actionable remediation guidance for any issues.
// It supports both human-readable (default) and machine-readable (--json) output.
package main

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/server"
)

// DoctorResult is the top-level JSON output for `pseudocoder doctor --json`.
type DoctorResult struct {
	// Version is the doctor output schema version. Always "1".
	Version string `json:"version"`

	// Checks is the ordered list of diagnostic checks that were evaluated.
	Checks []DoctorCheck `json:"checks"`

	// Summary contains aggregate pass/warn/fail counts derived from Checks.
	Summary DoctorSummary `json:"summary"`
}

// DoctorCheck is one diagnostic check in the doctor output.
type DoctorCheck struct {
	// ID is a stable, machine-readable identifier for the check (e.g., "trust.certificate").
	ID string `json:"id"`

	// Status is the check result: "pass", "warn", or "fail".
	Status string `json:"status"`

	// Message is a human-readable summary of what was found.
	Message string `json:"message"`

	// NextAction is a concrete remediation step the operator should take.
	NextAction string `json:"next_action"`
}

// DoctorSummary holds aggregate counts of check outcomes.
type DoctorSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// Stable check IDs used by the doctor command.
// These are part of the public CLI contract and must not change.
const (
	checkIDTrustCert    = "trust.certificate"
	checkIDNetReach     = "network.reachability"
	checkIDPairingIPC   = "pairing.ipc"
	checkIDHostReady    = "host.readiness"
)

// Stable status values for doctor checks.
const (
	statusPass = "pass"
	statusWarn = "warn"
	statusFail = "fail"
)

// Function-variable seams for testability.
// Tests override these to inject deterministic behavior without network or filesystem access.
var (
	// doctorQueryHostStatus probes a single address for host status.
	// Default implementation calls queryHostStatus from host.go.
	doctorQueryHostStatus = queryHostStatus

	// doctorLoadCertificate reads and parses a TLS certificate file.
	// Returns nil error on success. Used by the trust.certificate check.
	doctorLoadCertificate = defaultLoadCertificate

	// doctorProbeIPC attempts an IPC pairing request on the given socket path.
	// Returns nil on success or a typed error (errPairSocket*) on failure.
	doctorProbeIPC = defaultProbeIPC

	// doctorResolveAddrCandidates returns candidate addresses for host probing.
	doctorResolveAddrCandidates = resolveAddrCandidates
)

// defaultLoadCertificate reads the certificate file at path and verifies it can be parsed.
func defaultLoadCertificate(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Attempt to parse using the same logic as loadHostCertificate
	// but we only need to know if it parses, not the TLS config.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return fmt.Errorf("failed to parse certificate from %s", path)
	}
	return nil
}

// defaultProbeIPC attempts a pairing IPC request against the given socket path.
// It reuses the IPC function from pair.go to classify socket errors consistently.
func defaultProbeIPC(socketPath string) error {
	_, _, err := requestPairingCodeIPC(socketPath)
	if err == nil {
		return nil
	}
	// The IPC call may succeed (pairing code returned) or fail with a typed error.
	// For doctor, we only care about connectivity, not the actual code.
	// Socket-level errors (not found, permission, unavailable) propagate.
	// HTTP-level errors (e.g., 403 from non-localhost) mean the socket IS reachable.
	if errors.Is(err, errPairSocketNotFound) ||
		errors.Is(err, errPairSocketPermission) ||
		errors.Is(err, errPairSocketUnavailable) {
		return err
	}
	// Any other error (e.g., path is not a socket, unexpected stat failure)
	// must propagate so evalPairingIPC classifies it as fail.
	return err
}

// runDoctor implements the `pseudocoder doctor` CLI command.
// It evaluates preflight checks and reports results to stdout (human or JSON).
// Returns 0 when no checks fail, 1 when any check fails or an internal error occurs.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var jsonMode bool
	var addr string
	var port int
	var pairSocket string
	var tlsCert string

	fs.BoolVar(&jsonMode, "json", false, "Emit machine-readable JSON to stdout")
	fs.StringVar(&addr, "addr", "", "Host address override for readiness checks")
	fs.IntVar(&port, "port", 7070, "Port for auto-selected IPs (default 7070)")
	fs.StringVar(&pairSocket, "pair-socket", "", "Pairing IPC socket path override")
	fs.StringVar(&tlsCert, "tls-cert", "", "TLS certificate path override")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder doctor [options]\n\nDiagnose onboarding readiness and connectivity.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	// Track explicitly set flags for override detection.
	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	// Validate port range.
	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Resolve pairing socket path: flag override or default.
	if pairSocket == "" {
		defaultPath, err := config.DefaultPairSocketPath()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to determine pairing socket path: %v\n", err)
			return 1
		}
		pairSocket = defaultPath
	}

	// Resolve TLS certificate path: flag override or default.
	if tlsCert == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to get home directory: %v\n", err)
			return 1
		}
		tlsCert = filepath.Join(homeDir, ".pseudocoder", "certs", "host.crt")
	}

	// Shared probing context: probe host status once.
	addrs := doctorResolveAddrCandidates(addr, port, explicitFlags["port"], stderr)
	var statusResp *server.StatusResponse
	var statusErr error
	for _, target := range addrs {
		statusResp, statusErr = doctorQueryHostStatus(target)
		if statusErr == nil {
			break
		}
	}

	// Evaluate checks in deterministic order.
	checks := make([]DoctorCheck, 0, 4)
	checks = append(checks, evalTrustCertificate(statusResp, tlsCert))
	checks = append(checks, evalNetworkReachability(statusResp))
	checks = append(checks, evalPairingIPC(pairSocket))
	checks = append(checks, evalHostReadiness(statusResp))

	// Compute summary from check results.
	summary := DoctorSummary{}
	for _, c := range checks {
		switch c.Status {
		case statusPass:
			summary.Pass++
		case statusWarn:
			summary.Warn++
		case statusFail:
			summary.Fail++
		}
	}

	result := DoctorResult{
		Version: "1",
		Checks:  checks,
		Summary: summary,
	}

	// Render output.
	if jsonMode {
		if err := renderDoctorJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "Error: failed to encode JSON: %v\n", err)
			return 1
		}
	} else {
		renderDoctorHuman(stdout, result)
	}

	// Exit code: 0 when no failures, 1 when any failure.
	if summary.Fail > 0 {
		return 1
	}
	return 0
}

// evalTrustCertificate evaluates the trust.certificate check.
// Decision table:
//   - statusResponse != nil and TLSEnabled == false -> warn (TLS disabled)
//   - TLS required and cert loads+parses successfully -> pass
//   - TLS required and cert missing/unreadable/invalid -> fail
func evalTrustCertificate(status *server.StatusResponse, certPath string) DoctorCheck {
	check := DoctorCheck{ID: checkIDTrustCert}

	// If host responded and TLS is disabled, warn.
	if status != nil && !status.TLSEnabled {
		check.Status = statusWarn
		check.Message = "TLS is disabled on the host."
		check.NextAction = "Restart host without `--no-tls` for production pairing, or continue only for local dev."
		return check
	}

	// TLS is required: either host said TLS=true, or host is unreachable (secure default).
	err := doctorLoadCertificate(certPath)
	if err == nil {
		check.Status = statusPass
		check.Message = fmt.Sprintf("TLS certificate loaded from %s.", certPath)
		check.NextAction = "No action required."
		return check
	}

	check.Status = statusFail
	check.Message = fmt.Sprintf("TLS certificate error: %v", err)
	check.NextAction = "Provide a valid cert with `--tls-cert` or regenerate default host certs under `~/.pseudocoder/certs`."
	return check
}

// evalNetworkReachability evaluates the network.reachability check.
// Decision table:
//   - no status response -> fail
//   - status response with loopback listening address -> warn
//   - status response with non-loopback listening address -> pass
func evalNetworkReachability(status *server.StatusResponse) DoctorCheck {
	check := DoctorCheck{ID: checkIDNetReach}

	if status == nil {
		check.Status = statusFail
		check.Message = "Host is not reachable on any candidate address."
		check.NextAction = "Start host (`pseudocoder start` or `pseudocoder host start`) and verify address/port."
		return check
	}

	// Parse host from ListeningAddress to determine loopback vs non-loopback.
	host, _, err := net.SplitHostPort(status.ListeningAddress)
	if err != nil {
		check.Status = statusWarn
		check.Message = fmt.Sprintf("Could not parse listening address: %s", status.ListeningAddress)
		check.NextAction = "Check host bind address format and rerun doctor."
		return check
	}

	if isLoopback(host) {
		check.Status = statusWarn
		check.Message = fmt.Sprintf("Host is listening on loopback (%s).", status.ListeningAddress)
		check.NextAction = "Bind host to LAN/Tailscale address (for example `pseudocoder start --port 7070`) for mobile reachability."
		return check
	}

	check.Status = statusPass
	check.Message = fmt.Sprintf("Host is reachable at %s.", status.ListeningAddress)
	check.NextAction = "No action required."
	return check
}

// isLoopback returns true if the host string is a loopback address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// evalPairingIPC evaluates the pairing.ipc check.
// Decision table:
//   - IPC succeeds -> pass
//   - errPairSocketNotFound -> warn
//   - errPairSocketPermission -> fail
//   - errPairSocketUnavailable -> fail
//   - any other error -> fail
func evalPairingIPC(socketPath string) DoctorCheck {
	check := DoctorCheck{ID: checkIDPairingIPC}

	err := doctorProbeIPC(socketPath)
	if err == nil {
		check.Status = statusPass
		check.Message = fmt.Sprintf("IPC pairing socket is responsive at %s.", socketPath)
		check.NextAction = "No action required."
		return check
	}

	if errors.Is(err, errPairSocketNotFound) {
		check.Status = statusWarn
		check.Message = fmt.Sprintf("IPC pairing socket not found at %s.", socketPath)
		check.NextAction = "Start/update host IPC pairing socket or continue with loopback fallback for local-only usage."
		return check
	}

	if errors.Is(err, errPairSocketPermission) {
		check.Status = statusFail
		check.Message = fmt.Sprintf("Permission denied accessing IPC socket at %s.", socketPath)
		check.NextAction = "Run host and CLI as same user; fix socket permissions (`0600`) and parent dir (`0700`)."
		return check
	}

	if errors.Is(err, errPairSocketUnavailable) {
		check.Status = statusFail
		check.Message = fmt.Sprintf("IPC socket at %s is not accepting connections.", socketPath)
		check.NextAction = "Remove stale socket and restart host."
		return check
	}

	// Any other error.
	check.Status = statusFail
	check.Message = fmt.Sprintf("IPC pairing error: %v", err)
	check.NextAction = "Fix `--pair-socket` path and ensure it points to a live Unix socket."
	return check
}

// evalHostReadiness evaluates the host.readiness check.
// Decision table:
//   - no status response -> fail
//   - status response with RequireAuth == false -> warn
//   - status response with RequireAuth == true -> pass
func evalHostReadiness(status *server.StatusResponse) DoctorCheck {
	check := DoctorCheck{ID: checkIDHostReady}

	if status == nil {
		check.Status = statusFail
		check.Message = "Host is not reachable."
		check.NextAction = "Host is not reachable; start host and re-run doctor."
		return check
	}

	if !status.RequireAuth {
		check.Status = statusWarn
		check.Message = "Host is running without authentication."
		check.NextAction = "Restart host with authentication (`pseudocoder start` or `pseudocoder host start --require-auth`)."
		return check
	}

	check.Status = statusPass
	check.Message = "Host is running with authentication enabled."
	check.NextAction = "No action required."
	return check
}

// renderDoctorJSON writes the doctor result as JSON to stdout.
// Only valid JSON is written to stdout; no extra lines.
func renderDoctorJSON(w io.Writer, result DoctorResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// renderDoctorHuman writes the doctor result in human-readable format.
func renderDoctorHuman(w io.Writer, result DoctorResult) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Pseudocoder Doctor")
	fmt.Fprintln(w, "==================")
	fmt.Fprintln(w, "")

	for _, c := range result.Checks {
		icon := statusIcon(c.Status)
		fmt.Fprintf(w, "  %s %s: %s\n", icon, c.ID, c.Message)
		if c.Status != statusPass {
			fmt.Fprintf(w, "    -> %s\n", c.NextAction)
		}
	}

	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Summary: %d passed, %d warnings, %d failures\n",
		result.Summary.Pass, result.Summary.Warn, result.Summary.Fail)
	fmt.Fprintln(w, "")
}

// statusIcon returns a text marker for the check status.
func statusIcon(status string) string {
	switch status {
	case statusPass:
		return "[PASS]"
	case statusWarn:
		return "[WARN]"
	case statusFail:
		return "[FAIL]"
	default:
		return "[????]"
	}
}
