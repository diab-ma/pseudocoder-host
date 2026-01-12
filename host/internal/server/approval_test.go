package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apperrors "github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/storage"
)

// TestApprovalQueue_QueueAndDecide tests the normal flow of queueing
// an approval request and receiving a decision.
func TestApprovalQueue_QueueAndDecide(t *testing.T) {
	var mu sync.Mutex
	var broadcasted []Message
	broadcaster := func(msg Message) {
		mu.Lock()
		broadcasted = append(broadcasted, msg)
		mu.Unlock()
	}

	queue := NewApprovalQueue(5*time.Second, broadcaster)

	// Queue an approval request in a goroutine
	var response ApprovalResponse
	var queueErr error
	done := make(chan struct{})

	go func() {
		response, queueErr = queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "test-request-1",
			Command:   "rm -rf important",
			Cwd:       "/home/user",
			Repo:      "/home/user/project",
			Rationale: "Cleaning up build artifacts",
		})
		close(done)
	}()

	// Wait a bit for the request to be queued
	time.Sleep(50 * time.Millisecond)

	// Verify the request was broadcast
	mu.Lock()
	broadcastCount := len(broadcasted)
	var firstMsg Message
	if broadcastCount > 0 {
		firstMsg = broadcasted[0]
	}
	mu.Unlock()

	if broadcastCount != 1 {
		t.Fatalf("expected 1 broadcast, got %d", broadcastCount)
	}
	if firstMsg.Type != MessageTypeApprovalRequest {
		t.Errorf("expected type %s, got %s", MessageTypeApprovalRequest, firstMsg.Type)
	}

	// Approve the request
	err := queue.Decide("test-request-1", "approve", nil)
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}

	// Wait for Queue to return
	<-done

	// Verify response
	if queueErr != nil {
		t.Fatalf("Queue returned error: %v", queueErr)
	}
	if response.Decision != "approve" {
		t.Errorf("expected decision 'approve', got %q", response.Decision)
	}
	if response.TemporaryAllowUntil != nil {
		t.Errorf("expected nil TemporaryAllowUntil, got %v", response.TemporaryAllowUntil)
	}
}

// TestApprovalQueue_QueueAndDecide_Deny tests the deny flow
func TestApprovalQueue_QueueAndDecide_Deny(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	var response ApprovalResponse
	var queueErr error
	done := make(chan struct{})

	go func() {
		response, queueErr = queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "test-deny",
			Command:   "dangerous command",
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	err := queue.Decide("test-deny", "deny", nil)
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}

	<-done

	// Verify we get an error for deny
	if queueErr == nil {
		t.Fatal("expected error for deny, got nil")
	}
	if !apperrors.IsCode(queueErr, apperrors.CodeApprovalDenied) {
		t.Errorf("expected code %s, got %s", apperrors.CodeApprovalDenied, apperrors.GetCode(queueErr))
	}
	if response.Decision != "deny" {
		t.Errorf("expected decision 'deny', got %q", response.Decision)
	}
}

// TestApprovalQueue_Timeout tests that requests timeout correctly
func TestApprovalQueue_Timeout(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, func(msg Message) {})

	start := time.Now()
	response, err := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "test-timeout",
		Command:   "slow command",
	})
	elapsed := time.Since(start)

	// Verify timeout occurred
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !apperrors.IsCode(err, apperrors.CodeApprovalTimeout) {
		t.Errorf("expected code %s, got %s", apperrors.CodeApprovalTimeout, apperrors.GetCode(err))
	}
	if response.Decision != "deny" {
		t.Errorf("expected decision 'deny' on timeout, got %q", response.Decision)
	}

	// Verify timeout was roughly correct (allow some slack)
	if elapsed < 80*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Errorf("expected timeout around 100ms, got %v", elapsed)
	}
}

// TestApprovalQueue_DecideNonExistent tests error for unknown request
func TestApprovalQueue_DecideNonExistent(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	err := queue.Decide("nonexistent-request", "approve", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent request, got nil")
	}
	if !apperrors.IsCode(err, apperrors.CodeApprovalNotFound) {
		t.Errorf("expected code %s, got %s", apperrors.CodeApprovalNotFound, apperrors.GetCode(err))
	}
}

// TestApprovalQueue_DuplicateRequestID tests that duplicate request IDs are rejected
func TestApprovalQueue_DuplicateRequestID(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	// Queue first request in background
	done := make(chan struct{})
	go func() {
		queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "duplicate-id",
			Command:   "first command",
		})
		close(done)
	}()

	// Wait for first request to be queued
	time.Sleep(50 * time.Millisecond)

	// Verify first request is pending
	if queue.PendingCount() != 1 {
		t.Fatalf("expected 1 pending request, got %d", queue.PendingCount())
	}

	// Try to queue second request with same ID - should fail immediately
	_, err := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "duplicate-id",
		Command:   "second command",
	})
	if err == nil {
		t.Fatal("expected error for duplicate request ID, got nil")
	}
	if !apperrors.IsCode(err, apperrors.CodeApprovalDuplicate) {
		t.Errorf("expected code %s, got %s", apperrors.CodeApprovalDuplicate, apperrors.GetCode(err))
	}

	// First request should still be pending
	if queue.PendingCount() != 1 {
		t.Errorf("expected 1 pending request after duplicate rejection, got %d", queue.PendingCount())
	}

	// Clean up: approve the first request so goroutine exits
	queue.Decide("duplicate-id", "approve", nil)
	<-done
}

// TestApprovalQueue_Cancel tests cancellation before decision
func TestApprovalQueue_Cancel(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	// Queue a request
	done := make(chan struct{})
	go func() {
		queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "test-cancel",
			Command:   "command",
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	// Cancel the request
	queue.Cancel("test-cancel")

	// Verify it's no longer pending
	if queue.PendingCount() != 0 {
		t.Errorf("expected 0 pending after cancel, got %d", queue.PendingCount())
	}

	// The Queue goroutine should timeout (since we cancelled without deciding)
	select {
	case <-done:
		// Expected - Queue timed out
	case <-time.After(6 * time.Second):
		t.Fatal("Queue did not return after cancel and timeout")
	}
}

// TestApprovalQueue_ContextCancel tests context cancellation
func TestApprovalQueue_ContextCancel(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var queueErr error
	go func() {
		_, queueErr = queue.Queue(ctx, ApprovalRequest{
			RequestID: "test-ctx-cancel",
			Command:   "command",
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	<-done

	if queueErr != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", queueErr)
	}
}

// TestApprovalQueue_ConcurrentAccess tests thread safety
func TestApprovalQueue_ConcurrentAccess(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	var wg sync.WaitGroup
	const numRequests = 10

	// Queue multiple requests concurrently
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		requestID := "concurrent-" + string(rune('a'+i))
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			queue.Queue(ctx, ApprovalRequest{
				RequestID: id,
				Command:   "cmd " + id,
			})
		}(requestID)
	}

	// Decide on some concurrently
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < numRequests/2; i++ {
		wg.Add(1)
		requestID := "concurrent-" + string(rune('a'+i))
		go func(id string) {
			defer wg.Done()
			queue.Decide(id, "approve", nil)
		}(requestID)
	}

	wg.Wait()

	// Should not panic and all pending should eventually clear
	// (remaining half will timeout)
	time.Sleep(250 * time.Millisecond)
	if queue.PendingCount() != 0 {
		t.Errorf("expected 0 pending after all complete, got %d", queue.PendingCount())
	}
}

// TestApprovalQueue_GetPending tests listing pending requests
func TestApprovalQueue_GetPending(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	// Initially empty
	if len(queue.GetPending()) != 0 {
		t.Errorf("expected 0 pending initially, got %d", len(queue.GetPending()))
	}

	// Queue some requests
	go queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "pending-1",
		Command:   "cmd1",
	})
	go queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "pending-2",
		Command:   "cmd2",
	})

	time.Sleep(50 * time.Millisecond)

	pending := queue.GetPending()
	if len(pending) != 2 {
		t.Errorf("expected 2 pending, got %d", len(pending))
	}

	// Verify request data is preserved
	found := make(map[string]bool)
	for _, req := range pending {
		found[req.RequestID] = true
	}
	if !found["pending-1"] || !found["pending-2"] {
		t.Errorf("expected pending-1 and pending-2, got %v", found)
	}
}

// TestApprovalQueue_TemporaryAllowUntil tests the temporary allow feature
func TestApprovalQueue_TemporaryAllowUntil(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})

	var response ApprovalResponse
	done := make(chan struct{})

	go func() {
		response, _ = queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "test-temp-allow",
			Command:   "npm install",
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	// Approve with temporary allow until
	tempAllow := time.Now().Add(15 * time.Minute)
	err := queue.Decide("test-temp-allow", "approve", &tempAllow)
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}

	<-done

	if response.Decision != "approve" {
		t.Errorf("expected decision 'approve', got %q", response.Decision)
	}
	if response.TemporaryAllowUntil == nil {
		t.Fatal("expected TemporaryAllowUntil to be set")
	}
	if !response.TemporaryAllowUntil.Equal(tempAllow) {
		t.Errorf("expected %v, got %v", tempAllow, *response.TemporaryAllowUntil)
	}
}

// TestApprovalQueue_AutoGenerateRequestID tests that request IDs are generated
func TestApprovalQueue_AutoGenerateRequestID(t *testing.T) {
	var mu sync.Mutex
	var broadcasted Message
	queue := NewApprovalQueue(100*time.Millisecond, func(msg Message) {
		mu.Lock()
		broadcasted = msg
		mu.Unlock()
	})

	// Queue without request ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		queue.Queue(ctx, ApprovalRequest{
			Command: "test command",
		})
	}()

	time.Sleep(30 * time.Millisecond)

	// Verify request ID was generated
	mu.Lock()
	msg := broadcasted
	mu.Unlock()

	payload := msg.Payload.(ApprovalRequestPayload)
	if payload.RequestID == "" {
		t.Error("expected request ID to be auto-generated")
	}
}

// TestApprovalQueue_NilBroadcaster tests that nil broadcaster is handled
func TestApprovalQueue_NilBroadcaster(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Should not panic with nil broadcaster
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		queue.Queue(ctx, ApprovalRequest{
			RequestID: "test-nil-broadcaster",
			Command:   "test",
		})
		close(done)
	}()

	<-done
	// Test passes if no panic occurred
}

// TestNewApprovalRequestMessage tests the message constructor
func TestNewApprovalRequestMessage(t *testing.T) {
	expiresAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	msg := NewApprovalRequestMessage(
		"req-123",
		"npm install express",
		"/home/user/project",
		"/home/user/project",
		"Installing dependencies",
		expiresAt,
	)

	if msg.Type != MessageTypeApprovalRequest {
		t.Errorf("expected type %s, got %s", MessageTypeApprovalRequest, msg.Type)
	}

	payload, ok := msg.Payload.(ApprovalRequestPayload)
	if !ok {
		t.Fatalf("expected ApprovalRequestPayload, got %T", msg.Payload)
	}

	if payload.RequestID != "req-123" {
		t.Errorf("expected request_id 'req-123', got %q", payload.RequestID)
	}
	if payload.Command != "npm install express" {
		t.Errorf("expected command 'npm install express', got %q", payload.Command)
	}
	if payload.Cwd != "/home/user/project" {
		t.Errorf("expected cwd '/home/user/project', got %q", payload.Cwd)
	}
	if payload.Rationale != "Installing dependencies" {
		t.Errorf("expected rationale 'Installing dependencies', got %q", payload.Rationale)
	}
	if payload.ExpiresAt != "2025-01-15T10:30:00Z" {
		t.Errorf("expected expires_at '2025-01-15T10:30:00Z', got %q", payload.ExpiresAt)
	}
}

// =============================================================================
// Handler Validation Tests
// These test the handleApprovalDecision method's validation logic
// =============================================================================

// createTestClientForApproval creates a minimal client for testing the handler.
// It returns the client and a channel to receive error messages.
func createTestClientForApproval(queue *ApprovalQueue) (*Client, chan Message) {
	server := &Server{
		approvalQueue: queue,
	}
	sendCh := make(chan Message, 10)
	client := &Client{
		server: server,
		send:   sendCh,
		done:   make(chan struct{}),
	}
	return client, sendCh
}

// TestHandleApprovalDecision_InvalidJSON tests that invalid JSON is rejected
func TestHandleApprovalDecision_InvalidJSON(t *testing.T) {
	client, sendCh := createTestClientForApproval(nil)

	// Send invalid JSON
	client.handleApprovalDecision([]byte("not valid json"))

	// Should receive an error message
	select {
	case msg := <-sendCh:
		if msg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", msg.Type)
		}
		payload := msg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected code %s, got %s", apperrors.CodeServerInvalidMessage, payload.Code)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_MissingRequestID tests that missing request_id is rejected
func TestHandleApprovalDecision_MissingRequestID(t *testing.T) {
	client, sendCh := createTestClientForApproval(nil)

	// Send message with empty request_id
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID: "", // Missing
			Decision:  "approve",
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Should receive an error message
	select {
	case errMsg := <-sendCh:
		if errMsg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", errMsg.Type)
		}
		payload := errMsg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected code %s, got %s", apperrors.CodeServerInvalidMessage, payload.Code)
		}
		if payload.Message != "request_id is required" {
			t.Errorf("expected message 'request_id is required', got %q", payload.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_InvalidDecision tests that invalid decision values are rejected
func TestHandleApprovalDecision_InvalidDecision(t *testing.T) {
	client, sendCh := createTestClientForApproval(nil)

	// Send message with invalid decision
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID: "test-123",
			Decision:  "maybe", // Invalid - must be "approve" or "deny"
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Should receive an error message
	select {
	case errMsg := <-sendCh:
		if errMsg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", errMsg.Type)
		}
		payload := errMsg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected code %s, got %s", apperrors.CodeServerInvalidMessage, payload.Code)
		}
		if payload.Message != "decision must be 'approve' or 'deny'" {
			t.Errorf("expected decision validation message, got %q", payload.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_NoQueue tests that missing queue returns error
func TestHandleApprovalDecision_NoQueue(t *testing.T) {
	client, sendCh := createTestClientForApproval(nil) // No queue

	// Send valid message but no queue configured
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID: "test-123",
			Decision:  "approve",
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Should receive handler missing error
	select {
	case errMsg := <-sendCh:
		if errMsg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", errMsg.Type)
		}
		payload := errMsg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeServerHandlerMissing {
			t.Errorf("expected code %s, got %s", apperrors.CodeServerHandlerMissing, payload.Code)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_RequestNotFound tests that nonexistent request returns error
func TestHandleApprovalDecision_RequestNotFound(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	client, sendCh := createTestClientForApproval(queue)

	// Send decision for nonexistent request
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID: "nonexistent-request",
			Decision:  "approve",
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Should receive not found error
	select {
	case errMsg := <-sendCh:
		if errMsg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", errMsg.Type)
		}
		payload := errMsg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeApprovalNotFound {
			t.Errorf("expected code %s, got %s", apperrors.CodeApprovalNotFound, payload.Code)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_InvalidTemporaryAllowUntil tests invalid timestamp
func TestHandleApprovalDecision_InvalidTemporaryAllowUntil(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	client, sendCh := createTestClientForApproval(queue)

	// Send decision with invalid timestamp
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID:           "test-123",
			Decision:            "approve",
			TemporaryAllowUntil: "not-a-valid-timestamp",
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Should receive invalid timestamp error
	select {
	case errMsg := <-sendCh:
		if errMsg.Type != MessageTypeError {
			t.Errorf("expected error message, got %s", errMsg.Type)
		}
		payload := errMsg.Payload.(ErrorPayload)
		if payload.Code != apperrors.CodeServerInvalidMessage {
			t.Errorf("expected code %s, got %s", apperrors.CodeServerInvalidMessage, payload.Code)
		}
		if payload.Message != "invalid temporary_allow_until timestamp" {
			t.Errorf("expected timestamp validation message, got %q", payload.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected error message, got none")
	}
}

// TestHandleApprovalDecision_Success tests the full success path through the handler
func TestHandleApprovalDecision_Success(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	client, sendCh := createTestClientForApproval(queue)

	// Queue a request first
	done := make(chan struct{})
	var response ApprovalResponse
	go func() {
		response, _ = queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "handler-test-123",
			Command:   "test command",
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	// Send decision through handler
	msg := Message{
		Type: MessageTypeApprovalDecision,
		Payload: ApprovalDecisionPayload{
			RequestID: "handler-test-123",
			Decision:  "approve",
		},
	}
	data, _ := json.Marshal(msg)
	client.handleApprovalDecision(data)

	// Wait for Queue to return
	<-done

	// Verify no error was sent (success case doesn't send error)
	select {
	case errMsg := <-sendCh:
		t.Errorf("unexpected message: %+v", errMsg)
	case <-time.After(50 * time.Millisecond):
		// Expected - no error message on success
	}

	// Verify the response was received
	if response.Decision != "approve" {
		t.Errorf("expected decision 'approve', got %q", response.Decision)
	}
}

// =============================================================================
// ApproveHandler HTTP Tests (Phase 6.1b)
// These test the /approve HTTP endpoint handler
// =============================================================================

// mockTokenValidator is a mock implementation of ApprovalTokenValidator for tests.
type mockTokenValidator struct {
	validToken string
}

func (m *mockTokenValidator) ValidateToken(token string) bool {
	return m.validToken != "" && token == m.validToken
}

// createTestApproveHandler creates a handler with a mock token validator and optional queue.
func createTestApproveHandler(validToken string, queue *ApprovalQueue) *ApproveHandler {
	server := NewServer("127.0.0.1:0")
	if queue != nil {
		server.SetApprovalQueue(queue)
	}
	return NewApproveHandler(server, &mockTokenValidator{validToken: validToken})
}

// TestApproveHandler_LoopbackOnly verifies that non-loopback requests are rejected
func TestApproveHandler_LoopbackOnly(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"test"}`))
	req.RemoteAddr = "192.168.1.100:12345" // Non-loopback address
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Approved {
		t.Error("expected approved=false for non-loopback request")
	}
	if resp.Error != "forbidden" {
		t.Errorf("expected error 'forbidden', got %q", resp.Error)
	}
}

// TestApproveHandler_PostOnly verifies that GET requests are rejected
func TestApproveHandler_PostOnly(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodGet, "/approve", nil)
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Bearer valid-token")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "method_not_allowed" {
		t.Errorf("expected error 'method_not_allowed', got %q", resp.Error)
	}
}

// TestApproveHandler_TokenRequired verifies that missing Authorization header is rejected
func TestApproveHandler_TokenRequired(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"test"}`))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	// No Authorization header

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "approval.invalid_token" {
		t.Errorf("expected error 'approval.invalid_token', got %q", resp.Error)
	}
}

// TestApproveHandler_InvalidToken verifies that wrong token is rejected
func TestApproveHandler_InvalidToken(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"test"}`))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "approval.invalid_token" {
		t.Errorf("expected error 'approval.invalid_token', got %q", resp.Error)
	}
}

// TestApproveHandler_InvalidBearerFormat verifies that non-Bearer auth is rejected
func TestApproveHandler_InvalidBearerFormat(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"test"}`))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Basic abc123") // Wrong format
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "approval.invalid_token" {
		t.Errorf("expected error 'approval.invalid_token', got %q", resp.Error)
	}
}

// TestApproveHandler_InvalidJSON verifies that invalid JSON body is rejected
func TestApproveHandler_InvalidJSON(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader("not valid json"))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "invalid_request" {
		t.Errorf("expected error 'invalid_request', got %q", resp.Error)
	}
}

// TestApproveHandler_MissingCommand verifies that empty command is rejected
func TestApproveHandler_MissingCommand(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"","cwd":"/tmp"}`))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "invalid_request" {
		t.Errorf("expected error 'invalid_request', got %q", resp.Error)
	}
	if !strings.Contains(resp.Message, "Command is required") {
		t.Errorf("expected 'Command is required' in message, got %q", resp.Message)
	}
}

// TestApproveHandler_NoQueue verifies that missing queue returns service unavailable
func TestApproveHandler_NoQueue(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil) // No queue

	body := `{"command":"test command","cwd":"/tmp","repo":"/tmp","rationale":"testing"}`
	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Error != "service_unavailable" {
		t.Errorf("expected error 'service_unavailable', got %q", resp.Error)
	}
}

// TestApproveHandler_Success tests the full approval flow
func TestApproveHandler_Success(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	body := `{"command":"ls -la","cwd":"/tmp","repo":"/tmp","rationale":"list files"}`

	// Start the request in a goroutine since it blocks
	done := make(chan struct{})
	var resp approveResponse
	var statusCode int
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/approve", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		statusCode = w.Code
		json.Unmarshal(w.Body.Bytes(), &resp)
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Approve the request
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	queue.Decide(pending[0].RequestID, "approve", nil)

	// Wait for handler to return
	<-done

	if statusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", statusCode)
	}
	if !resp.Approved {
		t.Errorf("expected approved=true, got false")
	}
	if resp.Error != "" {
		t.Errorf("expected no error, got %q", resp.Error)
	}
}

// TestApproveHandler_Denied tests the denial flow
func TestApproveHandler_Denied(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	body := `{"command":"rm -rf /","cwd":"/","repo":"/","rationale":"dangerous"}`

	// Start the request in a goroutine
	done := make(chan struct{})
	var resp approveResponse
	var statusCode int
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/approve", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		statusCode = w.Code
		json.Unmarshal(w.Body.Bytes(), &resp)
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Deny the request
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	queue.Decide(pending[0].RequestID, "deny", nil)

	// Wait for handler to return
	<-done

	if statusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", statusCode)
	}
	if resp.Approved {
		t.Errorf("expected approved=false, got true")
	}
	if resp.Error != apperrors.CodeApprovalDenied {
		t.Errorf("expected error '%s', got %q", apperrors.CodeApprovalDenied, resp.Error)
	}
}

// TestApproveHandler_Timeout tests the timeout flow
func TestApproveHandler_Timeout(t *testing.T) {
	// Short timeout for testing
	queue := NewApprovalQueue(100*time.Millisecond, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	body := `{"command":"test","cwd":"/tmp","repo":"/tmp","rationale":"test"}`

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp approveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Approved {
		t.Errorf("expected approved=false on timeout, got true")
	}
	if resp.Error != apperrors.CodeApprovalTimeout {
		t.Errorf("expected error '%s', got %q", apperrors.CodeApprovalTimeout, resp.Error)
	}
}

// TestApproveHandler_TemporaryAllowUntil tests that temporary allow is returned
func TestApproveHandler_TemporaryAllowUntil(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	body := `{"command":"test","cwd":"/tmp","repo":"/tmp","rationale":"test"}`

	// Start the request in a goroutine
	done := make(chan struct{})
	var resp approveResponse
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/approve", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		json.Unmarshal(w.Body.Bytes(), &resp)
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Approve with temporary allow
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	tempAllowUntil := time.Now().Add(15 * time.Minute)
	queue.Decide(pending[0].RequestID, "approve", &tempAllowUntil)

	// Wait for handler to return
	<-done

	if !resp.Approved {
		t.Errorf("expected approved=true, got false")
	}
	if resp.TemporaryAllowUntil == nil {
		t.Error("expected temporary_allow_until to be set")
	} else {
		// Parse the timestamp to verify it's valid
		_, err := time.Parse(time.RFC3339, *resp.TemporaryAllowUntil)
		if err != nil {
			t.Errorf("invalid temporary_allow_until timestamp: %v", err)
		}
	}
}

// TestApproveHandler_IPv6Loopback tests that IPv6 loopback is accepted
func TestApproveHandler_IPv6Loopback(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	body := `{"command":"test","cwd":"/tmp","repo":"/tmp","rationale":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
	req.RemoteAddr = "[::1]:12345" // IPv6 loopback
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should succeed (not blocked by loopback check)
	// Will timeout but that means it passed the loopback check
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (with timeout), got %d", w.Code)
	}

	var resp approveResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Should be timeout, not forbidden
	if resp.Error == "forbidden" {
		t.Error("IPv6 loopback should not be rejected as forbidden")
	}
}

// TestApproveHandler_LongCommandString tests handling of very long command strings
func TestApproveHandler_LongCommandString(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	// Create a 100KB command string
	longCommand := strings.Repeat("x", 100*1024)
	body := fmt.Sprintf(`{"command":%q,"cwd":"/tmp","repo":"/tmp","rationale":"test"}`, longCommand)

	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should succeed parsing and queue the request (will timeout)
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp approveResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Will timeout since we don't approve it
	if resp.Error != apperrors.CodeApprovalTimeout {
		t.Errorf("expected timeout error, got %q", resp.Error)
	}
}

// TestApproveHandler_SpecialCharacters tests unicode and special characters in request fields
func TestApproveHandler_SpecialCharacters(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	// Unicode, JSON escapes, control characters (except actual control chars which are invalid JSON)
	body := `{"command":"echo '日本語' && ls \"quoted\"","cwd":"/tmp/путь","repo":"/tmp/emoji/🚀","rationale":"Testing\tspecial\ncharacters"}`

	// Run handler in goroutine since it blocks waiting for approval
	done := make(chan struct{})
	var statusCode int
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		statusCode = w.Code
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Verify the request was queued with correct content
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	if pending[0].Command != "echo '日本語' && ls \"quoted\"" {
		t.Errorf("command not preserved correctly: %q", pending[0].Command)
	}
	if pending[0].Cwd != "/tmp/путь" {
		t.Errorf("cwd not preserved correctly: %q", pending[0].Cwd)
	}
	if pending[0].Repo != "/tmp/emoji/🚀" {
		t.Errorf("repo not preserved correctly: %q", pending[0].Repo)
	}

	// Approve to let the handler complete
	queue.Decide(pending[0].RequestID, "approve", nil)
	<-done

	// Should succeed
	if statusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", statusCode)
	}
}

// TestApproveHandler_AuthHeaderEdgeCases tests various Authorization header formats
func TestApproveHandler_AuthHeaderEdgeCases(t *testing.T) {
	handler := createTestApproveHandler("valid-token", nil)

	testCases := []struct {
		name       string
		authHeader string
		expectCode int
	}{
		{"multiple spaces after Bearer", "Bearer  valid-token", http.StatusUnauthorized},
		{"lowercase bearer", "bearer valid-token", http.StatusUnauthorized},
		{"no space after Bearer", "Bearervalid-token", http.StatusUnauthorized},
		{"trailing space in token", "Bearer valid-token ", http.StatusUnauthorized},
		{"leading space in token", "Bearer  valid-token", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"just Bearer", "Bearer", http.StatusUnauthorized},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(`{"command":"test"}`))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Authorization", tc.authHeader)
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.expectCode {
				t.Errorf("expected status %d, got %d", tc.expectCode, w.Code)
			}
		})
	}
}

// TestApproveHandler_EmptyOptionalFields tests that empty cwd/repo/rationale are accepted
func TestApproveHandler_EmptyOptionalFields(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	// Only command is required
	body := `{"command":"test"}`

	// Run handler in goroutine since it blocks waiting for approval
	done := make(chan struct{})
	var statusCode int
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		statusCode = w.Code
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Verify request was queued with empty optional fields
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	if pending[0].Command != "test" {
		t.Errorf("command not set correctly: %q", pending[0].Command)
	}
	if pending[0].Cwd != "" {
		t.Errorf("cwd should be empty, got %q", pending[0].Cwd)
	}
	if pending[0].Repo != "" {
		t.Errorf("repo should be empty, got %q", pending[0].Repo)
	}
	if pending[0].Rationale != "" {
		t.Errorf("rationale should be empty, got %q", pending[0].Rationale)
	}

	// Approve to let the handler complete
	queue.Decide(pending[0].RequestID, "approve", nil)
	<-done

	if statusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", statusCode)
	}
}

// TestApproveHandler_ConcurrentRequests tests multiple simultaneous approval requests
func TestApproveHandler_ConcurrentRequests(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("valid-token", queue)

	const numRequests = 10
	var wg sync.WaitGroup
	results := make(chan int, numRequests)

	// Start multiple concurrent requests
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			body := fmt.Sprintf(`{"command":"cmd-%d","cwd":"/tmp","repo":"/tmp","rationale":"test"}`, idx)
			req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(body))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Authorization", "Bearer valid-token")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()

			// Run in goroutine to allow concurrent execution
			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(w, req)
				close(done)
			}()

			// Wait briefly then approve
			time.Sleep(50 * time.Millisecond)
			pending := queue.GetPending()
			for _, p := range pending {
				if p.Command == fmt.Sprintf("cmd-%d", idx) {
					queue.Decide(p.RequestID, "approve", nil)
					break
				}
			}

			<-done
			results <- w.Code
		}(i)
	}

	wg.Wait()
	close(results)

	// All requests should succeed
	successCount := 0
	for code := range results {
		if code == http.StatusOK {
			successCount++
		}
	}

	if successCount != numRequests {
		t.Errorf("expected %d successful requests, got %d", numRequests, successCount)
	}
}

// =============================================================================
// Integration Tests with Real HTTP Server
// These tests use httptest.NewServer for more realistic HTTP testing
// =============================================================================

// TestApproveHandler_IntegrationWithRealServer tests the /approve endpoint with a real HTTP server.
// This validates the full HTTP stack including network handling.
func TestApproveHandler_IntegrationWithRealServer(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("integration-test-token", queue)

	// Create a real test server
	server := httptest.NewServer(handler)
	defer server.Close()

	// Start the request in a goroutine since it blocks
	done := make(chan struct{})
	var resp *http.Response
	var respBody []byte
	var reqErr error
	go func() {
		body := `{"command":"integration-test-cmd","cwd":"/tmp","repo":"/tmp","rationale":"integration test"}`
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/approve", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer integration-test-token")
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, reqErr = client.Do(req)
		if reqErr == nil {
			respBody, _ = json.Marshal(resp) // Just to capture response
			defer resp.Body.Close()
			respBody = make([]byte, 1024)
			n, _ := resp.Body.Read(respBody)
			respBody = respBody[:n]
		}
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(100 * time.Millisecond)

	// Approve the request
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	if pending[0].Command != "integration-test-cmd" {
		t.Errorf("expected command 'integration-test-cmd', got %q", pending[0].Command)
	}
	queue.Decide(pending[0].RequestID, "approve", nil)

	// Wait for response
	<-done

	if reqErr != nil {
		t.Fatalf("request failed: %v", reqErr)
	}

	// Note: The test server runs on 127.0.0.1 so loopback check passes
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d. Body: %s", resp.StatusCode, string(respBody))
	}

	var approveResp approveResponse
	if err := json.Unmarshal(respBody, &approveResp); err != nil {
		t.Fatalf("failed to parse response: %v. Body: %s", err, string(respBody))
	}
	if !approveResp.Approved {
		t.Errorf("expected approved=true, got false. Error: %s", approveResp.Error)
	}
}

// TestApproveHandler_IntegrationInvalidToken tests that invalid token is rejected via real HTTP.
func TestApproveHandler_IntegrationInvalidToken(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("correct-token", queue)

	server := httptest.NewServer(handler)
	defer server.Close()

	body := `{"command":"test","cwd":"/tmp","repo":"/tmp","rationale":"test"}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/approve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}

	respBody := make([]byte, 1024)
	n, _ := resp.Body.Read(respBody)
	var approveResp approveResponse
	if err := json.Unmarshal(respBody[:n], &approveResp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if approveResp.Error != "approval.invalid_token" {
		t.Errorf("expected error 'approval.invalid_token', got %q", approveResp.Error)
	}
}

// TestApproveHandler_IntegrationDenied tests the denial flow via real HTTP.
func TestApproveHandler_IntegrationDenied(t *testing.T) {
	queue := NewApprovalQueue(5*time.Second, func(msg Message) {})
	handler := createTestApproveHandler("test-token", queue)

	server := httptest.NewServer(handler)
	defer server.Close()

	done := make(chan struct{})
	var resp *http.Response
	var respBody []byte
	go func() {
		body := `{"command":"denied-cmd","cwd":"/tmp","repo":"/tmp","rationale":"will be denied"}`
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/approve", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, _ = client.Do(req)
		if resp != nil {
			defer resp.Body.Close()
			respBody = make([]byte, 1024)
			n, _ := resp.Body.Read(respBody)
			respBody = respBody[:n]
		}
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(100 * time.Millisecond)

	// Deny the request
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	queue.Decide(pending[0].RequestID, "deny", nil)

	<-done

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var approveResp approveResponse
	json.Unmarshal(respBody, &approveResp)
	if approveResp.Approved {
		t.Error("expected approved=false")
	}
	if approveResp.Error != apperrors.CodeApprovalDenied {
		t.Errorf("expected error '%s', got %q", apperrors.CodeApprovalDenied, approveResp.Error)
	}
}

// -----------------------------------------------------------------------------
// Temporary Allow Rules Tests
// -----------------------------------------------------------------------------

// TestTemporaryAllowRule_ExactMatch verifies exact command matching.
func TestTemporaryAllowRule_ExactMatch(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Add a temporary allow rule
	expiresAt := time.Now().Add(15 * time.Minute)
	queue.AddTemporaryAllow("go test ./...", expiresAt, "device-1")

	// Check exact match
	ctx := context.Background()
	resp, err := queue.Queue(ctx, ApprovalRequest{
		RequestID: "req-1",
		Command:   "go test ./...",
		Cwd:       "/project",
		Repo:      "/project",
		Rationale: "Run tests",
	})

	if err != nil {
		t.Fatalf("Queue failed: %v", err)
	}
	if resp.Decision != "approve" {
		t.Errorf("Decision = %q, want approve", resp.Decision)
	}
	if resp.TemporaryAllowUntil == nil {
		t.Error("TemporaryAllowUntil should not be nil")
	}
}

// TestTemporaryAllowRule_NoMatch verifies non-matching commands are queued normally.
func TestTemporaryAllowRule_NoMatch(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Add a rule for one command
	expiresAt := time.Now().Add(15 * time.Minute)
	queue.AddTemporaryAllow("go test ./...", expiresAt, "device-1")

	// Try a different command
	ctx := context.Background()
	resp, err := queue.Queue(ctx, ApprovalRequest{
		RequestID: "req-1",
		Command:   "go build ./...", // Different command
		Cwd:       "/project",
		Repo:      "/project",
		Rationale: "Build",
	})

	// Should timeout (no auto-approve)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if resp.Decision != "deny" {
		t.Errorf("Decision = %q, want deny", resp.Decision)
	}
}

// TestTemporaryAllowRule_Expired verifies expired rules are not matched.
func TestTemporaryAllowRule_Expired(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Add an already expired rule
	expiresAt := time.Now().Add(-1 * time.Minute)
	queue.AddTemporaryAllow("go test ./...", expiresAt, "device-1")

	// Try the matching command
	ctx := context.Background()
	resp, err := queue.Queue(ctx, ApprovalRequest{
		RequestID: "req-1",
		Command:   "go test ./...",
		Cwd:       "/project",
		Repo:      "/project",
		Rationale: "Run tests",
	})

	// Should timeout (expired rule ignored)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if resp.Decision != "deny" {
		t.Errorf("Decision = %q, want deny", resp.Decision)
	}
}

// TestTemporaryAllowRule_AutoApprove verifies auto-approval records audit entry.
func TestTemporaryAllowRule_AutoApprove(t *testing.T) {
	var capturedEntry *ApprovalAuditEntry
	mockStore := &mockAuditStore{
		saveFn: func(entry *ApprovalAuditEntry) error {
			capturedEntry = entry
			return nil
		},
	}

	queue := NewApprovalQueue(100*time.Millisecond, nil)
	queue.SetAuditStore(mockStore)

	// Add a temporary allow rule
	expiresAt := time.Now().Add(15 * time.Minute)
	queue.AddTemporaryAllow("go test ./...", expiresAt, "device-1")

	// Auto-approved request
	ctx := context.Background()
	queue.Queue(ctx, ApprovalRequest{
		RequestID: "req-1",
		Command:   "go test ./...",
		Cwd:       "/project",
		Repo:      "/project",
		Rationale: "Run tests",
	})

	if capturedEntry == nil {
		t.Fatal("audit entry not recorded")
	}
	if capturedEntry.Source != "rule" {
		t.Errorf("Source = %q, want rule", capturedEntry.Source)
	}
	if capturedEntry.Decision != "approved" {
		t.Errorf("Decision = %q, want approved", capturedEntry.Decision)
	}
}

// TestCleanExpiredRules verifies expired rules are removed.
func TestCleanExpiredRules(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Add one expired and one active rule
	queue.AddTemporaryAllow("expired", time.Now().Add(-1*time.Minute), "device-1")
	queue.AddTemporaryAllow("active", time.Now().Add(15*time.Minute), "device-2")

	removed := queue.CleanExpiredRules()
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	rules := queue.GetTemporaryAllowRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Command != "active" {
		t.Errorf("remaining rule command = %q, want active", rules[0].Command)
	}
}

// TestAddTemporaryAllow_UpdateExisting verifies updating existing rule.
func TestAddTemporaryAllow_UpdateExisting(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)

	// Add a rule
	oldExpiry := time.Now().Add(5 * time.Minute)
	queue.AddTemporaryAllow("test cmd", oldExpiry, "device-1")

	// Add same command with new expiry
	newExpiry := time.Now().Add(30 * time.Minute)
	queue.AddTemporaryAllow("test cmd", newExpiry, "device-2")

	rules := queue.GetTemporaryAllowRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule (updated), got %d", len(rules))
	}
	if rules[0].DeviceID != "device-2" {
		t.Errorf("DeviceID = %q, want device-2", rules[0].DeviceID)
	}
}

// -----------------------------------------------------------------------------
// Audit Logging Tests
// -----------------------------------------------------------------------------

// mockAuditStore is a mock implementation of ApprovalAuditStore.
type mockAuditStore struct {
	saveFn func(entry *ApprovalAuditEntry) error
	saved  []*ApprovalAuditEntry
}

func (m *mockAuditStore) SaveApprovalAudit(entry *ApprovalAuditEntry) error {
	m.saved = append(m.saved, entry)
	if m.saveFn != nil {
		return m.saveFn(entry)
	}
	return nil
}

// TestAuditLogging_Approved verifies audit entry for approved decisions.
func TestAuditLogging_Approved(t *testing.T) {
	mockStore := &mockAuditStore{}
	queue := NewApprovalQueue(1*time.Second, nil)
	queue.SetAuditStore(mockStore)

	// Start a request in background
	done := make(chan struct{})
	go func() {
		queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "req-1",
			Command:   "test cmd",
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
		})
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Approve the request
	pending := queue.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	queue.DecideWithDevice(pending[0].RequestID, "approve", nil, "device-abc")

	<-done

	if len(mockStore.saved) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(mockStore.saved))
	}

	entry := mockStore.saved[0]
	if entry.Decision != "approved" {
		t.Errorf("Decision = %q, want approved", entry.Decision)
	}
	if entry.Source != "mobile" {
		t.Errorf("Source = %q, want mobile", entry.Source)
	}
	if entry.DeviceID != "device-abc" {
		t.Errorf("DeviceID = %q, want device-abc", entry.DeviceID)
	}
}

// TestAuditLogging_Denied verifies audit entry for denied decisions.
func TestAuditLogging_Denied(t *testing.T) {
	mockStore := &mockAuditStore{}
	queue := NewApprovalQueue(1*time.Second, nil)
	queue.SetAuditStore(mockStore)

	// Start a request in background
	done := make(chan struct{})
	go func() {
		queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "req-1",
			Command:   "test cmd",
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
		})
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Deny the request
	pending := queue.GetPending()
	queue.DecideWithDevice(pending[0].RequestID, "deny", nil, "device-xyz")

	<-done

	if len(mockStore.saved) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(mockStore.saved))
	}

	entry := mockStore.saved[0]
	if entry.Decision != "denied" {
		t.Errorf("Decision = %q, want denied", entry.Decision)
	}
	if entry.Source != "mobile" {
		t.Errorf("Source = %q, want mobile", entry.Source)
	}
}

// TestAuditLogging_Timeout verifies audit entry for timeout decisions.
func TestAuditLogging_Timeout(t *testing.T) {
	mockStore := &mockAuditStore{}
	queue := NewApprovalQueue(100*time.Millisecond, nil) // Short timeout
	queue.SetAuditStore(mockStore)

	// Queue a request and let it timeout
	_, err := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "req-1",
		Command:   "test cmd",
		Cwd:       "/",
		Repo:      "/",
		Rationale: "test",
	})

	if err == nil {
		t.Error("expected timeout error")
	}

	if len(mockStore.saved) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(mockStore.saved))
	}

	entry := mockStore.saved[0]
	if entry.Decision != "denied" {
		t.Errorf("Decision = %q, want denied", entry.Decision)
	}
	if entry.Source != "timeout" {
		t.Errorf("Source = %q, want timeout", entry.Source)
	}
	if entry.DeviceID != "" {
		t.Errorf("DeviceID = %q, want empty", entry.DeviceID)
	}
}

// TestDecideWithDevice_AddsTemporaryAllow verifies temporary allow rule is added on approve with expiry.
func TestDecideWithDevice_AddsTemporaryAllow(t *testing.T) {
	queue := NewApprovalQueue(1*time.Second, nil)

	// Start a request in background
	done := make(chan struct{})
	go func() {
		queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "req-1",
			Command:   "go test ./...",
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
		})
		close(done)
	}()

	// Wait for request to be queued
	time.Sleep(50 * time.Millisecond)

	// Approve with temporary allow
	pending := queue.GetPending()
	tempUntil := time.Now().Add(15 * time.Minute)
	queue.DecideWithDevice(pending[0].RequestID, "approve", &tempUntil, "device-1")

	<-done

	// Verify rule was added
	rules := queue.GetTemporaryAllowRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Command != "go test ./..." {
		t.Errorf("Command = %q, want 'go test ./...'", rules[0].Command)
	}
}

// TestAuditLogging_NoStore verifies no panic when audit store is nil.
func TestAuditLogging_NoStore(t *testing.T) {
	queue := NewApprovalQueue(100*time.Millisecond, nil)
	// No audit store set

	// Should not panic
	_, err := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "req-1",
		Command:   "test cmd",
		Cwd:       "/",
		Repo:      "/",
		Rationale: "test",
	})

	if err == nil {
		t.Error("expected timeout error")
	}
}

// -----------------------------------------------------------------------------
// Integration Tests with Real Storage
// -----------------------------------------------------------------------------

// TestIntegrationAuditLogAfterApproval verifies that audit log entries are
// persisted to SQLite after an approval flow completes.
func TestIntegrationAuditLogAfterApproval(t *testing.T) {
	// Create a real SQLite store (in-memory for testing)
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create the approval queue with audit logging wired up
	queue := NewApprovalQueue(5*time.Second, nil)
	adapter := NewAuditStoreAdapter(store)
	queue.SetAuditStore(adapter)

	// Queue an approval request in a goroutine
	var response ApprovalResponse
	var queueErr error
	done := make(chan struct{})

	go func() {
		response, queueErr = queue.Queue(context.Background(), ApprovalRequest{
			RequestID: "audit-test-req",
			Command:   "rm -rf /important",
			Cwd:       "/home/user",
			Repo:      "/home/user/project",
			Rationale: "Cleanup old files",
		})
		close(done)
	}()

	// Wait for the request to be queued
	time.Sleep(50 * time.Millisecond)

	// Approve the request with a temporary allow
	tempAllowUntil := time.Now().Add(15 * time.Minute)
	err = queue.DecideWithDevice("audit-test-req", "approve", &tempAllowUntil, "device-123")
	if err != nil {
		t.Fatalf("DecideWithDevice failed: %v", err)
	}

	// Wait for Queue to return
	<-done

	if queueErr != nil {
		t.Fatalf("Queue failed: %v", queueErr)
	}
	if response.Decision != "approve" {
		t.Errorf("Decision = %q, want %q", response.Decision, "approve")
	}

	// Verify audit log entry was created
	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("ListApprovalAudit returned %d entries, want 1", len(entries))
	}

	entry := entries[0]
	if entry.RequestID != "audit-test-req" {
		t.Errorf("RequestID = %q, want %q", entry.RequestID, "audit-test-req")
	}
	if entry.Command != "rm -rf /important" {
		t.Errorf("Command = %q, want %q", entry.Command, "rm -rf /important")
	}
	if entry.Cwd != "/home/user" {
		t.Errorf("Cwd = %q, want %q", entry.Cwd, "/home/user")
	}
	if entry.Repo != "/home/user/project" {
		t.Errorf("Repo = %q, want %q", entry.Repo, "/home/user/project")
	}
	if entry.Rationale != "Cleanup old files" {
		t.Errorf("Rationale = %q, want %q", entry.Rationale, "Cleanup old files")
	}
	if entry.Decision != "approved" {
		t.Errorf("Decision = %q, want %q", entry.Decision, "approved")
	}
	if entry.DeviceID != "device-123" {
		t.Errorf("DeviceID = %q, want %q", entry.DeviceID, "device-123")
	}
	if entry.Source != "mobile" {
		t.Errorf("Source = %q, want %q", entry.Source, "mobile")
	}
	if entry.ExpiresAt == nil {
		t.Error("ExpiresAt should not be nil for temporary allow")
	}
}

// TestIntegrationAuditLogAfterTimeout verifies that audit log entries are
// persisted when an approval request times out.
func TestIntegrationAuditLogAfterTimeout(t *testing.T) {
	// Create a real SQLite store (in-memory for testing)
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create queue with short timeout
	queue := NewApprovalQueue(100*time.Millisecond, nil)
	adapter := NewAuditStoreAdapter(store)
	queue.SetAuditStore(adapter)

	// Queue a request that will timeout
	_, queueErr := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "timeout-test-req",
		Command:   "dangerous command",
		Cwd:       "/",
		Repo:      "/repo",
		Rationale: "Test timeout",
	})

	// Should get timeout error
	if queueErr == nil {
		t.Fatal("expected timeout error")
	}
	if !apperrors.IsCode(queueErr, "approval.timeout") {
		t.Errorf("error code = %v, want approval.timeout", queueErr)
	}

	// Verify audit log entry was created for timeout
	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("ListApprovalAudit returned %d entries, want 1", len(entries))
	}

	entry := entries[0]
	if entry.RequestID != "timeout-test-req" {
		t.Errorf("RequestID = %q, want %q", entry.RequestID, "timeout-test-req")
	}
	if entry.Decision != "denied" {
		t.Errorf("Decision = %q, want %q", entry.Decision, "denied")
	}
	if entry.Source != "timeout" {
		t.Errorf("Source = %q, want %q", entry.Source, "timeout")
	}
	if entry.DeviceID != "" {
		t.Errorf("DeviceID = %q, want empty for timeout", entry.DeviceID)
	}
}

// TestIntegrationAuditLogAfterAutoApprove verifies that audit log entries are
// persisted when a temporary allow rule auto-approves a request.
func TestIntegrationAuditLogAfterAutoApprove(t *testing.T) {
	// Create a real SQLite store (in-memory for testing)
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create queue with audit logging
	queue := NewApprovalQueue(5*time.Second, nil)
	adapter := NewAuditStoreAdapter(store)
	queue.SetAuditStore(adapter)

	// Add a temporary allow rule
	expiresAt := time.Now().Add(15 * time.Minute)
	queue.AddTemporaryAllow("allowed command", expiresAt, "original-device")

	// Queue a request that matches the rule - should auto-approve
	response, queueErr := queue.Queue(context.Background(), ApprovalRequest{
		RequestID: "auto-approve-req",
		Command:   "allowed command",
		Cwd:       "/home",
		Repo:      "/home/repo",
		Rationale: "Test auto-approve",
	})

	if queueErr != nil {
		t.Fatalf("Queue failed: %v", queueErr)
	}
	if response.Decision != "approve" {
		t.Errorf("Decision = %q, want %q", response.Decision, "approve")
	}

	// Verify audit log entry was created for auto-approval
	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("ListApprovalAudit returned %d entries, want 1", len(entries))
	}

	entry := entries[0]
	if entry.RequestID != "auto-approve-req" {
		t.Errorf("RequestID = %q, want %q", entry.RequestID, "auto-approve-req")
	}
	if entry.Command != "allowed command" {
		t.Errorf("Command = %q, want %q", entry.Command, "allowed command")
	}
	if entry.Decision != "approved" {
		t.Errorf("Decision = %q, want %q", entry.Decision, "approved")
	}
	if entry.Source != "rule" {
		t.Errorf("Source = %q, want %q", entry.Source, "rule")
	}
	if entry.DeviceID != "original-device" {
		t.Errorf("DeviceID = %q, want %q", entry.DeviceID, "original-device")
	}
	if entry.ExpiresAt == nil {
		t.Error("ExpiresAt should not be nil for auto-approve by rule")
	}
}
