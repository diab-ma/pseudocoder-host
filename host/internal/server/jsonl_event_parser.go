package server

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// jsonSecretKeyPattern matches JSON key-value pairs where the key contains
// sensitive words. Values are redacted to prevent secret leakage in excerpts.
// Matches both string and non-string values after the key.
var jsonSecretKeyPattern = regexp.MustCompile(
	`(?i)"([^"]*(?:api_key|apikey|secret|token|password|credential|private_key|access_key)[^"]*)"\s*:\s*("(?:[^"\\]|\\.)*"|[^\s,}\]]+)`,
)

// jsonlEvent represents a single line from a Claude Code JSONL session file.
type jsonlEvent struct {
	UUID      string          `json:"uuid"`
	Type      string          `json:"type"`    // "human"/"user" or "assistant" or "system"
	Subtype   string          `json:"subtype"` // for system events
	Message   jsonlMessage    `json:"message"`
	Timestamp json.RawMessage `json:"timestamp"` // RFC3339 string or Unix millis number
}

// jsonlMessage wraps the Anthropic API message format.
type jsonlMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // plain string or []jsonlContentBlock
}

// jsonlContentBlock represents a single content block within a message.
type jsonlContentBlock struct {
	Type      string          `json:"type"`                 // "text", "thinking", "tool_use", "tool_result"
	Text      string          `json:"text,omitempty"`       // text block content
	Thinking  string          `json:"thinking,omitempty"`   // thinking block content
	ID        string          `json:"id,omitempty"`         // tool_use id
	Name      string          `json:"name,omitempty"`       // tool name
	Input     json.RawMessage `json:"input,omitempty"`      // tool_use input JSON
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content (string or blocks)
	IsError   bool            `json:"is_error,omitempty"`
}

// jsonlPendingSeed holds the state for a prompt seeded by the mobile client.
// When a JSONL user event arrives with matching text, the seed is consumed
// and the ChatItem reuses the seed's request ID for deduplication.
type jsonlPendingSeed struct {
	requestID string
	text      string
	createdAt int64
}

// jsonlEventParser transforms JSONL events into ChatItem slices.
// It maintains state to correlate tool_use and tool_result events across
// assistant and user messages. File order is authoritative — items are never
// re-sorted by timestamp.
type jsonlEventParser struct {
	sessionID     string
	items         []ChatItem
	toolItemIndex map[string]int // tool_use_id → index in items slice
	lastCreatedAt int64

	// pendingSeed is set by SeedPrompt and consumed when a matching JSONL
	// user event arrives. nil means no seed is pending.
	pendingSeed *jsonlPendingSeed
}

func newJSONLEventParser(sessionID string) *jsonlEventParser {
	return &jsonlEventParser{
		sessionID:     sessionID,
		toolItemIndex: make(map[string]int),
	}
}

// ParseEvent processes a single JSONL line and returns the updated timeline.
// Unknown event types and content block types are skipped with a log warning.
func (p *jsonlEventParser) ParseEvent(line []byte) []ChatItem {
	var event jsonlEvent
	if err := json.Unmarshal(line, &event); err != nil {
		log.Printf("jsonl_event_parser: skipping malformed JSON line: %v", err)
		return p.timeline()
	}

	createdAt := p.parseTimestamp(event.Timestamp)
	p.lastCreatedAt = createdAt

	switch event.Type {
	case "human", "user":
		p.processUserEvent(event, createdAt)
	case "assistant":
		p.processAssistantEvent(event, createdAt)
	case "system":
		// Unknown system subtypes are skipped per spec 03.
		log.Printf("jsonl_event_parser: skipping system event subtype=%q", event.Subtype)
	default:
		log.Printf("jsonl_event_parser: skipping unknown event type %q", event.Type)
	}

	return p.timeline()
}

// SeedPrompt registers a pending seed for prompt deduplication. When the next
// JSONL user event has text matching the seed, the ChatItem reuses the seed's
// request ID (producing ID "jsonl-user:<requestID>") and the seed is consumed.
// Only one seed can be pending at a time; calling SeedPrompt replaces any
// existing unconsumed seed.
func (p *jsonlEventParser) SeedPrompt(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
}

// ClearPendingSeed removes any unconsumed pending seed. Called by RollbackPrompt,
// provider_reset, and adapter_downgrade to prevent ghost bubbles.
func (p *jsonlEventParser) ClearPendingSeed() {
	p.pendingSeed = nil
}

// SeedPromptWithItem registers a pending seed AND adds a seeded user item to
// the timeline. This allows the mobile client to see the message immediately
// before the JSONL echo arrives. When the JSONL user event arrives with matching
// text, consumeSeedOrFreshID reuses the same ID and addOrReplaceItem updates
// the existing item in place.
func (p *jsonlEventParser) SeedPromptWithItem(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
	p.items = append(p.items, ChatItem{
		ID:        "jsonl-user:" + requestID,
		SessionID: p.sessionID,
		Kind:      ChatItemKindUserMessage,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderClaude,
		Status:    ChatItemStatusCompleted,
		RequestID: requestID,
		Text:      text,
	})
}

// RollbackSeedItem clears the pending seed and removes the seeded user item
// from the timeline. Called when the PTY write fails after seeding.
func (p *jsonlEventParser) RollbackSeedItem(requestID string) {
	p.ClearPendingSeed()
	targetID := "jsonl-user:" + requestID
	for i, item := range p.items {
		if item.ID == targetID {
			p.items = append(p.items[:i], p.items[i+1:]...)
			break
		}
	}
}

// addOrReplaceItem adds an item or replaces an existing item with the same ID.
// This is used by processUserEvent to update seeded items when the JSONL echo arrives.
func (p *jsonlEventParser) addOrReplaceItem(item ChatItem) {
	for i, existing := range p.items {
		if existing.ID == item.ID {
			p.items[i] = item
			return
		}
	}
	p.items = append(p.items, item)
}

// Reset clears all parser state for a fresh replay (e.g. after provider_reset).
func (p *jsonlEventParser) Reset() {
	p.items = nil
	p.toolItemIndex = make(map[string]int)
	p.lastCreatedAt = 0
	p.pendingSeed = nil
}

// parseTimestamp extracts a Unix-millis timestamp from the raw JSON value.
// Supports RFC3339 strings and numeric Unix-millis. Falls back to
// max(prev_created_at+1, time.Now().UnixMilli()) on missing or malformed input.
func (p *jsonlEventParser) parseTimestamp(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		fallback := maxInt64(p.lastCreatedAt+1, time.Now().UnixMilli())
		log.Printf("jsonl_event_parser: missing timestamp, using fallback %d", fallback)
		return fallback
	}

	// Try as RFC3339 string first (most common in Claude Code JSONL).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli()
		}
		fallback := maxInt64(p.lastCreatedAt+1, time.Now().UnixMilli())
		log.Printf("jsonl_event_parser: malformed timestamp %q, using fallback %d", s, fallback)
		return fallback
	}

	// Try as numeric Unix milliseconds.
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return int64(n)
	}

	fallback := maxInt64(p.lastCreatedAt+1, time.Now().UnixMilli())
	log.Printf("jsonl_event_parser: unparseable timestamp, using fallback %d", fallback)
	return fallback
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// consumeSeedOrFreshID checks if the given text matches an unconsumed pending
// seed. If so, the seed is consumed and the seed's request ID is returned;
// otherwise a fresh ID based on the event UUID is used. Returns (id, requestID).
func (p *jsonlEventParser) consumeSeedOrFreshID(text, eventUUID string) (string, string) {
	if p.pendingSeed != nil && p.pendingSeed.text == text {
		id := "jsonl-user:" + p.pendingSeed.requestID
		requestID := p.pendingSeed.requestID
		p.pendingSeed = nil // consumed
		return id, requestID
	}
	return "jsonl-user:" + eventUUID, ""
}

// processUserEvent handles "human"/"user" type JSONL events.
// Plain text content produces a user_message; tool_result blocks update or
// create tool_call items. If a pending seed matches the user text, the seed's
// request ID is used for deduplication (spec 05).
func (p *jsonlEventParser) processUserEvent(event jsonlEvent, createdAt int64) {
	blocks := p.parseContentBlocks(event.Message.Content)

	if len(blocks) == 0 {
		// Content might be a plain string (simple user messages).
		var text string
		if err := json.Unmarshal(event.Message.Content, &text); err == nil && text != "" {
			id, requestID := p.consumeSeedOrFreshID(text, event.UUID)
			// addOrReplaceItem handles the case where a seeded item with the
			// same ID already exists — the JSONL echo replaces it in place.
			p.addOrReplaceItem(ChatItem{
				ID:        id,
				SessionID: p.sessionID,
				Kind:      ChatItemKindUserMessage,
				CreatedAt: createdAt,
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				RequestID: requestID,
				Text:      text,
			})
		}
		return
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			id, requestID := p.consumeSeedOrFreshID(block.Text, event.UUID)
			p.addOrReplaceItem(ChatItem{
				ID:        id,
				SessionID: p.sessionID,
				Kind:      ChatItemKindUserMessage,
				CreatedAt: createdAt,
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				RequestID: requestID,
				Text:      block.Text,
			})
		case "tool_result":
			p.processToolResult(block, createdAt)
		default:
			log.Printf("jsonl_event_parser: skipping unknown content block type %q in user event", block.Type)
		}
	}
}

// processAssistantEvent handles "assistant" type JSONL events.
// Content blocks are emitted in array order, all sharing the parent event's
// created_at timestamp.
func (p *jsonlEventParser) processAssistantEvent(event jsonlEvent, createdAt int64) {
	blocks := p.parseContentBlocks(event.Message.Content)

	for i, block := range blocks {
		switch block.Type {
		case "text":
			p.items = append(p.items, ChatItem{
				ID:        fmt.Sprintf("jsonl-text:%s:%d", event.UUID, i),
				SessionID: p.sessionID,
				Kind:      ChatItemKindAssistant,
				CreatedAt: createdAt,
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				Markdown:  block.Text,
			})
		case "thinking":
			p.items = append(p.items, ChatItem{
				ID:        fmt.Sprintf("jsonl-think:%s:%d", event.UUID, i),
				SessionID: p.sessionID,
				Kind:      ChatItemKindThinking,
				CreatedAt: createdAt,
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				Text:      block.Thinking,
			})
		case "tool_use":
			inputExcerpt := makeExcerpt(block.Input)
			item := ChatItem{
				ID:           "jsonl-tool:" + block.ID,
				SessionID:    p.sessionID,
				Kind:         ChatItemKindToolCall,
				CreatedAt:    createdAt,
				Provider:     ChatItemProviderClaude,
				Status:       ChatItemStatusInProgress,
				ToolName:     block.Name,
				Summary:      block.Name,
				InputExcerpt: inputExcerpt,
				StartedAt:    createdAt,
			}
			p.toolItemIndex[block.ID] = len(p.items)
			p.items = append(p.items, item)
		default:
			log.Printf("jsonl_event_parser: skipping unknown content block type %q in assistant event", block.Type)
		}
	}
}

// processToolResult updates an existing tool_call or creates a synthetic one
// for unmatched tool_results.
func (p *jsonlEventParser) processToolResult(block jsonlContentBlock, createdAt int64) {
	status := ChatItemStatusCompleted
	if block.IsError {
		status = ChatItemStatusFailed
	}

	resultExcerpt := makeResultExcerpt(block.Content)

	if idx, ok := p.toolItemIndex[block.ToolUseID]; ok && idx < len(p.items) {
		// Update the existing tool_call with result information.
		p.items[idx].Status = status
		p.items[idx].IsError = block.IsError
		p.items[idx].ResultExcerpt = resultExcerpt
		p.items[idx].CompletedAt = createdAt
		return
	}

	// Unmatched tool_result: create synthetic tool_call with tool_name="unknown".
	p.items = append(p.items, ChatItem{
		ID:            "jsonl-tool:" + block.ToolUseID,
		SessionID:     p.sessionID,
		Kind:          ChatItemKindToolCall,
		CreatedAt:     createdAt,
		Provider:      ChatItemProviderClaude,
		Status:        status,
		ToolName:      "unknown",
		Summary:       "unknown",
		IsError:       block.IsError,
		ResultExcerpt: resultExcerpt,
		CompletedAt:   createdAt,
	})
}

// parseContentBlocks attempts to unmarshal the content field as an array of
// content blocks. Returns nil if content is a plain string or unparseable.
func (p *jsonlEventParser) parseContentBlocks(raw json.RawMessage) []jsonlContentBlock {
	if len(raw) == 0 {
		return nil
	}
	// Quick check: if it starts with '[' it's an array.
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []jsonlContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// makeExcerpt produces a sanitized excerpt from raw JSON (tool input).
// First 500 UTF-8 chars of the marshaled JSON, ANSI-stripped and sanitized.
func makeExcerpt(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return sanitizeToolExcerpt(string(raw))
}

// makeResultExcerpt produces a sanitized excerpt from tool_result content.
// Tries to extract a plain string first; otherwise uses raw JSON.
func makeResultExcerpt(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return sanitizeToolExcerpt(s)
	}
	return sanitizeToolExcerpt(string(raw))
}

func (p *jsonlEventParser) timeline() []ChatItem {
	return cloneChatItems(p.items)
}

// sanitizeJSONSecretKeys redacts values for JSON keys matching sensitive patterns.
func sanitizeJSONSecretKeys(text string) string {
	return jsonSecretKeyPattern.ReplaceAllString(text, `"$1":"[REDACTED]"`)
}
