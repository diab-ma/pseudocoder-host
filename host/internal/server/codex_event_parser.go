package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// codexResponseItem is the outer envelope for a Codex response_item event.
type codexResponseItem struct {
	Type string `json:"type"` // "message", "function_call", "function_call_output", "reasoning"
	// message fields (when type == "message")
	Role    string              `json:"role,omitempty"` // "user", "assistant", "developer"
	ID      string              `json:"id,omitempty"`
	Content []codexContentBlock `json:"content,omitempty"`
	// function_call fields
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output fields (also uses CallID, Output)
	Output string `json:"output,omitempty"`
	// reasoning fields
	Summary []codexReasoningSummary `json:"summary,omitempty"`
}

type codexContentBlock struct {
	Type string `json:"type"` // "input_text", "output_text"
	Text string `json:"text"`
}

type codexReasoningSummary struct {
	Type string `json:"type"` // "summary_text" or other
	Text string `json:"text,omitempty"`
}

// codexEvent represents a single JSONL line from a Codex session file.
type codexEvent struct {
	Timestamp float64         `json:"-"`
	Type      string          `json:"type"` // "response_item", "event_msg", "session_meta", "turn_context"
	Payload   json.RawMessage `json:"payload"`
}

func (e codexEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Timestamp float64         `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}{
		Timestamp: e.Timestamp,
		Type:      e.Type,
		Payload:   e.Payload,
	})
}

func (e *codexEvent) UnmarshalJSON(data []byte) error {
	var raw struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	timestamp, err := parseFlexibleTimestampSeconds(raw.Timestamp)
	if err != nil {
		return err
	}

	e.Timestamp = timestamp
	e.Type = raw.Type
	e.Payload = raw.Payload
	return nil
}

// codexEventMsg is the payload for type "event_msg".
type codexEventMsg struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"` // "task_started", "task_complete", etc.
}

// codexEventParser transforms Codex JSONL events into ChatItem slices.
// It maintains state to correlate function_call and function_call_output events.
// File order is authoritative — items are never re-sorted by timestamp.
type codexEventParser struct {
	sessionID       string
	items           []ChatItem
	toolItemIndex   map[string]int // call_id → index in items
	lastCreatedAt   int64
	pendingSeed     *jsonlPendingSeed // reuse the same seed type
	seenTaskStarted bool              // gate: skip assistant messages before task_started
	seq             int               // monotonic counter for synthesized IDs when source ID fields are empty
}

func newCodexEventParser(sessionID string) *codexEventParser {
	return &codexEventParser{
		sessionID:     sessionID,
		toolItemIndex: make(map[string]int),
	}
}

// ParseEvent processes a single Codex JSONL line and returns the updated timeline.
func (p *codexEventParser) ParseEvent(line []byte) []ChatItem {
	var event codexEvent
	if err := json.Unmarshal(line, &event); err != nil {
		log.Printf("codex_event_parser: skipping malformed JSON line: %v", err)
		return p.timeline()
	}

	createdAt := p.parseTimestamp(event.Timestamp)
	p.lastCreatedAt = createdAt

	switch event.Type {
	case "response_item":
		p.processResponseItem(event, createdAt)
	case "event_msg":
		p.processEventMsg(event, createdAt)
	case "session_meta", "turn_context":
		// Intentionally skipped.
	default:
		log.Printf("codex_event_parser: skipping unknown event type %q", event.Type)
	}

	return p.timeline()
}

// SeedPrompt registers a pending seed for prompt deduplication.
func (p *codexEventParser) SeedPrompt(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
}

// ClearPendingSeed removes any unconsumed pending seed.
func (p *codexEventParser) ClearPendingSeed() {
	p.pendingSeed = nil
}

// SeedPromptWithItem registers a pending seed AND adds a seeded user item to
// the timeline immediately with Provider=ChatItemProviderCodex.
func (p *codexEventParser) SeedPromptWithItem(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
	p.items = append(p.items, ChatItem{
		ID:        "codex-user:" + requestID,
		SessionID: p.sessionID,
		Kind:      ChatItemKindUserMessage,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		RequestID: requestID,
		Text:      text,
	})
}

// RollbackSeedItem clears the pending seed and removes the seeded user item
// from the timeline. Called when the write fails after seeding.
func (p *codexEventParser) RollbackSeedItem(requestID string) {
	p.ClearPendingSeed()
	targetID := "codex-user:" + requestID
	for i, item := range p.items {
		if item.ID == targetID {
			p.items = append(p.items[:i], p.items[i+1:]...)
			break
		}
	}
}

// Reset clears all parser state for a fresh replay.
func (p *codexEventParser) Reset() {
	p.items = nil
	p.toolItemIndex = make(map[string]int)
	p.lastCreatedAt = 0
	p.pendingSeed = nil
	p.seenTaskStarted = false
	p.seq = 0
}

func (p *codexEventParser) timeline() []ChatItem {
	return cloneChatItems(p.items)
}

// addOrReplaceItem adds an item or replaces an existing item with the same ID.
func (p *codexEventParser) addOrReplaceItem(item ChatItem) {
	for i, existing := range p.items {
		if existing.ID == item.ID {
			p.items[i] = item
			return
		}
	}
	p.items = append(p.items, item)
}

// itemID returns id unchanged if non-empty; otherwise increments the shared
// sequence counter and returns a unique synthetic ID.
func (p *codexEventParser) itemID(id string) string {
	if id != "" {
		return id
	}
	p.seq++
	return fmt.Sprintf("seq%d", p.seq)
}

// consumeSeedOrFreshID checks if the given text matches an unconsumed pending
// seed. If so, the seed is consumed and the seed's request ID is returned;
// otherwise a fresh ID based on the event ID is used. Returns (id, requestID).
func (p *codexEventParser) consumeSeedOrFreshID(text, eventID string) (string, string) {
	if p.pendingSeed != nil && p.pendingSeed.text == text {
		id := "codex-user:" + p.pendingSeed.requestID
		requestID := p.pendingSeed.requestID
		p.pendingSeed = nil // consumed
		return id, requestID
	}
	return "codex-user:" + p.itemID(eventID), ""
}

// parseTimestamp converts a Codex Unix-seconds float64 to Unix milliseconds.
// Falls back to max(lastCreatedAt+1, time.Now().UnixMilli()) if 0.
func (p *codexEventParser) parseTimestamp(ts float64) int64 {
	if ts == 0 {
		fallback := maxInt64(p.lastCreatedAt+1, time.Now().UnixMilli())
		log.Printf("codex_event_parser: missing timestamp, using fallback %d", fallback)
		return fallback
	}
	return int64(ts * 1000)
}

// processResponseItem handles "response_item" type events by dispatching on the
// payload's inner type field.
func (p *codexEventParser) processResponseItem(event codexEvent, createdAt int64) {
	var item codexResponseItem
	if err := json.Unmarshal(event.Payload, &item); err != nil {
		log.Printf("codex_event_parser: skipping malformed response_item payload: %v", err)
		return
	}

	switch item.Type {
	case "message":
		p.processMessage(item, createdAt)
	case "function_call":
		p.processFunctionCall(item, createdAt)
	case "function_call_output":
		p.processFunctionCallOutput(item, createdAt)
	case "reasoning":
		p.processReasoning(item, createdAt)
	default:
		log.Printf("codex_event_parser: skipping unknown response_item type %q", item.Type)
	}
}

// processMessage handles response_item payloads with type "message".
func (p *codexEventParser) processMessage(item codexResponseItem, createdAt int64) {
	switch item.Role {
	case "user":
		p.processUserMessage(item, createdAt)
	case "assistant":
		p.processAssistantMessage(item, createdAt)
	case "developer":
		// Intentionally skipped.
	default:
		log.Printf("codex_event_parser: skipping message with unknown role %q", item.Role)
	}
}

// processUserMessage extracts input_text content blocks and creates a UserMessage item.
func (p *codexEventParser) processUserMessage(item codexResponseItem, createdAt int64) {
	var parts []string
	for _, block := range item.Content {
		if block.Type == "input_text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return
	}

	text := strings.Join(parts, "\n")
	id, requestID := p.consumeSeedOrFreshID(text, item.ID)
	// Before task_started, only keep seeded user messages (sent from mobile).
	// Non-seeded user messages are system context (AGENTS.md, environment)
	// that Codex records before the task begins.
	if !p.seenTaskStarted && requestID == "" {
		return
	}
	p.addOrReplaceItem(ChatItem{
		ID:        id,
		SessionID: p.sessionID,
		Kind:      ChatItemKindUserMessage,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		RequestID: requestID,
		Text:      text,
	})
}

// processAssistantMessage creates one Assistant item per output_text block.
// Assistant messages arriving before task_started (e.g. AGENTS.md dumps) are
// silently dropped so they never reach the mobile timeline.
func (p *codexEventParser) processAssistantMessage(item codexResponseItem, createdAt int64) {
	if !p.seenTaskStarted {
		return
	}
	resolvedID := p.itemID(item.ID)
	for i, block := range item.Content {
		if block.Type != "output_text" || block.Text == "" {
			continue
		}
		text := stripANSIForChat(block.Text)
		p.items = append(p.items, ChatItem{
			ID:        fmt.Sprintf("codex-text:%s:%d", resolvedID, i),
			SessionID: p.sessionID,
			Kind:      ChatItemKindAssistant,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Markdown:  text,
		})
	}
}

// processFunctionCall creates an InProgress ToolCall item, tracked by call_id.
func (p *codexEventParser) processFunctionCall(item codexResponseItem, createdAt int64) {
	inputExcerpt := makeExcerpt(json.RawMessage(item.Arguments))
	toolItem := ChatItem{
		ID:           "codex-tool:" + item.CallID,
		SessionID:    p.sessionID,
		Kind:         ChatItemKindToolCall,
		CreatedAt:    createdAt,
		Provider:     ChatItemProviderCodex,
		Status:       ChatItemStatusInProgress,
		ToolName:     item.Name,
		Summary:      item.Name,
		InputExcerpt: inputExcerpt,
		StartedAt:    createdAt,
	}
	p.toolItemIndex[item.CallID] = len(p.items)
	p.items = append(p.items, toolItem)
}

// processFunctionCallOutput updates an existing ToolCall to Completed, or creates
// a synthetic one with ToolName="unknown" if unmatched.
func (p *codexEventParser) processFunctionCallOutput(item codexResponseItem, createdAt int64) {
	resultExcerpt := sanitizeToolExcerpt(item.Output)

	if idx, ok := p.toolItemIndex[item.CallID]; ok && idx < len(p.items) {
		p.items[idx].Status = ChatItemStatusCompleted
		p.items[idx].ResultExcerpt = resultExcerpt
		p.items[idx].CompletedAt = createdAt
		return
	}

	// Unmatched function_call_output: create synthetic tool_call.
	p.items = append(p.items, ChatItem{
		ID:            "codex-tool:" + item.CallID,
		SessionID:     p.sessionID,
		Kind:          ChatItemKindToolCall,
		CreatedAt:     createdAt,
		Provider:      ChatItemProviderCodex,
		Status:        ChatItemStatusCompleted,
		ToolName:      "unknown",
		Summary:       "unknown",
		ResultExcerpt: resultExcerpt,
		CompletedAt:   createdAt,
	})
}

// processReasoning creates Thinking items from summary entries with type "summary_text".
func (p *codexEventParser) processReasoning(item codexResponseItem, createdAt int64) {
	resolvedID := p.itemID(item.ID)
	for i, s := range item.Summary {
		if s.Type != "summary_text" || s.Text == "" {
			continue
		}
		p.items = append(p.items, ChatItem{
			ID:        fmt.Sprintf("codex-think:%s:%d", resolvedID, i),
			SessionID: p.sessionID,
			Kind:      ChatItemKindThinking,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Text:      s.Text,
		})
	}
}

// processEventMsg handles "event_msg" type events.
func (p *codexEventParser) processEventMsg(event codexEvent, createdAt int64) {
	var msg codexEventMsg
	if err := json.Unmarshal(event.Payload, &msg); err != nil {
		log.Printf("codex_event_parser: skipping malformed event_msg payload: %v", err)
		return
	}

	// Codex CLI may leave event_id empty; generate a unique fallback using
	// a sequence counter so multi-turn sessions don't produce duplicate IDs.
	eventID := msg.EventID
	if eventID == "" {
		p.seq++
		eventID = fmt.Sprintf("%s-%d", msg.Type, p.seq)
	}

	switch msg.Type {
	case "task_started":
		p.seenTaskStarted = true
		p.items = append(p.items, ChatItem{
			ID:        "codex-event:" + eventID,
			SessionID: p.sessionID,
			Kind:      ChatItemKindSystemEvent,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Code:      "task_started",
			Message:   "Task started",
		})
	case "task_complete":
		p.items = append(p.items, ChatItem{
			ID:        "codex-event:" + eventID,
			SessionID: p.sessionID,
			Kind:      ChatItemKindSystemEvent,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Code:      "task_complete",
			Message:   "Task complete",
		})
	default:
		// Other event_msg types are skipped.
		log.Printf("codex_event_parser: skipping event_msg type %q", msg.Type)
	}
}
