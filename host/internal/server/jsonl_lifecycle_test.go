package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// mockPipeline implements structuredPipeline for testing.
type mockPipeline struct {
	onStop func()
}

func (m *mockPipeline) SeedPrompt(text, requestID string, createdAt int64) error { return nil }
func (m *mockPipeline) RollbackPrompt(requestID string) error                    { return nil }
func (m *mockPipeline) Stop() {
	if m.onStop != nil {
		m.onStop()
	}
}

// makeJSONLUserEvent creates a JSONL user event with specific text and timestamp.
func makeJSONLUserEvent(uuid, text string, ts time.Time) string {
	event := map[string]interface{}{
		"uuid":      uuid,
		"type":      "human",
		"timestamp": ts.Format(time.RFC3339Nano),
		"message": map[string]interface{}{
			"role":    "user",
			"content": text,
		},
	}
	data, _ := json.Marshal(event)
	return string(data)
}

// makeJSONLAssistantEvent creates a JSONL assistant event with a text block.
func makeJSONLAssistantEvent(uuid, text string, ts time.Time) string {
	event := map[string]interface{}{
		"uuid":      uuid,
		"type":      "assistant",
		"timestamp": ts.Format(time.RFC3339Nano),
		"message": map[string]interface{}{
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "text", "text": text},
			},
		},
	}
	data, _ := json.Marshal(event)
	return string(data)
}

// newJSONLTestRuntime creates a runtime with JSONL enabled, a Claude session store,
// and returns the broadcast collector. The session's StartedAt is set to launchTime.
func newJSONLTestRuntime(t *testing.T, launchTime time.Time) (
	*storage.SQLiteStore, StructuredChatController, StructuredChatRuntime, *[]Message,
) {
	t.Helper()
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	session := &storage.Session{
		ID:        "session-1",
		Repo:      "/repo",
		Branch:    "main",
		StartedAt: launchTime.UTC(),
		LastSeen:  launchTime.UTC(),
		Status:    storage.SessionStatusRunning,
	}
	if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("claude", nil, "")); err != nil {
		t.Fatalf("ApplySessionMetadataToStorage: %v", err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	controller := NewStructuredChatController(store)
	var mu sync.Mutex
	var broadcast []Message
	runtime := NewStructuredChatRuntime(store, controller, func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	})

	return store, controller, runtime, &broadcast
}

// getBroadcast safely copies the broadcast slice.
func getBroadcast(mu *sync.Mutex, broadcast *[]Message) []Message {
	mu.Lock()
	defer mu.Unlock()
	result := make([]Message, len(*broadcast))
	copy(result, *broadcast)
	return result
}

// TestJSONLBootReplay verifies that on boot, the JSONL file is read from the
// beginning, MergeProviderTimeline is called once with the full list, and
// connected clients receive a chat.snapshot.
func TestJSONLBootReplay(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	// Write a JSONL file with two events.
	eventTime1 := launchTime.Add(100 * time.Millisecond)
	eventTime2 := launchTime.Add(200 * time.Millisecond)
	lines := []string{
		makeJSONLUserEvent("uuid-1", "hello", eventTime1),
		makeJSONLAssistantEvent("uuid-2", "world", eventTime2),
	}
	writeJSONLFile(t, dir, "test-session.jsonl", lines, launchTime)

	store, controller, _, broadcastPtr := newJSONLTestRuntime(t, launchTime)
	_ = store

	// Build the pipeline manually (bypassing watcher discovery) to test
	// tail reader → parser → controller → broadcast directly.
	parser := newJSONLEventParser("session-1")
	var pipeMu sync.Mutex
	pipeState := jsonlPipelineStateInitialReplay

	var mu sync.Mutex
	readCompleteCh := make(chan struct{}, 1)

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filepath.Join(dir, "test-session.jsonl"),
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipeMu.Lock()
			parser.ParseEvent(line)
			if pipeState != jsonlPipelineStateLive {
				pipeMu.Unlock()
				return
			}
			items := parser.timeline()
			pipeMu.Unlock()

			messages, err := controller.MergeProviderTimeline("session-1", items)
			if err != nil {
				t.Errorf("MergeProviderTimeline: %v", err)
				return
			}
			mu.Lock()
			*broadcastPtr = append(*broadcastPtr, messages...)
			mu.Unlock()
		},
		OnReadComplete: func() {
			pipeMu.Lock()
			items := parser.timeline()
			pipeState = jsonlPipelineStateLive
			pipeMu.Unlock()

			if _, err := controller.MergeProviderTimeline("session-1", items); err != nil {
				t.Errorf("batch MergeProviderTimeline: %v", err)
				return
			}

			snapshotMsg, err := controller.LoadSnapshotMessage("session-1")
			if err != nil {
				t.Errorf("LoadSnapshotMessage: %v", err)
				return
			}
			if snapshotMsg != nil {
				mu.Lock()
				*broadcastPtr = append(*broadcastPtr, *snapshotMsg)
				mu.Unlock()
			}
			readCompleteCh <- struct{}{}
		},
	})
	defer reader.Stop()
	reader.Start()

	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial read complete")
	}

	mu.Lock()
	msgs := make([]Message, len(*broadcastPtr))
	copy(msgs, *broadcastPtr)
	mu.Unlock()

	// Should have a chat.snapshot message.
	var foundSnapshot bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			foundSnapshot = true
			var payload ChatSnapshotPayload
			raw, _ := json.Marshal(msg.Payload)
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("unmarshal snapshot payload: %v", err)
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
		t.Fatalf("no chat.snapshot message broadcast; got %d messages: %v", len(msgs), messageTypes(msgs))
	}
}

// TestJSONLReconnectReplay verifies that after boot, a reconnecting mobile
// client gets the stored snapshot without re-reading the JSONL file.
func TestJSONLReconnectReplay(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	controller := NewStructuredChatController(store)

	// Simulate a previous boot by persisting a snapshot.
	items := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "session-1", Kind: ChatItemKindUserMessage, CreatedAt: 1000, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "hello"},
		{ID: "jsonl-text:a1:0", SessionID: "session-1", Kind: ChatItemKindAssistant, CreatedAt: 2000, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "world"},
	}
	if _, err := controller.MergeProviderTimeline("session-1", items); err != nil {
		t.Fatalf("MergeProviderTimeline: %v", err)
	}

	// LoadSnapshotMessage should return the stored snapshot (no file re-read).
	snapshotMsg, err := controller.LoadSnapshotMessage("session-1")
	if err != nil {
		t.Fatalf("LoadSnapshotMessage: %v", err)
	}
	if snapshotMsg == nil {
		t.Fatal("expected snapshot message, got nil")
	}
	if snapshotMsg.Type != MessageTypeChatSnapshot {
		t.Fatalf("message type = %q, want chat.snapshot", snapshotMsg.Type)
	}
}

// TestJSONLProviderResetSequence verifies that on file truncation:
// 1. chat.reset is sent at revision N
// 2. File is re-read from beginning
// 3. chat.snapshot is sent at revision N+1
func TestJSONLProviderResetSequence(t *testing.T) {
	dir := t.TempDir()
	launchTime := time.Now()

	// Initial JSONL file.
	eventTime1 := launchTime.Add(100 * time.Millisecond)
	filePath := writeJSONLFile(t, dir, "session.jsonl",
		[]string{makeJSONLUserEvent("uuid-1", "hello", eventTime1)}, launchTime)

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	controller := NewStructuredChatController(store)

	parser := newJSONLEventParser("session-1")
	var pipeMu sync.Mutex
	pipeState := jsonlPipelineStateInitialReplay

	var mu sync.Mutex
	var broadcast []Message
	readCompleteCh := make(chan struct{}, 10)
	readCompleteCount := 0

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filePath,
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipeMu.Lock()
			parser.ParseEvent(line)
			if pipeState != jsonlPipelineStateLive {
				pipeMu.Unlock()
				return
			}
			items := parser.timeline()
			pipeMu.Unlock()

			messages, _ := controller.MergeProviderTimeline("session-1", items)
			mu.Lock()
			broadcast = append(broadcast, messages...)
			mu.Unlock()
		},
		OnReset: func() {
			pipeMu.Lock()
			parser.Reset()
			pipeState = jsonlPipelineStateResetReplay
			pipeMu.Unlock()
		},
		OnReadComplete: func() {
			pipeMu.Lock()
			items := parser.timeline()
			prevState := pipeState
			pipeState = jsonlPipelineStateLive
			pipeMu.Unlock()

			if prevState == jsonlPipelineStateResetReplay {
				revN, _ := controller.NextRevision("session-1")
				mu.Lock()
				broadcast = append(broadcast, NewChatResetMessage("session-1", controller.ServerBootID(), revN, ChatResetReasonProviderReset))
				mu.Unlock()
			}

			controller.MergeProviderTimeline("session-1", items)
			snapshotMsg, _ := controller.LoadSnapshotMessage("session-1")
			if snapshotMsg != nil {
				mu.Lock()
				broadcast = append(broadcast, *snapshotMsg)
				mu.Unlock()
			}

			readCompleteCh <- struct{}{}
		},
	})
	defer reader.Stop()
	reader.Start()

	// Wait for initial read complete.
	select {
	case <-readCompleteCh:
		readCompleteCount++
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial read")
	}

	// Clear broadcast to track only reset-related messages.
	mu.Lock()
	broadcast = nil
	mu.Unlock()

	// Truncate the file to trigger provider_reset (tail reader detects size < offset).
	if err := os.Truncate(filePath, 0); err != nil {
		t.Fatalf("truncate file: %v", err)
	}
	// Give the tail reader one poll cycle to detect truncation.
	time.Sleep(50 * time.Millisecond)

	// Write new content (the tail reader re-reads from beginning after reset).
	eventTime2 := launchTime.Add(300 * time.Millisecond)
	if err := os.WriteFile(filePath, []byte(makeJSONLUserEvent("uuid-2", "world", eventTime2)+"\n"), 0644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	// Wait for reset read complete.
	select {
	case <-readCompleteCh:
		readCompleteCount++
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reset read complete")
	}

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	// Verify: chat.reset, then chat.snapshot (or chat.upsert + snapshot).
	var resetIdx, snapshotIdx int
	resetIdx, snapshotIdx = -1, -1
	for i, msg := range msgs {
		if msg.Type == MessageTypeChatReset {
			resetIdx = i
		}
		if msg.Type == MessageTypeChatSnapshot {
			snapshotIdx = i
		}
	}

	if resetIdx < 0 {
		t.Fatalf("no chat.reset message found; got: %v", messageTypes(msgs))
	}
	if snapshotIdx < 0 {
		t.Fatalf("no chat.snapshot message found; got: %v", messageTypes(msgs))
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
		t.Errorf("snapshot revision %d should be > reset revision %d", snapshotPayload.Revision, resetPayload.Revision)
	}
}

// TestJSONLAdapterDowngrade verifies that when the watcher fails to discover
// a JSONL file, an adapter_downgrade is emitted and capabilities are reset.
func TestJSONLAdapterDowngrade(t *testing.T) {
	launchTime := time.Now()

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	session := &storage.Session{
		ID:        "session-1",
		Repo:      "/repo",
		Branch:    "main",
		StartedAt: launchTime.UTC(),
		LastSeen:  launchTime.UTC(),
		Status:    storage.SessionStatusRunning,
	}
	if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("claude", nil, "")); err != nil {
		t.Fatalf("ApplySessionMetadataToStorage: %v", err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	controller := NewStructuredChatController(store)
	var mu sync.Mutex
	var broadcast []Message
	downgradeCh := make(chan struct{}, 1)

	runtime := &structuredChatRuntime{
		store:          store,
		controller:     controller,
		broadcast: func(msg Message) {
			mu.Lock()
			broadcast = append(broadcast, msg)
			mu.Unlock()
			if msg.Type == MessageTypeChatReset {
				select {
				case downgradeCh <- struct{}{}:
				default:
				}
			}
		},
		pipelines: make(map[string]structuredPipeline),
	}

	// Simulate downgrade callback directly.
	runtime.handleProviderDowngrade("session-1", AgentProviderClaude, time.Now().UnixMilli())

	select {
	case <-downgradeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for downgrade")
	}

	// Verify session capabilities are downgraded.
	updated, err := store.GetSession("session-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	info, err := SessionInfoFromStoredSession(updated)
	if err != nil {
		t.Fatalf("SessionInfoFromStoredSession: %v", err)
	}
	if info.ChatCapabilities == nil {
		t.Fatal("capabilities should not be nil after downgrade")
	}
	if info.ChatCapabilities.StructuredTimeline {
		t.Error("StructuredTimeline should be false after downgrade")
	}
	if info.ChatCapabilities.DefaultView != ChatDefaultViewTerminal {
		t.Errorf("DefaultView = %q, want terminal", info.ChatCapabilities.DefaultView)
	}

	// Verify chat.reset with adapter_downgrade reason was broadcast.
	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	var foundReset bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatReset {
			var payload ChatResetPayload
			raw, _ := json.Marshal(msg.Payload)
			json.Unmarshal(raw, &payload)
			if payload.Reason == ChatResetReasonAdapterDowngrade {
				foundReset = true
			}
		}
	}
	if !foundReset {
		t.Errorf("no chat.reset with adapter_downgrade reason; got: %v", messageTypes(msgs))
	}
}

// TestJSONLRevisionMonotonicity verifies that revision numbers always increase
// across boot replay, incremental updates, and provider resets.
func TestJSONLRevisionMonotonicity(t *testing.T) {
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	controller := NewStructuredChatController(store)

	// Simulate a sequence of operations and verify revisions increase.
	// Each operation's revision must be strictly greater than the previous
	// operation's revision. Messages within the same operation share a revision.
	var lastOperationRevision int64

	// Helper to extract the max revision from a set of operation messages.
	maxRevFromMsgs := func(msgs []Message) int64 {
		var maxRev int64
		for _, msg := range msgs {
			rev := extractLifecycleRevision(t, msg)
			if rev > maxRev {
				maxRev = rev
			}
		}
		return maxRev
	}

	// 1. Initial merge (boot replay).
	items1 := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 1000, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "hello"},
	}
	msgs, err := controller.MergeProviderTimeline("s1", items1)
	if err != nil {
		t.Fatalf("MergeProviderTimeline 1: %v", err)
	}
	rev := maxRevFromMsgs(msgs)
	if rev <= lastOperationRevision {
		t.Fatalf("revision %d not > %d after initial merge", rev, lastOperationRevision)
	}
	lastOperationRevision = rev

	// 2. Incremental update.
	items2 := append(items1, ChatItem{
		ID: "jsonl-text:a1:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 2000, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "world",
	})
	msgs, err = controller.MergeProviderTimeline("s1", items2)
	if err != nil {
		t.Fatalf("MergeProviderTimeline 2: %v", err)
	}
	rev = maxRevFromMsgs(msgs)
	if rev <= lastOperationRevision {
		t.Fatalf("revision %d not > %d after incremental update", rev, lastOperationRevision)
	}
	lastOperationRevision = rev

	// 3. Provider reset (NextRevision for chat.reset).
	revN, err := controller.NextRevision("s1")
	if err != nil {
		t.Fatalf("NextRevision: %v", err)
	}
	if revN <= lastOperationRevision {
		t.Fatalf("reset revision %d not > %d", revN, lastOperationRevision)
	}
	lastOperationRevision = revN

	// 4. Post-reset merge (produces revision N+1).
	items3 := []ChatItem{
		{ID: "jsonl-user:u2", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 3000, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "rebuilt"},
	}
	msgs, err = controller.MergeProviderTimeline("s1", items3)
	if err != nil {
		t.Fatalf("MergeProviderTimeline 3: %v", err)
	}
	rev = maxRevFromMsgs(msgs)
	if rev <= lastOperationRevision {
		t.Fatalf("revision %d not > %d after post-reset merge", rev, lastOperationRevision)
	}
}

// TestJSONLPromptSeedAndReset verifies that provider_reset clears the pending
// seed, preventing duplicate ghost bubbles on the next JSONL arrival.
func TestJSONLPromptSeedAndReset(t *testing.T) {
	parser := newJSONLEventParser("session-1")

	// Seed a prompt.
	parser.SeedPromptWithItem("hello", "req-1", 1000)

	items := parser.timeline()
	if len(items) != 1 {
		t.Fatalf("after seed: len(items) = %d, want 1", len(items))
	}
	if items[0].ID != "jsonl-user:req-1" {
		t.Errorf("seeded item ID = %q, want jsonl-user:req-1", items[0].ID)
	}

	// Provider reset clears the seed.
	parser.Reset()

	if parser.pendingSeed != nil {
		t.Error("pending seed should be nil after Reset()")
	}
	items = parser.timeline()
	if len(items) != 0 {
		t.Fatalf("after reset: len(items) = %d, want 0", len(items))
	}

	// Next JSONL user event gets a fresh ID (not the seeded one).
	line := []byte(makeJSONLUserEvent("uuid-fresh", "hello", time.UnixMilli(2000)))
	items = parser.ParseEvent(line)
	if len(items) != 1 {
		t.Fatalf("after parse: len(items) = %d, want 1", len(items))
	}
	if items[0].ID != "jsonl-user:uuid-fresh" {
		t.Errorf("item ID = %q, want jsonl-user:uuid-fresh (seed should not match after reset)", items[0].ID)
	}
}

// TestJSONLSeedConsumedByEcho verifies that when a JSONL user event matches
// a seeded prompt, the item is updated in place (not duplicated).
func TestJSONLSeedConsumedByEcho(t *testing.T) {
	parser := newJSONLEventParser("session-1")

	// Seed a prompt (adds item to timeline).
	parser.SeedPromptWithItem("hello", "req-1", 1000)

	// JSONL echo arrives with matching text.
	line := []byte(makeJSONLUserEvent("uuid-echo", "hello", time.UnixMilli(2000)))
	items := parser.ParseEvent(line)

	// Should have exactly one item (seed replaced, not duplicated).
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (no duplicate)", len(items))
	}
	if items[0].ID != "jsonl-user:req-1" {
		t.Errorf("item ID = %q, want jsonl-user:req-1 (should reuse seed ID)", items[0].ID)
	}
	// Timestamp should be updated to the JSONL event's timestamp.
	if items[0].CreatedAt != 2000 {
		t.Errorf("CreatedAt = %d, want 2000 (should use JSONL timestamp)", items[0].CreatedAt)
	}
}

// TestJSONLRollbackSeedItem verifies that RollbackSeedItem removes the seeded
// item from the timeline and clears the pending seed.
func TestJSONLRollbackSeedItem(t *testing.T) {
	parser := newJSONLEventParser("session-1")

	// Seed a prompt.
	parser.SeedPromptWithItem("hello", "req-1", 1000)
	if len(parser.timeline()) != 1 {
		t.Fatalf("after seed: len(items) = %d, want 1", len(parser.timeline()))
	}

	// Rollback removes the seeded item.
	parser.RollbackSeedItem("req-1")

	items := parser.timeline()
	if len(items) != 0 {
		t.Fatalf("after rollback: len(items) = %d, want 0", len(items))
	}
	if parser.pendingSeed != nil {
		t.Error("pending seed should be nil after rollback")
	}
}

// TestJSONLPipelineStopCleansUp verifies that StopSession cleans up
// pipeline resources.
func TestJSONLPipelineStopCleansUp(t *testing.T) {
	runtime := &structuredChatRuntime{
		pipelines: make(map[string]structuredPipeline),
	}

	// Use a simple mock pipeline that tracks Stop() calls.
	stopped := false
	runtime.mu.Lock()
	runtime.pipelines["session-1"] = &mockPipeline{onStop: func() { stopped = true }}
	runtime.mu.Unlock()

	runtime.StopSession("session-1")

	runtime.mu.Lock()
	_, exists := runtime.pipelines["session-1"]
	runtime.mu.Unlock()

	if exists {
		t.Error("pipeline should be removed after stop")
	}
	if !stopped {
		t.Error("pipeline Stop() should have been called")
	}
}

// TestStopSessionCleansUpPipeline verifies that the public StopSession
// interface method (called by handleSessionClose) cleans up the pipeline.
func TestStopSessionCleansUpPipeline(t *testing.T) {
	runtime := &structuredChatRuntime{
		pipelines: make(map[string]structuredPipeline),
	}

	stopped := false
	runtime.mu.Lock()
	runtime.pipelines["session-close"] = &mockPipeline{onStop: func() { stopped = true }}
	runtime.mu.Unlock()

	runtime.StopSession("session-close")

	runtime.mu.Lock()
	_, exists := runtime.pipelines["session-close"]
	runtime.mu.Unlock()

	if exists {
		t.Error("pipeline should be removed after StopSession")
	}
	if !stopped {
		t.Error("pipeline Stop() should have been called")
	}
}

// TestStopSessionNoopForUnknownSession verifies StopSession is safe to call
// for sessions without a pipeline.
func TestStopSessionNoopForUnknownSession(t *testing.T) {
	runtime := &structuredChatRuntime{
		pipelines: make(map[string]structuredPipeline),
	}

	// Should not panic when no pipeline exists for the session.
	runtime.StopSession("nonexistent-session")
}

// TestJSONLSeedPromptViaRuntime verifies the runtime's SeedPrompt method
// works through the pipeline path.
func TestJSONLSeedPromptViaRuntime(t *testing.T) {
	launchTime := time.Now()
	store, controller, _, broadcastPtr := newJSONLTestRuntime(t, launchTime)

	// Create a Claude pipeline manually (bypassing watcher).
	parser := newJSONLEventParser("session-1")
	pipe := &claudePipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast: func(msg Message) {
			*broadcastPtr = append(*broadcastPtr, msg)
		},
	}

	runtime := &structuredChatRuntime{
		store:      store,
		controller: controller,
		broadcast: func(msg Message) {
			*broadcastPtr = append(*broadcastPtr, msg)
		},
		pipelines: make(map[string]structuredPipeline),
	}
	runtime.mu.Lock()
	runtime.pipelines["session-1"] = pipe
	runtime.mu.Unlock()

	// SeedPrompt should use the pipeline path.
	err := runtime.SeedPrompt("session-1", "hello", "req-1", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	if len(*broadcastPtr) == 0 {
		t.Fatal("expected broadcast message after SeedPrompt")
	}

	// Verify the seeded item was merged.
	found := false
	for _, msg := range *broadcastPtr {
		if msg.Type == MessageTypeChatUpsert {
			found = true
		}
	}
	if !found {
		t.Errorf("expected chat.upsert message; got: %v", messageTypes(*broadcastPtr))
	}
}

// TestJSONLRollbackPromptViaRuntime verifies the runtime's RollbackPrompt method
// works through the pipeline path.
func TestJSONLRollbackPromptViaRuntime(t *testing.T) {
	launchTime := time.Now()
	store, controller, _, broadcastPtr := newJSONLTestRuntime(t, launchTime)

	parser := newJSONLEventParser("session-1")
	pipe := &claudePipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
		broadcast: func(msg Message) {
			*broadcastPtr = append(*broadcastPtr, msg)
		},
	}

	runtime := &structuredChatRuntime{
		store:      store,
		controller: controller,
		broadcast: func(msg Message) {
			*broadcastPtr = append(*broadcastPtr, msg)
		},
		pipelines: make(map[string]structuredPipeline),
	}
	runtime.mu.Lock()
	runtime.pipelines["session-1"] = pipe
	runtime.mu.Unlock()

	// Seed then rollback.
	if err := runtime.SeedPrompt("session-1", "hello", "req-1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	*broadcastPtr = nil // clear

	if err := runtime.RollbackPrompt("session-1", "req-1"); err != nil {
		t.Fatalf("RollbackPrompt: %v", err)
	}

	// Should have broadcast a remove message.
	found := false
	for _, msg := range *broadcastPtr {
		if msg.Type == MessageTypeChatRemove {
			found = true
		}
	}
	if !found {
		t.Errorf("expected chat.remove message after rollback; got: %v", messageTypes(*broadcastPtr))
	}
}

// extractLifecycleRevision extracts the revision from a chat message payload.
func extractLifecycleRevision(t *testing.T, msg Message) int64 {
	t.Helper()
	raw, err := json.Marshal(msg.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var obj struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal revision: %v", err)
	}
	return obj.Revision
}

// messageTypes returns a list of message types for debugging.
func messageTypes(msgs []Message) []string {
	types := make([]string, len(msgs))
	for i, msg := range msgs {
		types[i] = fmt.Sprintf("%s", msg.Type)
	}
	return types
}
