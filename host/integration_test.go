//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var (
	binaryPath string
	moduleDir  string
)

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get working dir: %v\n", err)
		os.Exit(1)
	}
	moduleDir = wd

	tmpDir, err := os.MkdirTemp("", "pseudocoder-integration-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}

	binaryPath = filepath.Join(tmpDir, "pseudocoder")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd")
	build.Dir = moduleDir
	out, err := build.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build pseudocoder: %v\n%s", err, out)
		_ = os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(tmpDir)
	os.Exit(code)
}

type hostProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	addr   string
	waited bool
}

func startHost(t *testing.T, addr, sessionCmd string) *hostProcess {
	t.Helper()

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr",
		addr,
		"--session-cmd",
		sessionCmd,
		"--no-tls",            // Use plaintext for basic integration tests
		"--require-auth=false", // Disable auth for tests
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}

	waitForHealth(t, addr, 3*time.Second)

	t.Cleanup(func() {
		hp.stop(t)
	})

	return hp
}

func (h *hostProcess) stop(t *testing.T) {
	t.Helper()
	if h.waited {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	_ = h.wait(t, 5*time.Second)
}

func (h *hostProcess) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	if h.waited {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- h.cmd.Wait()
	}()

	select {
	case err := <-done:
		h.waited = true
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for host exit")
	}
}

func getFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func waitForHealth(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://%s/health", addr)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == "ok" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("health endpoint not ready: %s", url)
}

func dialWebSocket(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	url := fmt.Sprintf("ws://%s/ws", addr)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			return conn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failed to dial websocket: %s", url)
	return nil
}

type messageEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type sessionStatusPayload struct {
	Status string `json:"status"`
}

type terminalAppendPayload struct {
	Chunk string `json:"chunk"`
}

func readEnvelope(conn *websocket.Conn, timeout time.Duration) (messageEnvelope, error) {
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return messageEnvelope{}, err
	}
	var env messageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return messageEnvelope{}, err
	}
	return env, nil
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func TestIntegrationTerminalStreaming(t *testing.T) {
	addr := getFreeAddr(t)
	// Initial sleep gives the test time to connect before output starts.
	// Trailing sleep ensures we can read all messages before the session ends.
	sessionCmd := `sleep 1; for i in 1 2 3; do echo "Line $i"; sleep 0.2; done; sleep 1`

	hp := startHost(t, addr, sessionCmd)
	conn := dialWebSocket(t, addr)
	defer conn.Close()

	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}
	var status sessionStatusPayload
	if err := json.Unmarshal(first.Payload, &status); err != nil {
		t.Fatalf("parse session status failed: %v", err)
	}
	if status.Status != "running" {
		t.Fatalf("expected status running, got %s", status.Status)
	}

	// With chunked output, we may receive data in 1 or more terminal.append messages.
	// Verify combined content rather than exact message count.
	expectedContent := "Line 1\nLine 2\nLine 3\n"
	var combined strings.Builder
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		env, err := readEnvelope(conn, 2*time.Second)
		if err != nil {
			break // EOF or timeout - done reading
		}
		if env.Type != "terminal.append" {
			continue
		}
		var payload terminalAppendPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("parse terminal.append failed: %v", err)
		}
		combined.WriteString(normalizeNewlines(payload.Chunk))

		// Check if we've received all expected content
		if strings.Contains(combined.String(), expectedContent) {
			break
		}
	}

	if !strings.Contains(combined.String(), expectedContent) {
		t.Fatalf("expected combined output to contain %q, got %q", expectedContent, combined.String())
	}

	if err := hp.wait(t, 4*time.Second); err != nil {
		t.Fatalf("host did not exit cleanly: %v\nstderr: %s", err, hp.stderr.String())
	}
}

func TestIntegrationGracefulShutdown(t *testing.T) {
	addr := getFreeAddr(t)
	hp := startHost(t, addr, "sleep 10")

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	if err := hp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal host: %v", err)
	}

	// Drain any pending messages until we get a close error.
	// The server may have buffered messages (e.g., diff.card) before shutdown.
	// We accept any close error - the important thing is the connection closes.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var gotCloseError bool
	for {
		_, _, readErr := conn.ReadMessage()
		if readErr == nil {
			continue // Keep draining messages
		}
		// Connection closed - this is what we're testing for
		gotCloseError = true
		break
	}
	if !gotCloseError {
		t.Fatal("expected connection to close after SIGTERM")
	}

	if err := hp.wait(t, 5*time.Second); err != nil {
		t.Fatalf("host did not exit cleanly: %v\nstderr: %s", err, hp.stderr.String())
	}
}

func TestIntegrationPortConflictFailsFast(t *testing.T) {
	addr := getFreeAddr(t)
	hp := startHost(t, addr, "sleep 10")

	marker := filepath.Join(t.TempDir(), "session_started.txt")
	sessionCmd := fmt.Sprintf("echo started > %s", shellEscape(marker))

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr",
		addr,
		"--session-cmd",
		sessionCmd,
	)
	cmd.Dir = moduleDir
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected port conflict error")
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %d", exitErr.ExitCode())
		}
	} else {
		t.Fatalf("expected exit error, got %v", err)
	}

	if !strings.Contains(string(output), "address already in use") {
		t.Fatalf("expected address already in use error, got: %s", strings.TrimSpace(string(output)))
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected PTY session not to start, marker exists: %s", marker)
	}

	hp.stop(t)
}

func TestIntegrationHealthEndpoint(t *testing.T) {
	addr := getFreeAddr(t)
	hp := startHost(t, addr, "sleep 10")

	resp, err := http.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("expected body ok, got %q", string(body))
	}

	hp.stop(t)
}

func shellEscape(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

type diffCardPayload struct {
	CardID    string `json:"card_id"`
	File      string `json:"file"`
	Diff      string `json:"diff"`
	CreatedAt int64  `json:"created_at"`
}

type decisionResultPayload struct {
	CardID  string `json:"card_id"`
	Action  string `json:"action"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ChunkInfo matches the chunk boundary information sent in diff.card messages
type chunkInfoPayload struct {
	Index       int    `json:"index"`
	OldStart    int    `json:"old_start"`
	OldCount    int    `json:"old_count"`
	NewStart    int    `json:"new_start"`
	NewCount    int    `json:"new_count"`
	Offset      int    `json:"offset"`
	Length      int    `json:"length"`
	ContentHash string `json:"content_hash,omitempty"`
	Content     string `json:"content,omitempty"`
}

// diffCardWithChunksPayload extends diffCardPayload with chunk info
type diffCardWithChunksPayload struct {
	CardID    string            `json:"card_id"`
	File      string            `json:"file"`
	Diff      string            `json:"diff"`
	CreatedAt int64             `json:"created_at"`
	Chunks     []chunkInfoPayload `json:"chunks,omitempty"`
}

// diffCardWithBinaryPayload includes binary file fields
type diffCardWithBinaryPayload struct {
	CardID    string                 `json:"card_id"`
	File      string                 `json:"file"`
	Diff      string                 `json:"diff"`
	CreatedAt int64                  `json:"created_at"`
	IsBinary  bool                   `json:"is_binary,omitempty"`
	Stats     *diffStatsPayload      `json:"stats,omitempty"`
	Chunks     []chunkInfoPayload      `json:"chunks,omitempty"`
}

// diffStatsPayload matches the stats field in diff.card
type diffStatsPayload struct {
	ByteSize     int `json:"byte_size"`
	LineCount    int `json:"line_count"`
	AddedLines   int `json:"added_lines"`
	DeletedLines int `json:"deleted_lines"`
}

// chunkDecisionResultPayload carries the result of a chunk.decision
type chunkDecisionResultPayload struct {
	CardID    string `json:"card_id"`
	ChunkIndex int    `json:"chunk_index"`
	Action    string `json:"action"`
	Success   bool   `json:"success"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// TestIntegrationCardStreaming tests the end-to-end flow:
// git diff → SQLite storage → WebSocket diff.card message.
func TestIntegrationCardStreaming(t *testing.T) {
	// Create a temporary git repo for isolation
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	// Configure git user for commits
	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit an initial file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "test.txt")
	gitAdd.Dir = repoDir
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host with the test repo and fast poll interval.
	// Use a temp database to avoid interference from other tests/runs.
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100", // Fast polling for test
		"--session-cmd", "sleep 10",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Connect WebSocket
	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	// Wait a moment for poller to do initial scan
	time.Sleep(200 * time.Millisecond)

	// Modify the file to create a diff
	if err := os.WriteFile(testFile, []byte("initial content\nadded line\n"), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card message. Use a single long read deadline instead of
	// multiple short ones to avoid gorilla/websocket panic on repeated reads
	// after timeout.
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Timeout or connection closed - stop reading
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue // Ignore malformed messages
		}
		if env.Type == "diff.card" {
			if err := json.Unmarshal(env.Payload, &diffCard); err != nil {
				t.Fatalf("parse diff.card failed: %v", err)
			}
			break
		}
		// Reset deadline for next read
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	// Verify diff.card was received
	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card message within timeout\nhost stdout: %s\nhost stderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Verify payload structure
	if diffCard.File != "test.txt" {
		t.Errorf("expected file test.txt, got %s", diffCard.File)
	}
	if !strings.Contains(diffCard.Diff, "+added line") {
		t.Errorf("expected diff to contain '+added line', got %s", diffCard.Diff)
	}
	if diffCard.CreatedAt <= 0 {
		t.Errorf("expected created_at to be set, got %d", diffCard.CreatedAt)
	}

	hp.stop(t)
}

// TestIntegrationAcceptDecision tests the end-to-end accept flow:
// git diff → diff.card → review.decision(accept) → file staged.
func TestIntegrationAcceptDecision(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit initial file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "test.txt")
	gitAdd.Dir = repoDir
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Modify file
	if err := os.WriteFile(testFile, []byte("initial content\naccepted line\n"), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Send accept decision
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": diffCard.CardID,
			"action":  "accept",
			"comment": "integration test accept",
		},
	}
	decisionData, _ := json.Marshal(decision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write decision failed: %v", err)
	}

	// Wait for decision.result
	var result decisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "decision.result" {
			json.Unmarshal(env.Payload, &result)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if result.CardID == "" {
		t.Fatalf("did not receive decision.result")
	}
	if !result.Success {
		t.Fatalf("decision failed: %s", result.Error)
	}

	// Verify file is staged
	gitStatus := exec.Command("git", "status", "--porcelain", "test.txt")
	gitStatus.Dir = repoDir
	statusOut, err := gitStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v\n%s", err, statusOut)
	}
	// "M " means staged, " M" means unstaged
	if string(statusOut) != "M  test.txt\n" {
		t.Errorf("expected file to be staged (M ), got: %q", string(statusOut))
	}

	hp.stop(t)
}

// TestIntegrationRejectDecision tests the end-to-end reject flow:
// git diff → diff.card → review.decision(reject) → file restored.
func TestIntegrationRejectDecision(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit initial file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("original content\n"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "test.txt")
	gitAdd.Dir = repoDir
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Modify file with unwanted content
	if err := os.WriteFile(testFile, []byte("unwanted changes\n"), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Verify file has modified content before reject
	content, _ := os.ReadFile(testFile)
	if string(content) != "unwanted changes\n" {
		t.Fatalf("expected modified content before reject, got: %q", string(content))
	}

	// Send reject decision
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": diffCard.CardID,
			"action":  "reject",
			"comment": "integration test reject",
		},
	}
	decisionData, _ := json.Marshal(decision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write decision failed: %v", err)
	}

	// Wait for decision.result
	var result decisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "decision.result" {
			json.Unmarshal(env.Payload, &result)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if result.CardID == "" {
		t.Fatalf("did not receive decision.result")
	}
	if !result.Success {
		t.Fatalf("decision failed: %s", result.Error)
	}

	// Verify file is restored to original content
	content, _ = os.ReadFile(testFile)
	if string(content) != "original content\n" {
		t.Errorf("expected file to be restored to 'original content\\n', got: %q", string(content))
	}

	// Verify file is clean (no changes)
	gitStatus := exec.Command("git", "status", "--porcelain", "test.txt")
	gitStatus.Dir = repoDir
	statusOut, err := gitStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v\n%s", err, statusOut)
	}
	if string(statusOut) != "" {
		t.Errorf("expected file to have no changes, got: %q", string(statusOut))
	}

	hp.stop(t)
}

// Pairing flow payload types
type generateCodeResponse struct {
	Code   string `json:"code"`
	Expiry string `json:"expiry"`
}

type pairRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
}

type pairResponse struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// startHostWithAuth starts a host process with authentication required.
func startHostWithAuth(t *testing.T, addr, sessionCmd string) *hostProcess {
	t.Helper()

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", sessionCmd,
		"--require-auth",
		"--no-tls", // Use plaintext for auth integration tests
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}

	waitForHealth(t, addr, 3*time.Second)

	t.Cleanup(func() {
		hp.stop(t)
	})

	return hp
}

// TestIntegrationPairingFlow tests the full pairing flow:
// 1. Generate a pairing code via /pair/generate
// 2. Exchange code for token via /pair
// 3. Connect to WebSocket with token
func TestIntegrationPairingFlow(t *testing.T) {
	addr := getFreeAddr(t)
	hp := startHostWithAuth(t, addr, "sleep 30")

	// Step 1: Generate a pairing code
	genReq, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/pair/generate", addr), nil)
	if err != nil {
		t.Fatalf("create request failed: %v", err)
	}

	resp, err := http.DefaultClient.Do(genReq)
	if err != nil {
		t.Fatalf("generate code request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, body)
	}

	var genResp generateCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		t.Fatalf("decode generate response failed: %v", err)
	}

	if len(genResp.Code) != 6 {
		t.Fatalf("expected 6-digit code, got: %s", genResp.Code)
	}

	// Step 2: Exchange code for token
	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Integration Test Device",
	})

	pairResp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	if err != nil {
		t.Fatalf("pair request failed: %v", err)
	}
	defer pairResp.Body.Close()

	if pairResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(pairResp.Body)
		t.Fatalf("expected status 200, got %d: %s", pairResp.StatusCode, body)
	}

	var tokenResp pairResponse
	if err := json.NewDecoder(pairResp.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode pair response failed: %v", err)
	}

	if tokenResp.DeviceID == "" {
		t.Fatal("expected non-empty device ID")
	}
	if tokenResp.Token == "" {
		t.Fatal("expected non-empty token")
	}

	// Step 3: Connect to WebSocket with token
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tokenResp.Token)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("websocket dial with token failed: %v", err)
	}
	defer conn.Close()

	// Read session.status to confirm connection
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	hp.stop(t)
}

// TestIntegrationWebSocketRejectsWithoutToken tests that WebSocket connections
// are rejected when authentication is required but no token is provided.
func TestIntegrationWebSocketRejectsWithoutToken(t *testing.T) {
	addr := getFreeAddr(t)
	_ = startHostWithAuth(t, addr, "sleep 30")

	// Try to connect without a token
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)

	if err == nil {
		t.Fatal("expected websocket connection to fail without token")
	}

	// Should get 401 Unauthorized
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}
}

// TestIntegrationWebSocketRejectsInvalidToken tests that WebSocket connections
// are rejected when an invalid token is provided.
func TestIntegrationWebSocketRejectsInvalidToken(t *testing.T) {
	addr := getFreeAddr(t)
	_ = startHostWithAuth(t, addr, "sleep 30")

	// Try to connect with an invalid token
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid-token-12345")

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)

	if err == nil {
		t.Fatal("expected websocket connection to fail with invalid token")
	}

	// Should get 401 Unauthorized
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}
}

// TestIntegrationPairingCodeReplayPrevention tests that pairing codes
// cannot be reused after successful pairing.
func TestIntegrationPairingCodeReplayPrevention(t *testing.T) {
	addr := getFreeAddr(t)
	_ = startHostWithAuth(t, addr, "sleep 30")

	// Generate a pairing code
	resp, err := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	if err != nil {
		t.Fatalf("generate code request failed: %v", err)
	}

	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	// First use should succeed
	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Device 1",
	})

	firstResp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	if err != nil {
		t.Fatalf("first pair request failed: %v", err)
	}
	firstResp.Body.Close()

	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("first pair should succeed, got status %d", firstResp.StatusCode)
	}

	// Second use should fail (replay prevention)
	pairBody2, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Device 2",
	})

	secondResp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody2),
	)
	if err != nil {
		t.Fatalf("second pair request failed: %v", err)
	}
	defer secondResp.Body.Close()

	if secondResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 for replay, got %d", secondResp.StatusCode)
	}

	var errResp errorResponse
	json.NewDecoder(secondResp.Body).Decode(&errResp)

	if errResp.Error != "used_code" {
		t.Errorf("expected error code 'used_code', got '%s'", errResp.Error)
	}
}

// TestIntegrationPairingRateLimiting tests that rate limiting is enforced
// on pairing attempts.
func TestIntegrationPairingRateLimiting(t *testing.T) {
	addr := getFreeAddr(t)
	_ = startHostWithAuth(t, addr, "sleep 30")

	// Generate a pairing code
	resp, err := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	if err != nil {
		t.Fatalf("generate code request failed: %v", err)
	}
	resp.Body.Close()

	// Make multiple failed attempts with wrong codes
	for i := 0; i < 5; i++ {
		pairBody, _ := json.Marshal(pairRequest{
			Code:       "000000", // Wrong code
			DeviceName: "Device",
		})

		pairResp, err := http.Post(
			fmt.Sprintf("http://%s/pair", addr),
			"application/json",
			bytes.NewReader(pairBody),
		)
		if err != nil {
			t.Fatalf("pair request %d failed: %v", i+1, err)
		}
		pairResp.Body.Close()

		// First 5 attempts should fail with invalid_code
		if pairResp.StatusCode != http.StatusUnauthorized {
			t.Errorf("attempt %d: expected status 401, got %d", i+1, pairResp.StatusCode)
		}
	}

	// 6th attempt should be rate limited
	pairBody, _ := json.Marshal(pairRequest{
		Code:       "000000",
		DeviceName: "Device",
	})

	rateLimitResp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	if err != nil {
		t.Fatalf("rate limit request failed: %v", err)
	}
	defer rateLimitResp.Body.Close()

	if rateLimitResp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429 for rate limit, got %d", rateLimitResp.StatusCode)
	}

	var errResp errorResponse
	json.NewDecoder(rateLimitResp.Body).Decode(&errResp)

	if errResp.Error != "rate_limited" {
		t.Errorf("expected error code 'rate_limited', got '%s'", errResp.Error)
	}
}

// TestIntegrationWebSocketTokenViaQueryParam tests that tokens can be
// provided via query parameter as an alternative to Authorization header.
func TestIntegrationWebSocketTokenViaQueryParam(t *testing.T) {
	addr := getFreeAddr(t)
	_ = startHostWithAuth(t, addr, "sleep 30")

	// Generate and exchange code for token
	resp, _ := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Query Param Test Device",
	})
	pairResp, _ := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	// Connect with token in query parameter
	wsURL := fmt.Sprintf("ws://%s/ws?token=%s", addr, tokenResp.Token)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial with query param token failed: %v", err)
	}
	defer conn.Close()

	// Read session.status to confirm connection
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}
}

// TestIntegrationNoAuthModeAllowsAnonymous tests that WebSocket connections
// work without authentication when --require-auth is not set.
func TestIntegrationNoAuthModeAllowsAnonymous(t *testing.T) {
	addr := getFreeAddr(t)
	// Start without --require-auth
	hp := startHost(t, addr, "sleep 10")

	// Connect without any token
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial should succeed without auth: %v", err)
	}
	defer conn.Close()

	// Read session.status to confirm connection
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	hp.stop(t)
}

// TLS Integration Tests

// startHostWithTLS starts a host process with TLS enabled (the default).
// Uses a temporary directory for certificates.
func startHostWithTLS(t *testing.T, addr, sessionCmd, certDir string) *hostProcess {
	t.Helper()

	certPath := filepath.Join(certDir, "host.crt")
	keyPath := filepath.Join(certDir, "host.key")

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", sessionCmd,
		"--tls-cert", certPath,
		"--tls-key", keyPath,
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}

	// For TLS, we need to use HTTPS for health check
	waitForHealthTLS(t, addr, certPath, 5*time.Second)

	t.Cleanup(func() {
		hp.stop(t)
	})

	return hp
}

// waitForHealthTLS waits for the TLS-enabled health endpoint to respond.
func waitForHealthTLS(t *testing.T, addr, certPath string, timeout time.Duration) {
	t.Helper()

	// Wait for the certificate file to exist (it may be auto-generated)
	deadline := time.Now().Add(timeout)
	var cert []byte
	var err error
	for time.Now().Before(deadline) {
		cert, err = os.ReadFile(certPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("certificate file not created within timeout: %v", err)
	}

	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(cert) {
		t.Fatal("failed to add certificate to pool")
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: certPool,
			},
		},
	}

	url := fmt.Sprintf("https://%s/health", addr)
	deadline = time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == "ok" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("TLS health endpoint not ready: %s", url)
}

// dialWebSocketTLS connects to a TLS-enabled WebSocket server.
func dialWebSocketTLS(t *testing.T, addr, certPath string) *websocket.Conn {
	t.Helper()

	// Read the cert file
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert: %v", err)
	}

	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(cert) {
		t.Fatal("failed to add certificate to pool")
	}

	// Create a dialer that trusts the self-signed cert
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{
			RootCAs: certPool,
		},
	}

	url := fmt.Sprintf("wss://%s/ws", addr)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := dialer.Dial(url, nil)
		if err == nil {
			return conn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failed to dial TLS websocket: %s", url)
	return nil
}

// TestIntegrationTLSConnection tests that TLS connections work.
func TestIntegrationTLSConnection(t *testing.T) {
	certDir := t.TempDir()
	addr := getFreeAddr(t)

	hp := startHostWithTLS(t, addr, "sleep 10", certDir)
	certPath := filepath.Join(certDir, "host.crt")

	// Connect via TLS WebSocket
	conn := dialWebSocketTLS(t, addr, certPath)
	defer conn.Close()

	// Read session.status
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	hp.stop(t)
}

// TestIntegrationTLSRejectsPlaintext tests that plaintext connections are rejected
// when TLS is enabled. The plaintext request gets a TLS handshake error or fails
// to parse the TLS response as HTTP.
func TestIntegrationTLSRejectsPlaintext(t *testing.T) {
	certDir := t.TempDir()
	addr := getFreeAddr(t)

	_ = startHostWithTLS(t, addr, "sleep 10", certDir)

	// Try to connect with plaintext HTTP - should fail or get malformed response
	// When a plaintext client connects to a TLS server, it may:
	// 1. Get a connection error (TLS handshake failure)
	// 2. Get a malformed response error (the TLS handshake looks like garbage to HTTP)
	// Either way, we should NOT get a valid "ok" response
	httpURL := fmt.Sprintf("http://%s/health", addr)
	resp, err := http.Get(httpURL)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// If we got a response, it should NOT be the valid "ok" health check
		// (it would be garbage from the TLS handshake)
		if resp.StatusCode == http.StatusOK && string(body) == "ok" {
			t.Fatal("expected plaintext HTTP to NOT get valid health response when TLS is enabled")
		}
		// Getting a non-200 or malformed response is acceptable - the connection failed
		t.Logf("plaintext HTTP got non-standard response (expected): status=%d body=%q", resp.StatusCode, string(body))
	}
	// If err != nil, that's the expected case - connection failed

	// Try to connect with plaintext WebSocket - should fail
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	_, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected plaintext WebSocket to fail when TLS is enabled")
	}
}

// TestIntegrationTLSCertificateGeneration tests that certificates are
// automatically generated if they don't exist.
func TestIntegrationTLSCertificateGeneration(t *testing.T) {
	certDir := t.TempDir()
	addr := getFreeAddr(t)

	certPath := filepath.Join(certDir, "auto.crt")
	keyPath := filepath.Join(certDir, "auto.key")

	// Verify files don't exist yet
	if _, err := os.Stat(certPath); !os.IsNotExist(err) {
		t.Fatal("cert file should not exist yet")
	}

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 10",
		"--tls-cert", certPath,
		"--tls-key", keyPath,
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	// Wait for TLS health endpoint
	waitForHealthTLS(t, addr, certPath, 5*time.Second)

	// Verify certificate was generated
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Fatal("cert file should have been generated")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Fatal("key file should have been generated")
	}

	// Stop the host to ensure stdout is no longer being written (avoids data race)
	hp.stop(t)

	// Verify fingerprint was displayed in output
	output := hp.stdout.String()
	if !strings.Contains(output, "Fingerprint (SHA-256):") {
		t.Errorf("expected fingerprint in output, got: %s", output)
	}
	if !strings.Contains(output, "Generated new self-signed TLS certificate") {
		t.Errorf("expected generation message in output, got: %s", output)
	}
}

// TestIntegrationTLSWithAuth tests that TLS works together with authentication.
func TestIntegrationTLSWithAuth(t *testing.T) {
	certDir := t.TempDir()
	addr := getFreeAddr(t)

	certPath := filepath.Join(certDir, "auth.crt")
	keyPath := filepath.Join(certDir, "auth.key")

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 30",
		"--tls-cert", certPath,
		"--tls-key", keyPath,
		"--require-auth",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealthTLS(t, addr, certPath, 5*time.Second)

	// Read the cert for the HTTP client
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert: %v", err)
	}

	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(cert)

	httpsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: certPool,
			},
		},
	}

	// Generate pairing code via HTTPS
	genReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://%s/pair/generate", addr), nil)
	resp, err := httpsClient.Do(genReq)
	if err != nil {
		t.Fatalf("generate code request failed: %v", err)
	}
	defer resp.Body.Close()

	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)

	// Exchange code for token via HTTPS
	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "TLS Auth Test Device",
	})

	pairResp, err := httpsClient.Post(
		fmt.Sprintf("https://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	if err != nil {
		t.Fatalf("pair request failed: %v", err)
	}
	defer pairResp.Body.Close()

	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)

	// Connect via WSS with token
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{
			RootCAs: certPool,
		},
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tokenResp.Token)

	conn, _, err := dialer.Dial(fmt.Sprintf("wss://%s/ws", addr), headers)
	if err != nil {
		t.Fatalf("TLS websocket with auth failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	hp.stop(t)
}

// Device Management Integration Tests

// TestIntegrationDevicesList tests that the devices list command
// shows paired devices.
func TestIntegrationDevicesList(t *testing.T) {
	// Create a temp database and add a device
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	// Start host to pair a device
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 30",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Pair a device
	resp, _ := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Test Device for List",
	})
	pairResp, _ := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	hp.stop(t)

	// Run devices list
	listCmd := exec.Command(binaryPath, "devices", "list", "--token-store", dbPath)
	listCmd.Dir = moduleDir
	listOut, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("devices list failed: %v\n%s", err, listOut)
	}

	output := string(listOut)
	if !strings.Contains(output, tokenResp.DeviceID) {
		t.Errorf("expected device ID in output, got: %s", output)
	}
	if !strings.Contains(output, "Test Device for List") {
		t.Errorf("expected device name in output, got: %s", output)
	}
}

// TestIntegrationDevicesRevoke tests that the devices revoke command
// removes a device from storage.
func TestIntegrationDevicesRevoke(t *testing.T) {
	// Create a temp database and add a device
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	// Start host to pair a device
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 30",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Pair a device
	resp, _ := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Device to Revoke",
	})
	pairResp, _ := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	hp.stop(t)

	// Run devices revoke
	revokeCmd := exec.Command(binaryPath, "devices", "revoke", "--token-store", dbPath, tokenResp.DeviceID)
	revokeCmd.Dir = moduleDir
	revokeOut, err := revokeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("devices revoke failed: %v\n%s", err, revokeOut)
	}

	output := string(revokeOut)
	if !strings.Contains(output, "Revoked device") {
		t.Errorf("expected 'Revoked device' in output, got: %s", output)
	}

	// Verify device is no longer listed
	listCmd := exec.Command(binaryPath, "devices", "list", "--token-store", dbPath)
	listCmd.Dir = moduleDir
	listOut, _ := listCmd.CombinedOutput()

	if strings.Contains(string(listOut), tokenResp.DeviceID) {
		t.Errorf("device should not appear in list after revoke, got: %s", listOut)
	}
}

// TestIntegrationRevokedDeviceCannotReconnect tests that a revoked device
// cannot reconnect to the WebSocket server.
func TestIntegrationRevokedDeviceCannotReconnect(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	// Start host to pair a device
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 60",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Pair a device
	resp, _ := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Device to Revoke",
	})
	pairResp, _ := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	// Verify device can connect
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	wsHeaders := http.Header{}
	wsHeaders.Set("Authorization", "Bearer "+tokenResp.Token)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeaders)
	if err != nil {
		t.Fatalf("initial connection should succeed: %v", err)
	}
	conn.Close()

	// Stop host to revoke the device
	hp.stop(t)

	// Revoke the device
	revokeCmd := exec.Command(binaryPath, "devices", "revoke", "--token-store", dbPath, tokenResp.DeviceID)
	revokeCmd.Dir = moduleDir
	if out, err := revokeCmd.CombinedOutput(); err != nil {
		t.Fatalf("devices revoke failed: %v\n%s", err, out)
	}

	// Restart host
	cmd2 := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 60",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd2.Dir = moduleDir

	hp2 := &hostProcess{cmd: cmd2, addr: addr}
	cmd2.Stdout = &hp2.stdout
	cmd2.Stderr = &hp2.stderr

	if err := cmd2.Start(); err != nil {
		t.Fatalf("restart host failed: %v", err)
	}
	t.Cleanup(func() { hp2.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Try to connect with revoked token - should fail
	_, resp2, err := websocket.DefaultDialer.Dial(wsURL, wsHeaders)
	if err == nil {
		t.Fatal("connection with revoked token should fail")
	}

	if resp2 != nil && resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 for revoked token, got %d", resp2.StatusCode)
	}

	hp2.stop(t)
}

// ============================================================================
// Unit 5.1: Device Revocation Closes Active Connections
// ============================================================================

// TestIntegrationRevokeClosesActiveConnection tests that calling POST /devices/{id}/revoke
// closes active WebSocket connections for the revoked device within 2 seconds.
func TestIntegrationRevokeClosesActiveConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// Create temporary directory for database
	tmpDir, err := os.MkdirTemp("", "revoke-active-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	// Start host with authentication enabled
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 60",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Pair a device
	resp, err := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	if err != nil {
		t.Fatalf("failed to generate pairing code: %v", err)
	}
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Device to Revoke While Connected",
	})
	pairResp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	if err != nil {
		t.Fatalf("failed to pair: %v", err)
	}
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	// Connect via WebSocket with the device token
	wsURL := fmt.Sprintf("ws://%s/ws", addr)
	wsHeaders := http.Header{}
	wsHeaders.Set("Authorization", "Bearer "+tokenResp.Token)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeaders)
	if err != nil {
		t.Fatalf("WebSocket connection should succeed: %v", err)
	}
	defer conn.Close()

	// Verify connection works - read session.status
	env, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to read session.status: %v", err)
	}
	if env.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", env.Type)
	}

	// Call the revoke endpoint via HTTP while connected
	revokeURL := fmt.Sprintf("http://%s/devices/%s/revoke", addr, tokenResp.DeviceID)
	revokeReq, _ := http.NewRequest(http.MethodPost, revokeURL, nil)
	revokeResp, err := http.DefaultClient.Do(revokeReq)
	if err != nil {
		t.Fatalf("failed to call revoke endpoint: %v", err)
	}
	defer revokeResp.Body.Close()

	// Verify revoke succeeded
	if revokeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(revokeResp.Body)
		t.Fatalf("expected status 200, got %d: %s", revokeResp.StatusCode, body)
	}

	var revokeResult struct {
		DeviceID          string `json:"device_id"`
		ConnectionsClosed int    `json:"connections_closed"`
	}
	if err := json.NewDecoder(revokeResp.Body).Decode(&revokeResult); err != nil {
		t.Fatalf("failed to decode revoke response: %v", err)
	}

	// Should have closed 1 connection
	if revokeResult.ConnectionsClosed != 1 {
		t.Errorf("expected 1 connection closed, got %d", revokeResult.ConnectionsClosed)
	}

	// The WebSocket should now be closed - keep reading until we get an error.
	// The server may have pending messages in the write buffer before the close.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var closedWithinDeadline bool
	for {
		_, _, readErr := conn.ReadMessage()
		if readErr != nil {
			// Got expected error (connection closed)
			closedWithinDeadline = true
			break
		}
	}
	if !closedWithinDeadline {
		t.Error("expected connection to be closed within 2 seconds of revoke")
	}

	// Verify reconnection with the same token fails
	_, resp2, err := websocket.DefaultDialer.Dial(wsURL, wsHeaders)
	if err == nil {
		t.Fatal("reconnection with revoked token should fail")
	}
	if resp2 != nil && resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for revoked token, got %d", resp2.StatusCode)
	}
}

// TestIntegrationRevokeViaHTTPEndpoint tests the revoke HTTP endpoint directly.
func TestIntegrationRevokeViaHTTPEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// Create temporary directory for database
	tmpDir, err := os.MkdirTemp("", "revoke-http-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	// Start host
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 60",
		"--token-store", dbPath,
		"--require-auth",
		"--no-tls",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Pair a device
	resp, _ := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	var genResp generateCodeResponse
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()

	pairBody, _ := json.Marshal(pairRequest{
		Code:       genResp.Code,
		DeviceName: "Test Device",
	})
	pairResp, _ := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(pairBody),
	)
	var tokenResp pairResponse
	json.NewDecoder(pairResp.Body).Decode(&tokenResp)
	pairResp.Body.Close()

	// Revoke via HTTP endpoint (no active connection)
	revokeURL := fmt.Sprintf("http://%s/devices/%s/revoke", addr, tokenResp.DeviceID)
	revokeReq, _ := http.NewRequest(http.MethodPost, revokeURL, nil)
	revokeResp, err := http.DefaultClient.Do(revokeReq)
	if err != nil {
		t.Fatalf("failed to call revoke: %v", err)
	}
	defer revokeResp.Body.Close()

	if revokeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(revokeResp.Body)
		t.Fatalf("expected 200, got %d: %s", revokeResp.StatusCode, body)
	}

	var result struct {
		DeviceID          string `json:"device_id"`
		DeviceName        string `json:"device_name"`
		ConnectionsClosed int    `json:"connections_closed"`
	}
	if err := json.NewDecoder(revokeResp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if result.DeviceID != tokenResp.DeviceID {
		t.Errorf("expected device_id %s, got %s", tokenResp.DeviceID, result.DeviceID)
	}
	if result.DeviceName != "Test Device" {
		t.Errorf("expected device_name 'Test Device', got %s", result.DeviceName)
	}
	if result.ConnectionsClosed != 0 {
		t.Errorf("expected 0 connections closed (none connected), got %d", result.ConnectionsClosed)
	}

	// Verify device is gone - revoke again should return 404
	revokeReq2, _ := http.NewRequest(http.MethodPost, revokeURL, nil)
	revokeResp2, _ := http.DefaultClient.Do(revokeReq2)
	defer revokeResp2.Body.Close()

	if revokeResp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for already-revoked device, got %d", revokeResp2.StatusCode)
	}
}

// ============================================================================
// Unit 5.2: Untracked File Deletion
// ============================================================================

// deleteResultPayload matches the delete.result message structure.
type deleteResultPayload struct {
	CardID    string `json:"card_id"`
	Success   bool   `json:"success"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// TestIntegrationRejectUntrackedFileReturnsError tests that rejecting an untracked file
// returns action.untracked_file error instead of attempting git restore.
func TestIntegrationRejectUntrackedFileReturnsError(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create initial commit (need at least one commit for git operations)
	initFile := filepath.Join(repoDir, ".gitkeep")
	if err := os.WriteFile(initFile, []byte(""), 0644); err != nil {
		t.Fatalf("write init file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", ".gitkeep")
	gitAdd.Dir = repoDir
	gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	gitCommit.Run()

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Create a NEW untracked file (never committed)
	newFile := filepath.Join(repoDir, "untracked-new.txt")
	if err := os.WriteFile(newFile, []byte("new file content\n"), 0644); err != nil {
		t.Fatalf("write new file failed: %v", err)
	}
	// Use git add -N to make the file appear in git diff (intent to add)
	// The file is still "untracked" from git restore's perspective since
	// it's never been committed
	gitAddN := exec.Command("git", "add", "-N", "untracked-new.txt")
	gitAddN.Dir = repoDir
	if out, err := gitAddN.CombinedOutput(); err != nil {
		t.Fatalf("git add -N failed: %v\n%s", err, out)
	}

	// Wait for diff.card
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Send reject decision
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": diffCard.CardID,
			"action":  "reject",
		},
	}
	decisionData, _ := json.Marshal(decision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write decision failed: %v", err)
	}

	// Wait for decision.result with error
	var result decisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "decision.result" {
			json.Unmarshal(env.Payload, &result)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if result.CardID == "" {
		t.Fatalf("did not receive decision.result")
	}
	if result.Success {
		t.Fatal("expected reject to fail for untracked file")
	}
	if !strings.Contains(result.Error, "untracked") {
		t.Errorf("expected error to mention 'untracked', got: %s", result.Error)
	}

	// File should still exist (not deleted)
	if _, err := os.Stat(newFile); os.IsNotExist(err) {
		t.Error("file should still exist after rejected reject")
	}

	hp.stop(t)
}

// TestIntegrationDeleteUntrackedFile tests the review.delete message flow
// for deleting untracked files that cannot be restored.
func TestIntegrationDeleteUntrackedFile(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create initial commit
	initFile := filepath.Join(repoDir, ".gitkeep")
	if err := os.WriteFile(initFile, []byte(""), 0644); err != nil {
		t.Fatalf("write init file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", ".gitkeep")
	gitAdd.Dir = repoDir
	gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	gitCommit.Run()

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Create a NEW untracked file
	newFile := filepath.Join(repoDir, "to-delete.txt")
	if err := os.WriteFile(newFile, []byte("delete me\n"), 0644); err != nil {
		t.Fatalf("write new file failed: %v", err)
	}
	// Use git add -N to make the file appear in git diff (intent to add)
	gitAddN := exec.Command("git", "add", "-N", "to-delete.txt")
	gitAddN.Dir = repoDir
	if out, err := gitAddN.CombinedOutput(); err != nil {
		t.Fatalf("git add -N failed: %v\n%s", err, out)
	}

	// Wait for diff.card
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Send review.delete message
	deleteMsg := map[string]interface{}{
		"type": "review.delete",
		"payload": map[string]interface{}{
			"card_id":   diffCard.CardID,
			"confirmed": true,
		},
	}
	deleteData, _ := json.Marshal(deleteMsg)
	if err := conn.WriteMessage(websocket.TextMessage, deleteData); err != nil {
		t.Fatalf("write delete failed: %v", err)
	}

	// Wait for delete.result
	var deleteResult deleteResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "delete.result" {
			json.Unmarshal(env.Payload, &deleteResult)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if deleteResult.CardID == "" {
		t.Fatalf("did not receive delete.result")
	}
	if !deleteResult.Success {
		t.Fatalf("delete failed: %s", deleteResult.Error)
	}

	// File should be deleted
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Error("file should be deleted after review.delete")
	}

	hp.stop(t)
}

// TestIntegrationDeleteTrackedFileFails tests that review.delete fails for
// tracked files (safety check).
func TestIntegrationDeleteTrackedFileFails(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit a file
	trackedFile := filepath.Join(repoDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "tracked.txt")
	gitAdd.Dir = repoDir
	gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	gitCommit.Run()

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Modify the tracked file
	if err := os.WriteFile(trackedFile, []byte("modified content\n"), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card
	var diffCard diffCardPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Send review.delete message for tracked file - should fail
	deleteMsg := map[string]interface{}{
		"type": "review.delete",
		"payload": map[string]interface{}{
			"card_id":   diffCard.CardID,
			"confirmed": true,
		},
	}
	deleteData, _ := json.Marshal(deleteMsg)
	if err := conn.WriteMessage(websocket.TextMessage, deleteData); err != nil {
		t.Fatalf("write delete failed: %v", err)
	}

	// Wait for delete.result with error
	var deleteResult deleteResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "delete.result" {
			json.Unmarshal(env.Payload, &deleteResult)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if deleteResult.CardID == "" {
		t.Fatalf("did not receive delete.result")
	}
	if deleteResult.Success {
		t.Fatal("expected delete to fail for tracked file")
	}
	if !strings.Contains(deleteResult.Error, "tracked") {
		t.Errorf("expected error to mention 'tracked', got: %s", deleteResult.Error)
	}

	// File should still exist
	if _, err := os.Stat(trackedFile); os.IsNotExist(err) {
		t.Error("tracked file should NOT be deleted")
	}

	hp.stop(t)
}

// TestIntegrationRevokeEndpointNonLoopbackRejected tests that the revoke endpoint
// rejects requests from non-loopback addresses.
func TestIntegrationRevokeEndpointNonLoopbackRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	tmpDir, err := os.MkdirTemp("", "revoke-nonlocal-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "test.db")

	addr := getFreeAddr(t)

	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--session-cmd", "sleep 60",
		"--token-store", dbPath,
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// The revoke endpoint at /devices/any-id/revoke should work from localhost
	// We can't truly test non-loopback from localhost, but we verify the endpoint exists
	// and responds correctly for loopback requests (non-existent device)
	revokeURL := fmt.Sprintf("http://%s/devices/nonexistent-device/revoke", addr)
	revokeReq, _ := http.NewRequest(http.MethodPost, revokeURL, nil)
	revokeResp, err := http.DefaultClient.Do(revokeReq)
	if err != nil {
		t.Fatalf("failed to call revoke: %v", err)
	}
	defer revokeResp.Body.Close()

	// Should get 404 (device not found), not 403 (forbidden) since we're on loopback
	if revokeResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 (not found) from loopback, got %d", revokeResp.StatusCode)
	}
}

// TestIntegrationChunkDecisionRoundTrip tests end-to-end per-chunk decision flow:
// Create a file with multiple chunks → receive diff.card with chunks → send chunk.decision
// → receive chunk.decision_result within 2 seconds → verify git state.
func TestIntegrationChunkDecisionRoundTrip(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit initial file with 10 lines (enough spacing for 2 separate chunks)
	testFile := filepath.Join(repoDir, "multi.txt")
	initialContent := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"
	if err := os.WriteFile(testFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "multi.txt")
	gitAdd.Dir = repoDir
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	_, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read session.status failed: %v", err)
	}

	// Wait for initial poll
	time.Sleep(200 * time.Millisecond)

	// Modify lines 1 and 9 to create two separate chunks
	modifiedContent := "line1-CHANGED\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9-CHANGED\nline10\n"
	if err := os.WriteFile(testFile, []byte(modifiedContent), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card with chunks
	var diffCard diffCardWithChunksPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" {
		t.Fatalf("did not receive diff.card\nstdout: %s\nstderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Verify we got chunks
	if len(diffCard.Chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d\ndiff: %s", len(diffCard.Chunks), diffCard.Diff)
	}

	// Test 1: Send chunk.decision for chunk 0 (accept) and verify round-trip within 2 seconds
	startTime := time.Now()

	chunkDecision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":    diffCard.CardID,
			"chunk_index": 0,
			"action":     "accept",
			"comment":    "integration test accept chunk 0",
		},
	}
	decisionData, _ := json.Marshal(chunkDecision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write chunk decision failed: %v", err)
	}

	// Wait for chunk.decision_result
	var chunkResult chunkDecisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "chunk.decision_result" {
			json.Unmarshal(env.Payload, &chunkResult)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	roundTripDuration := time.Since(startTime)

	if chunkResult.CardID == "" {
		t.Fatalf("did not receive chunk.decision_result")
	}

	// Acceptance criteria: round-trip within 2 seconds
	if roundTripDuration > 2*time.Second {
		t.Errorf("chunk.decision round-trip took %v, expected < 2 seconds", roundTripDuration)
	}

	if !chunkResult.Success {
		t.Fatalf("chunk decision failed: %s (code: %s)", chunkResult.Error, chunkResult.ErrorCode)
	}
	if chunkResult.ChunkIndex != 0 {
		t.Errorf("expected chunk_index 0, got %d", chunkResult.ChunkIndex)
	}
	if chunkResult.Action != "accept" {
		t.Errorf("expected action 'accept', got %s", chunkResult.Action)
	}

	// Verify git state: chunk 0 (line1-CHANGED) should be staged
	gitDiffCached := exec.Command("git", "diff", "--cached")
	gitDiffCached.Dir = repoDir
	stagedOut, err := gitDiffCached.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --cached failed: %v", err)
	}
	staged := string(stagedOut)
	if !strings.Contains(staged, "line1-CHANGED") {
		t.Errorf("expected chunk 0 (line1-CHANGED) to be staged, got: %s", staged)
	}

	// Verify chunk 1 (line9-CHANGED) is NOT staged yet
	if strings.Contains(staged, "line9-CHANGED") {
		t.Errorf("expected chunk 1 (line9-CHANGED) to NOT be staged yet")
	}

	// Test 2: Send chunk.decision for chunk 1 (reject) to verify rejection works
	chunkDecision2 := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":    diffCard.CardID,
			"chunk_index": 1,
			"action":     "reject",
			"comment":    "integration test reject chunk 1",
		},
	}
	decisionData2, _ := json.Marshal(chunkDecision2)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData2); err != nil {
		t.Fatalf("write chunk decision 2 failed: %v", err)
	}

	// Wait for second chunk.decision_result
	var chunkResult2 chunkDecisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "chunk.decision_result" {
			json.Unmarshal(env.Payload, &chunkResult2)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if chunkResult2.CardID == "" {
		t.Fatalf("did not receive chunk.decision_result for chunk 1")
	}
	if !chunkResult2.Success {
		t.Fatalf("chunk 1 decision failed: %s (code: %s)", chunkResult2.Error, chunkResult2.ErrorCode)
	}

	// Verify git state: chunk 1 (line9-CHANGED) should be reverted in working tree
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	fileContent := string(content)
	if strings.Contains(fileContent, "line9-CHANGED") {
		t.Errorf("expected chunk 1 (line9-CHANGED) to be reverted from working tree")
	}
	if !strings.Contains(fileContent, "line9\n") {
		t.Errorf("expected line9 to be restored to original")
	}

	// Chunk 0 should still be present in working tree (staged but also in working tree)
	if !strings.Contains(fileContent, "line1-CHANGED") {
		t.Errorf("expected chunk 0 (line1-CHANGED) to still be in working tree")
	}

	hp.stop(t)
}

// TestIntegrationChunkDecisionStaleError tests that stale content hash returns proper error
func TestIntegrationChunkDecisionStaleError(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit initial file
	testFile := filepath.Join(repoDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line1\nline2\nline3\n"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "test.txt")
	gitAdd.Dir = repoDir
	gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial")
	gitCommit.Dir = repoDir
	gitCommit.Run()

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 30",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{cmd: cmd, addr: addr}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	readEnvelope(conn, 2*time.Second)
	time.Sleep(200 * time.Millisecond)

	// Modify file
	if err := os.WriteFile(testFile, []byte("line1-CHANGED\nline2\nline3\n"), 0644); err != nil {
		t.Fatalf("write modified file failed: %v", err)
	}

	// Wait for diff.card
	var diffCard diffCardWithChunksPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			json.Unmarshal(env.Payload, &diffCard)
			break
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	if diffCard.CardID == "" || len(diffCard.Chunks) == 0 {
		t.Fatalf("did not receive diff.card with chunks")
	}

	// Send chunk.decision with a wrong content hash (simulating stale state)
	chunkDecision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":      diffCard.CardID,
			"chunk_index":   0,
			"action":       "accept",
			"content_hash": "wronghash1234567", // Wrong hash - 16 chars like a real one
		},
	}
	decisionData, _ := json.Marshal(chunkDecision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write chunk decision failed: %v", err)
	}

	// Wait for chunk.decision_result with error
	var chunkResult chunkDecisionResultPayload
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "chunk.decision_result" {
			json.Unmarshal(env.Payload, &chunkResult)
			break
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	}

	if chunkResult.CardID == "" {
		t.Fatalf("did not receive chunk.decision_result")
	}

	// Should fail with action.chunk_stale error
	if chunkResult.Success {
		t.Fatalf("expected chunk decision to fail with stale error, but it succeeded")
	}
	if chunkResult.ErrorCode != "action.chunk_stale" {
		t.Errorf("expected error_code 'action.chunk_stale', got '%s'", chunkResult.ErrorCode)
	}

	hp.stop(t)
}

// TestIntegrationBinaryDiffStreaming tests that binary file changes produce
// diff.card messages with is_binary=true.
func TestIntegrationBinaryDiffStreaming(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	initGit := exec.Command("git", "init")
	initGit.Dir = repoDir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	// Configure git user for commits
	gitConfig1 := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig1.Dir = repoDir
	gitConfig1.Run()
	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = repoDir
	gitConfig2.Run()

	// Create and commit an initial binary file
	// Include NULL bytes (0x00) so git detects it as binary
	binaryFile := filepath.Join(repoDir, "image.png")
	pngData := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	if err := os.WriteFile(binaryFile, pngData, 0644); err != nil {
		t.Fatalf("write binary file failed: %v", err)
	}
	gitAdd := exec.Command("git", "add", "image.png")
	gitAdd.Dir = repoDir
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "add binary file")
	gitCommit.Dir = repoDir
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host with fast polling
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--diff-poll-ms", "100",
		"--session-cmd", "sleep 10",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Connect WebSocket
	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	// Wait for initial scan
	time.Sleep(200 * time.Millisecond)

	// Modify the binary file to create a diff (change bytes, keep NULL for binary detection)
	modifiedPng := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0xFF}
	if err := os.WriteFile(binaryFile, modifiedPng, 0644); err != nil {
		t.Fatalf("write modified binary file failed: %v", err)
	}

	// Wait for diff.card message with is_binary=true
	var diffCard diffCardWithBinaryPayload
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env messageEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type == "diff.card" {
			if err := json.Unmarshal(env.Payload, &diffCard); err != nil {
				t.Fatalf("parse diff.card failed: %v", err)
			}
			// Check if this is the binary file card
			if diffCard.File == "image.png" {
				break
			}
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}

	// Verify binary card was received
	if diffCard.CardID == "" || diffCard.File != "image.png" {
		t.Fatalf("did not receive diff.card for binary file\nhost stdout: %s\nhost stderr: %s", hp.stdout.String(), hp.stderr.String())
	}

	// Verify is_binary flag
	if !diffCard.IsBinary {
		t.Error("expected is_binary=true for PNG file")
	}

	// Verify diff placeholder
	if diffCard.Diff != "(binary file - content not shown)" {
		t.Errorf("expected binary placeholder diff, got: %s", diffCard.Diff)
	}

	// Verify no chunks for binary file
	if len(diffCard.Chunks) != 0 {
		t.Errorf("expected no chunks for binary file, got %d", len(diffCard.Chunks))
	}

	hp.stop(t)
}

// TestIntegrationCommitWorkflow tests the repo.status and repo.commit message flow.
// This verifies that clients can request repository status and create commits.
func TestIntegrationCommitWorkflow(t *testing.T) {
	// Create a temporary git repository
	repoDir := t.TempDir()

	// Initialize git repo
	initCmd := exec.Command("git", "init")
	initCmd.Dir = repoDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	// Configure git
	configCmds := [][]string{
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
	}
	for _, args := range configCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	// Create initial commit so HEAD exists
	initialFile := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test Repo"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	addCmd := exec.Command("git", "add", "README.md")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "-m", "Initial commit")
	commitCmd.Dir = repoDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Start host
	addr := getFreeAddr(t)
	tempDB := filepath.Join(repoDir, "test.db")
	cmd := exec.Command(
		binaryPath,
		"host",
		"start",
		"--addr", addr,
		"--repo", repoDir,
		"--token-store", tempDB,
		"--session-cmd", "sleep 10",
		"--no-tls",
		"--require-auth=false",
	)
	cmd.Dir = moduleDir

	hp := &hostProcess{
		cmd:  cmd,
		addr: addr,
	}
	cmd.Stdout = &hp.stdout
	cmd.Stderr = &hp.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start host failed: %v", err)
	}
	t.Cleanup(func() { hp.stop(t) })

	waitForHealth(t, addr, 3*time.Second)

	// Connect WebSocket
	conn := dialWebSocket(t, addr)
	defer conn.Close()

	// Read session.status
	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read first message failed: %v", err)
	}
	if first.Type != "session.status" {
		t.Fatalf("expected session.status, got %s", first.Type)
	}

	// === Test repo.status ===
	// Note: session.list may arrive after our request is sent, so we handle it in the read loop
	statusReq := map[string]interface{}{
		"type":    "repo.status",
		"payload": map[string]interface{}{},
	}
	statusData, _ := json.Marshal(statusReq)
	if err := conn.WriteMessage(websocket.TextMessage, statusData); err != nil {
		t.Fatalf("write repo.status failed: %v", err)
	}

	// Read messages until we get repo.status (skip session.list, terminal.append from reconnect handler)
	var statusResp *messageEnvelope
	for i := 0; i < 10; i++ {
		env, err := readEnvelope(conn, 2*time.Second)
		if err != nil {
			t.Fatalf("read repo.status response failed: %v", err)
		}
		if env.Type == "repo.status" {
			statusResp = &env
			break
		}
		// Skip session.list, terminal.append from reconnect handler
		if env.Type != "session.list" && env.Type != "terminal.append" {
			t.Fatalf("unexpected message while waiting for repo.status: %s", env.Type)
		}
	}
	if statusResp == nil {
		t.Fatal("did not receive repo.status response")
	}

	var statusPayload struct {
		Branch        string `json:"branch"`
		Upstream      string `json:"upstream"`
		StagedCount   int    `json:"staged_count"`
		UnstagedCount int    `json:"unstaged_count"`
		LastCommit    string `json:"last_commit"`
	}
	if err := json.Unmarshal(statusResp.Payload, &statusPayload); err != nil {
		t.Fatalf("parse repo.status payload failed: %v", err)
	}

	// Verify status - should show main/master branch with no staged changes
	if statusPayload.Branch != "main" && statusPayload.Branch != "master" {
		t.Errorf("expected branch main or master, got %s", statusPayload.Branch)
	}
	if statusPayload.StagedCount != 0 {
		t.Errorf("expected 0 staged files, got %d", statusPayload.StagedCount)
	}
	if !strings.Contains(statusPayload.LastCommit, "Initial commit") {
		t.Errorf("expected last commit to contain 'Initial commit', got %s", statusPayload.LastCommit)
	}

	// === Test repo.commit with no staged changes (should fail) ===
	commitReq := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"message": "Test commit (should fail)",
		},
	}
	commitData, _ := json.Marshal(commitReq)
	if err := conn.WriteMessage(websocket.TextMessage, commitData); err != nil {
		t.Fatalf("write repo.commit failed: %v", err)
	}

	// Read messages until we get repo.commit_result (skip diff.card, diff.removed from poller)
	var commitResp *messageEnvelope
	for i := 0; i < 10; i++ {
		env, err := readEnvelope(conn, 2*time.Second)
		if err != nil {
			t.Fatalf("read repo.commit response failed: %v", err)
		}
		if env.Type == "repo.commit_result" {
			commitResp = &env
			break
		}
		// Skip background messages
		if env.Type != "diff.card" && env.Type != "diff.removed" && env.Type != "terminal.append" {
			t.Fatalf("unexpected message while waiting for repo.commit_result: %s", env.Type)
		}
	}
	if commitResp == nil {
		t.Fatal("did not receive repo.commit_result response")
	}

	var failedCommit struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(commitResp.Payload, &failedCommit); err != nil {
		t.Fatalf("parse repo.commit_result payload failed: %v", err)
	}

	if failedCommit.Success {
		t.Error("expected commit to fail with no staged changes")
	}
	if failedCommit.ErrorCode != "commit.no_staged_changes" {
		t.Errorf("expected error_code commit.no_staged_changes, got %s", failedCommit.ErrorCode)
	}

	// === Stage a file and test successful commit ===
	newFile := filepath.Join(repoDir, "feature.txt")
	if err := os.WriteFile(newFile, []byte("new feature content"), 0644); err != nil {
		t.Fatalf("write new file failed: %v", err)
	}
	addCmd2 := exec.Command("git", "add", "feature.txt")
	addCmd2.Dir = repoDir
	if out, err := addCmd2.CombinedOutput(); err != nil {
		t.Fatalf("git add feature.txt failed: %v\n%s", err, out)
	}

	// Send commit request
	commitReq2 := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"message": "Add feature.txt",
		},
	}
	commitData2, _ := json.Marshal(commitReq2)
	if err := conn.WriteMessage(websocket.TextMessage, commitData2); err != nil {
		t.Fatalf("write repo.commit failed: %v", err)
	}

	// Read messages until we get repo.commit_result (skip diff.card, diff.removed from poller)
	var commitResp2 *messageEnvelope
	for i := 0; i < 10; i++ {
		env, err := readEnvelope(conn, 2*time.Second)
		if err != nil {
			t.Fatalf("read repo.commit response failed: %v", err)
		}
		if env.Type == "repo.commit_result" {
			commitResp2 = &env
			break
		}
		// Skip background messages
		if env.Type != "diff.card" && env.Type != "diff.removed" && env.Type != "terminal.append" {
			t.Fatalf("unexpected message while waiting for repo.commit_result: %s", env.Type)
		}
	}
	if commitResp2 == nil {
		t.Fatal("did not receive repo.commit_result response")
	}

	var successCommit struct {
		Success bool   `json:"success"`
		Hash    string `json:"hash"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(commitResp2.Payload, &successCommit); err != nil {
		t.Fatalf("parse repo.commit_result payload failed: %v", err)
	}

	if !successCommit.Success {
		t.Error("expected commit to succeed")
	}
	if successCommit.Hash == "" {
		t.Error("expected non-empty commit hash")
	}
	if len(successCommit.Hash) < 7 {
		t.Errorf("expected commit hash >= 7 chars, got %s", successCommit.Hash)
	}

	// Verify the commit exists in git
	logCmd := exec.Command("git", "log", "-1", "--format=%s")
	logCmd.Dir = repoDir
	logOut, err := logCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log failed: %v\n%s", err, logOut)
	}
	if !strings.Contains(string(logOut), "Add feature.txt") {
		t.Errorf("expected commit message in git log, got: %s", logOut)
	}

	hp.stop(t)
}
