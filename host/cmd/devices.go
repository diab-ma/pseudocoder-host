package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// DevicesConfig holds the configuration for device management commands.
type DevicesConfig struct {
	TokenStore string
	Addr       string // Host address for notifying running host of revocation
}

// getDefaultTokenStorePath returns the default path to the token/device store.
func getDefaultTokenStorePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pseudocoder", "pseudocoder.db"), nil
}

// formatDuration formats a duration in a human-readable way.
// Examples: "just now", "5m ago", "2h ago", "3d ago", "never"
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "in the future"
	}
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func runDevicesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devices list", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := &DevicesConfig{}
	fs.StringVar(&cfg.TokenStore, "token-store", "", "Path to token/device store (default: ~/.pseudocoder/pseudocoder.db)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder devices list [options]\n\nList all paired devices.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	// Determine the token store path
	tokenStorePath := cfg.TokenStore
	if tokenStorePath == "" {
		var err error
		tokenStorePath, err = getDefaultTokenStorePath()
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
	}

	// Check if the database file exists
	if _, err := os.Stat(tokenStorePath); os.IsNotExist(err) {
		fmt.Fprintln(stdout, "No paired devices found.")
		return 0
	}

	// Open storage
	store, err := storage.NewSQLiteStore(tokenStorePath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to open storage: %v\n", err)
		return 1
	}
	defer store.Close()

	// List devices
	devices, err := store.ListDevices()
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to list devices: %v\n", err)
		return 1
	}

	if len(devices) == 0 {
		fmt.Fprintln(stdout, "No paired devices found.")
		return 0
	}

	// Print devices in a table format
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE ID\tNAME\tCREATED\tLAST SEEN")
	fmt.Fprintln(w, "---------\t----\t-------\t---------")

	now := time.Now()
	for _, device := range devices {
		createdAgo := formatDuration(now.Sub(device.CreatedAt))
		lastSeenAgo := formatDuration(now.Sub(device.LastSeen))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			device.ID,
			device.Name,
			createdAgo,
			lastSeenAgo,
		)
	}
	w.Flush()

	return 0
}

func runDevicesRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devices revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := &DevicesConfig{}
	var port int
	fs.StringVar(&cfg.TokenStore, "token-store", "", "Path to token/device store (default: ~/.pseudocoder/pseudocoder.db)")
	fs.StringVar(&cfg.Addr, "addr", "", "Host address to notify (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder devices revoke [options] <device-id>\n\nRevoke a device token and disconnect any active sessions.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Error: device-id is required")
		fs.Usage()
		return 1
	}

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	deviceID := fs.Arg(0)

	// Determine the token store path
	tokenStorePath := cfg.TokenStore
	if tokenStorePath == "" {
		var err error
		tokenStorePath, err = getDefaultTokenStorePath()
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
	}

	// Check if the database file exists
	if _, err := os.Stat(tokenStorePath); os.IsNotExist(err) {
		fmt.Fprintf(stderr, "Error: device %s not found\n", deviceID)
		return 1
	}

	// Open storage
	store, err := storage.NewSQLiteStore(tokenStorePath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to open storage: %v\n", err)
		return 1
	}
	defer store.Close()

	// Check if device exists before attempting to revoke
	device, err := store.GetDevice(deviceID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to lookup device: %v\n", err)
		return 1
	}
	if device == nil {
		fmt.Fprintf(stderr, "Error: device %s not found\n", deviceID)
		return 1
	}

	addrs := resolveAddrCandidates(cfg.Addr, port, explicitFlags["port"], stderr)

	// Try to notify the running host first. The host endpoint will:
	// 1. Close active connections for this device
	// 2. Delete the device from storage
	// This order ensures connections are closed before the device is removed.
	closedCount, hostHandled := notifyHostRevocation(deviceID, addrs)
	if hostHandled {
		// Host handled the revocation (closed connections + deleted from DB)
		fmt.Fprintf(stdout, "Revoked device: %s (%s)\n", device.ID, device.Name)
		fmt.Fprintf(stdout, "Closed %d active connection(s).\n", closedCount)
		return 0
	}

	// Host not reachable - delete from storage directly.
	// Any active connections will be rejected on next auth check.
	if err := store.DeleteDevice(deviceID); err != nil {
		fmt.Fprintf(stderr, "Error: failed to revoke device: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Revoked device: %s (%s)\n", device.ID, device.Name)
	fmt.Fprintln(stdout, "Note: Host is not running or unreachable. The device has been revoked and will be disconnected if it tries to reconnect.")

	return 0
}

// notifyHostRevocation calls the running host's HTTP endpoint to close active connections
// and delete the device. Returns (connections_closed, true) if the host handled the request,
// or (0, false) if the host is unreachable.
// This tries HTTPS first (default), then falls back to HTTP for --no-tls hosts.
func notifyHostRevocation(deviceID string, addrs []string) (int, bool) {
	// Create HTTP client with short timeout and TLS config that accepts self-signed certs.
	// We use InsecureSkipVerify because:
	// 1. This is localhost-only communication (enforced by host endpoint)
	// 2. The host uses a self-signed certificate by default
	// 3. We're not transmitting sensitive data (just a device ID to revoke)
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	// Try HTTPS first (default), then HTTP (for --no-tls hosts)
	schemes := []string{"https", "http"}

	for _, addr := range addrs {
		for _, scheme := range schemes {
			url := fmt.Sprintf("%s://%s/devices/%s/revoke", scheme, addr, deviceID)

			req, err := http.NewRequest(http.MethodPost, url, nil)
			if err != nil {
				continue
			}

			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				var result struct {
					DeviceID          string `json:"device_id"`
					DeviceName        string `json:"device_name"`
					ConnectionsClosed int    `json:"connections_closed"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
					return result.ConnectionsClosed, true
				}
			}

			// Got a response but not success - keep trying other candidates.
			continue
		}
	}

	// Host unreachable
	return 0, false
}
