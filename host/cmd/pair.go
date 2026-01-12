package main

import (
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
	"time"

	"github.com/skip2/go-qrcode"

	hostTLS "github.com/pseudocoder/host/internal/tls"
)

// PairConfig holds configuration for the pair command.
type PairConfig struct {
	Addr    string
	TLSCert string
	NoTLS   bool
	QR      bool // Display pairing info as QR code
}

func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := &PairConfig{}
	fs.StringVar(&cfg.Addr, "addr", "", "Host address for QR code/display (default: Tailscale or LAN IP:7070)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Path to host TLS certificate for verification (default: ~/.pseudocoder/certs/host.crt)")
	fs.BoolVar(&cfg.NoTLS, "no-tls", false, "Use HTTP instead of HTTPS (insecure, for development only)")
	fs.BoolVar(&cfg.QR, "qr", false, "Display pairing information as QR code")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder pair [options]\n\nGenerate a short pairing code for mobile device.\n\nOptions:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nThe pairing code is valid for 5 minutes and can only be used once.\n")
		fmt.Fprintf(stderr, "The mobile app enters this code at the /pair endpoint to get an access token.\n")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	// Determine display address for QR code reachability from mobile.
	// Priority: Tailscale IP > LAN IP > localhost (with warning).
	// This is separate from the connection address (always localhost for code generation).
	displayAddr := cfg.Addr
	if displayAddr == "" {
		if ip := GetTailscaleIP(); ip != "" {
			displayAddr = ip + ":7070"
		} else if ip := GetPreferredOutboundIP(); ip != "" {
			displayAddr = ip + ":7070"
		} else {
			fmt.Fprintf(stderr, "Warning: could not detect network IP, using localhost\n")
			displayAddr = "127.0.0.1:7070"
		}
	}

	// Always connect to localhost for code generation (host restricts /pair/generate to localhost).
	cfg.Addr = "127.0.0.1:7070"

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
		fmt.Fprintf(stderr, "\nThe host must be running to generate a pairing code.\n")
		fmt.Fprintf(stderr, "Start it with: pseudocoder host start --require-auth\n")
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
// It uses HTTPS by default and verifies the host certificate.
// Returns the pairing code, expiry time, and certificate fingerprint (empty if no TLS).
func requestPairingCode(cfg *PairConfig, stderr io.Writer) (code string, expiry time.Time, fingerprint string, err error) {
	var reqURL string
	var client *http.Client

	if cfg.NoTLS {
		// Insecure mode: use plain HTTP (development only)
		reqURL = fmt.Sprintf("http://%s/pair/generate", cfg.Addr)
		client = &http.Client{Timeout: 5 * time.Second}
		fingerprint = "" // No fingerprint in insecure mode
	} else {
		// Secure mode: use HTTPS with certificate verification
		reqURL = fmt.Sprintf("https://%s/pair/generate", cfg.Addr)

		// Load and verify the host certificate
		tlsConfig, fp, loadErr := loadHostCertificate(cfg.TLSCert)
		if loadErr != nil {
			return "", time.Time{}, "", fmt.Errorf("failed to load host certificate: %w", loadErr)
		}
		fingerprint = fp

		fmt.Fprintf(stderr, "Using certificate: %s\n", cfg.TLSCert)
		fmt.Fprintf(stderr, "Fingerprint: %s\n", fingerprint)

		client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		}
	}

	resp, err := client.Post(reqURL, "application/json", nil)
	if err != nil {
		return "", time.Time{}, "", fmt.Errorf("could not connect to host at %s: %w", cfg.Addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", time.Time{}, "", fmt.Errorf("pairing code generation is restricted to localhost")
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, "", fmt.Errorf("host returned status %d", resp.StatusCode)
	}

	var result struct {
		Code   string    `json:"code"`
		Expiry time.Time `json:"expiry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, "", err
	}

	return result.Code, result.Expiry, fingerprint, nil
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
