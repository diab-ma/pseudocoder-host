package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/storage"
)

func TestClassifyStructuredChatSessionClaude(t *testing.T) {
	tests := []struct {
		command string
		args    []string
	}{
		{command: "claude"},
		{command: "/usr/local/bin/claude"},
		{command: "ANTHROPIC_API_KEY=test /opt/bin/claude --print"},
	}

	for _, tt := range tests {
		info := classifyStructuredChatSession(tt.command, tt.args, "")
		if info.SessionKind != SessionKindAgent {
			t.Fatalf("classify(%q) session kind = %q, want agent", tt.command, info.SessionKind)
		}
		if info.AgentProvider != AgentProviderClaude {
			t.Fatalf("classify(%q) agent provider = %q, want claude", tt.command, info.AgentProvider)
		}
		if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
			t.Fatalf("classify(%q) capabilities = %#v", tt.command, info.ChatCapabilities)
		}
	}

	for _, command := range []string{"my-claude-wrapper", "claude-code", "/tmp/notclaude"} {
		info := classifyStructuredChatSession(command, nil, "")
		if info.SessionKind != SessionKindShell || info.AgentProvider != AgentProviderUnknown {
			t.Fatalf("classify(%q) = (%q,%q), want (shell,unknown)", command, info.SessionKind, info.AgentProvider)
		}
	}
}

func TestClassifyStructuredChatSessionCodex(t *testing.T) {
	tests := []struct {
		command string
		args    []string
	}{
		{command: "codex"},
		{command: "/usr/local/bin/codex"},
		{command: "OPENAI_API_KEY=test /opt/bin/codex --print"},
	}

	for _, tt := range tests {
		info := classifyStructuredChatSession(tt.command, tt.args, "")
		if info.SessionKind != SessionKindAgent {
			t.Fatalf("classify(%q) session kind = %q, want agent", tt.command, info.SessionKind)
		}
		if info.AgentProvider != AgentProviderCodex {
			t.Fatalf("classify(%q) agent provider = %q, want codex", tt.command, info.AgentProvider)
		}
		if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
			t.Fatalf("classify(%q) capabilities = %#v", tt.command, info.ChatCapabilities)
		}
		if info.ChatCapabilities.DefaultView != ChatDefaultViewChat {
			t.Fatalf("classify(%q) default view = %q, want chat", tt.command, info.ChatCapabilities.DefaultView)
		}
	}

	for _, command := range []string{"codex-wrapper", "mycodex", "/tmp/notcodex"} {
		info := classifyStructuredChatSession(command, nil, "")
		if info.SessionKind != SessionKindShell || info.AgentProvider != AgentProviderUnknown {
			t.Fatalf("classify(%q) = (%q,%q), want (shell,unknown)", command, info.SessionKind, info.AgentProvider)
		}
	}
}

func TestClassifyStructuredChatSessionTmuxWins(t *testing.T) {
	info := classifyStructuredChatSession("claude", nil, "dev")
	if info.SessionKind != SessionKindTmux {
		t.Fatalf("SessionKind = %q, want tmux", info.SessionKind)
	}
	if info.AgentProvider != AgentProviderNone {
		t.Fatalf("AgentProvider = %q, want none", info.AgentProvider)
	}
	if info.ChatCapabilities != nil {
		t.Fatalf("ChatCapabilities = %#v, want nil", info.ChatCapabilities)
	}
}

func TestSessionInfoStructuredChatMetadataRehydratesFromStorage(t *testing.T) {
	session := &storage.Session{
		ID:                   "session-1",
		Repo:                 "/repo",
		Branch:               "main",
		StartedAt:            time.UnixMilli(1),
		LastSeen:             time.UnixMilli(2),
		Status:               storage.SessionStatusRunning,
		SessionKind:          "agent",
		AgentProvider:        "codex",
		ChatCapabilitiesJSON: `{"structured_timeline":true,"markdown":true,"tool_cards":false,"approval_inline":false,"transcript_fallback":true,"default_view":"chat"}`,
	}

	info, err := SessionInfoFromStoredSession(session)
	if err != nil {
		t.Fatalf("SessionInfoFromStoredSession failed: %v", err)
	}
	if info.SessionKind != SessionKindAgent || info.AgentProvider != AgentProviderCodex {
		t.Fatalf("rehydrated metadata = (%q,%q)", info.SessionKind, info.AgentProvider)
	}
	if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
		t.Fatalf("rehydrated capabilities = %#v", info.ChatCapabilities)
	}
}

func TestSessionInfoStructuredChatMetadataRehydratesGeminiFromStorage(t *testing.T) {
	session := &storage.Session{
		ID:                   "session-1",
		Repo:                 "/repo",
		Branch:               "main",
		StartedAt:            time.UnixMilli(1),
		LastSeen:             time.UnixMilli(2),
		Status:               storage.SessionStatusRunning,
		SessionKind:          "agent",
		AgentProvider:        "gemini",
		ChatCapabilitiesJSON: `{"structured_timeline":true,"markdown":true,"tool_cards":false,"approval_inline":false,"transcript_fallback":true,"default_view":"chat"}`,
	}

	info, err := SessionInfoFromStoredSession(session)
	if err != nil {
		t.Fatalf("SessionInfoFromStoredSession failed: %v", err)
	}
	if info.SessionKind != SessionKindAgent || info.AgentProvider != AgentProviderGemini {
		t.Fatalf("rehydrated metadata = (%q,%q)", info.SessionKind, info.AgentProvider)
	}
	if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
		t.Fatalf("rehydrated capabilities = %#v", info.ChatCapabilities)
	}
}

func TestStructuredChatReplaySkipsDowngradedClaudeSnapshot(t *testing.T) {
	store := newStructuredRuntimeSessionStore(t, SessionInfo{
		SessionKind:      SessionKindAgent,
		AgentProvider:    AgentProviderClaude,
		ChatCapabilities: cloneChatCapabilities(claudeDowngradedCapabilities),
	})
	defer store.Close()

	if err := store.ReplaceStructuredChatSnapshot(&storage.StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  2,
		UpdatedAt: time.Now().UTC(),
		Items: []storage.StructuredChatStoredItem{
			testServerStructuredChatStoredItem("session-1", "item-1", 1),
		},
	}); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(NewStructuredChatController(store))
	s.SetSessionStore(store)
	manager := pty.NewSessionManager()
	if err := manager.Register(pty.NewSession(pty.SessionConfig{ID: "session-1"})); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	s.SetSessionManager(manager)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	writeClientMessage(t, conn, Message{
		Type: MessageTypeSessionSwitch,
		Payload: SessionSwitchPayload{
			SessionID: "session-1",
		},
	})

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("expected session.buffer, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no chat.snapshot for downgraded claude session, got %s", msg.Type)
	}
}

func TestStructuredChatReplaySkipsDowngradedCodexSnapshot(t *testing.T) {
	store := newStructuredRuntimeSessionStore(t, SessionInfo{
		SessionKind:      SessionKindAgent,
		AgentProvider:    AgentProviderCodex,
		ChatCapabilities: cloneChatCapabilities(codexDowngradedCapabilities),
	})
	defer store.Close()

	if err := store.ReplaceStructuredChatSnapshot(&storage.StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  2,
		UpdatedAt: time.Now().UTC(),
		Items: []storage.StructuredChatStoredItem{
			testServerStructuredChatStoredItem("session-1", "item-1", 1),
		},
	}); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(NewStructuredChatController(store))
	s.SetSessionStore(store)
	manager := pty.NewSessionManager()
	if err := manager.Register(pty.NewSession(pty.SessionConfig{ID: "session-1"})); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	s.SetSessionManager(manager)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	writeClientMessage(t, conn, Message{
		Type: MessageTypeSessionSwitch,
		Payload: SessionSwitchPayload{
			SessionID: "session-1",
		},
	})

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("expected session.buffer, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no chat.snapshot for downgraded codex session, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchSkipsDowngradedCodexSnapshot(t *testing.T) {
	TestStructuredChatReplaySkipsDowngradedCodexSnapshot(t)
}

func newStructuredRuntimeSessionStore(t *testing.T, metadata SessionInfo) *storage.SQLiteStore {
	t.Helper()

	return newStructuredRuntimeSessionStoreForSession(t, "session-1", metadata)
}

func newStructuredRuntimeSessionStoreForSession(t *testing.T, sessionID string, metadata SessionInfo) *storage.SQLiteStore {
	t.Helper()

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	session := &storage.Session{
		ID:        sessionID,
		Repo:      "/repo",
		Branch:    "main",
		StartedAt: time.UnixMilli(1),
		LastSeen:  time.UnixMilli(1),
		Status:    storage.SessionStatusRunning,
	}
	if err := ApplySessionMetadataToStorage(session, metadata); err != nil {
		t.Fatalf("ApplySessionMetadataToStorage failed: %v", err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	return store
}

func TestStructuredChatRuntimeStructuredCapabilitiesEnableToolCardsAndApprovalInline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata SessionInfo
	}{
		{name: "claude", metadata: ClassifySessionMetadata("claude", nil, "")},
		{name: "codex", metadata: ClassifySessionMetadata("codex", nil, "")},
		{name: "gemini", metadata: ClassifySessionMetadata("gemini", nil, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.metadata.ChatCapabilities == nil {
				t.Fatal("ChatCapabilities is nil")
			}
			if !tc.metadata.ChatCapabilities.ToolCards {
				t.Fatal("ToolCards = false, want true")
			}
			if !tc.metadata.ChatCapabilities.ApprovalInline {
				t.Fatal("ApprovalInline = false, want true")
			}
		})
	}
}

func TestClassifyStructuredChatSessionGemini(t *testing.T) {
	tests := []struct {
		command string
		args    []string
	}{
		{command: "gemini"},
		{command: "/usr/local/bin/gemini"},
		{command: "GOOGLE_API_KEY=test /opt/bin/gemini --print"},
	}

	for _, tt := range tests {
		info := classifyStructuredChatSession(tt.command, tt.args, "")
		if info.SessionKind != SessionKindAgent {
			t.Fatalf("classify(%q) session kind = %q, want agent", tt.command, info.SessionKind)
		}
		if info.AgentProvider != AgentProviderGemini {
			t.Fatalf("classify(%q) agent provider = %q, want gemini", tt.command, info.AgentProvider)
		}
		if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
			t.Fatalf("classify(%q) capabilities = %#v", tt.command, info.ChatCapabilities)
		}
		if info.ChatCapabilities.DefaultView != ChatDefaultViewChat {
			t.Fatalf("classify(%q) default view = %q, want chat", tt.command, info.ChatCapabilities.DefaultView)
		}
	}

	for _, command := range []string{"gemini-wrapper", "mygemini", "/tmp/notgemini"} {
		info := classifyStructuredChatSession(command, nil, "")
		if info.SessionKind != SessionKindShell || info.AgentProvider != AgentProviderUnknown {
			t.Fatalf("classify(%q) = (%q,%q), want (shell,unknown)", command, info.SessionKind, info.AgentProvider)
		}
	}
}

func TestStructuredChatReplaySkipsDowngradedGeminiSnapshot(t *testing.T) {
	store := newStructuredRuntimeSessionStore(t, SessionInfo{
		SessionKind:      SessionKindAgent,
		AgentProvider:    AgentProviderGemini,
		ChatCapabilities: cloneChatCapabilities(geminiDowngradedCapabilities),
	})
	defer store.Close()

	if err := store.ReplaceStructuredChatSnapshot(&storage.StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  2,
		UpdatedAt: time.Now().UTC(),
		Items: []storage.StructuredChatStoredItem{
			testServerStructuredChatStoredItem("session-1", "item-1", 1),
		},
	}); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(NewStructuredChatController(store))
	s.SetSessionStore(store)
	manager := pty.NewSessionManager()
	if err := manager.Register(pty.NewSession(pty.SessionConfig{ID: "session-1"})); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	s.SetSessionManager(manager)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}

	writeClientMessage(t, conn, Message{
		Type: MessageTypeSessionSwitch,
		Payload: SessionSwitchPayload{
			SessionID: "session-1",
		},
	})

	msg := readMessage(t, conn)
	if msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("expected session.buffer, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no chat.snapshot for downgraded gemini session, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchSkipsDowngradedGeminiSnapshot(t *testing.T) {
	TestStructuredChatReplaySkipsDowngradedGeminiSnapshot(t)
}

func writeClientMessage(t *testing.T, conn *websocket.Conn, msg Message) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
}

// --- All providers now use file-based pipelines ---

func TestClaudeAlwaysSkipsPTY(t *testing.T) {
	// Claude → ObservePTYOutput is skipped (JSONL is always primary).
	// The JSONL pipeline IS started by ObservePTYOutput using observedAt
	// (not session.StartedAt) so the discovery window is fresh.
	store := newStructuredRuntimeSessionStore(t, ClassifySessionMetadata("claude", nil, ""))
	controller := NewStructuredChatController(store)
	var broadcast []Message
	runtime := NewStructuredChatRuntime(store, controller, func(msg Message) {
		broadcast = append(broadcast, msg)
	})

	now := time.Now().UnixMilli()
	runtime.ObservePTYOutput("session-1", "Human: hello\n", now)
	runtime.ObservePTYOutput("session-1", "Assistant: goodbye\n", now+1)

	// Stop the JSONL pipeline before the discovery timeout can fire.
	runtime.StopSession("session-1")

	// The only broadcast should be the initial empty snapshot from pipeline
	// startup — no PTY-parsed chat items.
	for _, msg := range broadcast {
		if msg.Type != MessageTypeChatSnapshot {
			t.Fatalf("unexpected broadcast type %q (Claude PTY should be skipped)", msg.Type)
		}
	}
}

func TestClaudeObservePTYStartsPipeline(t *testing.T) {
	// Verifies ObservePTYOutput for Claude DOES create a JSONL pipeline entry
	// so that chat mode works even when the user interacts only via terminal.
	store := newStructuredRuntimeSessionStore(t, ClassifySessionMetadata("claude", nil, ""))
	controller := NewStructuredChatController(store)
	var broadcast []Message
	rt := NewStructuredChatRuntime(store, controller, func(msg Message) {
		broadcast = append(broadcast, msg)
	})

	rt.ObservePTYOutput("session-1", "Human: hello\n", 1000)

	// Type-assert to inspect internal state (consistent with jsonl_lifecycle_test.go patterns).
	impl, ok := rt.(*structuredChatRuntime)
	if !ok {
		t.Fatal("expected *structuredChatRuntime")
	}

	impl.mu.Lock()
	_, exists := impl.pipelines["session-1"]
	impl.mu.Unlock()

	if !exists {
		t.Error("ObservePTYOutput SHOULD create a pipeline for Claude sessions")
	}

	// Watcher starts at pipeline creation so existing JSONL files are
	// discovered immediately. Pre-prompt timeouts are swallowed.
	pipe := impl.pipelines["session-1"].(*claudePipeline)
	pipe.mu.Lock()
	hasWatcher := pipe.watcher != nil
	pipe.mu.Unlock()
	if !hasWatcher {
		t.Error("watcher should start at pipeline creation")
	}

	// Clean up: stop the pipeline to avoid leaked goroutines.
	rt.StopSession("session-1")
}

func TestCodexObservePTYStartsPipeline(t *testing.T) {
	// Codex → all providers now use file-based pipelines, PTY is skipped.
	store := newStructuredRuntimeSessionStore(t, ClassifySessionMetadata("codex", nil, ""))
	controller := NewStructuredChatController(store)
	var broadcast []Message
	rt := NewStructuredChatRuntime(store, controller, func(msg Message) {
		broadcast = append(broadcast, msg)
	})

	rt.ObservePTYOutput("session-1", "User: hello\n", 1000)

	impl := rt.(*structuredChatRuntime)
	impl.mu.Lock()
	_, exists := impl.pipelines["session-1"]
	impl.mu.Unlock()

	if !exists {
		t.Error("ObservePTYOutput SHOULD create a pipeline for Codex sessions")
	}

	rt.StopSession("session-1")
}

func TestGeminiObservePTYStartsPipeline(t *testing.T) {
	// Gemini → all providers now use file-based pipelines, PTY is skipped.
	store := newStructuredRuntimeSessionStore(t, ClassifySessionMetadata("gemini", nil, ""))
	controller := NewStructuredChatController(store)
	var broadcast []Message
	rt := NewStructuredChatRuntime(store, controller, func(msg Message) {
		broadcast = append(broadcast, msg)
	})

	rt.ObservePTYOutput("session-1", "User: hello\n", 1000)

	impl := rt.(*structuredChatRuntime)
	impl.mu.Lock()
	_, exists := impl.pipelines["session-1"]
	impl.mu.Unlock()

	if !exists {
		t.Error("ObservePTYOutput SHOULD create a pipeline for Gemini sessions")
	}

	rt.StopSession("session-1")
}

func TestMultiSessionClaudeAndCodexCoexist(t *testing.T) {
	// Multi-session: Claude + Codex on same runtime. Both use file-based pipelines.
	storeClaudeSession := func(store *storage.SQLiteStore) {
		session := &storage.Session{
			ID:        "session-claude",
			Repo:      "/repo",
			Branch:    "main",
			StartedAt: time.UnixMilli(1),
			LastSeen:  time.UnixMilli(1),
			Status:    storage.SessionStatusRunning,
		}
		if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("claude", nil, "")); err != nil {
			t.Fatalf("ApplySessionMetadataToStorage failed: %v", err)
		}
		if err := store.SaveSession(session); err != nil {
			t.Fatalf("SaveSession failed: %v", err)
		}
	}
	storeCodexSession := func(store *storage.SQLiteStore) {
		session := &storage.Session{
			ID:        "session-codex",
			Repo:      "/repo",
			Branch:    "main",
			StartedAt: time.UnixMilli(1),
			LastSeen:  time.UnixMilli(1),
			Status:    storage.SessionStatusRunning,
		}
		if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata("codex", nil, "")); err != nil {
			t.Fatalf("ApplySessionMetadataToStorage failed: %v", err)
		}
		if err := store.SaveSession(session); err != nil {
			t.Fatalf("SaveSession failed: %v", err)
		}
	}

	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	storeClaudeSession(store)
	storeCodexSession(store)

	controller := NewStructuredChatController(store)
	var broadcast []Message
	rt := NewStructuredChatRuntime(store, controller, func(msg Message) {
		broadcast = append(broadcast, msg)
	})

	now := time.Now().UnixMilli()

	// Claude session: file-based pipeline started, initial empty snapshot broadcast.
	rt.ObservePTYOutput("session-claude", "Human: hello\n", now)
	snapshotCount := 0
	for _, msg := range broadcast {
		if msg.Type == MessageTypeChatSnapshot {
			snapshotCount++
		}
	}
	if snapshotCount != 1 {
		t.Fatalf("after Claude observe: snapshot count = %d, want 1", snapshotCount)
	}

	// Codex session: file-based pipeline started, initial empty snapshot broadcast.
	rt.ObservePTYOutput("session-codex", "User: hello\n", now+1)

	// Both sessions should have pipelines.
	impl := rt.(*structuredChatRuntime)
	impl.mu.Lock()
	claudeExists := impl.pipelines["session-claude"] != nil
	codexExists := impl.pipelines["session-codex"] != nil
	impl.mu.Unlock()

	if !claudeExists {
		t.Error("Claude pipeline should exist")
	}
	if !codexExists {
		t.Error("Codex pipeline should exist")
	}

	// Clean up pipelines to prevent leaked goroutines.
	rt.StopSession("session-claude")
	rt.StopSession("session-codex")
}
