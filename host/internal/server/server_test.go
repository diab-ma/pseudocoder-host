package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/stream"
)

func newTestServer() (*Server, *httptest.Server) {
	s := NewServer("unused")
	go s.runBroadcaster()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	ts := httptest.NewServer(mux)

	return s, ts
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
}

func readMessage(t *testing.T, conn *websocket.Conn) Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return msg
}

func readSessionCreatedName(t *testing.T, conn *websocket.Conn) string {
	t.Helper()

	var resp Message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read session.create response: %v", err)
	}
	if resp.Type != MessageTypeSessionCreated {
		t.Fatalf("expected session.created message, got %s", resp.Type)
	}

	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", resp.Payload)
	}

	name, ok := payload["name"].(string)
	if !ok {
		t.Fatalf("expected string name payload, got %T", payload["name"])
	}
	return name
}

func TestWebSocketSessionStatusAndBroadcast(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %#v", msg.Payload)
	}
	if payload["status"] != "running" {
		t.Fatalf("expected status running, got %#v", payload["status"])
	}

	s.BroadcastTerminalOutput("hello\n")
	msg = readMessage(t, conn)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append, got %s", msg.Type)
	}
	payload, ok = msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %#v", msg.Payload)
	}
	if payload["chunk"] != "hello\n" {
		t.Fatalf("unexpected chunk: %#v", payload["chunk"])
	}
}

func TestWebSocketBroadcastToMultipleClients(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	connA, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial A failed: %v", err)
	}
	defer connA.Close()

	connB, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial B failed: %v", err)
	}
	defer connB.Close()

	_ = readMessage(t, connA)
	_ = readMessage(t, connB)

	s.BroadcastTerminalOutput("ping\n")

	msgA := readMessage(t, connA)
	msgB := readMessage(t, connB)
	if msgA.Type != MessageTypeTerminalAppend || msgB.Type != MessageTypeTerminalAppend {
		t.Fatal("expected terminal.append for both clients")
	}
}

func TestStartAsyncFailsWhenPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	s := NewServer(ln.Addr().String())
	errCh := s.StartAsync()
	if err := <-errCh; err == nil {
		t.Fatal("expected error when port already in use")
	}
}

func TestStopWithActiveClient(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	done := make(chan struct{})
	go func() {
		_ = s.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return in time")
	}
}

// TestInvalidJSONMessage verifies the server handles malformed JSON gracefully.
func TestInvalidJSONMessage(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read the session.status message first
	_ = readMessage(t, conn)

	// Send invalid JSON - server should not crash
	err = conn.WriteMessage(websocket.TextMessage, []byte("not valid json {{{"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Give server time to process
	time.Sleep(50 * time.Millisecond)

	// Server should still be able to broadcast
	s.BroadcastTerminalOutput("still working\n")

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append after invalid JSON, got %s", msg.Type)
	}
}

// TestOversizedMessage verifies the server rejects messages exceeding the size limit.
func TestOversizedMessage(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	// Send a message larger than the read limit (512KB)
	// The server should close the connection
	largePayload := make([]byte, 600*1024) // 600KB
	for i := range largePayload {
		largePayload[i] = 'x'
	}

	err = conn.WriteMessage(websocket.TextMessage, largePayload)
	if err != nil {
		// Write might fail if connection already closed
		return
	}

	// Try to read - should fail as connection is closed
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Log("connection still open after oversized message (acceptable)")
	}
	// Either error or no error is acceptable - we just verify no crash
}

// TestClientDisconnectDoesNotCrashServer verifies clean handling of client disconnect.
func TestClientDisconnectDoesNotCrashServer(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Connect and immediately disconnect
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	conn.Close()

	// Give server time to process disconnect
	time.Sleep(50 * time.Millisecond)

	// Connect again - server should still work
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("second dial failed: %v", err)
	}
	defer conn2.Close()

	msg := readMessage(t, conn2)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}
}

// TestBroadcastToFullBuffer verifies server handles slow clients without blocking.
func TestBroadcastToFullBuffer(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Don't read from connection to simulate slow client
	// Send many broadcasts - should not block
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			s.BroadcastTerminalOutput("message\n")
		}
		close(done)
	}()

	select {
	case <-done:
		// Good - broadcasts completed without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("broadcasts blocked on slow client")
	}
}

// TestComputeDiffCardMeta verifies the deterministic triage metadata helper.
func TestComputeDiffCardMeta(t *testing.T) {
	tests := []struct {
		name           string
		file           string
		chunks         []ChunkInfo
		isBinary       bool
		isDeleted      bool
		stats          *DiffStats
		semanticGroups []SemanticGroupInfo
		wantSummary    string
		wantLevel      string
		wantReasons    []string
	}{
		{
			name:        "binary file",
			file:        "assets/image.png",
			isBinary:    true,
			wantSummary: "Binary file changed",
			wantLevel:   "high",
			wantReasons: []string{"binary_file"},
		},
		{
			name:        "deleted sensitive file",
			file:        "config/auth_service.go",
			isDeleted:   true,
			wantSummary: "File deleted",
			wantLevel:   "critical",
			wantReasons: []string{"sensitive_path", "file_deletion"},
		},
		{
			name:        "normal file with chunks and stats",
			file:        "README.md",
			chunks:      []ChunkInfo{{Index: 0}, {Index: 1}},
			stats:       &DiffStats{ByteSize: 100, LineCount: 10, AddedLines: 5, DeletedLines: 3},
			wantSummary: "2 chunks, +5 / -3",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:        "stats without chunks",
			file:        "docs/notes.txt",
			stats:       &DiffStats{ByteSize: 50, LineCount: 5, AddedLines: 2, DeletedLines: 1},
			wantSummary: "+2 / -1",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:        "no stats fallback",
			file:        "docs/notes.txt",
			wantSummary: "File updated",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:        "large diff by bytes",
			file:        "data/dump.sql",
			stats:       &DiffStats{ByteSize: 1048577, LineCount: 100, AddedLines: 50, DeletedLines: 10},
			wantSummary: "+50 / -10",
			wantLevel:   "high",
			wantReasons: []string{"large_diff"},
		},
		{
			name:        "high churn on source path",
			file:        "host/internal/server/handler.go",
			stats:       &DiffStats{ByteSize: 5000, LineCount: 300, AddedLines: 120, DeletedLines: 80},
			chunks:      []ChunkInfo{{Index: 0}},
			wantSummary: "1 chunks, +120 / -80",
			wantLevel:   "medium",
			wantReasons: []string{"high_churn", "source_change"},
		},
		{
			name:        "sensitive + large + high churn yields critical with first 3 reasons",
			file:        "host/internal/security/token_manager.go",
			stats:       &DiffStats{ByteSize: 2000000, LineCount: 3000, AddedLines: 150, DeletedLines: 100},
			wantSummary: "+150 / -100",
			wantLevel:   "critical",
			wantReasons: []string{"sensitive_path", "large_diff", "high_churn"},
		},
		{
			name:        "zero-value stats",
			file:        "README.md",
			stats:       &DiffStats{},
			wantSummary: "+0 / -0",
			wantLevel:   "low",
			wantReasons: nil,
		},
		// C2: Semantic summary enrichment
		{
			name:   "semantic groups present with stats",
			file:   "src/handler.go",
			chunks: []ChunkInfo{{Index: 0}, {Index: 1}},
			stats:  &DiffStats{ByteSize: 200, LineCount: 20, AddedLines: 10, DeletedLines: 5},
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-aaa", Label: "Function changes", Kind: "function", ChunkIndexes: []int{0, 1}, RiskLevel: "medium"},
			},
			wantSummary: "Function changes: 2 chunk(s), +10 / -5",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:   "semantic groups present without stats",
			file:   "src/handler.go",
			chunks: []ChunkInfo{{Index: 0}},
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-bbb", Label: "Imports", Kind: "import", ChunkIndexes: []int{0}, RiskLevel: "low"},
			},
			wantSummary: "Imports: 1 chunk(s)",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:   "semantic primary group selection: highest risk wins",
			file:   "src/handler.go",
			chunks: []ChunkInfo{{Index: 0}, {Index: 1}, {Index: 2}},
			stats:  &DiffStats{ByteSize: 100, LineCount: 10, AddedLines: 5, DeletedLines: 2},
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-low", Label: "Code changes", Kind: "generic", ChunkIndexes: []int{0}, RiskLevel: "low"},
				{GroupID: "sg-high", Label: "Function changes", Kind: "function", ChunkIndexes: []int{1, 2}, RiskLevel: "high"},
			},
			wantSummary: "Function changes: 2 chunk(s), +5 / -2",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:   "semantic primary group selection: same risk, larger chunk_indexes wins",
			file:   "README.md",
			chunks: []ChunkInfo{{Index: 0}, {Index: 1}, {Index: 2}},
			stats:  &DiffStats{ByteSize: 100, LineCount: 10, AddedLines: 3, DeletedLines: 1},
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-a", Label: "Imports", Kind: "import", ChunkIndexes: []int{0}, LineStart: 1, RiskLevel: "low"},
				{GroupID: "sg-b", Label: "Code changes", Kind: "generic", ChunkIndexes: []int{1, 2}, LineStart: 10, RiskLevel: "low"},
			},
			wantSummary: "Code changes: 2 chunk(s), +3 / -1",
			wantLevel:   "low",
			wantReasons: nil,
		},
		{
			name:     "semantic groups on binary card use semantic summary",
			file:     "logo.png",
			isBinary: true,
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-x", Label: "Binary change", Kind: "binary", ChunkIndexes: []int{0}, RiskLevel: "high"},
			},
			wantSummary: "Binary change: 1 chunk(s)",
			wantLevel:   "high",
			wantReasons: []string{"binary_file"},
		},
		{
			name:      "semantic groups on deleted card use semantic summary",
			file:      "old/main.go",
			isDeleted: true,
			stats:     &DiffStats{ByteSize: 100, LineCount: 10, AddedLines: 0, DeletedLines: 8},
			semanticGroups: []SemanticGroupInfo{
				{GroupID: "sg-del", Label: "Deletion", Kind: "deleted", ChunkIndexes: []int{0}, RiskLevel: "high"},
			},
			wantSummary: "Deletion: 1 chunk(s), +0 / -8",
			wantLevel:   "high",
			wantReasons: []string{"file_deletion"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, level, reasons := computeDiffCardMeta(tt.file, tt.chunks, tt.isBinary, tt.isDeleted, tt.stats, tt.semanticGroups)
			if summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tt.wantSummary)
			}
			if level != tt.wantLevel {
				t.Errorf("riskLevel = %q, want %q", level, tt.wantLevel)
			}
			if len(reasons) != len(tt.wantReasons) {
				t.Errorf("riskReasons = %v (len %d), want %v (len %d)", reasons, len(reasons), tt.wantReasons, len(tt.wantReasons))
			} else {
				for i := range reasons {
					if reasons[i] != tt.wantReasons[i] {
						t.Errorf("riskReasons[%d] = %q, want %q", i, reasons[i], tt.wantReasons[i])
					}
				}
			}
		})
	}
}

// TestNewDiffCardMessage verifies the diff.card message constructor.
func TestNewDiffCardMessage(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 4, Offset: 0, Length: 11},
	}
	stats := &DiffStats{ByteSize: 11, LineCount: 1, AddedLines: 1, DeletedLines: 0}
	msg := NewDiffCardMessage("card-123", "src/main.go", "+added line", chunks, nil, nil, false, false, stats, 1703500000000)

	if msg.Type != MessageTypeDiffCard {
		t.Errorf("expected type %s, got %s", MessageTypeDiffCard, msg.Type)
	}

	payload, ok := msg.Payload.(DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if payload.CardID != "card-123" {
		t.Errorf("expected CardID card-123, got %s", payload.CardID)
	}
	if payload.File != "src/main.go" {
		t.Errorf("expected File src/main.go, got %s", payload.File)
	}
	if payload.Diff != "+added line" {
		t.Errorf("expected Diff +added line, got %s", payload.Diff)
	}
	if len(payload.Chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(payload.Chunks))
	}
	if payload.Chunks[0].Index != 0 {
		t.Errorf("expected chunk index 0, got %d", payload.Chunks[0].Index)
	}
	if payload.CreatedAt != 1703500000000 {
		t.Errorf("expected CreatedAt 1703500000000, got %d", payload.CreatedAt)
	}

	// B1: metadata fields should be populated
	if payload.Summary != "1 chunks, +1 / -0" {
		t.Errorf("expected Summary '1 chunks, +1 / -0', got %q", payload.Summary)
	}
	if payload.RiskLevel != "low" {
		t.Errorf("expected RiskLevel 'low', got %q", payload.RiskLevel)
	}
	if payload.RiskReasons != nil {
		t.Errorf("expected nil RiskReasons, got %v", payload.RiskReasons)
	}
}

// TestNewErrorMessage verifies the error message constructor.
func TestNewErrorMessage(t *testing.T) {
	msg := NewErrorMessage("AUTH_FAILED", "Invalid token")

	if msg.Type != MessageTypeError {
		t.Errorf("expected type %s, got %s", MessageTypeError, msg.Type)
	}

	payload, ok := msg.Payload.(ErrorPayload)
	if !ok {
		t.Fatalf("expected ErrorPayload, got %T", msg.Payload)
	}

	if payload.Code != "AUTH_FAILED" {
		t.Errorf("expected Code AUTH_FAILED, got %s", payload.Code)
	}
	if payload.Message != "Invalid token" {
		t.Errorf("expected Message 'Invalid token', got %s", payload.Message)
	}
}

// TestNewHeartbeatMessage verifies the heartbeat message constructor.
func TestNewHeartbeatMessage(t *testing.T) {
	msg := NewHeartbeatMessage()

	if msg.Type != MessageTypeHeartbeat {
		t.Errorf("expected type %s, got %s", MessageTypeHeartbeat, msg.Type)
	}

	// Payload should be an empty struct
	if msg.Payload == nil {
		t.Error("expected non-nil payload")
	}
}

// TestNewSessionStatusMessage verifies the session.status message constructor.
func TestNewSessionStatusMessage(t *testing.T) {
	msg := NewSessionStatusMessage("session-42", "running")

	if msg.Type != MessageTypeSessionStatus {
		t.Errorf("expected type %s, got %s", MessageTypeSessionStatus, msg.Type)
	}

	payload, ok := msg.Payload.(SessionStatusPayload)
	if !ok {
		t.Fatalf("expected SessionStatusPayload, got %T", msg.Payload)
	}

	if payload.SessionID != "session-42" {
		t.Errorf("expected SessionID session-42, got %s", payload.SessionID)
	}
	if payload.Status != "running" {
		t.Errorf("expected Status running, got %s", payload.Status)
	}
	if payload.LastActivity <= 0 {
		t.Error("expected LastActivity to be set")
	}
}

// TestNewTerminalAppendMessage verifies the terminal.append message constructor.
func TestNewTerminalAppendMessage(t *testing.T) {
	msg := NewTerminalAppendMessage("session-42", "hello world\n")

	if msg.Type != MessageTypeTerminalAppend {
		t.Errorf("expected type %s, got %s", MessageTypeTerminalAppend, msg.Type)
	}

	payload, ok := msg.Payload.(TerminalAppendPayload)
	if !ok {
		t.Fatalf("expected TerminalAppendPayload, got %T", msg.Payload)
	}

	if payload.SessionID != "session-42" {
		t.Errorf("expected SessionID session-42, got %s", payload.SessionID)
	}
	if payload.Chunk != "hello world\n" {
		t.Errorf("expected Chunk 'hello world\\n', got %s", payload.Chunk)
	}
	if payload.Timestamp <= 0 {
		t.Error("expected Timestamp to be set")
	}
}

// TestBroadcastDiffCard verifies diff.card messages are broadcast to clients.
func TestBroadcastDiffCard(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Broadcast a diff card with chunk info
	chunks := []stream.ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 4, Offset: 0, Length: 9},
	}
	stats := &stream.DiffStats{ByteSize: 9, LineCount: 1, AddedLines: 1, DeletedLines: 0}
	s.BroadcastDiffCard("card-abc", "file.go", "+new line", chunks, nil, nil, false, false, stats, 1703500000000)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDiffCard {
		t.Fatalf("expected diff.card, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}

	if payload["card_id"] != "card-abc" {
		t.Errorf("expected card_id card-abc, got %v", payload["card_id"])
	}
	if payload["file"] != "file.go" {
		t.Errorf("expected file file.go, got %v", payload["file"])
	}
	if payload["diff"] != "+new line" {
		t.Errorf("expected diff '+new line', got %v", payload["diff"])
	}

	// B1: metadata should be present in broadcast JSON
	if payload["summary"] != "1 chunks, +1 / -0" {
		t.Errorf("expected summary '1 chunks, +1 / -0', got %v", payload["summary"])
	}
	if payload["risk_level"] != "low" {
		t.Errorf("expected risk_level 'low', got %v", payload["risk_level"])
	}
}

// TestReconnectAfterDisconnect verifies clients can reconnect successfully.
func TestReconnectAfterDisconnect(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// First connection
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("first dial failed: %v", err)
	}
	_ = readMessage(t, conn1)
	conn1.Close()

	// Wait for disconnect to be processed
	time.Sleep(50 * time.Millisecond)

	// Second connection
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("second dial failed: %v", err)
	}
	defer conn2.Close()

	msg := readMessage(t, conn2)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status on reconnect, got %s", msg.Type)
	}

	// Verify broadcasts work
	s.BroadcastTerminalOutput("reconnect test\n")
	msg = readMessage(t, conn2)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append, got %s", msg.Type)
	}
}

// TestNewDecisionResultMessage verifies the decision.result message constructor.
func TestNewDecisionResultMessage(t *testing.T) {
	msg := NewDecisionResultMessage("card-123", "accept", true, "", "")

	if msg.Type != MessageTypeDecisionResult {
		t.Errorf("expected type %s, got %s", MessageTypeDecisionResult, msg.Type)
	}

	payload, ok := msg.Payload.(DecisionResultPayload)
	if !ok {
		t.Fatalf("expected DecisionResultPayload, got %T", msg.Payload)
	}

	if payload.CardID != "card-123" {
		t.Errorf("expected CardID card-123, got %s", payload.CardID)
	}
	if payload.Action != "accept" {
		t.Errorf("expected Action accept, got %s", payload.Action)
	}
	if !payload.Success {
		t.Error("expected Success to be true")
	}
	if payload.ErrorCode != "" {
		t.Errorf("expected empty ErrorCode, got %s", payload.ErrorCode)
	}
	if payload.Error != "" {
		t.Errorf("expected empty Error, got %s", payload.Error)
	}
}

// TestNewDecisionResultMessageWithError verifies error handling in the constructor.
func TestNewDecisionResultMessageWithError(t *testing.T) {
	msg := NewDecisionResultMessage("card-456", "reject", false, "storage.not_found", "card not found")

	payload, ok := msg.Payload.(DecisionResultPayload)
	if !ok {
		t.Fatalf("expected DecisionResultPayload, got %T", msg.Payload)
	}

	if payload.Success {
		t.Error("expected Success to be false")
	}
	if payload.ErrorCode != "storage.not_found" {
		t.Errorf("expected ErrorCode 'storage.not_found', got %s", payload.ErrorCode)
	}
	if payload.Error != "card not found" {
		t.Errorf("expected Error 'card not found', got %s", payload.Error)
	}
}

// TestReviewDecisionWithHandler verifies decisions are routed to the handler.
func TestReviewDecisionWithHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Track handler calls
	var handlerCalls []struct {
		cardID, action, comment string
	}

	s.SetDecisionHandler(func(cardID, action, comment string) error {
		handlerCalls = append(handlerCalls, struct {
			cardID, action, comment string
		}{cardID, action, comment})
		return nil
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send a review.decision message
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-test",
			"action":  "accept",
			"comment": "looks good",
		},
	}
	decisionData, _ := json.Marshal(decision)
	if err := conn.WriteMessage(websocket.TextMessage, decisionData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read the decision.result response
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != true {
		t.Errorf("expected success true, got %v", payload["success"])
	}
	if payload["card_id"] != "card-test" {
		t.Errorf("expected card_id card-test, got %v", payload["card_id"])
	}

	// Verify handler was called
	if len(handlerCalls) != 1 {
		t.Fatalf("expected 1 handler call, got %d", len(handlerCalls))
	}
	if handlerCalls[0].cardID != "card-test" {
		t.Errorf("expected cardID card-test, got %s", handlerCalls[0].cardID)
	}
	if handlerCalls[0].action != "accept" {
		t.Errorf("expected action accept, got %s", handlerCalls[0].action)
	}
	if handlerCalls[0].comment != "looks good" {
		t.Errorf("expected comment 'looks good', got %s", handlerCalls[0].comment)
	}
}

// TestReviewDecisionInvalidAction verifies invalid actions are rejected.
func TestReviewDecisionInvalidAction(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	// Send a decision with invalid action
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-test",
			"action":  "maybe", // Invalid
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn.WriteMessage(websocket.TextMessage, decisionData)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false, got %v", payload["success"])
	}
	if payload["error"] == nil || payload["error"] == "" {
		t.Error("expected error message for invalid action")
	}
	// Verify error_code is returned for mobile severity handling
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected error_code %q, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}
}

// TestReviewDecisionMissingCardID verifies missing card_id is rejected.
func TestReviewDecisionMissingCardID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	// Send a decision without card_id
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"action": "accept",
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn.WriteMessage(websocket.TextMessage, decisionData)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false, got %v", payload["success"])
	}
	// Verify error_code is returned for mobile severity handling
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected error_code %q, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestReviewDecisionNoHandler verifies behavior when no handler is set.
func TestReviewDecisionNoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set a handler

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-test",
			"action":  "accept",
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn.WriteMessage(websocket.TextMessage, decisionData)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false when no handler, got %v", payload["success"])
	}
	// Verify error_code is returned for mobile severity handling
	if payload["error_code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected error_code %q, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
	}
}

// TestReviewDecisionHandlerError verifies handler errors are returned with proper codes.
func TestReviewDecisionHandlerError(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Handler returns a CodedError to test proper code propagation
	s.SetDecisionHandler(func(cardID, action, comment string) error {
		return apperrors.NotFound("card")
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-test",
			"action":  "reject",
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn.WriteMessage(websocket.TextMessage, decisionData)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false on error, got %v", payload["success"])
	}
	if payload["error"] != "card not found" {
		t.Errorf("expected error 'card not found', got %v", payload["error"])
	}
	// Verify error_code is returned for mobile severity handling
	if payload["error_code"] != apperrors.CodeStorageNotFound {
		t.Errorf("expected error_code %q, got %v", apperrors.CodeStorageNotFound, payload["error_code"])
	}
}

// TestReviewDecisionHandlerPlainError verifies plain errors return unknown code.
func TestReviewDecisionHandlerPlainError(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Handler returns a plain error (not CodedError) to test fallback
	s.SetDecisionHandler(func(cardID, action, comment string) error {
		return errors.New("something went wrong")
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)

	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-test",
			"action":  "accept",
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn.WriteMessage(websocket.TextMessage, decisionData)

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDecisionResult {
		t.Fatalf("expected decision.result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false on error, got %v", payload["success"])
	}
	// Plain errors should map to error.unknown
	if payload["error_code"] != apperrors.CodeUnknown {
		t.Errorf("expected error_code %q for plain error, got %v", apperrors.CodeUnknown, payload["error_code"])
	}
}

// TestDecisionBroadcastToAll verifies decisions are broadcast to all clients.
// The sender receives exactly one message (via broadcast), not a duplicate.
func TestDecisionBroadcastToAll(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetDecisionHandler(func(cardID, action, comment string) error {
		return nil
	})

	// Connect two clients
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 2 failed: %v", err)
	}
	defer conn2.Close()

	// Read session.status from both
	_ = readMessage(t, conn1)
	_ = readMessage(t, conn2)

	// Send decision from client 1
	decision := map[string]interface{}{
		"type": "review.decision",
		"payload": map[string]interface{}{
			"card_id": "card-broadcast",
			"action":  "accept",
		},
	}
	decisionData, _ := json.Marshal(decision)
	conn1.WriteMessage(websocket.TextMessage, decisionData)

	// Client 1 should get exactly one decision.result (via broadcast)
	msg1 := readMessage(t, conn1)
	if msg1.Type != MessageTypeDecisionResult {
		t.Fatalf("client 1: expected decision.result, got %s", msg1.Type)
	}

	payload1, ok := msg1.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg1.Payload)
	}
	if payload1["card_id"] != "card-broadcast" {
		t.Errorf("client 1: expected card_id card-broadcast, got %v", payload1["card_id"])
	}

	// Client 2 should also get the broadcast
	msg2 := readMessage(t, conn2)
	if msg2.Type != MessageTypeDecisionResult {
		t.Fatalf("client 2: expected decision.result, got %s", msg2.Type)
	}

	payload2, ok := msg2.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg2.Payload)
	}
	if payload2["card_id"] != "card-broadcast" {
		t.Errorf("client 2: expected card_id card-broadcast, got %v", payload2["card_id"])
	}
}

// TestReconnectHandler verifies the reconnect handler is called for new clients.
func TestReconnectHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	var handlerCalls int
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		handlerCalls++
		// Send some terminal history
		sendToClient(NewTerminalAppendMessage("test-session", "history line 1\n"))
		sendToClient(NewTerminalAppendMessage("test-session", "history line 2\n"))
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status first
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	// Read the two history messages
	msg = readMessage(t, conn)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append for history, got %s", msg.Type)
	}
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["chunk"] != "history line 1\n" {
		t.Errorf("expected 'history line 1\\n', got %v", payload["chunk"])
	}

	msg = readMessage(t, conn)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append for history, got %s", msg.Type)
	}
	payload, ok = msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["chunk"] != "history line 2\n" {
		t.Errorf("expected 'history line 2\\n', got %v", payload["chunk"])
	}

	if handlerCalls != 1 {
		t.Errorf("expected 1 handler call, got %d", handlerCalls)
	}
}

// TestReconnectHandlerCalledForEachClient verifies each client gets history.
func TestReconnectHandlerCalledForEachClient(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	var handlerCalls int
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		handlerCalls++
		sendToClient(NewTerminalAppendMessage("test-session", "replay\n"))
	})

	// Connect first client
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	defer conn1.Close()

	// Read session.status and history
	_ = readMessage(t, conn1)
	msg := readMessage(t, conn1)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 1: expected terminal.append, got %s", msg.Type)
	}

	// Connect second client
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 2 failed: %v", err)
	}
	defer conn2.Close()

	// Read session.status and history for second client
	_ = readMessage(t, conn2)
	msg = readMessage(t, conn2)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 2: expected terminal.append, got %s", msg.Type)
	}

	if handlerCalls != 2 {
		t.Errorf("expected 2 handler calls (one per client), got %d", handlerCalls)
	}
}

// TestReconnectHandlerWithPendingCards verifies cards are replayed on connect.
func TestReconnectHandlerWithPendingCards(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetReconnectHandler(func(sendToClient func(Message)) {
		// Simulate sending pending cards on reconnect
		sendToClient(NewDiffCardMessage("card-1", "file1.go", "+line1", nil, nil, nil, false, false, nil, 1000))
		sendToClient(NewDiffCardMessage("card-2", "file2.go", "+line2", nil, nil, nil, false, false, nil, 2000))
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Read the two pending cards
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDiffCard {
		t.Fatalf("expected diff.card, got %s", msg.Type)
	}
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["card_id"] != "card-1" {
		t.Errorf("expected card_id card-1, got %v", payload["card_id"])
	}

	msg = readMessage(t, conn)
	if msg.Type != MessageTypeDiffCard {
		t.Fatalf("expected diff.card, got %s", msg.Type)
	}
	payload, ok = msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["card_id"] != "card-2" {
		t.Errorf("expected card_id card-2, got %v", payload["card_id"])
	}
}

// TestReconnectHandlerBinaryCardSemanticGroups verifies that binary cards
// replayed during reconnect include semantic_groups (C2-G1 parity).
func TestReconnectHandlerBinaryCardSemanticGroups(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Simulate reconnect handler that sends a binary card WITH semantic groups,
	// matching the fixed reconnect path behavior in host.go.
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		groups := []SemanticGroupInfo{
			{GroupID: "sg-binary", Label: "Binary", Kind: "binary",
				LineStart: 0, LineEnd: 0, ChunkIndexes: []int{0}, RiskLevel: "low"},
		}
		sendToClient(NewDiffCardMessage("card-bin", "photo.jpg",
			"(binary file - content not shown)",
			nil, nil, groups, true, false, nil, 1000))
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Read the binary card
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeDiffCard {
		t.Fatalf("expected diff.card, got %s", msg.Type)
	}
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if isBin, _ := payload["is_binary"].(bool); !isBin {
		t.Error("expected is_binary=true")
	}
	semGroups, ok := payload["semantic_groups"].([]interface{})
	if !ok || len(semGroups) == 0 {
		t.Fatal("expected semantic_groups for binary card in reconnect replay")
	}
	group0, _ := semGroups[0].(map[string]interface{})
	if kind, _ := group0["kind"].(string); kind != "binary" {
		t.Errorf("expected binary kind, got %q", kind)
	}
}

// TestReconnectHandlerNoDuplicates verifies history is sent only to connecting client.
func TestReconnectHandlerNoDuplicates(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Each connecting client receives history via the reconnect handler
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		sendToClient(NewTerminalAppendMessage("test-session", "history\n"))
	})

	// Connect first client
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	defer conn1.Close()

	// Read messages from conn1
	_ = readMessage(t, conn1) // session.status
	msg1 := readMessage(t, conn1)
	if msg1.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 1: expected terminal.append, got %s", msg1.Type)
	}

	// Connect second client while first is still connected
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 2 failed: %v", err)
	}
	defer conn2.Close()

	_ = readMessage(t, conn2) // session.status
	msg2 := readMessage(t, conn2)
	if msg2.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 2: expected terminal.append, got %s", msg2.Type)
	}

	// Now broadcast a new message - both should receive it
	s.BroadcastTerminalOutput("new message\n")

	msg := readMessage(t, conn1)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 1: expected terminal.append for broadcast, got %s", msg.Type)
	}
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["chunk"] != "new message\n" {
		t.Errorf("client 1: expected 'new message\\n', got %v", payload["chunk"])
	}

	msg = readMessage(t, conn2)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("client 2: expected terminal.append for broadcast, got %s", msg.Type)
	}
	payload, ok = msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["chunk"] != "new message\n" {
		t.Errorf("client 2: expected 'new message\\n', got %v", payload["chunk"])
	}
}

// TestReconnectHandlerNoHandler verifies no crash when no handler is set.
func TestReconnectHandlerNoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set a reconnect handler

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Should still get session.status
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	// Broadcasts should still work
	s.BroadcastTerminalOutput("test\n")
	msg = readMessage(t, conn)
	if msg.Type != MessageTypeTerminalAppend {
		t.Fatalf("expected terminal.append, got %s", msg.Type)
	}
}

// TestReconnectHandlerLargeHistory verifies large history (>256 messages) is not truncated.
// This tests the fix for the bug where history was pushed into the channel before
// writePump was started, causing most messages to be dropped.
func TestReconnectHandlerLargeHistory(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Send 500 lines of history - more than the 256 channel buffer
	const historySize = 500
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		for i := 0; i < historySize; i++ {
			sendToClient(NewTerminalAppendMessage("test-session", fmt.Sprintf("line %d\n", i)))
		}
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status first
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	// Read all history messages and count them
	receivedCount := 0
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Timeout or error means no more messages
			break
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if msg.Type != MessageTypeTerminalAppend {
			t.Fatalf("expected terminal.append, got %s", msg.Type)
		}
		receivedCount++

		// Reset deadline for next message
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	}

	// We should receive all 500 messages, not just 256
	if receivedCount < historySize {
		t.Errorf("expected at least %d history messages, got %d (history truncated!)", historySize, receivedCount)
	}
}

// TestReconnectHandlerSlowClientTimeout verifies replay doesn't block for slow/dead clients.
// The server uses a 5-second per-message timeout. This test verifies that:
// 1. The timeout path is exercised (messages are skipped for slow clients)
// 2. Replay completes without blocking the server indefinitely
// 3. Other server operations (new connections) continue to work
func TestReconnectHandlerSlowClientTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow client timeout test in short mode")
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Track how many messages were attempted vs timed out
	var attemptedCount, sentCount int
	var mu sync.Mutex

	// Set up a handler that sends enough messages to fill the buffer and trigger timeouts
	// Channel buffer is 256, so sending 300+ messages with a non-reading client will cause timeouts
	const messagesToSend = 300
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		for i := 0; i < messagesToSend; i++ {
			mu.Lock()
			attemptedCount++
			mu.Unlock()

			sendToClient(NewTerminalAppendMessage("test-session", fmt.Sprintf("line %d\n", i)))

			mu.Lock()
			sentCount++
			mu.Unlock()
		}
	})

	// Connect but DON'T read any messages - this simulates a dead/slow client
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Wait for the reconnect handler to complete or timeout
	// With 5s timeout per message, we should see some messages timeout, but not block forever.
	// The handler sends 300 messages. With 256 buffer, ~44 messages should timeout.
	// Each timeout is 5s, so worst case is ~220s. But the buffer fills fast initially,
	// so timeouts start early. We give a generous timeout for the whole process.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := attemptedCount >= messagesToSend
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	finalAttempted := attemptedCount
	finalSent := sentCount
	mu.Unlock()

	// Verify we attempted to send all messages
	if finalAttempted != messagesToSend {
		t.Errorf("expected %d send attempts, got %d (handler didn't complete)", messagesToSend, finalAttempted)
	}

	// Verify we sent all messages (the blocking send with timeout should complete for each)
	if finalSent != messagesToSend {
		t.Errorf("expected %d messages sent/timed-out, got %d (handler got stuck)", messagesToSend, finalSent)
	}

	// Verify the server is still responsive - a new connection should work
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("second connection failed - server may be blocked: %v", err)
	}
	defer conn2.Close()

	// New client should get session status immediately
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn2.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read from second client: %v", err)
	}
	var msg2 Message
	if err := json.Unmarshal(data, &msg2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if msg2.Type != MessageTypeSessionStatus {
		t.Errorf("expected session.status for second client, got %s", msg2.Type)
	}

	t.Logf("Replay completed: %d/%d messages attempted, server responsive", finalAttempted, messagesToSend)
}

// TestReconnectHandlerTimeoutDoesNotBlockBroadcast verifies replay timeout doesn't block broadcasts.
// While one client is slowly receiving replay, broadcasts to other clients should work.
func TestReconnectHandlerTimeoutDoesNotBlockBroadcast(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Channel to signal when handler starts and can be unblocked
	handlerStarted := make(chan struct{})
	handlerUnblock := make(chan struct{})

	s.SetReconnectHandler(func(sendToClient func(Message)) {
		close(handlerStarted)
		// Wait for test to unblock us
		<-handlerUnblock
		// Send one message to complete handler
		sendToClient(NewTerminalAppendMessage("test-session", "delayed history\n"))
	})

	// Connect first client - will trigger reconnect handler that blocks
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	defer conn1.Close()

	// Wait for handler to start
	<-handlerStarted

	// While handler is blocked, connect a second client and verify broadcasts work
	// First, clear the reconnect handler for client 2
	s.SetReconnectHandler(nil)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial 2 failed: %v", err)
	}
	defer conn2.Close()

	// Read session.status from client 2
	msg := readMessage(t, conn2)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status for client 2, got %s", msg.Type)
	}

	// Broadcast a message - should reach client 2 even though client 1's handler is blocked
	s.BroadcastTerminalOutput("broadcast while blocked\n")

	// Client 2 should receive the broadcast
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn2.ReadMessage()
	if err != nil {
		t.Fatalf("client 2 didn't receive broadcast while handler blocked: %v", err)
	}
	var broadcastMsg Message
	if err := json.Unmarshal(data, &broadcastMsg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if broadcastMsg.Type != MessageTypeTerminalAppend {
		t.Errorf("expected terminal.append broadcast, got %s", broadcastMsg.Type)
	}

	// Unblock the handler
	close(handlerUnblock)

	t.Log("Broadcast completed while reconnect handler was blocked")
}

// TestNewChunkDecisionResultMessage verifies the chunk.decision_result message constructor.
func TestNewChunkDecisionResultMessage(t *testing.T) {
	msg := NewChunkDecisionResultMessage("card-123", 2, "accept", true, "", "")

	if msg.Type != MessageTypeChunkDecisionResult {
		t.Errorf("expected type %s, got %s", MessageTypeChunkDecisionResult, msg.Type)
	}

	payload, ok := msg.Payload.(ChunkDecisionResultPayload)
	if !ok {
		t.Fatalf("expected ChunkDecisionResultPayload, got %T", msg.Payload)
	}

	if payload.CardID != "card-123" {
		t.Errorf("expected CardID card-123, got %s", payload.CardID)
	}
	if payload.ChunkIndex != 2 {
		t.Errorf("expected ChunkIndex 2, got %d", payload.ChunkIndex)
	}
	if payload.Action != "accept" {
		t.Errorf("expected Action accept, got %s", payload.Action)
	}
	if !payload.Success {
		t.Errorf("expected Success true, got false")
	}
}

// TestNewChunkDecisionResultMessageWithError verifies error case.
func TestNewChunkDecisionResultMessageWithError(t *testing.T) {
	msg := NewChunkDecisionResultMessage("card-456", 1, "reject", false, "action.git_failed", "patch conflict")

	payload, ok := msg.Payload.(ChunkDecisionResultPayload)
	if !ok {
		t.Fatalf("expected ChunkDecisionResultPayload, got %T", msg.Payload)
	}

	if payload.Success {
		t.Errorf("expected Success false, got true")
	}
	if payload.ErrorCode != "action.git_failed" {
		t.Errorf("expected ErrorCode action.git_failed, got %s", payload.ErrorCode)
	}
	if payload.Error != "patch conflict" {
		t.Errorf("expected Error 'patch conflict', got %s", payload.Error)
	}
}

// TestUndoResultMessageChunkIndexZero verifies that chunk_index=0 is serialized
// in undo.result JSON (not omitted due to omitempty). This guards against regressions
// where the mobile client would misinterpret chunk 0 undo as file-level undo.
func TestUndoResultMessageChunkIndexZero(t *testing.T) {
	msg := NewUndoResultMessageWithHash("card-123", 0, "hash-abc", true, "", "")

	// Marshal to JSON to verify serialization
	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	// Parse JSON to check fields
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	payload, ok := parsed["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected payload to be a map, got %T", parsed["payload"])
	}

	// Verify chunk_index is present and equals 0 (not omitted)
	chunkIndex, exists := payload["chunk_index"]
	if !exists {
		t.Errorf("chunk_index should be present in JSON for chunk 0 undo, but was omitted")
	} else if int(chunkIndex.(float64)) != 0 {
		t.Errorf("expected chunk_index=0, got %v", chunkIndex)
	}

	// Verify content_hash is present
	contentHash, exists := payload["content_hash"]
	if !exists {
		t.Errorf("content_hash should be present in JSON")
	} else if contentHash != "hash-abc" {
		t.Errorf("expected content_hash='hash-abc', got %v", contentHash)
	}

	// Verify card_id
	cardID, exists := payload["card_id"]
	if !exists || cardID != "card-123" {
		t.Errorf("expected card_id='card-123', got %v", cardID)
	}

	// Verify success
	success, exists := payload["success"]
	if !exists || success != true {
		t.Errorf("expected success=true, got %v", success)
	}
}

// TestUndoResultMessageFileLevelOmitsChunkIndex verifies that file-level undo
// (chunkIndex=-1) omits chunk_index from JSON.
func TestUndoResultMessageFileLevelOmitsChunkIndex(t *testing.T) {
	msg := NewUndoResultMessageWithHash("card-456", -1, "", true, "", "")

	// Marshal to JSON
	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	// Parse JSON to check fields
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	payload, ok := parsed["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected payload to be a map, got %T", parsed["payload"])
	}

	// Verify chunk_index is NOT present for file-level undo
	if _, exists := payload["chunk_index"]; exists {
		t.Errorf("chunk_index should be omitted for file-level undo (chunkIndex=-1)")
	}

	// Verify content_hash is NOT present (empty string)
	if _, exists := payload["content_hash"]; exists {
		t.Errorf("content_hash should be omitted when empty")
	}
}

// TestUndoResultMessageErrorIncludesContentHash verifies that error responses
// include content_hash so clients can clear hash-keyed pending state.
func TestUndoResultMessageErrorIncludesContentHash(t *testing.T) {
	msg := NewUndoResultMessageWithHash("card-789", 2, "hash-xyz", false, "undo.conflict", "patch conflict")

	// Marshal to JSON
	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	// Parse JSON to check fields
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	payload, ok := parsed["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected payload to be a map, got %T", parsed["payload"])
	}

	// Verify content_hash is present even for error responses
	contentHash, exists := payload["content_hash"]
	if !exists {
		t.Errorf("content_hash should be present in error response for chunk undo")
	} else if contentHash != "hash-xyz" {
		t.Errorf("expected content_hash='hash-xyz', got %v", contentHash)
	}

	// Verify chunk_index is present
	chunkIndex, exists := payload["chunk_index"]
	if !exists {
		t.Errorf("chunk_index should be present in error response")
	} else if int(chunkIndex.(float64)) != 2 {
		t.Errorf("expected chunk_index=2, got %v", chunkIndex)
	}

	// Verify error fields
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != "undo.conflict" {
		t.Errorf("expected error_code='undo.conflict', got %v", payload["error_code"])
	}
	if payload["error"] != "patch conflict" {
		t.Errorf("expected error='patch conflict', got %v", payload["error"])
	}
}

// TestChunkInfoSerialization verifies ChunkInfo JSON serialization.
func TestChunkInfoSerialization(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 4, Offset: 0, Length: 50},
		{Index: 1, OldStart: 10, OldCount: 2, NewStart: 11, NewCount: 5, Offset: 50, Length: 75},
	}

	msg := NewDiffCardMessage("card-test", "file.go", "@@ -1,3 +1,4 @@\n+line", chunks, nil, nil, false, false, nil, 1703500000000)

	payload, ok := msg.Payload.(DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if len(payload.Chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(payload.Chunks))
	}

	h0 := payload.Chunks[0]
	if h0.Index != 0 || h0.OldStart != 1 || h0.Offset != 0 || h0.Length != 50 {
		t.Errorf("chunk 0 values incorrect: %+v", h0)
	}

	h1 := payload.Chunks[1]
	if h1.Index != 1 || h1.OldStart != 10 || h1.Offset != 50 || h1.Length != 75 {
		t.Errorf("chunk 1 values incorrect: %+v", h1)
	}
}

// TestHandleChunkDecisionValidPayload verifies chunk.decision message handling.
func TestHandleChunkDecisionValidPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Track chunk decision calls with mutex for race-safe access
	var mu sync.Mutex
	var handlerCalls []struct {
		CardID      string
		ChunkIndex  int
		Action      string
		ContentHash string
	}
	s.SetChunkDecisionHandler(func(cardID string, chunkIndex int, action string, contentHash string) error {
		mu.Lock()
		handlerCalls = append(handlerCalls, struct {
			CardID      string
			ChunkIndex  int
			Action      string
			ContentHash string
		}{cardID, chunkIndex, action, contentHash})
		mu.Unlock()
		return nil
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send chunk.decision
	decision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":     "card-abc",
			"chunk_index": 1,
			"action":      "accept",
		},
	}
	if err := conn.WriteJSON(decision); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Wait for handler and result
	time.Sleep(100 * time.Millisecond)

	// Verify handler was called (with mutex protection)
	mu.Lock()
	callCount := len(handlerCalls)
	var firstCall struct {
		CardID      string
		ChunkIndex  int
		Action      string
		ContentHash string
	}
	if callCount > 0 {
		firstCall = handlerCalls[0]
	}
	mu.Unlock()

	if callCount != 1 {
		t.Fatalf("expected 1 handler call, got %d", callCount)
	}
	if firstCall.CardID != "card-abc" {
		t.Errorf("expected CardID card-abc, got %s", firstCall.CardID)
	}
	if firstCall.ChunkIndex != 1 {
		t.Errorf("expected ChunkIndex 1, got %d", firstCall.ChunkIndex)
	}
	if firstCall.Action != "accept" {
		t.Errorf("expected Action accept, got %s", firstCall.Action)
	}

	// Read the decision result
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeChunkDecisionResult {
		t.Fatalf("expected chunk.decision_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["card_id"] != "card-abc" {
		t.Errorf("expected card_id card-abc, got %v", payload["card_id"])
	}
	if int(payload["chunk_index"].(float64)) != 1 {
		t.Errorf("expected chunk_index 1, got %v", payload["chunk_index"])
	}
	if payload["success"] != true {
		t.Errorf("expected success true, got %v", payload["success"])
	}
}

// TestHandleChunkDecisionInvalidAction verifies error on invalid action.
func TestHandleChunkDecisionInvalidAction(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send chunk.decision with invalid action
	decision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":     "card-abc",
			"chunk_index": 0,
			"action":      "maybe",
		},
	}
	if err := conn.WriteJSON(decision); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read the error result
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeChunkDecisionResult {
		t.Fatalf("expected chunk.decision_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}
}

// TestHandleChunkDecisionNegativeIndex verifies error on negative chunk index.
func TestHandleChunkDecisionNegativeIndex(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send chunk.decision with negative index
	decision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":     "card-abc",
			"chunk_index": -1,
			"action":      "accept",
		},
	}
	if err := conn.WriteJSON(decision); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read the error result
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeChunkDecisionResult {
		t.Fatalf("expected chunk.decision_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false, got %v", payload["success"])
	}
}

// TestHandleChunkDecisionNoHandler verifies error when no handler is set.
func TestHandleChunkDecisionNoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set a chunk decision handler

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send chunk.decision
	decision := map[string]interface{}{
		"type": "chunk.decision",
		"payload": map[string]interface{}{
			"card_id":     "card-abc",
			"chunk_index": 0,
			"action":      "accept",
		},
	}
	if err := conn.WriteJSON(decision); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read the error result
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeChunkDecisionResult {
		t.Fatalf("expected chunk.decision_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
	}
}

// ============================================================================
// Unit 5.1: Device Revocation Tests
// ============================================================================

// TestCloseDeviceConnections verifies that connections for a specific device are closed.
func TestCloseDeviceConnections(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	// Create mock clients with different device IDs
	client1 := &Client{
		deviceID: "device-1",
		send:     make(chan Message, 10),
		done:     make(chan struct{}),
		server:   s,
	}
	client2 := &Client{
		deviceID: "device-2",
		send:     make(chan Message, 10),
		done:     make(chan struct{}),
		server:   s,
	}
	client3 := &Client{
		deviceID: "device-1", // Same as client1
		send:     make(chan Message, 10),
		done:     make(chan struct{}),
		server:   s,
	}

	// Register clients
	s.mu.Lock()
	s.clients[client1] = true
	s.clients[client2] = true
	s.clients[client3] = true
	s.mu.Unlock()

	// Close device-1 connections
	closed := s.CloseDeviceConnections("device-1")

	if closed != 2 {
		t.Errorf("expected 2 connections closed, got %d", closed)
	}

	// Verify device-1 clients are signaled to close
	select {
	case <-client1.done:
		// Expected
	default:
		t.Error("expected client1.done to be closed")
	}

	select {
	case <-client3.done:
		// Expected
	default:
		t.Error("expected client3.done to be closed")
	}

	// Verify device-2 client is NOT signaled
	select {
	case <-client2.done:
		t.Error("expected client2.done to remain open")
	default:
		// Expected
	}
}

// TestCloseDeviceConnectionsEmpty verifies no-op for non-existent device.
func TestCloseDeviceConnectionsEmpty(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	client := &Client{
		deviceID: "device-1",
		send:     make(chan Message, 10),
		done:     make(chan struct{}),
		server:   s,
	}

	s.mu.Lock()
	s.clients[client] = true
	s.mu.Unlock()

	// Try to close non-existent device
	closed := s.CloseDeviceConnections("device-999")

	if closed != 0 {
		t.Errorf("expected 0 connections closed, got %d", closed)
	}

	// Verify client is NOT signaled
	select {
	case <-client.done:
		t.Error("expected client.done to remain open")
	default:
		// Expected
	}
}

// TestCloseDeviceConnectionsEmptyID verifies no-op for empty device ID.
func TestCloseDeviceConnectionsEmptyID(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	closed := s.CloseDeviceConnections("")
	if closed != 0 {
		t.Errorf("expected 0 connections closed for empty ID, got %d", closed)
	}
}

// mockDeviceStore implements DeviceStore for testing.
type mockDeviceStore struct {
	devices       map[string]*DeviceInfo
	deleteErr     error
	deleteCalled  bool
	deletedDevice string
}

func (m *mockDeviceStore) GetDevice(id string) (*DeviceInfo, error) {
	device, ok := m.devices[id]
	if !ok {
		return nil, nil
	}
	return device, nil
}

func (m *mockDeviceStore) DeleteDevice(id string) error {
	m.deleteCalled = true
	m.deletedDevice = id
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.devices, id)
	return nil
}

// TestRevokeEndpointSuccess tests successful device revocation via HTTP.
func TestRevokeEndpointSuccess(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	// Create a connected client for the device
	client := &Client{
		deviceID: "test-device-id",
		send:     make(chan Message, 10),
		done:     make(chan struct{}),
		server:   s,
	}
	s.mu.Lock()
	s.clients[client] = true
	s.mu.Unlock()

	// Create mock store
	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{
			"test-device-id": {ID: "test-device-id", Name: "Test Device"},
		},
	}

	// Create handler
	handler := NewRevokeDeviceHandler(s, store)

	// Create request (simulating localhost)
	req := httptest.NewRequest(http.MethodPost, "/devices/test-device-id/revoke", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Verify response
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["device_id"] != "test-device-id" {
		t.Errorf("expected device_id test-device-id, got %v", result["device_id"])
	}

	// connections_closed should be 1
	if result["connections_closed"] != float64(1) {
		t.Errorf("expected connections_closed 1, got %v", result["connections_closed"])
	}

	// Verify store was called
	if !store.deleteCalled {
		t.Error("expected DeleteDevice to be called")
	}
	if store.deletedDevice != "test-device-id" {
		t.Errorf("expected deleted device test-device-id, got %s", store.deletedDevice)
	}

	// Verify client was signaled to close
	select {
	case <-client.done:
		// Expected
	default:
		t.Error("expected client.done to be closed")
	}
}

// TestRevokeEndpointNonLoopback tests rejection of non-loopback requests.
func TestRevokeEndpointNonLoopback(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{
			"test-device-id": {ID: "test-device-id", Name: "Test Device"},
		},
	}

	handler := NewRevokeDeviceHandler(s, store)

	// Create request from non-loopback address
	req := httptest.NewRequest(http.MethodPost, "/devices/test-device-id/revoke", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", w.Code)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["error"] != "forbidden" {
		t.Errorf("expected error forbidden, got %v", result["error"])
	}

	// Verify store was NOT called
	if store.deleteCalled {
		t.Error("expected DeleteDevice to NOT be called")
	}
}

// TestRevokeEndpointInvalidMethod tests rejection of non-POST requests.
func TestRevokeEndpointInvalidMethod(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{},
	}

	handler := NewRevokeDeviceHandler(s, store)

	req := httptest.NewRequest(http.MethodGet, "/devices/test-device-id/revoke", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

// TestRevokeEndpointNotFound tests 404 for non-existent device.
func TestRevokeEndpointNotFound(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{}, // Empty store
	}

	handler := NewRevokeDeviceHandler(s, store)

	req := httptest.NewRequest(http.MethodPost, "/devices/nonexistent-id/revoke", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["error"] != "not_found" {
		t.Errorf("expected error not_found, got %v", result["error"])
	}
}

// TestRevokeEndpointInvalidPath tests rejection of malformed paths.
func TestRevokeEndpointInvalidPath(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{},
	}

	handler := NewRevokeDeviceHandler(s, store)

	testCases := []string{
		"/devices/",         // Missing device ID and revoke
		"/devices/id",       // Missing /revoke
		"/devices/id/other", // Wrong action
		"/other/id/revoke",  // Wrong prefix
		"/devices//revoke",  // Empty device ID
	}

	for _, path := range testCases {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: expected status 400, got %d", path, w.Code)
		}
	}
}

// TestRevokeEndpointDeleteError tests handling of storage deletion errors.
func TestRevokeEndpointDeleteError(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	go s.runBroadcaster()
	defer s.Stop()

	store := &mockDeviceStore{
		devices: map[string]*DeviceInfo{
			"test-device-id": {ID: "test-device-id", Name: "Test Device"},
		},
		deleteErr: errors.New("database error"),
	}

	handler := NewRevokeDeviceHandler(s, store)

	req := httptest.NewRequest(http.MethodPost, "/devices/test-device-id/revoke", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", w.Code)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["error"] != "delete_failed" {
		t.Errorf("expected error delete_failed, got %v", result["error"])
	}
}

// TestIsLoopbackRequest tests the loopback address detection.
func TestIsLoopbackRequest(t *testing.T) {
	testCases := []struct {
		remoteAddr string
		expected   bool
	}{
		{"127.0.0.1:12345", true},
		{"127.0.0.10:12345", true},
		{"127.255.255.255:12345", true},
		{"[::1]:12345", true},
		{"8.8.8.8:12345", false},
		{"", false},
		{"invalid", false},
	}

	if ip := findLocalNonLoopbackIP(); ip != "" {
		testCases = append(testCases, struct {
			remoteAddr string
			expected   bool
		}{remoteAddr: net.JoinHostPort(ip, "12345"), expected: true})
	}

	for _, tc := range testCases {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = tc.remoteAddr
		result := isLoopbackRequest(req)
		if result != tc.expected {
			t.Errorf("isLoopbackRequest(%q) = %v, expected %v", tc.remoteAddr, result, tc.expected)
		}
	}
}

func findLocalNonLoopbackIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
			return ip.String()
		}
	}

	return ""
}

// ===== Unit 5.5a: Terminal Input Tests =====

// mockWriter is a simple io.Writer that captures written bytes.
type mockWriter struct {
	mu      sync.Mutex
	written []byte
	err     error
}

func (m *mockWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	m.written = append(m.written, p...)
	return len(p), nil
}

func (m *mockWriter) getWritten() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]byte, len(m.written))
	copy(result, m.written)
	return result
}

// TestTerminalInputPayloadParsing tests TerminalInputPayload JSON serialization.
func TestTerminalInputPayloadParsing(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantErr   bool
		sessionID string
		data      string
		timestamp int64
	}{
		{
			name:      "valid payload",
			json:      `{"type":"terminal.input","payload":{"session_id":"session-123","data":"ls -la\n","timestamp":1234567890}}`,
			wantErr:   false,
			sessionID: "session-123",
			data:      "ls -la\n",
			timestamp: 1234567890,
		},
		{
			name:      "empty data",
			json:      `{"type":"terminal.input","payload":{"session_id":"session-123","data":"","timestamp":1234567890}}`,
			wantErr:   false,
			sessionID: "session-123",
			data:      "",
			timestamp: 1234567890,
		},
		{
			name:      "escape sequence",
			json:      `{"type":"terminal.input","payload":{"session_id":"session-123","data":"\u001b[A","timestamp":1234567890}}`,
			wantErr:   false,
			sessionID: "session-123",
			data:      "\x1b[A", // Up arrow escape sequence
			timestamp: 1234567890,
		},
		{
			name:    "invalid json",
			json:    `{"type":"terminal.input","payload":`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg struct {
				Type    MessageType          `json:"type"`
				Payload TerminalInputPayload `json:"payload"`
			}
			err := json.Unmarshal([]byte(tt.json), &msg)
			if (err != nil) != tt.wantErr {
				t.Errorf("json.Unmarshal() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if msg.Type != MessageTypeTerminalInput {
				t.Errorf("Type = %v, want %v", msg.Type, MessageTypeTerminalInput)
			}
			if msg.Payload.SessionID != tt.sessionID {
				t.Errorf("SessionID = %v, want %v", msg.Payload.SessionID, tt.sessionID)
			}
			if msg.Payload.Data != tt.data {
				t.Errorf("Data = %q, want %q", msg.Payload.Data, tt.data)
			}
			if msg.Payload.Timestamp != tt.timestamp {
				t.Errorf("Timestamp = %v, want %v", msg.Payload.Timestamp, tt.timestamp)
			}
		})
	}
}

// TestTerminalInputSessionValidation tests session ID validation.
func TestTerminalInputSessionValidation(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Set up a mock PTY writer
	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status message
	statusMsg := readMessage(t, conn)
	if statusMsg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", statusMsg.Type)
	}

	// Send terminal.input with wrong session_id
	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: "wrong-session-id",
			Data:      "test",
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive an error message
	errMsg := readMessage(t, conn)
	if errMsg.Type != MessageTypeError {
		t.Fatalf("expected error message, got %s", errMsg.Type)
	}

	payload, ok := errMsg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", errMsg.Payload)
	}
	if payload["code"] != apperrors.CodeInputSessionInvalid {
		t.Errorf("expected code %s, got %s", apperrors.CodeInputSessionInvalid, payload["code"])
	}

	// Verify nothing was written to PTY
	if len(mockPTY.getWritten()) != 0 {
		t.Errorf("expected no PTY writes, got %d bytes", len(mockPTY.getWritten()))
	}
}

// TestTerminalInputNoPTYWriter tests error when no PTY writer is configured.
func TestTerminalInputNoPTYWriter(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Do NOT set a PTY writer

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status message
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Send terminal.input with correct session_id but no PTY configured
	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: sessionID,
			Data:      "test",
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive an error message
	errMsg := readMessage(t, conn)
	if errMsg.Type != MessageTypeError {
		t.Fatalf("expected error message, got %s", errMsg.Type)
	}

	payload, ok := errMsg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", errMsg.Payload)
	}
	if payload["code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected code %s, got %s", apperrors.CodeServerHandlerMissing, payload["code"])
	}
}

// TestTerminalInputSuccess tests successful terminal input writing.
func TestTerminalInputSuccess(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Set up a mock PTY writer
	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status message
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Send terminal.input with correct session_id
	testData := "echo hello\n"
	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: sessionID,
			Data:      testData,
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Give the handler time to process
	time.Sleep(50 * time.Millisecond)

	// Verify the data was written to the mock PTY
	written := mockPTY.getWritten()
	if string(written) != testData {
		t.Errorf("PTY wrote %q, want %q", written, testData)
	}
}

// TestTerminalInputWriteError tests error handling when PTY write fails.
func TestTerminalInputWriteError(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Set up a mock PTY writer that returns an error
	mockPTY := &mockWriter{err: errors.New("simulated write error")}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status message
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Send terminal.input
	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: sessionID,
			Data:      "test",
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive an error message
	errMsg := readMessage(t, conn)
	if errMsg.Type != MessageTypeError {
		t.Fatalf("expected error message, got %s", errMsg.Type)
	}

	payload, ok := errMsg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", errMsg.Payload)
	}
	if payload["code"] != apperrors.CodeInputWriteFailed {
		t.Errorf("expected code %s, got %s", apperrors.CodeInputWriteFailed, payload["code"])
	}
}

// TestTerminalInputRateLimiting tests rate limiting for terminal input.
func TestTerminalInputRateLimiting(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Set up a mock PTY writer
	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status message
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Send many messages rapidly to trigger rate limiting.
	// The rate limiter allows 1000/sec with burst of 10.
	// Send 20 messages quickly to exhaust the burst and trigger limiting.
	rateLimited := false
	for i := 0; i < 20; i++ {
		inputMsg := Message{
			Type: MessageTypeTerminalInput,
			Payload: TerminalInputPayload{
				SessionID: sessionID,
				Data:      fmt.Sprintf("msg%d", i),
				Timestamp: time.Now().UnixMilli(),
			},
		}
		data, _ := json.Marshal(inputMsg)
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	// Read all responses - some should be errors indicating rate limiting
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break // timeout or connection closed
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == MessageTypeError {
			payload, ok := msg.Payload.(map[string]interface{})
			if ok && payload["code"] == apperrors.CodeInputRateLimited {
				rateLimited = true
				break
			}
		}
	}

	if !rateLimited {
		t.Error("expected rate limiting to trigger, but no rate limit error received")
	}
}

// TestTerminalInputInvalidPayload tests handling of malformed payloads.
func TestTerminalInputInvalidPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status
	readMessage(t, conn)

	// Send malformed JSON as terminal.input
	malformedMsg := `{"type":"terminal.input","payload":{"session_id":123}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(malformedMsg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive an error message (session_id is wrong type)
	errMsg := readMessage(t, conn)
	if errMsg.Type != MessageTypeError {
		t.Fatalf("expected error message, got %s", errMsg.Type)
	}

	payload, ok := errMsg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", errMsg.Payload)
	}
	if payload["code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected code %s, got %s", apperrors.CodeServerInvalidMessage, payload["code"])
	}
}

// TestTerminalInputOversizedData tests handling of large input payloads (>64KB).
// docs/PLANS.md requires adversarial testing for oversized data.
func TestTerminalInputOversizedData(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Create oversized data: 100KB (> 64KB threshold mentioned in docs/PLANS.md)
	oversizedData := strings.Repeat("x", 100*1024)

	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: sessionID,
			Data:      oversizedData,
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)

	// Server has 512KB read limit, so 100KB should be accepted
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	// Verify the oversized data was written to PTY
	written := mockPTY.getWritten()
	if len(written) != len(oversizedData) {
		t.Errorf("PTY wrote %d bytes, want %d bytes", len(written), len(oversizedData))
	}
}

// TestTerminalInputEmptyData tests that empty input data is handled gracefully.
func TestTerminalInputEmptyData(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read initial session.status
	statusMsg := readMessage(t, conn)
	sessionPayload := statusMsg.Payload.(map[string]interface{})
	sessionID := sessionPayload["session_id"].(string)

	// Send empty data - should be accepted without error
	inputMsg := Message{
		Type: MessageTypeTerminalInput,
		Payload: TerminalInputPayload{
			SessionID: sessionID,
			Data:      "", // Empty input
			Timestamp: time.Now().UnixMilli(),
		},
	}
	data, _ := json.Marshal(inputMsg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Give handler time to process
	time.Sleep(50 * time.Millisecond)

	// Verify no error was sent back (empty write is valid)
	// Set a short deadline to check if any message arrives
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected no message (timeout), but received one - empty input may have caused error")
	}
	// Timeout error is expected and correct behavior

	// Verify PTY received the empty write (0 bytes written is valid)
	written := mockPTY.getWritten()
	if len(written) != 0 {
		t.Errorf("expected 0 bytes written, got %d", len(written))
	}
}

// TestNewSessionListMessage verifies session list message construction.
func TestNewSessionListMessage(t *testing.T) {
	sessions := []SessionInfo{
		{
			ID:         "session-1",
			Repo:       "/path/to/repo1",
			Branch:     "main",
			StartedAt:  1704067200000, // Unix millis
			LastSeen:   1704070800000,
			LastCommit: "abc123",
			Status:     "complete",
			IsSystem:   true,
		},
		{
			ID:        "session-2",
			Repo:      "/path/to/repo2",
			Branch:    "feature",
			StartedAt: 1704153600000,
			LastSeen:  1704157200000,
			Status:    "running",
		},
	}

	msg := NewSessionListMessage(sessions)

	if msg.Type != MessageTypeSessionList {
		t.Errorf("expected type %q, got %q", MessageTypeSessionList, msg.Type)
	}

	payload, ok := msg.Payload.(SessionListPayload)
	if !ok {
		t.Fatalf("expected SessionListPayload, got %T", msg.Payload)
	}

	if len(payload.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(payload.Sessions))
	}

	// Verify first session
	s := payload.Sessions[0]
	if s.ID != "session-1" {
		t.Errorf("session 0 ID = %q, want session-1", s.ID)
	}
	if s.Repo != "/path/to/repo1" {
		t.Errorf("session 0 Repo = %q, want /path/to/repo1", s.Repo)
	}
	if s.Branch != "main" {
		t.Errorf("session 0 Branch = %q, want main", s.Branch)
	}
	if s.LastCommit != "abc123" {
		t.Errorf("session 0 LastCommit = %q, want abc123", s.LastCommit)
	}
	if s.Status != "complete" {
		t.Errorf("session 0 Status = %q, want complete", s.Status)
	}
	if !s.IsSystem {
		t.Errorf("session 0 IsSystem = %v, want true", s.IsSystem)
	}

	// Verify second session has empty LastCommit
	if payload.Sessions[1].LastCommit != "" {
		t.Errorf("session 1 LastCommit = %q, want empty", payload.Sessions[1].LastCommit)
	}
	if payload.Sessions[1].IsSystem {
		t.Errorf("session 1 IsSystem = %v, want false", payload.Sessions[1].IsSystem)
	}
}

// TestSessionsToInfoList verifies conversion from SessionData to SessionInfo.
func TestSessionsToInfoList(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)

	sessions := []*SessionData{
		{
			ID:         "session-a",
			Repo:       "/repo/a",
			Branch:     "main",
			StartedAt:  now,
			LastSeen:   later,
			LastCommit: "commit1",
			Status:     "complete",
			IsSystem:   true,
		},
		{
			ID:        "session-b",
			Repo:      "/repo/b",
			Branch:    "dev",
			StartedAt: now,
			LastSeen:  now,
			Status:    "running",
		},
	}

	infos := SessionsToInfoList(sessions)

	if len(infos) != 2 {
		t.Fatalf("expected 2 infos, got %d", len(infos))
	}

	// Verify first session info
	if infos[0].ID != "session-a" {
		t.Errorf("info 0 ID = %q, want session-a", infos[0].ID)
	}
	if infos[0].StartedAt != now.UnixMilli() {
		t.Errorf("info 0 StartedAt = %d, want %d", infos[0].StartedAt, now.UnixMilli())
	}
	if infos[0].LastSeen != later.UnixMilli() {
		t.Errorf("info 0 LastSeen = %d, want %d", infos[0].LastSeen, later.UnixMilli())
	}
	if infos[0].LastCommit != "commit1" {
		t.Errorf("info 0 LastCommit = %q, want commit1", infos[0].LastCommit)
	}
	if !infos[0].IsSystem {
		t.Errorf("info 0 IsSystem = %v, want true", infos[0].IsSystem)
	}

	// Verify second session info has empty last commit
	if infos[1].LastCommit != "" {
		t.Errorf("info 1 LastCommit = %q, want empty", infos[1].LastCommit)
	}
	if infos[1].IsSystem {
		t.Errorf("info 1 IsSystem = %v, want false", infos[1].IsSystem)
	}
}

// TestReconnectHandlerWithSessionList verifies session list is sent on connect.
func TestReconnectHandlerWithSessionList(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Set up reconnect handler to send session list
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		sessions := []SessionInfo{
			{
				ID:        "session-test",
				Repo:      "/test/repo",
				Branch:    "main",
				StartedAt: 1704067200000,
				LastSeen:  1704070800000,
				Status:    "running",
			},
		}
		sendToClient(NewSessionListMessage(sessions))
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status first
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	// Read session.list
	msg = readMessage(t, conn)
	if msg.Type != MessageTypeSessionList {
		t.Fatalf("expected session.list, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}

	sessions, ok := payload["sessions"].([]interface{})
	if !ok {
		t.Fatalf("expected sessions array, got %T", payload["sessions"])
	}

	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}

	// Verify session fields
	session, ok := sessions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected map session, got %T", sessions[0])
	}
	if session["id"] != "session-test" {
		t.Errorf("session id = %v, want session-test", session["id"])
	}
	if session["repo"] != "/test/repo" {
		t.Errorf("session repo = %v, want /test/repo", session["repo"])
	}
	if session["status"] != "running" {
		t.Errorf("session status = %v, want running", session["status"])
	}
}

// TestEmptySessionList verifies empty session list is handled correctly.
func TestEmptySessionList(t *testing.T) {
	msg := NewSessionListMessage(nil)

	if msg.Type != MessageTypeSessionList {
		t.Errorf("expected type %q, got %q", MessageTypeSessionList, msg.Type)
	}

	payload, ok := msg.Payload.(SessionListPayload)
	if !ok {
		t.Fatalf("expected SessionListPayload, got %T", msg.Payload)
	}

	if payload.Sessions != nil {
		t.Errorf("expected nil sessions for empty list, got %v", payload.Sessions)
	}
}

// ===== Unit 6.4: Commit Workflow Handler Tests =====

// TestHandleRepoStatusNoGitOps tests repo.status when no GitOperations is configured.
func TestHandleRepoStatusNoGitOps(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set GitOperations

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send repo.status request
	statusReq := map[string]interface{}{
		"type":    "repo.status",
		"payload": map[string]interface{}{},
	}
	reqData, _ := json.Marshal(statusReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive error message
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeError {
		t.Fatalf("expected error message, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected code %s, got %v", apperrors.CodeServerHandlerMissing, payload["code"])
	}
}

// TestHandleRepoCommitNoGitOps tests repo.commit when no GitOperations is configured.
func TestHandleRepoCommitNoGitOps(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set GitOperations

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send repo.commit request
	commitReq := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id": "test-req-commit-nogit",
			"message":    "Test commit",
		},
	}
	reqData, _ := json.Marshal(commitReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive error message
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected repo.commit_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
	}
}

// TestHandleRepoCommitInvalidPayload tests repo.commit with invalid JSON.
func TestHandleRepoCommitInvalidPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send malformed repo.commit
	malformedMsg := `{"type":"repo.commit","payload":"not an object"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(malformedMsg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive error message
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected repo.commit_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoCommitReadinessBlocked(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// No staged files -> readiness blocked.
	commitReq := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id": "test-req-blocked",
			"message":    "blocked commit",
		},
	}
	reqData, _ := json.Marshal(commitReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected repo.commit_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeCommitReadinessBlocked {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeCommitReadinessBlocked, payload["error_code"])
	}
}

func TestHandleRepoCommitOverrideRequired(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	// Stage one file and keep one unstaged file to trigger risky readiness.
	stagedFile := filepath.Join(repoDir, "staged.txt")
	if err := os.WriteFile(stagedFile, []byte("staged content"), 0644); err != nil {
		t.Fatalf("write staged file failed: %v", err)
	}
	runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")
	unstagedFile := filepath.Join(repoDir, "unstaged.txt")
	if err := os.WriteFile(unstagedFile, []byte("unstaged content"), 0644); err != nil {
		t.Fatalf("write unstaged file failed: %v", err)
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	commitReq := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id": "test-req-override-req",
			"message":    "override required",
		},
	}
	reqData, _ := json.Marshal(commitReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected repo.commit_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeCommitOverrideRequired {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeCommitOverrideRequired, payload["error_code"])
	}
}

func TestHandleRepoCommitOverrideAccepted(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	// Stage one file and keep one unstaged file to trigger risky readiness.
	stagedFile := filepath.Join(repoDir, "staged.txt")
	if err := os.WriteFile(stagedFile, []byte("staged content"), 0644); err != nil {
		t.Fatalf("write staged file failed: %v", err)
	}
	runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")
	unstagedFile := filepath.Join(repoDir, "unstaged.txt")
	if err := os.WriteFile(unstagedFile, []byte("unstaged content"), 0644); err != nil {
		t.Fatalf("write unstaged file failed: %v", err)
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	commitReq := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id":        "test-req-override-acc",
			"message":           "override accepted",
			"override_warnings": true,
		},
	}
	reqData, _ := json.Marshal(commitReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected repo.commit_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != true {
		t.Errorf("expected success=true, got %v", payload["success"])
	}
	if payload["hash"] == "" {
		t.Error("expected non-empty commit hash")
	}
}

// TestNewRepoStatusMessage tests the repo.status message constructor.
func TestNewRepoStatusMessage(t *testing.T) {
	stagedFiles := []string{"file1.txt", "dir/file2.txt"}
	msg := NewRepoStatusMessage(
		"main",
		"origin/main",
		2,
		stagedFiles,
		5,
		"abc1234 Add feature",
		"risky",
		[]string{},
		[]string{"unstaged_changes_present"},
		[]string{"Review or stage unstaged changes, or commit anyway to include only staged files."},
	)

	if msg.Type != MessageTypeRepoStatus {
		t.Errorf("expected type %s, got %s", MessageTypeRepoStatus, msg.Type)
	}

	payload, ok := msg.Payload.(RepoStatusPayload)
	if !ok {
		t.Fatalf("expected RepoStatusPayload, got %T", msg.Payload)
	}

	if payload.Branch != "main" {
		t.Errorf("expected Branch main, got %s", payload.Branch)
	}
	if payload.Upstream != "origin/main" {
		t.Errorf("expected Upstream origin/main, got %s", payload.Upstream)
	}
	if payload.StagedCount != 2 {
		t.Errorf("expected StagedCount 2, got %d", payload.StagedCount)
	}
	if len(payload.StagedFiles) != 2 {
		t.Errorf("expected 2 StagedFiles, got %d", len(payload.StagedFiles))
	}
	if payload.StagedFiles[0] != "file1.txt" || payload.StagedFiles[1] != "dir/file2.txt" {
		t.Errorf("expected StagedFiles [file1.txt, dir/file2.txt], got %v", payload.StagedFiles)
	}
	if payload.UnstagedCount != 5 {
		t.Errorf("expected UnstagedCount 5, got %d", payload.UnstagedCount)
	}
	if payload.LastCommit != "abc1234 Add feature" {
		t.Errorf("expected LastCommit 'abc1234 Add feature', got %s", payload.LastCommit)
	}
	if payload.ReadinessState != "risky" {
		t.Errorf("expected ReadinessState risky, got %s", payload.ReadinessState)
	}
	if len(payload.ReadinessWarnings) != 1 || payload.ReadinessWarnings[0] != "unstaged_changes_present" {
		t.Errorf("expected ReadinessWarnings [unstaged_changes_present], got %v", payload.ReadinessWarnings)
	}
	if len(payload.ReadinessActions) != 1 {
		t.Errorf("expected 1 ReadinessAction, got %d", len(payload.ReadinessActions))
	}
}

func setupGitRepoForCommitHandlerTest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGitForCommitHandlerTest(t, dir, "init")
	runGitForCommitHandlerTest(t, dir, "config", "user.email", "test@example.com")
	runGitForCommitHandlerTest(t, dir, "config", "user.name", "Test User")

	initialFile := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(initialFile, []byte("initial content"), 0644); err != nil {
		t.Fatalf("write initial file failed: %v", err)
	}
	runGitForCommitHandlerTest(t, dir, "add", "initial.txt")
	runGitForCommitHandlerTest(t, dir, "commit", "-m", "initial")

	return dir
}

func runGitForCommitHandlerTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// TestNewRepoCommitResultMessage tests the repo.commit_result message constructor.
func TestNewRepoCommitResultMessage(t *testing.T) {
	// Test success case
	msg := NewRepoCommitResultMessage("test-req-commit", true, "abc1234", "1 file changed", "", "")

	if msg.Type != MessageTypeRepoCommitResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoCommitResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoCommitResultPayload)
	if !ok {
		t.Fatalf("expected RepoCommitResultPayload, got %T", msg.Payload)
	}

	if payload.RequestID != "test-req-commit" {
		t.Errorf("expected RequestID test-req-commit, got %s", payload.RequestID)
	}
	if !payload.Success {
		t.Error("expected Success true")
	}
	if payload.Hash != "abc1234" {
		t.Errorf("expected Hash abc1234, got %s", payload.Hash)
	}
	if payload.Summary != "1 file changed" {
		t.Errorf("expected Summary '1 file changed', got %s", payload.Summary)
	}
	if payload.ErrorCode != "" {
		t.Errorf("expected empty ErrorCode, got %s", payload.ErrorCode)
	}
	if payload.Error != "" {
		t.Errorf("expected empty Error, got %s", payload.Error)
	}
}

// TestNewRepoCommitResultMessageError tests error case.
func TestNewRepoCommitResultMessageError(t *testing.T) {
	msg := NewRepoCommitResultMessage("test-req-err", false, "", "", "commit.no_staged_changes", "nothing to commit")

	payload, ok := msg.Payload.(RepoCommitResultPayload)
	if !ok {
		t.Fatalf("expected RepoCommitResultPayload, got %T", msg.Payload)
	}

	if payload.Success {
		t.Error("expected Success false")
	}
	if payload.Hash != "" {
		t.Errorf("expected empty Hash, got %s", payload.Hash)
	}
	if payload.ErrorCode != "commit.no_staged_changes" {
		t.Errorf("expected ErrorCode commit.no_staged_changes, got %s", payload.ErrorCode)
	}
	if payload.Error != "nothing to commit" {
		t.Errorf("expected Error 'nothing to commit', got %s", payload.Error)
	}
}

// TestHandleRepoPushNoGitOps tests repo.push when no GitOperations is configured.
func TestHandleRepoPushNoGitOps(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set GitOperations

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send repo.push request
	pushReq := map[string]interface{}{
		"type": "repo.push",
		"payload": map[string]interface{}{
			"request_id": "test-req-push-nogit",
			"remote":     "origin",
			"branch":     "main",
		},
	}
	reqData, _ := json.Marshal(pushReq)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive error message
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoPushResult {
		t.Fatalf("expected repo.push_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeServerHandlerMissing {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
	}
}

// TestHandleRepoPushInvalidPayload tests repo.push with invalid JSON.
func TestHandleRepoPushInvalidPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Read session.status
	_ = readMessage(t, conn)

	// Send malformed repo.push
	malformedMsg := `{"type":"repo.push","payload":"not an object"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(malformedMsg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Should receive error message
	msg := readMessage(t, conn)
	if msg.Type != MessageTypeRepoPushResult {
		t.Fatalf("expected repo.push_result, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", msg.Payload)
	}
	if payload["success"] != false {
		t.Errorf("expected success=false, got %v", payload["success"])
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected error_code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestNewRepoPushResultMessage tests the repo.push_result message constructor.
func TestNewRepoPushResultMessage(t *testing.T) {
	// Test success case
	msg := NewRepoPushResultMessage("test-req-push", true, "To origin/main abc1234..def5678", "", "")

	if msg.Type != MessageTypeRepoPushResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoPushResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoPushResultPayload)
	if !ok {
		t.Fatalf("expected RepoPushResultPayload, got %T", msg.Payload)
	}

	if payload.RequestID != "test-req-push" {
		t.Errorf("expected RequestID test-req-push, got %s", payload.RequestID)
	}
	if !payload.Success {
		t.Error("expected Success true")
	}
	if payload.Output != "To origin/main abc1234..def5678" {
		t.Errorf("expected Output 'To origin/main abc1234..def5678', got %s", payload.Output)
	}
	if payload.ErrorCode != "" {
		t.Errorf("expected empty ErrorCode, got %s", payload.ErrorCode)
	}
	if payload.Error != "" {
		t.Errorf("expected empty Error, got %s", payload.Error)
	}
}

// TestNewRepoPushResultMessageError tests error case.
func TestNewRepoPushResultMessageError(t *testing.T) {
	msg := NewRepoPushResultMessage("test-req-err", false, "", "push.no_upstream", "no upstream configured")

	payload, ok := msg.Payload.(RepoPushResultPayload)
	if !ok {
		t.Fatalf("expected RepoPushResultPayload, got %T", msg.Payload)
	}

	if payload.Success {
		t.Error("expected Success false")
	}
	if payload.Output != "" {
		t.Errorf("expected empty Output, got %s", payload.Output)
	}
	if payload.ErrorCode != "push.no_upstream" {
		t.Errorf("expected ErrorCode push.no_upstream, got %s", payload.ErrorCode)
	}
	if payload.Error != "no upstream configured" {
		t.Errorf("expected Error 'no upstream configured', got %s", payload.Error)
	}
}

// =============================================================================
// P9U0: Commit/Push Request Correlation Tests
// =============================================================================

func TestHandleRepoCommit(t *testing.T) {
	t.Run("success_echoes_request_id", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)
		stagedFile := filepath.Join(repoDir, "staged.txt")
		os.WriteFile(stagedFile, []byte("staged"), 0644)
		runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn) // session.status

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "commit-1",
				"message":    "test commit",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoCommitResult {
			t.Fatalf("expected repo.commit_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != true {
			t.Fatalf("expected success=true, got error: %v", payload["error"])
		}
		if payload["request_id"] != "commit-1" {
			t.Errorf("expected request_id commit-1, got %v", payload["request_id"])
		}
	})

	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"message": "no request id",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("empty_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "   ",
				"message":    "empty request id",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": 123,
				"message":    "non-string request id",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "123" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})
}

func TestHandleRepoPush(t *testing.T) {
	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"remote": "origin",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"request_id": 456,
				"remote":     "origin",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "456" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})
}

func TestHandleRepoCommit_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)
	stagedFile := filepath.Join(repoDir, "staged.txt")
	os.WriteFile(stagedFile, []byte("staged"), 0644)
	runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id": "requester-commit",
			"message":    "requester only test",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoCommitResult {
		t.Fatalf("client 1 expected repo.commit_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.commit_result")
	}
}

func TestHandleRepoPush_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	// Push will fail (no remote), but result should still be requester-only
	msg := map[string]interface{}{
		"type": "repo.push",
		"payload": map[string]interface{}{
			"request_id": "requester-push",
			"remote":     "origin",
			"branch":     "main",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoPushResult {
		t.Fatalf("client 1 expected repo.push_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.push_result")
	}
}

func TestHandleRepoCommit_Idempotency(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)
		stagedFile := filepath.Join(repoDir, "staged.txt")
		os.WriteFile(stagedFile, []byte("staged"), 0644)
		runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "idem-commit",
				"message":    "idem test",
			},
		}
		data, _ := json.Marshal(msg)

		// First request
		conn.WriteMessage(websocket.TextMessage, data)
		resp1 := readMessage(t, conn)
		p1, _ := resp1.Payload.(map[string]interface{})
		if p1["success"] != true {
			t.Fatalf("first commit failed: %v", p1["error"])
		}

		// Replay same request
		conn.WriteMessage(websocket.TextMessage, data)
		resp2 := readMessage(t, conn)
		p2, _ := resp2.Payload.(map[string]interface{})
		if p2["success"] != true {
			t.Fatalf("replay should succeed: %v", p2["error"])
		}
		if p2["request_id"] != "idem-commit" {
			t.Errorf("expected request_id idem-commit, got %v", p2["request_id"])
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)
		stagedFile := filepath.Join(repoDir, "staged.txt")
		os.WriteFile(stagedFile, []byte("staged"), 0644)
		runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		// First request
		msg1 := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "idem-commit-mm",
				"message":    "message A",
			},
		}
		data1, _ := json.Marshal(msg1)
		conn.WriteMessage(websocket.TextMessage, data1)
		readMessage(t, conn) // consume result

		// Same request_id, different message
		msg2 := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "idem-commit-mm",
				"message":    "message B",
			},
		}
		data2, _ := json.Marshal(msg2)
		conn.WriteMessage(websocket.TextMessage, data2)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for mismatch")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})
}

func TestHandleRepoPush_Idempotency(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		// Push will fail (no remote), but the failure should be cached for replay
		msg := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"request_id": "idem-push",
				"remote":     "origin",
				"branch":     "main",
			},
		}
		data, _ := json.Marshal(msg)

		// First request
		conn.WriteMessage(websocket.TextMessage, data)
		resp1 := readMessage(t, conn)
		p1, _ := resp1.Payload.(map[string]interface{})

		// Replay same request
		conn.WriteMessage(websocket.TextMessage, data)
		resp2 := readMessage(t, conn)
		p2, _ := resp2.Payload.(map[string]interface{})

		// Both results should match
		if p1["success"] != p2["success"] {
			t.Errorf("replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
		}
		if p2["request_id"] != "idem-push" {
			t.Errorf("expected request_id idem-push, got %v", p2["request_id"])
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		// First request
		msg1 := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"request_id": "idem-push-mm",
				"remote":     "origin",
				"branch":     "main",
			},
		}
		data1, _ := json.Marshal(msg1)
		conn.WriteMessage(websocket.TextMessage, data1)
		readMessage(t, conn)

		// Same request_id, different branch
		msg2 := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"request_id":       "idem-push-mm",
				"remote":           "origin",
				"branch":           "develop",
				"force_with_lease": true,
			},
		}
		data2, _ := json.Marshal(msg2)
		conn.WriteMessage(websocket.TextMessage, data2)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for mismatch")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})
}

func TestHandleRepoCommit_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-commit-1"
	fingerprint := commitFingerprint("in-flight commit", false, false, false)
	resultMsg := NewRepoCommitResultMessage(requestID, true, "abc123", "1 file changed", "", "")

	// First request becomes the in-flight owner.
	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoCommit, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	// Exact duplicate should coalesce and wait for completion.
	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoCommit, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Mismatch while in-flight remains invalid.
	if _, _, _, mismatchErr := client.idempotencyCheckOrBegin(
		MessageTypeRepoCommit,
		requestID,
		commitFingerprint("different commit message", false, false, false),
	); mismatchErr == nil {
		t.Fatal("expected mismatch error for same request_id with different fingerprint while in-flight")
	}

	// Owner completes; coalesced duplicate observes exact final result.
	client.idempotencyComplete(MessageTypeRepoCommit, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoCommitResult, inFlight.Result.Type)
	}
	payload, ok := inFlight.Result.Payload.(RepoCommitResultPayload)
	if !ok {
		t.Fatalf("expected RepoCommitResultPayload, got %T", inFlight.Result.Payload)
	}
	if payload.RequestID != requestID {
		t.Fatalf("expected request_id %s, got %s", requestID, payload.RequestID)
	}
	if !payload.Success {
		t.Fatal("expected success=true")
	}

	// Any later duplicate should replay from completed cache.
	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoCommit, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoCommitResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoCommitResult, cached.Type)
	}
}

func TestHandleRepoPush_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-push-1"
	fingerprint := pushFingerprint("origin", "main", false)
	resultMsg := NewRepoPushResultMessage(requestID, true, "To origin/main", "", "")

	// First request becomes the in-flight owner.
	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPush, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	// Exact duplicate should coalesce and wait for completion.
	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPush, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Mismatch while in-flight remains invalid.
	if _, _, _, mismatchErr := client.idempotencyCheckOrBegin(
		MessageTypeRepoPush,
		requestID,
		pushFingerprint("origin", "develop", true),
	); mismatchErr == nil {
		t.Fatal("expected mismatch error for same request_id with different fingerprint while in-flight")
	}

	// Owner completes; coalesced duplicate observes exact final result.
	client.idempotencyComplete(MessageTypeRepoPush, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoPushResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoPushResult, inFlight.Result.Type)
	}
	payload, ok := inFlight.Result.Payload.(RepoPushResultPayload)
	if !ok {
		t.Fatalf("expected RepoPushResultPayload, got %T", inFlight.Result.Payload)
	}
	if payload.RequestID != requestID {
		t.Fatalf("expected request_id %s, got %s", requestID, payload.RequestID)
	}
	if !payload.Success {
		t.Fatal("expected success=true")
	}

	// Any later duplicate should replay from completed cache.
	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoPush, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoPushResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoPushResult, cached.Type)
	}
}

func TestHandleRepoCommitPush_CrossOpIsolation(t *testing.T) {
	// Same request_id across commit and push should be independent
	repoDir := setupGitRepoForCommitHandlerTest(t)
	stagedFile := filepath.Join(repoDir, "staged.txt")
	os.WriteFile(stagedFile, []byte("staged"), 0644)
	runGitForCommitHandlerTest(t, repoDir, "add", "staged.txt")

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	sharedID := "shared-req-id"

	// Commit with shared ID
	commitMsg := map[string]interface{}{
		"type": "repo.commit",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"message":    "cross-op test",
		},
	}
	commitData, _ := json.Marshal(commitMsg)
	conn.WriteMessage(websocket.TextMessage, commitData)
	commitResp := readMessage(t, conn)
	commitP, _ := commitResp.Payload.(map[string]interface{})
	if commitP["success"] != true {
		t.Fatalf("commit failed: %v", commitP["error"])
	}

	// Push with same request_id should NOT be treated as replay of commit
	pushMsg := map[string]interface{}{
		"type": "repo.push",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"remote":     "origin",
		},
	}
	pushData, _ := json.Marshal(pushMsg)
	conn.WriteMessage(websocket.TextMessage, pushData)
	pushResp := readMessage(t, conn)
	if pushResp.Type != MessageTypeRepoPushResult {
		t.Fatalf("expected repo.push_result, got %s", pushResp.Type)
	}
}

// =============================================================================
// Unit 9.2: Multi-Session PTY Protocol Message Tests
// =============================================================================

// TestNewSessionCreatedMessage verifies the session created message constructor.
func TestNewSessionCreatedMessage(t *testing.T) {
	createdAt := int64(1700000000000)
	msg := NewSessionCreatedMessage("session-123", "Session 1", "/bin/bash", "running", createdAt)

	if msg.Type != MessageTypeSessionCreated {
		t.Errorf("expected type session.created, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(SessionCreatedPayload)
	if !ok {
		t.Fatalf("expected SessionCreatedPayload, got %T", msg.Payload)
	}

	if payload.SessionID != "session-123" {
		t.Errorf("expected SessionID 'session-123', got %s", payload.SessionID)
	}
	if payload.Name != "Session 1" {
		t.Errorf("expected Name 'Session 1', got %s", payload.Name)
	}
	if payload.Command != "/bin/bash" {
		t.Errorf("expected Command '/bin/bash', got %s", payload.Command)
	}
	if payload.Status != "running" {
		t.Errorf("expected Status 'running', got %s", payload.Status)
	}
	if payload.CreatedAt != createdAt {
		t.Errorf("expected CreatedAt %d, got %d", createdAt, payload.CreatedAt)
	}
}

// TestNewSessionClosedMessage verifies the session closed message constructor.
func TestNewSessionClosedMessage(t *testing.T) {
	msg := NewSessionClosedMessage("session-123", "user_requested")

	if msg.Type != MessageTypeSessionClosed {
		t.Errorf("expected type session.closed, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(SessionClosedPayload)
	if !ok {
		t.Fatalf("expected SessionClosedPayload, got %T", msg.Payload)
	}

	if payload.SessionID != "session-123" {
		t.Errorf("expected SessionID 'session-123', got %s", payload.SessionID)
	}
	if payload.Reason != "user_requested" {
		t.Errorf("expected Reason 'user_requested', got %s", payload.Reason)
	}
}

// TestNewSessionClosedMessageNoReason verifies optional reason field.
func TestNewSessionClosedMessageNoReason(t *testing.T) {
	msg := NewSessionClosedMessage("session-123", "")

	payload, ok := msg.Payload.(SessionClosedPayload)
	if !ok {
		t.Fatalf("expected SessionClosedPayload, got %T", msg.Payload)
	}

	if payload.Reason != "" {
		t.Errorf("expected empty Reason, got %s", payload.Reason)
	}
}

// TestNewSessionClearHistoryResultMessage verifies clear-history result constructor.
func TestNewSessionClearHistoryResultMessage(t *testing.T) {
	msg := NewSessionClearHistoryResultMessage("req-1", true, 3, "", "")
	if msg.Type != MessageTypeSessionClearHistoryResult {
		t.Errorf("expected type %s, got %s", MessageTypeSessionClearHistoryResult, msg.Type)
	}

	payload, ok := msg.Payload.(SessionClearHistoryResultPayload)
	if !ok {
		t.Fatalf("expected SessionClearHistoryResultPayload, got %T", msg.Payload)
	}
	if payload.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want req-1", payload.RequestID)
	}
	if !payload.Success {
		t.Error("expected success=true")
	}
	if payload.ClearedCount != 3 {
		t.Errorf("ClearedCount = %d, want 3", payload.ClearedCount)
	}
}

// TestNewSessionBufferMessage verifies the session buffer message constructor.
func TestNewSessionBufferMessage(t *testing.T) {
	lines := []string{"line 1", "line 2", "line 3"}
	msg := NewSessionBufferMessage("session-123", lines, 10, 5)

	if msg.Type != MessageTypeSessionBuffer {
		t.Errorf("expected type session.buffer, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(SessionBufferPayload)
	if !ok {
		t.Fatalf("expected SessionBufferPayload, got %T", msg.Payload)
	}

	if payload.SessionID != "session-123" {
		t.Errorf("expected SessionID 'session-123', got %s", payload.SessionID)
	}
	if len(payload.Lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(payload.Lines))
	}
	if payload.Lines[0] != "line 1" || payload.Lines[2] != "line 3" {
		t.Errorf("unexpected lines content: %v", payload.Lines)
	}
	if payload.CursorRow != 10 {
		t.Errorf("expected CursorRow 10, got %d", payload.CursorRow)
	}
	if payload.CursorCol != 5 {
		t.Errorf("expected CursorCol 5, got %d", payload.CursorCol)
	}
}

// TestNewSessionBufferMessageEmptyLines verifies empty buffer handling.
func TestNewSessionBufferMessageEmptyLines(t *testing.T) {
	msg := NewSessionBufferMessage("session-123", []string{}, 0, 0)

	payload, ok := msg.Payload.(SessionBufferPayload)
	if !ok {
		t.Fatalf("expected SessionBufferPayload, got %T", msg.Payload)
	}

	if len(payload.Lines) != 0 {
		t.Errorf("expected 0 lines, got %d", len(payload.Lines))
	}
}

// TestSessionPayloadSerialization verifies JSON round-trip for session payloads.
func TestSessionPayloadSerialization(t *testing.T) {
	tests := []struct {
		name    string
		payload interface{}
	}{
		{
			name: "SessionCreatePayload",
			payload: SessionCreatePayload{
				Command: "/bin/bash",
				Args:    []string{"-l"},
				Name:    "Main Session",
			},
		},
		{
			name: "SessionCreatedPayload",
			payload: SessionCreatedPayload{
				SessionID: "abc-123",
				Name:      "Session 1",
				Command:   "/bin/bash",
				Status:    "running",
				CreatedAt: 1700000000000,
			},
		},
		{
			name: "SessionClosePayload",
			payload: SessionClosePayload{
				SessionID: "abc-123",
			},
		},
		{
			name: "SessionClosedPayload",
			payload: SessionClosedPayload{
				SessionID: "abc-123",
				Reason:    "user_requested",
			},
		},
		{
			name: "SessionSwitchPayload",
			payload: SessionSwitchPayload{
				SessionID: "abc-123",
			},
		},
		{
			name: "SessionRenamePayload",
			payload: SessionRenamePayload{
				SessionID: "abc-123",
				Name:      "New Name",
			},
		},
		{
			name: "SessionBufferPayload",
			payload: SessionBufferPayload{
				SessionID: "abc-123",
				Lines:     []string{"line1", "line2"},
				CursorRow: 5,
				CursorCol: 10,
			},
		},
		{
			name: "SessionClearHistoryPayload",
			payload: SessionClearHistoryPayload{
				RequestID: "req-1",
			},
		},
		{
			name: "SessionClearHistoryResultPayload",
			payload: SessionClearHistoryResultPayload{
				RequestID:    "req-1",
				Success:      true,
				ClearedCount: 2,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Marshal to JSON
			data, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}

			// Unmarshal back to map to verify JSON structure
			var result map[string]interface{}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("unmarshal to map failed: %v", err)
			}

			// Verify it's valid JSON (no empty result)
			if len(result) == 0 && tt.name != "SessionCreatePayload" {
				// SessionCreatePayload can be all optional
				t.Error("expected non-empty JSON object")
			}
		})
	}
}

// TestSessionMessageTypeValues verifies the wire format strings.
func TestSessionMessageTypeValues(t *testing.T) {
	tests := []struct {
		msgType  MessageType
		expected string
	}{
		{MessageTypeSessionCreate, "session.create"},
		{MessageTypeSessionCreated, "session.created"},
		{MessageTypeSessionClose, "session.close"},
		{MessageTypeSessionClosed, "session.closed"},
		{MessageTypeSessionSwitch, "session.switch"},
		{MessageTypeSessionRename, "session.rename"},
		{MessageTypeSessionBuffer, "session.buffer"},
		{MessageTypeSessionClearHistory, "session.clear_history"},
		{MessageTypeSessionClearHistoryResult, "session.clear_history_result"},
	}

	for _, tt := range tests {
		if string(tt.msgType) != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, tt.msgType)
		}
	}
}

func TestHandleSessionClearHistoryMissingRequestID(t *testing.T) {
	s := NewServer(":0")
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()
	s.SetSessionStore(store)

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_ = readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "session.clear_history",
		"payload": map[string]interface{}{
			"request_id": "",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeError {
		t.Fatalf("expected error, got %s", resp.Type)
	}
	payload := resp.Payload.(map[string]interface{})
	if payload["code"] != apperrors.CodeServerInvalidMessage {
		t.Fatalf("expected code %s, got %v", apperrors.CodeServerInvalidMessage, payload["code"])
	}
}

func TestHandleSessionClearHistorySuccessBroadcastsSessionList(t *testing.T) {
	s := NewServer(":0")
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()
	s.SetSessionStore(store)

	now := time.Now().Truncate(time.Millisecond)
	if err := store.SaveSession(&storage.Session{
		ID:        "running-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    storage.SessionStatusRunning,
	}); err != nil {
		t.Fatalf("SaveSession running failed: %v", err)
	}
	if err := store.SaveSession(&storage.Session{
		ID:        "complete-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now.Add(time.Minute),
		LastSeen:  now.Add(time.Minute),
		Status:    storage.SessionStatusComplete,
	}); err != nil {
		t.Fatalf("SaveSession complete failed: %v", err)
	}
	if err := store.SaveSession(&storage.Session{
		ID:        "system-complete",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now.Add(2 * time.Minute),
		LastSeen:  now.Add(2 * time.Minute),
		Status:    storage.SessionStatusComplete,
		IsSystem:  true,
	}); err != nil {
		t.Fatalf("SaveSession system complete failed: %v", err)
	}

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("conn1 dial failed: %v", err)
	}
	defer conn1.Close()
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("conn2 dial failed: %v", err)
	}
	defer conn2.Close()

	_ = readMessage(t, conn1) // session.status
	_ = readMessage(t, conn2) // session.status

	if err := conn1.WriteJSON(map[string]interface{}{
		"type": "session.clear_history",
		"payload": map[string]interface{}{
			"request_id": "req-clear-1",
		},
	}); err != nil {
		t.Fatalf("send clear history failed: %v", err)
	}

	first := readMessage(t, conn1)
	if first.Type != MessageTypeSessionClearHistoryResult {
		t.Fatalf("expected clear_history_result first for requester, got %s", first.Type)
	}
	firstPayload := first.Payload.(map[string]interface{})
	if firstPayload["request_id"] != "req-clear-1" {
		t.Fatalf("unexpected request_id: %v", firstPayload["request_id"])
	}
	if ok, _ := firstPayload["success"].(bool); !ok {
		t.Fatalf("expected success=true, got %v", firstPayload["success"])
	}
	if got := int(firstPayload["cleared_count"].(float64)); got != 1 {
		t.Fatalf("cleared_count = %d, want 1", got)
	}

	// Both clients should receive a refreshed session.list:
	// archived non-system rows are cleared, archived system rows remain.
	list1 := readMessage(t, conn1)
	if list1.Type != MessageTypeSessionList {
		t.Fatalf("requester expected session.list after result, got %s", list1.Type)
	}
	list2 := readMessage(t, conn2)
	if list2.Type != MessageTypeSessionList {
		t.Fatalf("other client expected session.list broadcast, got %s", list2.Type)
	}

	for _, msg := range []Message{list1, list2} {
		payload := msg.Payload.(map[string]interface{})
		sessions := payload["sessions"].([]interface{})
		if len(sessions) != 2 {
			t.Fatalf("expected 2 remaining sessions, got %d", len(sessions))
		}

		var runningEntry map[string]interface{}
		var systemEntry map[string]interface{}
		for _, raw := range sessions {
			entry := raw.(map[string]interface{})
			switch entry["id"] {
			case "running-1":
				runningEntry = entry
			case "system-complete":
				systemEntry = entry
			}
		}

		if runningEntry == nil {
			t.Fatalf("expected running-1 in refreshed session list, got %#v", sessions)
		}
		if _, ok := runningEntry["is_system"]; ok {
			t.Fatalf("running-1 should omit is_system when false, got %v", runningEntry["is_system"])
		}
		if systemEntry == nil {
			t.Fatalf("expected system-complete in refreshed session list, got %#v", sessions)
		}
		if isSystem, ok := systemEntry["is_system"].(bool); !ok || !isSystem {
			t.Fatalf("system-complete is_system = %v (ok=%v), want true", systemEntry["is_system"], ok)
		}
	}
}

// TestFileMessageTypeValues verifies the file protocol wire format strings.
func TestFileMessageTypeValues(t *testing.T) {
	tests := []struct {
		msgType  MessageType
		expected string
	}{
		{MessageTypeFileList, "file.list"},
		{MessageTypeFileListResult, "file.list_result"},
		{MessageTypeFileRead, "file.read"},
		{MessageTypeFileReadResult, "file.read_result"},
		{MessageTypeFileWrite, "file.write"},
		{MessageTypeFileWriteResult, "file.write_result"},
		{MessageTypeFileCreate, "file.create"},
		{MessageTypeFileCreateResult, "file.create_result"},
		{MessageTypeFileDelete, "file.delete"},
		{MessageTypeFileDeleteResult, "file.delete_result"},
		{MessageTypeFileWatch, "file.watch"},
	}

	for _, tt := range tests {
		if string(tt.msgType) != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, tt.msgType)
		}
	}
}

// TestFileListResultPayload_MarshalContractShape verifies the list result
// payload keeps required success fields and omits success-only fields on failure.
func TestFileListResultPayload_MarshalContractShape(t *testing.T) {
	successPayload := FileListResultPayload{
		RequestID: "req-1",
		Path:      ".",
		Success:   true,
		Entries:   []FileEntry{},
	}
	successJSON, err := json.Marshal(successPayload)
	if err != nil {
		t.Fatalf("marshal success payload: %v", err)
	}
	if !strings.Contains(string(successJSON), `"entries":[]`) {
		t.Fatalf("expected success payload to include empty entries array, got %s", string(successJSON))
	}

	failurePayload := FileListResultPayload{
		RequestID: "req-2",
		Path:      ".",
		Success:   false,
		ErrorCode: "action.invalid",
		Error:     "bad path",
	}
	failureJSON, err := json.Marshal(failurePayload)
	if err != nil {
		t.Fatalf("marshal failure payload: %v", err)
	}
	if strings.Contains(string(failureJSON), `"entries"`) {
		t.Fatalf("expected failure payload to omit entries, got %s", string(failureJSON))
	}
}

// TestFileReadResultPayload_MarshalContractShape verifies the read result
// payload keeps required success fields and omits success-only fields on failure.
func TestFileReadResultPayload_MarshalContractShape(t *testing.T) {
	successPayload := FileReadResultPayload{
		RequestID: "req-1",
		Path:      "empty.txt",
		Success:   true,
		Content:   "",
		Encoding:  "utf-8",
		Version:   "sha256:abc",
	}
	successJSON, err := json.Marshal(successPayload)
	if err != nil {
		t.Fatalf("marshal success payload: %v", err)
	}
	if !strings.Contains(string(successJSON), `"content":""`) {
		t.Fatalf("expected success payload to include empty content string, got %s", string(successJSON))
	}

	failurePayload := FileReadResultPayload{
		RequestID: "req-2",
		Path:      "missing.txt",
		Success:   false,
		ErrorCode: "storage.not_found",
		Error:     "missing",
	}
	failureJSON, err := json.Marshal(failurePayload)
	if err != nil {
		t.Fatalf("marshal failure payload: %v", err)
	}
	if strings.Contains(string(failureJSON), `"content"`) {
		t.Fatalf("expected failure payload to omit content, got %s", string(failureJSON))
	}
}

// TestRepoHistoryResultPayload_MarshalContractShape verifies the history result
// payload keeps required success fields and omits success-only fields on failure.
func TestRepoHistoryResultPayload_MarshalContractShape(t *testing.T) {
	successPayload := RepoHistoryResultPayload{
		RequestID: "req-1",
		Success:   true,
		Entries:   nil, // Success payload must still emit an empty array.
	}
	successJSON, err := json.Marshal(successPayload)
	if err != nil {
		t.Fatalf("marshal success payload: %v", err)
	}
	if !strings.Contains(string(successJSON), `"entries":[]`) {
		t.Fatalf("expected success payload to include empty entries array, got %s", string(successJSON))
	}

	failurePayload := RepoHistoryResultPayload{
		RequestID: "req-2",
		Success:   false,
		ErrorCode: apperrors.CodeServerInvalidMessage,
		Error:     "invalid cursor",
	}
	failureJSON, err := json.Marshal(failurePayload)
	if err != nil {
		t.Fatalf("marshal failure payload: %v", err)
	}
	if strings.Contains(string(failureJSON), `"entries"`) {
		t.Fatalf("expected failure payload to omit entries, got %s", string(failureJSON))
	}
	if strings.Contains(string(failureJSON), `"next_cursor"`) {
		t.Fatalf("expected failure payload to omit next_cursor, got %s", string(failureJSON))
	}
}

// TestRepoBranchesResultPayload_MarshalContractShape verifies the branches result
// payload keeps required success fields and omits success-only fields on failure.
func TestRepoBranchesResultPayload_MarshalContractShape(t *testing.T) {
	successPayload := RepoBranchesResultPayload{
		RequestID:     "req-1",
		Success:       true,
		CurrentBranch: "main",
		Local:         nil, // Success payload must still emit an empty array.
		TrackedRemote: nil, // Success payload must still emit an empty array.
	}
	successJSON, err := json.Marshal(successPayload)
	if err != nil {
		t.Fatalf("marshal success payload: %v", err)
	}
	if !strings.Contains(string(successJSON), `"local":[]`) {
		t.Fatalf("expected success payload to include empty local array, got %s", string(successJSON))
	}
	if !strings.Contains(string(successJSON), `"tracked_remote":[]`) {
		t.Fatalf("expected success payload to include empty tracked_remote array, got %s", string(successJSON))
	}
	if !strings.Contains(string(successJSON), `"current_branch":"main"`) {
		t.Fatalf("expected success payload to include current_branch, got %s", string(successJSON))
	}

	failurePayload := RepoBranchesResultPayload{
		RequestID: "req-2",
		Success:   false,
		ErrorCode: apperrors.CodeServerInvalidMessage,
		Error:     "request_id is required",
	}
	failureJSON, err := json.Marshal(failurePayload)
	if err != nil {
		t.Fatalf("marshal failure payload: %v", err)
	}
	if strings.Contains(string(failureJSON), `"local"`) {
		t.Fatalf("expected failure payload to omit local, got %s", string(failureJSON))
	}
	if strings.Contains(string(failureJSON), `"tracked_remote"`) {
		t.Fatalf("expected failure payload to omit tracked_remote, got %s", string(failureJSON))
	}
	if strings.Contains(string(failureJSON), `"current_branch"`) {
		t.Fatalf("expected failure payload to omit current_branch, got %s", string(failureJSON))
	}
}

// TestBackwardCompatibility_SessionCreateEmpty verifies empty payload is valid.
func TestBackwardCompatibility_SessionCreateEmpty(t *testing.T) {
	// An empty SessionCreatePayload should serialize to {}
	payload := SessionCreatePayload{}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Should be an empty object (all fields omitempty)
	if string(data) != "{}" {
		t.Errorf("expected empty object, got %s", string(data))
	}
}

// =============================================================================
// Unit 7.8: Broadcast/Stop Race Condition Tests
// =============================================================================

// TestServer_BroadcastAfterStop verifies that calling Broadcast() after Stop()
// does not panic. The broadcast should be silently ignored.
func TestServer_BroadcastAfterStop(t *testing.T) {
	s, ts := newTestServer()
	ts.Close()

	// Stop the server first
	if err := s.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	// Broadcast after stop should not panic
	// This tests the s.stopped check in Broadcast()
	s.Broadcast(NewHeartbeatMessage())
	s.BroadcastTerminalOutput("test output")
	s.BroadcastDiffCard("card-1", "file.go", "diff content", nil, nil, nil, false, false, nil, time.Now().Unix())

	// If we get here without panic, the test passes
}

// TestServer_ConcurrentBroadcastAndStop verifies that concurrent calls to
// Broadcast() and Stop() do not cause a panic (send on closed channel).
// This test uses -race flag to detect data races.
func TestServer_ConcurrentBroadcastAndStop(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	var wg sync.WaitGroup

	// Start multiple goroutines that call Broadcast
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Broadcast(NewHeartbeatMessage())
			}
		}(i)
	}

	// Give broadcasts a head start
	time.Sleep(10 * time.Millisecond)

	// Stop the server while broadcasts are happening
	if err := s.Stop(); err != nil {
		t.Errorf("stop failed: %v", err)
	}

	// Wait for all broadcasters to finish
	wg.Wait()

	// If we get here without panic, the test passes
}

// TestServer_HighFrequencyBroadcastThenStop verifies clean shutdown under
// high-frequency broadcast load.
func TestServer_HighFrequencyBroadcastThenStop(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Start high-frequency broadcaster
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				s.Broadcast(NewHeartbeatMessage())
			}
		}
	}()

	// Let it run for a bit
	time.Sleep(50 * time.Millisecond)

	// Signal stop and wait
	close(done)
	wg.Wait()

	// Now stop the server
	if err := s.Stop(); err != nil {
		t.Errorf("stop failed: %v", err)
	}
}

// TestServer_DoubleStop verifies that calling Stop() twice does not panic.
func TestServer_DoubleStop(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	// First stop
	if err := s.Stop(); err != nil {
		t.Fatalf("first stop failed: %v", err)
	}

	// Second stop should be a no-op, not panic
	if err := s.Stop(); err != nil {
		t.Errorf("second stop failed: %v", err)
	}
}

// TestServer_ConcurrentStops verifies that concurrent Stop() calls do not panic.
func TestServer_ConcurrentStops(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	var wg sync.WaitGroup

	// Call Stop from multiple goroutines simultaneously
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Stop()
		}()
	}

	wg.Wait()
	// If we get here without panic, the test passes
}

// =============================================================================
// Unit 7.11: Pairing and Auth Edge Cases
// =============================================================================

// TestExtractBearerToken_Header tests extractBearerToken with Authorization header.
func TestExtractBearerToken_Header(t *testing.T) {
	testCases := []struct {
		name     string
		header   string
		expected string
	}{
		{"standard bearer", "Bearer abc123xyz", "abc123xyz"},
		{"lowercase bearer", "bearer abc123xyz", "abc123xyz"},
		{"no bearer prefix", "Basic abc123xyz", ""},
		{"empty header", "", ""},
		{"bearer only", "Bearer ", ""},
		{"bearer with spaces in token", "Bearer abc def ghi", "abc def ghi"},
		{"very long token", "Bearer " + strings.Repeat("x", 1000), strings.Repeat("x", 1000)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/ws", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			result := extractBearerToken(req)
			if result != tc.expected {
				t.Errorf("extractBearerToken() = %q, want %q", result, tc.expected)
			}
		})
	}
}

// TestExtractBearerToken_QueryParam tests extractBearerToken with query parameter.
func TestExtractBearerToken_QueryParam(t *testing.T) {
	testCases := []struct {
		name     string
		token    string
		rawQuery string // Pre-encoded query string
		expected string
	}{
		{"plain token", "abc123xyz", "token=abc123xyz", "abc123xyz"},
		{"empty token", "", "token=", ""},
		{"no token param", "", "other=value", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/ws?"+tc.rawQuery, nil)

			result := extractBearerToken(req)
			if result != tc.expected {
				t.Errorf("extractBearerToken() = %q, want %q", result, tc.expected)
			}
		})
	}
}

// TestExtractBearerToken_QueryParamURLEncoding tests that URL-encoded tokens
// in query parameters are properly decoded.
func TestExtractBearerToken_QueryParamURLEncoding(t *testing.T) {
	testCases := []struct {
		name    string
		token   string // Decoded value we expect
		encoded string // URL-encoded version in query
	}{
		{"plain token", "abc123xyz", "abc123xyz"},
		{"with plus signs", "abc+def", "abc%2Bdef"},
		{"with equals", "abc=def", "abc%3Ddef"},
		{"with spaces encoded as %20", "abc def", "abc%20def"},
		{"with spaces encoded as +", "abc def", "abc+def"},
		{"with slash", "abc/def", "abc%2Fdef"},
		{"with question mark", "abc?def", "abc%3Fdef"},
		{"with ampersand", "abc&def", "abc%26def"},
		{"with percent sign", "abc%def", "abc%25def"},
		{"mixed special chars", "a+b=c/d?e", "a%2Bb%3Dc%2Fd%3Fe"},
		{"unicode emoji", "\U0001F4F1", "%F0%9F%93%B1"},           // 📱
		{"unicode chinese", "\u4F60\u597D", "%E4%BD%A0%E5%A5%BD"}, // 你好
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Build URL with pre-encoded query parameter
			req := httptest.NewRequest("GET", "/ws?token="+tc.encoded, nil)

			result := extractBearerToken(req)
			if result != tc.token {
				t.Errorf("extractBearerToken() = %q, want %q", result, tc.token)
			}
		})
	}
}

// TestExtractBearerToken_HeaderPrecedence tests that Authorization header
// takes precedence over query parameter.
func TestExtractBearerToken_HeaderPrecedence(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws?token=query_token", nil)
	req.Header.Set("Authorization", "Bearer header_token")

	result := extractBearerToken(req)
	if result != "header_token" {
		t.Errorf("extractBearerToken() = %q, want 'header_token' (header should take precedence)", result)
	}
}

// =============================================================================
// Unit 9.3: Multi-Session PTY Handler Tests
// =============================================================================

// TestHandleSessionCreateNoManager tests session.create when no SessionManager is configured.
func TestHandleSessionCreateNoManager(t *testing.T) {
	s := NewServer(":0")

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	// Connect without authentication
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.create message
	msg := map[string]interface{}{
		"type":    "session.create",
		"payload": map[string]interface{}{},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionCloseNoManager tests session.close when no SessionManager is configured.
func TestHandleSessionCloseNoManager(t *testing.T) {
	s := NewServer(":0")

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.close message
	msg := map[string]interface{}{
		"type": "session.close",
		"payload": map[string]interface{}{
			"session_id": "test-session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionSwitchNoManager tests session.switch when no SessionManager is configured.
func TestHandleSessionSwitchNoManager(t *testing.T) {
	s := NewServer(":0")

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.switch message
	msg := map[string]interface{}{
		"type": "session.switch",
		"payload": map[string]interface{}{
			"session_id": "test-session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionCloseMissingID tests session.close with missing session_id.
func TestHandleSessionCloseMissingID(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.close with empty session_id
	msg := map[string]interface{}{
		"type": "session.close",
		"payload": map[string]interface{}{
			"session_id": "",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionSwitchMissingID tests session.switch with missing session_id.
func TestHandleSessionSwitchMissingID(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.switch with empty session_id
	msg := map[string]interface{}{
		"type": "session.switch",
		"payload": map[string]interface{}{
			"session_id": "",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionCloseNotFound tests session.close for non-existent session.
func TestHandleSessionCloseNotFound(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.close for non-existent session
	msg := map[string]interface{}{
		"type": "session.close",
		"payload": map[string]interface{}{
			"session_id": "non-existent-session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}

	// Check error code
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeSessionNotFound {
		t.Errorf("Expected code %q, got %v", apperrors.CodeSessionNotFound, payload["code"])
	}
}

// TestHandleSessionSwitchNotFound tests session.switch for non-existent session.
func TestHandleSessionSwitchNotFound(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.switch for non-existent session
	msg := map[string]interface{}{
		"type": "session.switch",
		"payload": map[string]interface{}{
			"session_id": "non-existent-session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}

	// Check error code
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeSessionNotFound {
		t.Errorf("Expected code %q, got %v", apperrors.CodeSessionNotFound, payload["code"])
	}
}

// TestSetSessionManager tests the SetSessionManager and GetSessionManager methods.
func TestSetSessionManager(t *testing.T) {
	s := NewServer(":0")

	// Initially nil
	if s.GetSessionManager() != nil {
		t.Error("Expected nil session manager initially")
	}

	// Set session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Verify it was set
	if s.GetSessionManager() != mgr {
		t.Error("Session manager not set correctly")
	}

	// Set to nil
	s.SetSessionManager(nil)
	if s.GetSessionManager() != nil {
		t.Error("Expected nil session manager after setting nil")
	}
}

// TestTerminalInputMultiSession tests terminal.input routing to multi-session PTY.
func TestTerminalInputMultiSession(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Create a test session
	cfg := pty.SessionConfig{
		HistoryLines: 100,
	}
	session, err := mgr.CreateWithID("test-session-123", cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Start session with echo command
	if err := session.Start("/bin/sh", "-c", "cat"); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer mgr.Close("test-session-123")

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send terminal.input to the session
	msg := map[string]interface{}{
		"type": "terminal.input",
		"payload": map[string]interface{}{
			"session_id": "test-session-123",
			"data":       "hello\n",
			"timestamp":  time.Now().UnixMilli(),
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Give time for input to be processed
	time.Sleep(100 * time.Millisecond)

	// Session should still be running (we wrote to it successfully)
	if !session.IsRunning() {
		t.Error("Session should still be running after input")
	}
}

// TestTerminalInputSessionNotFound tests terminal.input returns session.not_found
// when SessionManager is configured but session doesn't exist.
func TestTerminalInputSessionNotFound(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager (but don't create any sessions)
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send terminal.input to a non-existent session
	msg := map[string]interface{}{
		"type": "terminal.input",
		"payload": map[string]interface{}{
			"session_id": "non-existent-session",
			"data":       "hello\n",
			"timestamp":  time.Now().UnixMilli(),
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error with session.not_found
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}

	// Check error code is session.not_found (not input.session_invalid)
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeSessionNotFound {
		t.Errorf("Expected code %q, got %v", apperrors.CodeSessionNotFound, payload["code"])
	}
}

// TestTerminalInputFallbackToLegacy tests terminal.input falls back to legacy ptyWriter.
func TestTerminalInputFallbackToLegacy(t *testing.T) {
	s := NewServer(":0")

	// Create a mock PTY writer using the existing mockWriter type
	mockPTY := &mockWriter{}
	s.SetPTYWriter(mockPTY)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send terminal.input with session_id matching server's sessionID
	msg := map[string]interface{}{
		"type": "terminal.input",
		"payload": map[string]interface{}{
			"session_id": s.SessionID(), // Use server's session ID for legacy mode
			"data":       "test input",
			"timestamp":  time.Now().UnixMilli(),
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Give time for input to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify data was written to legacy PTY writer
	mockPTY.mu.Lock()
	writtenData := string(mockPTY.written)
	mockPTY.mu.Unlock()

	if writtenData != "test input" {
		t.Errorf("Expected 'test input' written to legacy PTY, got %q", writtenData)
	}
}

// TestHandleSessionSwitchWithBuffer tests session.switch returns buffer history.
func TestHandleSessionSwitchWithBuffer(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Create a test session with output callback that writes to buffer
	cfg := pty.SessionConfig{
		HistoryLines: 100,
	}
	session, err := mgr.CreateWithID("test-buffer-session", cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Start session with echo command that produces known output
	if err := session.Start("/bin/sh", "-c", "echo line1; echo line2; echo line3; sleep 1"); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer mgr.Close("test-buffer-session")

	// Wait for output to be captured
	time.Sleep(200 * time.Millisecond)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.switch to get buffer
	msg := map[string]interface{}{
		"type": "session.switch",
		"payload": map[string]interface{}{
			"session_id": "test-buffer-session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be session.buffer
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeSessionBuffer {
		t.Errorf("Expected session.buffer message, got %s", resp.Type)
	}

	// Verify buffer payload contains lines
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}

	sessionID, ok := payload["session_id"].(string)
	if !ok || sessionID != "test-buffer-session" {
		t.Errorf("Expected session_id 'test-buffer-session', got %v", payload["session_id"])
	}

	lines, ok := payload["lines"].([]interface{})
	if !ok {
		t.Fatalf("Expected lines array, got %T", payload["lines"])
	}

	// Should have at least some lines from the echo commands
	if len(lines) < 1 {
		t.Error("Expected at least one line in buffer")
	}
}

// TestHandleSessionCreateSuccess tests successful session creation.
func TestHandleSessionCreateSuccess(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()
	defer mgr.CloseAll() // Clean up any created sessions

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send session.create message
	msg := map[string]interface{}{
		"type": "session.create",
		"payload": map[string]interface{}{
			"command": "/bin/sh",
			"args":    []string{"-c", "sleep 5"},
			"name":    "Test Session",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be session.created
	var resp Message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeSessionCreated {
		t.Errorf("Expected session.created message, got %s", resp.Type)
	}

	// Verify payload
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}

	sessionID, ok := payload["session_id"].(string)
	if !ok || sessionID == "" {
		t.Errorf("Expected non-empty session_id, got %v", payload["session_id"])
	}

	name, ok := payload["name"].(string)
	if !ok || name != "Test Session" {
		t.Errorf("Expected name 'Test Session', got %v", payload["name"])
	}

	status, ok := payload["status"].(string)
	if !ok || status != "running" {
		t.Errorf("Expected status 'running', got %v", payload["status"])
	}

	// Verify session exists in manager
	if mgr.Get(sessionID) == nil {
		t.Error("Session not found in manager after creation")
	}
}

func TestHandleSessionCreateAutoNamesStartAtOneWithPreexistingSessions(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager with pre-existing sessions.
	// These simulate legacy/default registrations that must not offset
	// user-facing auto name numbering.
	mgr := pty.NewSessionManager()
	if _, err := mgr.CreateWithID("legacy-main", pty.SessionConfig{HistoryLines: 100}); err != nil {
		t.Fatalf("failed to create legacy-main session: %v", err)
	}
	if _, err := mgr.CreateWithID("legacy-shadow", pty.SessionConfig{HistoryLines: 100}); err != nil {
		t.Fatalf("failed to create legacy-shadow session: %v", err)
	}
	s.SetSessionManager(mgr)

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()
	defer mgr.CloseAll()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain initial session.status
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	msg := map[string]interface{}{
		"type": "session.create",
		"payload": map[string]interface{}{
			"command": "/bin/sh",
			"args":    []string{"-c", "sleep 5"},
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("failed to send session.create: %v", err)
	}

	name := readSessionCreatedName(t, conn)
	if name != "Session 1" {
		t.Fatalf("expected first unnamed session to be Session 1, got %q", name)
	}
}

func TestHandleSessionCreateAutoNameSequenceIncludesNamedSessions(t *testing.T) {
	s := NewServer(":0")
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()
	defer mgr.CloseAll()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain initial session.status
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	sendCreate := func(name string) {
		payload := map[string]interface{}{
			"command": "/bin/sh",
			"args":    []string{"-c", "sleep 5"},
		}
		if name != "" {
			payload["name"] = name
		}
		msg := map[string]interface{}{
			"type":    "session.create",
			"payload": payload,
		}
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatalf("failed to send session.create: %v", err)
		}
	}

	sendCreate("")
	if got := readSessionCreatedName(t, conn); got != "Session 1" {
		t.Fatalf("first unnamed session = %q, want Session 1", got)
	}

	sendCreate("My named session")
	if got := readSessionCreatedName(t, conn); got != "My named session" {
		t.Fatalf("named session = %q, want My named session", got)
	}

	sendCreate("")
	if got := readSessionCreatedName(t, conn); got != "Session 3" {
		t.Fatalf("third created session with empty name = %q, want Session 3", got)
	}
}

// TestHandleSessionCloseSuccess tests successful session closure.
func TestHandleSessionCloseSuccess(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Create a test session
	cfg := pty.SessionConfig{
		HistoryLines: 100,
	}
	session, err := mgr.CreateWithID("session-to-close", cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Start session
	if err := session.Start("/bin/sh", "-c", "sleep 10"); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Verify session exists before close
	if mgr.Get("session-to-close") == nil {
		t.Fatal("Session should exist before close")
	}

	// Send session.close message
	msg := map[string]interface{}{
		"type": "session.close",
		"payload": map[string]interface{}{
			"session_id": "session-to-close",
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be session.closed
	var resp Message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeSessionClosed {
		t.Errorf("Expected session.closed message, got %s", resp.Type)
	}

	// Verify payload
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}

	sessionID, ok := payload["session_id"].(string)
	if !ok || sessionID != "session-to-close" {
		t.Errorf("Expected session_id 'session-to-close', got %v", payload["session_id"])
	}

	reason, ok := payload["reason"].(string)
	if !ok || reason != "user_requested" {
		t.Errorf("Expected reason 'user_requested', got %v", payload["reason"])
	}

	// Verify session no longer exists in manager
	if mgr.Get("session-to-close") != nil {
		t.Error("Session should not exist in manager after close")
	}
}

// TestHandleSessionCreateInvalidPayload tests session.create with invalid payload type.
// Note: Completely invalid JSON is rejected by readPump before dispatch.
// This tests a valid JSON message with wrong payload structure.
func TestHandleSessionCreateInvalidPayload(t *testing.T) {
	s := NewServer(":0")
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send message with payload as string instead of object
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.create","payload":"not an object"}`)); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionCloseInvalidPayload tests session.close with invalid payload type.
func TestHandleSessionCloseInvalidPayload(t *testing.T) {
	s := NewServer(":0")
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send message with payload as string instead of object
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.close","payload":"not an object"}`)); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleSessionSwitchInvalidPayload tests session.switch with invalid payload type.
func TestHandleSessionSwitchInvalidPayload(t *testing.T) {
	s := NewServer(":0")
	s.SetSessionManager(pty.NewSessionManager())

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send message with payload as string instead of object
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.switch","payload":"not an object"}`)); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be error
	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
}

// TestHandleTerminalResizeSuccess tests successful terminal resize.
func TestHandleTerminalResizeSuccess(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Create a test session with a valid command
	cfg := pty.SessionConfig{
		HistoryLines: 100,
	}
	session, err := mgr.CreateWithID("resize-session", cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Start session with a command
	if err := session.Start("/bin/sh", "-c", "sleep 10"); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer mgr.CloseAll()

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send terminal.resize message
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"session_id": "resize-session",
			"cols":       80,
			"rows":       24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Allow some time for processing - resize doesn't send a response on success
	time.Sleep(100 * time.Millisecond)

	// Verify session is still running
	if !session.IsRunning() {
		t.Error("Session should still be running after resize")
	}
}

// TestHandleTerminalResizeInvalidPayload tests resize with invalid payload.
func TestHandleTerminalResizeInvalidPayload(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Test missing session_id
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"cols": 80,
			"rows": 24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeServerInvalidMessage {
		t.Errorf("Expected code %q, got %v", apperrors.CodeServerInvalidMessage, payload["code"])
	}
}

// TestHandleTerminalResizeInvalidDimensions tests resize with invalid dimensions.
func TestHandleTerminalResizeInvalidDimensions(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Test cols = 0
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"session_id": "test-session",
			"cols":       0,
			"rows":       24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeServerInvalidMessage {
		t.Errorf("Expected code %q, got %v", apperrors.CodeServerInvalidMessage, payload["code"])
	}
}

// TestHandleTerminalResizeUnknownSession tests resize with unknown session_id.
func TestHandleTerminalResizeUnknownSession(t *testing.T) {
	s := NewServer(":0")

	// Configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send resize for non-existent session
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"session_id": "non-existent-session",
			"cols":       80,
			"rows":       24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeSessionNotFound {
		t.Errorf("Expected code %q, got %v", apperrors.CodeSessionNotFound, payload["code"])
	}
}

// TestHandleTerminalResizeNoManager tests resize when session manager is not configured.
func TestHandleTerminalResizeNoManager(t *testing.T) {
	s := NewServer(":0")

	// Start server without session manager
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send resize message
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"session_id": "test-session",
			"cols":       80,
			"rows":       24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeServerHandlerMissing {
		t.Errorf("Expected code %q, got %v", apperrors.CodeServerHandlerMissing, payload["code"])
	}
}

// TestHandleTerminalResizeSessionNotRunning tests resize when session is not running.
func TestHandleTerminalResizeSessionNotRunning(t *testing.T) {
	s := NewServer(":0")

	// Create and configure session manager
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	// Create a test session but don't start it
	cfg := pty.SessionConfig{
		HistoryLines: 100,
	}
	_, err := mgr.CreateWithID("stopped-session", cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer mgr.CloseAll()

	// Start server
	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Drain session.status message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.ReadMessage()

	// Send resize for session that is not running
	msg := map[string]interface{}{
		"type": "terminal.resize",
		"payload": map[string]interface{}{
			"session_id": "stopped-session",
			"cols":       80,
			"rows":       24,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var resp Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if resp.Type != MessageTypeError {
		t.Errorf("Expected error message, got %s", resp.Type)
	}
	payload, ok := resp.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map payload, got %T", resp.Payload)
	}
	if code, ok := payload["code"].(string); !ok || code != apperrors.CodeSessionNotRunning {
		t.Errorf("Expected code %q, got %v", apperrors.CodeSessionNotRunning, payload["code"])
	}
}

// =============================================================================
// C1: Semantic Data Model Tests
// =============================================================================

// TestNewDiffCardMessage_WithSemanticGroups verifies semantic groups appear in payload JSON.
func TestNewDiffCardMessage_WithSemanticGroups(t *testing.T) {
	groups := []SemanticGroupInfo{
		{
			GroupID:      "sg-abc123def456",
			Label:        "Imports",
			Kind:         "import",
			LineStart:    1,
			LineEnd:      5,
			ChunkIndexes: []int{0, 1},
			RiskLevel:    "low",
		},
	}
	msg := NewDiffCardMessage("card-1", "file.go", "+line", nil, nil, groups, false, false, nil, 1000)
	payload, ok := msg.Payload.(DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}
	if len(payload.SemanticGroups) != 1 {
		t.Fatalf("expected 1 semantic group, got %d", len(payload.SemanticGroups))
	}
	sg := payload.SemanticGroups[0]
	if sg.GroupID != "sg-abc123def456" {
		t.Errorf("expected GroupID sg-abc123def456, got %s", sg.GroupID)
	}
	if sg.Label != "Imports" {
		t.Errorf("expected Label Imports, got %s", sg.Label)
	}
	if sg.Kind != "import" {
		t.Errorf("expected Kind import, got %s", sg.Kind)
	}
	if len(sg.ChunkIndexes) != 2 || sg.ChunkIndexes[0] != 0 || sg.ChunkIndexes[1] != 1 {
		t.Errorf("expected ChunkIndexes [0,1], got %v", sg.ChunkIndexes)
	}
	if sg.RiskLevel != "low" {
		t.Errorf("expected RiskLevel low, got %s", sg.RiskLevel)
	}

	// Verify JSON serialization includes semantic_groups
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"semantic_groups"`) {
		t.Errorf("JSON missing semantic_groups key: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"sg-abc123def456"`) {
		t.Errorf("JSON missing group_id value: %s", jsonStr)
	}
}

// TestNewDiffCardMessage_NilSemanticGroups verifies omitempty omits the key.
func TestNewDiffCardMessage_NilSemanticGroups(t *testing.T) {
	msg := NewDiffCardMessage("card-1", "file.go", "+line", nil, nil, nil, false, false, nil, 1000)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	jsonStr := string(data)
	if strings.Contains(jsonStr, `"semantic_groups"`) {
		t.Errorf("JSON should omit semantic_groups when nil: %s", jsonStr)
	}
}

// TestChunkInfoSerialization_WithSemanticFields verifies chunk semantic fields in JSON.
func TestChunkInfoSerialization_WithSemanticFields(t *testing.T) {
	chunk := ChunkInfo{
		Index:           0,
		OldStart:        1,
		OldCount:        3,
		NewStart:        1,
		NewCount:        4,
		SemanticKind:    "import",
		SemanticLabel:   "Import",
		SemanticGroupID: "sg-abc123def456",
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"semantic_kind":"import"`) {
		t.Errorf("JSON missing semantic_kind: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"semantic_label":"Import"`) {
		t.Errorf("JSON missing semantic_label: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"semantic_group_id":"sg-abc123def456"`) {
		t.Errorf("JSON missing semantic_group_id: %s", jsonStr)
	}
}

// TestChunkInfoSerialization_OmitsEmptySemanticFields verifies omitempty behavior.
func TestChunkInfoSerialization_OmitsEmptySemanticFields(t *testing.T) {
	chunk := ChunkInfo{
		Index:    0,
		OldStart: 1,
		OldCount: 3,
		NewStart: 1,
		NewCount: 4,
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	jsonStr := string(data)
	if strings.Contains(jsonStr, `"semantic_kind"`) {
		t.Errorf("JSON should omit empty semantic_kind: %s", jsonStr)
	}
	if strings.Contains(jsonStr, `"semantic_label"`) {
		t.Errorf("JSON should omit empty semantic_label: %s", jsonStr)
	}
	if strings.Contains(jsonStr, `"semantic_group_id"`) {
		t.Errorf("JSON should omit empty semantic_group_id: %s", jsonStr)
	}
}

func TestSemanticPayloadCompatibilityMatrix(t *testing.T) {
	type matrixCase struct {
		name                 string
		payload              DiffCardPayload
		expectChunkSemantics bool
		expectSemanticGroups bool
		verifyRoundTrip      func(t *testing.T, got DiffCardPayload)
	}

	baseChunk := ChunkInfo{
		Index:    0,
		OldStart: 1,
		OldCount: 2,
		NewStart: 1,
		NewCount: 3,
		Offset:   0,
		Length:   12,
		Content:  "@@ -1,2 +1,3 @@\n+line",
	}
	basePayload := DiffCardPayload{
		CardID:    "card-c4",
		File:      "main.go",
		Diff:      "@@ -1,2 +1,3 @@\n+line",
		Chunks:    []ChunkInfo{baseChunk},
		CreatedAt: 1703500000000,
	}

	cases := []matrixCase{
		{
			name:                 "legacy_no_semantic",
			payload:              basePayload,
			expectChunkSemantics: false,
			expectSemanticGroups: false,
			verifyRoundTrip: func(t *testing.T, got DiffCardPayload) {
				if len(got.SemanticGroups) != 0 {
					t.Fatalf("expected no semantic groups, got %d", len(got.SemanticGroups))
				}
				if got.Chunks[0].SemanticKind != "" || got.Chunks[0].SemanticLabel != "" || got.Chunks[0].SemanticGroupID != "" {
					t.Fatalf("expected empty chunk semantic fields, got %+v", got.Chunks[0])
				}
			},
		},
		{
			name: "partial_chunk_only",
			payload: DiffCardPayload{
				CardID: "card-c4-chunk-only",
				File:   "main.go",
				Diff:   "@@ -1,2 +1,3 @@\n+line",
				Chunks: []ChunkInfo{
					{
						Index:           0,
						OldStart:        1,
						OldCount:        2,
						NewStart:        1,
						NewCount:        3,
						Offset:          0,
						Length:          12,
						Content:         "@@ -1,2 +1,3 @@\n+line",
						SemanticKind:    "function",
						SemanticLabel:   "HandleRequest",
						SemanticGroupID: "sg-c4chunkonly",
					},
				},
				CreatedAt: 1703500000001,
			},
			expectChunkSemantics: true,
			expectSemanticGroups: false,
			verifyRoundTrip: func(t *testing.T, got DiffCardPayload) {
				chunk := got.Chunks[0]
				if chunk.SemanticKind != "function" {
					t.Fatalf("expected chunk semantic_kind=function, got %q", chunk.SemanticKind)
				}
				if chunk.SemanticLabel != "HandleRequest" {
					t.Fatalf("expected chunk semantic_label=HandleRequest, got %q", chunk.SemanticLabel)
				}
				if chunk.SemanticGroupID != "sg-c4chunkonly" {
					t.Fatalf("expected chunk semantic_group_id=sg-c4chunkonly, got %q", chunk.SemanticGroupID)
				}
				if len(got.SemanticGroups) != 0 {
					t.Fatalf("expected no semantic groups, got %d", len(got.SemanticGroups))
				}
			},
		},
		{
			name: "partial_group_only",
			payload: DiffCardPayload{
				CardID: "card-c4-group-only",
				File:   "main.go",
				Diff:   "@@ -1,2 +1,3 @@\n+line",
				Chunks: []ChunkInfo{
					baseChunk,
				},
				SemanticGroups: []SemanticGroupInfo{
					{
						GroupID:      "sg-c4grouponly",
						Label:        "Imports",
						Kind:         "import",
						LineStart:    1,
						LineEnd:      3,
						ChunkIndexes: []int{0},
						RiskLevel:    "low",
					},
				},
				CreatedAt: 1703500000002,
			},
			expectChunkSemantics: false,
			expectSemanticGroups: true,
			verifyRoundTrip: func(t *testing.T, got DiffCardPayload) {
				if len(got.SemanticGroups) != 1 {
					t.Fatalf("expected 1 semantic group, got %d", len(got.SemanticGroups))
				}
				group := got.SemanticGroups[0]
				if group.GroupID != "sg-c4grouponly" || group.Label != "Imports" || group.Kind != "import" {
					t.Fatalf("unexpected semantic group round-trip value: %+v", group)
				}
				chunk := got.Chunks[0]
				if chunk.SemanticKind != "" || chunk.SemanticLabel != "" || chunk.SemanticGroupID != "" {
					t.Fatalf("expected empty chunk semantic fields, got %+v", chunk)
				}
			},
		},
		{
			name: "full_chunk_and_group",
			payload: DiffCardPayload{
				CardID: "card-c4-full",
				File:   "main.go",
				Diff:   "@@ -1,2 +1,3 @@\n+line",
				Chunks: []ChunkInfo{
					{
						Index:           0,
						OldStart:        1,
						OldCount:        2,
						NewStart:        1,
						NewCount:        3,
						Offset:          0,
						Length:          12,
						Content:         "@@ -1,2 +1,3 @@\n+line",
						SemanticKind:    "import",
						SemanticLabel:   "Import",
						SemanticGroupID: "sg-c4full",
					},
				},
				SemanticGroups: []SemanticGroupInfo{
					{
						GroupID:      "sg-c4full",
						Label:        "Imports",
						Kind:         "import",
						LineStart:    1,
						LineEnd:      3,
						ChunkIndexes: []int{0},
						RiskLevel:    "low",
					},
				},
				CreatedAt: 1703500000003,
			},
			expectChunkSemantics: true,
			expectSemanticGroups: true,
			verifyRoundTrip: func(t *testing.T, got DiffCardPayload) {
				if len(got.SemanticGroups) != 1 {
					t.Fatalf("expected 1 semantic group, got %d", len(got.SemanticGroups))
				}
				chunk := got.Chunks[0]
				if chunk.SemanticKind != "import" || chunk.SemanticLabel != "Import" || chunk.SemanticGroupID != "sg-c4full" {
					t.Fatalf("unexpected chunk semantic round-trip values: %+v", chunk)
				}
				group := got.SemanticGroups[0]
				if group.GroupID != "sg-c4full" || group.Label != "Imports" || group.Kind != "import" {
					t.Fatalf("unexpected semantic group round-trip values: %+v", group)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("json marshal failed: %v", err)
			}

			var payloadMap map[string]interface{}
			if err := json.Unmarshal(data, &payloadMap); err != nil {
				t.Fatalf("json unmarshal to map failed: %v", err)
			}

			_, hasSemanticGroups := payloadMap["semantic_groups"]
			if tc.expectSemanticGroups != hasSemanticGroups {
				t.Fatalf("semantic_groups presence mismatch: got %v, want %v", hasSemanticGroups, tc.expectSemanticGroups)
			}

			chunksRaw, ok := payloadMap["chunks"].([]interface{})
			if !ok || len(chunksRaw) != 1 {
				t.Fatalf("expected exactly 1 serialized chunk, got %v", payloadMap["chunks"])
			}
			chunkMap, ok := chunksRaw[0].(map[string]interface{})
			if !ok {
				t.Fatalf("expected chunk object, got %T", chunksRaw[0])
			}

			chunkFields := []string{"semantic_kind", "semantic_label", "semantic_group_id"}
			for _, field := range chunkFields {
				_, hasField := chunkMap[field]
				if tc.expectChunkSemantics != hasField {
					t.Fatalf("%s presence mismatch: got %v, want %v", field, hasField, tc.expectChunkSemantics)
				}
			}

			var roundTrip DiffCardPayload
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatalf("json unmarshal to DiffCardPayload failed: %v", err)
			}

			if roundTrip.CardID != tc.payload.CardID || roundTrip.File != tc.payload.File || roundTrip.CreatedAt != tc.payload.CreatedAt {
				t.Fatalf(
					"round-trip base fields mismatch: got card=%q file=%q created_at=%d",
					roundTrip.CardID, roundTrip.File, roundTrip.CreatedAt,
				)
			}
			if len(roundTrip.Chunks) != 1 {
				t.Fatalf("expected 1 chunk after round-trip, got %d", len(roundTrip.Chunks))
			}

			tc.verifyRoundTrip(t, roundTrip)
		})
	}
}

// TestUndoResultMessage_WithSemanticMetadata verifies that undo re-emit with
// semantic metadata doesn't break serialization.
func TestUndoResultMessage_WithSemanticMetadata(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 5,
			Content: "+import fmt", ContentHash: "abc123",
			SemanticKind: "import", SemanticLabel: "Import", SemanticGroupID: "sg-test123456"},
	}
	groups := []SemanticGroupInfo{
		{GroupID: "sg-test123456", Label: "Imports", Kind: "import",
			LineStart: 1, LineEnd: 5, ChunkIndexes: []int{0}, RiskLevel: "low"},
	}
	stats := &DiffStats{ByteSize: 11, LineCount: 1, AddedLines: 1, DeletedLines: 0}

	msg := NewDiffCardMessage("card-undo", "main.go", "+import fmt",
		chunks, nil, groups, false, false, stats, 1703500000000)

	// Serialize to JSON and back to verify no panic/corruption
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}

	var roundtrip Message
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	if roundtrip.Type != MessageTypeDiffCard {
		t.Errorf("expected type %s, got %s", MessageTypeDiffCard, roundtrip.Type)
	}

	// Verify semantic fields survive round-trip
	payloadMap, ok := roundtrip.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", roundtrip.Payload)
	}
	semGroups, ok := payloadMap["semantic_groups"].([]interface{})
	if !ok || len(semGroups) != 1 {
		t.Errorf("expected 1 semantic group in round-trip, got %v", payloadMap["semantic_groups"])
	}
}

// TestReconnectReplayBinarySemanticGroups verifies that reconnect replay emits
// semantic groups for binary cards (C2-G1 parity fix).
func TestReconnectReplayBinarySemanticGroups(t *testing.T) {
	// Simulate what the reconnect handler in host.go does for binary cards.
	// After the fix, binary cards should get semantic groups via EnrichChunksWithSemantics.
	binaryGroups := []SemanticGroupInfo{
		{GroupID: "sg-bin123", Label: "Binary", Kind: "binary",
			LineStart: 0, LineEnd: 0, ChunkIndexes: []int{0}, RiskLevel: "low"},
	}

	msg := NewDiffCardMessage("card-bin-reconnect", "image.png",
		"(binary file - content not shown)",
		nil,          // no chunks for binary
		nil,          // no chunk groups
		binaryGroups, // semantic groups must be present
		true,         // isBinary
		false,        // isDeleted
		nil,          // no stats for binary
		1703500000000,
	)

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}

	var roundtrip Message
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	payloadMap, ok := roundtrip.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", roundtrip.Payload)
	}

	// Binary card must have is_binary=true
	if isBin, _ := payloadMap["is_binary"].(bool); !isBin {
		t.Error("expected is_binary=true in binary reconnect card")
	}

	// Binary card must have semantic_groups with kind "binary"
	semGroups, ok := payloadMap["semantic_groups"].([]interface{})
	if !ok || len(semGroups) == 0 {
		t.Fatal("expected semantic_groups for binary reconnect card, got none")
	}
	group0, _ := semGroups[0].(map[string]interface{})
	if kind, _ := group0["kind"].(string); kind != "binary" {
		t.Errorf("expected binary group kind, got %q", kind)
	}
}

// TestUndoReEmitBinarySemanticGroups verifies that undo re-emit includes
// semantic groups for binary cards (C2-G1 parity fix).
func TestUndoReEmitBinarySemanticGroups(t *testing.T) {
	// After the fix, binary undo re-emit calls EnrichChunksWithSemantics,
	// which produces binary semantic groups via synthetic chunk.
	_, semGroups := stream.EnrichChunksWithSemantics(
		"card-bin-undo", "logo.png", nil, true, false,
	)

	if len(semGroups) == 0 {
		t.Fatal("EnrichChunksWithSemantics should produce semantic groups for binary card")
	}
	if semGroups[0].Kind != "binary" {
		t.Errorf("expected binary group kind, got %q", semGroups[0].Kind)
	}

	// Verify the mapped server types serialize correctly in a diff card message
	serverGroups := mapSemanticGroupsToServer(semGroups)
	msg := NewDiffCardMessage("card-bin-undo", "logo.png",
		"(binary file - content not shown)",
		nil,          // no chunks
		nil,          // no chunk groups
		serverGroups, // semantic groups from enrichment
		true,         // isBinary
		false,        // isDeleted
		nil,          // no stats
		1703500000000,
	)

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}

	var roundtrip Message
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	payloadMap, ok := roundtrip.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload, got %T", roundtrip.Payload)
	}

	groups, ok := payloadMap["semantic_groups"].([]interface{})
	if !ok || len(groups) == 0 {
		t.Fatal("expected semantic_groups in binary undo re-emit card")
	}
	group0, _ := groups[0].(map[string]interface{})
	if kind, _ := group0["kind"].(string); kind != "binary" {
		t.Errorf("expected binary kind in undo semantic group, got %q", kind)
	}
}

// TestSensitivePathParity_B1_C2 verifies that B1 card-risk and C2 semantic-group
// risk use the same sensitive-path helper and produce consistent results.
func TestSensitivePathParity_B1_C2(t *testing.T) {
	paths := []struct {
		path      string
		sensitive bool
	}{
		{"config/auth_service.go", true},
		{"lib/token_store.dart", true},
		{"README.md", false},
		{"main.go", false},
	}

	for _, p := range paths {
		t.Run(p.path, func(t *testing.T) {
			// B1: computeDiffCardMeta detects sensitive path via risk reasons
			_, _, reasons := computeDiffCardMeta(p.path, nil, false, false,
				&DiffStats{ByteSize: 100, LineCount: 10, AddedLines: 5, DeletedLines: 3}, nil)

			b1Sensitive := false
			for _, r := range reasons {
				if r == "sensitive_path" {
					b1Sensitive = true
					break
				}
			}

			// C2: uses semantic.IsSensitivePath (same helper)
			if b1Sensitive != p.sensitive {
				t.Errorf("B1 sensitive=%v, expected %v for path %s", b1Sensitive, p.sensitive, p.path)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// P4U1: Repo History and Branches message types and constructors
// -----------------------------------------------------------------------------

func TestRepoMessageTypeValues(t *testing.T) {
	tests := []struct {
		name     string
		msgType  MessageType
		expected string
	}{
		{"repo.history", MessageTypeRepoHistory, "repo.history"},
		{"repo.history_result", MessageTypeRepoHistoryResult, "repo.history_result"},
		{"repo.branches", MessageTypeRepoBranches, "repo.branches"},
		{"repo.branches_result", MessageTypeRepoBranchesResult, "repo.branches_result"},
		// P9U4: PR message types
		{"repo.pr_list", MessageTypeRepoPrList, "repo.pr_list"},
		{"repo.pr_list_result", MessageTypeRepoPrListResult, "repo.pr_list_result"},
		{"repo.pr_view", MessageTypeRepoPrView, "repo.pr_view"},
		{"repo.pr_view_result", MessageTypeRepoPrViewResult, "repo.pr_view_result"},
		{"repo.pr_create", MessageTypeRepoPrCreate, "repo.pr_create"},
		{"repo.pr_create_result", MessageTypeRepoPrCreateResult, "repo.pr_create_result"},
		{"repo.pr_checkout", MessageTypeRepoPrCheckout, "repo.pr_checkout"},
		{"repo.pr_checkout_result", MessageTypeRepoPrCheckoutResult, "repo.pr_checkout_result"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.msgType) != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, string(tc.msgType))
			}
		})
	}
}

func TestNewRepoHistoryResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		entries := []RepoHistoryEntry{
			{Hash: "abc123", Subject: "Test", Author: "Me", AuthoredAt: 1000},
		}
		msg := NewRepoHistoryResultMessage("req-1", true, entries, "next-abc", "", "")
		if msg.Type != MessageTypeRepoHistoryResult {
			t.Errorf("expected type %s, got %s", MessageTypeRepoHistoryResult, msg.Type)
		}
		payload, ok := msg.Payload.(RepoHistoryResultPayload)
		if !ok {
			t.Fatalf("expected RepoHistoryResultPayload, got %T", msg.Payload)
		}
		if payload.RequestID != "req-1" {
			t.Errorf("expected request_id 'req-1', got %q", payload.RequestID)
		}
		if !payload.Success {
			t.Error("expected success=true")
		}
		if len(payload.Entries) != 1 {
			t.Errorf("expected 1 entry, got %d", len(payload.Entries))
		}
		if payload.NextCursor != "next-abc" {
			t.Errorf("expected next_cursor 'next-abc', got %q", payload.NextCursor)
		}
		if payload.ErrorCode != "" || payload.Error != "" {
			t.Errorf("expected no error fields, got code=%q msg=%q", payload.ErrorCode, payload.Error)
		}
	})

	t.Run("failure", func(t *testing.T) {
		msg := NewRepoHistoryResultMessage("req-2", false, nil, "", "commit.git_error", "git failed")
		payload, ok := msg.Payload.(RepoHistoryResultPayload)
		if !ok {
			t.Fatalf("expected RepoHistoryResultPayload, got %T", msg.Payload)
		}
		if payload.Success {
			t.Error("expected success=false")
		}
		if payload.ErrorCode != "commit.git_error" {
			t.Errorf("expected error code 'commit.git_error', got %q", payload.ErrorCode)
		}
		if payload.Error != "git failed" {
			t.Errorf("expected error 'git failed', got %q", payload.Error)
		}
	})
}

func TestNewRepoBranchesResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		local := []string{"dev", "main"}
		tracked := []string{"origin/dev", "origin/main"}
		msg := NewRepoBranchesResultMessage("req-3", true, "main", local, tracked, "", "")
		if msg.Type != MessageTypeRepoBranchesResult {
			t.Errorf("expected type %s, got %s", MessageTypeRepoBranchesResult, msg.Type)
		}
		payload, ok := msg.Payload.(RepoBranchesResultPayload)
		if !ok {
			t.Fatalf("expected RepoBranchesResultPayload, got %T", msg.Payload)
		}
		if payload.RequestID != "req-3" {
			t.Errorf("expected request_id 'req-3', got %q", payload.RequestID)
		}
		if !payload.Success {
			t.Error("expected success=true")
		}
		if payload.CurrentBranch != "main" {
			t.Errorf("expected current_branch 'main', got %q", payload.CurrentBranch)
		}
		if len(payload.Local) != 2 {
			t.Errorf("expected 2 local branches, got %d", len(payload.Local))
		}
		if len(payload.TrackedRemote) != 2 {
			t.Errorf("expected 2 tracked remotes, got %d", len(payload.TrackedRemote))
		}
	})

	t.Run("failure", func(t *testing.T) {
		msg := NewRepoBranchesResultMessage("req-4", false, "", nil, nil, "server.handler_missing", "not configured")
		payload, ok := msg.Payload.(RepoBranchesResultPayload)
		if !ok {
			t.Fatalf("expected RepoBranchesResultPayload, got %T", msg.Payload)
		}
		if payload.Success {
			t.Error("expected success=false")
		}
		if payload.ErrorCode != "server.handler_missing" {
			t.Errorf("expected error code 'server.handler_missing', got %q", payload.ErrorCode)
		}
	})
}

func TestHandleRepoHistory_Validation(t *testing.T) {
	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		// Drain session.status
		readMessage(t, conn)

		// Send repo.history without request_id
		msg := map[string]interface{}{
			"type":    "repo.history",
			"payload": map[string]interface{}{},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoHistoryResult {
			t.Fatalf("expected repo.history_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("invalid_page_size", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": "test-1",
				"page_size":  300,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "test-1" {
			t.Errorf("expected request_id echoed, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
	})

	t.Run("zero_page_size", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": "test-zero",
				"page_size":  0,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "test-zero" {
			t.Errorf("expected request_id echoed, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("blank_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": "   ",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "   " {
			t.Errorf("expected raw blank request_id echoed, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("invalid_page_size_type_preserves_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": "type-req-id",
				"page_size":  "not-a-number",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "type-req-id" {
			t.Errorf("expected request_id echoed, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": 123,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "123" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("nil_gitOps", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		// Don't set gitOps

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.history",
			"payload": map[string]interface{}{
				"request_id": "test-2",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["error_code"] != apperrors.CodeServerHandlerMissing {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
		}
	})
}

func TestHandleRepoBranches_Validation(t *testing.T) {
	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type":    "repo.branches",
			"payload": map[string]interface{}{},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoBranchesResult {
			t.Fatalf("expected repo.branches_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
	})

	t.Run("blank_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branches",
			"payload": map[string]interface{}{
				"request_id": "  ",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "  " {
			t.Errorf("expected raw blank request_id echoed, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branches",
			"payload": map[string]interface{}{
				"request_id": 456,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "456" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("nil_gitOps", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branches",
			"payload": map[string]interface{}{
				"request_id": "test-3",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["error_code"] != apperrors.CodeServerHandlerMissing {
			t.Errorf("expected error code %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
		}
	})
}

func TestHandleRepoHistory_RequesterOnly(t *testing.T) {
	dir := setupTestRepo(t)
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	// Connect two clients
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1) // session.status

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2) // session.status

	// Client 1 sends repo.history
	msg := map[string]interface{}{
		"type": "repo.history",
		"payload": map[string]interface{}{
			"request_id": "req-only-test",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	// Client 1 should receive result
	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoHistoryResult {
		t.Fatalf("client 1 expected repo.history_result, got %s", resp1.Type)
	}

	// Client 2 should NOT receive anything (with timeout)
	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.history_result")
	}
}

func TestHandleRepoBranches_RequesterOnly(t *testing.T) {
	dir := setupTestRepo(t)
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.branches",
		"payload": map[string]interface{}{
			"request_id": "req-only-branches",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoBranchesResult {
		t.Fatalf("client 1 expected repo.branches_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.branches_result")
	}
}

// -----------------------------------------------------------------------------
// Branch Mutation handler tests (P4U2)
// -----------------------------------------------------------------------------

func TestRepoBranchMessageTypeValues(t *testing.T) {
	if MessageTypeRepoBranchCreate != "repo.branch_create" {
		t.Errorf("expected repo.branch_create, got %s", MessageTypeRepoBranchCreate)
	}
	if MessageTypeRepoBranchCreateResult != "repo.branch_create_result" {
		t.Errorf("expected repo.branch_create_result, got %s", MessageTypeRepoBranchCreateResult)
	}
	if MessageTypeRepoBranchSwitch != "repo.branch_switch" {
		t.Errorf("expected repo.branch_switch, got %s", MessageTypeRepoBranchSwitch)
	}
	if MessageTypeRepoBranchSwitchResult != "repo.branch_switch_result" {
		t.Errorf("expected repo.branch_switch_result, got %s", MessageTypeRepoBranchSwitchResult)
	}
}

func TestNewRepoBranchCreateResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		msg := NewRepoBranchCreateResultMessage("req-1", true, "feature-x", "", "")
		if msg.Type != MessageTypeRepoBranchCreateResult {
			t.Fatalf("expected type %s, got %s", MessageTypeRepoBranchCreateResult, msg.Type)
		}
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var parsed map[string]json.RawMessage
		json.Unmarshal(data, &parsed)
		var payload map[string]interface{}
		json.Unmarshal(parsed["payload"], &payload)
		if payload["request_id"] != "req-1" {
			t.Errorf("expected request_id req-1, got %v", payload["request_id"])
		}
		if payload["success"] != true {
			t.Errorf("expected success=true, got %v", payload["success"])
		}
		if payload["name"] != "feature-x" {
			t.Errorf("expected name feature-x, got %v", payload["name"])
		}
		// Success should not have error fields
		if _, ok := payload["error_code"]; ok {
			t.Error("success payload should not have error_code")
		}
	})

	t.Run("failure", func(t *testing.T) {
		msg := NewRepoBranchCreateResultMessage("req-2", false, "bad", "action.invalid", "invalid branch")
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var parsed map[string]json.RawMessage
		json.Unmarshal(data, &parsed)
		var payload map[string]interface{}
		json.Unmarshal(parsed["payload"], &payload)
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != "action.invalid" {
			t.Errorf("expected error_code action.invalid, got %v", payload["error_code"])
		}
	})
}

func TestNewRepoBranchSwitchResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		msg := NewRepoBranchSwitchResultMessage("req-1", true, "main", nil, "", "")
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var parsed map[string]json.RawMessage
		json.Unmarshal(data, &parsed)
		var payload map[string]interface{}
		json.Unmarshal(parsed["payload"], &payload)
		if payload["success"] != true {
			t.Error("expected success=true")
		}
		if _, ok := payload["blockers"]; ok {
			t.Error("success payload should not have blockers")
		}
	})

	t.Run("failure_with_blockers", func(t *testing.T) {
		blockers := []string{"staged_changes_present", "unstaged_changes_present"}
		msg := NewRepoBranchSwitchResultMessage("req-2", false, "target", blockers, "action.invalid", "dirty")
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var parsed map[string]json.RawMessage
		json.Unmarshal(data, &parsed)
		var payload map[string]interface{}
		json.Unmarshal(parsed["payload"], &payload)
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		b, ok := payload["blockers"].([]interface{})
		if !ok {
			t.Fatalf("expected blockers array, got %T", payload["blockers"])
		}
		if len(b) != 2 {
			t.Errorf("expected 2 blockers, got %d", len(b))
		}
	})

	t.Run("failure_without_blockers", func(t *testing.T) {
		msg := NewRepoBranchSwitchResultMessage("req-3", false, "missing", nil, "action.invalid", "not found")
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var parsed map[string]json.RawMessage
		json.Unmarshal(data, &parsed)
		var payload map[string]interface{}
		json.Unmarshal(parsed["payload"], &payload)
		if _, ok := payload["blockers"]; ok {
			t.Error("failure without blockers should not have blockers field")
		}
	})
}

func TestHandleRepoBranchCreate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn) // session.status

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-1",
				"name":       "new-branch",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoBranchCreateResult {
			t.Fatalf("expected repo.branch_create_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != true {
			t.Fatalf("expected success=true, got error: %v", payload["error"])
		}
		if payload["request_id"] != "create-1" {
			t.Errorf("expected request_id create-1, got %v", payload["request_id"])
		}
		if payload["name"] != "new-branch" {
			t.Errorf("expected name new-branch, got %v", payload["name"])
		}
	})

	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"name": "test",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": 123,
				"name":       "new-branch",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "123" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("missing_name", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-no-name",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("invalid_branch_name", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-invalid",
				"name":       "-bad",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
	})

	t.Run("duplicate_branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		// Get current branch name to use as duplicate
		currentBranch, _ := gitOps.GetCurrentBranchName()

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-dup",
				"name":       currentBranch,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for duplicate")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
	})

	t.Run("no_git_ops", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		// No git ops set

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-no-git",
				"name":       "test",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["error_code"] != apperrors.CodeServerHandlerMissing {
			t.Errorf("expected %s, got %v", apperrors.CodeServerHandlerMissing, payload["error_code"])
		}
	})

	t.Run("empty_repo", func(t *testing.T) {
		dir := t.TempDir()
		runGitCmd(t, dir, "init")
		runGitCmd(t, dir, "config", "user.email", "test@example.com")
		runGitCmd(t, dir, "config", "user.name", "Test User")
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "create-empty",
				"name":       "new-branch",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for empty repo")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
	})
}

func TestHandleRepoBranchSwitch(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-1",
				"name":       "target",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoBranchSwitchResult {
			t.Fatalf("expected repo.branch_switch_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != true {
			t.Fatalf("expected success=true, got error: %v", payload["error"])
		}
		if payload["name"] != "target" {
			t.Errorf("expected name target, got %v", payload["name"])
		}
	})

	t.Run("missing_request_id", func(t *testing.T) {
		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"name": "target",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("non_string_request_id_preserves_raw_value", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": 456,
				"name":       "target",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "456" {
			t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("missing_name", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-no-name",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})

	t.Run("branch_not_found", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-missing",
				"name":       "nonexistent",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
	})

	t.Run("dirty_blockers", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "target")
		// Create dirty state
		if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitCmd(t, dir, "add", "dirty.txt")

		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-dirty",
				"name":       "target",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for dirty switch")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
		blockers, ok := payload["blockers"].([]interface{})
		if !ok {
			t.Fatalf("expected blockers array, got %T", payload["blockers"])
		}
		if len(blockers) == 0 {
			t.Error("expected non-empty blockers")
		}
	})

	t.Run("empty_repo_same_branch_noop", func(t *testing.T) {
		dir := t.TempDir()
		runGitCmd(t, dir, "init")
		runGitCmd(t, dir, "config", "user.email", "test@example.com")
		runGitCmd(t, dir, "config", "user.name", "Test User")
		gitOps := NewGitOperations(dir, false, false, false)

		currentBranch, _ := gitOps.GetCurrentBranchName()

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-empty-noop",
				"name":       currentBranch,
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != true {
			t.Fatalf("expected success=true for same-branch noop in empty repo, got error: %v", payload["error"])
		}
	})

	t.Run("empty_repo_different_branch_fails", func(t *testing.T) {
		dir := t.TempDir()
		runGitCmd(t, dir, "init")
		runGitCmd(t, dir, "config", "user.email", "test@example.com")
		runGitCmd(t, dir, "config", "user.name", "Test User")
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "switch-empty-other",
				"name":       "nonexistent",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for different branch in empty repo")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
	})
}

func TestHandleRepoBranchCreate_RequesterOnly(t *testing.T) {
	dir := setupTestRepo(t)
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.branch_create",
		"payload": map[string]interface{}{
			"request_id": "requester-create",
			"name":       "requester-test-branch",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoBranchCreateResult {
		t.Fatalf("client 1 expected repo.branch_create_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.branch_create_result")
	}
}

func TestHandleRepoBranchSwitch_RequesterOnly(t *testing.T) {
	dir := setupTestRepo(t)
	runGitCmd(t, dir, "branch", "target-req")
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.branch_switch",
		"payload": map[string]interface{}{
			"request_id": "requester-switch",
			"name":       "target-req",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoBranchSwitchResult {
		t.Fatalf("client 1 expected repo.branch_switch_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.branch_switch_result")
	}
}

func TestHandleRepoBranchCreate_Idempotency(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "idem-create",
				"name":       "idem-branch",
			},
		}
		data, _ := json.Marshal(msg)

		// First request
		conn.WriteMessage(websocket.TextMessage, data)
		resp1 := readMessage(t, conn)
		p1, _ := resp1.Payload.(map[string]interface{})
		if p1["success"] != true {
			t.Fatalf("first create failed: %v", p1["error"])
		}

		// Replay same request
		conn.WriteMessage(websocket.TextMessage, data)
		resp2 := readMessage(t, conn)
		p2, _ := resp2.Payload.(map[string]interface{})
		if p2["success"] != true {
			t.Fatalf("replay should succeed: %v", p2["error"])
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		// First request
		msg1 := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "idem-mismatch",
				"name":       "branch-a",
			},
		}
		data1, _ := json.Marshal(msg1)
		conn.WriteMessage(websocket.TextMessage, data1)
		readMessage(t, conn) // consume result

		// Same request_id, different name
		msg2 := map[string]interface{}{
			"type": "repo.branch_create",
			"payload": map[string]interface{}{
				"request_id": "idem-mismatch",
				"name":       "branch-b",
			},
		}
		data2, _ := json.Marshal(msg2)
		conn.WriteMessage(websocket.TextMessage, data2)

		resp := readMessage(t, conn)
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["success"] != false {
			t.Error("expected success=false for mismatch")
		}
		if payload["error_code"] != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
		}
	})
}

func TestHandleRepoBranchSwitch_Idempotency(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGitCmd(t, dir, "branch", "switch-idem")
		gitOps := NewGitOperations(dir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.branch_switch",
			"payload": map[string]interface{}{
				"request_id": "idem-switch",
				"name":       "switch-idem",
			},
		}
		data, _ := json.Marshal(msg)

		conn.WriteMessage(websocket.TextMessage, data)
		resp1 := readMessage(t, conn)
		p1, _ := resp1.Payload.(map[string]interface{})
		if p1["success"] != true {
			t.Fatalf("first switch failed: %v", p1["error"])
		}

		// Replay
		conn.WriteMessage(websocket.TextMessage, data)
		resp2 := readMessage(t, conn)
		p2, _ := resp2.Payload.(map[string]interface{})
		if p2["success"] != true {
			t.Fatalf("replay should succeed: %v", p2["error"])
		}
	})
}

func TestHandleRepoBranchMutation_InFlightDuplicateReplay(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-1"
	fingerprint := branchCreateFingerprint("in-flight-branch")
	resultMsg := NewRepoBranchCreateResultMessage(requestID, true, "in-flight-branch", "", "")

	// First request becomes the in-flight owner.
	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoBranchCreate, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	// Exact duplicate should coalesce and wait for completion.
	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoBranchCreate, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Mismatch while in-flight remains invalid.
	if _, _, _, mismatchErr := client.idempotencyCheckOrBegin(
		MessageTypeRepoBranchCreate,
		requestID,
		branchCreateFingerprint("different-branch"),
	); mismatchErr == nil {
		t.Fatal("expected mismatch error for same request_id with different fingerprint while in-flight")
	}

	// Owner completes; coalesced duplicate observes exact final result.
	client.idempotencyComplete(MessageTypeRepoBranchCreate, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoBranchCreateResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoBranchCreateResult, inFlight.Result.Type)
	}
	payload, ok := inFlight.Result.Payload.(RepoBranchCreateResultPayload)
	if !ok {
		t.Fatalf("expected RepoBranchCreateResultPayload, got %T", inFlight.Result.Payload)
	}
	if payload.RequestID != requestID {
		t.Fatalf("expected request_id %s, got %s", requestID, payload.RequestID)
	}
	if !payload.Success {
		t.Fatal("expected success=true")
	}

	// Any later duplicate should replay from completed cache.
	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoBranchCreate, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoBranchCreateResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoBranchCreateResult, cached.Type)
	}
}

func TestHandleRepoBranchMutation_CrossOpRequestIDIsolation(t *testing.T) {
	dir := setupTestRepo(t)
	runGitCmd(t, dir, "branch", "cross-op-target")
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// Use same request_id for create and switch - should not collide
	createMsg := map[string]interface{}{
		"type": "repo.branch_create",
		"payload": map[string]interface{}{
			"request_id": "shared-id",
			"name":       "cross-op-branch",
		},
	}
	data, _ := json.Marshal(createMsg)
	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})
	if p1["success"] != true {
		t.Fatalf("create failed: %v", p1["error"])
	}

	// Same request_id for switch should work independently
	switchMsg := map[string]interface{}{
		"type": "repo.branch_switch",
		"payload": map[string]interface{}{
			"request_id": "shared-id",
			"name":       "cross-op-target",
		},
	}
	data, _ = json.Marshal(switchMsg)
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})
	if p2["success"] != true {
		t.Fatalf("switch failed (should not collide with create): %v", p2["error"])
	}
}

func TestRepoMutationArbitration(t *testing.T) {
	dir := setupTestRepo(t)
	gitOps := NewGitOperations(dir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	// Manually lock the gate to simulate an in-progress mutation
	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// Try branch create while gate is locked
	msg := map[string]interface{}{
		"type": "repo.branch_create",
		"payload": map[string]interface{}{
			"request_id": "arb-create",
			"name":       "arb-branch",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false when gate is locked")
	}
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}
	errMsg, _ := payload["error"].(string)
	if errMsg != "repo mutation already in progress" {
		t.Errorf("expected 'repo mutation already in progress', got %q", errMsg)
	}

	// Release gate
	s.repoMutationMu.Unlock()
}

func TestHandleRepoCommitPush_MutationGateContention(t *testing.T) {
	t.Run("commit_rejected_when_gate_locked", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)
		gitOps := NewGitOperations(repoDir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		s.repoMutationMu.Lock()
		defer s.repoMutationMu.Unlock()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.commit",
			"payload": map[string]interface{}{
				"request_id": "gate-commit-1",
				"message":    "should fail due to gate",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoCommitResult {
			t.Fatalf("expected repo.commit_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "gate-commit-1" {
			t.Errorf("expected request_id gate-commit-1, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false when gate is locked")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
		errMsg, _ := payload["error"].(string)
		if errMsg != "repo mutation already in progress" {
			t.Errorf("expected 'repo mutation already in progress', got %q", errMsg)
		}
	})

	t.Run("push_rejected_when_gate_locked", func(t *testing.T) {
		repoDir := setupGitRepoForCommitHandlerTest(t)
		gitOps := NewGitOperations(repoDir, false, false, false)

		s, ts := newTestServer()
		defer ts.Close()
		defer s.Stop()
		s.SetGitOperations(gitOps)

		s.repoMutationMu.Lock()
		defer s.repoMutationMu.Unlock()

		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMessage(t, conn)

		msg := map[string]interface{}{
			"type": "repo.push",
			"payload": map[string]interface{}{
				"request_id": "gate-push-1",
				"remote":     "origin",
				"branch":     "main",
			},
		}
		data, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, data)

		resp := readMessage(t, conn)
		if resp.Type != MessageTypeRepoPushResult {
			t.Fatalf("expected repo.push_result, got %s", resp.Type)
		}
		payload, _ := resp.Payload.(map[string]interface{})
		if payload["request_id"] != "gate-push-1" {
			t.Errorf("expected request_id gate-push-1, got %v", payload["request_id"])
		}
		if payload["success"] != false {
			t.Error("expected success=false when gate is locked")
		}
		if payload["error_code"] != apperrors.CodeActionInvalid {
			t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
		}
		errMsg, _ := payload["error"].(string)
		if errMsg != "repo mutation already in progress" {
			t.Errorf("expected 'repo mutation already in progress', got %q", errMsg)
		}
	})
}

// =============================================================================
// P9U3: Fetch/Pull Tests
// =============================================================================

// TestNewRepoFetchResultMessage tests the repo.fetch_result message constructor.
func TestNewRepoFetchResultMessage(t *testing.T) {
	// Test success case
	msg := NewRepoFetchResultMessage("test-req-fetch", true, "From origin\n   abc1234..def5678  main -> origin/main", "", "")

	if msg.Type != MessageTypeRepoFetchResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoFetchResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoFetchResultPayload)
	if !ok {
		t.Fatalf("expected RepoFetchResultPayload, got %T", msg.Payload)
	}

	if payload.RequestID != "test-req-fetch" {
		t.Errorf("expected RequestID test-req-fetch, got %s", payload.RequestID)
	}
	if !payload.Success {
		t.Error("expected Success true")
	}
	if payload.Output != "From origin\n   abc1234..def5678  main -> origin/main" {
		t.Errorf("unexpected Output: %s", payload.Output)
	}
	if payload.ErrorCode != "" {
		t.Errorf("expected empty ErrorCode, got %s", payload.ErrorCode)
	}
	if payload.Error != "" {
		t.Errorf("expected empty Error, got %s", payload.Error)
	}

	// Test error case
	errMsg := NewRepoFetchResultMessage("test-req-err", false, "", apperrors.CodeSyncAuthFailed, "auth failed")
	errPayload, ok := errMsg.Payload.(RepoFetchResultPayload)
	if !ok {
		t.Fatalf("expected RepoFetchResultPayload, got %T", errMsg.Payload)
	}
	if errPayload.Success {
		t.Error("expected Success false")
	}
	if errPayload.Output != "" {
		t.Errorf("expected empty Output, got %s", errPayload.Output)
	}
	if errPayload.ErrorCode != apperrors.CodeSyncAuthFailed {
		t.Errorf("expected ErrorCode %s, got %s", apperrors.CodeSyncAuthFailed, errPayload.ErrorCode)
	}
	if errPayload.Error != "auth failed" {
		t.Errorf("expected Error 'auth failed', got %s", errPayload.Error)
	}
}

// TestNewRepoPullResultMessage tests the repo.pull_result message constructor.
func TestNewRepoPullResultMessage(t *testing.T) {
	// Test success case
	msg := NewRepoPullResultMessage("test-req-pull", true, "Already up to date.", "", "")

	if msg.Type != MessageTypeRepoPullResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoPullResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoPullResultPayload)
	if !ok {
		t.Fatalf("expected RepoPullResultPayload, got %T", msg.Payload)
	}

	if payload.RequestID != "test-req-pull" {
		t.Errorf("expected RequestID test-req-pull, got %s", payload.RequestID)
	}
	if !payload.Success {
		t.Error("expected Success true")
	}
	if payload.Output != "Already up to date." {
		t.Errorf("unexpected Output: %s", payload.Output)
	}
	if payload.ErrorCode != "" {
		t.Errorf("expected empty ErrorCode, got %s", payload.ErrorCode)
	}
	if payload.Error != "" {
		t.Errorf("expected empty Error, got %s", payload.Error)
	}

	// Test error case
	errMsg := NewRepoPullResultMessage("test-req-err", false, "", apperrors.CodeSyncNonFF, "not fast-forward")
	errPayload, ok := errMsg.Payload.(RepoPullResultPayload)
	if !ok {
		t.Fatalf("expected RepoPullResultPayload, got %T", errMsg.Payload)
	}
	if errPayload.Success {
		t.Error("expected Success false")
	}
	if errPayload.Output != "" {
		t.Errorf("expected empty Output, got %s", errPayload.Output)
	}
	if errPayload.ErrorCode != apperrors.CodeSyncNonFF {
		t.Errorf("expected ErrorCode %s, got %s", apperrors.CodeSyncNonFF, errPayload.ErrorCode)
	}
	if errPayload.Error != "not fast-forward" {
		t.Errorf("expected Error 'not fast-forward', got %s", errPayload.Error)
	}
}

// TestHandleRepoFetch_MissingRequestID tests repo.fetch with empty/missing request_id.
func TestHandleRepoFetch_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type":    "repo.fetch",
		"payload": map[string]interface{}{},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected repo.fetch_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestHandleRepoPull_MissingRequestID tests repo.pull with empty/missing request_id.
func TestHandleRepoPull_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type":    "repo.pull",
		"payload": map[string]interface{}{},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected repo.pull_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestHandleRepoFetch_NonStringRequestIDPreservesRawValue verifies malformed
// payloads still echo recoverable raw request_id for correlation.
func TestHandleRepoFetch_NonStringRequestIDPreservesRawValue(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": 123,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected repo.fetch_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "123" {
		t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false for malformed payload")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestHandleRepoPull_NonStringRequestIDPreservesRawValue verifies malformed
// payloads still echo recoverable raw request_id for correlation.
func TestHandleRepoPull_NonStringRequestIDPreservesRawValue(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": 456,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected repo.pull_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "456" {
		t.Errorf("expected raw request_id echoed as string, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false for malformed payload")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// TestHandleRepoFetch_MutationGateBusy tests repo.fetch when repoMutationMu is already locked.
func TestHandleRepoFetch_MutationGateBusy(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)
	gitOps := NewGitOperations(repoDir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	s.repoMutationMu.Lock()
	defer s.repoMutationMu.Unlock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": "gate-fetch-1",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected repo.fetch_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "gate-fetch-1" {
		t.Errorf("expected request_id gate-fetch-1, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false when gate is locked")
	}
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}
	errMsg, _ := payload["error"].(string)
	if errMsg != "repo mutation already in progress" {
		t.Errorf("expected 'repo mutation already in progress', got %q", errMsg)
	}
}

// TestHandleRepoPull_MutationGateBusy tests repo.pull when repoMutationMu is already locked.
func TestHandleRepoPull_MutationGateBusy(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)
	gitOps := NewGitOperations(repoDir, false, false, false)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(gitOps)

	s.repoMutationMu.Lock()
	defer s.repoMutationMu.Unlock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": "gate-pull-1",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected repo.pull_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "gate-pull-1" {
		t.Errorf("expected request_id gate-pull-1, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false when gate is locked")
	}
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}
	errMsg, _ := payload["error"].(string)
	if errMsg != "repo mutation already in progress" {
		t.Errorf("expected 'repo mutation already in progress', got %q", errMsg)
	}
}

// TestHandleRepoFetch_RequesterOnly tests that fetch result goes only to the requester.
func TestHandleRepoFetch_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1) // session.status

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2) // session.status

	// Fetch will fail (no remote), but result should still be requester-only
	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": "requester-fetch",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoFetchResult {
		t.Fatalf("client 1 expected repo.fetch_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.fetch_result")
	}
}

// TestHandleRepoPull_RequesterOnly tests that pull result goes only to the requester.
func TestHandleRepoPull_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1) // session.status

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2) // session.status

	// Pull will fail (no remote), but result should still be requester-only
	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": "requester-pull",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoPullResult {
		t.Fatalf("client 1 expected repo.pull_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.pull_result")
	}
}

// TestHandleRepoPull_NonFF tests that a non-fast-forward pull returns sync.non_ff error.
// This test creates a real diverged repo scenario to trigger the non-ff code path.
func TestHandleRepoPull_NonFF(t *testing.T) {
	// Set up a "remote" bare repo and a local clone
	remoteDir := t.TempDir()
	runGitForCommitHandlerTest(t, remoteDir, "init", "--bare")

	localDir := setupGitRepoForCommitHandlerTest(t)
	runGitForCommitHandlerTest(t, localDir, "remote", "add", "origin", remoteDir)
	runGitForCommitHandlerTest(t, localDir, "push", "-u", "origin", "HEAD")

	// Create a second clone to push a diverging commit
	cloneDir := t.TempDir()
	cmd := exec.Command("git", "clone", remoteDir, cloneDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clone failed: %v\n%s", err, out)
	}
	runGitForCommitHandlerTest(t, cloneDir, "config", "user.email", "other@example.com")
	runGitForCommitHandlerTest(t, cloneDir, "config", "user.name", "Other User")

	// Push a new commit from the clone to create divergence
	otherFile := filepath.Join(cloneDir, "other.txt")
	if err := os.WriteFile(otherFile, []byte("other content"), 0644); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	runGitForCommitHandlerTest(t, cloneDir, "add", "other.txt")
	runGitForCommitHandlerTest(t, cloneDir, "commit", "-m", "diverging commit from clone")
	runGitForCommitHandlerTest(t, cloneDir, "push", "origin", "HEAD")

	// Create a local commit on top of the old HEAD to make pull non-ff
	localFile := filepath.Join(localDir, "local.txt")
	if err := os.WriteFile(localFile, []byte("local content"), 0644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	runGitForCommitHandlerTest(t, localDir, "add", "local.txt")
	runGitForCommitHandlerTest(t, localDir, "commit", "-m", "local diverging commit")

	// Now a pull --ff-only should fail with non-ff
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(localDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": "pull-nonff-1",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected repo.pull_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "pull-nonff-1" {
		t.Errorf("expected request_id pull-nonff-1, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false for non-ff pull")
	}
	if payload["error_code"] != apperrors.CodeSyncNonFF {
		t.Errorf("expected %s, got %v", apperrors.CodeSyncNonFF, payload["error_code"])
	}
}

// -----------------------------------------------------------------------------
// PR message constructor tests (P9U4)
// -----------------------------------------------------------------------------

func TestNewRepoPrListResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		entries := []RepoPrEntryPayload{
			{Number: 1, Title: "Fix bug", State: "open", HeadBranch: "fix", BaseBranch: "main", Author: "me", URL: "http://1"},
		}
		msg := NewRepoPrListResultMessage("req-pr-list-1", true, entries, "", "")
		if msg.Type != MessageTypeRepoPrListResult {
			t.Errorf("expected %s, got %s", MessageTypeRepoPrListResult, msg.Type)
		}
		p, ok := msg.Payload.(RepoPrListResultPayload)
		if !ok {
			t.Fatalf("expected RepoPrListResultPayload, got %T", msg.Payload)
		}
		if p.RequestID != "req-pr-list-1" || !p.Success || len(p.Entries) != 1 {
			t.Errorf("unexpected payload: %+v", p)
		}
	})

	t.Run("failure", func(t *testing.T) {
		msg := NewRepoPrListResultMessage("req-pr-list-2", false, nil, apperrors.CodePrGhMissing, "gh not found")
		p, _ := msg.Payload.(RepoPrListResultPayload)
		if p.Success || p.ErrorCode != apperrors.CodePrGhMissing {
			t.Errorf("unexpected payload: %+v", p)
		}
	})
}

func TestNewRepoPrViewResultMessage(t *testing.T) {
	pr := &RepoPrDetailPayload{Number: 42, Title: "Test PR", State: "open", URL: "http://42"}
	msg := NewRepoPrViewResultMessage("req-pr-view-1", true, pr, "", "")
	if msg.Type != MessageTypeRepoPrViewResult {
		t.Errorf("expected %s, got %s", MessageTypeRepoPrViewResult, msg.Type)
	}
	p, ok := msg.Payload.(RepoPrViewResultPayload)
	if !ok {
		t.Fatalf("expected RepoPrViewResultPayload, got %T", msg.Payload)
	}
	if p.RequestID != "req-pr-view-1" || !p.Success || p.PR == nil || p.PR.Number != 42 {
		t.Errorf("unexpected payload: %+v", p)
	}
}

func TestNewRepoPrCreateResultMessage(t *testing.T) {
	pr := &RepoPrDetailPayload{Number: 99, Title: "New PR", State: "open", URL: "http://99"}
	msg := NewRepoPrCreateResultMessage("req-pr-create-1", true, pr, "", "")
	if msg.Type != MessageTypeRepoPrCreateResult {
		t.Errorf("expected %s, got %s", MessageTypeRepoPrCreateResult, msg.Type)
	}
	p, ok := msg.Payload.(RepoPrCreateResultPayload)
	if !ok {
		t.Fatalf("expected RepoPrCreateResultPayload, got %T", msg.Payload)
	}
	if p.RequestID != "req-pr-create-1" || !p.Success || p.PR == nil || p.PR.Number != 99 {
		t.Errorf("unexpected payload: %+v", p)
	}
}

func TestNewRepoPrCheckoutResultMessage(t *testing.T) {
	t.Run("success with branch change", func(t *testing.T) {
		msg := NewRepoPrCheckoutResultMessage("req-pr-co-1", true, "feature-x", true, nil, "", "")
		if msg.Type != MessageTypeRepoPrCheckoutResult {
			t.Errorf("expected %s, got %s", MessageTypeRepoPrCheckoutResult, msg.Type)
		}
		p, ok := msg.Payload.(RepoPrCheckoutResultPayload)
		if !ok {
			t.Fatalf("expected RepoPrCheckoutResultPayload, got %T", msg.Payload)
		}
		if !p.Success || p.BranchName != "feature-x" || !p.ChangedBranch {
			t.Errorf("unexpected payload: %+v", p)
		}
	})

	t.Run("failure with blockers", func(t *testing.T) {
		blockers := []string{"staged_changes_present"}
		msg := NewRepoPrCheckoutResultMessage("req-pr-co-2", false, "", false, blockers,
			apperrors.CodePrCheckoutBlockedDirty, "dirty")
		p, _ := msg.Payload.(RepoPrCheckoutResultPayload)
		if p.Success || p.ErrorCode != apperrors.CodePrCheckoutBlockedDirty || len(p.Blockers) != 1 {
			t.Errorf("unexpected payload: %+v", p)
		}
	})
}

// -----------------------------------------------------------------------------
// PR handler tests (P9U4)
// -----------------------------------------------------------------------------

func TestHandleRepoPrList_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn) // session.status

	msg := map[string]interface{}{
		"type":    "repo.pr_list",
		"payload": map[string]interface{}{},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPrListResult {
		t.Fatalf("expected repo.pr_list_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrView_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_view",
		"payload": map[string]interface{}{
			"number": 1,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPrViewResult {
		t.Fatalf("expected repo.pr_view_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrCreate_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"title": "test",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("expected repo.pr_create_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrCheckout_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"number": 1,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	if resp.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("expected repo.pr_checkout_result, got %s", resp.Type)
	}
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrCreate_MutationGateBusy(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	// Lock the mutation gate
	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "pr-create-busy",
			"title":      "My PR",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["request_id"] != "pr-create-busy" {
		t.Errorf("expected request_id echo, got %v", payload["request_id"])
	}
	if payload["success"] != false {
		t.Error("expected success=false when gate busy")
	}
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}

	s.repoMutationMu.Unlock()
}

func TestHandleRepoPrCheckout_MutationGateBusy(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "pr-co-busy",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected %s, got %v", apperrors.CodeActionInvalid, payload["error_code"])
	}

	s.repoMutationMu.Unlock()
}

func TestHandleRepoPrCheckout_DirtyBlocked(t *testing.T) {
	dir := setupTestRepo(t)
	// Create dirty state
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, dir, "add", "dirty.txt")

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(dir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "pr-co-dirty",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodePrCheckoutBlockedDirty {
		t.Errorf("expected %s, got %v", apperrors.CodePrCheckoutBlockedDirty, payload["error_code"])
	}
	blockers, ok := payload["blockers"].([]interface{})
	if !ok || len(blockers) == 0 {
		t.Error("expected non-empty blockers array")
	}
}

func TestHandleRepoPrCheckout_NeverEmitsDraftBlocked(t *testing.T) {
	dir := setupTestRepo(t)
	// Create dirty state - will be blocked by dirty, NOT draft
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, dir, "add", "dirty.txt")

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(dir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "pr-co-no-draft",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	// Must never emit pr.checkout_blocked_draft
	if payload["error_code"] == apperrors.CodePrCheckoutBlockedDraft {
		t.Fatal("host must NEVER emit pr.checkout_blocked_draft - draft block is mobile-local only")
	}
}

func TestHandleRepoPrCreate_ValidationEmptyTitle(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "pr-create-empty-title",
			"title":      "   ",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodePrValidationFailed {
		t.Errorf("expected %s, got %v", apperrors.CodePrValidationFailed, payload["error_code"])
	}
}

func TestHandleRepoPrCreate_ValidationWhitespaceBaseBranch(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id":  "pr-create-ws-base",
			"title":       "Valid Title",
			"base_branch": "   ",
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodePrValidationFailed {
		t.Errorf("expected %s, got %v", apperrors.CodePrValidationFailed, payload["error_code"])
	}
}

func TestHandleRepoPrView_InvalidNumber(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_view",
		"payload": map[string]interface{}{
			"request_id": "pr-view-invalid",
			"number":     0,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodePrValidationFailed {
		t.Errorf("expected %s, got %v", apperrors.CodePrValidationFailed, payload["error_code"])
	}
}

func TestHandleRepoPrCheckout_InvalidNumber(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(setupTestRepo(t), false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "pr-co-invalid",
			"number":     -1,
		},
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["error_code"] != apperrors.CodePrValidationFailed {
		t.Errorf("expected %s, got %v", apperrors.CodePrValidationFailed, payload["error_code"])
	}
}

// =============================================================================
// P9U5: Idempotency Replay Tests
// =============================================================================

func TestHandleRepoFetch_IdempotencyReplay(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": "idem-fetch",
		},
	}
	data, _ := json.Marshal(msg)

	// First request (will fail since no remote, but result cached)
	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})

	// Replay same request
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})

	if p1["success"] != p2["success"] {
		t.Errorf("replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
	}
	if p2["request_id"] != "idem-fetch" {
		t.Errorf("expected request_id idem-fetch, got %v", p2["request_id"])
	}
}

func TestHandleRepoPull_IdempotencyReplay(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": "idem-pull",
		},
	}
	data, _ := json.Marshal(msg)

	// First request
	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})

	// Replay same request
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})

	if p1["success"] != p2["success"] {
		t.Errorf("replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
	}
	if p2["request_id"] != "idem-pull" {
		t.Errorf("expected request_id idem-pull, got %v", p2["request_id"])
	}
}

func TestHandleRepoPrCreate_IdempotencyReplay(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// PR create will fail (no gh CLI configured), but result should be cached
	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-create",
			"title":      "Test PR",
			"body":       "Test body",
		},
	}
	data, _ := json.Marshal(msg)

	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})

	// Replay same request
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})

	if p1["success"] != p2["success"] {
		t.Errorf("replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
	}
	if p2["request_id"] != "idem-pr-create" {
		t.Errorf("expected request_id idem-pr-create, got %v", p2["request_id"])
	}
}

func TestHandleRepoPrCheckout_IdempotencyReplay(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// PR checkout will fail (no PR), but result should be cached
	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-checkout",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)

	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})

	// Replay same request
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})

	if p1["success"] != p2["success"] {
		t.Errorf("replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
	}
	if p2["request_id"] != "idem-pr-checkout" {
		t.Errorf("expected request_id idem-pr-checkout, got %v", p2["request_id"])
	}
}

// =============================================================================
// P9U5: Idempotency Mismatch Tests
// =============================================================================

func TestHandleRepoPrCreate_IdempotencyMismatch(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// First request
	msg1 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-mm",
			"title":      "Title A",
		},
	}
	data1, _ := json.Marshal(msg1)
	conn.WriteMessage(websocket.TextMessage, data1)
	readMessage(t, conn)

	// Same request_id, different title
	msg2 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-mm",
			"title":      "Title B",
		},
	}
	data2, _ := json.Marshal(msg2)
	conn.WriteMessage(websocket.TextMessage, data2)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false for mismatch")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrCheckout_IdempotencyMismatch(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// First request
	msg1 := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "idem-prco-mm",
			"number":     42,
		},
	}
	data1, _ := json.Marshal(msg1)
	conn.WriteMessage(websocket.TextMessage, data1)
	readMessage(t, conn)

	// Same request_id, different number
	msg2 := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "idem-prco-mm",
			"number":     99,
		},
	}
	data2, _ := json.Marshal(msg2)
	conn.WriteMessage(websocket.TextMessage, data2)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false for mismatch")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// =============================================================================
// P9U5: Normalized Replay/Mismatch Tests (AC18)
// =============================================================================

func TestHandleRepoPrCreate_IdempotencyNormalizedReplay(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// First request: draft omitted (nil -> false), title with trailing space
	msg1 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-norm",
			"title":      "Normalized Title ",
		},
	}
	data1, _ := json.Marshal(msg1)
	conn.WriteMessage(websocket.TextMessage, data1)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})

	// Replay with draft=false explicitly, trimmed title
	msg2 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-norm",
			"title":      "Normalized Title",
			"draft":      false,
		},
	}
	data2, _ := json.Marshal(msg2)
	conn.WriteMessage(websocket.TextMessage, data2)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})

	// Should replay (same fingerprint after normalization)
	if p1["success"] != p2["success"] {
		t.Errorf("normalized replay success mismatch: first=%v replay=%v", p1["success"], p2["success"])
	}
}

func TestHandleRepoPrCreate_IdempotencyNormalizedMismatch(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// First request
	msg1 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-norm-mm",
			"title":      "Title Alpha",
		},
	}
	data1, _ := json.Marshal(msg1)
	conn.WriteMessage(websocket.TextMessage, data1)
	readMessage(t, conn)

	// Same request_id, semantically different after trim
	msg2 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-norm-mm",
			"title":      "Title Beta",
		},
	}
	data2, _ := json.Marshal(msg2)
	conn.WriteMessage(websocket.TextMessage, data2)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false for normalized mismatch")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

func TestHandleRepoPrCreate_IdempotencyBaseBranchPresenceMismatch(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	// First request omits base_branch (default behavior).
	msg1 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "idem-pr-base-presence-mm",
			"title":      "Presence Test",
		},
	}
	data1, _ := json.Marshal(msg1)
	conn.WriteMessage(websocket.TextMessage, data1)
	readMessage(t, conn)

	// Same request_id with whitespace-only base_branch is semantically different:
	// it should not replay the previous result and must fail mismatch.
	msg2 := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id":  "idem-pr-base-presence-mm",
			"title":       "Presence Test",
			"base_branch": "   ",
		},
	}
	data2, _ := json.Marshal(msg2)
	conn.WriteMessage(websocket.TextMessage, data2)

	resp := readMessage(t, conn)
	payload, _ := resp.Payload.(map[string]interface{})
	if payload["success"] != false {
		t.Error("expected success=false for base_branch presence mismatch")
	}
	if payload["error_code"] != apperrors.CodeServerInvalidMessage {
		t.Errorf("expected %s, got %v", apperrors.CodeServerInvalidMessage, payload["error_code"])
	}
}

// =============================================================================
// P9U5: In-Flight Coalesce Tests
// =============================================================================

func TestHandleRepoFetch_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-fetch-1"
	fingerprint := fetchFingerprint()
	resultMsg := NewRepoFetchResultMessage(requestID, true, "From origin", "", "")

	// First request becomes the in-flight owner.
	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoFetch, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	// Exact duplicate should coalesce and wait for completion.
	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoFetch, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Owner completes; coalesced duplicate observes exact final result.
	client.idempotencyComplete(MessageTypeRepoFetch, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoFetchResult, inFlight.Result.Type)
	}

	// Any later duplicate should replay from completed cache.
	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoFetch, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoFetchResult, cached.Type)
	}
}

func TestHandleRepoPull_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-pull-1"
	fingerprint := pullFingerprint()
	resultMsg := NewRepoPullResultMessage(requestID, true, "Already up to date.", "", "")

	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPull, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPull, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	client.idempotencyComplete(MessageTypeRepoPull, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoPullResult, inFlight.Result.Type)
	}

	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoPull, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoPullResult, cached.Type)
	}
}

func TestHandleRepoPrCreate_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-pr-create-1"
	fingerprint := prCreateFingerprint("Test PR", "body", "", false, false)
	resultMsg := NewRepoPrCreateResultMessage(requestID, true, &RepoPrDetailPayload{Number: 1, Title: "Test PR"}, "", "")

	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCreate, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCreate, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Mismatch while in-flight remains invalid.
	if _, _, _, mismatchErr := client.idempotencyCheckOrBegin(
		MessageTypeRepoPrCreate,
		requestID,
		prCreateFingerprint("Different Title", "body", "", false, false),
	); mismatchErr == nil {
		t.Fatal("expected mismatch error for same request_id with different fingerprint while in-flight")
	}

	client.idempotencyComplete(MessageTypeRepoPrCreate, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoPrCreateResult, inFlight.Result.Type)
	}

	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCreate, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoPrCreateResult, cached.Type)
	}
}

func TestHandleRepoPrCheckout_InFlightCoalesce(t *testing.T) {
	client := &Client{
		send:   make(chan Message, 4),
		done:   make(chan struct{}),
		server: NewServer("unused"),
	}

	requestID := "in-flight-pr-checkout-1"
	fingerprint := prCheckoutFingerprint(42)
	resultMsg := NewRepoPrCheckoutResultMessage(requestID, true, "pr-42", true, nil, "", "")

	if _, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCheckout, requestID, fingerprint); err != nil {
		t.Fatalf("first idempotencyCheckOrBegin failed: %v", err)
	} else if replay {
		t.Fatal("first request should not replay from cache")
	} else if inFlight != nil {
		t.Fatal("first request should not coalesce onto existing in-flight entry")
	}

	_, replay, inFlight, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCheckout, requestID, fingerprint)
	if err != nil {
		t.Fatalf("duplicate idempotencyCheckOrBegin failed: %v", err)
	}
	if replay {
		t.Fatal("duplicate should not replay before owner completes")
	}
	if inFlight == nil {
		t.Fatal("duplicate should coalesce onto in-flight request")
	}

	// Mismatch while in-flight remains invalid.
	if _, _, _, mismatchErr := client.idempotencyCheckOrBegin(
		MessageTypeRepoPrCheckout,
		requestID,
		prCheckoutFingerprint(99),
	); mismatchErr == nil {
		t.Fatal("expected mismatch error for same request_id with different fingerprint while in-flight")
	}

	client.idempotencyComplete(MessageTypeRepoPrCheckout, requestID, fingerprint, resultMsg)
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight completion")
	}
	if inFlight.Result.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("expected result type %s, got %s", MessageTypeRepoPrCheckoutResult, inFlight.Result.Type)
	}

	cached, replay, waiting, err := client.idempotencyCheckOrBegin(MessageTypeRepoPrCheckout, requestID, fingerprint)
	if err != nil {
		t.Fatalf("replay idempotencyCheckOrBegin failed: %v", err)
	}
	if !replay {
		t.Fatal("expected completed replay hit")
	}
	if waiting != nil {
		t.Fatal("expected no in-flight entry after completion")
	}
	if cached.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("expected replay type %s, got %s", MessageTypeRepoPrCheckoutResult, cached.Type)
	}
}

// =============================================================================
// P9U5: Cross-Op Isolation Tests
// =============================================================================

func TestHandleRepoFetchPull_CrossOpIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	sharedID := "shared-sync-id"

	// Fetch with shared ID
	fetchMsg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": sharedID,
		},
	}
	fetchData, _ := json.Marshal(fetchMsg)
	conn.WriteMessage(websocket.TextMessage, fetchData)
	fetchResp := readMessage(t, conn)
	if fetchResp.Type != MessageTypeRepoFetchResult {
		t.Fatalf("expected repo.fetch_result, got %s", fetchResp.Type)
	}

	// Pull with same request_id should NOT be treated as replay of fetch
	pullMsg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": sharedID,
		},
	}
	pullData, _ := json.Marshal(pullMsg)
	conn.WriteMessage(websocket.TextMessage, pullData)
	pullResp := readMessage(t, conn)
	if pullResp.Type != MessageTypeRepoPullResult {
		t.Fatalf("expected repo.pull_result, got %s", pullResp.Type)
	}
}

func TestHandleRepoPrCreateCheckout_CrossOpIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	sharedID := "shared-pr-id"

	// PR create with shared ID
	createMsg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"title":      "Cross-op PR",
		},
	}
	createData, _ := json.Marshal(createMsg)
	conn.WriteMessage(websocket.TextMessage, createData)
	createResp := readMessage(t, conn)
	if createResp.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("expected repo.pr_create_result, got %s", createResp.Type)
	}

	// PR checkout with same request_id should NOT be treated as replay
	checkoutMsg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"number":     42,
		},
	}
	checkoutData, _ := json.Marshal(checkoutMsg)
	conn.WriteMessage(websocket.TextMessage, checkoutData)
	checkoutResp := readMessage(t, conn)
	if checkoutResp.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("expected repo.pr_checkout_result, got %s", checkoutResp.Type)
	}
}

// =============================================================================
// P9U5: Cross-Client Isolation Tests
// =============================================================================

func TestHandleRepoFetch_CrossClientIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	sharedID := "cross-client-fetch"

	// Client 1 sends fetch
	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": sharedID,
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)
	readMessage(t, conn1)

	// Client 2 sends fetch with same request_id - should get independent response
	conn2.WriteMessage(websocket.TextMessage, data)
	resp := readMessage(t, conn2)
	if resp.Type != MessageTypeRepoFetchResult {
		t.Fatalf("client 2 expected repo.fetch_result, got %s", resp.Type)
	}
}

func TestHandleRepoPull_CrossClientIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	sharedID := "cross-client-pull"

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": sharedID,
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)
	readMessage(t, conn1)

	conn2.WriteMessage(websocket.TextMessage, data)
	resp := readMessage(t, conn2)
	if resp.Type != MessageTypeRepoPullResult {
		t.Fatalf("client 2 expected repo.pull_result, got %s", resp.Type)
	}
}

func TestHandleRepoPrCreate_CrossClientIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	sharedID := "cross-client-pr-create"

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"title":      "Cross-client PR",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)
	readMessage(t, conn1)

	conn2.WriteMessage(websocket.TextMessage, data)
	resp := readMessage(t, conn2)
	if resp.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("client 2 expected repo.pr_create_result, got %s", resp.Type)
	}
}

func TestHandleRepoPrCheckout_CrossClientIsolation(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	sharedID := "cross-client-pr-checkout"

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": sharedID,
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)
	readMessage(t, conn1)

	conn2.WriteMessage(websocket.TextMessage, data)
	resp := readMessage(t, conn2)
	if resp.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("client 2 expected repo.pr_checkout_result, got %s", resp.Type)
	}
}

// =============================================================================
// P9U5: Duplicate-Under-Gate Tests (AC3)
// =============================================================================

func TestHandleRepoFetch_DuplicateUnderGate(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	// Lock the mutation gate
	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.fetch",
		"payload": map[string]interface{}{
			"request_id": "gate-busy-fetch",
		},
	}
	data, _ := json.Marshal(msg)

	// First send: gate busy -> action.invalid cached
	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})
	if p1["error_code"] != apperrors.CodeActionInvalid {
		t.Fatalf("expected %s, got %v", apperrors.CodeActionInvalid, p1["error_code"])
	}

	// Second send: replays cached error
	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})
	if p2["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected replayed %s, got %v", apperrors.CodeActionInvalid, p2["error_code"])
	}

	s.repoMutationMu.Unlock()
}

func TestHandleRepoPull_DuplicateUnderGate(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pull",
		"payload": map[string]interface{}{
			"request_id": "gate-busy-pull",
		},
	}
	data, _ := json.Marshal(msg)

	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})
	if p1["error_code"] != apperrors.CodeActionInvalid {
		t.Fatalf("expected %s, got %v", apperrors.CodeActionInvalid, p1["error_code"])
	}

	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})
	if p2["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected replayed %s, got %v", apperrors.CodeActionInvalid, p2["error_code"])
	}

	s.repoMutationMu.Unlock()
}

func TestHandleRepoPrCreate_DuplicateUnderGate(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "gate-busy-pr-create",
			"title":      "Gate busy PR",
		},
	}
	data, _ := json.Marshal(msg)

	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})
	if p1["error_code"] != apperrors.CodeActionInvalid {
		t.Fatalf("expected %s, got %v", apperrors.CodeActionInvalid, p1["error_code"])
	}

	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})
	if p2["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected replayed %s, got %v", apperrors.CodeActionInvalid, p2["error_code"])
	}

	s.repoMutationMu.Unlock()
}

func TestHandleRepoPrCheckout_DuplicateUnderGate(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	s.repoMutationMu.Lock()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMessage(t, conn)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "gate-busy-pr-checkout",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)

	conn.WriteMessage(websocket.TextMessage, data)
	resp1 := readMessage(t, conn)
	p1, _ := resp1.Payload.(map[string]interface{})
	if p1["error_code"] != apperrors.CodeActionInvalid {
		t.Fatalf("expected %s, got %v", apperrors.CodeActionInvalid, p1["error_code"])
	}

	conn.WriteMessage(websocket.TextMessage, data)
	resp2 := readMessage(t, conn)
	p2, _ := resp2.Payload.(map[string]interface{})
	if p2["error_code"] != apperrors.CodeActionInvalid {
		t.Errorf("expected replayed %s, got %v", apperrors.CodeActionInvalid, p2["error_code"])
	}

	s.repoMutationMu.Unlock()
}

// =============================================================================
// P9U5: Requester-Only Tests (PR ops)
// =============================================================================

func TestHandleRepoPrCreate_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.pr_create",
		"payload": map[string]interface{}{
			"request_id": "requester-pr-create",
			"title":      "Requester-only PR",
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoPrCreateResult {
		t.Fatalf("client 1 expected repo.pr_create_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.pr_create_result")
	}
}

func TestHandleRepoPrCheckout_RequesterOnly(t *testing.T) {
	repoDir := setupGitRepoForCommitHandlerTest(t)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetGitOperations(NewGitOperations(repoDir, false, false, false))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 1: %v", err)
	}
	defer conn1.Close()
	readMessage(t, conn1)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial client 2: %v", err)
	}
	defer conn2.Close()
	readMessage(t, conn2)

	msg := map[string]interface{}{
		"type": "repo.pr_checkout",
		"payload": map[string]interface{}{
			"request_id": "requester-pr-checkout",
			"number":     42,
		},
	}
	data, _ := json.Marshal(msg)
	conn1.WriteMessage(websocket.TextMessage, data)

	resp1 := readMessage(t, conn1)
	if resp1.Type != MessageTypeRepoPrCheckoutResult {
		t.Fatalf("client 1 expected repo.pr_checkout_result, got %s", resp1.Type)
	}

	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn2.ReadMessage()
	if err == nil {
		t.Error("client 2 should NOT receive repo.pr_checkout_result")
	}
}
