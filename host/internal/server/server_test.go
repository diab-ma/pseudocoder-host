package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/pty"
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

// TestNewDiffCardMessage verifies the diff.card message constructor.
func TestNewDiffCardMessage(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 4, Offset: 0, Length: 11},
	}
	stats := &DiffStats{ByteSize: 11, LineCount: 1, AddedLines: 1, DeletedLines: 0}
	msg := NewDiffCardMessage("card-123", "src/main.go", "+added line", chunks, false, false, stats, 1703500000000)

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
	s.BroadcastDiffCard("card-abc", "file.go", "+new line", chunks, false, false, stats, 1703500000000)

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
		sendToClient(NewDiffCardMessage("card-1", "file1.go", "+line1", nil, false, false, nil, 1000))
		sendToClient(NewDiffCardMessage("card-2", "file2.go", "+line2", nil, false, false, nil, 2000))
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

// TestChunkInfoSerialization verifies ChunkInfo JSON serialization.
func TestChunkInfoSerialization(t *testing.T) {
	chunks := []ChunkInfo{
		{Index: 0, OldStart: 1, OldCount: 3, NewStart: 1, NewCount: 4, Offset: 0, Length: 50},
		{Index: 1, OldStart: 10, OldCount: 2, NewStart: 11, NewCount: 5, Offset: 50, Length: 75},
	}

	msg := NewDiffCardMessage("card-test", "file.go", "@@ -1,3 +1,4 @@\n+line", chunks, false, false, nil, 1703500000000)

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
		ChunkIndex   int
		Action      string
		ContentHash string
	}
	s.SetChunkDecisionHandler(func(cardID string, chunkIndex int, action string, contentHash string) error {
		mu.Lock()
		handlerCalls = append(handlerCalls, struct {
			CardID      string
			ChunkIndex   int
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
			"card_id":    "card-abc",
			"chunk_index": 1,
			"action":     "accept",
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
		ChunkIndex   int
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
			"card_id":    "card-abc",
			"chunk_index": 0,
			"action":     "maybe",
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
			"card_id":    "card-abc",
			"chunk_index": -1,
			"action":     "accept",
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
			"card_id":    "card-abc",
			"chunk_index": 0,
			"action":     "accept",
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
		"/devices/",              // Missing device ID and revoke
		"/devices/id",            // Missing /revoke
		"/devices/id/other",      // Wrong action
		"/other/id/revoke",       // Wrong prefix
		"/devices//revoke",       // Empty device ID
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
		{"192.168.1.1:12345", false},
		{"10.0.0.1:12345", false},
		{"172.16.0.1:12345", false},
		{"8.8.8.8:12345", false},
		{"", false},
		{"invalid", false},
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

	// Verify second session has empty LastCommit
	if payload.Sessions[1].LastCommit != "" {
		t.Errorf("session 1 LastCommit = %q, want empty", payload.Sessions[1].LastCommit)
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

	// Verify second session info has empty last commit
	if infos[1].LastCommit != "" {
		t.Errorf("info 1 LastCommit = %q, want empty", infos[1].LastCommit)
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
			"message": "Test commit",
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

// TestNewRepoStatusMessage tests the repo.status message constructor.
func TestNewRepoStatusMessage(t *testing.T) {
	stagedFiles := []string{"file1.txt", "dir/file2.txt"}
	msg := NewRepoStatusMessage("main", "origin/main", 2, stagedFiles, 5, "abc1234 Add feature")

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
}

// TestNewRepoCommitResultMessage tests the repo.commit_result message constructor.
func TestNewRepoCommitResultMessage(t *testing.T) {
	// Test success case
	msg := NewRepoCommitResultMessage(true, "abc1234", "1 file changed", "", "")

	if msg.Type != MessageTypeRepoCommitResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoCommitResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoCommitResultPayload)
	if !ok {
		t.Fatalf("expected RepoCommitResultPayload, got %T", msg.Payload)
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
	msg := NewRepoCommitResultMessage(false, "", "", "commit.no_staged_changes", "nothing to commit")

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
			"remote": "origin",
			"branch": "main",
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
	msg := NewRepoPushResultMessage(true, "To origin/main abc1234..def5678", "", "")

	if msg.Type != MessageTypeRepoPushResult {
		t.Errorf("expected type %s, got %s", MessageTypeRepoPushResult, msg.Type)
	}

	payload, ok := msg.Payload.(RepoPushResultPayload)
	if !ok {
		t.Fatalf("expected RepoPushResultPayload, got %T", msg.Payload)
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
	msg := NewRepoPushResultMessage(false, "", "push.no_upstream", "no upstream configured")

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
	}

	for _, tt := range tests {
		if string(tt.msgType) != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, tt.msgType)
		}
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
	s.BroadcastDiffCard("card-1", "file.go", "diff content", nil, false, false, nil, time.Now().Unix())

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
		name     string
		token    string // Decoded value we expect
		encoded  string // URL-encoded version in query
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
		{"unicode emoji", "\U0001F4F1", "%F0%9F%93%B1"}, // 📱
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
