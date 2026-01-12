// Package server provides WebSocket server functionality for the pseudocoder host.
// This file contains tests for the session API handlers (Unit 9.5).
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pseudocoder/host/internal/pty"
)

// TestSessionAPIHandler_New tests the POST /api/session/new endpoint.
func TestSessionAPIHandler_New(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "echo",
		HistoryLines:   100,
	})

	// Create a request with session name
	reqBody := `{"name": "test-session", "command": "echo", "args": ["hello"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345" // Loopback address
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SessionNewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.ID == "" {
		t.Error("expected non-empty session ID")
	}
	if resp.Name != "test-session" {
		t.Errorf("expected name 'test-session', got %q", resp.Name)
	}
	if resp.Command != "echo" {
		t.Errorf("expected command 'echo', got %q", resp.Command)
	}

	// Verify session was created in manager
	if mgr.Count() != 1 {
		t.Errorf("expected 1 session in manager, got %d", mgr.Count())
	}

	// Cleanup
	mgr.CloseAll()
}

// TestSessionAPIHandler_New_DefaultCommand tests that default command is used when not specified.
func TestSessionAPIHandler_New_DefaultCommand(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "/bin/sh",
		HistoryLines:   100,
	})

	// Create a request without command
	reqBody := `{"name": "default-cmd-test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SessionNewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Command != "/bin/sh" {
		t.Errorf("expected default command '/bin/sh', got %q", resp.Command)
	}

	mgr.CloseAll()
}

// TestSessionAPIHandler_List tests the GET /api/session/list endpoint.
func TestSessionAPIHandler_List(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "sleep",
		HistoryLines:   100,
	})

	// Create a couple of sessions
	s1, _ := mgr.Create(pty.SessionConfig{Name: "session-one"})
	s1.Start("sleep", "10")
	s2, _ := mgr.Create(pty.SessionConfig{Name: "session-two"})
	s2.Start("sleep", "10")

	req := httptest.NewRequest(http.MethodGet, "/api/session/list", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var sessions []SessionListItem
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}

	// Check that sessions have the expected names
	names := make(map[string]bool)
	for _, s := range sessions {
		names[s.Name] = true
	}
	if !names["session-one"] || !names["session-two"] {
		t.Errorf("expected sessions with names 'session-one' and 'session-two', got %v", names)
	}

	mgr.CloseAll()
}

// TestSessionAPIHandler_List_Empty tests list with no sessions.
func TestSessionAPIHandler_List_Empty(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	req := httptest.NewRequest(http.MethodGet, "/api/session/list", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var sessions []SessionListItem
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

// TestSessionAPIHandler_Kill tests the POST /api/session/{id}/kill endpoint.
func TestSessionAPIHandler_Kill(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	// Create a session
	session, _ := mgr.Create(pty.SessionConfig{Name: "to-kill"})
	session.Start("sleep", "60")

	req := httptest.NewRequest(http.MethodPost, "/api/session/"+session.ID+"/kill", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SessionKillResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !resp.Killed {
		t.Error("expected killed=true")
	}
	if resp.ID != session.ID {
		t.Errorf("expected ID %q, got %q", session.ID, resp.ID)
	}

	// Verify session was removed
	if mgr.Count() != 0 {
		t.Errorf("expected 0 sessions after kill, got %d", mgr.Count())
	}
}

// TestSessionAPIHandler_Kill_NotFound tests kill with invalid session ID.
func TestSessionAPIHandler_Kill_NotFound(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/session/nonexistent/kill", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rr.Code)
	}
}

// TestSessionAPIHandler_Rename tests the POST /api/session/{id}/rename endpoint.
func TestSessionAPIHandler_Rename(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	// Create a session
	session, _ := mgr.Create(pty.SessionConfig{Name: "old-name"})
	session.Start("sleep", "10")

	reqBody := `{"name": "new-name"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/"+session.ID+"/rename", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SessionRenameResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Name != "new-name" {
		t.Errorf("expected name 'new-name', got %q", resp.Name)
	}

	// Verify the name was actually changed
	if session.GetName() != "new-name" {
		t.Errorf("session name not updated, got %q", session.GetName())
	}

	mgr.CloseAll()
}

// TestSessionAPIHandler_Rename_NotFound tests rename with invalid session ID.
func TestSessionAPIHandler_Rename_NotFound(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	reqBody := `{"name": "new-name"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/nonexistent/rename", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rr.Code)
	}
}

// TestSessionAPIHandler_LoopbackOnly tests that non-loopback requests are rejected.
func TestSessionAPIHandler_LoopbackOnly(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/session/new"},
		{http.MethodGet, "/api/session/list"},
		{http.MethodPost, "/api/session/test-id/kill"},
		{http.MethodPost, "/api/session/test-id/rename"},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		req.RemoteAddr = "192.168.1.100:12345" // Non-loopback address

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected status 403 for non-loopback, got %d", ep.method, ep.path, rr.Code)
		}
	}
}

// TestSessionAPIHandler_MethodNotAllowed tests that wrong methods are rejected.
func TestSessionAPIHandler_MethodNotAllowed(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/session/new"},      // Should be POST
		{http.MethodPost, "/api/session/list"},    // Should be GET
		{http.MethodGet, "/api/session/id/kill"},  // Should be POST
		{http.MethodGet, "/api/session/id/rename"}, // Should be POST
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected status 405, got %d", tt.method, tt.path, rr.Code)
		}
	}
}

// =============================================================================
// Session Name Validation Tests (Unit 9.5 - addressing review gaps)
// =============================================================================

// TestSessionAPIHandler_New_NameTooLong tests that long session names are rejected.
func TestSessionAPIHandler_New_NameTooLong(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "echo",
	})

	// Create a name that exceeds MaxSessionNameLength (100)
	longName := string(make([]byte, MaxSessionNameLength+1))
	for i := range longName {
		longName = longName[:i] + "a" + longName[i+1:]
	}

	reqBody := `{"name": "` + longName + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for long name, got %d: %s", rr.Code, rr.Body.String())
	}

	var errResp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}

	if errResp["error"] != "Session name too long (max 100 characters)" {
		t.Errorf("unexpected error message: %q", errResp["error"])
	}
}

// TestSessionAPIHandler_New_ControlCharacters tests that control characters in names are rejected.
func TestSessionAPIHandler_New_ControlCharacters(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "echo",
	})

	// Test various control characters
	controlChars := []string{
		"test\x00name", // null
		"test\nnewline",
		"test\ttab",
		"test\rcarriage",
	}

	for _, name := range controlChars {
		reqBody, _ := json.Marshal(map[string]string{"name": name})
		req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBuffer(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400 for name with control char, got %d for %q", rr.Code, name)
		}
	}
}

// TestSessionAPIHandler_Rename_NameTooLong tests that long names are rejected in rename.
func TestSessionAPIHandler_Rename_NameTooLong(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	// Create a session
	session, _ := mgr.Create(pty.SessionConfig{Name: "original"})
	session.Start("sleep", "10")

	// Try to rename with a too-long name
	longName := string(make([]byte, MaxSessionNameLength+1))
	for i := range longName {
		longName = longName[:i] + "x" + longName[i+1:]
	}

	reqBody := `{"name": "` + longName + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/"+session.ID+"/rename", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for long name, got %d", rr.Code)
	}

	// Verify original name is preserved
	if session.GetName() != "original" {
		t.Errorf("name should not have changed, got %q", session.GetName())
	}

	mgr.CloseAll()
}

// TestSessionAPIHandler_Rename_ControlCharacters tests that control characters are rejected in rename.
func TestSessionAPIHandler_Rename_ControlCharacters(t *testing.T) {
	mgr := pty.NewSessionManager()
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{})

	session, _ := mgr.Create(pty.SessionConfig{Name: "original"})
	session.Start("sleep", "10")

	reqBody, _ := json.Marshal(map[string]string{"name": "test\x00name"})
	req := httptest.NewRequest(http.MethodPost, "/api/session/"+session.ID+"/rename", bytes.NewBuffer(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rr.Code)
	}

	mgr.CloseAll()
}

// =============================================================================
// Session Limit Tests (Unit 9.5 - addressing review gaps)
// =============================================================================

// TestSessionAPIHandler_New_MaxSessionsReached tests that creating sessions
// beyond the limit returns an appropriate error.
func TestSessionAPIHandler_New_MaxSessionsReached(t *testing.T) {
	// Create a manager with a small limit for testing
	mgr := pty.NewSessionManagerWithLimit(2)
	handler := NewSessionAPIHandler(mgr, SessionAPIConfig{
		DefaultCommand: "echo",
	})

	// Create 2 sessions (the limit)
	for i := 0; i < 2; i++ {
		reqBody := `{"command": "echo"}`
		req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBufferString(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("session %d: expected status 200, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	// Third session should fail
	reqBody := `{"command": "echo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/session/new", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 when limit reached, got %d: %s", rr.Code, rr.Body.String())
	}

	var errResp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}

	expectedErr := "Maximum number of sessions reached (limit: 2)"
	if errResp["error"] != expectedErr {
		t.Errorf("expected error %q, got %q", expectedErr, errResp["error"])
	}

	mgr.CloseAll()
}

// TestValidateSessionName tests the validateSessionName function directly.
func TestValidateSessionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		errMatch string
	}{
		{"empty is valid", "", false, ""},
		{"normal name", "my-session", false, ""},
		{"with spaces", "my session name", false, ""},
		{"with numbers", "session123", false, ""},
		{"unicode allowed", "session-\u00e9\u00e0\u00fc", false, ""},
		{"exactly 100 chars", string(make([]byte, 100)), false, ""},
		{"101 chars too long", string(make([]byte, 101)), true, "too long"},
		{"null char", "test\x00name", true, "invalid characters"},
		{"newline", "test\nname", true, "invalid characters"},
		{"tab", "test\tname", true, "invalid characters"},
		{"carriage return", "test\rname", true, "invalid characters"},
	}

	// Fix the 100 and 101 char test cases to use valid chars
	for i := range tests {
		if tests[i].name == "exactly 100 chars" || tests[i].name == "101 chars too long" {
			chars := make([]byte, len(tests[i].input))
			for j := range chars {
				chars[j] = 'a'
			}
			tests[i].input = string(chars)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionName(tt.input)
			if tt.wantErr {
				if err == "" {
					t.Error("expected error but got none")
				} else if tt.errMatch != "" && !bytes.Contains([]byte(err), []byte(tt.errMatch)) {
					t.Errorf("error %q should contain %q", err, tt.errMatch)
				}
			} else if err != "" {
				t.Errorf("unexpected error: %s", err)
			}
		})
	}
}
