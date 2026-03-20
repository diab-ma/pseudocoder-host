package server

import (
	"fmt"
	"strings"
	"testing"
)

// --- helpers ---

func geminiMsg(typ, content string, ts float64) geminiMessage {
	return geminiMessage{Type: typ, Content: content, Timestamp: ts}
}

func geminiMsgWithThoughts(content string, ts float64, thoughts []geminiThought) geminiMessage {
	return geminiMessage{Type: "gemini", Content: content, Thoughts: thoughts, Timestamp: ts}
}

func geminiMsgWithTools(content string, ts float64, tools []geminiToolCall) geminiMessage {
	return geminiMessage{Type: "gemini", Content: content, ToolCalls: tools, Timestamp: ts}
}

func geminiMsgFull(content string, ts float64, thoughts []geminiThought, tools []geminiToolCall) geminiMessage {
	return geminiMessage{Type: "gemini", Content: content, Thoughts: thoughts, ToolCalls: tools, Timestamp: ts}
}

// --- tests ---

func TestGeminiParser_UserMessage(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello world", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-user:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-user:0")
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
	if item.Provider != ChatItemProviderGemini {
		t.Errorf("Provider = %q, want %q", item.Provider, ChatItemProviderGemini)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.CreatedAt != 1717200000000 {
		t.Errorf("CreatedAt = %d, want %d", item.CreatedAt, int64(1717200000000))
	}
}

func TestGeminiParser_GeminiContent(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("gemini", "Here is my answer.", 1717200001),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-text:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-text:0")
	}
	if item.Kind != ChatItemKindAssistant {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindAssistant)
	}
	if item.Markdown != "Here is my answer." {
		t.Errorf("Markdown = %q, want %q", item.Markdown, "Here is my answer.")
	}
}

func TestGeminiParser_GeminiThoughts(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithThoughts("", 1717200001, []geminiThought{
			{Subject: "Analysis", Description: "Let me think about this"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-think:0:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-think:0:0")
	}
	if item.Kind != ChatItemKindThinking {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindThinking)
	}
	if item.Text != "Analysis: Let me think about this" {
		t.Errorf("Text = %q, want %q", item.Text, "Analysis: Let me think about this")
	}
}

func TestGeminiParser_ThoughtSubjectOnly(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithThoughts("", 1717200001, []geminiThought{
			{Subject: "Planning", Description: ""},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Text != "Planning" {
		t.Errorf("Text = %q, want %q", items[0].Text, "Planning")
	}
}

func TestGeminiParser_ThoughtDescriptionOnly(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithThoughts("", 1717200001, []geminiThought{
			{Subject: "", Description: "thinking..."},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Text != "thinking..." {
		t.Errorf("Text = %q, want %q", items[0].Text, "thinking...")
	}
}

func TestGeminiParser_EmptyThoughtSkipped(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithThoughts("", 1717200001, []geminiThought{
			{Subject: "", Description: ""},
		}),
	})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty thought, got %d", len(items))
	}
}

func TestGeminiParser_ToolCallSuccess(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "ReadFile", Input: "/tmp/test.txt", Result: "file contents here", Status: "success"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-tool:0:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-tool:0:0")
	}
	if item.Kind != ChatItemKindToolCall {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindToolCall)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.ToolName != "ReadFile" {
		t.Errorf("ToolName = %q, want %q", item.ToolName, "ReadFile")
	}
	if item.Summary != "ReadFile" {
		t.Errorf("Summary = %q, want %q", item.Summary, "ReadFile")
	}
	if !strings.Contains(item.InputExcerpt, "/tmp/test.txt") {
		t.Errorf("InputExcerpt should contain input, got %q", item.InputExcerpt)
	}
	if !strings.Contains(item.ResultExcerpt, "file contents here") {
		t.Errorf("ResultExcerpt should contain result, got %q", item.ResultExcerpt)
	}
	if item.StartedAt != 1717200001000 {
		t.Errorf("StartedAt = %d, want %d", item.StartedAt, int64(1717200001000))
	}
	if item.CompletedAt != 0 {
		t.Errorf("CompletedAt = %d, want 0 (Gemini has no per-tool timestamps)", item.CompletedAt)
	}
}

func TestGeminiParser_ToolCallInProgress(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Input: "ls -la", Status: ""},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != ChatItemStatusInProgress {
		t.Errorf("Status = %q, want %q", items[0].Status, ChatItemStatusInProgress)
	}
	if items[0].CompletedAt != 0 {
		t.Errorf("CompletedAt = %d, want 0 for in-progress", items[0].CompletedAt)
	}
}

func TestGeminiParser_ToolCallError(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Input: "rm -rf /", Result: "permission denied", Status: "error"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q", items[0].Status, ChatItemStatusFailed)
	}
	if items[0].CompletedAt != 0 {
		t.Errorf("CompletedAt = %d, want 0 (Gemini has no per-tool timestamps)", items[0].CompletedAt)
	}
}

func TestGeminiParser_MixedGeminiMessage(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgFull("Here is the result.", 1717200001,
			[]geminiThought{
				{Subject: "Plan", Description: "I need to read the file"},
			},
			[]geminiToolCall{
				{Name: "ReadFile", Input: "/tmp/a.txt", Result: "ok", Status: "success"},
			},
		),
	})

	// thoughts first, then toolCalls, then content
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Kind != ChatItemKindThinking {
		t.Errorf("items[0].Kind = %q, want thinking", items[0].Kind)
	}
	if items[0].ID != "gemini-think:0:0" {
		t.Errorf("items[0].ID = %q, want gemini-think:0:0", items[0].ID)
	}
	if items[1].Kind != ChatItemKindToolCall {
		t.Errorf("items[1].Kind = %q, want tool_call", items[1].Kind)
	}
	if items[1].ID != "gemini-tool:0:0" {
		t.Errorf("items[1].ID = %q, want gemini-tool:0:0", items[1].ID)
	}
	if items[2].Kind != ChatItemKindAssistant {
		t.Errorf("items[2].Kind = %q, want assistant_message", items[2].Kind)
	}
	if items[2].ID != "gemini-text:0" {
		t.Errorf("items[2].ID = %q, want gemini-text:0", items[2].ID)
	}
}

func TestGeminiParser_InfoMessage(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("info", "Session started", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-event:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-event:0")
	}
	if item.Kind != ChatItemKindSystemEvent {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindSystemEvent)
	}
	if item.Status != ChatItemStatusCompleted {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusCompleted)
	}
	if item.Message != "Session started" {
		t.Errorf("Message = %q, want %q", item.Message, "Session started")
	}
	if item.Code != "gemini_info" {
		t.Errorf("Code = %q, want %q", item.Code, "gemini_info")
	}
}

func TestGeminiParser_InfoMessageEmptyContent(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("info", "", 1717200000),
	})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty info message, got %d", len(items))
	}
}

func TestGeminiParser_ErrorMessage(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("error", "Something went wrong", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != "gemini-event:0" {
		t.Errorf("ID = %q, want %q", item.ID, "gemini-event:0")
	}
	if item.Kind != ChatItemKindSystemEvent {
		t.Errorf("Kind = %q, want %q", item.Kind, ChatItemKindSystemEvent)
	}
	if item.Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q", item.Status, ChatItemStatusFailed)
	}
	if item.Message != "Something went wrong" {
		t.Errorf("Message = %q, want %q", item.Message, "Something went wrong")
	}
	if item.Code != "gemini_error" {
		t.Errorf("Code = %q, want %q", item.Code, "gemini_error")
	}
}

func TestGeminiParser_ErrorMessageEmptyContent(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("error", "", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item for empty error (should emit with fallback), got %d", len(items))
	}
	if items[0].Message != "Error" {
		t.Errorf("Message = %q, want %q", items[0].Message, "Error")
	}
	if items[0].Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q", items[0].Status, ChatItemStatusFailed)
	}
}

func TestGeminiParser_UnknownType(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("unknown_type", "some content", 1717200000),
	})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for unknown type, got %d", len(items))
	}
}

func TestGeminiParser_EmptyMessages(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty messages, got %d", len(items))
	}
}

func TestGeminiParser_StableIDsAcrossRebuilds(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	messages := []geminiMessage{
		geminiMsg("user", "hello", 1717200000),
		geminiMsg("gemini", "world", 1717200001),
	}

	items1 := p.RebuildTimeline(messages)
	items2 := p.RebuildTimeline(messages)

	if len(items1) != len(items2) {
		t.Fatalf("rebuild lengths differ: %d vs %d", len(items1), len(items2))
	}
	for i := range items1 {
		if items1[i].ID != items2[i].ID {
			t.Errorf("items[%d].ID changed across rebuilds: %q → %q", i, items1[i].ID, items2[i].ID)
		}
	}
}

func TestGeminiParser_SeedConsumed(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("hello world", "req-abc", 1717200000000) // millis

	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello world", 1717200000), // seconds → 1717200000000 millis
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "gemini-user:req-abc" {
		t.Errorf("ID = %q, want %q", items[0].ID, "gemini-user:req-abc")
	}
	if items[0].RequestID != "req-abc" {
		t.Errorf("RequestID = %q, want %q", items[0].RequestID, "req-abc")
	}
	// Seed is preserved after consumption so subsequent rebuilds produce the same stable ID.
	if p.pendingSeed == nil {
		t.Error("pendingSeed should be preserved after consumption for stable IDs across rebuilds")
	}
}

func TestGeminiParser_SeedHistoricalTextSkipped(t *testing.T) {
	// Seed should NOT match a historical message whose timestamp is
	// before (seed.createdAt - 5000).
	p := newGeminiEventParser("sess-1")
	// Seed created at T=100000 (millis)
	p.SeedPrompt("continue", "req-new", 100000)

	items := p.RebuildTimeline([]geminiMessage{
		// Old "continue" at T=50 seconds → 50000 millis. This is < 100000 - 5000 = 95000
		geminiMsg("user", "continue", 50),
		// New "continue" at T=100 seconds → 100000 millis. This is >= 95000
		geminiMsg("user", "continue", 100),
	})

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// First message should have index-based ID (seed not consumed by old message).
	if items[0].ID != "gemini-user:0" {
		t.Errorf("items[0].ID = %q, want %q (old message should not consume seed)", items[0].ID, "gemini-user:0")
	}
	// Second message should consume the seed.
	if items[1].ID != "gemini-user:req-new" {
		t.Errorf("items[1].ID = %q, want %q (new message should consume seed)", items[1].ID, "gemini-user:req-new")
	}
	// Seed is preserved after consumption for stable IDs across rebuilds.
	if p.pendingSeed == nil {
		t.Error("pendingSeed should be preserved after consumption")
	}
}

func TestGeminiParser_SeedRollback(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-roll", 1000)

	if len(p.items) != 1 {
		t.Fatalf("expected 1 seeded item, got %d", len(p.items))
	}

	p.RollbackSeedItem("req-roll")

	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after rollback")
	}
	if len(p.items) != 0 {
		t.Errorf("expected 0 items after rollback, got %d", len(p.items))
	}
}

func TestGeminiParser_Reset(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("hello", "req-reset", 1000)
	p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello", 1717200000),
	})

	p.Reset()

	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after Reset")
	}
	if len(p.items) != 0 {
		t.Errorf("expected 0 items after Reset, got %d", len(p.items))
	}
}

func TestGeminiParser_ANSIStripping(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("gemini", "\x1b[31mred text\x1b[0m", 1717200001),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if strings.Contains(items[0].Markdown, "\x1b[") {
		t.Error("Markdown should not contain ANSI escape codes")
	}
	if !strings.Contains(items[0].Markdown, "red text") {
		t.Errorf("Markdown should contain 'red text', got %q", items[0].Markdown)
	}
}

func TestGeminiParser_SecretSanitization(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{
				Name:   "Bash",
				Input:  "API_TOKEN=mySecretToken123 ./run.sh",
				Result: "Authorization: Bearer sk-abc123secret",
				Status: "success",
			},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if strings.Contains(items[0].InputExcerpt, "mySecretToken123") {
		t.Error("InputExcerpt should not contain secret token value")
	}
	if !strings.Contains(items[0].InputExcerpt, "[REDACTED]") {
		t.Error("InputExcerpt should contain [REDACTED]")
	}
	if strings.Contains(items[0].ResultExcerpt, "sk-abc123secret") {
		t.Error("ResultExcerpt should not contain bearer token")
	}
	if !strings.Contains(items[0].ResultExcerpt, "[REDACTED]") {
		t.Error("ResultExcerpt should contain [REDACTED]")
	}
}

func TestGeminiParser_FullConversation(t *testing.T) {
	p := newGeminiEventParser("sess-full")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "list files", 1717200000),
		geminiMsgFull("Here are the files.", 1717200001,
			[]geminiThought{
				{Subject: "Plan", Description: "Use ls command"},
			},
			[]geminiToolCall{
				{Name: "Bash", Input: "ls", Result: "file1.txt\nfile2.txt", Status: "success"},
			},
		),
		geminiMsg("user", "thanks", 1717200002),
		geminiMsg("gemini", "You're welcome!", 1717200003),
	})

	// user(0) + think(1:0) + tool(1:0) + text(1) + user(2) + text(3)
	if len(items) != 6 {
		t.Fatalf("expected 6 items, got %d", len(items))
	}

	expectedKinds := []ChatItemKind{
		ChatItemKindUserMessage,
		ChatItemKindThinking,
		ChatItemKindToolCall,
		ChatItemKindAssistant,
		ChatItemKindUserMessage,
		ChatItemKindAssistant,
	}
	for i, ek := range expectedKinds {
		if items[i].Kind != ek {
			t.Errorf("items[%d].Kind = %q, want %q", i, items[i].Kind, ek)
		}
	}

	// Verify tool call is completed.
	toolItem := findChatItem(items, "gemini-tool:1:0")
	if toolItem == nil {
		t.Fatal("tool_call item not found")
	}
	if toolItem.Status != ChatItemStatusCompleted {
		t.Errorf("tool_call status = %q, want completed", toolItem.Status)
	}
}

func TestGeminiParser_TimestampZeroFallback(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "no timestamp", 0),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Should use the parser's stable fallback time (set at creation).
	if items[0].CreatedAt != p.fallbackTime {
		t.Errorf("CreatedAt = %d, expected parser fallbackTime %d", items[0].CreatedAt, p.fallbackTime)
	}

	// Verify stability: rebuilding again produces the same CreatedAt.
	items2 := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "no timestamp", 0),
	})
	if items2[0].CreatedAt != items[0].CreatedAt {
		t.Errorf("CreatedAt changed across rebuilds: %d → %d (should be stable)", items[0].CreatedAt, items2[0].CreatedAt)
	}
}

func TestGeminiParser_SeedPromptWithItem(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-seed", 1000)

	// Should have a seeded item immediately.
	items := p.timeline()
	if len(items) != 1 {
		t.Fatalf("expected 1 seeded item, got %d", len(items))
	}
	if items[0].ID != "gemini-user:req-seed" {
		t.Errorf("ID = %q, want %q", items[0].ID, "gemini-user:req-seed")
	}
	if items[0].Provider != ChatItemProviderGemini {
		t.Errorf("Provider = %q, want %q", items[0].Provider, ChatItemProviderGemini)
	}
	if items[0].RequestID != "req-seed" {
		t.Errorf("RequestID = %q, want %q", items[0].RequestID, "req-seed")
	}
	if p.pendingSeed == nil {
		t.Error("pendingSeed should be set after SeedPromptWithItem")
	}
}

func TestGeminiParser_ClearPendingSeed(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("hello", "req-clear", 1000)
	p.ClearPendingSeed()

	if p.pendingSeed != nil {
		t.Error("pendingSeed should be nil after ClearPendingSeed")
	}

	// JSONL event with the same text should get a fresh ID.
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "gemini-user:0" {
		t.Errorf("ID = %q, want %q (seed was cleared)", items[0].ID, "gemini-user:0")
	}
}

func TestGeminiParser_SeedNotConsumedOnMismatch(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("hello", "req-mismatch", 1717200000000)

	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "goodbye", 1717200000),
	})

	// 2 items: file message "goodbye" + re-injected seed "hello".
	if len(items) != 2 {
		t.Fatalf("expected 2 items (file + re-injected seed), got %d", len(items))
	}
	if items[0].ID != "gemini-user:0" {
		t.Errorf("ID = %q, want %q", items[0].ID, "gemini-user:0")
	}
	if items[1].ID != "gemini-user:req-mismatch" {
		t.Errorf("ID = %q, want %q (re-injected seed)", items[1].ID, "gemini-user:req-mismatch")
	}
	// Seed should still be pending.
	if p.pendingSeed == nil {
		t.Error("pendingSeed should still be pending after non-matching event")
	}
}

func TestGeminiParser_GeminiEmptyContent(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("gemini", "", 1717200001),
	})

	// No thoughts, no tools, empty content → 0 items.
	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty gemini content, got %d", len(items))
	}
}

func TestGeminiParser_ToolCallEmptyInput(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Input: "", Result: "output", Status: "success"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].InputExcerpt != "(none)" {
		t.Errorf("InputExcerpt = %q, want fallback %q for no input", items[0].InputExcerpt, "(none)")
	}
	if !strings.Contains(items[0].ResultExcerpt, "output") {
		t.Errorf("ResultExcerpt should contain 'output', got %q", items[0].ResultExcerpt)
	}
}

func TestGeminiParser_ToolCallEmptyResult(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Input: "ls", Result: "", Status: "success"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ResultExcerpt != "(pending)" {
		t.Errorf("ResultExcerpt = %q, want fallback %q for no result", items[0].ResultExcerpt, "(pending)")
	}
}

func TestGeminiParser_MultipleThoughts(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithThoughts("", 1717200001, []geminiThought{
			{Subject: "First", Description: "thought one"},
			{Subject: "", Description: ""},
			{Subject: "Third", Description: "thought three"},
		}),
	})

	// Second thought is empty → skipped.
	if len(items) != 2 {
		t.Fatalf("expected 2 items (empty skipped), got %d", len(items))
	}
	if items[0].ID != "gemini-think:0:0" {
		t.Errorf("items[0].ID = %q, want gemini-think:0:0", items[0].ID)
	}
	if items[1].ID != "gemini-think:0:2" {
		t.Errorf("items[1].ID = %q, want gemini-think:0:2", items[1].ID)
	}
}

func TestGeminiParser_MultipleToolCalls(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Read", Input: "/a.txt", Status: "success"},
			{Name: "Write", Input: "/b.txt", Status: "error"},
			{Name: "Bash", Input: "echo hi", Status: ""},
		}),
	})

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Status != ChatItemStatusCompleted {
		t.Errorf("items[0].Status = %q, want completed", items[0].Status)
	}
	if items[1].Status != ChatItemStatusFailed {
		t.Errorf("items[1].Status = %q, want failed", items[1].Status)
	}
	if items[2].Status != ChatItemStatusInProgress {
		t.Errorf("items[2].Status = %q, want in_progress", items[2].Status)
	}
}

func TestGeminiParser_UserEmptyContentSkipped(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "", 1717200000),
	})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for empty user content, got %d", len(items))
	}
}

func TestGeminiParser_UserWhitespaceContentSkipped(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "   \n\t  ", 1717200000),
	})

	if len(items) != 0 {
		t.Fatalf("expected 0 items for whitespace-only user content, got %d", len(items))
	}
}

func TestGeminiParser_InputExcerptTruncation(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	longInput := strings.Repeat("A", 600)
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Input: longInput, Status: "success"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	runeCount := 0
	for range items[0].InputExcerpt {
		runeCount++
	}
	if runeCount > 500 {
		t.Errorf("InputExcerpt has %d runes, want <= 500", runeCount)
	}
}

func TestGeminiParser_SeedConsumedOnlyOnce(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	// Seed created at T=100s (millis=100000)
	p.SeedPrompt("yes", "req-once", 100000)

	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "yes", 100), // 100s → 100000ms, >= 100000-5000=95000 ✓
		geminiMsg("user", "yes", 101), // 101s → 101000ms
	})

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// First "yes" consumes the seed.
	if items[0].ID != "gemini-user:req-once" {
		t.Errorf("items[0].ID = %q, want seeded %q", items[0].ID, "gemini-user:req-once")
	}
	// Second "yes" gets index-based ID.
	if items[1].ID != "gemini-user:1" {
		t.Errorf("items[1].ID = %q, want fresh %q", items[1].ID, "gemini-user:1")
	}
}

func TestGeminiParser_SeedPreservedAcrossRebuilds(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("hello", "req-rebuild", 1717200000000)

	messages := []geminiMessage{
		geminiMsg("user", "hello", 1717200000),
	}

	// First rebuild: seed consumed.
	items1 := p.RebuildTimeline(messages)
	if items1[0].ID != "gemini-user:req-rebuild" {
		t.Errorf("first rebuild: ID = %q, want seeded", items1[0].ID)
	}

	// Second rebuild: seed preserved, same stable ID produced.
	items2 := p.RebuildTimeline(messages)
	if items2[0].ID != "gemini-user:req-rebuild" {
		t.Errorf("second rebuild: ID = %q, want stable seeded %q", items2[0].ID, "gemini-user:req-rebuild")
	}
}

func TestGeminiParser_ToolCallUnknownStatus(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsgWithTools("", 1717200001, []geminiToolCall{
			{Name: "Bash", Status: "unknown_status"},
		}),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != ChatItemStatusFailed {
		t.Errorf("Status = %q, want %q for unknown status", items[0].Status, ChatItemStatusFailed)
	}
}

func TestGeminiParser_MessageIndexInIDs(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("info", "start", 1717200000),
		geminiMsg("user", "hello", 1717200001),
		geminiMsg("gemini", "world", 1717200002),
		geminiMsg("error", "oops", 1717200003),
	})

	// info:0, user:1, text:2, event:3
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	expectedIDs := []string{
		"gemini-event:0",
		"gemini-user:1",
		"gemini-text:2",
		"gemini-event:3",
	}
	for i, eid := range expectedIDs {
		if items[i].ID != eid {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].ID, eid)
		}
	}
}

func TestGeminiParser_RebuildClearsItems(t *testing.T) {
	p := newGeminiEventParser("sess-1")

	// First rebuild with 2 messages.
	items1 := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello", 1717200000),
		geminiMsg("gemini", "world", 1717200001),
	})
	if len(items1) != 2 {
		t.Fatalf("first rebuild: expected 2 items, got %d", len(items1))
	}

	// Second rebuild with only 1 message — items should not accumulate.
	items2 := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "just one", 1717200002),
	})
	if len(items2) != 1 {
		t.Fatalf("second rebuild: expected 1 item, got %d (items accumulated?)", len(items2))
	}
}

func TestGeminiParser_SeedTimestampWindowEdge(t *testing.T) {
	// Seed at T=100000ms. Window: msg.timestamp >= 100000 - 5000 = 95000.
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("test", "req-edge", 100000)

	// Message at exactly the boundary: 95s → 95000ms.
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "test", 95), // 95*1000 = 95000 ≥ 95000 ✓
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "gemini-user:req-edge" {
		t.Errorf("ID = %q, want seeded %q (edge of window should match)", items[0].ID, "gemini-user:req-edge")
	}
}

func TestGeminiParser_SeedTimestampJustOutsideWindow(t *testing.T) {
	// Seed at T=100000ms. Window: >= 95000.
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("test", "req-outside", 100000)

	// Message at 94.999s → 94999ms < 95000.
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "test", 94.999),
	})

	// 2 items: file message + re-injected seed (seed timestamp 100000 > file 94999).
	if len(items) != 2 {
		t.Fatalf("expected 2 items (file + re-injected seed), got %d", len(items))
	}
	if items[0].ID != "gemini-user:0" {
		t.Errorf("ID = %q, want index-based %q (outside window)", items[0].ID, "gemini-user:0")
	}
	if items[1].ID != "gemini-user:req-outside" {
		t.Errorf("ID = %q, want %q (re-injected seed)", items[1].ID, "gemini-user:req-outside")
	}
	// Seed should still be pending (not consumed).
	if p.pendingSeed == nil {
		t.Error("pendingSeed should still be pending")
	}
}

func TestGeminiParser_AllProviderFieldsSet(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hi", 1717200000),
		geminiMsgFull("response", 1717200001,
			[]geminiThought{{Subject: "S", Description: "D"}},
			[]geminiToolCall{{Name: "T", Status: "success"}},
		),
		geminiMsg("info", "event", 1717200002),
		geminiMsg("error", "err", 1717200003),
	})

	for i, item := range items {
		if item.Provider != ChatItemProviderGemini {
			t.Errorf("items[%d].Provider = %q, want gemini", i, item.Provider)
		}
		if item.SessionID != "sess-1" {
			t.Errorf("items[%d].SessionID = %q, want sess-1", i, item.SessionID)
		}
	}
}

func TestGeminiParser_UserTrimSpace(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "  hello world  ", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Text != "hello world" {
		t.Errorf("Text = %q, want %q", items[0].Text, "hello world")
	}
}

func TestGeminiParser_SeedTrimSpaceMatch(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPrompt("  hello  ", "req-trim", 1717200000000)

	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "  hello  ", 1717200000),
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "gemini-user:req-trim" {
		t.Errorf("ID = %q, want seeded (trimmed match)", items[0].ID)
	}
}

func TestGeminiParser_SeedSurvivesRebuildWithoutFileMessage(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-survive", 1717200005000)

	// Rebuild with empty messages — file hasn't caught up yet.
	items := p.RebuildTimeline([]geminiMessage{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item (seed re-injected), got %d", len(items))
	}
	if items[0].ID != "gemini-user:req-survive" {
		t.Errorf("ID = %q, want %q", items[0].ID, "gemini-user:req-survive")
	}
	if items[0].Text != "hello" {
		t.Errorf("Text = %q, want %q", items[0].Text, "hello")
	}

	// Rebuild again with the user message now in the file.
	items2 := p.RebuildTimeline([]geminiMessage{
		geminiMsg("user", "hello", 1717200005), // 1717200005000ms matches seed
	})
	if len(items2) != 1 {
		t.Fatalf("expected 1 item (seed consumed), got %d", len(items2))
	}
	if items2[0].ID != "gemini-user:req-survive" {
		t.Errorf("ID = %q, want seeded %q", items2[0].ID, "gemini-user:req-survive")
	}
	// Seed is preserved after consumption for stable IDs across rebuilds.
	if p.pendingSeed == nil {
		t.Error("pendingSeed should be preserved after consumption")
	}
}

func TestGeminiParser_SeedSurvivesRebuildWithOtherMessages(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-other", 1717200010000)

	// Rebuild with a gemini message but no user message — seed should survive.
	items := p.RebuildTimeline([]geminiMessage{
		geminiMsg("gemini", "I'm exploring the codebase.", 1717200005),
	})

	// Should have gemini item + re-injected seed.
	if len(items) != 2 {
		t.Fatalf("expected 2 items (gemini + seed), got %d", len(items))
	}
	// Gemini message at T=5s, seed at T=10s → gemini first, seed second.
	if items[0].Kind != ChatItemKindAssistant {
		t.Errorf("items[0].Kind = %q, want assistant", items[0].Kind)
	}
	if items[1].ID != "gemini-user:req-other" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "gemini-user:req-other")
	}
	if items[1].Text != "hello" {
		t.Errorf("items[1].Text = %q, want %q", items[1].Text, "hello")
	}
}

func TestGeminiParser_SeedNotReinjectedAfterRollback(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-rollback", 1000)

	p.RollbackSeedItem("req-rollback")

	// Rebuild with empty messages — seed was rolled back, nothing to re-inject.
	items := p.RebuildTimeline([]geminiMessage{})
	if len(items) != 0 {
		t.Fatalf("expected 0 items after rollback + rebuild, got %d", len(items))
	}
}

func TestGeminiParser_SeedNotReinjectedAfterReset(t *testing.T) {
	p := newGeminiEventParser("sess-1")
	p.SeedPromptWithItem("hello", "req-reset2", 1000)

	p.Reset()

	// Rebuild with empty messages — seed was reset, nothing to re-inject.
	items := p.RebuildTimeline([]geminiMessage{})
	if len(items) != 0 {
		t.Fatalf("expected 0 items after reset + rebuild, got %d", len(items))
	}
}

func TestGeminiParser_LargeConversation(t *testing.T) {
	p := newGeminiEventParser("sess-large")
	var messages []geminiMessage
	for i := 0; i < 50; i++ {
		messages = append(messages,
			geminiMsg("user", fmt.Sprintf("question %d", i), float64(1717200000+i*2)),
			geminiMsg("gemini", fmt.Sprintf("answer %d", i), float64(1717200001+i*2)),
		)
	}

	items := p.RebuildTimeline(messages)
	if len(items) != 100 {
		t.Fatalf("expected 100 items, got %d", len(items))
	}

	// Verify first and last items.
	if items[0].Text != "question 0" {
		t.Errorf("first item text = %q", items[0].Text)
	}
	if items[99].Markdown != "answer 49" {
		t.Errorf("last item markdown = %q", items[99].Markdown)
	}
}
