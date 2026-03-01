package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
	"github.com/pseudocoder/host/internal/ipc"
	"github.com/pseudocoder/host/internal/keepawake"
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
	PairSocket              string
}

type keepAwakeController interface {
	SetDesiredEnabled(ctx context.Context, enabled bool) keepawake.Status
	Close(ctx context.Context) error
}

var newKeepAwakeController = func() keepAwakeController {
	return keepawake.NewManager(keepawake.NewDefaultAdapter(), keepawake.Options{})
}

var probeKeepAwakeAuditWrite = func(store *storage.SQLiteStore) error {
	return store.ProbeKeepAwakeAuditWrite()
}

const keepAwakeTestEnableEnv = "PSEUDOCODER_KEEP_AWAKE_TEST_ENABLE"

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
	fs.StringVar(&cfg.PairSocket, "pair-socket", "", "Path to pairing IPC socket (default: ~/.pseudocoder/pair.sock)")

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
	if cfg.PairSocket == "" {
		cfg.PairSocket = fileCfg.PairSocket
	}
	if cfg.PairSocket == "" {
		defaultPairSocket, err := config.DefaultPairSocketPath()
		if err != nil {
			fmt.Fprintf(stderr, "Error: failed to determine pairing socket path: %v\n", err)
			return 1
		}
		cfg.PairSocket = defaultPairSocket
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

	// Phase 17 (P17U2): initialize keep-awake runtime manager.
	// Runtime-only boundary in this unit: no protocol wiring yet.
	keepAwakeManager := newKeepAwakeController()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := keepAwakeManager.Close(closeCtx); err != nil {
			fmt.Fprintf(stderr, "Warning: keep-awake cleanup failed: %v\n", err)
		}
	}()
	if os.Getenv(keepAwakeTestEnableEnv) == "1" {
		st := keepAwakeManager.SetDesiredEnabled(context.Background(), true)
		if st.State == keepawake.StateDegraded {
			fmt.Fprintf(stderr, "Warning: keep-awake test enable degraded: %s (%s)\n", st.Reason, st.LastError)
		}
	}

	// Open SQLite storage for review cards and devices.
	// This allows cards and device tokens to survive restarts.
	store, err := storage.NewSQLiteStore(tokenStorePath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to open storage: %v\n", err)
		return 1
	}

	// Phase 7: Open rollout metrics store (shared SQLite file) so rollout monitor,
	// flags API, and instrumented handlers can record and query rollout evidence.
	metricsStore, err := storage.NewSQLiteMetricsStore(tokenStorePath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to open rollout metrics store: %v\n", err)
		store.Close()
		return 1
	}
	defer metricsStore.Close()

	rolloutConfigPath := cfg.Config
	if rolloutConfigPath == "" {
		if p, err := config.DefaultConfigPath(); err == nil {
			if _, err := os.Stat(p); err == nil {
				rolloutConfigPath = p
			}
		}
	}
	loadRolloutConfig := func() (*config.Config, error) {
		return config.Load(rolloutConfigPath)
	}

	// Create the pairing manager for device authentication.
	// This handles pairing codes, token generation, and rate limiting.
	pairingManager := auth.NewPairingManager(auth.PairingConfig{
		DeviceStore: store,
	})

	// Start the pairing IPC server for local code generation.
	// This keeps code generation off loopback HTTP unless explicitly used.
	pairSocketLogger := log.New(stderr, "ipc: ", log.LstdFlags)
	pairSocketServer := ipc.NewPairSocketServer(cfg.PairSocket, auth.NewGenerateCodeHandler(pairingManager), pairSocketLogger)
	pairIPCRunning := false
	pairIPCCleanup := false
	stopPairIPC := func() {
		if !pairIPCCleanup {
			return
		}
		pairIPCCleanup = false
		if err := pairSocketServer.Stop(); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to stop pairing IPC: %v\n", err)
		}
	}
	if err := pairSocketServer.Start(); err != nil {
		if cfg.RequireAuth || cfg.Pair {
			fmt.Fprintf(stderr, "Error: failed to start pairing IPC: %v\n", err)
			store.Close()
			return 1
		}
		fmt.Fprintf(stderr, "Warning: pairing IPC unavailable (%v); pairing requires --require-auth\n", err)
	} else {
		pairIPCRunning = true
		pairIPCCleanup = true
		defer stopPairIPC()
	}

	// Create the token validator for WebSocket authentication.
	tokenValidator := auth.NewTokenValidator(store)

	// Create WebSocket server
	wsServer := server.NewServer(addr)
	wsServer.SetKeepAwakeRuntimeManager(keepAwakeManager)
	wsServer.SetMetricsStore(metricsStore)

	// Phase 7: monitor rollout metrics and auto-disable V2 flags on sustained
	// threshold breaches (enabled only when we have a persisted config path).
	var rolloutMonitor *server.RolloutMonitor
	if rolloutConfigPath != "" {
		rolloutMonitor = server.NewRolloutMonitor(metricsStore, rolloutConfigPath, loadRolloutConfig)
	}

	// P17U5: Wire durable audit adapter (replaces log-only keepAwakeAuditLogger).
	auditMaxRows := fileCfg.KeepAwakeAuditMaxRows
	if auditMaxRows == 0 {
		auditMaxRows = 1000
	}
	wsServer.SetKeepAwakeAuditWriter(server.NewKeepAwakeAuditStoreAdapter(store, auditMaxRows))

	// Fail-safe: if keep-awake audit storage is unavailable, force remote keep-awake disabled.
	keepAwakeAuditUnavailable := false
	if err := probeKeepAwakeAuditWrite(store); err != nil {
		keepAwakeAuditUnavailable = true
		wsServer.SetKeepAwakeMigrationFailed(true)
		fmt.Fprintf(stderr, "Warning: keep-awake audit storage unavailable, remote keep-awake disabled: %v\n", err)
	}

	// P17U5: Wire power provider.
	wsServer.SetKeepAwakePowerProvider(keepawake.NewDefaultPowerProvider())

	// P17U5: Wire keep-awake policy from config with defaults.
	wsServer.SetKeepAwakePolicy(server.KeepAwakePolicyConfig{
		RemoteEnabled:             fileCfg.KeepAwakeRemoteEnabled && !keepAwakeAuditUnavailable,
		AllowAdminRevoke:          fileCfg.KeepAwakeAllowAdminRevoke,
		AdminDeviceIDs:            config.NormalizeKeepAwakeAdminDeviceIDs(fileCfg.KeepAwakeAdminDeviceIDs),
		AllowOnBattery:            fileCfg.EffectiveKeepAwakeAllowOnBattery(),
		AutoDisableBatteryPercent: fileCfg.KeepAwakeAutoDisableBatteryPercent,
	})
	if err := wsServer.SetKeepAwakeDisconnectGrace(45 * time.Second); err != nil {
		fmt.Fprintf(stderr, "Error: invalid keep-awake disconnect grace: %v\n", err)
		store.Close()
		return 1
	}

	wsServer.SetSessionStore(store)

	// Create and save the current session for history tracking (Unit 6.3a).
	// This enables mobile clients to see session history and switch contexts.
	currentSession := &storage.Session{
		ID:        wsServer.SessionID(),
		Repo:      repoPath,
		Branch:    getCurrentBranch(repoPath),
		StartedAt: time.Now(),
		LastSeen:  time.Now(),
		Status:    storage.SessionStatusRunning,
		IsSystem:  true,
	}
	if err := store.SaveSession(currentSession); err != nil {
		// Log warning but don't fail - session history is nice-to-have
		fmt.Fprintf(stderr, "Warning: failed to save session: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "Session: %s (branch: %s)\n", currentSession.ID, currentSession.Branch)
	}
	if pairIPCRunning {
		fmt.Fprintf(stdout, "Pairing:  %s\n", cfg.PairSocket)
	} else {
		fmt.Fprintf(stdout, "Pairing:  disabled (IPC unavailable)\n")
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

	// Wire up pairing endpoints.
	pairHandler := auth.NewPairHandler(pairingManager)
	pairHandler.SetMetricsRecorder(func(deviceID string, success bool) {
		if err := metricsStore.RecordPairingAttempt(deviceID, success); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to record pairing metrics: %v\n", err)
		}
	})
	wsServer.SetPairHandler(pairHandler)
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

	// P18U2: Wire keep-awake policy HTTP endpoint.
	// Resolve the config path for persistence (cfg.Config may be empty = default).
	keepAwakeConfigPath := cfg.Config
	if keepAwakeConfigPath == "" {
		if p, err := config.DefaultConfigPath(); err == nil {
			if _, err := os.Stat(p); err == nil {
				keepAwakeConfigPath = p
			}
		}
	}
	keepAwakePolicyHandler := server.NewKeepAwakePolicyHandler(wsServer, approvalTokenManager, keepAwakeConfigPath)
	wsServer.SetKeepAwakePolicyHandler(keepAwakePolicyHandler)

	// Phase 7: wire /api/flags endpoint for rollout flag management.
	// Endpoint is enabled only when a persisted config path is available.
	var flagsAPIHandler *server.FlagsAPIHandler
	if rolloutConfigPath != "" {
		flagsAPIHandler = server.NewFlagsAPIHandler(rolloutConfigPath, loadRolloutConfig, metricsStore, rolloutMonitor)
		flagsAPIHandler.SetOnChanged(func(payload server.ServerFlagsPayload) {
			wsServer.BroadcastFlags(payload)
		})
		wsServer.SetFlagsAPIHandler(flagsAPIHandler)
	} else {
		fmt.Fprintf(stderr, "Warning: rollout flags API disabled (no writable config path found)\n")
	}

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
	pairSocket := ""
	if pairIPCRunning {
		pairSocket = cfg.PairSocket
	}
	statusHandler := server.NewStatusHandler(wsServer, repoPath, currentSession.Branch, !cfg.NoTLS, cfg.RequireAuth, pairSocket)
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
		stopPairIPC()
		if rolloutMonitor != nil {
			rolloutMonitor.Stop()
		}
		store.Close()
		return 1
	}

	if rolloutMonitor != nil {
		rolloutMonitor.Start()
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

	wsServer.SetChunkUndoHandler(func(cardID string, chunkIndex int, contentHash string, confirmed bool) (*server.UndoResult, error) {
		chunk, card, err := actionProcessor.ProcessChunkUndo(cardID, chunkIndex, contentHash, confirmed)
		if err != nil {
			return nil, err
		}
		return &server.UndoResult{
			CardID:       card.ID,
			ChunkIndex:   chunk.ChunkIndex,
			ContentHash:  chunk.ContentHash, // Return for client to clear pending state
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

	// Phase 3: Wire file operations for file explorer.
	fileOps := server.NewFileOperations(repoPath, 1048576) // 1MB cap
	wsServer.SetFileOperations(fileOps)

	// Phase 3 (P3U3): File watch poller for external filesystem changes.
	fileWatchPoller := server.NewFileWatchPoller(server.FileWatchPollerConfig{
		RepoPath:     repoPath,
		PollInterval: 2 * time.Second,
		OnEvents: func(events []server.FileWatchEvent) {
			for _, ev := range events {
				wsServer.BroadcastFileWatch(ev.Path, ev.Change, "")
			}
		},
		OnError: func(err error) {
			fmt.Fprintf(stderr, "File watch poll error: %v\n", err)
		},
	})

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
		stopPairIPC()
		store.Close()
		wsServer.Stop()
		return 1
	}

	fileWatchPoller.Start()

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
		fileWatchPoller.Stop()
		cardStreamer.Stop()
		stopPairIPC()
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
			// Send current rollout flags so clients start with host-authoritative
			// feature-gating state before processing subsequent updates.
			if flagsAPIHandler != nil {
				payload, err := flagsAPIHandler.CurrentPayload()
				if err != nil {
					fmt.Fprintf(stderr, "Warning: failed to load rollout flags for reconnect: %v\n", err)
				} else {
					sendToClient(server.NewServerFlagsMessage(payload))
				}
			}

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
					IsSystem:   s.IsSystem,
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
				sendToClient(buildReconnectDiffCardMessage(
					card,
					fileCfg.ChunkGroupingEnabled,
					fileCfg.ChunkGroupingProximity,
					cardStreamer.ReconcileChunksForCard,
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
	fileWatchPoller.Stop()
	if rolloutMonitor != nil {
		rolloutMonitor.Stop()
	}
	sessionManager.CloseAll() // Close all managed sessions (Unit 9.5)
	cardStreamer.Stop()
	store.Close()
	wsServer.Stop()
	stopPairIPC()

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

// buildReconnectDiffCardMessage rebuilds one pending card into a diff.card message
// for reconnect replay, including semantic enrichment and summary metadata.
// The reconcile callback is optional and is used to refresh chunk rows in storage.
func buildReconnectDiffCardMessage(
	card *storage.ReviewCard,
	chunkGroupingEnabled bool,
	chunkGroupingProximity int,
	reconcile func(cardID string, chunks []stream.ChunkInfo),
) server.Message {
	isBinary := card.Diff == diff.BinaryDiffPlaceholder

	var serverChunks []server.ChunkInfo
	var serverChunkGroups []server.ChunkGroupInfo
	var serverSemanticGroups []server.SemanticGroupInfo
	var stats *server.DiffStats

	if !isBinary {
		chunks := stream.ParseChunkInfoFromDiff(card.Diff)

		if chunkGroupingEnabled && len(chunks) > 0 {
			proximity := chunkGroupingProximity
			if proximity <= 0 {
				proximity = 20
			}
			groups, groupedChunks := stream.GroupChunksByProximity(chunks, proximity)
			chunks = groupedChunks
			serverChunkGroups = mapStreamChunkGroupsToServer(groups)
		}

		if reconcile != nil {
			reconcile(card.ID, chunks)
		}

		diffStats := diff.CalculateDiffStats(card.Diff)
		stats = &server.DiffStats{
			ByteSize:     diffStats.ByteSize,
			LineCount:    diffStats.LineCount,
			AddedLines:   diffStats.AddedLines,
			DeletedLines: diffStats.DeletedLines,
		}

		// Deleted cards are represented by zero added lines and at least one deletion.
		isDeletedLocal := stats.AddedLines == 0 && stats.DeletedLines > 0
		enrichedChunks, semGroups := stream.EnrichChunksWithSemantics(card.ID, card.File, chunks, false, isDeletedLocal)

		serverChunks = mapStreamChunksToServer(enrichedChunks)
		serverSemanticGroups = mapStreamSemanticGroupsToServer(semGroups)
	} else {
		// Binary cards have no chunk list, but still need semantic_groups parity.
		_, semGroups := stream.EnrichChunksWithSemantics(card.ID, card.File, nil, true, false)
		serverSemanticGroups = mapStreamSemanticGroupsToServer(semGroups)
	}

	isDeleted := stats != nil && stats.AddedLines == 0 && stats.DeletedLines > 0
	return server.NewDiffCardMessage(
		card.ID,
		card.File,
		card.Diff,
		serverChunks,
		serverChunkGroups,
		serverSemanticGroups,
		isBinary,
		isDeleted,
		stats,
		card.CreatedAt.UnixMilli(),
	)
}

func mapStreamChunksToServer(chunks []stream.ChunkInfo) []server.ChunkInfo {
	if len(chunks) == 0 {
		return nil
	}
	serverChunks := make([]server.ChunkInfo, len(chunks))
	for i, h := range chunks {
		serverChunks[i] = server.ChunkInfo{
			Index:           h.Index,
			OldStart:        h.OldStart,
			OldCount:        h.OldCount,
			NewStart:        h.NewStart,
			NewCount:        h.NewCount,
			Offset:          h.Offset,
			Length:          h.Length,
			Content:         h.Content,
			ContentHash:     h.ContentHash,
			GroupIndex:      h.GroupIndex,
			SemanticKind:    h.SemanticKind,
			SemanticLabel:   h.SemanticLabel,
			SemanticGroupID: h.SemanticGroupID,
		}
	}
	return serverChunks
}

func mapStreamChunkGroupsToServer(groups []stream.ChunkGroupInfo) []server.ChunkGroupInfo {
	if len(groups) == 0 {
		return nil
	}
	serverGroups := make([]server.ChunkGroupInfo, len(groups))
	for i, g := range groups {
		serverGroups[i] = server.ChunkGroupInfo{
			GroupIndex: g.GroupIndex,
			LineStart:  g.LineStart,
			LineEnd:    g.LineEnd,
			ChunkCount: g.ChunkCount,
		}
	}
	return serverGroups
}

func mapStreamSemanticGroupsToServer(groups []stream.SemanticGroupInfo) []server.SemanticGroupInfo {
	if len(groups) == 0 {
		return nil
	}
	serverGroups := make([]server.SemanticGroupInfo, len(groups))
	for i, g := range groups {
		var idxs []int
		if g.ChunkIndexes != nil {
			idxs = make([]int, len(g.ChunkIndexes))
			copy(idxs, g.ChunkIndexes)
		}
		serverGroups[i] = server.SemanticGroupInfo{
			GroupID:      g.GroupID,
			Label:        g.Label,
			Kind:         g.Kind,
			LineStart:    g.LineStart,
			LineEnd:      g.LineEnd,
			ChunkIndexes: idxs,
			RiskLevel:    g.RiskLevel,
		}
	}
	return serverGroups
}

func runHostStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("host status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", "", "Host address to query (default: localhost, then Tailscale/LAN)")
	port := fs.Int("port", 7070, "Port to query when auto-selecting address")

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

	explicitFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if err := validatePort(*port); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	addrs := resolveAddrCandidates(*addr, *port, explicitFlags["port"], stderr)

	// Query the running host for status
	var status *server.StatusResponse
	var err error
	for _, target := range addrs {
		status, err = queryHostStatus(target)
		if err == nil {
			break
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	writeHostStatusOutput(stdout, status)

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

// writeHostStatusOutput renders human-readable host status output.
func writeHostStatusOutput(stdout io.Writer, status *server.StatusResponse) {
	fmt.Fprintf(stdout, "Host Status\n")
	fmt.Fprintf(stdout, "===========\n")
	fmt.Fprintf(stdout, "Listening:    %s\n", status.ListeningAddress)
	fmt.Fprintf(stdout, "TLS:          %v\n", status.TLSEnabled)
	fmt.Fprintf(stdout, "Auth:         %v\n", status.RequireAuth)
	if status.PairSocketPath != "" {
		fmt.Fprintf(stdout, "Pairing:      %s\n", status.PairSocketPath)
	} else {
		fmt.Fprintf(stdout, "Pairing:      disabled (IPC unavailable)\n")
	}
	fmt.Fprintf(stdout, "Clients:      %d connected\n", status.ConnectedClients)
	fmt.Fprintf(stdout, "Session:      %s\n", status.SessionID)
	fmt.Fprintf(stdout, "Repository:   %s\n", status.RepositoryPath)
	fmt.Fprintf(stdout, "Branch:       %s\n", status.CurrentBranch)
	fmt.Fprintf(stdout, "Uptime:       %s\n", formatUptime(status.UptimeSeconds))

	if status.KeepAwake != nil {
		ka := status.KeepAwake
		fmt.Fprintf(stdout, "\nKeep-Awake\n")
		fmt.Fprintf(stdout, "----------\n")
		fmt.Fprintf(stdout, "State:        %s\n", ka.State)
		fmt.Fprintf(stdout, "Remote:       %v\n", ka.RemoteEnabled)
		fmt.Fprintf(stdout, "Admin Revoke: %v\n", ka.AllowAdminRevoke)
		fmt.Fprintf(stdout, "Battery:      allow=%v", ka.AllowOnBattery)
		if ka.AutoDisableBatteryPercent > 0 {
			fmt.Fprintf(stdout, " threshold=%d%%", ka.AutoDisableBatteryPercent)
		}
		fmt.Fprintf(stdout, "\n")
		if ka.OnBattery != nil {
			fmt.Fprintf(stdout, "Power:        on_battery=%v", *ka.OnBattery)
			if ka.BatteryPercent != nil {
				fmt.Fprintf(stdout, " battery=%d%%", *ka.BatteryPercent)
			}
			fmt.Fprintf(stdout, "\n")
		}
		if ka.PolicyBlocked {
			fmt.Fprintf(stdout, "Policy:       blocked (%s)\n", ka.PolicyReason)
		}
		fmt.Fprintf(stdout, "Leases:       %d active\n", ka.ActiveLeaseCount)
		if ka.NextExpiryMs > 0 {
			remainingMs := ka.NextExpiryMs - time.Now().UnixMilli()
			if remainingMs < 0 {
				remainingMs = 0
			}
			fmt.Fprintf(stdout, "Next Expiry:  %s\n", formatUptime(remainingMs/1000))
		}
		if ka.DegradedReason != "" {
			fmt.Fprintf(stdout, "Degraded:     %s\n", ka.DegradedReason)
		}
		if ka.RecoveryHint != "" {
			fmt.Fprintf(stdout, "Hint:         %s\n", ka.RecoveryHint)
		}
	}
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
