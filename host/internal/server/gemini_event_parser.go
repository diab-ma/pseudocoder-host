package server

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// geminiSessionFile represents the top-level JSON structure of a Gemini session file.
type geminiSessionFile struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   float64         `json:"-"`
	LastUpdated float64         `json:"-"`
	Messages    []geminiMessage `json:"messages"`
}

func (s geminiSessionFile) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SessionID   string          `json:"sessionId"`
		ProjectHash string          `json:"projectHash,omitempty"`
		StartTime   float64         `json:"startTime"`
		LastUpdated float64         `json:"lastUpdated"`
		Messages    []geminiMessage `json:"messages"`
	}{
		SessionID:   s.SessionID,
		ProjectHash: s.ProjectHash,
		StartTime:   s.StartTime,
		LastUpdated: s.LastUpdated,
		Messages:    s.Messages,
	})
}

func (s *geminiSessionFile) UnmarshalJSON(data []byte) error {
	var raw struct {
		SessionID   string          `json:"sessionId"`
		ProjectHash string          `json:"projectHash"`
		StartTime   json.RawMessage `json:"startTime"`
		LastUpdated json.RawMessage `json:"lastUpdated"`
		Messages    []geminiMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	startTime, err := parseFlexibleTimestampSeconds(raw.StartTime)
	if err != nil {
		return err
	}
	lastUpdated, err := parseFlexibleTimestampSeconds(raw.LastUpdated)
	if err != nil {
		return err
	}

	s.SessionID = raw.SessionID
	s.ProjectHash = raw.ProjectHash
	s.StartTime = startTime
	s.LastUpdated = lastUpdated
	s.Messages = raw.Messages
	return nil
}

// geminiMessage represents a single message in a Gemini session file.
type geminiMessage struct {
	Type      string           `json:"type"` // "user", "gemini", "info", "error"
	Content   string           `json:"-"`
	Thoughts  []geminiThought  `json:"thoughts,omitempty"`
	ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
	Timestamp float64          `json:"-"` // Unix seconds
}

func (m geminiMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      string           `json:"type"`
		Content   string           `json:"content,omitempty"`
		Thoughts  []geminiThought  `json:"thoughts,omitempty"`
		ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
		Timestamp float64          `json:"timestamp"`
	}{
		Type:      m.Type,
		Content:   m.Content,
		Thoughts:  m.Thoughts,
		ToolCalls: m.ToolCalls,
		Timestamp: m.Timestamp,
	})
}

func (m *geminiMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type      string           `json:"type"`
		Content   json.RawMessage  `json:"content"`
		Thoughts  []geminiThought  `json:"thoughts,omitempty"`
		ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
		Timestamp json.RawMessage  `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	timestamp, err := parseFlexibleTimestampSeconds(raw.Timestamp)
	if err != nil {
		return err
	}
	content, err := parseTextContent(raw.Content)
	if err != nil {
		return err
	}

	m.Type = raw.Type
	m.Content = content
	m.Thoughts = raw.Thoughts
	m.ToolCalls = raw.ToolCalls
	m.Timestamp = timestamp
	return nil
}

// geminiThought represents a single thought entry in a Gemini message.
type geminiThought struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

// geminiToolCall represents a single tool call in a Gemini message.
type geminiToolCall struct {
	Name   string `json:"name"`
	Input  string `json:"-"`
	Result string `json:"-"`
	Status string `json:"status,omitempty"` // "", "success", "error"
}

func (c geminiToolCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name   string `json:"name"`
		Input  string `json:"input,omitempty"`
		Result string `json:"result,omitempty"`
		Status string `json:"status,omitempty"`
	}{
		Name:   c.Name,
		Input:  c.Input,
		Result: c.Result,
		Status: c.Status,
	})
}

func (c *geminiToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name        string          `json:"name"`
		DisplayName string          `json:"displayName"`
		Input       json.RawMessage `json:"input"`
		Args        json.RawMessage `json:"args"`
		Result      json.RawMessage `json:"result"`
		Status      string          `json:"status"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	input, err := compactJSONExcerpt(raw.Input)
	if err != nil {
		return err
	}
	if input == "" {
		input, err = compactJSONExcerpt(raw.Args)
		if err != nil {
			return err
		}
	}
	result, err := compactJSONExcerpt(raw.Result)
	if err != nil {
		return err
	}

	c.Name = raw.Name
	if c.Name == "" {
		c.Name = raw.DisplayName
	}
	c.Input = input
	c.Result = result
	c.Status = raw.Status
	return nil
}

// geminiEventParser transforms Gemini session JSON into ChatItem slices.
// Unlike the JSONL/Codex parsers which process events incrementally,
// Gemini does a full rebuild each time since the JSON file is complete.
type geminiEventParser struct {
	sessionID    string
	items        []ChatItem
	pendingSeed  *jsonlPendingSeed
	fallbackTime int64 // stable fallback for zero timestamps (avoids churn across rebuilds)
}

func newGeminiEventParser(sessionID string) *geminiEventParser {
	return &geminiEventParser{
		sessionID:    sessionID,
		fallbackTime: time.Now().UnixMilli(),
	}
}

// RebuildTimeline clears p.items and rebuilds the full timeline from scratch.
// Seed handling works across rebuilds using timestamp-bounded matching.
func (p *geminiEventParser) RebuildTimeline(messages []geminiMessage) []ChatItem {
	p.items = nil

	// Snapshot the pending seed for this rebuild. If consumed during rebuild,
	// we clear p.pendingSeed at the end.
	var seed *jsonlPendingSeed
	if p.pendingSeed != nil {
		seedCopy := *p.pendingSeed
		seed = &seedCopy
	}
	seedConsumed := false

	for msgIdx, msg := range messages {
		createdAt := p.geminiTimestampToMillis(msg.Timestamp)

		switch msg.Type {
		case "user":
			p.processUserMessage(msg, msgIdx, createdAt, seed, &seedConsumed)
		case "gemini":
			p.processGeminiMessage(msg, msgIdx, createdAt)
		case "info":
			p.processInfoMessage(msg, msgIdx, createdAt)
		case "error":
			p.processErrorMessage(msg, msgIdx, createdAt)
		default:
			log.Printf("gemini_event_parser: skipping unknown message type %q", msg.Type)
		}
	}

	if seedConsumed {
		// Keep pendingSeed so subsequent rebuilds re-consume it and produce
		// the same stable ID. It is naturally replaced when SeedPromptWithItem
		// is called for a new prompt.
	} else if seed != nil {
		// Re-inject unconsumed seed so it survives until the file catches up.
		seedItem := ChatItem{
			ID:        "gemini-user:" + seed.requestID,
			SessionID: p.sessionID,
			Kind:      ChatItemKindUserMessage,
			CreatedAt: seed.createdAt,
			Provider:  ChatItemProviderGemini,
			Status:    ChatItemStatusCompleted,
			RequestID: seed.requestID,
			Text:      strings.TrimSpace(seed.text),
		}
		// Insert at correct position by createdAt (append if latest).
		inserted := false
		for i, item := range p.items {
			if item.CreatedAt > seed.createdAt {
				p.items = slices.Insert(p.items, i, seedItem)
				inserted = true
				break
			}
		}
		if !inserted {
			p.items = append(p.items, seedItem)
		}
	}

	return p.timeline()
}

// SeedPrompt registers a pending seed for prompt deduplication.
func (p *geminiEventParser) SeedPrompt(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
}

// SeedPromptWithItem registers a pending seed AND adds a seeded user item to
// the timeline immediately with Provider=ChatItemProviderGemini.
func (p *geminiEventParser) SeedPromptWithItem(text, requestID string, createdAt int64) {
	p.pendingSeed = &jsonlPendingSeed{
		requestID: requestID,
		text:      text,
		createdAt: createdAt,
	}
	p.items = append(p.items, ChatItem{
		ID:        "gemini-user:" + requestID,
		SessionID: p.sessionID,
		Kind:      ChatItemKindUserMessage,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderGemini,
		Status:    ChatItemStatusCompleted,
		RequestID: requestID,
		Text:      text,
	})
}

// RollbackSeedItem clears the pending seed and removes the seeded user item.
func (p *geminiEventParser) RollbackSeedItem(requestID string) {
	p.ClearPendingSeed()
	targetID := "gemini-user:" + requestID
	for i, item := range p.items {
		if item.ID == targetID {
			p.items = append(p.items[:i], p.items[i+1:]...)
			break
		}
	}
}

// ClearPendingSeed removes any unconsumed pending seed.
func (p *geminiEventParser) ClearPendingSeed() {
	p.pendingSeed = nil
}

// Reset clears all parser state.
func (p *geminiEventParser) Reset() {
	p.items = nil
	p.pendingSeed = nil
}

func (p *geminiEventParser) timeline() []ChatItem {
	return cloneChatItems(p.items)
}

// processUserMessage handles "user" type messages.
func (p *geminiEventParser) processUserMessage(msg geminiMessage, msgIdx int, createdAt int64, seed *jsonlPendingSeed, seedConsumed *bool) {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return
	}

	id := fmt.Sprintf("gemini-user:%d", msgIdx)
	var requestID string

	// Seed dedup: match only if seed is unconsumed, text matches, and timestamp
	// is within the 5-second tolerance window.
	if seed != nil && !*seedConsumed && strings.TrimSpace(seed.text) == text {
		if createdAt >= seed.createdAt-5000 {
			id = "gemini-user:" + seed.requestID
			requestID = seed.requestID
			*seedConsumed = true
		}
	}

	p.items = append(p.items, ChatItem{
		ID:        id,
		SessionID: p.sessionID,
		Kind:      ChatItemKindUserMessage,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderGemini,
		Status:    ChatItemStatusCompleted,
		RequestID: requestID,
		Text:      text,
	})
}

// processGeminiMessage handles "gemini" type messages. Extracts in order:
// thoughts first, then toolCalls, then content.
func (p *geminiEventParser) processGeminiMessage(msg geminiMessage, msgIdx int, createdAt int64) {
	// Thoughts
	for tIdx, thought := range msg.Thoughts {
		text := geminiThoughtText(thought)
		if text == "" {
			continue
		}
		p.items = append(p.items, ChatItem{
			ID:        fmt.Sprintf("gemini-think:%d:%d", msgIdx, tIdx),
			SessionID: p.sessionID,
			Kind:      ChatItemKindThinking,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderGemini,
			Status:    ChatItemStatusCompleted,
			Text:      text,
		})
	}

	// Tool calls
	for cIdx, tc := range msg.ToolCalls {
		status := geminiToolCallStatus(tc.Status)
		item := ChatItem{
			ID:        fmt.Sprintf("gemini-tool:%d:%d", msgIdx, cIdx),
			SessionID: p.sessionID,
			Kind:      ChatItemKindToolCall,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderGemini,
			Status:    status,
			ToolName:  tc.Name,
			Summary:   tc.Name,
			StartedAt: createdAt,
		}
		if tc.Input != "" {
			item.InputExcerpt = sanitizeToolExcerpt(tc.Input)
		} else {
			item.InputExcerpt = "(none)"
		}
		if tc.Result != "" {
			item.ResultExcerpt = sanitizeToolExcerpt(tc.Result)
		} else {
			item.ResultExcerpt = "(pending)"
		}
		p.items = append(p.items, item)
	}

	// Content (assistant text)
	content := stripANSIForChat(msg.Content)
	if content != "" {
		p.items = append(p.items, ChatItem{
			ID:        fmt.Sprintf("gemini-text:%d", msgIdx),
			SessionID: p.sessionID,
			Kind:      ChatItemKindAssistant,
			CreatedAt: createdAt,
			Provider:  ChatItemProviderGemini,
			Status:    ChatItemStatusCompleted,
			Markdown:  content,
		})
	}
}

// processInfoMessage handles "info" type messages → SystemEvent (Completed).
// Skipped if content is empty.
func (p *geminiEventParser) processInfoMessage(msg geminiMessage, msgIdx int, createdAt int64) {
	if msg.Content == "" {
		return
	}
	p.items = append(p.items, ChatItem{
		ID:        fmt.Sprintf("gemini-event:%d", msgIdx),
		SessionID: p.sessionID,
		Kind:      ChatItemKindSystemEvent,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderGemini,
		Status:    ChatItemStatusCompleted,
		Code:      "gemini_info",
		Message:   msg.Content,
	})
}

// processErrorMessage handles "error" type messages → SystemEvent (Failed).
// Emitted even if content is empty (uses "Error" as fallback message).
func (p *geminiEventParser) processErrorMessage(msg geminiMessage, msgIdx int, createdAt int64) {
	message := msg.Content
	if message == "" {
		message = "Error"
	}
	p.items = append(p.items, ChatItem{
		ID:        fmt.Sprintf("gemini-event:%d", msgIdx),
		SessionID: p.sessionID,
		Kind:      ChatItemKindSystemEvent,
		CreatedAt: createdAt,
		Provider:  ChatItemProviderGemini,
		Status:    ChatItemStatusFailed,
		Code:      "gemini_error",
		Message:   message,
	})
}

// geminiTimestampToMillis converts a Gemini Unix-seconds float64 to Unix millis.
// If 0, uses the parser's stable fallback timestamp (set once at creation) to
// prevent continuous churn when the same zero-timestamp message is rebuilt on
// every poll cycle.
func (p *geminiEventParser) geminiTimestampToMillis(ts float64) int64 {
	if ts == 0 {
		return p.fallbackTime
	}
	return int64(ts * 1000)
}

// geminiThoughtText builds the display text for a thought.
// Returns subject + ": " + description, or just one if the other is empty.
// Returns "" if both are empty.
func geminiThoughtText(t geminiThought) string {
	s := strings.TrimSpace(t.Subject)
	d := strings.TrimSpace(t.Description)
	if s == "" && d == "" {
		return ""
	}
	if s == "" {
		return d
	}
	if d == "" {
		return s
	}
	return s + ": " + d
}

// geminiToolCallStatus maps a Gemini tool call status string to a ChatItemStatus.
func geminiToolCallStatus(status string) ChatItemStatus {
	switch status {
	case "success":
		return ChatItemStatusCompleted
	case "":
		return ChatItemStatusInProgress
	default:
		return ChatItemStatusFailed
	}
}
