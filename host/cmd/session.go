// Package main provides CLI commands for the pseudocoder host.
// This file implements session management commands (Unit 9.5).
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
)

// SessionConfig holds the configuration for session management commands.
type SessionConfig struct {
	Addr string // Host address (e.g., "127.0.0.1:7070")
}

func resolveSessionAddrs(addr string, port int, explicitFlags map[string]bool, stderr io.Writer) []string {
	return resolveAddrCandidates(addr, port, explicitFlags["port"], stderr)
}

// runSession handles the "pseudocoder session" subcommand.
// It routes to the appropriate subcommand (new, list, kill, rename, list-tmux, attach-tmux, detach).
func runSession(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "Usage: pseudocoder session <command>")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Commands:")
		fmt.Fprintln(stdout, "  new          Create a new session")
		fmt.Fprintln(stdout, "  list         List all sessions")
		fmt.Fprintln(stdout, "  kill         Kill a session")
		fmt.Fprintln(stdout, "  rename       Rename a session")
		fmt.Fprintln(stdout, "  list-tmux    List available tmux sessions")
		fmt.Fprintln(stdout, "  attach-tmux  Attach to a tmux session")
		fmt.Fprintln(stdout, "  detach       Detach from a tmux session")
		return 1
	}

	switch args[0] {
	case "new":
		return runSessionNew(args[1:], stdout, stderr)
	case "list":
		return runSessionList(args[1:], stdout, stderr)
	case "kill":
		return runSessionKill(args[1:], stdout, stderr)
	case "rename":
		return runSessionRename(args[1:], stdout, stderr)
	case "list-tmux":
		return runSessionListTmux(args[1:], stdout, stderr)
	case "attach-tmux":
		return runSessionAttachTmux(args[1:], stdout, stderr)
	case "detach":
		return runSessionDetach(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stdout, "Unknown session command: %s\n", args[0])
		return 1
	}
}

// runSessionNew creates a new session.
// Usage: pseudocoder session new [--name NAME] [--command CMD] [--addr ADDR]
func runSessionNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session new", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var name, command, addr string
	var port int
	fs.StringVar(&name, "name", "", "Human-readable name for the session")
	fs.StringVar(&command, "command", "", "Command to run (default: host's default shell)")
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session new [options]\n\nCreate a new PTY session.\n\nOptions:\n")
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

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Build request body
	reqBody := struct {
		Name    string   `json:"name,omitempty"`
		Command string   `json:"command,omitempty"`
		Args    []string `json:"args,omitempty"`
	}{
		Name:    name,
		Command: command,
	}

	// Make the API call
	resp, err := callSessionAPI(addrs, "POST", "/api/session/new", reqBody)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Command string `json:"command"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	// Print success message
	displayName := result.Name
	if displayName == "" {
		displayName = result.ID
	}
	fmt.Fprintf(stdout, "Created session: %s\n", displayName)
	fmt.Fprintf(stdout, "  ID:      %s\n", result.ID)
	fmt.Fprintf(stdout, "  Command: %s\n", result.Command)

	return 0
}

// runSessionList lists all active sessions.
// Usage: pseudocoder session list [--addr ADDR] [--json]
func runSessionList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var jsonOutput bool
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session list [options]\n\nList all active sessions.\n\nOptions:\n")
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

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Make the API call
	resp, err := callSessionAPI(addrs, "GET", "/api/session/list", nil)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var sessions []struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Command   string    `json:"command"`
		CreatedAt time.Time `json:"created_at"`
		Running   bool      `json:"running"`
	}
	if err := json.Unmarshal(resp, &sessions); err != nil {
		// Check if it's an error response
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(resp, &errResp) == nil && errResp.Error != "" {
			fmt.Fprintf(stderr, "Error: %s\n", errResp.Error)
			return 1
		}
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No active sessions.")
		return 0
	}

	if jsonOutput {
		// Output as JSON
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sessions); err != nil {
			fmt.Fprintf(stderr, "Error: failed to encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	// Print sessions in a table format
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCOMMAND\tSTATUS\tCREATED")
	fmt.Fprintln(w, "--\t----\t-------\t------\t-------")

	now := time.Now()
	for _, s := range sessions {
		displayName := s.Name
		if displayName == "" {
			displayName = "-"
		}

		status := "stopped"
		if s.Running {
			status = "running"
		}

		createdAgo := formatDuration(now.Sub(s.CreatedAt))

		// Truncate ID for display
		shortID := s.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID,
			displayName,
			s.Command,
			status,
			createdAgo,
		)
	}
	w.Flush()

	return 0
}

// runSessionKill kills a session.
// Usage: pseudocoder session kill [--addr ADDR] <session-id>
func runSessionKill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session kill", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session kill [options] <session-id>\n\nKill a session.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Error: session-id is required")
		fs.Usage()
		return 1
	}

	sessionID := fs.Arg(0)

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Make the API call
	path := fmt.Sprintf("/api/session/%s/kill", sessionID)
	resp, err := callSessionAPI(addrs, "POST", path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		ID     string `json:"id"`
		Killed bool   `json:"killed"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	fmt.Fprintf(stdout, "Killed session: %s\n", result.ID)
	return 0
}

// runSessionRename renames a session.
// Usage: pseudocoder session rename [--addr ADDR] <session-id> <new-name>
func runSessionRename(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session rename", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session rename [options] <session-id> <new-name>\n\nRename a session.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "Error: session-id and new-name are required")
		fs.Usage()
		return 1
	}

	sessionID := fs.Arg(0)
	newName := fs.Arg(1)

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Build request body
	reqBody := struct {
		Name string `json:"name"`
	}{
		Name: newName,
	}

	// Make the API call
	path := fmt.Sprintf("/api/session/%s/rename", sessionID)
	resp, err := callSessionAPI(addrs, "POST", path, reqBody)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	fmt.Fprintf(stdout, "Renamed session %s to: %s\n", result.ID, result.Name)
	return 0
}

// callSessionAPI makes an HTTP request to the session API.
// It tries HTTPS first, then falls back to HTTP.
// Returns the response body or an error.
func callSessionAPI(addrs []string, method, path string, body interface{}) ([]byte, error) {
	// Create HTTP client with short timeout and TLS config that accepts self-signed certs.
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	// Try HTTPS first (default), then HTTP (for --no-tls hosts)
	schemes := []string{"https", "http"}

	var lastErr error
	var firstHTTPErr error
	for _, addr := range addrs {
		for _, scheme := range schemes {
			url := fmt.Sprintf("%s://%s%s", scheme, addr, path)

			var bodyReader io.Reader
			if body != nil {
				bodyBytes, err := json.Marshal(body)
				if err != nil {
					return nil, fmt.Errorf("failed to encode request: %w", err)
				}
				bodyReader = strings.NewReader(string(bodyBytes))
			}

			req, err := http.NewRequest(method, url, bodyReader)
			if err != nil {
				lastErr = err
				continue
			}

			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			defer resp.Body.Close()

			// Read response body
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				lastErr = err
				continue
			}

			// Check for HTTP errors
			if resp.StatusCode >= 400 {
				// Try to parse error message from body
				var errResp struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
					if firstHTTPErr == nil {
						firstHTTPErr = fmt.Errorf("%s", errResp.Error)
					}
					continue
				}
				if firstHTTPErr == nil {
					firstHTTPErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
				}
				continue
			}

			return respBody, nil
		}
	}

	if firstHTTPErr != nil {
		return nil, firstHTTPErr
	}
	if lastErr != nil {
		return nil, fmt.Errorf("host unreachable: %w", lastErr)
	}
	return nil, fmt.Errorf("host unreachable")
}

// runSessionListTmux lists available tmux sessions on the host.
// Usage: pseudocoder session list-tmux [--addr ADDR] [--json]
func runSessionListTmux(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session list-tmux", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var jsonOutput bool
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session list-tmux [options]\n\nList available tmux sessions on the host.\n\nOptions:\n")
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

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Make the API call
	resp, err := callSessionAPI(addrs, "GET", "/api/tmux/list", nil)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		Sessions []struct {
			Name      string    `json:"name"`
			Windows   int       `json:"windows"`
			Attached  bool      `json:"attached"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"sessions"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	if len(result.Sessions) == 0 {
		fmt.Fprintln(stdout, "No tmux sessions available.")
		return 0
	}

	if jsonOutput {
		// Output as JSON
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result.Sessions); err != nil {
			fmt.Fprintf(stderr, "Error: failed to encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	// Print sessions in a table format
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tWINDOWS\tATTACHED\tCREATED")
	fmt.Fprintln(w, "----\t-------\t--------\t-------")

	now := time.Now()
	for _, s := range result.Sessions {
		attachedStr := "no"
		if s.Attached {
			attachedStr = "yes"
		}

		createdAgo := formatDuration(now.Sub(s.CreatedAt))

		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			s.Name,
			s.Windows,
			attachedStr,
			createdAgo,
		)
	}
	w.Flush()

	return 0
}

// runSessionAttachTmux attaches to a tmux session.
// Usage: pseudocoder session attach-tmux [--addr ADDR] [--name SESSION_NAME] <tmux-session>
func runSessionAttachTmux(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session attach-tmux", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var sessionName string
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.StringVar(&sessionName, "name", "", "Optional name for the pseudocoder session")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session attach-tmux [options] <tmux-session>\n\nAttach to a tmux session.\n\nArguments:\n  <tmux-session>  Name of the tmux session to attach to\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Error: tmux-session name is required")
		fs.Usage()
		return 1
	}

	tmuxSession := fs.Arg(0)

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Build request body
	reqBody := struct {
		TmuxSession string `json:"tmux_session"`
		Name        string `json:"name,omitempty"`
	}{
		TmuxSession: tmuxSession,
		Name:        sessionName,
	}

	// Make the API call
	resp, err := callSessionAPI(addrs, "POST", "/api/tmux/attach", reqBody)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		SessionID   string `json:"session_id"`
		TmuxSession string `json:"tmux_session"`
		Name        string `json:"name"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	// Print success message
	displayName := result.Name
	if displayName == "" {
		displayName = result.TmuxSession
	}
	fmt.Fprintf(stdout, "Attached to tmux session: %s\n", displayName)
	fmt.Fprintf(stdout, "  Session ID:   %s\n", result.SessionID)
	fmt.Fprintf(stdout, "  tmux session: %s\n", result.TmuxSession)

	return 0
}

// runSessionDetach detaches from a tmux session.
// Usage: pseudocoder session detach [--addr ADDR] [--kill] <session-id>
func runSessionDetach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session detach", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var kill bool
	var port int
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.BoolVar(&kill, "kill", false, "Also kill the tmux session (not just detach)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder session detach [options] <session-id>\n\nDetach from a tmux session.\n\nArguments:\n  <session-id>  ID of the pseudocoder session to detach\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Error: session-id is required")
		fs.Usage()
		return 1
	}

	sessionID := fs.Arg(0)

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveSessionAddrs(addr, port, explicitFlags, stderr)

	// Build request body
	reqBody := struct {
		Kill bool `json:"kill,omitempty"`
	}{
		Kill: kill,
	}

	// Make the API call
	path := fmt.Sprintf("/api/tmux/%s/detach", sessionID)
	resp, err := callSessionAPI(addrs, "POST", path, reqBody)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Parse response
	var result struct {
		SessionID   string `json:"session_id"`
		TmuxSession string `json:"tmux_session"`
		Killed      bool   `json:"killed"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(stderr, "Error: failed to parse response: %v\n", err)
		return 1
	}

	if result.Error != "" {
		fmt.Fprintf(stderr, "Error: %s\n", result.Error)
		return 1
	}

	// Print success message
	if result.Killed {
		fmt.Fprintf(stdout, "Detached and killed tmux session: %s\n", result.TmuxSession)
	} else {
		fmt.Fprintf(stdout, "Detached from tmux session: %s\n", result.TmuxSession)
		fmt.Fprintln(stdout, "Note: The tmux session is still running and can be reattached.")
	}

	return 0
}
