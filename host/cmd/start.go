package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pseudocoder/host/internal/config"
)

// runStart implements the "pseudocoder start" command.
// This is a convenience wrapper for mobile-first workflows that:
//  1. Creates ~/.pseudocoder/config.toml with mobile-ready defaults if missing
//  2. Starts the host with auth required and a safe auto-selected bind address
//  3. Prints a connection summary for mobile pairing
//
// The command differs from "host start" in its defaults:
//   - "start": auto-select bind address, require_auth=true (mobile-ready)
//   - "host start": addr=127.0.0.1:7070, require_auth=false (local dev)
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repo := fs.String("repo", "", "Path to repository to supervise (default: current directory)")
	addr := fs.String("addr", "", "Host address to bind (default: auto-select Tailscale/LAN/localhost)")
	port := fs.Int("port", 7070, "Port for auto-selected bind addresses")
	mdns := fs.Bool("mdns", false, "Enable mDNS/Bonjour discovery (LAN-visible)")
	pair := fs.Bool("pair", false, "Generate and display pairing code during startup")
	qr := fs.Bool("qr", false, "Display pairing code as QR code (requires --pair)")
	pairSocket := fs.String("pair-socket", "", "Path to pairing IPC socket (default: ~/.pseudocoder/pair.sock)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: pseudocoder start [options]

Start the host with mobile-ready defaults (auto-selected bind + authentication).

This command is a convenience wrapper that:
  1. Creates ~/.pseudocoder/config.toml with mobile-ready settings if missing
  2. Starts the host listening on a safe auto-selected address
  3. Enables authentication by default for security

Use 'pseudocoder host start' for more control over configuration.

Options:
`)
		fs.PrintDefaults()
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

	if err := validatePort(*port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Determine repository path
	repoPath := *repo
	if repoPath == "" {
		var err error
		repoPath, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to get current directory: %v\n", err)
			return 1
		}
	}

	// Get default config path
	configPath, err := config.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to determine config path: %v\n", err)
		return 1
	}

	// Check if config exists before attempting to create
	configCreated := false
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Create config with mobile-ready defaults
		if err := config.WriteDefault(configPath, repoPath); err != nil {
			fmt.Fprintf(stderr, "Error: failed to create config file: %v\n", err)
			return 1
		}
		configCreated = true
		fmt.Fprintf(stdout, "Created config: %s\n", configPath)
	}

	bindAddr := selectBindAddr(*addr, *port, explicitFlags["port"], stderr)

	// Print mobile connection summary
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "===========================================")
	fmt.Fprintln(stdout, "  Mobile-Ready Host")
	fmt.Fprintln(stdout, "===========================================")
	if strings.HasPrefix(bindAddr, "0.0.0.0:") {
		fmt.Fprintf(stdout, "  Address:  %s (all interfaces)\n", bindAddr)
	} else {
		fmt.Fprintf(stdout, "  Address:  %s\n", bindAddr)
	}
	fmt.Fprintln(stdout, "  Auth:     Required")
	fmt.Fprintln(stdout, "  Pairing:  Run 'pseudocoder pair' to connect")
	fmt.Fprintln(stdout, "===========================================")
	fmt.Fprintln(stdout, "")

	// Build args for runHostStart
	// We explicitly pass the flags to ensure mobile-ready defaults even if
	// the config file was modified by the user. The key difference from
	// "host start" is that we set addr and require-auth explicitly.
	//
	// This ensures the banner output (LAN + auth) always matches reality.
	hostArgs := []string{
		"--addr", bindAddr,
		"--require-auth",
	}

	if !configCreated {
		// Config existed - note that we're overriding with mobile-ready defaults
		fmt.Fprintf(stdout, "Using config: %s (with mobile-ready overrides)\n", configPath)
		fmt.Fprintln(stdout, "")
	}

	// Pass --repo if specified (overrides config file)
	if *repo != "" {
		hostArgs = append(hostArgs, "--repo", *repo)
	}

	// Pass --mdns if specified
	if *mdns {
		hostArgs = append(hostArgs, "--mdns")
	}

	// Pass --pair if specified
	if *pair {
		hostArgs = append(hostArgs, "--pair")
	}

	// Pass --qr if specified
	if *qr {
		hostArgs = append(hostArgs, "--qr")
	}

	// Pass --pair-socket if specified
	if *pairSocket != "" {
		hostArgs = append(hostArgs, "--pair-socket", *pairSocket)
	}

	// Call the main host start function
	return runHostStart(hostArgs, stdout, stderr)
}

func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}
	return nil
}

var (
	getTailscaleIP        = GetTailscaleIP
	getPreferredOutboundIP = GetPreferredOutboundIP
)

func selectBindAddr(addr string, port int, explicitPort bool, stderr io.Writer) string {
	if addr != "" {
		if explicitPort {
			fmt.Fprintf(stderr, "Warning: --addr overrides --port; using %s\n", addr)
		}
		return addr
	}

	portStr := fmt.Sprintf("%d", port)
	if ip := getTailscaleIP(); ip != "" {
		return ip + ":" + portStr
	}
	if ip := getPreferredOutboundIP(); ip != "" {
		return ip + ":" + portStr
	}
	fmt.Fprintf(stderr, "Warning: could not detect network IP, using localhost\n")
	return "127.0.0.1:" + portStr
}
