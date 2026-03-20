package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pseudocoder/host/internal/storage"
)

var (
	ansiEscapePattern      = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	ansiCursorMovePattern  = regexp.MustCompile(`\x1b\[[0-9;]*[GHfCDEF]`)
	ansiAllEscapePattern   = regexp.MustCompile(`\x1b(?:\[[0-9;?>=]*[A-Za-z~@]|\][^\x07\x1b]*(?:\x07|\x1b\\)|\([A-Za-z]|[=>#78cDEMNOH])`)
	multiSpacePattern      = regexp.MustCompile(`[ ]{2,}`)
	authHeaderPattern      = regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+\S+`)
	bearerTokenPattern     = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	envSecretAssignPattern = regexp.MustCompile(`\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|API_KEY|AUTH)[A-Z0-9_]*)=([^\s]+)`)
)

// structuredChatController persists and replays host-authored structured chat snapshots.
type structuredChatController struct {
	store        storage.StructuredChatStore
	serverBootID string
	timelineMu   sync.Mutex   // serializes timeline mutations (load→diff→persist); see spec 08
	mu           sync.RWMutex // protects revisions map
	revisions    map[string]int64
}

// NewStructuredChatController creates the structured snapshot controller for one host process.
func NewStructuredChatController(store storage.StructuredChatStore) StructuredChatController {
	return &structuredChatController{
		store:        store,
		serverBootID: fmt.Sprintf("boot-%d", time.Now().UnixNano()),
		revisions:    make(map[string]int64),
	}
}

// ServerBootID returns the stable boot identifier for this process lifetime.
func (c *structuredChatController) ServerBootID() string {
	return c.serverBootID
}

// LoadSnapshotMessage loads a stored snapshot as a replay-ready message.
// It does not mutate revision or persistence state.
func (c *structuredChatController) LoadSnapshotMessage(sessionID string) (*Message, error) {
	snapshot, err := c.store.LoadStructuredChatSnapshot(sessionID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, nil
	}
	c.rememberRevision(snapshot.SessionID, snapshot.Revision)

	items := make([]ChatItem, 0, len(snapshot.Items))
	for _, storedItem := range snapshot.Items {
		var item ChatItem
		if err := json.Unmarshal([]byte(storedItem.PayloadJSON), &item); err != nil {
			return nil, fmt.Errorf("decode stored chat item %q: %w", storedItem.ItemID, err)
		}
		items = append(items, item)
	}

	items = filterProviderItems(items)

	msg := NewChatSnapshotMessage(snapshot.SessionID, c.serverBootID, snapshot.Revision, items)
	return &msg, nil
}

// BuildSessionSwitchReset creates the requester-scoped reset sent before session replay.
func (c *structuredChatController) BuildSessionSwitchReset(sessionID string) Message {
	revision, ok := c.lookupRevision(sessionID)
	if !ok {
		snapshot, err := c.store.LoadStructuredChatSnapshot(sessionID)
		if err == nil && snapshot != nil {
			revision = snapshot.Revision
			c.rememberRevision(sessionID, revision)
		}
	}
	return NewChatResetMessage(sessionID, c.serverBootID, revision, ChatResetReasonSessionSwitch)
}

// ApplyAuthoritativeTimeline writes a diff-aware snapshot replacement and returns live messages.
// The timeline mutation lock (timelineMu) serializes the full load→diff→persist cycle
// so that concurrent callers (e.g. approval upserts, future JSONL merges) never
// produce lost updates. See spec 08.
func (c *structuredChatController) ApplyAuthoritativeTimeline(sessionID string, items []ChatItem) ([]Message, error) {
	items = sanitizeStructuredTimeline(items) // pure — no lock needed
	items = filterProviderItems(items)

	c.timelineMu.Lock()
	defer c.timelineMu.Unlock()

	existing, err := c.store.LoadStructuredChatSnapshot(sessionID)
	if err != nil {
		return nil, err
	}
	currentRevision := int64(0)
	existingItems := []ChatItem(nil)
	if existing != nil {
		currentRevision = existing.Revision
		existingItems, err = decodeStoredChatItems(existing.Items)
		if err != nil {
			return nil, err
		}
	}
	// Respect revisions reserved by NextRevision (used for live-only messages
	// like chat.reset). Without this, Apply would reuse a revision that
	// NextRevision already allocated.
	if cached, ok := c.lookupRevision(sessionID); ok && cached > currentRevision {
		currentRevision = cached
	}

	removeIDs, upsertItems, changed := diffAuthoritativeTimeline(existingItems, items)
	if !changed {
		c.rememberRevision(sessionID, currentRevision)
		return nil, nil
	}

	storedItems, err := encodeStoredChatItems(items)
	if err != nil {
		return nil, err
	}

	// Each message gets its own incrementing revision so mobile's
	// "revision > last seen" gate accepts both remove and upsert.
	msgCount := 0
	if len(removeIDs) > 0 {
		msgCount++
	}
	if len(upsertItems) > 0 {
		msgCount++
	}
	if msgCount == 0 {
		msgCount = 1
	}
	finalRevision := currentRevision + int64(msgCount)

	snapshot := &storage.StructuredChatSnapshot{
		SessionID: sessionID,
		Revision:  finalRevision,
		UpdatedAt: time.Now().UTC(),
		Items:     storedItems,
	}
	if err := c.store.ReplaceStructuredChatSnapshot(snapshot); err != nil {
		return nil, err
	}
	c.rememberRevision(sessionID, finalRevision)

	messages := make([]Message, 0, 2)
	rev := currentRevision + 1
	if len(removeIDs) > 0 {
		messages = append(messages, NewChatRemoveMessage(sessionID, c.serverBootID, rev, removeIDs))
		rev++
	}
	if len(upsertItems) > 0 {
		messages = append(messages, NewChatUpsertMessage(sessionID, c.serverBootID, rev, upsertItems))
	}
	return messages, nil
}

func sanitizeStructuredTimeline(items []ChatItem) []ChatItem {
	sanitized := make([]ChatItem, len(items))
	for i, item := range items {
		sanitized[i] = sanitizeChatItem(item)
	}
	return sanitized
}

func sanitizeChatItem(item ChatItem) ChatItem {
	switch item.Kind {
	case ChatItemKindToolCall:
		item.Summary = stripANSIEscapes(item.Summary)
		item.InputExcerpt = sanitizeToolExcerpt(item.InputExcerpt)
		item.ResultExcerpt = sanitizeToolExcerpt(item.ResultExcerpt)
	case ChatItemKindAssistant:
		item.Markdown = ansiAllEscapePattern.ReplaceAllString(item.Markdown, "")
	case ChatItemKindUserMessage:
		item.Text = ansiAllEscapePattern.ReplaceAllString(item.Text, "")
	case ChatItemKindThinking:
		item.Text = ansiAllEscapePattern.ReplaceAllString(item.Text, "")
	}
	return item
}

func sanitizeToolExcerpt(text string) string {
	text = stripANSIEscapes(text)
	text = authHeaderPattern.ReplaceAllString(text, "Authorization: Bearer [REDACTED]")
	text = bearerTokenPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = envSecretAssignPattern.ReplaceAllString(text, "$1=[REDACTED]")
	text = sanitizeJSONSecretKeys(text)
	return truncateUTF8(text, 500)
}

func stripANSIEscapes(text string) string {
	return ansiEscapePattern.ReplaceAllString(text, "")
}

// stripANSIForChat handles TUI-style PTY output where cursor-positioning
// sequences (e.g. \x1b[5G) provide word spacing instead of literal spaces.
// It replaces cursor-movement sequences with a space, strips all remaining
// ANSI/OSC sequences, and collapses runs of whitespace.
func stripANSIForChat(text string) string {
	// Step 1: Replace cursor-movement sequences with a space.
	text = ansiCursorMovePattern.ReplaceAllString(text, " ")
	// Step 2: Remove all remaining escape sequences (colors, OSC, charset).
	text = ansiAllEscapePattern.ReplaceAllString(text, "")
	// Step 3: Remove any stray ESC characters (partial sequences).
	text = strings.ReplaceAll(text, "\x1b", "")
	// Step 4: Collapse multiple spaces into one.
	text = multiSpacePattern.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	count := 0
	for _, r := range text {
		if count == limit {
			break
		}
		builder.WriteRune(r)
		count++
	}
	return builder.String()
}

// MergeProviderTimeline replaces the provider partition (ID prefix "jsonl-")
// while preserving host-partition items (ID prefix "approval:"). The merged
// output interleaves host items chronologically among provider items.
// Holds timelineMu for the full load→merge→persist cycle. See spec 04/08.
func (c *structuredChatController) MergeProviderTimeline(sessionID string, providerItems []ChatItem) ([]Message, error) {
	providerItems = sanitizeStructuredTimeline(providerItems) // pure — no lock needed
	providerItems = filterProviderItems(providerItems)

	c.timelineMu.Lock()
	defer c.timelineMu.Unlock()

	existing, err := c.store.LoadStructuredChatSnapshot(sessionID)
	if err != nil {
		return nil, err
	}
	currentRevision := int64(0)
	var existingItems []ChatItem
	if existing != nil {
		currentRevision = existing.Revision
		existingItems, err = decodeStoredChatItems(existing.Items)
		if err != nil {
			return nil, err
		}
	}
	if cached, ok := c.lookupRevision(sessionID); ok && cached > currentRevision {
		currentRevision = cached
	}

	// Extract host-partition items from existing timeline, preserving order.
	hostItems := filterHostItems(existingItems)

	// Merge: interleave host items among provider items by created_at.
	// Provider items retain JSONL file order; ties → provider first.
	merged := mergeProviderAndHostItems(providerItems, hostItems)

	// Diff against full existing timeline to determine broadcast messages.
	removeIDs, upsertItems, changed := diffAuthoritativeTimeline(existingItems, merged)
	if !changed {
		c.rememberRevision(sessionID, currentRevision)
		return nil, nil
	}

	storedItems, err := encodeStoredChatItems(merged)
	if err != nil {
		return nil, err
	}

	// Each message gets its own incrementing revision so mobile's
	// "revision > last seen" gate accepts both remove and upsert.
	msgCount := 0
	if len(removeIDs) > 0 {
		msgCount++
	}
	if len(upsertItems) > 0 {
		msgCount++
	}
	if msgCount == 0 {
		msgCount = 1
	}
	finalRevision := currentRevision + int64(msgCount)

	snapshot := &storage.StructuredChatSnapshot{
		SessionID: sessionID,
		Revision:  finalRevision,
		UpdatedAt: time.Now().UTC(),
		Items:     storedItems,
	}
	if err := c.store.ReplaceStructuredChatSnapshot(snapshot); err != nil {
		return nil, err
	}
	c.rememberRevision(sessionID, finalRevision)

	messages := make([]Message, 0, 2)
	rev := currentRevision + 1
	if len(removeIDs) > 0 {
		messages = append(messages, NewChatRemoveMessage(sessionID, c.serverBootID, rev, removeIDs))
		rev++
	}
	if len(upsertItems) > 0 {
		messages = append(messages, NewChatUpsertMessage(sessionID, c.serverBootID, rev, upsertItems))
	}
	return messages, nil
}

// isHostItem returns true if the item belongs to the host partition (approval items).
func isHostItem(item ChatItem) bool {
	return strings.HasPrefix(item.ID, "approval:")
}

// filterHostItems extracts host-partition items from a timeline, preserving order.
func filterHostItems(items []ChatItem) []ChatItem {
	var host []ChatItem
	for _, item := range items {
		if isHostItem(item) {
			host = append(host, item)
		}
	}
	return host
}

// mergeProviderAndHostItems interleaves host items among provider items based
// on created_at. Provider items retain their original order (JSONL file order).
// On ties (equal created_at), provider items come first. See spec 04.
func mergeProviderAndHostItems(provider, host []ChatItem) []ChatItem {
	if len(host) == 0 {
		return cloneChatItems(provider)
	}
	if len(provider) == 0 {
		return cloneChatItems(host)
	}

	merged := make([]ChatItem, 0, len(provider)+len(host))
	hostIdx := 0

	for _, pi := range provider {
		// Insert host items whose created_at is strictly before this provider item.
		// Ties → provider first, so we use strict less-than.
		for hostIdx < len(host) && host[hostIdx].CreatedAt < pi.CreatedAt {
			merged = append(merged, host[hostIdx])
			hostIdx++
		}
		merged = append(merged, pi)
	}

	// Append remaining host items after all provider items.
	for hostIdx < len(host) {
		merged = append(merged, host[hostIdx])
		hostIdx++
	}

	return merged
}

// NextRevision reserves the next monotonic revision for live-only messages.
// Holds timelineMu to serialize against ApplyAuthoritativeTimeline and future
// MergeProviderTimeline, preventing revision counter races. See spec 08.
func (c *structuredChatController) NextRevision(sessionID string) (int64, error) {
	c.timelineMu.Lock()
	defer c.timelineMu.Unlock()

	currentRevision := int64(0)
	if revision, ok := c.lookupRevision(sessionID); ok {
		currentRevision = revision
	} else {
		snapshot, err := c.store.LoadStructuredChatSnapshot(sessionID)
		if err != nil {
			return 0, err
		}
		if snapshot != nil {
			currentRevision = snapshot.Revision
		}
	}
	nextRevision := currentRevision + 1
	c.rememberRevision(sessionID, nextRevision)
	return nextRevision, nil
}

func (c *structuredChatController) rememberRevision(sessionID string, revision int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisions[sessionID] = revision
}

func (c *structuredChatController) lookupRevision(sessionID string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	revision, ok := c.revisions[sessionID]
	return revision, ok
}

func encodeStoredChatItems(items []ChatItem) ([]storage.StructuredChatStoredItem, error) {
	storedItems := make([]storage.StructuredChatStoredItem, 0, len(items))
	for position, item := range items {
		payloadJSON, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("marshal chat item %q: %w", item.ID, err)
		}
		storedItems = append(storedItems, storage.StructuredChatStoredItem{
			ItemID:      item.ID,
			Position:    position,
			PayloadJSON: string(payloadJSON),
			CreatedAt:   time.UnixMilli(item.CreatedAt).UTC(),
		})
	}
	return storedItems, nil
}

func decodeStoredChatItems(stored []storage.StructuredChatStoredItem) ([]ChatItem, error) {
	items := make([]ChatItem, 0, len(stored))
	for _, storedItem := range stored {
		var item ChatItem
		if err := json.Unmarshal([]byte(storedItem.PayloadJSON), &item); err != nil {
			return nil, fmt.Errorf("decode stored chat item %q: %w", storedItem.ItemID, err)
		}
		items = append(items, item)
	}
	return items, nil
}

func diffAuthoritativeTimeline(existing, replacement []ChatItem) ([]string, []ChatItem, bool) {
	if chatItemsEqual(existing, replacement) {
		return nil, nil, false
	}

	existingByID := make(map[string]ChatItem, len(existing))
	existingPositions := make(map[string]int, len(existing))
	for i, item := range existing {
		existingByID[item.ID] = item
		existingPositions[item.ID] = i
	}

	replacementByID := make(map[string]ChatItem, len(replacement))
	positionChanged := false
	upsertItems := make([]ChatItem, 0, len(replacement))
	for i, item := range replacement {
		replacementByID[item.ID] = item
		if oldPos, ok := existingPositions[item.ID]; ok && oldPos != i {
			positionChanged = true
		}
		if oldItem, ok := existingByID[item.ID]; !ok || oldItem != item {
			upsertItems = append(upsertItems, item)
		}
	}

	removeIDs := make([]string, 0)
	for _, item := range existing {
		if _, ok := replacementByID[item.ID]; !ok {
			removeIDs = append(removeIDs, item.ID)
		}
	}

	// Any surviving-item reorder needs the full authoritative order in the upsert
	// payload. A remove alone cannot tell clients how the retained items moved.
	if positionChanged {
		return removeIDs, cloneChatItems(replacement), true
	}
	return removeIDs, upsertItems, true
}
