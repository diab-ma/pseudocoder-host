package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pseudocoder/host/internal/actions"
	"github.com/pseudocoder/host/internal/auth"
	"github.com/pseudocoder/host/internal/config"
	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/mdns"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/server"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/stream"
	hostTLS "github.com/pseudocoder/host/internal/tls"
	"github.com/pseudocoder/host/internal/tmux"
)

// HostStartConfig holds the configuration for the host start command.
type HostStartConfig struct {
	Repo                    string
	Addr                    string
	TLSCert                 string
	TLSKey                  string
	NoTLS                   bool
	TokenStore              string
	LogLevel                string
	HistoryLines            int
	DiffPollMs              int
	SessionCmd              string
	Config                  string
	RequireAuth             bool
	CommitAllowNoVerify     bool
	CommitAllowNoGpgSign    bool
	PushAllowForceWithLease bool
	Daemon                  bool
	LocalTerminal           bool
	PIDFile                 string
	LogFile                 string
	MdnsEnabled             bool
	Pair                    bool
	QR                      bool
}

func runHostStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("host start", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := &HostStartConfig{}

	fs.StringVar(&cfg.Config, "config", "", "Path to config file (default: ~/.pseudocoder/config.toml)")
	fs.StringVar(&cfg.Repo, "repo", "", "Path to repository to supervise (default: current directory)")
	fs.StringVar(&cfg.Addr, "addr", "", "Host address for WebSocket server (default: 127.0.0.1:7070)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Path to TLS certificate file (default: ~/.pseudocoder/certs/host.crt)")
	fs.StringVar(&cfg.TLSKey, "tls-key", "", "Path to TLS key file (default: ~/.pseudocoder/certs/host.key)")
	fs.BoolVar(&cfg.NoTLS, "no-tls", false, "Disable TLS (insecure, for development only)")
	fs.StringVar(&cfg.TokenStore, "token-store", "", "Path to token/device store (default: ~/.pseudocoder/pseudocoder.db)")
	fs.StringVar(&cfg.LogLevel, "log-level", "", "Log level: debug, info, warn, error (default: info)")
	fs.IntVar(&cfg.HistoryLines, "history-lines", 0, "Number of terminal lines to retain (default: 5000)")
	fs.IntVar(&cfg.DiffPollMs, "diff-poll-ms", 0, "Interval for git diff polling in ms (default: 1000)")
	fs.StringVar(&cfg.SessionCmd, "session-cmd", "", "Command to run in the PTY session")
	fs.BoolVar(&cfg.RequireAuth, "require-auth", false, "Require authentication for WebSocket connections")
	fs.BoolVar(&cfg.CommitAllowNoVerify, "commit-allow-no-verify", false, "Allow clients to skip pre-commit hooks (not recommended)")
	fs.BoolVar(&cfg.CommitAllowNoGpgSign, "commit-allow-no-gpg-sign", false, "Allow clients to skip GPG signing")
	fs.BoolVar(&cfg.PushAllowForceWithLease, "push-allow-force-with-lease", false, "Allow clients to use force-with-lease (not recommended)")
	fs.BoolVar(&cfg.Daemon, "daemon", false, "Run host in background as daemon")
	fs.BoolVar(&cfg.LocalTerminal, "local-terminal", false, "Show PTY output locally (default: headless)")
	fs.StringVar(&cfg.PIDFile, "pid-file", "", "PID file path (default: ~/.pseudocoder/host.pid)")
	fs.StringVar(&cfg.LogFile, "log-file", "", "Log file path (default: ~/.pseudocoder/host.log)")
	fs.BoolVar(&cfg.MdnsEnabled, "mdns", false, "Enable mDNS/Bonjour discovery (LAN-visible)")
	fs.BoolVar(&cfg.Pair, "pair", false, "Generate and display pairing code during startup")
	fs.BoolVar(&cfg.QR, "qr", false, "Display pairing code as QR code (requires --pair)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder host start [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	// Track which flags were explicitly set on the command line.
	// This allows us to distinguish "flag not specified" from "flag set to default value".
	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	// Load config file and merge with CLI flags.
	// CLI flags take precedence over file values.
	fileCfg, err := config.Load(cfg.Config)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Merge file config with CLI flags: only apply file values if CLI value is zero/empty.
	// This ensures explicit CLI flags always override config file settings.
	if cfg.Repo == "" {
		cfg.Repo = fileCfg.Repo
	}
	if cfg.Addr == "" {
		cfg.Addr = fileCfg.Addr
	}
	if cfg.TLSCert == "" {
		cfg.TLSCert = fileCfg.TLSCert
	}
	if cfg.TLSKey == "" {
		cfg.TLSKey = fileCfg.TLSKey
	}
	if cfg.TokenStore == "" {
		cfg.TokenStore = fileCfg.TokenStore
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = fileCfg.LogLevel
	}
	if cfg.HistoryLines == 0 {
		cfg.HistoryLines = fileCfg.HistoryLines
	}
	if cfg.DiffPollMs == 0 {
		cfg.DiffPollMs = fileCfg.DiffPollMs
	}
	if cfg.SessionCmd == "" {
		cfg.SessionCmd = fileCfg.SessionCmd
	}
	// Boolean flags: apply config value only if flag was NOT explicitly set on CLI.
	// This allows users to override config file booleans with --flag=false.
	if !explicitFlags["require-auth"] {
		cfg.RequireAuth = fileCfg.RequireAuth
	}
	if !explicitFlags["commit-allow-no-verify"] {
		cfg.CommitAllowNoVerify = fileCfg.CommitAllowNoVerify
	}
	if !explicitFlags["commit-allow-no-gpg-sign"] {
		cfg.CommitAllowNoGpgSign = fileCfg.CommitAllowNoGpgSign
	}
	if !explicitFlags["push-allow-force-with-lease"] {
		cfg.PushAllowForceWithLease = fileCfg.PushAllowForceWithLease
	}
	if !explicitFlags["daemon"] {
		cfg.Daemon = fileCfg.Daemon
	}
	if !explicitFlags["local-terminal"] {
		cfg.LocalTerminal = fileCfg.LocalTerminal
	}
	if cfg.PIDFile == "" {
		cfg.PIDFile = fileCfg.PIDFile
	}
	if cfg.LogFile == "" {
		cfg.LogFile = fileCfg.LogFile
	}
	if !explicitFlags["mdns"] {
		cfg.MdnsEnabled = fileCfg.MdnsEnabled
	}
	if !explicitFlags["pair"] {
		cfg.Pair = fileCfg.Pair
	}
	if !explicitFlags["qr"] {
		cfg.QR = fileCfg.QR
	}

	// If --qr is set without --pair, auto-enable --pair.
	// Displaying a QR code without generating a pairing code doesn't make sense.
	if cfg.QR && !cfg.Pair {
		cfg.Pair = true
	}

	// Handle daemon mode: re-exec in background if requested.
	// Go doesn't support fork(), so we use the re-exec pattern:
	// 1. Parent: set env var and re-exec the same binary, then exit
	// 2. Child: detect env var, continue with normal execution
	const daemonEnvVar = "PSEUDOCODER_DAEMON_CHILD"
	var logFile *os.File

	if cfg.Daemon && os.Getenv(daemonEnvVar) == "" {
		// Parent process: re-exec as daemon child
		logFilePath, err := resolveLogFilePath(cfg.LogFile)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}

		// Ensure log directory exists
		if err := os.MkdirAll(filepath.Dir(logFilePath), 0755); err != nil {
			fmt.Fprintf(stderr, "Error: failed to create log directory: %v\n", err)
			return 1
		}

		// Open log file for child's stdout/stderr
		logFileHandle, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to open log file: %v\n", err)
			return 1
		}

		// Re-exec the same command with daemon env var set
		exe, err := os.Executable()
		if err != nil {
			logFileHandle.Close()
			fmt.Fprintf(stderr, "Error: failed to get executable path: %v\n", err)
			return 1
		}

		// Build args: "host start" + all original args
		childArgs := append([]string{"host", "start"}, args...)
		cmd := exec.Command(exe, childArgs...)
		cmd.Stdout = logFileHandle
		cmd.Stderr = logFileHandle
		cmd.Stdin = nil
		cmd.Env = append(os.Environ(), daemonEnvVar+"=1")

		// Start the child process
		if err := cmd.Start(); err != nil {
			logFileHandle.Close()
			fmt.Fprintf(stderr, "Error: failed to start daemon: %v\n", err)
			return 1
		}

		// Wait for child to either exit (startup failure) or survive past startup.
		// Use a channel to detect early exit, with a timeout for successful startup.
		childPid := cmd.Process.Pid
		childDone := make(chan error, 1)
		go func() {
			childDone <- cmd.Wait()
		}()

		// Wait up to 2 seconds for child to either succeed or fail
		select {
		case err := <-childDone:
			// Child exited - this means startup failed
			logFileHandle.Close()
			if err != nil {
				fmt.Fprintf(stderr, "Error: daemon failed to start (exit: %v, check log: %s)\n", err, logFilePath)
			} else {
				fmt.Fprintf(stderr, "Error: daemon exited unexpectedly (check log: %s)\n", logFilePath)
			}
			return 1
		case <-time.After(2 * time.Second):
			// Child still running after 2 seconds - assume successful startup
			fmt.Fprintf(stdout, "Daemon started (pid %d). Logging to: %s\n", childPid, logFilePath)
			logFileHandle.Close()
			return 0
		}
	}

	// Child process (or non-daemon mode): redirect to log file if daemon
	if cfg.Daemon {
		// We're in daemon child - redirect to log file
		logFilePath, err := resolveLogFilePath(cfg.LogFile)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		logFile, err = os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to open log file: %v\n", err)
			return 1
		}
		// Redirect stdout and stderr to log file
		stdout = logFile
		stderr = logFile
	}

	// Apply defaults
	historyLines := cfg.HistoryLines
	if historyLines <= 0 {
		historyLines = 5000
	}

	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:7070"
	}

	// Determine the command to run
	sessionCmd := cfg.SessionCmd
	if sessionCmd == "" {
		// Default to the user's shell
		sessionCmd = os.Getenv("SHELL")
		if sessionCmd == "" {
			sessionCmd = "/bin/sh"
		}
	}

	// Apply defaults for diff polling
	diffPollMs := cfg.DiffPollMs
	if diffPollMs <= 0 {
		diffPollMs = 1000
	}

	// Determine the repo path for diff polling
	repoPath := cfg.Repo
	if repoPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to get current directory: %v\n", err)
			return 1
		}
		repoPath = cwd
	}

	// Determine the token store path (used for card storage)
	tokenStorePath := cfg.TokenStore
	if tokenStorePath == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to get home directory: %v\n", err)
			return 1
		}
		tokenStorePath = filepath.Join(homeDir, ".pseudocoder", "pseudocoder.db")

		// Ensure the directory exists
		if err := os.MkdirAll(filepath.Dir(tokenStorePath), 0700); err != nil {
			fmt.Fprintf(stderr, "Error: failed to create config directory: %v\n", err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "Starting PTY session with command: %s\n", sessionCmd)
	fmt.Fprintf(stdout, "History lines: %d\n", historyLines)
	fmt.Fprintf(stdout, "WebSocket server address: %s\n", addr)
	fmt.Fprintf(stdout, "Repository path: %s\n", repoPath)
	fmt.Fprintf(stdout, "Diff poll interval: %dms\n", diffPollMs)

	// Open SQLite storage for review cards and devices.
	// This allows cards and device tokens to survive restarts.
	store, err := storage.NewSQLiteStore(tokenStorePath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to open storage: %v\n", err)
		return 1
	}

	// Create the pairing manager for device authentication.
	// This handles pairing codes, token generation, and rate limiting.
	pairingManager := auth.NewPairingManager(auth.PairingConfig{
		DeviceStore: store,
	})

	// Create the token validator for WebSocket authentication.
	tokenValidator := auth.NewTokenValidator(store)

	// Create WebSocket server
	wsServer := server.NewServer(addr)

	// Create and save the current session for history tracking (Unit 6.3a).
	// This enables mobile clients to see session history and switch contexts.
	currentSession := &storage.Session{
		ID:        wsServer.SessionID(),
		Repo:      repoPath,
		Branch:    getCurrentBranch(repoPath),
		StartedAt: time.Now(),
		LastSeen:  time.Now(),
		Status:    storage.SessionStatusRunning,
	}
	if err := store.SaveSession(currentSession); err != nil {
		// Log warning but don't fail - session history is nice-to-have
		fmt.Fprintf(stderr, "Warning: failed to save session: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "Session: %s (branch: %s)\n", currentSession.ID, currentSession.Branch)
	}

	// Wire up authentication
	wsServer.SetRequireAuth(cfg.RequireAuth)
	wsServer.SetTokenValidator(func(token string) (string, error) {
		device, err := tokenValidator.ValidateToken(token)
		if err != nil {
			return "", err
		}
		return device.ID, nil
	})

	// Wire up pairing endpoints
	wsServer.SetPairHandler(auth.NewPairHandler(pairingManager))
	wsServer.SetGenerateCodeHandler(auth.NewGenerateCodeHandler(pairingManager))

	// Wire up device revocation endpoint.
	// This allows the CLI to signal the running host to close connections
	// for revoked devices immediately, meeting the 2-second requirement.
	revokeHandler := server.NewRevokeDeviceHandler(wsServer, &deviceStoreAdapter{store})
	wsServer.SetRevokeDeviceHandler(revokeHandler)

	// Wire up CLI approval endpoint (Phase 6.1b).
	// This allows CLI tools to request command approval via HTTP.
	approvalTokenPath, err := auth.DefaultApprovalTokenPath()
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to get approval token path: %v\n", err)
		store.Close()
		return 1
	}
	approvalTokenManager := auth.NewApprovalTokenManager(approvalTokenPath)
	if _, err := approvalTokenManager.EnsureToken(); err != nil {
		fmt.Fprintf(stderr, "Error: failed to initialize approval token: %v\n", err)
		store.Close()
		return 1
	}
	fmt.Fprintf(stdout, "Approval token: %s\n", approvalTokenManager.TokenPath())

	// Create approval queue and handler.
	// The queue manages pending approval requests, forwarding them to mobile clients.
	// Default timeout: 60 seconds.
	approvalQueue := server.NewApprovalQueue(60*time.Second, wsServer.Broadcast)
	wsServer.SetApprovalQueue(approvalQueue)

	// Wire up audit logging for approval decisions.
	// All approval decisions (approved, denied, timeout, auto-approved) are recorded.
	auditAdapter := server.NewAuditStoreAdapter(store)
	approvalQueue.SetAuditStore(auditAdapter)

	// Create approval HTTP handler.
	approveHandler := server.NewApproveHandler(wsServer, approvalTokenManager)
	wsServer.SetApproveHandler(approveHandler)

	// Wire up device activity tracking for last_seen updates.
	// This is called when a message is received from an authenticated client.
	wsServer.SetDeviceActivityTracker(func(deviceID string) {
		if err := store.UpdateLastSeen(deviceID, time.Now()); err != nil {
			// Log but don't fail - activity tracking is best-effort
			fmt.Fprintf(stderr, "Warning: failed to update last_seen for device %s: %v\n", deviceID, err)
		}
	})

	// Wire up status handler for CLI queries (Unit 7.5).
	// This enables "pseudocoder host status" to query the running host.
	// Must be set BEFORE starting the server so the endpoint is registered.
	statusHandler := server.NewStatusHandler(wsServer, repoPath, currentSession.Branch, !cfg.NoTLS, cfg.RequireAuth)
	wsServer.SetStatusHandler(statusHandler)

	// Create session manager for multi-session support (Unit 9.1-9.5).
	// The session manager tracks all PTY sessions and provides API access.
	// Must be created BEFORE starting the server so API endpoints are registered.
	sessionManager := pty.NewSessionManager()

	// Create the session API handler for CLI session commands (Unit 9.5).
	// This enables "pseudocoder session new/list/kill/rename" commands.
	// Must be set BEFORE starting the server so the endpoint is registered.
	sessionAPIHandler := server.NewSessionAPIHandler(sessionManager, server.SessionAPIConfig{
		DefaultCommand: sessionCmd,
		HistoryLines:   historyLines,
		OnSessionOutput: func(sessionID, line string) {
			// Broadcast session output to all connected clients.
			// TODO: Route to session-specific clients in Phase 10.
			wsServer.BroadcastTerminalOutputWithID(sessionID, line)
		},
	})
	wsServer.SetSessionAPIHandler(sessionAPIHandler)
	wsServer.SetSessionManager(sessionManager)

	// Create the tmux manager for session discovery and attachment (Phase 12).
	// Must be set BEFORE starting the server so API endpoints are registered.
	tmuxMgr := tmux.NewManager()
	wsServer.SetTmuxManager(tmuxMgr)

	// Create the tmux API handler for CLI tmux commands (Unit 12.9).
	// This enables "pseudocoder session list-tmux/attach-tmux/detach" commands.
	// Must be set BEFORE starting the server so the endpoint is registered.
	tmuxAPIHandler := server.NewTmuxAPIHandler(tmuxMgr, sessionManager, server.TmuxAPIConfig{
		HistoryLines: historyLines,
		OnSessionOutput: func(sessionID, line string) {
			// Broadcast session output to all connected clients.
			wsServer.BroadcastTerminalOutputWithID(sessionID, line)
		},
	})
	wsServer.SetTmuxAPIHandler(tmuxAPIHandler)

	// Start the WebSocket server with or without TLS.
	// TLS is enabled by default; use --no-tls to disable (insecure).
	var wsErrCh <-chan error
	var certInfo *hostTLS.CertInfo

	if cfg.NoTLS {
		// Insecure mode: no TLS (for development only)
		fmt.Fprintln(stdout, "WARNING: TLS disabled (--no-tls). Connections are NOT encrypted.")
		wsErrCh = wsServer.StartAsync()
	} else {
		// TLS mode: ensure certificate exists (generate if needed)
		// Build the list of hosts/IPs for the certificate SANs.
		// Always include localhost and common loopback addresses.
		// Also include the configured listen host if it's different.
		tlsHosts := []string{"localhost", "127.0.0.1", "0.0.0.0"}
		if listenHost, _, err := net.SplitHostPort(addr); err == nil && listenHost != "" {
			// Add the configured host if it's not already in the list
			found := false
			for _, h := range tlsHosts {
				if h == listenHost {
					found = true
					break
				}
			}
			if !found {
				tlsHosts = append(tlsHosts, listenHost)
			}
		}

		certInfo, err = hostTLS.EnsureCertificate(hostTLS.CertConfig{
			CertPath: cfg.TLSCert,
			KeyPath:  cfg.TLSKey,
			Hosts:    tlsHosts,
		})
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to setup TLS certificate: %v\n", err)
			store.Close()
			return 1
		}

		if certInfo.IsGenerated {
			fmt.Fprintln(stdout, "Generated new self-signed TLS certificate")
		} else {
			fmt.Fprintln(stdout, "Loaded existing TLS certificate")
		}
		fmt.Fprintf(stdout, "Certificate: %s\n", certInfo.CertPath)
		fmt.Fprintf(stdout, "Private key: %s\n", certInfo.KeyPath)
		fmt.Fprintf(stdout, "Valid until: %s\n", certInfo.NotAfter.Format("2006-01-02"))
		fmt.Fprintf(stdout, "Fingerprint (SHA-256):\n  %s\n", certInfo.Fingerprint)

		wsErrCh = wsServer.StartAsyncTLS(server.TLSConfig{
			CertPath: certInfo.CertPath,
			KeyPath:  certInfo.KeyPath,
		})
	}

	// Wait for server startup to complete.
	// This fails fast if the port is already in use or can't be bound.
	if err := <-wsErrCh; err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		store.Close()
		return 1
	}

	// Determine and write the PID file.
	// The PID file allows "host stop" and other tools to find the running host.
	pidFilePath := cfg.PIDFile
	if pidFilePath == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to get home directory for PID file: %v\n", err)
		} else {
			pidFilePath = filepath.Join(homeDir, ".pseudocoder", "host.pid")
		}
	}
	if pidFilePath != "" {
		if err := writePIDFile(pidFilePath); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to write PID file: %v\n", err)
		} else {
			fmt.Fprintf(stdout, "PID file: %s\n", pidFilePath)
		}
	}

	if cfg.RequireAuth {
		fmt.Fprintln(stdout, "Authentication: ENABLED (use 'pseudocoder pair' to pair devices)")
	} else {
		fmt.Fprintln(stdout, "Authentication: DISABLED (use --require-auth to enable)")
	}

	// Generate and display pairing code if --pair flag is set.
	// This allows users to pair devices without running 'pseudocoder pair' separately.
	if cfg.Pair {
		// Extract port from configured address for display address construction.
		// This ensures the display address uses the actual configured port, not a hardcoded default.
		_, portStr, _ := net.SplitHostPort(addr)
		if portStr == "" {
			portStr = "7070" // default port if not specified
		}

		// Determine display address using the same logic as 'pseudocoder pair' command.
		// Priority: Tailscale IP > LAN IP > configured address
		displayAddr := ""
		if ip := GetTailscaleIP(); ip != "" {
			displayAddr = ip + ":" + portStr
		} else if ip := GetPreferredOutboundIP(); ip != "" {
			displayAddr = ip + ":" + portStr
		} else {
			// Use the configured address as fallback
			displayAddr = addr
		}

		// Generate pairing code using the pairing manager
		code, err := pairingManager.GenerateCode()
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to generate pairing code: %v\n", err)
		} else {
			// Get the code expiry time from the pairing manager
			expiry := pairingManager.GetCodeExpiry()

			// Get certificate fingerprint for QR code (empty if no TLS)
			fingerprint := ""
			if certInfo != nil {
				fingerprint = certInfo.Fingerprint
			}

			// Display the pairing code (as QR or text, based on --qr flag)
			if cfg.QR {
				DisplayQRCode(stdout, code, expiry, displayAddr, fingerprint)
			} else {
				DisplayPairingCode(stdout, code, expiry, displayAddr)
			}
		}
	}

	// Start mDNS advertiser if enabled.
	// This allows mobile apps to discover the host on the local network.
	// mDNS only reveals presence; pairing codes are still required for auth.
	var mdnsAdvertiser *mdns.Advertiser
	if cfg.MdnsEnabled {
		// Extract port from address for mDNS advertisement
		_, portStr, _ := net.SplitHostPort(addr)
		port := 7070 // default
		if portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
				port = p
			}
		}

		// Get certificate fingerprint for trust verification (if TLS enabled)
		fingerprint := ""
		if certInfo != nil {
			fingerprint = certInfo.Fingerprint
		}

		mdnsAdvertiser = mdns.NewAdvertiser(mdns.Config{
			Port:        port,
			Fingerprint: fingerprint,
		})
		if err := mdnsAdvertiser.Start(); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to start mDNS discovery: %v\n", err)
		} else {
			fmt.Fprintln(stdout, "mDNS discovery: ENABLED (visible on LAN)")
		}
	}

	// Create the action processor for handling accept/reject decisions.
	// This applies git stage/restore operations based on user decisions.
	actionProcessor := actions.NewProcessor(store, repoPath)
	actionProcessor.SetChunkStore(store)
	actionProcessor.SetDecidedStore(store) // Phase 20: Enable undo support

	// Wire up the decision handler to route decisions from WebSocket clients
	// to the action processor.
	wsServer.SetDecisionHandler(func(cardID, action, comment string) error {
		return actionProcessor.ProcessDecision(cardID, action, comment)
	})

	// Wire up the chunk decision handler for per-chunk accept/reject.
	// The contentHash parameter enables stale detection (empty skips validation).
	wsServer.SetChunkDecisionHandler(func(cardID string, chunkIndex int, action string, contentHash string) error {
		return actionProcessor.ProcessChunkDecision(cardID, chunkIndex, action, contentHash)
	})

	// Wire up the delete handler for removing untracked files.
	// This is used when the user confirms deletion of a new file that cannot be restored.
	wsServer.SetDeleteHandler(func(cardID string) error {
		return actionProcessor.DeleteUntrackedFile(cardID)
	})

	// Phase 20.2: Wire up the undo handlers for reversing decisions.
	// These allow users to undo accepted/rejected/committed cards and re-review them.
	wsServer.SetUndoHandler(func(cardID string, confirmed bool) (*server.UndoResult, error) {
		card, err := actionProcessor.ProcessUndo(cardID, confirmed)
		if err != nil {
			return nil, err
		}
		return &server.UndoResult{
			CardID:       card.ID,
			ChunkIndex:   -1, // File-level undo
			File:         card.File,
			Diff:         card.Patch,
			OriginalDiff: card.OriginalDiff,
		}, nil
	})

	wsServer.SetChunkUndoHandler(func(cardID string, chunkIndex int, confirmed bool) (*server.UndoResult, error) {
		chunk, card, err := actionProcessor.ProcessChunkUndo(cardID, chunkIndex, confirmed)
		if err != nil {
			return nil, err
		}
		return &server.UndoResult{
			CardID:       card.ID,
			ChunkIndex:   chunk.ChunkIndex,
			File:         card.File,
			Diff:         chunk.Patch,
			OriginalDiff: card.OriginalDiff,
		}, nil
	})

	// Wire up git operations for commit/push workflow (Units 6.4, 6.6).
	// This enables mobile clients to request repo status, create commits, and push.
	gitOps := server.NewGitOperations(repoPath, cfg.CommitAllowNoVerify, cfg.CommitAllowNoGpgSign, cfg.PushAllowForceWithLease)
	wsServer.SetGitOperations(gitOps)

	// Phase 20.2: Set the decided store on the server for commit association.
	// This allows marking accepted cards as committed when a commit is created.
	wsServer.SetDecidedStore(store)

	// We'll set up the reconnect handler after creating the PTY session and card streamer,
	// since we need access to the ring buffer and pending cards.
	// For now, we defer this until after those are created.

	// Create the card streamer with a placeholder for the poller.
	// We'll set up the poller's OnChunks callback to use the streamer.
	var cardStreamer *stream.CardStreamer

	// Avoid surfacing host state files as review cards when stored inside the repo.
	excludePaths := []string{}
	if tokenStorePath != "" && tokenStorePath != ":memory:" {
		repoAbs, err := filepath.Abs(repoPath)
		if err == nil {
			tokenAbs, err := filepath.Abs(tokenStorePath)
			if err == nil {
				relPath, err := filepath.Rel(repoAbs, tokenAbs)
				if err == nil {
					relPath = filepath.ToSlash(relPath)
					if relPath != "." && relPath != ".." && !strings.HasPrefix(relPath, "../") {
						excludePaths = append(excludePaths, relPath)
					}
				}
			}
		}
	}

	// Create the diff poller with an OnChunksRaw callback that forwards to the streamer.
	// We use OnChunksRaw instead of OnChunks to get the raw diff output, which is needed
	// for binary file detection and diff statistics.
	diffPoller := diff.NewPoller(diff.PollerConfig{
		RepoPath:         repoPath,
		PollInterval:     time.Duration(diffPollMs) * time.Millisecond,
		IncludeStaged:    false, // Only track unstaged changes
		IncludeUntracked: true,  // Track new files that haven't been added yet
		ExcludePaths:     excludePaths,
		OnChunksRaw: func(chunks []*diff.Chunk, rawDiff string) {
			// Forward chunks and raw diff to the card streamer.
			// cardStreamer is set before the poller starts, so this is safe.
			if cardStreamer != nil {
				cardStreamer.ProcessChunksRaw(chunks, rawDiff)
			}
		},
		OnError: func(err error) {
			fmt.Fprintf(stderr, "Diff poll error: %v\n", err)
		},
	})

	// Now create the card streamer with all dependencies.
	cardStreamer = stream.NewCardStreamer(stream.CardStreamerConfig{
		Poller:                 diffPoller,
		Store:                  store,
		ChunkStore:             store, // SQLiteStore implements both CardStore and ChunkStore
		Broadcaster:            wsServer,
		SessionID:              wsServer.SessionID(),
		ChunkGroupingEnabled:   fileCfg.ChunkGroupingEnabled,
		ChunkGroupingProximity: fileCfg.ChunkGroupingProximity,
		OnError: func(err error) {
			fmt.Fprintf(stderr, "Card streaming error: %v\n", err)
		},
	})

	// Wire up the card removed callback so the streamer clears cached hashes.
	// This ensures identical diffs appearing after a decision are detected.
	actionProcessor.SetCardRemovedCallback(func(file string) {
		cardStreamer.ClearFileHash(file)
	})

	// Start the card streamer (which also starts the diff poller).
	if err := cardStreamer.Start(); err != nil {
		fmt.Fprintf(stderr, "Error: failed to start card streamer: %v\n", err)
		if pidFilePath != "" {
			removePIDFile(pidFilePath, stderr)
		}
		store.Close()
		wsServer.Stop()
		return 1
	}

	fmt.Fprintln(stdout, "Review card streaming started.")

	// Create PTY session with output going to WebSocket (and optionally stdout).
	// By default (headless mode), PTY output is only streamed to clients.
	// Use --local-terminal to also display PTY output locally.
	ptySession := pty.NewSession(pty.SessionConfig{
		HistoryLines: historyLines,
		OnOutput: func(line string) {
			// Print to stdout only if --local-terminal is set
			if cfg.LocalTerminal {
				fmt.Fprint(stdout, line)
			}
			// Always broadcast to all connected WebSocket clients
			wsServer.BroadcastTerminalOutput(line)
		},
	})

	// Start the PTY session - wrap in shell if command contains shell metacharacters
	var ptyErr error
	if strings.ContainsAny(sessionCmd, " \t;|&$`'\"\\") {
		// Use shell to interpret the command
		ptyErr = ptySession.Start("/bin/sh", "-c", sessionCmd)
	} else {
		// Simple command, run directly
		ptyErr = ptySession.Start(sessionCmd)
	}

	if ptyErr != nil {
		fmt.Fprintf(stderr, "Error: failed to start PTY session: %v\n", ptyErr)
		if pidFilePath != "" {
			removePIDFile(pidFilePath, stderr)
		}
		cardStreamer.Stop()
		store.Close()
		wsServer.Stop()
		return 1
	}

	// Enable bidirectional terminal: mobile clients can send input to the PTY.
	// Phase 5.5: This allows typing commands and interacting with the terminal
	// from the mobile app.
	wsServer.SetPTYWriter(ptySession)

	// Register the default PTY session with SessionManager (Phase 16.3a).
	// This enables terminal.resize messages to route through SessionManager.
	// The session ID matches the WebSocket server's session ID for consistency.
	ptySession.ID = wsServer.SessionID()
	if err := sessionManager.Register(ptySession); err != nil {
		fmt.Fprintf(stderr, "Warning: failed to register default PTY session: %v\n", err)
	}

	// Set up the reconnect handler now that we have the PTY session and card store.
	// This handler replays terminal history and pending cards when a client connects.
	wsServer.SetReconnectHandler(func(sendToClient func(server.Message)) {
		// Send session list (after session.status was already sent by the server).
		// This gives mobile clients the session history for context switching.
		sessions, err := store.ListSessions(20)
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to load sessions for reconnect: %v\n", err)
		} else if len(sessions) > 0 {
			// Convert storage.Session to server.SessionInfo
			sessionInfos := make([]server.SessionInfo, len(sessions))
			for i, s := range sessions {
				sessionInfos[i] = server.SessionInfo{
					ID:         s.ID,
					Repo:       s.Repo,
					Branch:     s.Branch,
					StartedAt:  s.StartedAt.UnixMilli(),
					LastSeen:   s.LastSeen.UnixMilli(),
					LastCommit: s.LastCommit,
					Status:     string(s.Status),
				}
			}
			sendToClient(server.NewSessionListMessage(sessionInfos))
		}

		// Replay terminal history from the ring buffer.
		// Each line is sent as a separate terminal.append message.
		historyLines := ptySession.Lines()
		for _, line := range historyLines {
			sendToClient(server.NewTerminalAppendMessage(wsServer.SessionID(), line))
		}

		// Validate cards before sending - remove stale ones for files that
		// were committed or staged externally while the client was disconnected.
		if err := cardStreamer.ValidateAndCleanStaleCards(); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to validate cards on reconnect: %v\n", err)
		}

		// Resend pending cards from storage.
		// This ensures reconnecting clients see all cards they haven't acted on.
		pendingCards, err := store.ListPending()
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to load pending cards for reconnect: %v\n", err)
		} else {
			for _, card := range pendingCards {
				// Detect if card is for a binary file based on stored placeholder content
				isBinary := card.Diff == diff.BinaryDiffPlaceholder

				var serverChunks []server.ChunkInfo
				var serverChunkGroups []server.ChunkGroupInfo
				var stats *server.DiffStats

				if !isBinary {
					// Parse chunk boundaries from stored diff content
					chunks := stream.ParseChunkInfoFromDiff(card.Diff)

					// Apply proximity-based grouping if enabled
					if fileCfg.ChunkGroupingEnabled && len(chunks) > 0 {
						proximity := fileCfg.ChunkGroupingProximity
						if proximity <= 0 {
							proximity = 20 // Default proximity
						}
						groups, groupedChunks := stream.GroupChunksByProximity(chunks, proximity)
						chunks = groupedChunks

						// Convert stream.ChunkGroupInfo to server.ChunkGroupInfo
						serverChunkGroups = make([]server.ChunkGroupInfo, len(groups))
						for i, g := range groups {
							serverChunkGroups[i] = server.ChunkGroupInfo{
								GroupIndex: g.GroupIndex,
								LineStart:  g.LineStart,
								LineEnd:    g.LineEnd,
								ChunkCount: g.ChunkCount,
							}
						}
					}

					// Reconcile chunks in storage to ensure per-chunk decisions work.
					// This handles missing or stale chunk rows from older sessions.
					cardStreamer.ReconcileChunksForCard(card.ID, chunks)

					// Convert stream.ChunkInfo to server.ChunkInfo
					serverChunks = make([]server.ChunkInfo, len(chunks))
					for i, h := range chunks {
						serverChunks[i] = server.ChunkInfo{
							Index:       h.Index,
							OldStart:    h.OldStart,
							OldCount:    h.OldCount,
							NewStart:    h.NewStart,
							NewCount:    h.NewCount,
							Offset:      h.Offset,
							Length:      h.Length,
							Content:     h.Content,
							ContentHash: h.ContentHash,
							GroupIndex:  h.GroupIndex,
						}
					}

					// Calculate diff stats for large file warnings
					diffStats := diff.CalculateDiffStats(card.Diff)
					stats = &server.DiffStats{
						ByteSize:     diffStats.ByteSize,
						LineCount:    diffStats.LineCount,
						AddedLines:   diffStats.AddedLines,
						DeletedLines: diffStats.DeletedLines,
					}
				}

				// Detect deletion from stats: no added lines but has deleted lines
				isDeleted := stats != nil && stats.AddedLines == 0 && stats.DeletedLines > 0

				sendToClient(server.NewDiffCardMessage(
					card.ID,
					card.File,
					card.Diff,
					serverChunks,
					serverChunkGroups,
					isBinary,
					isDeleted,
					stats,
					card.CreatedAt.UnixMilli(),
				))
			}
		}
	})

	fmt.Fprintln(stdout, "PTY session started. Press Ctrl+C to stop.")
	if cfg.NoTLS {
		fmt.Fprintf(stdout, "Connect to ws://%s/ws to receive terminal output.\n", addr)
	} else {
		fmt.Fprintf(stdout, "Connect to wss://%s/ws to receive terminal output.\n", addr)
	}
	fmt.Fprintln(stdout, "Review cards will be streamed as diffs are detected.")

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	select {
	case <-ptySession.Done():
		fmt.Fprintln(stdout, "\nPTY session exited.")
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "\nReceived signal %v, stopping...\n", sig)
		ptySession.Stop()
	}

	// Update session status to complete on graceful shutdown.
	if err := store.UpdateSessionStatus(wsServer.SessionID(), storage.SessionStatusComplete); err != nil {
		fmt.Fprintf(stderr, "Warning: failed to update session status: %v\n", err)
	}

	// Cleanup in reverse order of creation
	if mdnsAdvertiser != nil {
		mdnsAdvertiser.Stop()
	}
	sessionManager.CloseAll() // Close all managed sessions (Unit 9.5)
	cardStreamer.Stop()
	store.Close()
	wsServer.Stop()

	// Remove PID file
	if pidFilePath != "" {
		removePIDFile(pidFilePath, stderr)
	}

	// Close log file if we opened one
	if logFile != nil {
		logFile.Close()
	}

	// Print buffer summary
	lines := ptySession.Lines()
	fmt.Fprintf(stdout, "\nCaptured %d lines in ring buffer.\n", len(lines))
	return 0
}

func runHostStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("host status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", "127.0.0.1:7070", "Host address to query")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: pseudocoder host status [options]\n\nShow the current status of the host daemon.\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	// Query the running host for status
	status, err := queryHostStatus(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Display status in human-readable format
	fmt.Fprintf(stdout, "Host Status\n")
	fmt.Fprintf(stdout, "===========\n")
	fmt.Fprintf(stdout, "Listening:    %s\n", status.ListeningAddress)
	fmt.Fprintf(stdout, "TLS:          %v\n", status.TLSEnabled)
	fmt.Fprintf(stdout, "Auth:         %v\n", status.RequireAuth)
	fmt.Fprintf(stdout, "Clients:      %d connected\n", status.ConnectedClients)
	fmt.Fprintf(stdout, "Session:      %s\n", status.SessionID)
	fmt.Fprintf(stdout, "Repository:   %s\n", status.RepositoryPath)
	fmt.Fprintf(stdout, "Branch:       %s\n", status.CurrentBranch)
	fmt.Fprintf(stdout, "Uptime:       %s\n", formatUptime(status.UptimeSeconds))

	// Query and display sessions (Unit 9.5)
	sessions, err := queryHostSessions(*addr)
	if err == nil && len(sessions) > 0 {
		fmt.Fprintf(stdout, "\nSessions (%d):\n", len(sessions))
		for _, s := range sessions {
			displayName := s.Name
			if displayName == "" {
				displayName = s.ID
			}
			statusStr := "stopped"
			if s.Running {
				statusStr = "running"
			}
			fmt.Fprintf(stdout, "  - %s (%s, %s)\n", displayName, s.Command, statusStr)
		}
	}

	return 0
}

// SessionListItem is the response format for session list API.
// Defined here to avoid circular import with server package.
type SessionListItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Command   string    `json:"command"`
	CreatedAt time.Time `json:"created_at"`
	Running   bool      `json:"running"`
}

// queryHostSessions queries the running host for session list.
// Tries HTTPS first (default), falls back to HTTP for --no-tls mode.
func queryHostSessions(addr string) ([]SessionListItem, error) {
	// Try HTTPS first
	sessions, err := queryHostSessionsWithScheme("https", addr)
	if err == nil {
		return sessions, nil
	}

	// Fall back to HTTP
	return queryHostSessionsWithScheme("http", addr)
}

// queryHostSessionsWithScheme makes an HTTP GET request to the /api/session/list endpoint.
func queryHostSessionsWithScheme(scheme, addr string) ([]SessionListItem, error) {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	url := fmt.Sprintf("%s://%s/api/session/list", scheme, addr)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var sessions []SessionListItem
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return sessions, nil
}

// queryHostStatus queries the running host for status information.
// Tries HTTPS first (default), falls back to HTTP for --no-tls mode.
func queryHostStatus(addr string) (*server.StatusResponse, error) {
	// Try HTTPS first (most common case with TLS enabled)
	resp, err := queryHostStatusWithScheme("https", addr)
	if err == nil {
		return resp, nil
	}

	// Fall back to HTTP (for --no-tls development mode)
	resp, err = queryHostStatusWithScheme("http", addr)
	if err != nil {
		return nil, fmt.Errorf("host is not running at %s (or not reachable)", addr)
	}
	return resp, nil
}

// queryHostStatusWithScheme makes an HTTP GET request to the /status endpoint.
func queryHostStatusWithScheme(scheme, addr string) (*server.StatusResponse, error) {
	// Create HTTP client with short timeout and skip TLS verification
	// for self-signed certificates.
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	url := fmt.Sprintf("%s://%s/status", scheme, addr)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var status server.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &status, nil
}

// formatUptime formats an uptime in seconds as a human-readable string.
// Examples: "45s", "5m 23s", "2h 15m", "3d 4h"
func formatUptime(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", seconds)
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		secs := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// deviceStoreAdapter adapts storage.SQLiteStore to the server.DeviceStore interface.
// This allows the server package to access device storage without importing the storage package.
type deviceStoreAdapter struct {
	store *storage.SQLiteStore
}

func (a *deviceStoreAdapter) GetDevice(id string) (*server.DeviceInfo, error) {
	device, err := a.store.GetDevice(id)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, nil
	}
	return &server.DeviceInfo{
		ID:   device.ID,
		Name: device.Name,
	}, nil
}

func (a *deviceStoreAdapter) DeleteDevice(id string) error {
	return a.store.DeleteDevice(id)
}

// getCurrentBranch returns the current git branch for a repository.
// Returns "unknown" if the branch cannot be determined.
func getCurrentBranch(repoPath string) string {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// writePIDFile writes the current process ID to the specified file.
// Creates the parent directory if it doesn't exist.
func writePIDFile(path string) error {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create PID file directory: %w", err)
	}

	// Write PID to file with restrictive permissions
	pid := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(path, []byte(pid), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}
	return nil
}

// removePIDFile removes the PID file if it exists.
// Errors are logged but not returned (cleanup should not fail the shutdown).
func removePIDFile(path string, stderr io.Writer) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "Warning: failed to remove PID file: %v\n", err)
	}
}

// resolveLogFilePath returns the log file path, using the default if not specified.
// The default is ~/.pseudocoder/host.log.
func resolveLogFilePath(configPath string) (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pseudocoder", "host.log"), nil
}
