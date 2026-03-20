package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --- Codex test helpers ---

// makeCodexLine marshals a codexEvent to a single JSON line.
func makeCodexLine(t *testing.T, event codexEvent) []byte {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal codex event: %v", err)
	}
	return b
}

func codexResponseItemEvent(ts float64, payload codexResponseItem) codexEvent {
	return codexEvent{
		Timestamp: ts,
		Type:      "response_item",
		Payload:   mustMarshalValue(payload),
	}
}

func codexEventMsgEvent(ts float64, msg codexEventMsg) codexEvent {
	return codexEvent{
		Timestamp: ts,
		Type:      "event_msg",
		Payload:   mustMarshalValue(msg),
	}
}

// --- Tests ---

func TestCodexParser_UserMessage(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-u1",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "hello world"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	item := items[1]
	if item.ID != "codex-user:msg-u1" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-user:msg-u1")
	}
	if item.Kind != ChatItemKindUserMessage {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindUserMessage)
	}
	if item.Text != "hello world" {
		t.Errorf("Text = %q, want %q", item.Text, "hello world")
	}
	if item.Provider != ChatItemProviderCodex {
		t.Errorf("Provider = %q, want %q", item.Provider, ChatItemProviderCodex)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", item.SessionID, "sess-1")
	}
	// Timestamp: 1717200000.0 seconds → 1717200000000 millis
	if item.CreatedAt != 1717200000000 {
		t.Errorf("CreatedAt = %d, want %d", item.CreatedAt, int64(1717200000000))
	}
}

func TestCodexParser_UserMessageWithStringTimestamp(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	line := []byte(`{
		"timestamp":"2026-03-16T01:52:29.707Z",
		"type":"response_item",
		"payload":{
			"type":"message",
			"role":"user",
			"id":"msg-u1",
			"content":[{"type":"input_text","text":"hello world"}]
		}
	}`)

	items := p.ParseEvent(line)

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].CreatedAt != 1773625949707 {
		t.Errorf("CreatedAt = %d, want %d", items[1].CreatedAt, int64(1773625949707))
	}
}

func TestCodexParser_UserMessageMultipleInputTexts(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-u2",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "first line"},
			{Type: "input_text", Text: "second line"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].Text != "first line\nsecond line" {
		t.Errorf("Text = %q, want %q", items[1].Text, "first line\nsecond line")
	}
}

func TestCodexParser_AssistantMessage(t *testing.T) {
	p := newCodexEventParser("sess-1")
	// task_started is required before assistant messages are accepted.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   "msg-a1",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "Here is the answer."},
			{Type: "output_text", Text: "And more detail."},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	// 1 task_started + 2 assistant text items = 3
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	if items[1].ID != "codex-text:msg-a1:0" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-text:msg-a1:0")
	}
	if items[1].Kind != ChatItemKindAssistant {
		t.Errorf("items[1].Kind = %q, want %q", items[1].Kind, ChatItemKindAssistant)
	}
	if items[1].Markdown != "Here is the answer." {
		t.Errorf("items[1].Markdown = %q, want %q", items[1].Markdown, "Here is the answer.")
	}
	if items[1].Provider != ChatItemProviderCodex {
		t.Errorf("items[1].Provider = %q, want %q", items[1].Provider, ChatItemProviderCodex)
	}

	if items[2].ID != "codex-text:msg-a1:1" {
		t.Errorf("items[2].ID = %q, want %q", items[2].ID, "codex-text:msg-a1:1")
	}
	if items[2].Markdown != "And more detail." {
		t.Errorf("items[2].Markdown = %q, want %q", items[2].Markdown, "And more detail.")
	}
}

func TestCodexParser_FunctionCall(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-1",
		Name:      "shell",
		Arguments: `{"command":"ls -la"}`,
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "codex-tool:call-1" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-tool:call-1")
	}
	if item.Kind != ChatItemKindToolCall {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindToolCall)
	}
	if item.Status != ChatItemStatusInProgress {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusInProgress)
	}
	if item.ToolName != "shell" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "shell")
	}
	if item.Summary != "shell" {
		t.Errorf("Summary = %q, want %q", item.Summary, "shell")
	}
	if !strings.Contains(item.InputExcerpt, "command") {
		t.Errorf("InputExcerpt should contain 'command', got %q", item.InputExcerpt)
	}
	if item.StartedAt == 0 {
		t.Error("StartedAt should be set")
	}
}

func TestCodexParser_FunctionCallOutputUpdatesExisting(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// First: function_call
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-2",
		Name:      "shell",
		Arguments: `{"command":"cat file.txt"}`,
	})))

	// Then: function_call_output
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-2",
		Output: "file contents here",
	})))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "codex-tool:call-2" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-tool:call-2")
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if !strings.Contains(item.ResultExcerpt, "file contents here") {
		t.Errorf("ResultExcerpt should contain result, got %q", item.ResultExcerpt)
	}
	if item.CompletedAt == 0 {
		t.Error("CompletedAt should be set")
	}
}

func TestCodexParser_FunctionCallOutputUnmatched(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// No prior function_call — should create synthetic tool_call.
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-orphan",
		Output: "some output",
	})))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "codex-tool:call-orphan" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-tool:call-orphan")
	}
	if item.ToolName != "unknown" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "unknown")
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
}

func TestCodexParser_ReasoningWithSummaryText(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200004.0, codexResponseItem{
		Type: "reasoning",
		ID:   "reason-1",
		Summary: []codexReasoningSummary{
			{Type: "summary_text", Text: "Thinking about approach"},
			{Type: "summary_text", Text: "Decided on plan B"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "codex-think:reason-1:0" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "codex-think:reason-1:0")
	}
	if items[0].Kind != ChatItemKindThinking {
		t.Errorf("items[0].Kind = %q, want %q", items[0].Kind, ChatItemKindThinking)
	}
	if items[0].Text != "Thinking about approach" {
		t.Errorf("items[0].Text = %q, want %q", items[0].Text, "Thinking about approach")
	}
	if items[1].ID != "codex-think:reason-1:1" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-think:reason-1:1")
	}
	if items[1].Text != "Decided on plan B" {
		t.Errorf("items[1].Text = %q, want %q", items[1].Text, "Decided on plan B")
	}
}

func TestCodexParser_ReasoningSkipsNonSummaryText(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200004.0, codexResponseItem{
		Type: "reasoning",
		ID:   "reason-2",
		Summary: []codexReasoningSummary{
			{Type: "summary_text", Text: "Valid summary"},
			{Type: "other_type", Text: "Should be skipped"},
			{Type: "summary_text", Text: ""}, // empty text, skip
			{Type: "summary_text", Text: "Also valid"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items (skipped non-summary_text and empty), got %d", len(items))
	}
	if items[0].Text != "Valid summary" {
		t.Errorf("items[0].Text = %q, want %q", items[0].Text, "Valid summary")
	}
	// The second valid item has index 3 in the summary array.
	if items[1].ID != "codex-think:reason-2:3" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-think:reason-2:3")
	}
	if items[1].Text != "Also valid" {
		t.Errorf("items[1].Text = %q, want %q", items[1].Text, "Also valid")
	}
}

func TestCodexParser_TaskStarted(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexEventMsgEvent(1717200005.0, codexEventMsg{
		EventID: "evt-1",
		Type:    "task_started",
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "codex-event:evt-1" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-event:evt-1")
	}
	if item.Kind != ChatItemKindSystemEvent {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindSystemEvent)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.Code != "task_started" {
		t.Errorf("Code = %q, want %q", item.Code, "task_started")
	}
	if item.Message != "Task started" {
		t.Errorf("Message = %q, want %q", item.Message, "Task started")
	}
}

func TestCodexParser_TaskComplete(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexEventMsgEvent(1717200006.0, codexEventMsg{
		EventID: "evt-2",
		Type:    "task_complete",
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.Code != "task_complete" {
		t.Errorf("Code = %q, want %q", item.Code, "task_complete")
	}
	if item.Message != "Task complete" {
		t.Errorf("Message = %q, want %q", item.Message, "Task complete")
	}
}

func TestCodexParser_DeveloperMessageSkipped(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "developer",
		ID:   "msg-dev",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "system instruction"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for developer message, got %d", len(items))
	}
}

func TestCodexParser_UnknownResponseItemType(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "something_unknown",
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for unknown response_item type, got %d", len(items))
	}
}

func TestCodexParser_SessionMetaSkipped(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexEvent{
		Timestamp: 1717200001.0,
		Type:      "session_meta",
		Payload:   mustMarshalValue(map[string]string{"version": "1.0"}),
	}
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for session_meta, got %d", len(items))
	}
}

func TestCodexParser_TurnContextSkipped(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexEvent{
		Timestamp: 1717200001.0,
		Type:      "turn_context",
		Payload:   mustMarshalValue(map[string]string{"turn": "1"}),
	}
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for turn_context, got %d", len(items))
	}
}

func TestCodexParser_MalformedJSON(t *testing.T) {
	p := newCodexEventParser("sess-1")
	items := p.ParseEvent([]byte(`{this is not valid json`))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for malformed JSON, got %d", len(items))
	}
}

func TestCodexParser_EmptyContentBlocks(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// User message with no input_text blocks → skip
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type:    "message",
		Role:    "user",
		ID:      "msg-empty",
		Content: []codexContentBlock{},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty content blocks, got %d", len(items))
	}
}

func TestCodexParser_AssistantEmptyOutputText(t *testing.T) {
	p := newCodexEventParser("sess-1")
	// task_started is required before assistant messages are accepted.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	// Assistant message with empty output_text → skip
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   "msg-a-empty",
		Content: []codexContentBlock{
			{Type: "output_text", Text: ""},
			{Type: "output_text", Text: "valid text"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	// 1 task_started + 1 valid assistant text = 2
	if len(items) != 2 {
		t.Fatalf("expected 2 items (empty skipped), got %d", len(items))
	}
	if items[1].Markdown != "valid text" {
		t.Errorf("Markdown = %q, want %q", items[1].Markdown, "valid text")
	}
}

func TestCodexParser_ANSIStrippingInAssistantOutput(t *testing.T) {
	p := newCodexEventParser("sess-1")
	// task_started is required before assistant messages are accepted.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   "msg-ansi",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "\x1b[31mred text\x1b[0m"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	// 1 task_started + 1 assistant text = 2
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if strings.Contains(items[1].Markdown, "\x1b[") {
		t.Error("Markdown should not contain ANSI escape codes")
	}
	if !strings.Contains(items[1].Markdown, "red text") {
		t.Errorf("Markdown should contain 'red text', got %q", items[1].Markdown)
	}
}

func TestCodexParser_SecretSanitizationInToolArguments(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-secret",
		Name:      "shell",
		Arguments: `{"command":"API_KEY=sk-secret123 ./run.sh"}`,
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if strings.Contains(items[0].InputExcerpt, "sk-secret123") {
		t.Error("InputExcerpt should not contain secret value")
	}
	if !strings.Contains(items[0].InputExcerpt, "[REDACTED]") {
		t.Error("InputExcerpt should contain [REDACTED]")
	}
}

func TestCodexParser_SecretSanitizationInToolOutput(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// function_call first
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-outsecret",
		Name:      "shell",
		Arguments: `{"command":"env"}`,
	})))

	// function_call_output with a secret
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-outsecret",
		Output: "Authorization: Bearer sk-superSecret999",
	})))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if strings.Contains(items[0].ResultExcerpt, "sk-superSecret999") {
		t.Error("ResultExcerpt should not contain secret value")
	}
	if !strings.Contains(items[0].ResultExcerpt, "[REDACTED]") {
		t.Error("ResultExcerpt should contain [REDACTED]")
	}
}

func TestCodexParser_SeedConsumed(t *testing.T) {
	p := newCodexEventParser("sess-1")

	p.SeedPrompt("hello world", "req-abc", 1000)

	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-u-seed",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "hello world"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "codex-user:req-abc" {
		t.Errorf("ID = %q, want %q", item.ID, "codex-user:req-abc")
	}
	if item.RequestID != "req-abc" {
		t.Errorf("RequestID = %q, want %q", item.RequestID, "req-abc")
	}
	// CreatedAt should use the event timestamp, not the seed's.
	if item.CreatedAt != 1717200000000 {
		t.Errorf("CreatedAt = %d, want %d", item.CreatedAt, int64(1717200000000))
	}
	// Seed should be consumed.
	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after consumption")
	}
}

func TestCodexParser_SeedNotConsumedOnMismatch(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	p.SeedPrompt("hello", "req-mismatch", 1000)

	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-u-diff",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "goodbye"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].ID != "codex-user:msg-u-diff" {
		t.Errorf("ID = %q, want fresh %q", items[1].ID, "codex-user:msg-u-diff")
	}
	if items[1].RequestID != "" {
		t.Errorf("RequestID = %q, want empty", items[1].RequestID)
	}
	if p.pendingSeed == nil {
		t.Error("pendingSeed should still be pending after non-matching event")
	}
}

func TestCodexParser_SeedConsumedOnlyOnce(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	p.SeedPrompt("yes", "req-once", 1000)

	event1 := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-first",
		Content: []codexContentBlock{{Type: "input_text", Text: "yes"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event1))
	if len(items) != 2 {
		t.Fatalf("expected 2 items after first yes, got %d", len(items))
	}
	if items[1].ID != "codex-user:req-once" {
		t.Errorf("first event ID = %q, want seeded %q", items[1].ID, "codex-user:req-once")
	}

	event2 := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-second",
		Content: []codexContentBlock{{Type: "input_text", Text: "yes"}},
	})
	items = p.ParseEvent(makeCodexLine(t, event2))
	if len(items) != 3 {
		t.Fatalf("expected 3 items after second yes, got %d", len(items))
	}
	if items[2].ID != "codex-user:msg-second" {
		t.Errorf("second event ID = %q, want fresh %q", items[2].ID, "codex-user:msg-second")
	}
}

func TestCodexParser_SeedRollback(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	p.SeedPrompt("hello", "req-rolled", 1000)
	p.ClearPendingSeed()

	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-after-roll",
		Content: []codexContentBlock{{Type: "input_text", Text: "hello"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].ID != "codex-user:msg-after-roll" {
		t.Errorf("ID = %q, want fresh %q", items[1].ID, "codex-user:msg-after-roll")
	}
	if items[1].RequestID != "" {
		t.Errorf("RequestID = %q, want empty", items[1].RequestID)
	}
}

func TestCodexParser_SeedPromptWithItem(t *testing.T) {
	p := newCodexEventParser("sess-1")

	p.SeedPromptWithItem("hello world", "req-seeded", 5000)

	// The seeded item should appear immediately.
	items := p.timeline()
	if len(items) != 1 {
		t.Fatalf("expected 1 seeded item, got %d", len(items))
	}
	if items[0].ID != "codex-user:req-seeded" {
		t.Errorf("ID = %q, want %q", items[0].ID, "codex-user:req-seeded")
	}
	if items[0].Provider != ChatItemProviderCodex {
		t.Errorf("Provider = %q, want %q", items[0].Provider, ChatItemProviderCodex)
	}
	if items[0].CreatedAt != 5000 {
		t.Errorf("CreatedAt = %d, want 5000", items[0].CreatedAt)
	}

	// When the JSONL echo arrives, it should update in place.
	event := codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-echo",
		Content: []codexContentBlock{{Type: "input_text", Text: "hello world"}},
	})
	items = p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 1 {
		t.Fatalf("expected 1 item after echo (replaced in place), got %d", len(items))
	}
	if items[0].ID != "codex-user:req-seeded" {
		t.Errorf("ID = %q, want %q", items[0].ID, "codex-user:req-seeded")
	}
	// Timestamp should be updated from the event.
	if items[0].CreatedAt != 1717200000000 {
		t.Errorf("CreatedAt = %d, want %d", items[0].CreatedAt, int64(1717200000000))
	}
}

func TestCodexParser_RollbackSeedItem(t *testing.T) {
	p := newCodexEventParser("sess-1")

	p.SeedPromptWithItem("hello", "req-rollback", 5000)

	// Verify item exists.
	if len(p.items) != 1 {
		t.Fatalf("expected 1 seeded item, got %d", len(p.items))
	}

	p.RollbackSeedItem("req-rollback")

	if len(p.items) != 0 {
		t.Fatalf("expected 0 items after rollback, got %d", len(p.items))
	}
	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after rollback")
	}
}

func TestCodexParser_Reset(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Add some items and set seenTaskStarted.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-pre",
		Content: []codexContentBlock{{Type: "input_text", Text: "before reset"}},
	})))
	p.SeedPrompt("hello", "req-reset", 1000)

	if len(p.items) != 2 {
		t.Fatalf("expected 2 items before reset, got %d", len(p.items))
	}
	if !p.seenTaskStarted {
		t.Fatal("seenTaskStarted should be true before reset")
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
	if p.pendingSeed != nil {
		t.Fatal("pendingSeed should be nil after Reset()")
	}
	if p.seenTaskStarted {
		t.Fatal("seenTaskStarted should be false after Reset()")
	}
}

func TestCodexParser_ResetClearsPendingSeed(t *testing.T) {
	p := newCodexEventParser("sess-1")

	p.SeedPrompt("hello", "req-reset", 1000)
	p.Reset()

	if p.pendingSeed != nil {
		t.Fatal("pendingSeed should be nil after Reset()")
	}

	// task_started needed after reset for non-seeded user messages.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	// JSONL event with the same text gets a fresh ID.
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-after-reset",
		Content: []codexContentBlock{{Type: "input_text", Text: "hello"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].ID != "codex-user:msg-after-reset" {
		t.Errorf("ID = %q, want fresh %q", items[1].ID, "codex-user:msg-after-reset")
	}
}

func TestCodexParser_DuplicateFunctionCallOutput(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// function_call
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-dup",
		Name:      "shell",
		Arguments: `{"command":"echo hi"}`,
	})))

	// First function_call_output
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-dup",
		Output: "first output",
	})))

	// Second function_call_output for same call_id — updates in place.
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200004.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-dup",
		Output: "second output",
	})))

	// Should still be 1 item (the original tool_call updated twice).
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if !strings.Contains(items[0].ResultExcerpt, "second output") {
		t.Errorf("ResultExcerpt should contain second output, got %q", items[0].ResultExcerpt)
	}
}

func TestCodexParser_UnknownEventMsgType(t *testing.T) {
	p := newCodexEventParser("sess-1")
	event := codexEventMsgEvent(1717200005.0, codexEventMsg{
		EventID: "evt-unk",
		Type:    "something_else",
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for unknown event_msg type, got %d", len(items))
	}
}

func TestCodexParser_MissingTimestamp(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	// Set a known previous timestamp.
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200010.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-prev",
		Content: []codexContentBlock{{Type: "input_text", Text: "first"}},
	})))

	// Event with timestamp 0 → fallback.
	event := codexResponseItemEvent(0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-nots",
		Content: []codexContentBlock{{Type: "input_text", Text: "no timestamp"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// Fallback: max(prev_created_at + 1, time.Now().UnixMilli())
	prevTS := int64(1717200010000) // 1717200010.0 * 1000
	if items[2].CreatedAt < prevTS+1 {
		t.Errorf("fallback CreatedAt = %d, expected >= %d", items[2].CreatedAt, prevTS+1)
	}
}

func TestCodexParser_TimestampConversion(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	// 1717200000.5 seconds → 1717200000500 millis
	event := codexResponseItemEvent(1717200000.5, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-ts",
		Content: []codexContentBlock{{Type: "input_text", Text: "with float ts"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].CreatedAt != 1717200000500 {
		t.Errorf("CreatedAt = %d, want 1717200000500", items[1].CreatedAt)
	}
}

func TestCodexParser_TimestampFallbackNearNow(t *testing.T) {
	p := newCodexEventParser("sess-1")
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717199999.0, codexEventMsg{EventID: "evt-0", Type: "task_started"})))

	// Event with 0 timestamp → should be near now.
	event := codexResponseItemEvent(0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-near-now",
		Content: []codexContentBlock{{Type: "input_text", Text: "test"}},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	now := time.Now().UnixMilli()
	if items[1].CreatedAt < now-5000 || items[1].CreatedAt > now+5000 {
		t.Errorf("fallback CreatedAt = %d, expected near %d", items[1].CreatedAt, now)
	}
}

func TestCodexParser_FullConversation(t *testing.T) {
	p := newCodexEventParser("sess-full")

	// 1. task_started event
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{
		EventID: "evt-start",
		Type:    "task_started",
	})))

	// 2. User message
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-u1",
		Content: []codexContentBlock{{Type: "input_text", Text: "list files"}},
	})))

	// 3. Reasoning
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200002.0, codexResponseItem{
		Type: "reasoning", ID: "reason-1",
		Summary: []codexReasoningSummary{{Type: "summary_text", Text: "I should use ls"}},
	})))

	// 4. Assistant message
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "msg-a1",
		Content: []codexContentBlock{{Type: "output_text", Text: "I'll list the files."}},
	})))

	// 5. function_call
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200004.0, codexResponseItem{
		Type:      "function_call",
		CallID:    "call-ls",
		Name:      "shell",
		Arguments: `{"command":"ls"}`,
	})))

	// 6. function_call_output
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200005.0, codexResponseItem{
		Type:   "function_call_output",
		CallID: "call-ls",
		Output: "file1.txt\nfile2.txt",
	})))

	// 7. Final assistant message
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200006.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "msg-a2",
		Content: []codexContentBlock{{Type: "output_text", Text: "Found: file1.txt, file2.txt"}},
	})))

	// Expected: task_started, user, thinking, assistant, tool_call (updated), assistant
	// = 6 items total
	if len(items) != 6 {
		t.Fatalf("expected 6 items in full conversation, got %d", len(items))
	}

	expectedKinds := []ChatItemKind{
		ChatItemKindSystemEvent,
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

	// Tool call should be completed.
	toolItem := findChatItem(items, "codex-tool:call-ls")
	if toolItem == nil {
		t.Fatal("tool_call item not found")
	}
	if toolItem.Status != ChatItemStatusCompleted {
		t.Errorf("tool_call status = %q, want completed", toolItem.Status)
	}

	// All items should have Provider=codex.
	for i, item := range items {
		if item.Provider != ChatItemProviderCodex {
			t.Errorf("items[%d].Provider = %q, want %q", i, item.Provider, ChatItemProviderCodex)
		}
	}
}

func TestCodexParser_MalformedPayload(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Valid outer JSON structure, but the payload content is not a valid response_item.
	line := []byte(`{"timestamp":1717200001.0,"type":"response_item","payload":{"type":123}}`)
	items := p.ParseEvent(line)

	// type is numeric instead of string — unmarshal into codexResponseItem won't match
	// any known type, so it should be skipped.
	if len(items) != 0 {
		t.Fatalf("expected 0 items for malformed payload, got %d", len(items))
	}
}

func TestCodexParser_MalformedEventMsgPayload(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Valid outer JSON, but the payload is not valid JSON for an event_msg.
	line := []byte(`{"timestamp":1717200001.0,"type":"event_msg","payload":"not-an-object"}`)
	items := p.ParseEvent(line)

	if len(items) != 0 {
		t.Fatalf("expected 0 items for malformed event_msg payload, got %d", len(items))
	}
}

func TestCodexParser_UserMessageNonInputTextBlocks(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// User message with only non-input_text blocks → skip
	event := codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-other-blocks",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "this is output, not input"},
		},
	})
	items := p.ParseEvent(makeCodexLine(t, event))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for non-input_text blocks in user message, got %d", len(items))
	}
}

func TestCodexParser_IdenticalConsecutivePrompts(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// First "yes": seed → event → consume.
	p.SeedPrompt("yes", "req-1", 1000)
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200000.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-yes1",
		Content: []codexContentBlock{{Type: "input_text", Text: "yes"}},
	})))
	if len(items) != 1 {
		t.Fatalf("expected 1 item after first yes, got %d", len(items))
	}
	if items[0].ID != "codex-user:req-1" {
		t.Errorf("first yes ID = %q, want %q", items[0].ID, "codex-user:req-1")
	}

	// Second "yes": seed → event → consume.
	p.SeedPrompt("yes", "req-2", 2000)
	items = p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-yes2",
		Content: []codexContentBlock{{Type: "input_text", Text: "yes"}},
	})))
	if len(items) != 2 {
		t.Fatalf("expected 2 items after second yes, got %d", len(items))
	}
	if items[1].ID != "codex-user:req-2" {
		t.Errorf("second yes ID = %q, want %q", items[1].ID, "codex-user:req-2")
	}

	// Both items have distinct IDs.
	if items[0].ID == items[1].ID {
		t.Errorf("identical consecutive prompts should produce distinct IDs, both are %q", items[0].ID)
	}
}

func TestCodexParser_AssistantBeforeTaskStartedSkipped(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Assistant message BEFORE task_started → should be silently dropped.
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   "msg-agents-md",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "You are an AI assistant..."},
		},
	})))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for assistant before task_started, got %d", len(items))
	}
}

func TestCodexParser_AssistantAfterTaskStartedKept(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// task_started first.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{
		EventID: "evt-start",
		Type:    "task_started",
	})))

	// Assistant message after task_started → kept.
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   "msg-a1",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "Hi. What do you need?"},
		},
	})))

	// 1 task_started event + 1 assistant item = 2
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[1].Kind != ChatItemKindAssistant {
		t.Errorf("items[1].Kind = %q, want %q", items[1].Kind, ChatItemKindAssistant)
	}
	if items[1].Markdown != "Hi. What do you need?" {
		t.Errorf("items[1].Markdown = %q, want %q", items[1].Markdown, "Hi. What do you need?")
	}
}

func TestCodexParser_FullConversationWithLeadingSystemContext(t *testing.T) {
	p := newCodexEventParser("sess-full-lead")

	// Realistic Codex JSONL sequence:
	// 1. session_meta (skipped by parser)
	p.ParseEvent(makeCodexLine(t, codexEvent{
		Timestamp: 1717200000.0,
		Type:      "session_meta",
		Payload:   mustMarshalValue(map[string]string{"version": "1.0"}),
	}))

	// 2. AGENTS.md system context as user role — should be DROPPED (non-seeded, before task_started)
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-agents-dump",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "You are an AI coding assistant. Follow these rules..."},
		},
	})))

	// 3. task_started
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200002.0, codexEventMsg{
		EventID: "evt-start",
		Type:    "task_started",
	})))

	// 4. User CLI launch prompt — after task_started, kept
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200003.0, codexResponseItem{
		Type: "message", Role: "user", ID: "msg-u-cli",
		Content: []codexContentBlock{{Type: "input_text", Text: "fix the bug in main.go"}},
	})))

	// 5. Real assistant greeting — kept
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200004.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "msg-a-greeting",
		Content: []codexContentBlock{{Type: "output_text", Text: "I'll fix that bug for you."}},
	})))

	// 6. function_call
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200005.0, codexResponseItem{
		Type: "function_call", CallID: "call-fix", Name: "shell",
		Arguments: `{"command":"go build ./..."}`,
	})))

	// 7. function_call_output
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200006.0, codexResponseItem{
		Type: "function_call_output", CallID: "call-fix",
		Output: "ok",
	})))

	// Expected items: task_started, user(cli prompt), assistant(greeting), tool_call
	// The AGENTS.md user dump (msg-agents-dump) should NOT appear.
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}

	expectedKinds := []ChatItemKind{
		ChatItemKindSystemEvent,
		ChatItemKindUserMessage,
		ChatItemKindAssistant,
		ChatItemKindToolCall,
	}
	for i, ek := range expectedKinds {
		if items[i].Kind != ek {
			t.Errorf("items[%d].Kind = %q, want %q", i, items[i].Kind, ek)
		}
	}

	// Verify the AGENTS.md dump is not present.
	for _, item := range items {
		if item.ID == "codex-user:msg-agents-dump" {
			t.Error("AGENTS.md user dump should have been filtered out")
		}
	}

	// The real assistant greeting should be present.
	if items[2].Markdown != "I'll fix that bug for you." {
		t.Errorf("items[2].Markdown = %q, want greeting", items[2].Markdown)
	}
}

func TestCodexParser_UserBeforeTaskStartedSkipped(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Non-seeded user message before task_started → should be dropped.
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-agents-md",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "You are an AI assistant. <INSTRUCTIONS>...</INSTRUCTIONS>"},
		},
	})))

	if len(items) != 0 {
		t.Fatalf("expected 0 items for non-seeded user before task_started, got %d", len(items))
	}
}

func TestCodexParser_SeededUserBeforeTaskStartedKept(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Seeded user message before task_started → kept (sent from mobile).
	p.SeedPrompt("fix the bug", "req-mobile", 1000)

	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(1717200001.0, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   "msg-mobile-prompt",
		Content: []codexContentBlock{
			{Type: "input_text", Text: "fix the bug"},
		},
	})))

	if len(items) != 1 {
		t.Fatalf("expected 1 item for seeded user before task_started, got %d", len(items))
	}
	if items[0].ID != "codex-user:req-mobile" {
		t.Errorf("ID = %q, want %q", items[0].ID, "codex-user:req-mobile")
	}
	if items[0].RequestID != "req-mobile" {
		t.Errorf("RequestID = %q, want %q", items[0].RequestID, "req-mobile")
	}
}

func TestCodexParser_EmptyEventIDFallback(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Codex CLI may emit event_msg with empty event_id. Each should get a
	// unique sequence-based ID so multi-turn sessions don't collide.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200000.0, codexEventMsg{
		EventID: "",
		Type:    "task_started",
	})))
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200010.0, codexEventMsg{
		EventID: "",
		Type:    "task_complete",
	})))
	// Second turn: another task_started with empty EventID.
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200020.0, codexEventMsg{
		EventID: "",
		Type:    "task_started",
	})))
	items := p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1717200030.0, codexEventMsg{
		EventID: "",
		Type:    "task_complete",
	})))

	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	if items[0].ID != "codex-event:task_started-1" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "codex-event:task_started-1")
	}
	if items[1].ID != "codex-event:task_complete-2" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-event:task_complete-2")
	}
	if items[2].ID != "codex-event:task_started-3" {
		t.Errorf("items[2].ID = %q, want %q", items[2].ID, "codex-event:task_started-3")
	}
	if items[3].ID != "codex-event:task_complete-4" {
		t.Errorf("items[3].ID = %q, want %q", items[3].ID, "codex-event:task_complete-4")
	}
}

func TestCodexParser_EmptyItemIDAssistant(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// task_started (seq 1)
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1.0, codexEventMsg{Type: "task_started"})))

	// Two assistant messages with empty ID — each should get a distinct seq-based ID.
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(2.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "",
		Content: []codexContentBlock{{Type: "output_text", Text: "hello"}},
	})))
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(3.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "",
		Content: []codexContentBlock{{Type: "output_text", Text: "world"}},
	})))

	// items: task_started, assistant1, assistant2
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[1].ID == items[2].ID {
		t.Errorf("duplicate assistant IDs: both are %q", items[1].ID)
	}
	if items[1].ID != "codex-text:seq2:0" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-text:seq2:0")
	}
	if items[2].ID != "codex-text:seq3:0" {
		t.Errorf("items[2].ID = %q, want %q", items[2].ID, "codex-text:seq3:0")
	}
}

func TestCodexParser_EmptyItemIDUser(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// task_started (seq 1)
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1.0, codexEventMsg{Type: "task_started"})))

	// Two user messages with empty ID — should get distinct IDs via itemID.
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(2.0, codexResponseItem{
		Type: "message", Role: "user", ID: "",
		Content: []codexContentBlock{{Type: "input_text", Text: "first question"}},
	})))
	items := p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(3.0, codexResponseItem{
		Type: "message", Role: "user", ID: "",
		Content: []codexContentBlock{{Type: "input_text", Text: "second question"}},
	})))

	// items: task_started, user1, user2
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[1].ID == items[2].ID {
		t.Errorf("duplicate user IDs: both are %q", items[1].ID)
	}
	if items[1].ID != "codex-user:seq2" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "codex-user:seq2")
	}
	if items[2].ID != "codex-user:seq3" {
		t.Errorf("items[2].ID = %q, want %q", items[2].ID, "codex-user:seq3")
	}
}

func TestCodexParser_EmptyIDMultiTurn(t *testing.T) {
	p := newCodexEventParser("sess-1")

	// Full multi-turn with ALL IDs empty.
	// Turn 1: task_started → user → assistant → task_complete
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(1.0, codexEventMsg{Type: "task_started"})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(2.0, codexResponseItem{
		Type: "message", Role: "user", ID: "",
		Content: []codexContentBlock{{Type: "input_text", Text: "hello"}},
	})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(3.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "",
		Content: []codexContentBlock{{Type: "output_text", Text: "hi there"}},
	})))
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(4.0, codexEventMsg{Type: "task_complete"})))

	// Turn 2: task_started → user → assistant (2 blocks) → assistant → task_complete
	p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(5.0, codexEventMsg{Type: "task_started"})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(6.0, codexResponseItem{
		Type: "message", Role: "user", ID: "",
		Content: []codexContentBlock{{Type: "input_text", Text: "more"}},
	})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(7.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "",
		Content: []codexContentBlock{
			{Type: "output_text", Text: "block one"},
			{Type: "output_text", Text: "block two"},
		},
	})))
	p.ParseEvent(makeCodexLine(t, codexResponseItemEvent(8.0, codexResponseItem{
		Type: "message", Role: "assistant", ID: "",
		Content: []codexContentBlock{{Type: "output_text", Text: "final"}},
	})))
	items := p.ParseEvent(makeCodexLine(t, codexEventMsgEvent(9.0, codexEventMsg{Type: "task_complete"})))

	// Expected items:
	// 0: task_started (seq1)
	// 1: user (seq2)
	// 2: assistant (seq3:0)
	// 3: task_complete (seq4)
	// 4: task_started (seq5)
	// 5: user (seq6)
	// 6: assistant block one (seq7:0)
	// 7: assistant block two (seq7:1)
	// 8: assistant final (seq8:0)
	// 9: task_complete (seq9)
	if len(items) != 10 {
		t.Fatalf("expected 10 items, got %d", len(items))
	}

	// Verify no duplicate IDs.
	seen := make(map[string]bool)
	for i, it := range items {
		if seen[it.ID] {
			t.Errorf("duplicate ID at index %d: %q", i, it.ID)
		}
		seen[it.ID] = true
	}

	// Spot-check specific IDs.
	wantIDs := map[int]string{
		0: "codex-event:task_started-1",
		1: "codex-user:seq2",
		2: "codex-text:seq3:0",
		3: "codex-event:task_complete-4",
		4: "codex-event:task_started-5",
		5: "codex-user:seq6",
		6: "codex-text:seq7:0",
		7: "codex-text:seq7:1",
		8: "codex-text:seq8:0",
		9: "codex-event:task_complete-9",
	}
	for idx, wantID := range wantIDs {
		if items[idx].ID != wantID {
			t.Errorf("items[%d].ID = %q, want %q", idx, items[idx].ID, wantID)
		}
	}

	// Verify all expected kinds are present.
	kindCounts := make(map[ChatItemKind]int)
	for _, it := range items {
		kindCounts[it.Kind]++
	}
	if kindCounts[ChatItemKindSystemEvent] != 4 {
		t.Errorf("system events = %d, want 4", kindCounts[ChatItemKindSystemEvent])
	}
	if kindCounts[ChatItemKindUserMessage] != 2 {
		t.Errorf("user messages = %d, want 2", kindCounts[ChatItemKindUserMessage])
	}
	if kindCounts[ChatItemKindAssistant] != 4 {
		t.Errorf("assistant messages = %d, want 4", kindCounts[ChatItemKindAssistant])
	}
}
