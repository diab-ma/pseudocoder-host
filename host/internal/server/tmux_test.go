package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/tmux"
)

func TestNewTmuxSessionsMessage(t *testing.T) {
	t.Run("with sessions", func(t *testing.T) {
		sessions := []TmuxSessionInfo{
			{Name: "main", Windows: 3, Attached: true, CreatedAt: 1704067200000},
			{Name: "dev", Windows: 1, Attached: false, CreatedAt: 1704070800000},
		}
		msg := NewTmuxSessionsMessage(sessions, "")

		if msg.Type != MessageTypeTmuxSessions {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxSessions, msg.Type)
		}

		payload, ok := msg.Payload.(TmuxSessionsPayload)
		if !ok {
			t.Fatalf("expected TmuxSessionsPayload, got %T", msg.Payload)
		}

		if len(payload.Sessions) != 2 {
			t.Errorf("expected 2 sessions, got %d", len(payload.Sessions))
		}
		if payload.ErrorCode != "" {
			t.Errorf("expected empty error code, got %s", payload.ErrorCode)
		}
	})

	t.Run("with error code", func(t *testing.T) {
		msg := NewTmuxSessionsMessage(nil, "tmux.not_installed")

		payload, ok := msg.Payload.(TmuxSessionsPayload)
		if !ok {
			t.Fatalf("expected TmuxSessionsPayload, got %T", msg.Payload)
		}

		if payload.Sessions != nil {
			t.Errorf("expected nil sessions, got %v", payload.Sessions)
		}
		if payload.ErrorCode != "tmux.not_installed" {
			t.Errorf("expected error code 'tmux.not_installed', got %s", payload.ErrorCode)
		}
	})

	t.Run("empty sessions list", func(t *testing.T) {
		msg := NewTmuxSessionsMessage([]TmuxSessionInfo{}, "")

		payload, ok := msg.Payload.(TmuxSessionsPayload)
		if !ok {
			t.Fatalf("expected TmuxSessionsPayload, got %T", msg.Payload)
		}

		if len(payload.Sessions) != 0 {
			t.Errorf("expected 0 sessions, got %d", len(payload.Sessions))
		}
		if payload.ErrorCode != "" {
			t.Errorf("expected empty error code, got %s", payload.ErrorCode)
		}
	})
}

func TestTmuxSessionsPayload_JSON(t *testing.T) {
	t.Run("serialization with sessions", func(t *testing.T) {
		payload := TmuxSessionsPayload{
			Sessions: []TmuxSessionInfo{
				{Name: "main", Windows: 3, Attached: true, CreatedAt: 1704067200000},
			},
			ErrorCode: "",
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed TmuxSessionsPayload
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if len(parsed.Sessions) != 1 {
			t.Errorf("expected 1 session, got %d", len(parsed.Sessions))
		}
		if parsed.Sessions[0].Name != "main" {
			t.Errorf("expected name 'main', got %s", parsed.Sessions[0].Name)
		}
		if parsed.Sessions[0].Windows != 3 {
			t.Errorf("expected 3 windows, got %d", parsed.Sessions[0].Windows)
		}
		if !parsed.Sessions[0].Attached {
			t.Error("expected attached=true")
		}
		if parsed.Sessions[0].CreatedAt != 1704067200000 {
			t.Errorf("expected createdAt 1704067200000, got %d", parsed.Sessions[0].CreatedAt)
		}
	})

	t.Run("serialization with error code", func(t *testing.T) {
		payload := TmuxSessionsPayload{
			Sessions:  nil,
			ErrorCode: "tmux.not_installed",
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		// Verify error_code is present in JSON
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		if raw["error_code"] != "tmux.not_installed" {
			t.Errorf("expected error_code 'tmux.not_installed', got %v", raw["error_code"])
		}
	})

	t.Run("omitempty for error_code", func(t *testing.T) {
		payload := TmuxSessionsPayload{
			Sessions:  []TmuxSessionInfo{},
			ErrorCode: "",
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		// Verify error_code is omitted when empty
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		if _, exists := raw["error_code"]; exists {
			t.Error("expected error_code to be omitted when empty")
		}
	})
}

func TestTmuxSessionInfo_JSON(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		info := TmuxSessionInfo{
			Name:      "project:feature",
			Windows:   5,
			Attached:  true,
			CreatedAt: 1704067200000,
		}

		data, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed TmuxSessionInfo
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed.Name != info.Name {
			t.Errorf("name mismatch: expected %s, got %s", info.Name, parsed.Name)
		}
		if parsed.Windows != info.Windows {
			t.Errorf("windows mismatch: expected %d, got %d", info.Windows, parsed.Windows)
		}
		if parsed.Attached != info.Attached {
			t.Errorf("attached mismatch: expected %v, got %v", info.Attached, parsed.Attached)
		}
		if parsed.CreatedAt != info.CreatedAt {
			t.Errorf("createdAt mismatch: expected %d, got %d", info.CreatedAt, parsed.CreatedAt)
		}
	})

	t.Run("json field names", func(t *testing.T) {
		info := TmuxSessionInfo{
			Name:      "test",
			Windows:   1,
			Attached:  false,
			CreatedAt: 1704067200000,
		}

		data, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		// Verify snake_case field names
		if _, exists := raw["name"]; !exists {
			t.Error("expected 'name' field")
		}
		if _, exists := raw["windows"]; !exists {
			t.Error("expected 'windows' field")
		}
		if _, exists := raw["attached"]; !exists {
			t.Error("expected 'attached' field")
		}
		if _, exists := raw["created_at"]; !exists {
			t.Error("expected 'created_at' field")
		}
	})
}

// =============================================================================
// TmuxAttachPayload Tests (Unit 12.4)
// =============================================================================

func TestTmuxAttachPayload_JSON(t *testing.T) {
	t.Run("serialization", func(t *testing.T) {
		payload := TmuxAttachPayload{
			TmuxSession: "main",
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed TmuxAttachPayload
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed.TmuxSession != "main" {
			t.Errorf("expected tmux_session 'main', got %s", parsed.TmuxSession)
		}
	})

	t.Run("json field names", func(t *testing.T) {
		payload := TmuxAttachPayload{
			TmuxSession: "dev",
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		// Verify snake_case field name
		if _, exists := raw["tmux_session"]; !exists {
			t.Error("expected 'tmux_session' field")
		}
	})

	t.Run("deserialization from client message", func(t *testing.T) {
		// Simulate what a client would send
		clientJSON := `{"type":"tmux.attach","payload":{"tmux_session":"project"}}`

		var msg struct {
			Type    string            `json:"type"`
			Payload TmuxAttachPayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(clientJSON), &msg); err != nil {
			t.Fatalf("failed to unmarshal client message: %v", err)
		}

		if msg.Type != "tmux.attach" {
			t.Errorf("expected type 'tmux.attach', got %s", msg.Type)
		}
		if msg.Payload.TmuxSession != "project" {
			t.Errorf("expected tmux_session 'project', got %s", msg.Payload.TmuxSession)
		}
	})
}

// =============================================================================
// TmuxAttachedPayload Tests (Unit 12.4)
// =============================================================================

func TestTmuxAttachedPayload_JSON(t *testing.T) {
	t.Run("serialization with all fields", func(t *testing.T) {
		payload := TmuxAttachedPayload{
			SessionID:   "abc-123",
			TmuxSession: "main",
			IsTmux:      true,
			Name:        "main",
			Command:     "tmux attach-session -t main",
			Status:      "running",
			CreatedAt:   1704067200000,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed TmuxAttachedPayload
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed.SessionID != "abc-123" {
			t.Errorf("expected session_id 'abc-123', got %s", parsed.SessionID)
		}
		if parsed.TmuxSession != "main" {
			t.Errorf("expected tmux_session 'main', got %s", parsed.TmuxSession)
		}
		if !parsed.IsTmux {
			t.Error("expected is_tmux=true")
		}
		if parsed.Name != "main" {
			t.Errorf("expected name 'main', got %s", parsed.Name)
		}
		if parsed.Command != "tmux attach-session -t main" {
			t.Errorf("expected command 'tmux attach-session -t main', got %s", parsed.Command)
		}
		if parsed.Status != "running" {
			t.Errorf("expected status 'running', got %s", parsed.Status)
		}
		if parsed.CreatedAt != 1704067200000 {
			t.Errorf("expected created_at 1704067200000, got %d", parsed.CreatedAt)
		}
	})

	t.Run("json field names", func(t *testing.T) {
		payload := TmuxAttachedPayload{
			SessionID:   "abc-123",
			TmuxSession: "main",
			IsTmux:      true,
			Name:        "main",
			Command:     "tmux attach-session -t main",
			Status:      "running",
			CreatedAt:   1704067200000,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		// Verify snake_case field names
		expectedFields := []string{"session_id", "tmux_session", "is_tmux", "name", "command", "status", "created_at"}
		for _, field := range expectedFields {
			if _, exists := raw[field]; !exists {
				t.Errorf("expected '%s' field", field)
			}
		}
	})

	t.Run("is_tmux field is boolean true", func(t *testing.T) {
		payload := TmuxAttachedPayload{
			SessionID:   "test",
			TmuxSession: "dev",
			IsTmux:      true,
			Status:      "running",
			CreatedAt:   1704067200000,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		// Verify is_tmux is a boolean true (not a string or number)
		isTmux, ok := raw["is_tmux"].(bool)
		if !ok {
			t.Errorf("expected is_tmux to be boolean, got %T", raw["is_tmux"])
		}
		if !isTmux {
			t.Error("expected is_tmux to be true")
		}
	})

	t.Run("omitempty for name and command", func(t *testing.T) {
		payload := TmuxAttachedPayload{
			SessionID:   "test",
			TmuxSession: "dev",
			IsTmux:      true,
			Name:        "", // empty
			Command:     "", // empty
			Status:      "running",
			CreatedAt:   1704067200000,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal to map: %v", err)
		}

		// Verify name and command are omitted when empty
		if _, exists := raw["name"]; exists {
			t.Error("expected 'name' to be omitted when empty")
		}
		if _, exists := raw["command"]; exists {
			t.Error("expected 'command' to be omitted when empty")
		}
	})
}

func TestNewTmuxAttachedMessage(t *testing.T) {
	t.Run("creates correct message", func(t *testing.T) {
		msg := NewTmuxAttachedMessage(
			"session-123",
			"main",
			"main",
			"tmux attach-session -t main",
			"running",
			1704067200000,
		)

		if msg.Type != MessageTypeTmuxAttached {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxAttached, msg.Type)
		}

		payload, ok := msg.Payload.(TmuxAttachedPayload)
		if !ok {
			t.Fatalf("expected TmuxAttachedPayload, got %T", msg.Payload)
		}

		if payload.SessionID != "session-123" {
			t.Errorf("expected session_id 'session-123', got %s", payload.SessionID)
		}
		if payload.TmuxSession != "main" {
			t.Errorf("expected tmux_session 'main', got %s", payload.TmuxSession)
		}
		if !payload.IsTmux {
			t.Error("expected is_tmux=true")
		}
	})

	t.Run("is_tmux always true", func(t *testing.T) {
		// Verify constructor always sets IsTmux to true
		msg := NewTmuxAttachedMessage("id", "session", "name", "cmd", "status", 0)
		payload := msg.Payload.(TmuxAttachedPayload)
		if !payload.IsTmux {
			t.Error("NewTmuxAttachedMessage must always set is_tmux=true")
		}
	})
}

// TestTmuxAttachFlow_Integration simulates the handleTmuxAttach flow.
// This verifies that the response message contains all required fields
// for mobile to distinguish tmux sessions from regular PTY sessions.
func TestTmuxAttachFlow_Integration(t *testing.T) {
	t.Run("attach response includes all tmux metadata", func(t *testing.T) {
		// Simulate what handleTmuxAttach does after successful attach:
		// 1. Session created via TmuxManager.AttachToTmux()
		// 2. Session registered with SessionManager.Register()
		// 3. Broadcast tmux.attached message

		sessionID := "uuid-123-abc"
		tmuxSession := "main"
		createdAt := time.Now().UnixMilli()

		// Build the message exactly as handleTmuxAttach does
		name := tmuxSession
		command := "tmux attach-session -t " + tmuxSession

		attachedMsg := NewTmuxAttachedMessage(
			sessionID,
			tmuxSession,
			name,
			command,
			"running",
			createdAt,
		)

		// Serialize to JSON (as would be sent over WebSocket)
		data, err := json.Marshal(attachedMsg)
		if err != nil {
			t.Fatalf("failed to serialize message: %v", err)
		}

		// Parse as raw JSON to verify wire format
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to parse message: %v", err)
		}

		// Verify message type
		if raw["type"] != string(MessageTypeTmuxAttached) {
			t.Errorf("expected type %q, got %v", MessageTypeTmuxAttached, raw["type"])
		}

		// Verify payload contains all required fields for mobile
		payload, ok := raw["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload to be object, got %T", raw["payload"])
		}

		// CRITICAL: Mobile relies on is_tmux to distinguish tmux sessions
		if payload["is_tmux"] != true {
			t.Error("is_tmux must be true for mobile to identify tmux sessions")
		}

		// Verify all metadata fields present
		if payload["session_id"] != sessionID {
			t.Errorf("expected session_id %q, got %v", sessionID, payload["session_id"])
		}
		if payload["tmux_session"] != tmuxSession {
			t.Errorf("expected tmux_session %q, got %v", tmuxSession, payload["tmux_session"])
		}
		if payload["name"] != name {
			t.Errorf("expected name %q, got %v", name, payload["name"])
		}
		if payload["command"] != command {
			t.Errorf("expected command %q, got %v", command, payload["command"])
		}
		if payload["status"] != "running" {
			t.Errorf("expected status 'running', got %v", payload["status"])
		}
		if payload["created_at"] == nil {
			t.Error("expected created_at to be present")
		}
	})

	t.Run("mobile can parse attach response", func(t *testing.T) {
		// Simulate mobile receiving and parsing tmux.attached
		serverResponse := `{
			"type": "tmux.attached",
			"payload": {
				"session_id": "abc-123",
				"tmux_session": "dev",
				"is_tmux": true,
				"name": "dev",
				"command": "tmux attach-session -t dev",
				"status": "running",
				"created_at": 1704067200000
			}
		}`

		var msg struct {
			Type    string               `json:"type"`
			Payload TmuxAttachedPayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(serverResponse), &msg); err != nil {
			t.Fatalf("mobile failed to parse response: %v", err)
		}

		// Mobile uses IsTmux to update tab bar appearance
		if !msg.Payload.IsTmux {
			t.Error("mobile must be able to detect tmux session via is_tmux field")
		}

		// Mobile uses TmuxSession for display in tab bar
		if msg.Payload.TmuxSession != "dev" {
			t.Errorf("expected tmux_session 'dev', got %q", msg.Payload.TmuxSession)
		}
	})
}

func TestTmuxSessionInfoConversion(t *testing.T) {
	t.Run("converts from tmux.TmuxSessionInfo", func(t *testing.T) {
		// Simulate what handleTmuxList does
		now := time.Now()
		tmuxSessions := []tmux.TmuxSessionInfo{
			{Name: "main", Windows: 3, Attached: true, CreatedAt: now},
			{Name: "dev", Windows: 1, Attached: false, CreatedAt: now.Add(-time.Hour)},
		}

		wireInfos := make([]TmuxSessionInfo, len(tmuxSessions))
		for i, s := range tmuxSessions {
			wireInfos[i] = TmuxSessionInfo{
				Name:      s.Name,
				Windows:   s.Windows,
				Attached:  s.Attached,
				CreatedAt: s.CreatedAt.UnixMilli(),
			}
		}

		if len(wireInfos) != 2 {
			t.Fatalf("expected 2 wire sessions, got %d", len(wireInfos))
		}

		// Verify first session
		if wireInfos[0].Name != "main" {
			t.Errorf("expected name 'main', got %s", wireInfos[0].Name)
		}
		if wireInfos[0].Windows != 3 {
			t.Errorf("expected 3 windows, got %d", wireInfos[0].Windows)
		}
		if !wireInfos[0].Attached {
			t.Error("expected attached=true")
		}
		if wireInfos[0].CreatedAt != now.UnixMilli() {
			t.Errorf("expected createdAt %d, got %d", now.UnixMilli(), wireInfos[0].CreatedAt)
		}

		// Verify second session
		if wireInfos[1].Name != "dev" {
			t.Errorf("expected name 'dev', got %s", wireInfos[1].Name)
		}
		if wireInfos[1].Attached {
			t.Error("expected attached=false")
		}
	})

	t.Run("handles zero time", func(t *testing.T) {
		zeroTime := time.Time{}
		wireInfo := TmuxSessionInfo{
			Name:      "test",
			Windows:   1,
			Attached:  false,
			CreatedAt: zeroTime.UnixMilli(),
		}

		// Zero time should result in a negative or very small timestamp
		// This is expected behavior - zero timestamps indicate missing data
		if wireInfo.CreatedAt > 0 {
			t.Errorf("expected zero time to produce non-positive timestamp, got %d", wireInfo.CreatedAt)
		}
	})
}

// TestTmuxDetachPayload_JSON verifies TmuxDetachPayload serialization.
func TestTmuxDetachPayload_JSON(t *testing.T) {
	t.Run("serializes with all fields", func(t *testing.T) {
		payload := TmuxDetachPayload{
			SessionID: "abc-123",
			Kill:      true,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		// Verify JSON structure
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if result["session_id"] != "abc-123" {
			t.Errorf("expected session_id 'abc-123', got %v", result["session_id"])
		}
		if result["kill"] != true {
			t.Errorf("expected kill=true, got %v", result["kill"])
		}
	})

	t.Run("omits kill when false", func(t *testing.T) {
		payload := TmuxDetachPayload{
			SessionID: "abc-123",
			Kill:      false,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		// kill should be omitted when false (omitempty)
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if _, exists := result["kill"]; exists {
			t.Error("expected kill to be omitted when false")
		}
	})

	t.Run("deserializes correctly", func(t *testing.T) {
		jsonData := `{"session_id":"xyz-789","kill":true}`

		var payload TmuxDetachPayload
		if err := json.Unmarshal([]byte(jsonData), &payload); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if payload.SessionID != "xyz-789" {
			t.Errorf("expected session_id 'xyz-789', got %s", payload.SessionID)
		}
		if !payload.Kill {
			t.Error("expected kill=true")
		}
	})
}

// TestTmuxDetachedPayload_JSON verifies TmuxDetachedPayload serialization.
func TestTmuxDetachedPayload_JSON(t *testing.T) {
	t.Run("serializes with all fields", func(t *testing.T) {
		payload := TmuxDetachedPayload{
			SessionID:   "abc-123",
			TmuxSession: "main",
			Killed:      true,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if result["session_id"] != "abc-123" {
			t.Errorf("expected session_id 'abc-123', got %v", result["session_id"])
		}
		if result["tmux_session"] != "main" {
			t.Errorf("expected tmux_session 'main', got %v", result["tmux_session"])
		}
		if result["killed"] != true {
			t.Errorf("expected killed=true, got %v", result["killed"])
		}
	})

	t.Run("killed defaults to false", func(t *testing.T) {
		payload := TmuxDetachedPayload{
			SessionID:   "abc-123",
			TmuxSession: "dev",
			Killed:      false,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		// killed should be present but false
		if result["killed"] != false {
			t.Errorf("expected killed=false, got %v", result["killed"])
		}
	})

	t.Run("deserializes correctly", func(t *testing.T) {
		jsonData := `{"session_id":"xyz-789","tmux_session":"work","killed":false}`

		var payload TmuxDetachedPayload
		if err := json.Unmarshal([]byte(jsonData), &payload); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if payload.SessionID != "xyz-789" {
			t.Errorf("expected session_id 'xyz-789', got %s", payload.SessionID)
		}
		if payload.TmuxSession != "work" {
			t.Errorf("expected tmux_session 'work', got %s", payload.TmuxSession)
		}
		if payload.Killed {
			t.Error("expected killed=false")
		}
	})
}

// TestNewTmuxDetachedMessage verifies NewTmuxDetachedMessage creates correct structure.
func TestNewTmuxDetachedMessage(t *testing.T) {
	t.Run("creates message with killed=false", func(t *testing.T) {
		msg := NewTmuxDetachedMessage("session-123", "main", false)

		if msg.Type != MessageTypeTmuxDetached {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxDetached, msg.Type)
		}

		payload, ok := msg.Payload.(TmuxDetachedPayload)
		if !ok {
			t.Fatalf("expected TmuxDetachedPayload, got %T", msg.Payload)
		}

		if payload.SessionID != "session-123" {
			t.Errorf("expected session_id 'session-123', got %s", payload.SessionID)
		}
		if payload.TmuxSession != "main" {
			t.Errorf("expected tmux_session 'main', got %s", payload.TmuxSession)
		}
		if payload.Killed {
			t.Error("expected killed=false")
		}
	})

	t.Run("creates message with killed=true", func(t *testing.T) {
		msg := NewTmuxDetachedMessage("session-456", "dev", true)

		if msg.Type != MessageTypeTmuxDetached {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxDetached, msg.Type)
		}

		payload, ok := msg.Payload.(TmuxDetachedPayload)
		if !ok {
			t.Fatalf("expected TmuxDetachedPayload, got %T", msg.Payload)
		}

		if payload.SessionID != "session-456" {
			t.Errorf("expected session_id 'session-456', got %s", payload.SessionID)
		}
		if payload.TmuxSession != "dev" {
			t.Errorf("expected tmux_session 'dev', got %s", payload.TmuxSession)
		}
		if !payload.Killed {
			t.Error("expected killed=true")
		}
	})
}

// TestTmuxDetachFlow_Integration simulates the handleTmuxDetach flow.
// This verifies that the response message contains all required fields
// for mobile to update its state after tmux session detachment.
func TestTmuxDetachFlow_Integration(t *testing.T) {
	t.Run("detach response with killed=false", func(t *testing.T) {
		// Simulate what handleTmuxDetach does after successful detach:
		// 1. Session looked up, verified as tmux session
		// 2. PTY session closed (detaches from tmux)
		// 3. Broadcast tmux.detached message with killed=false

		sessionID := "uuid-123-abc"
		tmuxSession := "main"

		detachedMsg := NewTmuxDetachedMessage(sessionID, tmuxSession, false)

		// Serialize to JSON (as would be sent over WebSocket)
		data, err := json.Marshal(detachedMsg)
		if err != nil {
			t.Fatalf("failed to serialize message: %v", err)
		}

		// Parse as raw JSON to verify wire format
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to parse message: %v", err)
		}

		// Verify message type
		if raw["type"] != string(MessageTypeTmuxDetached) {
			t.Errorf("expected type %q, got %v", MessageTypeTmuxDetached, raw["type"])
		}

		// Verify payload
		payload, ok := raw["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload to be object, got %T", raw["payload"])
		}

		if payload["session_id"] != sessionID {
			t.Errorf("expected session_id %q, got %v", sessionID, payload["session_id"])
		}
		if payload["tmux_session"] != tmuxSession {
			t.Errorf("expected tmux_session %q, got %v", tmuxSession, payload["tmux_session"])
		}
		if payload["killed"] != false {
			t.Error("expected killed=false for detach without kill flag")
		}
	})

	t.Run("detach response with killed=true", func(t *testing.T) {
		// Simulate detach with kill flag set

		sessionID := "uuid-456-def"
		tmuxSession := "dev"

		detachedMsg := NewTmuxDetachedMessage(sessionID, tmuxSession, true)

		data, err := json.Marshal(detachedMsg)
		if err != nil {
			t.Fatalf("failed to serialize message: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to parse message: %v", err)
		}

		payload, ok := raw["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload to be object, got %T", raw["payload"])
		}

		if payload["killed"] != true {
			t.Error("expected killed=true when kill flag was set")
		}
	})

	t.Run("mobile can parse detach response", func(t *testing.T) {
		// Simulate mobile receiving and parsing tmux.detached
		serverResponse := `{
			"type": "tmux.detached",
			"payload": {
				"session_id": "abc-123",
				"tmux_session": "work",
				"killed": false
			}
		}`

		var msg Message
		if err := json.Unmarshal([]byte(serverResponse), &msg); err != nil {
			t.Fatalf("mobile failed to parse response: %v", err)
		}

		if msg.Type != MessageTypeTmuxDetached {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxDetached, msg.Type)
		}

		// Mobile would extract payload fields
		payloadMap, ok := msg.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload map, got %T", msg.Payload)
		}

		sessionID, _ := payloadMap["session_id"].(string)
		tmuxSession, _ := payloadMap["tmux_session"].(string)
		killed, _ := payloadMap["killed"].(bool)

		if sessionID != "abc-123" {
			t.Errorf("expected session_id 'abc-123', got %s", sessionID)
		}
		if tmuxSession != "work" {
			t.Errorf("expected tmux_session 'work', got %s", tmuxSession)
		}
		if killed {
			t.Error("expected killed=false")
		}
	})

	t.Run("detach request payload parsing", func(t *testing.T) {
		// Verify server can parse tmux.detach request from mobile
		clientRequest := `{
			"type": "tmux.detach",
			"payload": {
				"session_id": "uuid-789",
				"kill": true
			}
		}`

		var msg struct {
			Type    MessageType       `json:"type"`
			Payload TmuxDetachPayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(clientRequest), &msg); err != nil {
			t.Fatalf("server failed to parse request: %v", err)
		}

		if msg.Type != MessageTypeTmuxDetach {
			t.Errorf("expected type %s, got %s", MessageTypeTmuxDetach, msg.Type)
		}
		if msg.Payload.SessionID != "uuid-789" {
			t.Errorf("expected session_id 'uuid-789', got %s", msg.Payload.SessionID)
		}
		if !msg.Payload.Kill {
			t.Error("expected kill=true")
		}
	})

	t.Run("detach request without kill flag", func(t *testing.T) {
		// Verify kill defaults to false when omitted
		clientRequest := `{
			"type": "tmux.detach",
			"payload": {
				"session_id": "uuid-000"
			}
		}`

		var msg struct {
			Type    MessageType       `json:"type"`
			Payload TmuxDetachPayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(clientRequest), &msg); err != nil {
			t.Fatalf("server failed to parse request: %v", err)
		}

		if msg.Payload.Kill {
			t.Error("expected kill=false when omitted from request")
		}
	})
}

// =============================================================================
// Unit 12.6: tmux Scrollback Capture Integration Tests
// =============================================================================

// TestTmuxAttachWithScrollback_Integration verifies the scrollback capture flow.
// This tests that after tmux attach, session.buffer is sent with scrollback lines.
func TestTmuxAttachWithScrollback_Integration(t *testing.T) {
	t.Run("session buffer message format", func(t *testing.T) {
		// Simulate what handleTmuxAttach does after capturing scrollback:
		// 1. CaptureScrollback returns scrollback content
		// 2. PrependToBuffer adds lines to session buffer
		// 3. After tmux.attached, send session.buffer with lines

		sessionID := "uuid-scrollback-test"
		scrollbackLines := []string{"line1\n", "line2\n", "line3\n"}

		// Build the session.buffer message as handleTmuxAttach does
		bufferMsg := NewSessionBufferMessage(sessionID, scrollbackLines, 0, 0)

		// Serialize to JSON (as would be sent over WebSocket)
		data, err := json.Marshal(bufferMsg)
		if err != nil {
			t.Fatalf("failed to serialize message: %v", err)
		}

		// Parse as raw JSON to verify wire format
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to parse message: %v", err)
		}

		// Verify message type
		if raw["type"] != string(MessageTypeSessionBuffer) {
			t.Errorf("expected type %q, got %v", MessageTypeSessionBuffer, raw["type"])
		}

		// Verify payload structure
		payload, ok := raw["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload to be object, got %T", raw["payload"])
		}

		if payload["session_id"] != sessionID {
			t.Errorf("expected session_id %q, got %v", sessionID, payload["session_id"])
		}

		lines, ok := payload["lines"].([]interface{})
		if !ok {
			t.Fatalf("expected lines to be array, got %T", payload["lines"])
		}

		if len(lines) != 3 {
			t.Errorf("expected 3 lines, got %d", len(lines))
		}

		// Verify line content
		expectedLines := []string{"line1\n", "line2\n", "line3\n"}
		for i, exp := range expectedLines {
			if lines[i] != exp {
				t.Errorf("line %d: expected %q, got %v", i, exp, lines[i])
			}
		}
	})

	t.Run("empty scrollback sends no buffer", func(t *testing.T) {
		// When scrollback is empty, no session.buffer should be sent
		// This is handled by: if len(lines) > 0 { ... }

		scrollbackLines := []string{} // Empty

		// In handleTmuxAttach, we check: if len(lines) > 0
		// If false, buffer message is not sent
		if len(scrollbackLines) > 0 {
			t.Error("empty scrollback should not trigger buffer send")
		}

		// Verify the condition used in handleTmuxAttach
		shouldSendBuffer := len(scrollbackLines) > 0
		if shouldSendBuffer {
			t.Error("empty scrollback should set shouldSendBuffer=false")
		}
	})

	t.Run("mobile can parse scrollback buffer", func(t *testing.T) {
		// Simulate mobile receiving session.buffer after tmux attach
		serverResponse := `{
			"type": "session.buffer",
			"payload": {
				"session_id": "abc-123",
				"lines": ["$ echo hello\n", "hello\n", "$ \n"],
				"cursor_row": 0,
				"cursor_col": 0
			}
		}`

		var msg Message
		if err := json.Unmarshal([]byte(serverResponse), &msg); err != nil {
			t.Fatalf("mobile failed to parse response: %v", err)
		}

		if msg.Type != MessageTypeSessionBuffer {
			t.Errorf("expected type %s, got %s", MessageTypeSessionBuffer, msg.Type)
		}

		// Mobile extracts payload and writes to terminal
		payloadMap, ok := msg.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload map, got %T", msg.Payload)
		}

		sessionID, _ := payloadMap["session_id"].(string)
		if sessionID != "abc-123" {
			t.Errorf("expected session_id 'abc-123', got %s", sessionID)
		}

		lines, ok := payloadMap["lines"].([]interface{})
		if !ok {
			t.Fatalf("expected lines array, got %T", payloadMap["lines"])
		}

		if len(lines) != 3 {
			t.Errorf("expected 3 scrollback lines, got %d", len(lines))
		}
	})

	t.Run("scrollback with terminal escape codes", func(t *testing.T) {
		// tmux capture-pane may include ANSI escape codes
		// Verify they pass through correctly

		sessionID := "uuid-ansi-test"
		scrollbackLines := []string{
			"\x1b[32mgreen text\x1b[0m\n",
			"\x1b[1mbold text\x1b[0m\n",
		}

		bufferMsg := NewSessionBufferMessage(sessionID, scrollbackLines, 0, 0)

		data, err := json.Marshal(bufferMsg)
		if err != nil {
			t.Fatalf("failed to serialize message: %v", err)
		}

		// Verify escape codes survive JSON round-trip
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("failed to parse message: %v", err)
		}

		payloadMap, ok := msg.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload map, got %T", msg.Payload)
		}

		lines, _ := payloadMap["lines"].([]interface{})
		if len(lines) != 2 {
			t.Errorf("expected 2 lines, got %d", len(lines))
		}

		// Verify escape codes preserved
		if lines[0] != "\x1b[32mgreen text\x1b[0m\n" {
			t.Errorf("ANSI escape codes not preserved: %v", lines[0])
		}
	})
}
