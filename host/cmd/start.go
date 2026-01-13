package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pseudocoder/host/internal/config"
)

// runStart implements the "pseudocoder start" command.
// This is a convenience wrapper for mobile-first workflows that:
//  1. Creates ~/.pseudocoder/config.toml with mobile-ready defaults if missing
//  2. Starts the host with LAN-ready settings (0.0.0.0:7070, require-auth)
//  3. Prints a connection summary for mobile pairing
//
// The command differs from "host start" in its defaults:
//   - "start": addr=0.0.0.0:7070, require_auth=true (mobile-ready)
//   - "host start": addr=127.0.0.1:7070, require_auth=false (local dev)
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repo := fs.String("repo", "", "Path to repository to supervise (default: current directory)")
	mdns := fs.Bool("mdns", false, "Enable mDNS/Bonjour discovery (LAN-visible)")
	pair := fs.Bool("pair", false, "Generate and display pairing code during startup")
	qr := fs.Bool("qr", false, "Display pairing code as QR code (requires --pair)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: pseudocoder start [options]

Start the host with mobile-ready defaults (LAN access + authentication).

This command is a convenience wrapper that:
  1. Creates ~/.pseudocoder/config.toml with mobile-ready settings if missing
  2. Starts the host listening on all interfaces (0.0.0.0:7070)
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

	// Print mobile connection summary
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "===========================================")
	fmt.Fprintln(stdout, "  Mobile-Ready Host")
	fmt.Fprintln(stdout, "===========================================")
	fmt.Fprintln(stdout, "  Address:  0.0.0.0:7070 (all interfaces)")
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
		"--addr", "0.0.0.0:7070",
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

	// Call the main host start function
	return runHostStart(hostArgs, stdout, stderr)
}
