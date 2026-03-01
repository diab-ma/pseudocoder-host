package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/pseudocoder/host/internal/config"
	hostTLS "github.com/pseudocoder/host/internal/tls"
)

// PairConfig holds configuration for the pair command.
type PairConfig struct {
	Addr       string
	Addrs      []string
	TLSCert    string
	NoTLS      bool
	QR         bool // Display pairing info as QR code
	Port       int
	PairSocket string
}

var (
	errPairSocketNotFound    = errors.New("pairing socket not found")
	errPairSocketPermission  = errors.New("pairing socket permission denied")
	errPairSocketUnavailable = errors.New("pairing socket unavailable")
	errPairSocketNonSocket   = errors.New("pairing socket path is not a socket")
)

var (
	requestPairingCodeIPCFunc  = requestPairingCodeIPC
	requestPairingCodeHTTPFunc = requestPairingCodeHTTP
)

func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := &PairConfig{}
	fs.StringVar(&cfg.Addr, "addr", "", "Host address for QR code/display (default: Tailscale or LAN IP:7070)")
	fs.IntVar(&cfg.Port, "port", 7070, "Port for LAN access when auto-selecting IPs")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Path to host TLS certificate for verification (default: ~/.pseudocoder/certs/host.crt)")
	fs.BoolVar(&cfg.NoTLS, "no-tls", false, "Use HTTP instead of HTTPS (insecure, for development only)")
	fs.BoolVar(&cfg.QR, "qr", false, "Display pairing information as QR code")
	fs.StringVar(&cfg.PairSocket, "pair-socket", "", "Path to pairing IPC socket (default: ~/.pseudocoder/pair.sock)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder pair [options]\n\nGenerate a short pairing code for mobile device.\n\nOptions:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nThe pairing code is valid for 2 minutes and can only be used once.\n")
		fmt.Fprintf(stderr, "The mobile app enters this code at the /pair endpoint to get an access token.\n")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(cfg.Port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if cfg.PairSocket == "" {
		defaultPairSocket, err := config.DefaultPairSocketPath()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to determine pairing socket path: %v\n", err)
			return 1
		}
		cfg.PairSocket = defaultPairSocket
	}

	// Determine display address for QR code reachability from mobile.
	// Priority: Tailscale IP > LAN IP > localhost (with warning).
	// This is separate from the connection address (always localhost for code generation).
	portStr := fmt.Sprintf("%d", cfg.Port)
	displayAddr := cfg.Addr
	if displayAddr == "" {
		if ip := GetTailscaleIP(); ip != "" {
			displayAddr = ip + ":" + portStr
		} else if ip := GetPreferredOutboundIP(); ip != "" {
			displayAddr = ip + ":" + portStr
		} else {
			fmt.Fprintf(stderr, "Warning: could not detect network IP, using localhost\n")
			displayAddr = "127.0.0.1:" + portStr
		}
	} else if explicitFlags["port"] {
		fmt.Fprintf(stderr, "Warning: --addr overrides --port; using %s\n", displayAddr)
	}

	// Use local candidates for legacy HTTP fallback when IPC is unavailable.
	cfg.Addrs = resolveAddrCandidates("", cfg.Port, false, stderr)

	// Determine cert path default
	if cfg.TLSCert == "" && !cfg.NoTLS {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to get home directory: %v\n", err)
			return 1
		}
		cfg.TLSCert = filepath.Join(homeDir, ".pseudocoder", "certs", "host.crt")
	}

	// Request a code from the running host daemon
	code, expiry, fingerprint, err := requestPairingCode(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)

		// A4: Print scenario-specific recovery guidance when the error
		// matches a known failure pattern. Errors with structured host
		// guidance ("\nNext: ") bypass local classification entirely.
		scenario := classifyPairError(err)
		if scenario != pairScenarioNone {
			printRecoveryGuidance(stderr, scenario, cfg.PairSocket)
		} else if !strings.Contains(err.Error(), "\nNext: ") {
			fmt.Fprintf(stderr, "\nThe host must be running to generate a pairing code.\n")
			fmt.Fprintf(stderr, "Start it with: pseudocoder host start --require-auth\n")
		}
		return 1
	}

	// Display pairing information with LAN-reachable address, optionally as QR code
	if cfg.QR {
		DisplayQRCode(stdout, code, expiry, displayAddr, fingerprint)
	} else {
		DisplayPairingCode(stdout, code, expiry, displayAddr)
	}
	return 0
}

// requestPairingCode tries to get a pairing code from a running host daemon.
// IPC-first: attempts Unix socket first. On success, optionally loads cert for
// fingerprint display. On ENOENT, falls back to loopback HTTP with TLS.
// All other IPC errors are hard-stops (no fallback).
func requestPairingCode(cfg *PairConfig, stderr io.Writer) (code string, expiry time.Time, fingerprint string, err error) {
	code, expiry, err = requestPairingCodeIPCFunc(cfg.PairSocket)
	if err == nil {
		// IPC succeeded. Load cert for fingerprint display only (not required).
		if !cfg.NoTLS {
			_, fp, loadErr := loadHostCertificate(cfg.TLSCert)
			if loadErr == nil {
				fingerprint = fp
				fmt.Fprintf(stderr, "Using certificate: %s\n", cfg.TLSCert)
				fmt.Fprintf(stderr, "Fingerprint: %s\n", fingerprint)
			}
			// Missing cert after IPC success is OK — fingerprint stays empty.
		}
		return code, expiry, fingerprint, nil
	}

	// Non-ENOENT IPC errors are hard-stops — no cert load, no fallback.
	if errors.Is(err, errPairSocketPermission) {
		return "", time.Time{}, "", fmt.Errorf("permission denied accessing pairing socket %s (run as the host user or fix permissions): %w", cfg.PairSocket, errPairSocketPermission)
	}
	if errors.Is(err, errPairSocketUnavailable) {
		return "", time.Time{}, "", fmt.Errorf("pairing socket at %s is not accepting connections (restart the host): %w", cfg.PairSocket, errPairSocketUnavailable)
	}
	if errors.Is(err, errPairSocketNonSocket) {
		return "", time.Time{}, "", fmt.Errorf("pairing socket path is not a socket: %s (remove it and restart host): %w", cfg.PairSocket, errPairSocketNonSocket)
	}
	if !errors.Is(err, errPairSocketNotFound) {
		return "", time.Time{}, "", err
	}

	// ENOENT path: load cert before HTTP fallback.
	fmt.Fprintf(stderr, "Warning: pairing IPC socket not found at %s; falling back to localhost HTTP\n", cfg.PairSocket)

	var tlsConfig *tls.Config
	if !cfg.NoTLS {
		var loadErr error
		tlsConfig, fingerprint, loadErr = loadHostCertificate(cfg.TLSCert)
		if loadErr != nil {
			return "", time.Time{}, "", fmt.Errorf("failed to load host certificate: %w", loadErr)
		}
		fmt.Fprintf(stderr, "Using certificate: %s\n", cfg.TLSCert)
		fmt.Fprintf(stderr, "Fingerprint: %s\n", fingerprint)
	}

	code, expiry, err = requestPairingCodeHTTPFunc(cfg.Addrs, cfg.NoTLS, tlsConfig)
	if err != nil {
		return "", time.Time{}, "", err
	}

	return code, expiry, fingerprint, nil
}

func requestPairingCodeIPC(socketPath string) (code string, expiry time.Time, err error) {
	if socketPath == "" {
		return "", time.Time{}, errPairSocketNotFound
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", time.Time{}, errPairSocketNotFound
		}
		if os.IsPermission(err) {
			return "", time.Time{}, errPairSocketPermission
		}
		return "", time.Time{}, fmt.Errorf("failed to stat pairing socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", time.Time{}, fmt.Errorf("%w: %s", errPairSocketNonSocket, socketPath)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	resp, err := client.Post("http://unix/pair/generate", "application/json", nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return "", time.Time{}, errPairSocketNotFound
		}
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) {
			return "", time.Time{}, errPairSocketPermission
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return "", time.Time{}, errPairSocketUnavailable
		}
		return "", time.Time{}, fmt.Errorf("failed to connect to pairing socket: %w", err)
	}
	defer resp.Body.Close()

	code, expiry, err = parsePairingCodeResponse(resp)
	if err != nil {
		return "", time.Time{}, err
	}

	return code, expiry, nil
}

func requestPairingCodeHTTP(addrs []string, noTLS bool, tlsConfig *tls.Config) (code string, expiry time.Time, err error) {
	client := &http.Client{Timeout: 5 * time.Second}

	if !noTLS {
		if tlsConfig == nil {
			return "", time.Time{}, fmt.Errorf("missing TLS config for secure pairing request")
		}
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}

	var lastErr error
	for _, addr := range addrs {
		var reqURL string
		if noTLS {
			// Insecure mode: use plain HTTP (development only)
			reqURL = fmt.Sprintf("http://%s/pair/generate", addr)
		} else {
			// Secure mode: use HTTPS with certificate verification
			reqURL = fmt.Sprintf("https://%s/pair/generate", addr)
		}

		resp, err := client.Post(reqURL, "application/json", nil)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		code, expiry, err = parsePairingCodeResponse(resp)
		if err != nil {
			lastErr = err
			continue
		}

		return code, expiry, nil
	}

	if lastErr != nil {
		return "", time.Time{}, fmt.Errorf("could not connect to host: %w", lastErr)
	}

	return "", time.Time{}, fmt.Errorf("could not connect to host")
}

func parsePairingCodeResponse(resp *http.Response) (code string, expiry time.Time, err error) {
	if resp.StatusCode != http.StatusOK {
		// Try to parse structured error response with error_code and next_action.
		var errResp struct {
			Error      string `json:"error"`
			ErrorCode  string `json:"error_code"`
			Message    string `json:"message"`
			NextAction string `json:"next_action"`
		}
		if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr == nil && errResp.Message != "" {
			msg := errResp.Message
			if errResp.NextAction != "" {
				msg = fmt.Sprintf("%s\nNext: %s", msg, errResp.NextAction)
			}
			return "", time.Time{}, fmt.Errorf("%s", msg)
		}
		// Fallback for legacy hosts without structured error payloads.
		if resp.StatusCode == http.StatusForbidden {
			return "", time.Time{}, fmt.Errorf("pairing code generation is restricted to localhost")
		}
		return "", time.Time{}, fmt.Errorf("host returned status %d", resp.StatusCode)
	}

	var result struct {
		Code   string    `json:"code"`
		Expiry time.Time `json:"expiry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, err
	}

	return result.Code, result.Expiry, nil
}

// pairRecoveryScenario identifies the type of pairing failure for guided
// recovery output.
type pairRecoveryScenario int

const (
	pairScenarioNone pairRecoveryScenario = iota
	pairScenarioStaleSocket
	pairScenarioWrongPort
	pairScenarioUnreachableHost
	pairScenarioPermissionDenied
	pairScenarioNonSocketPath
	pairScenarioCertFailure
)

// classifyPairError determines the recovery scenario from an error returned
// by requestPairingCode. Structured host guidance ("\nNext: " in error)
// returns pairScenarioNone to preserve host-provided next_action. Then typed
// sentinels, cert failure tokens, and finally wrong-port/unreachable tokens.
func classifyPairError(err error) pairRecoveryScenario {
	errText := err.Error()

	// Structured host guidance takes precedence — preserve host next_action.
	if strings.Contains(errText, "\nNext: ") {
		return pairScenarioNone
	}

	// Typed sentinels.
	if errors.Is(err, errPairSocketUnavailable) {
		return pairScenarioStaleSocket
	}
	if errors.Is(err, errPairSocketPermission) {
		return pairScenarioPermissionDenied
	}
	if errors.Is(err, errPairSocketNonSocket) {
		return pairScenarioNonSocketPath
	}

	normalized := normalizeErrorText(errText)

	// Cert failure tokens.
	certTokens := []string{
		"failed to load host certificate",
		"certificate not found",
		"failed to read certificate",
		"failed to parse certificate",
		"failed to compute fingerprint",
	}
	for _, token := range certTokens {
		if strings.Contains(normalized, token) {
			return pairScenarioCertFailure
		}
	}

	// Wrong-port tokens (checked first per precedence rule).
	wrongPortTokens := []string{
		"connection refused",
		"econnrefused",
		"errno = 61",
		"errno = 111",
	}
	for _, token := range wrongPortTokens {
		if strings.Contains(normalized, token) {
			return pairScenarioWrongPort
		}
	}

	// Unreachable-host tokens.
	unreachableTokens := []string{
		"i/o timeout",
		"no such host",
		"network is unreachable",
		"host is unreachable",
	}
	for _, token := range unreachableTokens {
		if strings.Contains(normalized, token) {
			return pairScenarioUnreachableHost
		}
	}

	return pairScenarioNone
}

// normalizeErrorText trims whitespace, lowercases, and collapses repeated
// internal spaces. Shared normalization for the canonical wrong-port matcher.
func normalizeErrorText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	// Collapse repeated spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// printRecoveryGuidance prints scenario-specific recovery steps to stderr.
func printRecoveryGuidance(w io.Writer, scenario pairRecoveryScenario, socketPath string) {
	switch scenario {
	case pairScenarioStaleSocket:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Stale Pairing Socket")
		fmt.Fprintln(w, "  1. Check if the host process is running.")
		fmt.Fprintf(w, "  2. If the host is stopped, remove the stale socket: rm %s\n", socketPath)
		fmt.Fprintln(w, "  3. Restart the host and retry: pseudocoder host start --require-auth && pseudocoder pair")

	case pairScenarioWrongPort:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Wrong Port")
		fmt.Fprintln(w, "  1. Verify the active host bind address and port: pseudocoder host status")
		fmt.Fprintln(w, "  2. Retry with the correct port: pseudocoder pair --port <port>")

	case pairScenarioUnreachableHost:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Host Unreachable")
		fmt.Fprintln(w, "  1. Verify the host process is running and reachable on LAN or Tailscale.")
		fmt.Fprintln(w, "  2. Run diagnostics: pseudocoder doctor")
		fmt.Fprintln(w, "  3. Retry: pseudocoder pair")

	case pairScenarioPermissionDenied:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Permission Denied")
		fmt.Fprintf(w, "  1. Check socket permissions: ls -l %s\n", socketPath)
		fmt.Fprintln(w, "  2. Run as the host user or fix permissions on the socket file.")
		fmt.Fprintln(w, "  3. Retry: pseudocoder pair")

	case pairScenarioNonSocketPath:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Non-Socket Path")
		fmt.Fprintf(w, "  1. Remove the non-socket file: rm %s\n", socketPath)
		fmt.Fprintln(w, "  2. Restart the host: pseudocoder host start --require-auth")
		fmt.Fprintln(w, "  3. Retry: pseudocoder pair")

	case pairScenarioCertFailure:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Recovery: Certificate Failure")
		fmt.Fprintln(w, "  1. Regenerate certs: rm -rf ~/.pseudocoder/certs && pseudocoder host start --require-auth")
		fmt.Fprintln(w, "  2. Or use insecure mode: pseudocoder pair --no-tls")

	case pairScenarioNone:
		// No specific guidance for unclassified errors.
	}
}

// loadHostCertificate loads the host's TLS certificate and creates a TLS config
// that trusts only that certificate. Returns the TLS config and the certificate
// fingerprint for display.
func loadHostCertificate(certPath string) (*tls.Config, string, error) {
	// Check if certificate file exists
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		return nil, "", fmt.Errorf("certificate not found at %s (is the host running with TLS?)", certPath)
	}

	// Read the certificate file
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read certificate: %w", err)
	}

	// Create a certificate pool with just this certificate
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(certPEM) {
		return nil, "", fmt.Errorf("failed to parse certificate from %s", certPath)
	}

	// Compute the fingerprint for display
	fingerprint, err := hostTLS.ComputeFingerprintFromPEM(certPEM)
	if err != nil {
		return nil, "", fmt.Errorf("failed to compute fingerprint: %w", err)
	}

	// Create TLS config that trusts only the host certificate
	tlsConfig := &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}

	return tlsConfig, fingerprint, nil
}

// DisplayPairingCode shows the pairing code to the user.
func DisplayPairingCode(w io.Writer, code string, expiry time.Time, addr string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "         PAIRING CODE")
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "           %s\n", FormatCodeWithSpaces(code))
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  Expires: %s\n", expiry.Format("15:04:05"))
	fmt.Fprintf(w, "  Host:    %s\n", addr)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Enter this code in the mobile app to pair.")
	fmt.Fprintln(w, "  The code can only be used once.")
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "")
}

// DisplayQRCode shows pairing information as a QR code with plain-text fallback.
// The QR payload uses a URL scheme: pseudocoder://pair?host=<addr>&code=<code>&fp=<fingerprint>
func DisplayQRCode(w io.Writer, code string, expiry time.Time, addr, fingerprint string) {
	// Build the QR payload as a URL for easy mobile parsing.
	// Include host address, pairing code, and certificate fingerprint.
	payload := fmt.Sprintf("pseudocoder://pair?host=%s&code=%s&fp=%s",
		url.QueryEscape(addr),
		code,
		url.QueryEscape(fingerprint))

	// Generate QR code. Use Medium error correction for reasonable density.
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(w, "Error generating QR code: %v\n", err)
		fmt.Fprintf(w, "Falling back to text display.\n\n")
		DisplayPairingCode(w, code, expiry, addr)
		return
	}

	// Print QR code header
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "         SCAN TO PAIR")
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "")

	// Print QR code as ASCII art using half-block characters.
	// ToSmallString(false) produces compact output without a border.
	fmt.Fprint(w, qr.ToSmallString(false))

	// Print plain-text fallback with all pairing data
	fmt.Fprintln(w, "-------------------------------------------")
	fmt.Fprintln(w, "  Plain-text fallback:")
	fmt.Fprintf(w, "  Code:        %s\n", FormatCodeWithSpaces(code))
	fmt.Fprintf(w, "  Host:        %s\n", addr)
	fmt.Fprintf(w, "  Fingerprint: %s\n", fingerprint)
	fmt.Fprintf(w, "  Expires:     %s\n", expiry.Format("15:04:05"))
	fmt.Fprintln(w, "===========================================")
	fmt.Fprintln(w, "")
}

// FormatCodeWithSpaces adds spaces between digits for readability.
// "123456" -> "1 2 3 4 5 6"
func FormatCodeWithSpaces(code string) string {
	result := ""
	for i, c := range code {
		if i > 0 {
			result += " "
		}
		result += string(c)
	}
	return result
}

// GetPreferredOutboundIP returns the machine's preferred outbound IPv4 address.
// It works by dialing a UDP connection to a public IP (no actual traffic sent)
// and checking which local address was selected by the OS routing table.
// Returns empty string if detection fails.
func GetPreferredOutboundIP() string {
	// Dial UDP to a public IP. No actual packets are sent for UDP;
	// this just lets us query which local interface the OS would use.
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

// tailscaleNet is the CGNAT range used by Tailscale (100.64.0.0/10).
var tailscaleNet = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

// GetTailscaleIP scans network interfaces for a Tailscale IP address.
// Tailscale uses the 100.64.0.0/10 CGNAT range for its addresses.
// Returns empty string if no Tailscale IP is found.
func GetTailscaleIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			// Check if this is an IPv4 address in the Tailscale range
			ip := ipNet.IP.To4()
			if ip != nil && tailscaleNet.Contains(ip) {
				return ip.String()
			}
		}
	}

	return ""
}
