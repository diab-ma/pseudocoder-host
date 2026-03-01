// Package main provides CLI commands for the pseudocoder host.
// This file implements keep-awake policy commands (P18U3).
package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pseudocoder/host/internal/auth"
	"github.com/pseudocoder/host/internal/server"
)

// Error kind constants for keep-awake CLI commands.
const (
	kaErrInvalidArgs  = "invalid_args"
	kaErrAuth         = "auth"
	kaErrUnreachable  = "unreachable"
	kaErrTimeout      = "timeout"
	kaErrHostError    = "host_error"
	kaErrDecodeError  = "decode_error"
	kaErrMissing      = "missing_status"
)

// Locked JSON output types for mutation commands.

type kaJSONMutationSuccess struct {
	OK             bool   `json:"ok"`
	Command        string `json:"command"`
	Target         string `json:"target"`
	RequestID      string `json:"request_id"`
	RemoteEnabled  bool   `json:"remote_enabled"`
	StatusRevision int64  `json:"status_revision"`
	HotApplied     bool   `json:"hot_applied"`
	Persisted      bool   `json:"persisted"`
	Reason         string `json:"reason,omitempty"`
}

type kaJSONMutationFailure struct {
	OK             bool   `json:"ok"`
	Command        string `json:"command"`
	Kind           string `json:"kind"`
	Message        string `json:"message"`
	Target         string `json:"target,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	StatusCode     *int   `json:"status_code,omitempty"`
	StatusRevision *int64 `json:"status_revision,omitempty"`
}

type kaJSONStatusSuccess struct {
	OK             bool   `json:"ok"`
	Command        string `json:"command"`
	Target         string `json:"target"`
	RemoteEnabled  bool   `json:"remote_enabled"`
	StatusRevision int64  `json:"status_revision"`
	KeepAwakeState string `json:"keep_awake_state"`
	RecoveryHint   string `json:"recovery_hint,omitempty"`
}

type kaJSONStatusFailure struct {
	OK         bool   `json:"ok"`
	Command    string `json:"command"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	Target     string `json:"target,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	StatusCode *int   `json:"status_code,omitempty"`
}

// Testability seams (function variables).
var (
	kaReadApprovalToken = func() (string, error) {
		tokenPath, err := auth.DefaultApprovalTokenPath()
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(tokenPath)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("approval token file is empty")
		}
		return token, nil
	}

	kaNewHTTPClient = func() *http.Client {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
	}

	kaGenerateRequestID = func() string {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
)

// runKeepAwake handles the "pseudocoder keep-awake" command group.
func runKeepAwake(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "Usage: pseudocoder keep-awake <command>")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Commands:")
		fmt.Fprintln(stdout, "  enable-remote   Enable remote keep-awake policy")
		fmt.Fprintln(stdout, "  disable-remote  Disable remote keep-awake policy")
		fmt.Fprintln(stdout, "  status          Show keep-awake policy status")
		return 1
	}

	switch args[0] {
	case "enable-remote":
		return runKeepAwakeEnableRemote(args[1:], stdout, stderr)
	case "disable-remote":
		return runKeepAwakeDisableRemote(args[1:], stdout, stderr)
	case "status":
		return runKeepAwakeStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stdout, "Unknown keep-awake command: %s\n", args[0])
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Commands:")
		fmt.Fprintln(stdout, "  enable-remote   Enable remote keep-awake policy")
		fmt.Fprintln(stdout, "  disable-remote  Disable remote keep-awake policy")
		fmt.Fprintln(stdout, "  status          Show keep-awake policy status")
		return 1
	}
}

// runKeepAwakeEnableRemote enables remote keep-awake policy.
func runKeepAwakeEnableRemote(args []string, stdout, stderr io.Writer) int {
	return runKeepAwakeMutation(args, "enable-remote", true, stdout, stderr)
}

// runKeepAwakeDisableRemote disables remote keep-awake policy.
func runKeepAwakeDisableRemote(args []string, stdout, stderr io.Writer) int {
	return runKeepAwakeMutation(args, "disable-remote", false, stdout, stderr)
}

// runKeepAwakeMutation is the core handler for enable-remote and disable-remote.
func runKeepAwakeMutation(args []string, command string, remoteEnabled bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keep-awake "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr, reason string
	var port int
	var jsonOutput bool
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.StringVar(&reason, "reason", "", "Reason for the policy change")
	fs.BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	verb := "Enable"
	if !remoteEnabled {
		verb = "Disable"
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder keep-awake %s [options]\n\n%s remote keep-awake policy.\n\nOptions:\n", command, verb)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if len(fs.Args()) != 0 {
		msg := fmt.Sprintf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONMutationFailure{
				OK:      false,
				Command: command,
				Kind:    kaErrInvalidArgs,
				Message: msg,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		msg := fmt.Sprintf("invalid port: %v", err)
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONMutationFailure{
				OK:      false,
				Command: command,
				Kind:    kaErrInvalidArgs,
				Message: msg,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}
	if addr != "" {
		if err := kaValidateAddr(addr); err != nil {
			msg := fmt.Sprintf("invalid addr format: %v", err)
			if jsonOutput {
				kaWriteJSON(stdout, kaJSONMutationFailure{
					OK:      false,
					Command: command,
					Kind:    kaErrInvalidArgs,
					Message: msg,
				})
			} else {
				fmt.Fprintf(stderr, "Error: %s\n", msg)
			}
			return 1
		}
	}

	reason = strings.TrimSpace(reason)
	addrs := resolveAddrCandidates(addr, port, explicitFlags["port"], stderr)

	requestID := kaGenerateRequestID()

	token, err := kaReadApprovalToken()
	if err != nil {
		msg := fmt.Sprintf("cannot read approval token: %v\n  Run 'pseudocoder host start' to generate the token at ~/.pseudocoder/approval.token", err)
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONMutationFailure{
				OK:        false,
				Command:   command,
				Kind:      kaErrAuth,
				Message:   msg,
				RequestID: requestID,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}

	result := callKeepAwakePolicyAPI(addrs, requestID, remoteEnabled, reason, token)

	if result.OK {
		if jsonOutput {
			out := kaJSONMutationSuccess{
				OK:             true,
				Command:        command,
				Target:         result.Target,
				RequestID:      result.Response.RequestID,
				RemoteEnabled:  remoteEnabled,
				StatusRevision: result.Response.StatusRevision,
				HotApplied:     result.Response.HotApplied,
				Persisted:      result.Response.Persisted,
				Reason:         reason,
			}
			kaWriteJSON(stdout, out)
		} else {
			state := "enabled"
			if !remoteEnabled {
				state = "disabled"
			}
			fmt.Fprintf(stdout, "Keep-awake remote policy %s at %s\n", state, result.Target)
			fmt.Fprintf(stdout, "  Status revision: %d\n", result.Response.StatusRevision)
			fmt.Fprintf(stdout, "  Hot applied:     %v\n", result.Response.HotApplied)
			fmt.Fprintf(stdout, "  Persisted:       %v\n", result.Response.Persisted)
		}
		return 0
	}

	// Failure path
	if jsonOutput {
		out := kaJSONMutationFailure{
			OK:      false,
			Command: command,
			Kind:    result.Kind,
			Message: result.Message,
		}
		if result.LastTarget != "" {
			out.Target = result.LastTarget
		}
		out.RequestID = requestID
		if result.ErrorCode != "" {
			out.ErrorCode = result.ErrorCode
		}
		if result.StatusCode != 0 {
			sc := result.StatusCode
			out.StatusCode = &sc
		}
		if result.StatusRevision != nil {
			out.StatusRevision = result.StatusRevision
		}
		kaWriteJSON(stdout, out)
	} else {
		fmt.Fprintf(stderr, "Error: %s\n", result.Message)
		if result.ErrorCode != "" {
			fmt.Fprintf(stderr, "  Error code:  %s\n", result.ErrorCode)
		}
		if result.LastTarget != "" {
			fmt.Fprintf(stderr, "  Target:      %s\n", result.LastTarget)
		}
		if result.Kind == kaErrUnreachable {
			fmt.Fprintf(stderr, "  Tried:       %s\n", strings.Join(addrs, ", "))
			fmt.Fprintf(stderr, "  Hint:        Is the host running? Try 'pseudocoder host start'\n")
		}
		if result.Kind == kaErrTimeout {
			fmt.Fprintf(stderr, "  Hint:        The host did not respond in time. Check if it is overloaded or unreachable.\n")
		}
	}
	return 1
}

// kaAPIResult holds the outcome of a policy API call.
type kaAPIResult struct {
	OK             bool
	Target         string
	Response       server.KeepAwakePolicyMutationResponse
	Kind           string
	Message        string
	ErrorCode      string
	StatusCode     int
	StatusRevision *int64
	LastTarget     string
}

// callKeepAwakePolicyAPI calls the host policy mutation endpoint with retry across
// addresses and schemes. One stable requestID is used for all attempts.
func callKeepAwakePolicyAPI(addrs []string, requestID string, remoteEnabled bool, reason, token string) kaAPIResult {
	client := kaNewHTTPClient()
	schemes := []string{"https", "http"}

	reqBody := struct {
		RequestID     string `json:"request_id"`
		RemoteEnabled bool   `json:"remote_enabled"`
		Reason        string `json:"reason,omitempty"`
	}{
		RequestID:     requestID,
		RemoteEnabled: remoteEnabled,
		Reason:        reason,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	var firstHostError *kaAPIResult
	var lastTarget string

	for _, addr := range addrs {
		for _, scheme := range schemes {
			target := addr
			lastTarget = target
			url := fmt.Sprintf("%s://%s/api/keep-awake/policy", scheme, addr)

			req, err := http.NewRequest("POST", url, strings.NewReader(string(bodyBytes)))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := client.Do(req)
			if err != nil {
				if isTimeoutError(err) {
					return kaAPIResult{
						Kind:       kaErrTimeout,
						Message:    fmt.Sprintf("request to %s timed out", target),
						LastTarget: target,
					}
				}
				// Transport error → try next scheme/address
				continue
			}

			// Host responded — do not fall back regardless of status
			defer resp.Body.Close()
			var mutation server.KeepAwakePolicyMutationResponse
			if err := json.NewDecoder(resp.Body).Decode(&mutation); err != nil {
				return kaAPIResult{
					Kind:       kaErrDecodeError,
					Message:    fmt.Sprintf("failed to decode response from %s: %v", target, err),
					StatusCode: resp.StatusCode,
					LastTarget: target,
				}
			}

			if resp.StatusCode == http.StatusOK && mutation.Success {
				// Fail-closed: require request_id in response
				if mutation.RequestID == "" {
					return kaAPIResult{
						Kind:       kaErrDecodeError,
						Message:    fmt.Sprintf("response from %s missing request_id", target),
						StatusCode: resp.StatusCode,
						LastTarget: target,
					}
				}
				return kaAPIResult{
					OK:       true,
					Target:   target,
					Response: mutation,
				}
			}

			// Host returned an error response
			rev := mutation.StatusRevision
			result := kaAPIResult{
				Kind:       kaErrHostError,
				Message:    mutation.Error,
				ErrorCode:  mutation.ErrorCode,
				StatusCode: resp.StatusCode,
				LastTarget: target,
			}
			if rev != 0 {
				result.StatusRevision = &rev
			}
			if mutation.Error == "" {
				result.Message = fmt.Sprintf("host returned HTTP %d", resp.StatusCode)
			}
			if firstHostError == nil {
				saved := result
				firstHostError = &saved
			}
			return result
		}
	}

	if firstHostError != nil {
		return *firstHostError
	}
	return kaAPIResult{
		Kind:       kaErrUnreachable,
		Message:    "host is not reachable",
		LastTarget: lastTarget,
	}
}

// runKeepAwakeStatus handles "pseudocoder keep-awake status".
func runKeepAwakeStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keep-awake status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var addr string
	var port int
	var jsonOutput bool
	fs.StringVar(&addr, "addr", "", "Host address (default: localhost, then Tailscale/LAN)")
	fs.IntVar(&port, "port", 7070, "Port to query when auto-selecting address")
	fs.BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder keep-awake status [options]\n\nShow keep-awake policy status.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if len(fs.Args()) != 0 {
		msg := fmt.Sprintf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONStatusFailure{
				OK:      false,
				Command: "status",
				Kind:    kaErrInvalidArgs,
				Message: msg,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(port); err != nil {
		msg := fmt.Sprintf("invalid port: %v", err)
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONStatusFailure{
				OK:      false,
				Command: "status",
				Kind:    kaErrInvalidArgs,
				Message: msg,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}
	if addr != "" {
		if err := kaValidateAddr(addr); err != nil {
			msg := fmt.Sprintf("invalid addr format: %v", err)
			if jsonOutput {
				kaWriteJSON(stdout, kaJSONStatusFailure{
					OK:      false,
					Command: "status",
					Kind:    kaErrInvalidArgs,
					Message: msg,
				})
			} else {
				fmt.Fprintf(stderr, "Error: %s\n", msg)
			}
			return 1
		}
	}

	addrs := resolveAddrCandidates(addr, port, explicitFlags["port"], stderr)

	var status *server.StatusResponse
	var successTarget string
	var statusErr kaStatusError
	for _, target := range addrs {
		status, statusErr = queryKeepAwakeStatus(target)
		if statusErr.Kind == "" {
			successTarget = target
			break
		}
	}

	if status == nil {
		msg := statusErr.Message
		if msg == "" {
			msg = "host is not reachable"
		}
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONStatusFailure{
				OK:      false,
				Command: "status",
				Kind:    statusErr.Kind,
				Message: msg,
				Target:  statusErr.Target,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
			if statusErr.Target != "" {
				fmt.Fprintf(stderr, "  Target: %s\n", statusErr.Target)
			}
			if statusErr.Kind == kaErrUnreachable {
				fmt.Fprintf(stderr, "  Tried:  %s\n", strings.Join(addrs, ", "))
				fmt.Fprintf(stderr, "  Hint:   Is the host running? Try 'pseudocoder host start'\n")
			}
			if statusErr.Kind == kaErrTimeout {
				fmt.Fprintf(stderr, "  Hint:   The host did not respond in time. Check if it is overloaded or unreachable.\n")
			}
		}
		return 1
	}

	if status.KeepAwake == nil {
		msg := "keep-awake status is not available on this host"
		if jsonOutput {
			kaWriteJSON(stdout, kaJSONStatusFailure{
				OK:      false,
				Command: "status",
				Kind:    kaErrMissing,
				Message: msg,
				Target:  successTarget,
			})
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
		}
		return 1
	}

	ka := status.KeepAwake
	if jsonOutput {
		out := kaJSONStatusSuccess{
			OK:             true,
			Command:        "status",
			Target:         successTarget,
			RemoteEnabled:  ka.RemoteEnabled,
			StatusRevision: ka.StatusRevision,
			KeepAwakeState: ka.State,
			RecoveryHint:   ka.RecoveryHint,
		}
		kaWriteJSON(stdout, out)
	} else {
		fmt.Fprintf(stdout, "Keep-Awake Policy Status at %s\n", successTarget)
		fmt.Fprintf(stdout, "  Remote enabled:  %v\n", ka.RemoteEnabled)
		fmt.Fprintf(stdout, "  State:           %s\n", ka.State)
		fmt.Fprintf(stdout, "  Status revision: %d\n", ka.StatusRevision)
		if ka.RecoveryHint != "" {
			fmt.Fprintf(stdout, "  Recovery hint:   %s\n", ka.RecoveryHint)
		}
	}
	return 0
}

// isTimeoutError checks if an error is a network timeout.
func isTimeoutError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

type kaStatusError struct {
	Kind    string
	Message string
	Target  string
}

func queryKeepAwakeStatus(addr string) (*server.StatusResponse, kaStatusError) {
	client := kaNewHTTPClient()
	schemes := []string{"https", "http"}

	for _, scheme := range schemes {
		url := fmt.Sprintf("%s://%s/status", scheme, addr)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, kaStatusError{
				Kind:    kaErrInvalidArgs,
				Message: fmt.Sprintf("invalid target %q", addr),
				Target:  addr,
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			if isTimeoutError(err) {
				return nil, kaStatusError{
					Kind:    kaErrTimeout,
					Message: fmt.Sprintf("request to %s timed out", addr),
					Target:  addr,
				}
			}
			continue
		}

		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, kaStatusError{
				Kind:    kaErrHostError,
				Message: fmt.Sprintf("host returned HTTP %d", resp.StatusCode),
				Target:  addr,
			}
		}

		var status server.StatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return nil, kaStatusError{
				Kind:    kaErrDecodeError,
				Message: fmt.Sprintf("failed to decode status response from %s: %v", addr, err),
				Target:  addr,
			}
		}
		return &status, kaStatusError{}
	}

	return nil, kaStatusError{
		Kind:    kaErrUnreachable,
		Message: fmt.Sprintf("host is not reachable: %s", addr),
		Target:  addr,
	}
}

func kaValidateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("expected host:port (%v)", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host must not be empty")
	}
	if strings.TrimSpace(port) == "" {
		return fmt.Errorf("port must not be empty")
	}
	return nil
}

// kaWriteJSON writes a JSON object to the writer with indentation.
func kaWriteJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
