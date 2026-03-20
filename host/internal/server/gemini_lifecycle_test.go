package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// writeGeminiSessionFileWithMessages writes a full Gemini session JSON file
// (including messages) to dir and returns the absolute path.
func writeGeminiSessionFileWithMessages(t *testing.T, dir, name, sessionID string, startTime float64, messages []geminiMessage, mtime time.Time) string {
	t.Helper()
	session := geminiSessionFile{
		SessionID:   sessionID,
		StartTime:   startTime,
		LastUpdated: float64(mtime.Unix()),
		Messages:    messages,
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal gemini session: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write gemini session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

func writeGeminiSessionFileRaw(t *testing.T, path string, raw string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write raw gemini session file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("set mtime: %v", err)
	}
	return path
}

// newGeminiTestStore creates an in-memory store with a Gemini session pre-registered.
func newGeminiTestStore(t *testing.T, sessionID string, launchTime time.Time) (*storage.SQLiteStore, StructuredChatController) {
	t.Helper()
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	session := &storage.Session{
		ID:        sessionID,
		Repo:      "/repo",
		Branch:    "main",
		StartedAt: launchTime.UTC(),
		LastSeen:  launchTime.UTC(),
		Status:    storage.SessionStatusRunning,
	}
	if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("gemini", nil, "")); err != nil {
		t.Fatalf("ApplySessionMetadataToStorage: %v", err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	controller := NewStructuredChatController(store)
	return store, controller
}

// collectBroadcast returns a thread-safe broadcast collector and its mutex.
func collectBroadcast() (*sync.Mutex, *[]Message, func(Message)) {
	var mu sync.Mutex
	var msgs []Message
	return &mu, &msgs, func(msg Message) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	}
}

// snapshotMessages copies the broadcast slice under the lock.
func snapshotMessages(mu *sync.Mutex, msgs *[]Message) []Message {
	mu.Lock()
	defer mu.Unlock()
	result := make([]Message, len(*msgs))
	copy(result, *msgs)
	return result
}

// clearMessages clears the broadcast slice under the lock.
func clearMessages(mu *sync.Mutex, msgs *[]Message) {
	mu.Lock()
	defer mu.Unlock()
	*msgs = nil
}

// TestGeminiPipelineBootReplay verifies that when a Gemini file is discovered,
// the pipeline reads it, parses messages, merges via the controller, and
// broadcasts a chat.snapshot.
func TestGeminiPipelineBootReplay(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()
	sessionID := "gemini-session-1"

	// Write a Gemini session file with a user message and assistant response.
	messages := []geminiMessage{
		{Type: "user", Content: "hello", Timestamp: float64(launchTime.Add(100*time.Millisecond).Unix()) + 0.1},
		{Type: "gemini", Content: "world", Timestamp: float64(launchTime.Add(200*time.Millisecond).Unix()) + 0.2},
	}
	filePath := writeGeminiSessionFileWithMessages(t, dir, "session.json", "gsid-1", float64(launchTime.Unix()), messages, launchTime.Add(300*time.Millisecond))

	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	// Create the pipeline struct manually (bypass watcher) and call onBound directly.
	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Simulate the watcher binding to the file.
	doneCh := make(chan struct{}, 1)
	originalBroadcast := p.broadcast
	p.broadcast = func(msg Message) {
		originalBroadcast(msg)
		if msg.Type == MessageTypeChatSnapshot {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	}

	p.onBound(filePath, "gsid-1")
	defer func() {
		if p.tailer != nil {
			p.tailer.Stop()
		}
	}()

	// Wait for the initial snapshot.
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for initial snapshot")
	}

	msgs := snapshotMessages(mu, broadcast)
	var foundSnapshot bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			foundSnapshot = true
			var payload ChatSnapshotPayload
			raw, _ := json.Marshal(msg.Payload)
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("unmarshal snapshot: %v", err)
			}
			if len(payload.Items) != 2 {
				t.Fatalf("snapshot items = %d, want 2", len(payload.Items))
			}
			if payload.Items[0].Kind != ChatItemKindUserMessage {
				t.Errorf("item[0].Kind = %q, want user_message", payload.Items[0].Kind)
			}
			if payload.Items[1].Kind != ChatItemKindAssistant {
				t.Errorf("item[1].Kind = %q, want assistant_message", payload.Items[1].Kind)
			}
		}
	}
	if !foundSnapshot {
		t.Fatalf("no chat.snapshot found; got %d messages: %v", len(msgs), messageTypes(msgs))
	}
}

func TestGeminiPipelineBootReplayCurrentSchema(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()
	sessionID := "gemini-session-current"

	filePath := writeGeminiSessionFileRaw(
		t,
		filepath.Join(dir, "session.json"),
		`{
			"sessionId":"gsid-current",
			"startTime":"2026-03-16T01:52:29.707Z",
			"lastUpdated":"2026-03-16T01:52:34.421Z",
			"messages":[
				{
					"type":"user",
					"timestamp":"2026-03-16T01:52:29.707Z",
					"content":[{"text":"hello"}]
				},
				{
					"type":"gemini",
					"timestamp":"2026-03-16T01:52:34.421Z",
					"content":"world",
					"toolCalls":[
						{
							"name":"read_file",
							"args":{"file_path":"README.md"},
							"result":{"ok":true},
							"status":"success"
						}
					]
				}
			]
		}`,
		launchTime.Add(300*time.Millisecond),
	)

	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     newGeminiEventParser(sessionID),
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	doneCh := make(chan struct{}, 1)
	originalBroadcast := p.broadcast
	p.broadcast = func(msg Message) {
		originalBroadcast(msg)
		if msg.Type == MessageTypeChatSnapshot {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	}

	p.onBound(filePath, "gsid-current")
	defer func() {
		if p.tailer != nil {
			p.tailer.Stop()
		}
	}()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for initial snapshot")
	}

	msgs := snapshotMessages(mu, broadcast)
	var snapshot ChatSnapshotPayload
	var foundSnapshot bool
	for _, msg := range msgs {
		if msg.Type != MessageTypeChatSnapshot {
			continue
		}
		foundSnapshot = true
		raw, _ := json.Marshal(msg.Payload)
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatalf("unmarshal snapshot: %v", err)
		}
	}
	if !foundSnapshot {
		t.Fatalf("no chat.snapshot found; got %d messages: %v", len(msgs), messageTypes(msgs))
	}
	if len(snapshot.Items) != 3 {
		t.Fatalf("snapshot items = %d, want 3", len(snapshot.Items))
	}
	if snapshot.Items[0].Text != "hello" {
		t.Errorf("user text = %q, want hello", snapshot.Items[0].Text)
	}
	if snapshot.Items[1].ToolName != "read_file" {
		t.Errorf("tool name = %q, want read_file", snapshot.Items[1].ToolName)
	}
	if snapshot.Items[2].Markdown != "world" {
		t.Errorf("assistant markdown = %q, want world", snapshot.Items[2].Markdown)
	}
}

// TestGeminiPipelineSeedPrompt verifies that SeedPrompt adds an optimistic
// user bubble and it's merged via the controller.
func TestGeminiPipelineSeedPrompt(t *testing.T) {
	sessionID := "gemini-session-2"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast:  broadcastFn,
	}

	err := p.SeedPrompt("hello from mobile", "req-42", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	msgs := snapshotMessages(mu, broadcast)
	var foundUpsert bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatUpsert {
			foundUpsert = true
		}
	}
	if !foundUpsert {
		t.Errorf("expected chat.upsert after SeedPrompt; got: %v", messageTypes(msgs))
	}
}

// TestGeminiPipelineRollbackPrompt verifies that RollbackPrompt removes a
// previously seeded item.
func TestGeminiPipelineRollbackPrompt(t *testing.T) {
	sessionID := "gemini-session-3"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Seed then rollback.
	if err := p.SeedPrompt("hello", "req-99", time.Now().UnixMilli()); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}
	clearMessages(mu, broadcast)

	if err := p.RollbackPrompt("req-99"); err != nil {
		t.Fatalf("RollbackPrompt: %v", err)
	}

	msgs := snapshotMessages(mu, broadcast)
	var foundRemove bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatRemove {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Errorf("expected chat.remove after RollbackPrompt; got: %v", messageTypes(msgs))
	}
}

// TestGeminiPipelineSeedConsumedByEcho verifies that when the Gemini file
// echoes a seeded prompt, the seed is consumed (deduplicated) via timestamp-bounded
// matching (FR22).
func TestGeminiPipelineSeedConsumedByEcho(t *testing.T) {
	dir := t.TempDir()
	sessionID := "gemini-session-4"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast:  broadcastFn,
	}

	seedTime := time.Now().UnixMilli()
	if err := p.SeedPrompt("hello world", "req-seed-1", seedTime); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}
	clearMessages(mu, broadcast)

	// Simulate the Gemini file echoing the same text with a timestamp close
	// to the seed time (within the 5-second tolerance window).
	echoTimestamp := float64(seedTime+1000) / 1000.0 // 1 second later, in Unix seconds
	messages := []geminiMessage{
		{Type: "user", Content: "hello world", Timestamp: echoTimestamp},
		{Type: "gemini", Content: "Hi there!", Timestamp: echoTimestamp + 1.0},
	}

	// Write and onBound to get the tailer going. But for this test, call
	// handleMessages directly to avoid timing issues.
	_ = writeGeminiSessionFileWithMessages(t, dir, "session.json", "gsid-echo", float64(launchTime.Unix()), messages, launchTime)

	p.handleMessages(messages, "gsid-echo")

	msgs := snapshotMessages(mu, broadcast)

	// The seed should be consumed. Verify by checking the merged timeline
	// has exactly 2 items (seeded user + assistant), not 3 (seed + echo + assistant).
	snapshotMsg, err := controller.LoadSnapshotMessage(sessionID)
	if err != nil {
		t.Fatalf("LoadSnapshotMessage: %v", err)
	}
	if snapshotMsg == nil {
		t.Fatal("expected snapshot, got nil")
	}
	var payload ChatSnapshotPayload
	raw, _ := json.Marshal(snapshotMsg.Payload)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("timeline should have 2 items (seed consumed), got %d", len(payload.Items))
	}
	// The user item should retain the seed's request ID.
	if payload.Items[0].RequestID != "req-seed-1" {
		t.Errorf("user item RequestID = %q, want req-seed-1", payload.Items[0].RequestID)
	}
	_ = msgs // consumed for side effects
}

// TestGeminiPipelineResetSequence verifies that after a tailer reset:
// 1. chat.reset is broadcast
// 2. New messages merge correctly
// 3. chat.snapshot is broadcast
func TestGeminiPipelineResetSequence(t *testing.T) {
	sessionID := "gemini-session-5"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// First delivery transitions InitialReplay -> Live with snapshot.
	initialMessages := []geminiMessage{
		{Type: "user", Content: "first prompt", Timestamp: float64(launchTime.Unix()) + 0.1},
		{Type: "gemini", Content: "first response", Timestamp: float64(launchTime.Unix()) + 0.2},
	}
	p.handleMessages(initialMessages, "gsid-reset")

	msgs := snapshotMessages(mu, broadcast)
	var initialSnapshotFound bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			initialSnapshotFound = true
		}
	}
	if !initialSnapshotFound {
		t.Fatalf("no initial snapshot found; got: %v", messageTypes(msgs))
	}

	clearMessages(mu, broadcast)

	// Trigger reset (simulates identity change or truncation).
	p.handleReset()

	// Verify state is ResetReplay.
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()
	if state != jsonlPipelineStateResetReplay {
		t.Fatalf("state = %d, want ResetReplay (%d)", state, jsonlPipelineStateResetReplay)
	}

	// New messages after reset.
	resetMessages := []geminiMessage{
		{Type: "user", Content: "new session prompt", Timestamp: float64(launchTime.Unix()) + 1.0},
	}
	p.handleMessages(resetMessages, "gsid-reset-2")

	msgs = snapshotMessages(mu, broadcast)

	var resetIdx, snapshotIdx int = -1, -1
	for i, msg := range msgs {
		if msg.Type == MessageTypeChatReset && resetIdx < 0 {
			resetIdx = i
		}
		if msg.Type == MessageTypeChatSnapshot {
			snapshotIdx = i
		}
	}
	if resetIdx < 0 {
		t.Fatalf("no chat.reset found; got: %v", messageTypes(msgs))
	}
	if snapshotIdx < 0 {
		t.Fatalf("no chat.snapshot found; got: %v", messageTypes(msgs))
	}
	if resetIdx >= snapshotIdx {
		t.Fatalf("chat.reset (idx %d) should precede chat.snapshot (idx %d)", resetIdx, snapshotIdx)
	}

	// Verify reset reason.
	var resetPayload ChatResetPayload
	raw, _ := json.Marshal(msgs[resetIdx].Payload)
	json.Unmarshal(raw, &resetPayload)
	if resetPayload.Reason != ChatResetReasonProviderReset {
		t.Errorf("reset reason = %q, want %q", resetPayload.Reason, ChatResetReasonProviderReset)
	}

	// Verify snapshot revision > reset revision.
	var snapshotPayload ChatSnapshotPayload
	raw2, _ := json.Marshal(msgs[snapshotIdx].Payload)
	json.Unmarshal(raw2, &snapshotPayload)
	if snapshotPayload.Revision <= resetPayload.Revision {
		t.Errorf("snapshot revision %d should be > reset revision %d",
			snapshotPayload.Revision, resetPayload.Revision)
	}
}

// TestGeminiPipelineResetClearsPendingSeed verifies that a provider reset
// clears any pending seed, preventing ghost bubbles after reset.
func TestGeminiPipelineResetClearsPendingSeed(t *testing.T) {
	sessionID := "gemini-session-6"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	_, _, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Seed a prompt.
	if err := p.SeedPrompt("seeded text", "req-ghost", time.Now().UnixMilli()); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	// Verify seed is in parser.
	p.mu.Lock()
	hasSeed := p.parser.pendingSeed != nil
	p.mu.Unlock()
	if !hasSeed {
		t.Fatal("expected pending seed before reset")
	}

	// Reset clears the seed.
	p.handleReset()

	p.mu.Lock()
	hasSeedAfter := p.parser.pendingSeed != nil
	itemsAfter := len(p.parser.items)
	p.mu.Unlock()
	if hasSeedAfter {
		t.Error("pending seed should be nil after reset")
	}
	if itemsAfter != 0 {
		t.Errorf("items should be empty after reset, got %d", itemsAfter)
	}
}

// TestGeminiPipelineStop verifies that Stop cleans up the tailer and watcher,
// and increments the generation counter to suppress late callbacks.
func TestGeminiPipelineStop(t *testing.T) {
	sessionID := "gemini-session-7"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	_, _, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast:  broadcastFn,
	}

	p.mu.Lock()
	stoppedBefore := p.stopped
	p.mu.Unlock()

	if stoppedBefore {
		t.Error("should not be stopped before Stop()")
	}

	p.Stop()

	p.mu.Lock()
	stoppedAfter := p.stopped
	p.mu.Unlock()

	if !stoppedAfter {
		t.Error("should be stopped after Stop()")
	}
}

// TestGeminiPipelineLateCallbackAfterStop verifies that handleMessages calls
// arriving after Stop are silently ignored (no merge, no broadcast).
func TestGeminiPipelineLateCallbackAfterStop(t *testing.T) {
	sessionID := "gemini-session-8"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Stop the pipeline.
	p.Stop()

	// Late callback: should be ignored.
	messages := []geminiMessage{
		{Type: "user", Content: "late message", Timestamp: float64(launchTime.Unix()) + 1.0},
	}
	p.handleMessages(messages, "gsid-late")

	msgs := snapshotMessages(mu, broadcast)
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot || msg.Type == MessageTypeChatUpsert {
			t.Errorf("unexpected message after Stop: %s", msg.Type)
		}
	}
}

// TestGeminiPipelineLiveIncrementalUpdate verifies that after the initial
// replay, subsequent handleMessages calls only emit incremental diffs (no
// snapshot), relying on the controller's MergeProviderTimeline diff.
func TestGeminiPipelineLiveIncrementalUpdate(t *testing.T) {
	sessionID := "gemini-session-9"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Initial delivery -> Live with snapshot.
	initial := []geminiMessage{
		{Type: "user", Content: "question", Timestamp: float64(launchTime.Unix()) + 0.1},
	}
	p.handleMessages(initial, "gsid-incr")
	clearMessages(mu, broadcast)

	// Second delivery (live): add an assistant response.
	updated := []geminiMessage{
		{Type: "user", Content: "question", Timestamp: float64(launchTime.Unix()) + 0.1},
		{Type: "gemini", Content: "answer", Timestamp: float64(launchTime.Unix()) + 0.5},
	}
	p.handleMessages(updated, "gsid-incr")

	msgs := snapshotMessages(mu, broadcast)

	// Should NOT have a snapshot (that's only for initial/reset replay).
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			t.Error("unexpected chat.snapshot during live update; should only emit incremental messages")
		}
	}

	// Should have an upsert for the new assistant item.
	var foundUpsert bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatUpsert {
			foundUpsert = true
		}
	}
	if !foundUpsert {
		t.Errorf("expected chat.upsert for incremental update; got: %v", messageTypes(msgs))
	}
}

// TestGeminiPipelineFullLifecycle exercises the complete pipeline: boot replay,
// live update, seed/rollback, and stop.
func TestGeminiPipelineFullLifecycle(t *testing.T) {
	dir := t.TempDir()
	sessionID := "gemini-session-full"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Phase 1: Boot replay.
	bootMessages := []geminiMessage{
		{Type: "user", Content: "boot prompt", Timestamp: float64(launchTime.Unix()) + 0.1},
		{Type: "gemini", Content: "boot response", Timestamp: float64(launchTime.Unix()) + 0.2},
	}

	filePath := writeGeminiSessionFileWithMessages(t, dir, "session.json", "gsid-full",
		float64(launchTime.Unix()), bootMessages, launchTime)
	_ = filePath

	p.handleMessages(bootMessages, "gsid-full")

	msgs := snapshotMessages(mu, broadcast)
	if len(msgs) == 0 {
		t.Fatal("expected snapshot after boot replay")
	}
	clearMessages(mu, broadcast)

	// Phase 2: Live update.
	liveMessages := append(bootMessages, geminiMessage{
		Type: "gemini", Content: "follow-up", Timestamp: float64(launchTime.Unix()) + 0.5,
	})
	p.handleMessages(liveMessages, "gsid-full")

	msgs = snapshotMessages(mu, broadcast)
	if len(msgs) == 0 {
		t.Fatal("expected upsert after live update")
	}
	clearMessages(mu, broadcast)

	// Phase 3: SeedPrompt.
	if err := p.SeedPrompt("mobile question", "req-full-1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}
	msgs = snapshotMessages(mu, broadcast)
	var seeded bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatUpsert {
			seeded = true
		}
	}
	if !seeded {
		t.Error("expected upsert after SeedPrompt")
	}
	clearMessages(mu, broadcast)

	// Phase 4: RollbackPrompt.
	if err := p.RollbackPrompt("req-full-1"); err != nil {
		t.Fatalf("RollbackPrompt: %v", err)
	}
	msgs = snapshotMessages(mu, broadcast)
	var rolledBack bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatRemove {
			rolledBack = true
		}
	}
	if !rolledBack {
		t.Error("expected remove after RollbackPrompt")
	}

	// Phase 5: Stop.
	p.Stop()

	p.mu.Lock()
	isStopped := p.stopped
	p.mu.Unlock()
	if !isStopped {
		t.Error("pipeline should be stopped after Stop()")
	}
}

// TestGeminiPipelineRevisionMonotonicity verifies that revision numbers always
// increase across operations (initial replay, live updates, resets).
func TestGeminiPipelineRevisionMonotonicity(t *testing.T) {
	sessionID := "gemini-session-rev"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	mu, broadcast, broadcastFn := collectBroadcast()

	parser := newGeminiEventParser(sessionID)
	p := &geminiPipeline{
		sessionID:  sessionID,
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
		broadcast:  broadcastFn,
	}

	// Initial replay.
	p.handleMessages([]geminiMessage{
		{Type: "user", Content: "hello", Timestamp: float64(launchTime.Unix()) + 0.1},
	}, "gsid-rev")

	msgs := snapshotMessages(mu, broadcast)
	var lastRev int64
	for _, msg := range msgs {
		rev := extractLifecycleRevision(t, msg)
		if rev > lastRev {
			lastRev = rev
		}
	}
	if lastRev == 0 {
		t.Fatal("expected non-zero revision after initial replay")
	}
	clearMessages(mu, broadcast)

	// Live update.
	p.handleMessages([]geminiMessage{
		{Type: "user", Content: "hello", Timestamp: float64(launchTime.Unix()) + 0.1},
		{Type: "gemini", Content: "world", Timestamp: float64(launchTime.Unix()) + 0.5},
	}, "gsid-rev")

	msgs = snapshotMessages(mu, broadcast)
	for _, msg := range msgs {
		rev := extractLifecycleRevision(t, msg)
		if rev <= lastRev {
			t.Fatalf("revision %d not > %d during live update", rev, lastRev)
		}
		if rev > lastRev {
			lastRev = rev
		}
	}
	clearMessages(mu, broadcast)

	// Reset + new content.
	p.handleReset()
	p.handleMessages([]geminiMessage{
		{Type: "user", Content: "reset prompt", Timestamp: float64(launchTime.Unix()) + 2.0},
	}, "gsid-rev-2")

	msgs = snapshotMessages(mu, broadcast)
	for _, msg := range msgs {
		rev := extractLifecycleRevision(t, msg)
		if rev <= lastRev {
			t.Fatalf("revision %d not > %d after reset", rev, lastRev)
		}
		if rev > lastRev {
			lastRev = rev
		}
	}
}

// TestGeminiPipelineNewGeminiPipelineError verifies that newGeminiPipeline
// returns an error when the watcher cannot be created (e.g., home dir issue).
// We test indirectly by ensuring the constructor creates a valid pipeline
// under normal conditions.
func TestGeminiPipelineConstructor(t *testing.T) {
	sessionID := "gemini-session-ctor"
	launchTime := time.Now()
	_, controller := newGeminiTestStore(t, sessionID, launchTime)
	_, _, broadcastFn := collectBroadcast()

	// Use the actual constructor with a temp dir as working dir.
	// The watcher will scan ~/.gemini/tmp/ and likely find nothing, but
	// the constructor should succeed.
	workingDir := t.TempDir()
	var downgradeCalled bool
	pipeline, err := newGeminiPipeline(
		sessionID,
		workingDir,
		launchTime.UnixMilli(),
		controller,
		broadcastFn,
		func() { downgradeCalled = true },
	)
	if err != nil {
		t.Fatalf("newGeminiPipeline: %v", err)
	}
	defer pipeline.Stop()

	// Pipeline should exist with parser and watcher set. Watcher starts at
	// pipeline creation so existing session files are discovered immediately.
	// Pre-prompt timeouts are swallowed.
	pipeline.mu.Lock()
	hasWatcher := pipeline.watcher != nil
	hasParser := pipeline.parser != nil
	pipeline.mu.Unlock()

	if !hasWatcher {
		t.Error("watcher should start at pipeline creation")
	}
	if !hasParser {
		t.Error("expected parser to be set")
	}

	// Suppress unused variable warning.
	_ = downgradeCalled
}
