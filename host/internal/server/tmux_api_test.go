// Package server provides WebSocket server functionality for the pseudocoder host.
// This file contains tests for the tmux API endpoints (Unit 12.9).
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/errors"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/tmux"
)

// mockTmuxManager is a mock implementation of TmuxManager for testing.
// It allows tests to run without requiring tmux to be installed.
type mockTmuxManager struct {
	sessions     []tmux.TmuxSessionInfo
	listErr      error
	attachErr    error
	killErr      error
	attachResult *pty.Session
}

func (m *mockTmuxManager) ListSessions() ([]tmux.TmuxSessionInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.sessions, nil
}

func (m *mockTmuxManager) AttachToTmux(name string, sessionConfig pty.SessionConfig) (*pty.Session, error) {
	if m.attachErr != nil {
		return nil, m.attachErr
	}
	return m.attachResult, nil
}

func (m *mockTmuxManager) KillSession(name string) error {
	return m.killErr
}

// TestTmuxAPIHandler_List tests the GET /api/tmux/list endpoint.
func TestTmuxAPIHandler_List(t *testing.T) {
	t.Run("returns empty list when no tmux sessions", func(t *testing.T) {
		// Create handler with mock tmux manager (no sessions)
		mockMgr := &mockTmuxManager{sessions: []tmux.TmuxSessionInfo{}}
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(mockMgr, sessionMgr, TmuxAPIConfig{
			HistoryLines: 1000,
		})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/list", nil)
		req.RemoteAddr = "127.0.0.1:12345" // Loopback address

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
		}

		var result TmuxListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		if len(result.Sessions) != 0 {
			t.Errorf("expected 0 sessions, got %d", len(result.Sessions))
		}
	})

	t.Run("returns sessions from mock", func(t *testing.T) {
		// Create handler with mock tmux manager that has sessions
		mockMgr := &mockTmuxManager{
			sessions: []tmux.TmuxSessionInfo{
				{Name: "dev", Windows: 3, Attached: true, CreatedAt: time.Now()},
				{Name: "main", Windows: 1, Attached: false, CreatedAt: time.Now()},
			},
		}
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(mockMgr, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/list", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
		}

		var result TmuxListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		if len(result.Sessions) != 2 {
			t.Errorf("expected 2 sessions, got %d", len(result.Sessions))
		}
	})

	t.Run("rejects non-loopback requests", func(t *testing.T) {
		mockMgr := &mockTmuxManager{}
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(mockMgr, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/list", nil)
		req.RemoteAddr = "192.168.1.1:12345" // Non-loopback

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("expected status 403, got %d", rr.Code)
		}
	})

	t.Run("rejects non-GET method", func(t *testing.T) {
		mockMgr := &mockTmuxManager{}
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(mockMgr, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/list", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rr.Code)
		}
	})

	t.Run("returns 503 when tmux manager not configured", func(t *testing.T) {
		handler := NewTmuxAPIHandler(nil, nil, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/list", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rr.Code)
		}
	})
}

// TestTmuxAPIHandler_Attach tests the POST /api/tmux/attach endpoint.
func TestTmuxAPIHandler_Attach(t *testing.T) {
	t.Run("rejects missing tmux_session", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		reqBody := `{}`
		req := httptest.NewRequest(http.MethodPost, "/api/tmux/attach", bytes.NewBufferString(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
		}

		if !strings.Contains(rr.Body.String(), "tmux_session is required") {
			t.Errorf("expected error about missing tmux_session, got: %s", rr.Body.String())
		}
	})

	t.Run("rejects non-POST method", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/attach", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rr.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/attach", bytes.NewBufferString("not json"))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", rr.Code)
		}
	})

	t.Run("returns 503 when managers not configured", func(t *testing.T) {
		handler := NewTmuxAPIHandler(nil, nil, TmuxAPIConfig{})

		reqBody := `{"tmux_session":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/api/tmux/attach", bytes.NewBufferString(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rr.Code)
		}
	})

	t.Run("returns 404 for non-existent tmux session", func(t *testing.T) {
		mockMgr := &mockTmuxManager{
			attachErr: errors.TmuxSessionNotFound("nonexistent_session_12345"),
		}
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(mockMgr, sessionMgr, TmuxAPIConfig{})

		reqBody := `{"tmux_session":"nonexistent_session_12345"}`
		req := httptest.NewRequest(http.MethodPost, "/api/tmux/attach", bytes.NewBufferString(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("rejects session name too long", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		// Name exceeds 100 character limit
		longName := strings.Repeat("a", 101)
		reqBody := `{"tmux_session":"test","name":"` + longName + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/tmux/attach", bytes.NewBufferString(reqBody))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
		}

		if !strings.Contains(rr.Body.String(), "too long") {
			t.Errorf("expected error about name too long, got: %s", rr.Body.String())
		}
	})
}

// TestTmuxAPIHandler_Detach tests the POST /api/tmux/{id}/detach endpoint.
func TestTmuxAPIHandler_Detach(t *testing.T) {
	t.Run("rejects non-POST method", func(t *testing.T) {
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, pty.NewSessionManager(), TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodGet, "/api/tmux/some-id/detach", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rr.Code)
		}
	})

	t.Run("returns 404 for non-existent session", func(t *testing.T) {
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, pty.NewSessionManager(), TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/nonexistent-id/detach", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("returns 503 when session manager not configured", func(t *testing.T) {
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, nil, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/some-id/detach", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rr.Code)
		}
	})

	t.Run("returns 400 for non-tmux session", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()

		// Create a regular (non-tmux) session
		session, err := sessionMgr.Create(pty.SessionConfig{
			Name: "test-session",
		})
		if err != nil {
			t.Fatalf("failed to create session: %v", err)
		}
		// Note: session.TmuxSession is empty, so this is not a tmux session

		// Start a simple command so the session is valid
		if err := session.Start("echo", "test"); err != nil {
			t.Fatalf("failed to start session: %v", err)
		}
		defer sessionMgr.Close(session.ID)

		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/"+session.ID+"/detach", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
		}

		if !strings.Contains(rr.Body.String(), "not a tmux session") {
			t.Errorf("expected error about not being tmux session, got: %s", rr.Body.String())
		}
	})

	t.Run("rejects invalid JSON body", func(t *testing.T) {
		sessionMgr := pty.NewSessionManager()
		handler := NewTmuxAPIHandler(&mockTmuxManager{}, sessionMgr, TmuxAPIConfig{})

		req := httptest.NewRequest(http.MethodPost, "/api/tmux/some-id/detach", bytes.NewBufferString("not json"))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = 8 // Set content length to trigger body parsing

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", rr.Code)
		}
	})
}

// TestTmuxAPIHandler_Routing tests request routing to correct handlers.
func TestTmuxAPIHandler_Routing(t *testing.T) {
	handler := NewTmuxAPIHandler(&mockTmuxManager{}, pty.NewSessionManager(), TmuxAPIConfig{})

	tests := []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/api/tmux/list", http.StatusOK},
		{http.MethodPost, "/api/tmux/attach", http.StatusBadRequest},       // Missing body
		{http.MethodPost, "/api/tmux/test-id/detach", http.StatusNotFound}, // Session not found
		{http.MethodGet, "/api/tmux/unknown", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Errorf("expected status %d, got %d: %s", tc.status, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestTmuxSessionItem_JSON tests JSON serialization of TmuxSessionItem.
func TestTmuxSessionItem_JSON(t *testing.T) {
	item := TmuxSessionItem{
		Name:      "dev",
		Windows:   3,
		Attached:  true,
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var result TmuxSessionItem
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if result.Name != "dev" {
		t.Errorf("expected name 'dev', got %s", result.Name)
	}
	if result.Windows != 3 {
		t.Errorf("expected 3 windows, got %d", result.Windows)
	}
	if !result.Attached {
		t.Error("expected attached to be true")
	}
}

// TestTmuxAttachRequest_JSON tests JSON deserialization of TmuxAttachRequest.
func TestTmuxAttachRequest_JSON(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		expect TmuxAttachRequest
	}{
		{
			name: "with all fields",
			json: `{"tmux_session":"main","name":"My Session"}`,
			expect: TmuxAttachRequest{
				TmuxSession: "main",
				Name:        "My Session",
			},
		},
		{
			name: "tmux_session only",
			json: `{"tmux_session":"dev"}`,
			expect: TmuxAttachRequest{
				TmuxSession: "dev",
				Name:        "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req TmuxAttachRequest
			if err := json.Unmarshal([]byte(tc.json), &req); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if req.TmuxSession != tc.expect.TmuxSession {
				t.Errorf("expected tmux_session %s, got %s", tc.expect.TmuxSession, req.TmuxSession)
			}
			if req.Name != tc.expect.Name {
				t.Errorf("expected name %s, got %s", tc.expect.Name, req.Name)
			}
		})
	}
}

// TestTmuxDetachRequest_JSON tests JSON deserialization of TmuxDetachRequest.
func TestTmuxDetachRequest_JSON(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		expect bool
	}{
		{"kill true", `{"kill":true}`, true},
		{"kill false", `{"kill":false}`, false},
		{"empty", `{}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req TmuxDetachRequest
			if err := json.Unmarshal([]byte(tc.json), &req); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if req.Kill != tc.expect {
				t.Errorf("expected kill %v, got %v", tc.expect, req.Kill)
			}
		})
	}
}
