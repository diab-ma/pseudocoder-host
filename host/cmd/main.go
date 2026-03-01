package main

import (
	"fmt"
	"io"
	"os"
)

// Version is set at build time via -ldflags.
// Example: go build -ldflags="-X main.Version=v0.1.0-alpha.1" ./cmd
var Version = "dev"

const usage = `pseudocoder - mobile-first supervisory IDE for AI-driven development

Usage:
  pseudocoder <command> [options]

Commands:
  start         Start host with mobile-ready defaults (LAN + auth)
  host start    Start the host daemon (advanced)
  host status   Show host daemon status
  doctor        Diagnose onboarding readiness and connectivity
  pair          Generate a pairing code for mobile
  devices list  List paired devices
  devices revoke <device-id>  Revoke a device token
  session new          Create a new session
  session list         List all sessions
  session kill <id>    Kill a session
  session rename <id> <name>  Rename a session
  session list-tmux    List available tmux sessions
  session attach-tmux <name>  Attach to a tmux session
  session detach <id> [--kill]  Detach from a tmux session
  keep-awake enable-remote   Enable remote keep-awake policy
  keep-awake disable-remote  Disable remote keep-awake policy
  keep-awake status          Show keep-awake policy status
Run 'pseudocoder <command> --help' for more information on a command.
`

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch args[1] {
	case "start":
		return runStart(args[2:], stdout, stderr)
	case "host":
		if len(args) < 3 {
			fmt.Fprintln(stdout, "Usage: pseudocoder host <start|status>")
			return 1
		}
		switch args[2] {
		case "start":
			return runHostStart(args[3:], stdout, stderr)
		case "status":
			return runHostStatus(args[3:], stdout, stderr)
		default:
			fmt.Fprintf(stdout, "Unknown host command: %s\n", args[2])
			return 1
		}
	case "doctor":
		return runDoctor(args[2:], stdout, stderr)
	case "pair":
		return runPair(args[2:], stdout, stderr)
	case "devices":
		if len(args) < 3 {
			fmt.Fprintln(stdout, "Usage: pseudocoder devices <list|revoke>")
			return 1
		}
		switch args[2] {
		case "list":
			return runDevicesList(args[3:], stdout, stderr)
		case "revoke":
			return runDevicesRevoke(args[3:], stdout, stderr)
		default:
			fmt.Fprintf(stdout, "Unknown devices command: %s\n", args[2])
			return 1
		}
	case "session":
		return runSession(args[2:], stdout, stderr)
	case "keep-awake":
		return runKeepAwake(args[2:], stdout, stderr)
	case "--help", "-h", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "--version", "-v", "version":
		fmt.Fprintf(stdout, "pseudocoder %s\n", Version)
		return 0
	default:
		fmt.Fprintf(stdout, "Unknown command: %s\n", args[1])
		fmt.Fprint(stdout, usage)
		return 1
	}
}
