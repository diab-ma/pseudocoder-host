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

// makeCodexUserLine creates a Codex JSONL line for a user message.
func makeCodexUserLine(t *testing.T, id, text string, ts float64) string {
	t.Helper()
	event := codexResponseItemEvent(ts, codexResponseItem{
		Type: "message",
		Role: "user",
		ID:   id,
		Content: []codexContentBlock{
			{Type: "input_text", Text: text},
		},
	})
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal codex user event: %v", err)
	}
	return string(b)
}

// makeCodexTaskStartedLine creates a Codex JSONL line for a task_started event_msg.
func makeCodexTaskStartedLine(t *testing.T, eventID string, ts float64) string {
	t.Helper()
	event := codexEventMsgEvent(ts, codexEventMsg{EventID: eventID, Type: "task_started"})
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal codex task_started event: %v", err)
	}
	return string(b)
}

// makeCodexAssistantLine creates a Codex JSONL line for an assistant message.
func makeCodexAssistantLine(t *testing.T, id, text string, ts float64) string {
	t.Helper()
	event := codexResponseItemEvent(ts, codexResponseItem{
		Type: "message",
		Role: "assistant",
		ID:   id,
		Content: []codexContentBlock{
			{Type: "output_text", Text: text},
		},
	})
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal codex assistant event: %v", err)
	}
	return string(b)
}

// newCodexTestController creates a SQLite store with a codex session and
// returns the store and controller for testing.
func newCodexTestController(t *testing.T, launchTime time.Time) (*storage.SQLiteStore, StructuredChatController) {
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
	if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("codex", nil, "")); err != nil {
		t.Fatalf("ApplySessionMetadataToStorage: %v", err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	controller := NewStructuredChatController(store)
	return store, controller
}

// writeCodexJSONLFile writes Codex JSONL lines to a file and returns its path.
func writeCodexJSONLFile(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var content string
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write codex JSONL file: %v", err)
	}
	return path
}

// TestCodexPipelineBootReplay verifies that on boot, the JSONL file is read
// from the beginning, MergeProviderTimeline is called once with the full list,
// and connected clients receive a chat.snapshot.
func TestCodexPipelineBootReplay(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsStart := float64(launchTime.Add(50*time.Millisecond).UnixMilli()) / 1000.0
	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	tsAssistant := float64(launchTime.Add(200*time.Millisecond).UnixMilli()) / 1000.0

	lines := []string{
		makeCodexTaskStartedLine(t, "evt-start-1", tsStart),
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
		makeCodexAssistantLine(t, "msg-a1", "world", tsAssistant),
	}
	filePath := writeCodexJSONLFile(t, dir, "codex-session.jsonl", lines)

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	readCompleteCh := make(chan struct{}, 1)

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filePath,
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipe.mu.Lock()
			pipe.parser.ParseEvent(line)
			if pipe.state != jsonlPipelineStateLive {
				pipe.mu.Unlock()
				return
			}
			items := pipe.parser.timeline()
			pipe.mu.Unlock()

			messages, err := controller.MergeProviderTimeline("session-1", items)
			if err != nil {
				t.Errorf("MergeProviderTimeline: %v", err)
				return
			}
			mu.Lock()
			broadcast = append(broadcast, messages...)
			mu.Unlock()
		},
		OnReadComplete: func() {
			pipe.mu.Lock()
			items := pipe.parser.timeline()
			pipe.state = jsonlPipelineStateLive
			pipe.mu.Unlock()

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
				broadcast = append(broadcast, *snapshotMsg)
				mu.Unlock()
			}
			readCompleteCh <- struct{}{}
		},
	})
	defer reader.Stop()

	pipe.mu.Lock()
	pipe.reader = reader
	pipe.mu.Unlock()

	reader.Start()

	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial read complete")
	}

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	var foundSnapshot bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			foundSnapshot = true
			var payload ChatSnapshotPayload
			raw, _ := json.Marshal(msg.Payload)
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("unmarshal snapshot payload: %v", err)
			}
			if len(payload.Items) != 3 {
				t.Fatalf("snapshot items = %d, want 3 (task_started + user + assistant)", len(payload.Items))
			}
			if payload.Items[0].Kind != ChatItemKindSystemEvent {
				t.Errorf("item[0].Kind = %q, want system_event (task_started)", payload.Items[0].Kind)
			}
			if payload.Items[1].Kind != ChatItemKindUserMessage {
				t.Errorf("item[1].Kind = %q, want user_message", payload.Items[1].Kind)
			}
			if payload.Items[1].Provider != ChatItemProviderCodex {
				t.Errorf("item[1].Provider = %q, want codex", payload.Items[1].Provider)
			}
			if payload.Items[2].Kind != ChatItemKindAssistant {
				t.Errorf("item[2].Kind = %q, want assistant", payload.Items[2].Kind)
			}
		}
	}
	if !foundSnapshot {
		t.Fatalf("no chat.snapshot message broadcast; got %d messages: %v", len(msgs), codexMessageTypes(msgs))
	}
}

// TestCodexPipelineLiveUpdates verifies that lines appended while the pipeline
// is live are merged and broadcast incrementally.
func TestCodexPipelineLiveUpdates(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsStart := float64(launchTime.Add(50*time.Millisecond).UnixMilli()) / 1000.0
	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	filePath := writeCodexJSONLFile(t, dir, "codex-live.jsonl", []string{
		makeCodexTaskStartedLine(t, "evt-start-1", tsStart),
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
	})

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	readCompleteCh := make(chan struct{}, 10)

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filePath,
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipe.mu.Lock()
			pipe.parser.ParseEvent(line)
			if pipe.state != jsonlPipelineStateLive {
				pipe.mu.Unlock()
				return
			}
			items := pipe.parser.timeline()
			pipe.mu.Unlock()

			messages, err := controller.MergeProviderTimeline("session-1", items)
			if err != nil {
				t.Errorf("MergeProviderTimeline: %v", err)
				return
			}
			mu.Lock()
			broadcast = append(broadcast, messages...)
			mu.Unlock()
		},
		OnReadComplete: func() {
			pipe.mu.Lock()
			items := pipe.parser.timeline()
			pipe.state = jsonlPipelineStateLive
			pipe.mu.Unlock()

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

	pipe.mu.Lock()
	pipe.reader = reader
	pipe.mu.Unlock()

	reader.Start()

	// Wait for initial read.
	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial read")
	}

	// Clear broadcast.
	mu.Lock()
	broadcast = nil
	mu.Unlock()

	// Append a new assistant line while live.
	tsAssistant := float64(launchTime.Add(300*time.Millisecond).UnixMilli()) / 1000.0
	newLine := makeCodexAssistantLine(t, "msg-a1", "live response", tsAssistant)
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(newLine + "\n"); err != nil {
		f.Close()
		t.Fatalf("append line: %v", err)
	}
	f.Close()

	// Wait for the live update to be broadcast.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, msg := range broadcast {
			if msg.Type == MessageTypeChatUpsert {
				found = true
			}
		}
		mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timeout waiting for live upsert; got %d messages: %v", len(broadcast), codexMessageTypes(broadcast))
			mu.Unlock()
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestCodexPipelineProviderReset verifies that on file truncation:
// 1. chat.reset is sent at revision N
// 2. File is re-read from beginning
// 3. chat.snapshot is sent at revision N+1
func TestCodexPipelineProviderReset(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsStart := float64(launchTime.Add(50*time.Millisecond).UnixMilli()) / 1000.0
	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	filePath := writeCodexJSONLFile(t, dir, "codex-reset.jsonl", []string{
		makeCodexTaskStartedLine(t, "evt-start-1", tsStart),
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
	})

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	readCompleteCh := make(chan struct{}, 10)

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filePath,
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipe.mu.Lock()
			pipe.parser.ParseEvent(line)
			if pipe.state != jsonlPipelineStateLive {
				pipe.mu.Unlock()
				return
			}
			items := pipe.parser.timeline()
			pipe.mu.Unlock()

			messages, _ := controller.MergeProviderTimeline("session-1", items)
			mu.Lock()
			broadcast = append(broadcast, messages...)
			mu.Unlock()
		},
		OnReset: func() {
			pipe.mu.Lock()
			pipe.parser.Reset()
			pipe.state = jsonlPipelineStateResetReplay
			pipe.mu.Unlock()
		},
		OnReadComplete: func() {
			pipe.mu.Lock()
			items := pipe.parser.timeline()
			prevState := pipe.state
			pipe.state = jsonlPipelineStateLive
			pipe.mu.Unlock()

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

	pipe.mu.Lock()
	pipe.reader = reader
	pipe.mu.Unlock()

	reader.Start()

	// Wait for initial read.
	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial read")
	}

	// Clear broadcast.
	mu.Lock()
	broadcast = nil
	mu.Unlock()

	// Truncate the file to trigger provider_reset.
	if err := os.Truncate(filePath, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Write new content (task_started required for non-seeded user messages).
	tsNewStart := float64(launchTime.Add(400*time.Millisecond).UnixMilli()) / 1000.0
	tsNew := float64(launchTime.Add(500*time.Millisecond).UnixMilli()) / 1000.0
	newContent := makeCodexTaskStartedLine(t, "evt-start-2", tsNewStart) + "\n" + makeCodexUserLine(t, "msg-u2", "after reset", tsNew) + "\n"
	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	// Wait for reset read complete.
	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reset read complete")
	}

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	// Verify: chat.reset before chat.snapshot.
	resetIdx, snapshotIdx := -1, -1
	for i, msg := range msgs {
		if msg.Type == MessageTypeChatReset {
			resetIdx = i
		}
		if msg.Type == MessageTypeChatSnapshot {
			snapshotIdx = i
		}
	}

	if resetIdx < 0 {
		t.Fatalf("no chat.reset message found; got: %v", codexMessageTypes(msgs))
	}
	if snapshotIdx < 0 {
		t.Fatalf("no chat.snapshot message found; got: %v", codexMessageTypes(msgs))
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

// TestCodexPipelineSeedPrompt verifies that SeedPrompt adds a user item to the
// timeline and broadcasts an upsert.
func TestCodexPipelineSeedPrompt(t *testing.T) {
	launchTime := time.Now()
	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	err := pipe.SeedPrompt("hello codex", "req-1", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	if len(msgs) == 0 {
		t.Fatal("expected broadcast after SeedPrompt")
	}

	found := false
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatUpsert {
			found = true
		}
	}
	if !found {
		t.Errorf("expected chat.upsert message; got: %v", codexMessageTypes(msgs))
	}
}

// TestCodexPipelineRollbackPrompt verifies that RollbackPrompt removes the
// seeded item and broadcasts a remove message.
func TestCodexPipelineRollbackPrompt(t *testing.T) {
	launchTime := time.Now()
	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	// Seed first.
	if err := pipe.SeedPrompt("hello codex", "req-1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("SeedPrompt: %v", err)
	}

	// Clear broadcast.
	mu.Lock()
	broadcast = nil
	mu.Unlock()

	// Rollback.
	if err := pipe.RollbackPrompt("req-1"); err != nil {
		t.Fatalf("RollbackPrompt: %v", err)
	}

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	found := false
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatRemove {
			found = true
		}
	}
	if !found {
		t.Errorf("expected chat.remove message after rollback; got: %v", codexMessageTypes(msgs))
	}
}

// TestCodexPipelineStopCleanup verifies that Stop() halts the reader and watcher.
func TestCodexPipelineStopCleanup(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	filePath := writeCodexJSONLFile(t, dir, "codex-stop.jsonl", []string{
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
	})

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
	}

	pipe.broadcast = func(msg Message) {}

	readCompleteCh := make(chan struct{}, 1)

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath:     filePath,
		PollInterval: 10 * time.Millisecond,
		OnLine: func(line []byte) {
			pipe.mu.Lock()
			pipe.parser.ParseEvent(line)
			pipe.mu.Unlock()
		},
		OnReadComplete: func() {
			pipe.mu.Lock()
			pipe.state = jsonlPipelineStateLive
			pipe.mu.Unlock()
			readCompleteCh <- struct{}{}
		},
	})

	pipe.mu.Lock()
	pipe.reader = reader
	pipe.mu.Unlock()

	reader.Start()

	select {
	case <-readCompleteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for read complete")
	}

	// Stop should not panic or hang.
	pipe.Stop()

	// Verify generation was incremented.
	pipe.mu.Lock()
	gen := pipe.generation
	pipe.mu.Unlock()
	if gen != 1 {
		t.Errorf("generation = %d, want 1 after stop", gen)
	}
}

// TestCodexPipelineLateCallbackAfterStop verifies that callbacks from a
// previous generation are silently ignored after Stop (FR26).
func TestCodexPipelineLateCallbackAfterStop(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	filePath := writeCodexJSONLFile(t, dir, "codex-late.jsonl", []string{
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
	})

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateLive,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	// Use onBound to wire up the reader with generation-checked callbacks.
	pipe.onBound(filePath)

	// Wait for initial read to complete.
	deadline := time.After(2 * time.Second)
	for {
		pipe.mu.Lock()
		live := pipe.state == jsonlPipelineStateLive
		pipe.mu.Unlock()
		if live {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pipeline to become live")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Clear broadcast.
	mu.Lock()
	broadcast = nil
	mu.Unlock()

	// Stop the pipeline to increment generation.
	pipe.Stop()

	// Append a new line after stop — even though the reader is stopped,
	// verify that if somehow a late callback fires it won't merge.
	tsNew := float64(launchTime.Add(500*time.Millisecond).UnixMilli()) / 1000.0
	newLine := makeCodexAssistantLine(t, "msg-a1", "late response", tsNew)
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	f.WriteString(newLine + "\n")
	f.Close()

	// Give time for any lingering callbacks.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	// No messages should have been broadcast after stop.
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatUpsert {
			t.Error("unexpected chat.upsert after stop — late callback was not suppressed")
		}
	}
}

// TestCodexPipelineOnBound verifies that the onBound method creates a reader
// that successfully reads and merges events.
func TestCodexPipelineOnBound(t *testing.T) {
	launchTime := time.Now()
	dir := t.TempDir()

	tsStart := float64(launchTime.Add(50*time.Millisecond).UnixMilli()) / 1000.0
	tsUser := float64(launchTime.Add(100*time.Millisecond).UnixMilli()) / 1000.0
	tsAssistant := float64(launchTime.Add(200*time.Millisecond).UnixMilli()) / 1000.0
	filePath := writeCodexJSONLFile(t, dir, "codex-bound.jsonl", []string{
		makeCodexTaskStartedLine(t, "evt-start-1", tsStart),
		makeCodexUserLine(t, "msg-u1", "hello", tsUser),
		makeCodexAssistantLine(t, "msg-a1", "world", tsAssistant),
	})

	_, controller := newCodexTestController(t, launchTime)

	parser := newCodexEventParser("session-1")
	pipe := &codexPipeline{
		sessionID:  "session-1",
		parser:     parser,
		state:      jsonlPipelineStateInitialReplay,
		controller: controller,
	}

	var mu sync.Mutex
	var broadcast []Message
	pipe.broadcast = func(msg Message) {
		mu.Lock()
		broadcast = append(broadcast, msg)
		mu.Unlock()
	}

	// Use the real onBound method.
	pipe.onBound(filePath)
	defer pipe.Stop()

	// Wait for pipeline to become live.
	deadline := time.After(2 * time.Second)
	for {
		pipe.mu.Lock()
		live := pipe.state == jsonlPipelineStateLive
		pipe.mu.Unlock()
		if live {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pipeline to become live via onBound")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Wait a bit for snapshot broadcast.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	msgs := make([]Message, len(broadcast))
	copy(msgs, broadcast)
	mu.Unlock()

	var foundSnapshot bool
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatSnapshot {
			foundSnapshot = true
			var payload ChatSnapshotPayload
			raw, _ := json.Marshal(msg.Payload)
			json.Unmarshal(raw, &payload)
			if len(payload.Items) != 3 {
				t.Errorf("snapshot items = %d, want 3 (task_started + user + assistant)", len(payload.Items))
			}
		}
	}
	if !foundSnapshot {
		t.Fatalf("no chat.snapshot broadcast from onBound; got: %v", codexMessageTypes(msgs))
	}
}

// TestCodexPipelineInterfaceCompliance verifies that codexPipeline satisfies
// the structuredPipeline interface at compile time.
func TestCodexPipelineInterfaceCompliance(t *testing.T) {
	var _ structuredPipeline = (*codexPipeline)(nil)
}

// codexMessageTypes returns a list of message types for debugging.
func codexMessageTypes(msgs []Message) []string {
	types := make([]string, len(msgs))
	for i, msg := range msgs {
		types[i] = fmt.Sprintf("%s", msg.Type)
	}
	return types
}
