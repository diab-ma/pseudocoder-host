package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/server"
)

const flagsUsage = `Usage: pseudocoder flags <command> [options]

Commands:
  list                 Show current flag states and metrics
  enable <phase>       Enable a V2 phase
  disable <phase>      Disable a V2 phase
  promote <phase>      Enable a phase with gate verification
  rollout-stage <stage>  Set rollout stage (internal, beta, broader)
  kill-switch <on|off>   Toggle kill switch

Options:
  --addr <host:port>   Host address (default: 127.0.0.1:7070)
  --json               Output in JSON format
`

func runFlags(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stdout, flagsUsage)
		return 1
	}

	switch args[0] {
	case "list":
		return runFlagsList(args[1:], stdout, stderr)
	case "enable":
		return runFlagsPhaseAction(args[1:], "enable", stdout, stderr)
	case "disable":
		return runFlagsPhaseAction(args[1:], "disable", stdout, stderr)
	case "promote":
		return runFlagsPhaseAction(args[1:], "promote", stdout, stderr)
	case "rollout-stage":
		return runFlagsRolloutStage(args[1:], stdout, stderr)
	case "kill-switch":
		return runFlagsKillSwitch(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown flags command: %s\n", args[0])
		fmt.Fprint(stdout, flagsUsage)
		return 1
	}
}

func runFlagsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flags list", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7070", "Host address")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	resp, err := flagsGet(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(resp)
		return 0
	}

	// Human-readable output.
	fmt.Fprintf(stdout, "Kill Switch: %t\n", resp.KillSwitch)
	fmt.Fprintf(stdout, "Rollout Stage: %s\n", resp.RolloutStage)
	fmt.Fprintf(stdout, "\nPhase Flags:\n")
	for _, p := range []int{0, 1, 2, 3, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19} {
		status := "disabled"
		if resp.PhaseFlags[p] {
			status = "enabled"
		}
		fmt.Fprintf(stdout, "  Phase %2d: %s\n", p, status)
	}

	if resp.Metrics != nil {
		fmt.Fprintf(stdout, "\nMetrics (30-min window):\n")
		fmt.Fprintf(stdout, "  Crash-free rate: %.1f%%\n", resp.Metrics.CrashFreeRate)
		fmt.Fprintf(stdout, "  Terminal p95: %dms\n", resp.Metrics.TerminalP95Ms)
		fmt.Fprintf(stdout, "  Pairing success: %.1f%%\n", resp.Metrics.PairingSuccessRate)
	}

	return 0
}

func runFlagsPhaseAction(args []string, action string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flags "+action, flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7070", "Host address")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintf(stderr, "Usage: pseudocoder flags %s <phase>\n", action)
		return 1
	}

	phase, err := strconv.Atoi(remaining[0])
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid phase number: %s\n", remaining[0])
		return 1
	}

	resp, err := flagsPost(*addr, server.FlagsMutationRequest{
		Action: action,
		Phase:  &phase,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if !resp.OK {
		fmt.Fprintf(stderr, "Error: %s\n", resp.Error)
		return 1
	}

	fmt.Fprintf(stdout, "Phase %d %sd successfully\n", phase, action)
	return 0
}

func runFlagsRolloutStage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flags rollout-stage", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7070", "Host address")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintf(stderr, "Usage: pseudocoder flags rollout-stage <internal|beta|broader>\n")
		return 1
	}

	stage := remaining[0]
	resp, err := flagsPost(*addr, server.FlagsMutationRequest{
		Action: "rollout_stage",
		Stage:  stage,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if !resp.OK {
		fmt.Fprintf(stderr, "Error: %s\n", resp.Error)
		return 1
	}

	fmt.Fprintf(stdout, "Rollout stage set to %s\n", stage)
	return 0
}

func runFlagsKillSwitch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flags kill-switch", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7070", "Host address")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintf(stderr, "Usage: pseudocoder flags kill-switch <on|off>\n")
		return 1
	}

	var value bool
	switch strings.ToLower(remaining[0]) {
	case "on", "true", "1":
		value = true
	case "off", "false", "0":
		value = false
	default:
		fmt.Fprintf(stderr, "Error: invalid value %q (use on/off)\n", remaining[0])
		return 1
	}

	resp, err := flagsPost(*addr, server.FlagsMutationRequest{
		Action: "kill_switch",
		Value:  &value,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if !resp.OK {
		fmt.Fprintf(stderr, "Error: %s\n", resp.Error)
		return 1
	}

	state := "OFF"
	if value {
		state = "ON"
	}
	fmt.Fprintf(stdout, "Kill switch %s\n", state)
	return 0
}

func flagsGet(addr string) (*server.FlagsResponse, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	url := fmt.Sprintf("https://%s/api/flags", addr)
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result server.FlagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

func flagsPost(addr string, req server.FlagsMutationRequest) (*server.FlagsMutationResponse, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	body, _ := json.Marshal(req)
	url := fmt.Sprintf("https://%s/api/flags", addr)
	resp, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result server.FlagsMutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to decode response: %w (body: %s)", err, string(respBody))
	}

	return &result, nil
}
