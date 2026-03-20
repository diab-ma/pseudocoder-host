package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --- helpers ---

// makeJSONLLine marshals a jsonlEvent to a single JSON line.
func makeJSONLLine(t *testing.T, event jsonlEvent) []byte {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return b
}

func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func userTextEvent(uuid, text, ts string) jsonlEvent {
	return jsonlEvent{
		UUID: uuid,
		Type: "human",
		Message: jsonlMessage{
			Role:    "user",
			Content: mustMarshalValue([]jsonlContentBlock{{Type: "text", Text: text}}),
		},
		Timestamp: mustMarshalValue(ts),
	}
}

func userStringEvent(uuid, text, ts string) jsonlEvent {
	return jsonlEvent{
		UUID: uuid,
		Type: "human",
		Message: jsonlMessage{
			Role:    "user",
			Content: mustMarshalValue(text),
		},
		Timestamp: mustMarshalValue(ts),
	}
}

func assistantEvent(uuid string, blocks []jsonlContentBlock, ts string) jsonlEvent {
	return jsonlEvent{
		UUID: uuid,
		Type: "assistant",
		Message: jsonlMessage{
			Role:    "assistant",
			Content: mustMarshalValue(blocks),
		},
		Timestamp: mustMarshalValue(ts),
	}
}

func toolResultEvent(uuid, toolUseID string, content interface{}, isError bool, ts string) jsonlEvent {
	return jsonlEvent{
		UUID: uuid,
		Type: "human",
		Message: jsonlMessage{
			Role: "user",
			Content: mustMarshalValue([]jsonlContentBlock{{
				Type:      "tool_result",
				ToolUseID: toolUseID,
				Content:   mustMarshalValue(content),
				IsError:   isError,
			}}),
		},
		Timestamp: mustMarshalValue(ts),
	}
}

func mustMarshalValue(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- tests ---

func TestJSONLParser_UserTextBlock(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	line := makeJSONLLine(t, userTextEvent("u1", "hello world", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-user:u1" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-user:u1")
	}
	if item.Kind != ChatItemKindUserMessage {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindUserMessage)
	}
	if item.Text != "hello world" {
		t.Errorf("Text = %q, want %q", item.Text, "hello world")
	}
	if item.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", item.SessionID, "sess-1")
	}
	if item.Provider != ChatItemProviderClaude {
		t.Errorf("Provider = %q, want %q", item.Provider, ChatItemProviderClaude)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
}

func TestJSONLParser_UserPlainString(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	line := makeJSONLLine(t, userStringEvent("u2", "plain text", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:u2" {
		t.Errorf("ID = %q, want %q", items[0].ID, "jsonl-user:u2")
	}
	if items[0].Text != "plain text" {
		t.Errorf("Text = %q, want %q", items[0].Text, "plain text")
	}
}

func TestJSONLParser_UserEventWithTypeUser(t *testing.T) {
	// Claude Code uses "human", but the spec also allows "user".
	p := newJSONLEventParser("sess-1")
	event := userTextEvent("u3", "typed as user", "2024-06-01T00:00:00Z")
	event.Type = "user"
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != ChatItemKindUserMessage {
		t.Errorf("Kind = %q, want user_message", items[0].Kind)
	}
}

func TestJSONLParser_AssistantTextBlock(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	blocks := []jsonlContentBlock{{Type: "text", Text: "Hi there!"}}
	line := makeJSONLLine(t, assistantEvent("a1", blocks, "2024-06-01T00:00:01Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-text:a1:0" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-text:a1:0")
	}
	if item.Kind != ChatItemKindAssistant {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindAssistant)
	}
	if item.Markdown != "Hi there!" {
		t.Errorf("Markdown = %q, want %q", item.Markdown, "Hi there!")
	}
}

func TestJSONLParser_AssistantThinkingBlock(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	blocks := []jsonlContentBlock{{Type: "thinking", Thinking: "Let me think..."}}
	line := makeJSONLLine(t, assistantEvent("a2", blocks, "2024-06-01T00:00:01Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-think:a2:0" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-think:a2:0")
	}
	if item.Kind != ChatItemKindThinking {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindThinking)
	}
	if item.Text != "Let me think..." {
		t.Errorf("Text = %q, want %q", item.Text, "Let me think...")
	}
}

func TestJSONLParser_AssistantToolUseBlock(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_abc",
		Name:  "Bash",
		Input: mustMarshalValue(map[string]string{"command": "ls"}),
	}}
	line := makeJSONLLine(t, assistantEvent("a3", blocks, "2024-06-01T00:00:01Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-tool:toolu_abc" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-tool:toolu_abc")
	}
	if item.Kind != ChatItemKindToolCall {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindToolCall)
	}
	if item.Status != ChatItemStatusInProgress {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusInProgress)
	}
	if item.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "Bash")
	}
	if item.Summary != "Bash" {
		t.Errorf("Summary = %q, want %q", item.Summary, "Bash")
	}
	if !strings.Contains(item.InputExcerpt, "command") {
		t.Errorf("InputExcerpt should contain 'command', got %q", item.InputExcerpt)
	}
	if item.StartedAt == 0 {
		t.Error("StartedAt should be set")
	}
}

func TestJSONLParser_ToolResultUpdatesExisting(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// First: assistant sends tool_use
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_xyz",
		Name:  "Read",
		Input: mustMarshalValue(map[string]string{"path": "/tmp/a.txt"}),
	}}
	p.ParseEvent(makeJSONLLine(t, assistantEvent("a4", blocks, "2024-06-01T00:00:01Z")))

	// Then: user sends tool_result
	items := p.ParseEvent(makeJSONLLine(t, toolResultEvent("u4", "toolu_xyz", "file contents here", false, "2024-06-01T00:00:02Z")))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-tool:toolu_xyz" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-tool:toolu_xyz")
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.IsError {
		t.Error("IsError should be false")
	}
	if !strings.Contains(item.ResultExcerpt, "file contents here") {
		t.Errorf("ResultExcerpt should contain result, got %q", item.ResultExcerpt)
	}
	if item.CompletedAt == 0 {
		t.Error("CompletedAt should be set")
	}
}

func TestJSONLParser_ToolResultFailed(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_fail",
		Name:  "Write",
		Input: mustMarshalValue(map[string]string{"path": "/tmp/b.txt"}),
	}}
	p.ParseEvent(makeJSONLLine(t, assistantEvent("a5", blocks, "2024-06-01T00:00:01Z")))

	items := p.ParseEvent(makeJSONLLine(t, toolResultEvent("u5", "toolu_fail", "permission denied", true, "2024-06-01T00:00:02Z")))

	item := items[0]
	if item.Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusFailed)
	}
	if !item.IsError {
		t.Error("IsError should be true")
	}
}

func TestJSONLParser_UnmatchedToolResult(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// No prior tool_use — should create synthetic tool_call.
	items := p.ParseEvent(makeJSONLLine(t, toolResultEvent("u6", "toolu_orphan", "some output", false, "2024-06-01T00:00:01Z")))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "jsonl-tool:toolu_orphan" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-tool:toolu_orphan")
	}
	if item.ToolName != "unknown" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "unknown")
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
}

func TestJSONLParser_UnmatchedToolResultFailed(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	items := p.ParseEvent(makeJSONLLine(t, toolResultEvent("u7", "toolu_orphan2", "error", true, "2024-06-01T00:00:01Z")))

	item := items[0]
	if item.Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusFailed)
	}
	if item.ToolName != "unknown" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "unknown")
	}
	if !item.IsError {
		t.Error("IsError should be true for failed synthetic tool_call")
	}
}

func TestJSONLParser_MultipleContentBlocks(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{
		{Type: "thinking", Thinking: "Hmm..."},
		{Type: "text", Text: "Here's my answer"},
		{Type: "tool_use", ID: "toolu_multi", Name: "Grep", Input: mustMarshalValue(map[string]string{"pattern": "foo"})},
	}
	ts := "2024-06-01T00:00:05Z"
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-multi", blocks, ts)))

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	expectedTS, _ := time.Parse(time.RFC3339, ts)
	expectedMillis := expectedTS.UnixMilli()

	// All items share the parent event's created_at.
	for i, item := range items {
		if item.CreatedAt != expectedMillis {
			t.Errorf("items[%d].CreatedAt = %d, want %d", i, item.CreatedAt, expectedMillis)
		}
	}

	// Verify order matches array order.
	if items[0].Kind != ChatItemKindThinking {
		t.Errorf("items[0].Kind = %q, want thinking", items[0].Kind)
	}
	if items[0].ID != "jsonl-think:a-multi:0" {
		t.Errorf("items[0].ID = %q, want jsonl-think:a-multi:0", items[0].ID)
	}
	if items[1].Kind != ChatItemKindAssistant {
		t.Errorf("items[1].Kind = %q, want assistant_message", items[1].Kind)
	}
	if items[1].ID != "jsonl-text:a-multi:1" {
		t.Errorf("items[1].ID = %q, want jsonl-text:a-multi:1", items[1].ID)
	}
	if items[2].Kind != ChatItemKindToolCall {
		t.Errorf("items[2].Kind = %q, want tool_call", items[2].Kind)
	}
	if items[2].ID != "jsonl-tool:toolu_multi" {
		t.Errorf("items[2].ID = %q, want jsonl-tool:toolu_multi", items[2].ID)
	}
}

func TestJSONLParser_UnknownContentBlockType(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{
		{Type: "text", Text: "before"},
		{Type: "unknown_block_type"},
		{Type: "text", Text: "after"},
	}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-unk", blocks, "2024-06-01T00:00:01Z")))

	// Unknown block skipped; two text blocks remain.
	if len(items) != 2 {
		t.Fatalf("expected 2 items (unknown skipped), got %d", len(items))
	}
	if items[0].Markdown != "before" {
		t.Errorf("items[0].Markdown = %q, want %q", items[0].Markdown, "before")
	}
	if items[1].Markdown != "after" {
		t.Errorf("items[1].Markdown = %q, want %q", items[1].Markdown, "after")
	}
}

func TestJSONLParser_UnknownEventType(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID:      "e-unk",
		Type:      "something_else",
		Timestamp: mustMarshalValue("2024-06-01T00:00:01Z"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for unknown event type, got %d", len(items))
	}
}

func TestJSONLParser_SystemEventSkipped(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID:      "sys-1",
		Type:      "system",
		Subtype:   "init",
		Timestamp: mustMarshalValue("2024-06-01T00:00:00Z"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for system event, got %d", len(items))
	}
}

func TestJSONLParser_MalformedJSON(t *testing.T) {
	p := newJSONLEventParser("sess-1")
	items := p.ParseEvent([]byte(`{this is not valid json`))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for malformed JSON, got %d", len(items))
	}
}

func TestJSONLParser_MissingTimestamp(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Set a previous timestamp via a valid event first.
	p.ParseEvent(makeJSONLLine(t, userTextEvent("u-prev", "first", "2024-06-01T00:00:10Z")))

	// Now send an event with no timestamp.
	event := jsonlEvent{
		UUID: "u-nots",
		Type: "human",
		Message: jsonlMessage{
			Role:    "user",
			Content: mustMarshalValue("no timestamp"),
		},
		// Timestamp is zero-value (empty)
	}
	line, _ := json.Marshal(event)
	items := p.ParseEvent(line)

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Fallback: max(prev_created_at + 1, time.Now().UnixMilli())
	// prev_created_at was 2024-06-01T00:00:10Z = 1717200010000
	prevTS := int64(1717200010000)
	if items[1].CreatedAt < prevTS+1 {
		t.Errorf("fallback CreatedAt = %d, expected >= %d", items[1].CreatedAt, prevTS+1)
	}
}

func TestJSONLParser_MalformedTimestamp(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID: "u-badts",
		Type: "human",
		Message: jsonlMessage{
			Role:    "user",
			Content: mustMarshalValue("bad ts"),
		},
		Timestamp: mustMarshalValue("not-a-date"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Should use fallback (time.Now() vicinity).
	now := time.Now().UnixMilli()
	if items[0].CreatedAt < now-5000 || items[0].CreatedAt > now+5000 {
		t.Errorf("fallback CreatedAt = %d, expected near %d", items[0].CreatedAt, now)
	}
}

func TestJSONLParser_NumericTimestamp(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID: "u-numts",
		Type: "human",
		Message: jsonlMessage{
			Role:    "user",
			Content: mustMarshalValue("numeric ts"),
		},
		Timestamp: mustMarshalValue(float64(1717200000000)),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].CreatedAt != 1717200000000 {
		t.Errorf("CreatedAt = %d, want 1717200000000", items[0].CreatedAt)
	}
}

func TestJSONLParser_NonMonotonicTimestamps(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// First event at T=10, second at T=5 — file order is authoritative.
	p.ParseEvent(makeJSONLLine(t, userTextEvent("u-t10", "first", "2024-06-01T00:00:10Z")))
	items := p.ParseEvent(makeJSONLLine(t, userTextEvent("u-t5", "second", "2024-06-01T00:00:05Z")))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// Items should be in file order, not sorted by timestamp.
	if items[0].Text != "first" {
		t.Errorf("items[0].Text = %q, want %q", items[0].Text, "first")
	}
	if items[1].Text != "second" {
		t.Errorf("items[1].Text = %q, want %q", items[1].Text, "second")
	}
	// Second item should have the earlier timestamp.
	if items[1].CreatedAt >= items[0].CreatedAt {
		t.Errorf("expected non-monotonic: items[1].CreatedAt=%d should be < items[0].CreatedAt=%d",
			items[1].CreatedAt, items[0].CreatedAt)
	}
}

func TestJSONLParser_ExcerptTruncation(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Create input exceeding 500 UTF-8 chars.
	longInput := strings.Repeat("A", 600)
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_long",
		Name:  "Read",
		Input: mustMarshalValue(map[string]string{"data": longInput}),
	}}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-long", blocks, "2024-06-01T00:00:01Z")))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// InputExcerpt should be truncated to 500 UTF-8 chars.
	runeCount := 0
	for range items[0].InputExcerpt {
		runeCount++
	}
	if runeCount > 500 {
		t.Errorf("InputExcerpt has %d runes, want <= 500", runeCount)
	}
}

func TestJSONLParser_SanitizationBearerToken(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_bearer",
		Name:  "Fetch",
		Input: mustMarshalValue(map[string]string{"header": "Authorization: Bearer sk-abc123secret"}),
	}}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-bearer", blocks, "2024-06-01T00:00:01Z")))

	if strings.Contains(items[0].InputExcerpt, "sk-abc123secret") {
		t.Error("InputExcerpt should not contain bearer token")
	}
	if !strings.Contains(items[0].InputExcerpt, "[REDACTED]") {
		t.Error("InputExcerpt should contain [REDACTED]")
	}
}

func TestJSONLParser_SanitizationEnvSecret(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_env",
		Name:  "Bash",
		Input: mustMarshalValue(map[string]string{"command": "API_TOKEN=mySecretToken123 ./run.sh"}),
	}}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-env", blocks, "2024-06-01T00:00:01Z")))

	if strings.Contains(items[0].InputExcerpt, "mySecretToken123") {
		t.Error("InputExcerpt should not contain env secret value")
	}
	if !strings.Contains(items[0].InputExcerpt, "[REDACTED]") {
		t.Error("InputExcerpt should contain [REDACTED]")
	}
}

func TestJSONLParser_SanitizationJSONSecretKey(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	input := map[string]string{
		"api_key":  "sk-secret-key-12345",
		"username": "admin",
	}
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_jsonkey",
		Name:  "Fetch",
		Input: mustMarshalValue(input),
	}}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-jsonkey", blocks, "2024-06-01T00:00:01Z")))

	if strings.Contains(items[0].InputExcerpt, "sk-secret-key-12345") {
		t.Error("InputExcerpt should not contain api_key value")
	}
	// username should still be present (not a secret key).
	if !strings.Contains(items[0].InputExcerpt, "admin") {
		t.Error("InputExcerpt should still contain non-secret values")
	}
}

func TestJSONLParser_SanitizationJSONSecretKeyCaseInsensitive(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"API_KEY", "secret1"},
		{"apikey", "secret2"},
		{"Secret", "secret3"},
		{"TOKEN", "secret4"},
		{"Password", "secret5"},
		{"credential", "secret6"},
		{"PRIVATE_KEY", "secret7"},
		{"access_key", "secret8"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			result := sanitizeJSONSecretKeys(`{"` + tt.key + `":"` + tt.value + `"}`)
			if strings.Contains(result, tt.value) {
				t.Errorf("sanitizeJSONSecretKeys should redact value for key %q, got %q", tt.key, result)
			}
			if !strings.Contains(result, "[REDACTED]") {
				t.Errorf("sanitizeJSONSecretKeys should contain [REDACTED] for key %q, got %q", tt.key, result)
			}
		})
	}
}

func TestJSONLParser_SanitizationANSI(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_ansi",
		Name:  "Bash",
		Input: mustMarshalValue(map[string]string{"output": "\x1b[31mred text\x1b[0m"}),
	}}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a-ansi", blocks, "2024-06-01T00:00:01Z")))

	if strings.Contains(items[0].InputExcerpt, "\x1b[") {
		t.Error("InputExcerpt should not contain ANSI escape codes")
	}
}

func TestJSONLParser_ToolResultExcerpt(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Tool use first.
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_res",
		Name:  "Read",
		Input: mustMarshalValue(map[string]string{"path": "/tmp/test"}),
	}}
	p.ParseEvent(makeJSONLLine(t, assistantEvent("a-res", blocks, "2024-06-01T00:00:01Z")))

	// Tool result with long content.
	longResult := strings.Repeat("x", 600)
	items := p.ParseEvent(makeJSONLLine(t, toolResultEvent("u-res", "toolu_res", longResult, false, "2024-06-01T00:00:02Z")))

	runeCount := 0
	for range items[0].ResultExcerpt {
		runeCount++
	}
	if runeCount > 500 {
		t.Errorf("ResultExcerpt has %d runes, want <= 500", runeCount)
	}
}

func TestJSONLParser_Reset(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	p.ParseEvent(makeJSONLLine(t, userTextEvent("u-pre", "before reset", "2024-06-01T00:00:01Z")))
	if len(p.items) != 1 {
		t.Fatalf("expected 1 item before reset, got %d", len(p.items))
	}

	p.Reset()

	if len(p.items) != 0 {
		t.Fatalf("expected 0 items after reset, got %d", len(p.items))
	}
	if len(p.toolItemIndex) != 0 {
		t.Errorf("expected empty toolItemIndex after reset, got %d", len(p.toolItemIndex))
	}
	if p.lastCreatedAt != 0 {
		t.Errorf("expected lastCreatedAt=0 after reset, got %d", p.lastCreatedAt)
	}
}

func TestJSONLParser_FullConversation(t *testing.T) {
	p := newJSONLEventParser("sess-full")

	// User sends a message.
	p.ParseEvent(makeJSONLLine(t, userTextEvent("u1", "list files", "2024-06-01T00:00:00Z")))

	// Assistant responds with thinking + text + tool_use.
	blocks := []jsonlContentBlock{
		{Type: "thinking", Thinking: "I should use ls"},
		{Type: "text", Text: "I'll list the files for you."},
		{Type: "tool_use", ID: "toolu_ls", Name: "Bash", Input: mustMarshalValue(map[string]string{"command": "ls"})},
	}
	p.ParseEvent(makeJSONLLine(t, assistantEvent("a1", blocks, "2024-06-01T00:00:01Z")))

	// Tool result.
	p.ParseEvent(makeJSONLLine(t, toolResultEvent("u2", "toolu_ls", "file1.txt\nfile2.txt", false, "2024-06-01T00:00:02Z")))

	// Assistant responds with final text.
	blocks2 := []jsonlContentBlock{
		{Type: "text", Text: "Here are the files: file1.txt and file2.txt"},
	}
	items := p.ParseEvent(makeJSONLLine(t, assistantEvent("a2", blocks2, "2024-06-01T00:00:03Z")))

	// u1 → 1 user_message
	// a1 → 3 items (thinking, text, tool_use)
	// u2 → tool_result updates tool_call in place (no new item)
	// a2 → 1 text
	// Total: 5
	if len(items) != 5 {
		t.Fatalf("expected 5 items in full conversation, got %d", len(items))
	}

	// Verify order matches file order.
	expectedKinds := []ChatItemKind{
		ChatItemKindUserMessage,
		ChatItemKindThinking,
		ChatItemKindAssistant,
		ChatItemKindToolCall,
		ChatItemKindAssistant,
	}
	for i, ek := range expectedKinds {
		if items[i].Kind != ek {
			t.Errorf("items[%d].Kind = %q, want %q", i, items[i].Kind, ek)
		}
	}

	// Tool call should be completed after result.
	toolItem := findChatItem(items, "jsonl-tool:toolu_ls")
	if toolItem == nil {
		t.Fatal("tool_call item not found")
	}
	if toolItem.Status != ChatItemStatusCompleted {
		t.Errorf("tool_call status = %q, want completed", toolItem.Status)
	}
}

func TestJSONLParser_EmptyContent(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID: "e-empty",
		Type: "human",
		Message: jsonlMessage{
			Role: "user",
		},
		Timestamp: mustMarshalValue("2024-06-01T00:00:00Z"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty content, got %d", len(items))
	}
}

func TestJSONLParser_UnknownUserBlockType(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	event := jsonlEvent{
		UUID: "u-unkblock",
		Type: "human",
		Message: jsonlMessage{
			Role: "user",
			Content: mustMarshalValue([]jsonlContentBlock{
				{Type: "custom_type"},
			}),
		},
		Timestamp: mustMarshalValue("2024-06-01T00:00:00Z"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for unknown user block type, got %d", len(items))
	}
}

func TestJSONLParser_ToolResultNonStringContent(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// tool_use
	blocks := []jsonlContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_obj",
		Name:  "Fetch",
		Input: mustMarshalValue(map[string]string{"url": "http://example.com"}),
	}}
	p.ParseEvent(makeJSONLLine(t, assistantEvent("a-obj", blocks, "2024-06-01T00:00:01Z")))

	// tool_result with object content (not a string)
	event := jsonlEvent{
		UUID: "u-obj",
		Type: "human",
		Message: jsonlMessage{
			Role: "user",
			Content: mustMarshalValue([]jsonlContentBlock{{
				Type:      "tool_result",
				ToolUseID: "toolu_obj",
				Content:   mustMarshalValue(map[string]string{"status": "ok"}),
				IsError:   false,
			}}),
		},
		Timestamp: mustMarshalValue("2024-06-01T00:00:02Z"),
	}
	items := p.ParseEvent(makeJSONLLine(t, event))

	item := items[0]
	if !strings.Contains(item.ResultExcerpt, "status") {
		t.Errorf("ResultExcerpt should contain non-string content, got %q", item.ResultExcerpt)
	}
}

func TestSanitizeJSONSecretKeys(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantSafe string // substring that should be present
		wantGone string // substring that should be absent
	}{
		{
			name:     "api_key string value",
			input:    `{"api_key":"sk-12345","name":"test"}`,
			wantSafe: `"name":"test"`,
			wantGone: "sk-12345",
		},
		{
			name:     "nested secret in key name",
			input:    `{"my_api_key_here":"val123"}`,
			wantSafe: "[REDACTED]",
			wantGone: "val123",
		},
		{
			name:     "password case insensitive",
			input:    `{"PASSWORD":"hunter2"}`,
			wantSafe: "[REDACTED]",
			wantGone: "hunter2",
		},
		{
			name:     "non-secret key preserved",
			input:    `{"username":"admin","path":"/home"}`,
			wantSafe: "admin",
			wantGone: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeJSONSecretKeys(tt.input)
			if tt.wantSafe != "" && !strings.Contains(result, tt.wantSafe) {
				t.Errorf("result should contain %q, got %q", tt.wantSafe, result)
			}
			if tt.wantGone != "" && strings.Contains(result, tt.wantGone) {
				t.Errorf("result should not contain %q, got %q", tt.wantGone, result)
			}
		})
	}
}

// --- Spec 05: Prompt deduplication tests ---

func TestJSONLParser_SeedPromptConsumed(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Seed a prompt before the JSONL event arrives.
	p.SeedPrompt("hello world", "req-abc", 1000)

	// JSONL user event with matching text.
	line := makeJSONLLine(t, userTextEvent("u1", "hello world", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	// ID should use the seed's request ID, not the event UUID.
	if item.ID != "jsonl-user:req-abc" {
		t.Errorf("ID = %q, want %q", item.ID, "jsonl-user:req-abc")
	}
	if item.RequestID != "req-abc" {
		t.Errorf("RequestID = %q, want %q", item.RequestID, "req-abc")
	}
	// CreatedAt should use the JSONL event timestamp, not the seed's.
	expectedTS, _ := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	if item.CreatedAt != expectedTS.UnixMilli() {
		t.Errorf("CreatedAt = %d, want %d", item.CreatedAt, expectedTS.UnixMilli())
	}
	// Seed should be consumed — next event should get a fresh ID.
	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after consumption")
	}
}

func TestJSONLParser_SeedPromptConsumedPlainString(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Seed a prompt; JSONL event uses plain string content format.
	p.SeedPrompt("plain text", "req-plain", 1000)

	line := makeJSONLLine(t, userStringEvent("u-plain", "plain text", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:req-plain" {
		t.Errorf("ID = %q, want %q", items[0].ID, "jsonl-user:req-plain")
	}
	if items[0].RequestID != "req-plain" {
		t.Errorf("RequestID = %q, want %q", items[0].RequestID, "req-plain")
	}
}

func TestJSONLParser_SeedPromptRollback(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Seed, then rollback before JSONL event arrives.
	p.SeedPrompt("hello", "req-rolled", 1000)
	p.ClearPendingSeed()

	// JSONL event with the same text should get a fresh ID (not ghost bubble).
	line := makeJSONLLine(t, userTextEvent("u-after-roll", "hello", "2024-06-01T00:00:01Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:u-after-roll" {
		t.Errorf("ID = %q, want fresh %q", items[0].ID, "jsonl-user:u-after-roll")
	}
	if items[0].RequestID != "" {
		t.Errorf("RequestID = %q, want empty (no seed)", items[0].RequestID)
	}
}

func TestJSONLParser_NoSeedFreshID(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// No seed registered — replay or terminal-typed prompt.
	line := makeJSONLLine(t, userTextEvent("u-fresh", "typed in terminal", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:u-fresh" {
		t.Errorf("ID = %q, want %q", items[0].ID, "jsonl-user:u-fresh")
	}
	if items[0].RequestID != "" {
		t.Errorf("RequestID = %q, want empty", items[0].RequestID)
	}
}

func TestJSONLParser_IdenticalConsecutivePrompts(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// First "yes": seed → JSONL event → consume.
	p.SeedPrompt("yes", "req-1", 1000)
	items := p.ParseEvent(makeJSONLLine(t, userTextEvent("u-yes1", "yes", "2024-06-01T00:00:00Z")))
	if len(items) != 1 {
		t.Fatalf("expected 1 item after first yes, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:req-1" {
		t.Errorf("first yes ID = %q, want %q", items[0].ID, "jsonl-user:req-1")
	}

	// Second "yes": seed → JSONL event → consume.
	p.SeedPrompt("yes", "req-2", 2000)
	items = p.ParseEvent(makeJSONLLine(t, userTextEvent("u-yes2", "yes", "2024-06-01T00:00:01Z")))
	if len(items) != 2 {
		t.Fatalf("expected 2 items after second yes, got %d", len(items))
	}
	if items[1].ID != "jsonl-user:req-2" {
		t.Errorf("second yes ID = %q, want %q", items[1].ID, "jsonl-user:req-2")
	}

	// Both items have distinct IDs.
	if items[0].ID == items[1].ID {
		t.Errorf("identical consecutive prompts should produce distinct IDs, both are %q", items[0].ID)
	}
}

func TestJSONLParser_ResetClearsPendingSeed(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	p.SeedPrompt("hello", "req-reset", 1000)
	p.Reset()

	// After reset, the seed should be gone.
	if p.pendingSeed != nil {
		t.Fatal("pendingSeed should be nil after Reset()")
	}

	// JSONL event with the same text gets a fresh ID.
	line := makeJSONLLine(t, userTextEvent("u-after-reset", "hello", "2024-06-01T00:00:01Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "jsonl-user:u-after-reset" {
		t.Errorf("ID = %q, want fresh %q", items[0].ID, "jsonl-user:u-after-reset")
	}
}

func TestJSONLParser_SeedNotConsumedOnMismatch(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// Seed for "hello" but JSONL event has different text.
	p.SeedPrompt("hello", "req-mismatch", 1000)
	line := makeJSONLLine(t, userTextEvent("u-diff", "goodbye", "2024-06-01T00:00:00Z"))
	items := p.ParseEvent(line)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Non-matching text → fresh ID.
	if items[0].ID != "jsonl-user:u-diff" {
		t.Errorf("ID = %q, want %q", items[0].ID, "jsonl-user:u-diff")
	}
	// Seed should still be pending (not consumed by mismatched text).
	if p.pendingSeed == nil {
		t.Error("pendingSeed should still be pending after non-matching event")
	}
}

func TestJSONLParser_SeedConsumedOnlyOnce(t *testing.T) {
	p := newJSONLEventParser("sess-1")

	// One seed, two matching events — only the first consumes it.
	p.SeedPrompt("yes", "req-once", 1000)

	items := p.ParseEvent(makeJSONLLine(t, userTextEvent("u-first", "yes", "2024-06-01T00:00:00Z")))
	if items[0].ID != "jsonl-user:req-once" {
		t.Errorf("first event ID = %q, want seeded %q", items[0].ID, "jsonl-user:req-once")
	}

	items = p.ParseEvent(makeJSONLLine(t, userTextEvent("u-second", "yes", "2024-06-01T00:00:01Z")))
	if items[1].ID != "jsonl-user:u-second" {
		t.Errorf("second event ID = %q, want fresh %q", items[1].ID, "jsonl-user:u-second")
	}
}
