package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/pseudocoder/host/internal/pty"
	"github.com/pseudocoder/host/internal/storage"
)

type testStructuredChatStore struct {
	snapshots  map[string]*storage.StructuredChatSnapshot
	loadErr    error
	loadErrors []error
	replaceErr error
}

func (s *testStructuredChatStore) LoadStructuredChatSnapshot(sessionID string) (*storage.StructuredChatSnapshot, error) {
	if len(s.loadErrors) > 0 {
		err := s.loadErrors[0]
		s.loadErrors = s.loadErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	snapshot, ok := s.snapshots[sessionID]
	if !ok {
		return nil, nil
	}
	return cloneStructuredChatSnapshot(snapshot), nil
}

func (s *testStructuredChatStore) ReplaceStructuredChatSnapshot(snapshot *storage.StructuredChatSnapshot) error {
	if s.replaceErr != nil {
		return s.replaceErr
	}
	if s.snapshots == nil {
		s.snapshots = make(map[string]*storage.StructuredChatSnapshot)
	}
	s.snapshots[snapshot.SessionID] = cloneStructuredChatSnapshot(snapshot)
	return nil
}

type fakeStructuredChatController struct {
	loadMessages map[string]*Message
	loadErr      error
	resets       map[string]Message
	applyMsgs    []Message
	replaceErr   error
	nextRevision int64
}

func (c *fakeStructuredChatController) LoadSnapshotMessage(sessionID string) (*Message, error) {
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	return c.loadMessages[sessionID], nil
}

func (c *fakeStructuredChatController) BuildSessionSwitchReset(sessionID string) Message {
	if msg, ok := c.resets[sessionID]; ok {
		return msg
	}
	return NewChatResetMessage(sessionID, "boot-test", 0, ChatResetReasonSessionSwitch)
}

func (c *fakeStructuredChatController) ApplyAuthoritativeTimeline(sessionID string, items []ChatItem) ([]Message, error) {
	if c.replaceErr != nil {
		return nil, c.replaceErr
	}
	return c.applyMsgs, nil
}

func (c *fakeStructuredChatController) MergeProviderTimeline(sessionID string, providerItems []ChatItem) ([]Message, error) {
	if c.replaceErr != nil {
		return nil, c.replaceErr
	}
	return c.applyMsgs, nil
}

func (c *fakeStructuredChatController) NextRevision(sessionID string) (int64, error) {
	if c.nextRevision == 0 {
		c.nextRevision = 1
	}
	return c.nextRevision, nil
}

func (c *fakeStructuredChatController) ServerBootID() string {
	return "boot-test"
}

func TestStructuredChatControllerBootIDReuse(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  3,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-1", 1),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	snapshotMsg, err := controller.LoadSnapshotMessage("session-1")
	if err != nil {
		t.Fatalf("LoadSnapshotMessage failed: %v", err)
	}
	resetMsg := controller.BuildSessionSwitchReset("session-1")

	snapshotPayload := snapshotMsg.Payload.(ChatSnapshotPayload)
	resetPayload := resetMsg.Payload.(ChatResetPayload)
	if snapshotPayload.ServerBootID == "" {
		t.Fatal("expected non-empty snapshot server_boot_id")
	}
	if snapshotPayload.ServerBootID != resetPayload.ServerBootID {
		t.Fatalf("server_boot_id mismatch: snapshot=%q reset=%q", snapshotPayload.ServerBootID, resetPayload.ServerBootID)
	}
}

func TestStructuredChatControllerReadOnlyReplayKeepsRevision(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  7,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-1", 1),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msg, err := controller.LoadSnapshotMessage("session-1")
	if err != nil {
		t.Fatalf("LoadSnapshotMessage failed: %v", err)
	}
	if msg.Payload.(ChatSnapshotPayload).Revision != 7 {
		t.Fatalf("snapshot revision = %d, want 7", msg.Payload.(ChatSnapshotPayload).Revision)
	}
	if got := store.snapshots["session-1"].Revision; got != 7 {
		t.Fatalf("stored revision changed to %d, want 7", got)
	}
}

func TestStructuredChatControllerApplyAuthoritativeTimelineDiffEmitsRemoveThenUpsert(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  2,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-gone", 1),
					testServerStructuredChatStoredItem("session-1", "item-keep", 2),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
		ID:        "item-keep",
		SessionID: "session-1",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 2,
		Provider:  ChatItemProviderClaude,
		Status:    ChatItemStatusCompleted,
		Text:      "updated",
	}, {
		ID:        "item-1",
		SessionID: "session-1",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 10,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Type != MessageTypeChatRemove {
		t.Fatalf("first message = %s, want chat.remove", msgs[0].Type)
	}
	if msgs[1].Type != MessageTypeChatUpsert {
		t.Fatalf("second message = %s, want chat.upsert", msgs[1].Type)
	}
	if got := msgs[0].Payload.(ChatRemovePayload).Revision; got != 3 {
		t.Fatalf("remove revision = %d, want 3", got)
	}
	if got := msgs[1].Payload.(ChatUpsertPayload).Revision; got != 4 {
		t.Fatalf("upsert revision = %d, want 4", got)
	}
	if got := store.snapshots["session-1"].Revision; got != 4 {
		t.Fatalf("stored revision = %d, want 4", got)
	}
}

func TestStructuredChatControllerApplyAuthoritativeTimelineNoChange(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  4,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-1", 1),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
		ID:        "item-1",
		SessionID: "session-1",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 1,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("len(msgs) = %d, want 0", len(msgs))
	}
	if got := store.snapshots["session-1"].Revision; got != 4 {
		t.Fatalf("stored revision = %d, want 4", got)
	}
}

func TestStructuredChatControllerApplyAuthoritativeTimelineReorderOnlyUpsert(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-1", 1),
					testServerStructuredChatStoredItem("session-1", "item-2", 2),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{
		{
			ID:        "item-2",
			SessionID: "session-1",
			Kind:      ChatItemKindUserMessage,
			CreatedAt: 2,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Text:      "hello",
		},
		{
			ID:        "item-1",
			SessionID: "session-1",
			Kind:      ChatItemKindUserMessage,
			CreatedAt: 1,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Type != MessageTypeChatUpsert {
		t.Fatalf("message type = %s, want chat.upsert", msgs[0].Type)
	}
	if got := len(msgs[0].Payload.(ChatUpsertPayload).Items); got != 2 {
		t.Fatalf("upsert item count = %d, want 2", got)
	}
}

func TestStructuredChatControllerApplyAuthoritativeTimelineDeleteAndReorderEmitsFullUpsert(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  8,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-1", "item-1", 1),
					testServerStructuredChatStoredItem("session-1", "item-2", 2),
					testServerStructuredChatStoredItem("session-1", "item-3", 3),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{
		{
			ID:        "item-3",
			SessionID: "session-1",
			Kind:      ChatItemKindUserMessage,
			CreatedAt: 3,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Text:      "hello",
		},
		{
			ID:        "item-2",
			SessionID: "session-1",
			Kind:      ChatItemKindUserMessage,
			CreatedAt: 2,
			Provider:  ChatItemProviderCodex,
			Status:    ChatItemStatusCompleted,
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Type != MessageTypeChatRemove {
		t.Fatalf("first message = %s, want chat.remove", msgs[0].Type)
	}
	if msgs[1].Type != MessageTypeChatUpsert {
		t.Fatalf("second message = %s, want chat.upsert", msgs[1].Type)
	}
	if got := msgs[0].Payload.(ChatRemovePayload).ItemIDs; len(got) != 1 || got[0] != "item-1" {
		t.Fatalf("removed ids = %#v, want [item-1]", got)
	}
	upsert := msgs[1].Payload.(ChatUpsertPayload)
	if len(upsert.Items) != 2 {
		t.Fatalf("upsert item count = %d, want 2", len(upsert.Items))
	}
	if upsert.Items[0].ID != "item-3" || upsert.Items[1].ID != "item-2" {
		t.Fatalf("upsert order = [%s %s], want [item-3 item-2]", upsert.Items[0].ID, upsert.Items[1].ID)
	}
	if got := msgs[0].Payload.(ChatRemovePayload).Revision; got != 9 {
		t.Fatalf("remove revision = %d, want 9", got)
	}
	if got := upsert.Revision; got != 10 {
		t.Fatalf("upsert revision = %d, want 10", got)
	}
}

func TestStructuredChatControllerApplyAuthoritativeTimelinePersistenceFailureSuppressesBroadcast(t *testing.T) {
	store := &testStructuredChatStore{
		replaceErr: errors.New("boom"),
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
		ID:        "item-1",
		SessionID: "session-1",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 1,
		Provider:  ChatItemProviderClaude,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected persistence error, got %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("len(msgs) = %d, want 0", len(msgs))
	}
}

func TestStructuredChatControllerSanitizesToolCallExcerpts(t *testing.T) {
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)
	longSecret := strings.Repeat("x", 600)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
		ID:            "tool-1",
		SessionID:     "session-1",
		Kind:          ChatItemKindToolCall,
		CreatedAt:     1,
		Provider:      ChatItemProviderSystem,
		Status:        ChatItemStatusCompleted,
		ToolName:      "exec",
		Summary:       "\x1b[31msearch repo\x1b[0m",
		InputExcerpt:  "Authorization: Bearer abc123 TOKEN=value API_KEY=secret",
		ResultExcerpt: "Bearer abc123 " + longSecret,
		StartedAt:     2,
		CompletedAt:   3,
	}})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}

	upsert := msgs[0].Payload.(ChatUpsertPayload)
	item := upsert.Items[0]
	if strings.Contains(item.Summary, "\x1b") {
		t.Fatalf("summary still contains ANSI: %q", item.Summary)
	}
	if strings.Contains(item.InputExcerpt, "abc123") || strings.Contains(item.InputExcerpt, "secret") || strings.Contains(item.InputExcerpt, "value") {
		t.Fatalf("input_excerpt still contains raw secret: %q", item.InputExcerpt)
	}
	if strings.Contains(item.ResultExcerpt, "abc123") {
		t.Fatalf("result_excerpt still contains raw bearer token: %q", item.ResultExcerpt)
	}
	if got := utf8.RuneCountInString(item.ResultExcerpt); got > 500 {
		t.Fatalf("result_excerpt rune count = %d, want <= 500", got)
	}
}

func TestStructuredChatControllerSanitizesToolCallBeforeBroadcastAndPersistence(t *testing.T) {
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
		ID:            "tool-1",
		SessionID:     "session-1",
		Kind:          ChatItemKindToolCall,
		CreatedAt:     1,
		Provider:      ChatItemProviderSystem,
		Status:        ChatItemStatusCompleted,
		ToolName:      "exec",
		Summary:       "\x1b[32mrun\x1b[0m",
		InputExcerpt:  "PASSWORD=hunter2",
		ResultExcerpt: "Authorization: Bearer secret-token",
		StartedAt:     2,
		CompletedAt:   3,
	}})
	if err != nil {
		t.Fatalf("ApplyAuthoritativeTimeline failed: %v", err)
	}

	upsert := msgs[0].Payload.(ChatUpsertPayload)
	broadcastItem := upsert.Items[0]
	snapshot, err := store.LoadStructuredChatSnapshot("session-1")
	if err != nil {
		t.Fatalf("LoadStructuredChatSnapshot failed: %v", err)
	}
	if snapshot == nil || len(snapshot.Items) != 1 {
		t.Fatalf("snapshot = %#v, want one item", snapshot)
	}
	storedItems, err := decodeStoredChatItems(snapshot.Items)
	if err != nil {
		t.Fatalf("decodeStoredChatItems failed: %v", err)
	}
	storedItem := storedItems[0]

	if broadcastItem.InputExcerpt != storedItem.InputExcerpt {
		t.Fatalf("broadcast input_excerpt = %q, stored = %q", broadcastItem.InputExcerpt, storedItem.InputExcerpt)
	}
	if broadcastItem.ResultExcerpt != storedItem.ResultExcerpt {
		t.Fatalf("broadcast result_excerpt = %q, stored = %q", broadcastItem.ResultExcerpt, storedItem.ResultExcerpt)
	}
	if strings.Contains(storedItem.InputExcerpt, "hunter2") || strings.Contains(storedItem.ResultExcerpt, "secret-token") {
		t.Fatalf("stored excerpts still contain raw secrets: %#v", storedItem)
	}
}

func TestStructuredChatControllerBuildSessionSwitchReset(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-9": {
				SessionID: "session-9",
				Revision:  5,
			},
		},
	}
	controller := NewStructuredChatController(store)

	msg := controller.BuildSessionSwitchReset("session-9")
	payload := msg.Payload.(ChatResetPayload)
	if payload.SessionID != "session-9" {
		t.Fatalf("SessionID = %q, want session-9", payload.SessionID)
	}
	if payload.Revision != 5 {
		t.Fatalf("Revision = %d, want 5", payload.Revision)
	}
	if payload.Reason != ChatResetReasonSessionSwitch {
		t.Fatalf("Reason = %q, want %q", payload.Reason, ChatResetReasonSessionSwitch)
	}
}

func TestStructuredChatControllerBuildSessionSwitchResetReusesLoadedRevision(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-9": {
				SessionID: "session-9",
				Revision:  5,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("session-9", "item-1", 1),
				},
			},
		},
		loadErrors: []error{nil, errors.New("boom")},
	}
	controller := NewStructuredChatController(store)

	msg, err := controller.LoadSnapshotMessage("session-9")
	if err != nil {
		t.Fatalf("LoadSnapshotMessage failed: %v", err)
	}
	if got := msg.Payload.(ChatSnapshotPayload).Revision; got != 5 {
		t.Fatalf("snapshot revision = %d, want 5", got)
	}

	reset := controller.BuildSessionSwitchReset("session-9")
	if got := reset.Payload.(ChatResetPayload).Revision; got != 5 {
		t.Fatalf("reset revision = %d, want 5", got)
	}
}

func TestStructuredChatControllerMalformedStoredPayloadFailsReplay(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"session-1": {
				SessionID: "session-1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{{
					ItemID:      "broken",
					Position:    0,
					PayloadJSON: "{",
					CreatedAt:   time.Now().UTC(),
				}},
			},
		},
	}
	controller := NewStructuredChatController(store)

	if _, err := controller.LoadSnapshotMessage("session-1"); err == nil || !strings.Contains(err.Error(), "decode stored chat item") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestStructuredChatReconnectReplay(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	snapshot := NewChatSnapshotMessage(s.SessionID(), "boot-1", 4, []ChatItem{{
		ID:        "item-1",
		SessionID: s.SessionID(),
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 1,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}})
	controller := &fakeStructuredChatController{
		loadMessages: map[string]*Message{s.SessionID(): &snapshot},
	}
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(controller)
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		if !s.structuredChatEnabled || s.structuredChatController == nil {
			return
		}
		msg, err := s.structuredChatController.LoadSnapshotMessage(s.SessionID())
		if err == nil && msg != nil {
			sendToClient(*msg)
		}
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}
	snapshotMsg := readMessage(t, conn)
	if snapshotMsg.Type != MessageTypeChatSnapshot {
		t.Fatalf("expected chat.snapshot, got %s", snapshotMsg.Type)
	}
	snapshotPayload := mustMarshalToMap(t, snapshotMsg.Payload)
	assertStringField(t, snapshotPayload, "server_boot_id", "boot-1")
}

func TestStructuredChatReconnectReplayDisabled(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	snapshot := NewChatSnapshotMessage(s.SessionID(), "boot-1", 4, nil)
	controller := &fakeStructuredChatController{
		loadMessages: map[string]*Message{s.SessionID(): &snapshot},
	}
	s.SetStructuredChatEnabled(false)
	s.SetStructuredChatController(controller)
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		if !s.structuredChatEnabled || s.structuredChatController == nil {
			return
		}
		msg, err := s.structuredChatController.LoadSnapshotMessage(s.SessionID())
		if err == nil && msg != nil {
			sendToClient(*msg)
		}
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no structured replay, got %s", msg.Type)
	}
}

func TestStructuredChatReconnectReplayDefaultSessionOnly(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	other := NewChatSnapshotMessage("other-session", "boot-1", 4, nil)
	controller := &fakeStructuredChatController{
		loadMessages: map[string]*Message{"other-session": &other},
	}
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(controller)
	s.SetReconnectHandler(func(sendToClient func(Message)) {
		if !s.structuredChatEnabled || s.structuredChatController == nil {
			return
		}
		msg, err := s.structuredChatController.LoadSnapshotMessage(s.SessionID())
		if err == nil && msg != nil {
			sendToClient(*msg)
		}
	})

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionStatus {
		t.Fatalf("expected session.status, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no replay for non-default snapshot only, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchResetThenSnapshot(t *testing.T) {
	s, conn, cleanup := newStructuredChatSwitchTestConnection(t)
	defer cleanup()

	snapshot := NewChatSnapshotMessage("switch-session", "boot-1", 3, []ChatItem{{
		ID:        "item-1",
		SessionID: "switch-session",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 1,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}})
	controller := &fakeStructuredChatController{
		loadMessages: map[string]*Message{"switch-session": &snapshot},
		resets: map[string]Message{
			"switch-session": NewChatResetMessage("switch-session", "boot-1", 3, ChatResetReasonSessionSwitch),
		},
	}
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(controller)

	sendStructuredSwitch(t, conn, "switch-session")

	if msg := readMessage(t, conn); msg.Type != MessageTypeChatReset {
		t.Fatalf("expected chat.reset first, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeChatSnapshot {
		t.Fatalf("expected chat.snapshot second, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchMissingSnapshot(t *testing.T) {
	s, conn, cleanup := newStructuredChatSwitchTestConnection(t)
	defer cleanup()

	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(&fakeStructuredChatController{
		loadMessages: map[string]*Message{},
		resets:       map[string]Message{},
	})

	sendStructuredSwitch(t, conn, "switch-session")

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("expected only session.buffer, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no extra structured messages, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchResetThenSnapshotThenBuffer(t *testing.T) {
	s, conn, cleanup := newStructuredChatSwitchTestConnection(t)
	defer cleanup()

	snapshot := NewChatSnapshotMessage("switch-session", "boot-1", 3, nil)
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(&fakeStructuredChatController{
		loadMessages: map[string]*Message{"switch-session": &snapshot},
		resets: map[string]Message{
			"switch-session": NewChatResetMessage("switch-session", "boot-1", 3, ChatResetReasonSessionSwitch),
		},
	})

	sendStructuredSwitch(t, conn, "switch-session")

	if msg := readMessage(t, conn); msg.Type != MessageTypeChatReset {
		t.Fatalf("expected chat.reset first, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeChatSnapshot {
		t.Fatalf("expected chat.snapshot second, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("expected session.buffer third, got %s", msg.Type)
	}
}

func TestStructuredChatSessionSwitchReplayRequesterScoped(t *testing.T) {
	s := NewServer(":0")
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	session, err := mgr.CreateWithID("switch-session", pty.SessionConfig{HistoryLines: 32})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}
	session.PrependToBuffer("line1\nline2")

	snapshot := NewChatSnapshotMessage("switch-session", "boot-1", 3, nil)
	s.SetStructuredChatEnabled(true)
	s.SetStructuredChatController(&fakeStructuredChatController{
		loadMessages: map[string]*Message{"switch-session": &snapshot},
		resets: map[string]Message{
			"switch-session": NewChatResetMessage("switch-session", "boot-1", 3, ChatResetReasonSessionSwitch),
		},
	})

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()
	defer mgr.CloseAll()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	connA, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("connA dial failed: %v", err)
	}
	defer connA.Close()
	connB, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("connB dial failed: %v", err)
	}
	defer connB.Close()

	_ = readMessage(t, connA)
	_ = readMessage(t, connB)

	sendStructuredSwitch(t, connA, "switch-session")

	if msg := readMessage(t, connA); msg.Type != MessageTypeChatReset {
		t.Fatalf("requester expected chat.reset first, got %s", msg.Type)
	}
	if msg := readMessage(t, connA); msg.Type != MessageTypeChatSnapshot {
		t.Fatalf("requester expected chat.snapshot second, got %s", msg.Type)
	}
	if msg := readMessage(t, connA); msg.Type != MessageTypeSessionBuffer {
		t.Fatalf("requester expected session.buffer third, got %s", msg.Type)
	}

	if msg, ok := readOptionalServerMessage(connB, 150*time.Millisecond); ok {
		t.Fatalf("non-requester should not receive structured replay, got %s", msg.Type)
	}
}

func TestSessionClearHistoryRemovesArchivedStructuredSnapshots(t *testing.T) {
	s := NewServer(":0")
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()
	s.SetSessionStore(store)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.SaveSession(&storage.Session{
		ID:        "running-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    storage.SessionStatusRunning,
	}); err != nil {
		t.Fatalf("SaveSession running failed: %v", err)
	}
	if err := store.SaveSession(&storage.Session{
		ID:        "complete-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now.Add(time.Second),
		LastSeen:  now.Add(time.Second),
		Status:    storage.SessionStatusComplete,
	}); err != nil {
		t.Fatalf("SaveSession complete failed: %v", err)
	}
	for _, sessionID := range []string{"running-1", "complete-1"} {
		if err := store.ReplaceStructuredChatSnapshot(&storage.StructuredChatSnapshot{
			SessionID: sessionID,
			Revision:  1,
			UpdatedAt: now,
			Items: []storage.StructuredChatStoredItem{
				testServerStructuredChatStoredItem(sessionID, "item-"+sessionID, 1),
			},
		}); err != nil {
			t.Fatalf("ReplaceStructuredChatSnapshot(%s) failed: %v", sessionID, err)
		}
	}

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)
	if err := conn.WriteJSON(map[string]interface{}{
		"type": "session.clear_history",
		"payload": map[string]interface{}{
			"request_id": "clear-1",
		},
	}); err != nil {
		t.Fatalf("send clear history failed: %v", err)
	}

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionClearHistoryResult {
		t.Fatalf("expected session.clear_history_result first, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionList {
		t.Fatalf("expected session.list second, got %s", msg.Type)
	}

	if snapshot, err := store.LoadStructuredChatSnapshot("complete-1"); err != nil {
		t.Fatalf("LoadStructuredChatSnapshot complete-1 failed: %v", err)
	} else if snapshot != nil {
		t.Fatal("complete-1 structured snapshot should be deleted")
	}
	if snapshot, err := store.LoadStructuredChatSnapshot("running-1"); err != nil {
		t.Fatalf("LoadStructuredChatSnapshot running-1 failed: %v", err)
	} else if snapshot == nil {
		t.Fatal("running-1 structured snapshot should remain")
	}
}

func TestStructuredChatClearHistoryDoesNotEmitHistoryClearedReset(t *testing.T) {
	s := NewServer(":0")
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()
	s.SetSessionStore(store)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.SaveSession(&storage.Session{
		ID:        "complete-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    storage.SessionStatusComplete,
	}); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn)
	if err := conn.WriteJSON(map[string]interface{}{
		"type": "session.clear_history",
		"payload": map[string]interface{}{
			"request_id": "clear-1",
		},
	}); err != nil {
		t.Fatalf("send clear history failed: %v", err)
	}

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionClearHistoryResult {
		t.Fatalf("expected session.clear_history_result first, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionList {
		t.Fatalf("expected session.list second, got %s", msg.Type)
	}
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no chat.reset after clear_history, got %s", msg.Type)
	}
}

func TestStructuredChatClearHistoryNoArchivedSessionsLeavesLiveStructuredStateUntouched(t *testing.T) {
	s := NewServer(":0")
	store, err := storage.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()
	s.SetSessionStore(store)

	now := time.Now().UTC().Truncate(time.Millisecond)
	// Only running session -- no completed/archived sessions
	if err := store.SaveSession(&storage.Session{
		ID:        "running-1",
		Repo:      "/tmp/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    storage.SessionStatusRunning,
	}); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	if err := store.ReplaceStructuredChatSnapshot(&storage.StructuredChatSnapshot{
		SessionID: "running-1",
		Revision:  1,
		UpdatedAt: now,
		Items: []storage.StructuredChatStoredItem{
			testServerStructuredChatStoredItem("running-1", "item-running", 1),
		},
	}); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Stop()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	_ = readMessage(t, conn) // session.status
	if err := conn.WriteJSON(map[string]interface{}{
		"type": "session.clear_history",
		"payload": map[string]interface{}{
			"request_id": "clear-1",
		},
	}); err != nil {
		t.Fatalf("send clear history failed: %v", err)
	}

	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionClearHistoryResult {
		t.Fatalf("expected session.clear_history_result first, got %s", msg.Type)
	}
	if msg := readMessage(t, conn); msg.Type != MessageTypeSessionList {
		t.Fatalf("expected session.list second, got %s", msg.Type)
	}

	// Assert running session's structured snapshot remains
	if snapshot, err := store.LoadStructuredChatSnapshot("running-1"); err != nil {
		t.Fatalf("LoadStructuredChatSnapshot running-1 failed: %v", err)
	} else if snapshot == nil {
		t.Fatal("running-1 structured snapshot should remain after clear_history with no archived sessions")
	}

	// Assert no chat.reset arrives
	if msg, ok := readOptionalServerMessage(conn, 150*time.Millisecond); ok {
		t.Fatalf("expected no chat.reset after clear_history with no archived sessions, got %s", msg.Type)
	}
}

func newStructuredChatSwitchTestConnection(t *testing.T) (*Server, *websocket.Conn, func()) {
	t.Helper()

	s := NewServer(":0")
	mgr := pty.NewSessionManager()
	s.SetSessionManager(mgr)

	session, err := mgr.CreateWithID("switch-session", pty.SessionConfig{HistoryLines: 32})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}
	session.PrependToBuffer("line1\nline2")

	errCh := s.StartAsync()
	if err := <-errCh; err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleWebSocket(w, r)
	}))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		ts.Close()
		s.Stop()
		t.Fatalf("dial failed: %v", err)
	}
	_ = readMessage(t, conn)

	cleanup := func() {
		conn.Close()
		ts.Close()
		s.Stop()
		mgr.CloseAll()
	}
	return s, conn, cleanup
}

func sendStructuredSwitch(t *testing.T, conn *websocket.Conn, sessionID string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]interface{}{
		"type": "session.switch",
		"payload": map[string]interface{}{
			"session_id": sessionID,
		},
	}); err != nil {
		t.Fatalf("send session.switch failed: %v", err)
	}
}

func readOptionalServerMessage(conn *websocket.Conn, timeout time.Duration) (Message, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var msg Message
	if err := conn.ReadJSON(&msg); err != nil {
		if websocket.IsCloseError(err) || errors.Is(err, websocket.ErrReadLimit) {
			return Message{}, false
		}
		if strings.Contains(err.Error(), "i/o timeout") {
			return Message{}, false
		}
		return Message{}, false
	}
	return msg, true
}

func testServerStructuredChatStoredItem(sessionID, itemID string, createdAt int64) storage.StructuredChatStoredItem {
	return storage.StructuredChatStoredItem{
		ItemID:      itemID,
		Position:    0,
		PayloadJSON: fmt.Sprintf(`{"id":"%s","session_id":"%s","kind":"user_message","created_at":%d,"provider":"codex","status":"completed","text":"hello"}`, itemID, sessionID, createdAt),
		CreatedAt:   time.UnixMilli(createdAt).UTC(),
	}
}

func TestStripANSIForChat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text unchanged",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "basic SGR color code",
			input: "\x1b[31mred text\x1b[0m",
			want:  "red text",
		},
		{
			name:  "cursor movement replaced with space",
			input: "word1\x1b[10Gword2",
			want:  "word1 word2",
		},
		{
			name:  "tilde terminator bracketed paste",
			input: "\x1b[200~pasted text\x1b[201~",
			want:  "pasted text",
		},
		{
			name:  "tilde terminator function key",
			input: "before\x1b[2~after",
			want:  "beforeafter",
		},
		{
			name:  "at terminator insert mode",
			input: "before\x1b[2@after",
			want:  "beforeafter",
		},
		{
			name:  "greater-than CSI prefix device attributes",
			input: "\x1b[>0c",
			want:  "",
		},
		{
			name:  "equals CSI prefix",
			input: "\x1b[=1h",
			want:  "",
		},
		{
			name:  "two-char ESC sequence keypad mode",
			input: "before\x1b=after",
			want:  "beforeafter",
		},
		{
			name:  "two-char ESC sequence save cursor",
			input: "text\x1b7more",
			want:  "textmore",
		},
		{
			name:  "two-char ESC sequence restore cursor",
			input: "text\x1b8more",
			want:  "textmore",
		},
		{
			name:  "two-char ESC sequence normal keypad",
			input: "text\x1b>more",
			want:  "textmore",
		},
		{
			name:  "OSC sequence with BEL terminator",
			input: "\x1b]0;window title\x07content",
			want:  "content",
		},
		{
			name:  "OSC sequence with ST terminator",
			input: "\x1b]0;window title\x1b\\content",
			want:  "content",
		},
		{
			name:  "charset designation",
			input: "\x1b(Btext",
			want:  "text",
		},
		{
			name:  "stray ESC removed",
			input: "before\x1bafter",
			want:  "beforeafter",
		},
		{
			name:  "mixed escape types",
			input: "\x1b[?2026l\x1b[31m● Hello\x1b[0m\x1b[?2026h",
			want:  "● Hello",
		},
		{
			name:  "private mode set/reset",
			input: "\x1b[?2026h\x1b[?2026l",
			want:  "",
		},
		{
			name:  "multiple spaces collapsed",
			input: "a  \x1b[5G  b",
			want:  "a b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSIForChat(tt.input)
			if got != tt.want {
				t.Errorf("stripANSIForChat(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeChatItemStripsANSIFromAllKinds(t *testing.T) {
	tests := []struct {
		name string
		item ChatItem
		want string
	}{
		{
			name: "assistant markdown",
			item: ChatItem{Kind: ChatItemKindAssistant, Markdown: "Hello \x1b[31mworld\x1b[0m"},
			want: "Hello world",
		},
		{
			name: "user text",
			item: ChatItem{Kind: ChatItemKindUserMessage, Text: "\x1b[?2026lhello\x1b[?2026h"},
			want: "hello",
		},
		{
			name: "thinking text",
			item: ChatItem{Kind: ChatItemKindThinking, Text: "think\x1b=more"},
			want: "thinkmore",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeChatItem(tt.item)
			var field string
			switch tt.item.Kind {
			case ChatItemKindAssistant:
				field = got.Markdown
			default:
				field = got.Text
			}
			if field != tt.want {
				t.Errorf("sanitizeChatItem(%s) text = %q, want %q", tt.name, field, tt.want)
			}
		})
	}
}

// TestStructuredChatControllerConcurrentApplySerializes verifies that concurrent
// ApplyAuthoritativeTimeline calls serialize via timelineMu: each call loads the
// latest persisted state, diffs against it, and increments the revision. Without
// the lock, two calls loading the same snapshot would both compute the same
// next revision, producing a duplicate. (spec 08)
func TestStructuredChatControllerConcurrentApplySerializes(t *testing.T) {
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	revisions := make(chan int64, goroutines)
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			// Each goroutine applies a unique single-item timeline. The internal
			// load→diff→persist cycle serializes via timelineMu, so each call
			// observes the previous call's persisted state and increments revision.
			msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
				ID:        fmt.Sprintf("item-%d", idx),
				SessionID: "session-1",
				Kind:      ChatItemKindUserMessage,
				CreatedAt: int64(idx + 1),
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				Text:      fmt.Sprintf("msg-%d", idx),
			}})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", idx, err)
				return
			}
			// Collect the final (highest) revision from this call to verify uniqueness.
			if len(msgs) > 0 {
				revisions <- extractRevision(msgs[len(msgs)-1])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(revisions)

	for err := range errs {
		t.Fatal(err)
	}

	// Every returned revision must be unique — proves serialization.
	seen := make(map[int64]bool)
	for rev := range revisions {
		if seen[rev] {
			t.Fatalf("duplicate revision %d: concurrent Apply calls were not serialized", rev)
		}
		seen[rev] = true
	}

	// Final revision: first call emits 1 message (upsert only, no prior items),
	// each subsequent call emits 2 messages (remove old + upsert new).
	// Total = 1 + (goroutines-1)*2 = 2*goroutines - 1.
	expectedFinalRevision := int64(2*goroutines - 1)
	snapshot, err := store.LoadStructuredChatSnapshot("session-1")
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if snapshot == nil {
		t.Fatal("expected snapshot after concurrent applies")
	}
	if snapshot.Revision != expectedFinalRevision {
		t.Fatalf("final revision = %d, want %d", snapshot.Revision, expectedFinalRevision)
	}
}

// TestStructuredChatControllerConcurrentApplyAndNextRevisionSerialize verifies
// that ApplyAuthoritativeTimeline and NextRevision share the same timelineMu,
// so interleaved calls never produce duplicate or non-monotonic revisions. (spec 08)
func TestStructuredChatControllerConcurrentApplyAndNextRevisionSerialize(t *testing.T) {
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	const applyCount = 10
	const nextRevCount = 10
	var wg sync.WaitGroup
	wg.Add(applyCount + nextRevCount)
	revisions := make(chan int64, applyCount+nextRevCount)
	errs := make(chan error, applyCount+nextRevCount)

	// Half the goroutines call ApplyAuthoritativeTimeline.
	for i := 0; i < applyCount; i++ {
		go func(idx int) {
			defer wg.Done()
			msgs, err := controller.ApplyAuthoritativeTimeline("session-1", []ChatItem{{
				ID:        fmt.Sprintf("apply-item-%d", idx),
				SessionID: "session-1",
				Kind:      ChatItemKindUserMessage,
				CreatedAt: int64(idx + 1),
				Provider:  ChatItemProviderClaude,
				Status:    ChatItemStatusCompleted,
				Text:      fmt.Sprintf("apply-%d", idx),
			}})
			if err != nil {
				errs <- fmt.Errorf("apply %d: %w", idx, err)
				return
			}
			if len(msgs) > 0 {
				revisions <- extractRevision(msgs[len(msgs)-1])
			}
		}(i)
	}

	// Other half call NextRevision.
	for i := 0; i < nextRevCount; i++ {
		go func(idx int) {
			defer wg.Done()
			rev, err := controller.NextRevision("session-1")
			if err != nil {
				errs <- fmt.Errorf("next-rev %d: %w", idx, err)
				return
			}
			revisions <- rev
		}(i)
	}

	wg.Wait()
	close(errs)
	close(revisions)

	for err := range errs {
		t.Fatal(err)
	}

	// Every revision must be unique — proves Apply and NextRevision serialize.
	seen := make(map[int64]bool)
	for rev := range revisions {
		if seen[rev] {
			t.Fatalf("duplicate revision %d: Apply and NextRevision were not serialized", rev)
		}
		seen[rev] = true
	}
}

// extractRevision returns the revision from any chat message payload type.
func extractRevision(msg Message) int64 {
	switch p := msg.Payload.(type) {
	case ChatUpsertPayload:
		return p.Revision
	case ChatRemovePayload:
		return p.Revision
	case ChatSnapshotPayload:
		return p.Revision
	case ChatResetPayload:
		return p.Revision
	default:
		return 0
	}
}

func cloneStructuredChatSnapshot(snapshot *storage.StructuredChatSnapshot) *storage.StructuredChatSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	cloned.Items = append([]storage.StructuredChatStoredItem(nil), snapshot.Items...)
	return &cloned
}

// --- MergeProviderTimeline tests (spec 04) ---

func TestMergeProviderTimelineEmptyProvider(t *testing.T) {
	// Merging an empty provider partition preserves host items unchanged.
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {
				SessionID: "s1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("s1", "approval:req-1", 100),
					testServerStructuredChatStoredItem("s1", "jsonl-text:old", 200),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.MergeProviderTimeline("s1", nil)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	// Provider item removed, host item preserved → should emit remove + upsert.
	snapshot := store.snapshots["s1"]
	if snapshot == nil {
		t.Fatal("snapshot is nil")
	}
	if len(snapshot.Items) != 1 {
		t.Fatalf("snapshot item count = %d, want 1", len(snapshot.Items))
	}
	if snapshot.Items[0].ItemID != "approval:req-1" {
		t.Fatalf("remaining item = %q, want approval:req-1", snapshot.Items[0].ItemID)
	}
	// Should have a remove message for the old provider item.
	hasRemove := false
	for _, msg := range msgs {
		if msg.Type == MessageTypeChatRemove {
			removePayload := msg.Payload.(ChatRemovePayload)
			for _, id := range removePayload.ItemIDs {
				if id == "jsonl-text:old" {
					hasRemove = true
				}
			}
		}
	}
	if !hasRemove {
		t.Fatal("expected remove message for jsonl-text:old")
	}
}

func TestMergeProviderTimelineEmptyHost(t *testing.T) {
	// No existing snapshot → provider items are the full timeline.
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{ID: "jsonl-user:e1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "hello"},
		{ID: "jsonl-text:e2:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 200, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "hi"},
	}
	msgs, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	if snapshot == nil {
		t.Fatal("snapshot is nil")
	}
	if len(snapshot.Items) != 2 {
		t.Fatalf("snapshot item count = %d, want 2", len(snapshot.Items))
	}
	if snapshot.Items[0].ItemID != "jsonl-user:e1" {
		t.Fatalf("first item = %q, want jsonl-user:e1", snapshot.Items[0].ItemID)
	}
	if len(msgs) == 0 {
		t.Fatal("expected upsert messages")
	}
	if msgs[len(msgs)-1].Type != MessageTypeChatUpsert {
		t.Fatalf("last message type = %s, want chat.upsert", msgs[len(msgs)-1].Type)
	}
}

func TestMergeProviderTimelineInterleaveOrdering(t *testing.T) {
	// Host items interleaved among provider items by created_at.
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {
				SessionID: "s1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("s1", "approval:a1", 150),
					testServerStructuredChatStoredItem("s1", "approval:a2", 250),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "q1"},
		{ID: "jsonl-text:t1:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 200, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "a1"},
		{ID: "jsonl-user:u2", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 300, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "q2"},
	}
	_, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	if len(snapshot.Items) != 5 {
		t.Fatalf("item count = %d, want 5", len(snapshot.Items))
	}
	// Expected order: u1(100), a1(150), t1(200), a2(250), u2(300)
	wantOrder := []string{"jsonl-user:u1", "approval:a1", "jsonl-text:t1:0", "approval:a2", "jsonl-user:u2"}
	for i, want := range wantOrder {
		if snapshot.Items[i].ItemID != want {
			t.Errorf("item[%d] = %q, want %q", i, snapshot.Items[i].ItemID, want)
		}
	}
}

func TestMergeProviderTimelineTieBreaking(t *testing.T) {
	// Equal created_at → provider items come first.
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {
				SessionID: "s1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("s1", "approval:a1", 100),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "q1"},
	}
	_, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	if len(snapshot.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(snapshot.Items))
	}
	// Provider first on tie.
	if snapshot.Items[0].ItemID != "jsonl-user:u1" {
		t.Fatalf("first item = %q, want jsonl-user:u1 (provider first on tie)", snapshot.Items[0].ItemID)
	}
	if snapshot.Items[1].ItemID != "approval:a1" {
		t.Fatalf("second item = %q, want approval:a1", snapshot.Items[1].ItemID)
	}
}

func TestMergeProviderTimelineHostPreservedAcrossMerges(t *testing.T) {
	// Approval items in all lifecycle states survive repeated merges.
	approvalPending := ChatItem{
		ID: "approval:pending-1", SessionID: "s1", Kind: ChatItemKindApprovalRequest,
		CreatedAt: 150, Provider: ChatItemProviderSystem, Status: ChatItemStatusPending,
	}
	approvalApproved := ChatItem{
		ID: "approval:approved-1", SessionID: "s1", Kind: ChatItemKindApprovalRequest,
		CreatedAt: 250, Provider: ChatItemProviderSystem, Status: ChatItemStatusCompleted,
		Decision: "approved",
	}
	approvalDenied := ChatItem{
		ID: "approval:denied-1", SessionID: "s1", Kind: ChatItemKindApprovalRequest,
		CreatedAt: 350, Provider: ChatItemProviderSystem, Status: ChatItemStatusCompleted,
		Decision: "denied",
	}
	approvalTimedOut := ChatItem{
		ID: "approval:timeout-1", SessionID: "s1", Kind: ChatItemKindApprovalRequest,
		CreatedAt: 450, Provider: ChatItemProviderSystem, Status: ChatItemStatusFailed,
		Decision: "timed_out",
	}

	// Seed initial snapshot with all approval states + a provider item.
	initialItems := []ChatItem{
		{ID: "jsonl-user:old", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "old"},
		approvalPending,
		{ID: "jsonl-text:old:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 200, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "old reply"},
		approvalApproved,
		{ID: "jsonl-user:old2", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 300, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "old2"},
		approvalDenied,
		{ID: "jsonl-text:old2:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 400, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "old reply 2"},
		approvalTimedOut,
	}
	storedItems := make([]storage.StructuredChatStoredItem, len(initialItems))
	for i, item := range initialItems {
		storedItems[i] = testServerStructuredChatStoredItemFromChatItem(item, i)
	}
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {SessionID: "s1", Revision: 1, Items: storedItems},
		},
	}
	controller := NewStructuredChatController(store)

	// Merge with completely new provider items.
	newProviderItems := []ChatItem{
		{ID: "jsonl-user:new1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "new q1"},
		{ID: "jsonl-text:new1:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 200, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "new a1"},
		{ID: "jsonl-user:new2", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 500, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "new q2"},
	}
	_, err := controller.MergeProviderTimeline("s1", newProviderItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	if snapshot == nil {
		t.Fatal("snapshot is nil")
	}

	// All four approval items must survive.
	approvalIDs := map[string]bool{
		"approval:pending-1":  false,
		"approval:approved-1": false,
		"approval:denied-1":   false,
		"approval:timeout-1":  false,
	}
	for _, item := range snapshot.Items {
		if _, ok := approvalIDs[item.ItemID]; ok {
			approvalIDs[item.ItemID] = true
		}
	}
	for id, found := range approvalIDs {
		if !found {
			t.Errorf("approval item %q lost after merge", id)
		}
	}

	// Total items = 3 provider + 4 host = 7
	if len(snapshot.Items) != 7 {
		t.Fatalf("item count = %d, want 7", len(snapshot.Items))
	}
}

func TestMergeProviderTimelineProviderResetAndRebuild(t *testing.T) {
	// After provider_reset (clear provider partition), re-merge produces
	// a snapshot with both rebuilt provider items and preserved host items.
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {
				SessionID: "s1",
				Revision:  5,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItem("s1", "jsonl-user:old", 100),
					testServerStructuredChatStoredItem("s1", "approval:a1", 150),
					testServerStructuredChatStoredItem("s1", "jsonl-text:old:0", 200),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	// Step 1: provider_reset — clear provider partition (empty provider items).
	_, err := controller.MergeProviderTimeline("s1", nil)
	if err != nil {
		t.Fatalf("reset merge failed: %v", err)
	}
	snapshot := store.snapshots["s1"]
	if len(snapshot.Items) != 1 {
		t.Fatalf("after reset: item count = %d, want 1 (host only)", len(snapshot.Items))
	}
	if snapshot.Items[0].ItemID != "approval:a1" {
		t.Fatalf("after reset: remaining item = %q, want approval:a1", snapshot.Items[0].ItemID)
	}
	resetRevision := snapshot.Revision

	// Step 2: rebuild — merge new provider items from re-read.
	rebuilt := []ChatItem{
		{ID: "jsonl-user:new1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "rebuilt q"},
		{ID: "jsonl-text:new1:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 200, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "rebuilt a"},
	}
	msgs, err := controller.MergeProviderTimeline("s1", rebuilt)
	if err != nil {
		t.Fatalf("rebuild merge failed: %v", err)
	}

	snapshot = store.snapshots["s1"]
	if len(snapshot.Items) != 3 {
		t.Fatalf("after rebuild: item count = %d, want 3", len(snapshot.Items))
	}
	// Revision must be strictly greater than the reset revision.
	if snapshot.Revision <= resetRevision {
		t.Fatalf("rebuild revision %d not greater than reset revision %d", snapshot.Revision, resetRevision)
	}
	if len(msgs) == 0 {
		t.Fatal("expected broadcast messages after rebuild")
	}
	// Host item still present.
	found := false
	for _, item := range snapshot.Items {
		if item.ItemID == "approval:a1" {
			found = true
		}
	}
	if !found {
		t.Fatal("approval:a1 lost after rebuild")
	}
}

func TestMergeProviderTimelineRetainsFileOrder(t *testing.T) {
	// Provider items must retain JSONL file order even if timestamps
	// are non-monotonic.
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	// Non-monotonic timestamps: second item has earlier timestamp.
	providerItems := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 300, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "first"},
		{ID: "jsonl-text:t1:0", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Markdown: "second"},
	}
	_, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	if snapshot.Items[0].ItemID != "jsonl-user:u1" {
		t.Fatalf("first item = %q, want jsonl-user:u1 (file order)", snapshot.Items[0].ItemID)
	}
	if snapshot.Items[1].ItemID != "jsonl-text:t1:0" {
		t.Fatalf("second item = %q, want jsonl-text:t1:0 (file order)", snapshot.Items[1].ItemID)
	}
}

func TestMergeProviderTimelineNoChangeReturnsNil(t *testing.T) {
	// Merging identical provider items produces no messages.
	item := ChatItem{
		ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage,
		CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "hello",
	}
	storedItems := []storage.StructuredChatStoredItem{
		testServerStructuredChatStoredItemFromChatItem(item, 0),
	}
	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {SessionID: "s1", Revision: 3, Items: storedItems},
		},
	}
	controller := NewStructuredChatController(store)

	msgs, err := controller.MergeProviderTimeline("s1", []ChatItem{item})
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}
	if msgs != nil {
		t.Fatalf("expected nil messages for no-change merge, got %d", len(msgs))
	}
	if store.snapshots["s1"].Revision != 3 {
		t.Fatalf("revision changed to %d, want 3", store.snapshots["s1"].Revision)
	}
}

func TestMergeProviderTimelineConcurrentMergeAndApplySerialize(t *testing.T) {
	// Concurrent MergeProviderTimeline and ApplyAuthoritativeTimeline calls
	// must serialize via timelineMu — each call gets a unique final revision. (spec 08)
	// A single call may emit two messages (remove + upsert) with incrementing
	// revisions, so we collect the per-call max revision.
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	const n = 20
	var wg sync.WaitGroup
	callRevisions := make(chan int64, n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			item := ChatItem{
				ID: fmt.Sprintf("jsonl-user:u%d", idx), SessionID: "s1",
				Kind: ChatItemKindUserMessage, CreatedAt: int64(idx * 100),
				Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted,
				Text: fmt.Sprintf("msg-%d", idx),
			}
			var msgs []Message
			var err error
			if idx%2 == 0 {
				msgs, err = controller.MergeProviderTimeline("s1", []ChatItem{item})
			} else {
				msgs, err = controller.ApplyAuthoritativeTimeline("s1", []ChatItem{item})
			}
			if err != nil {
				errs <- err
				return
			}
			// Collect the revision for this call (all messages share it).
			var rev int64
			for _, msg := range msgs {
				if r := extractRevision(msg); r > rev {
					rev = r
				}
			}
			if rev > 0 {
				callRevisions <- rev
			}
		}()
	}
	wg.Wait()
	close(callRevisions)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	seen := make(map[int64]bool)
	for rev := range callRevisions {
		if seen[rev] {
			t.Fatalf("duplicate revision %d: Merge and Apply were not serialized", rev)
		}
		seen[rev] = true
	}
}

func TestMergeProviderTimelineSanitizes(t *testing.T) {
	// Tool call excerpts are sanitized during merge.
	store := &testStructuredChatStore{}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{
			ID: "jsonl-tool:t1", SessionID: "s1", Kind: ChatItemKindToolCall,
			CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted,
			ToolName: "bash", InputExcerpt: "Authorization: Bearer sk-secret-123",
			ResultExcerpt: "SECRET_KEY=hunter2",
		},
	}
	_, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}

	snapshot := store.snapshots["s1"]
	var item ChatItem
	if err := json.Unmarshal([]byte(snapshot.Items[0].PayloadJSON), &item); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if strings.Contains(item.InputExcerpt, "sk-secret-123") {
		t.Fatal("InputExcerpt not sanitized: bearer token still present")
	}
	if strings.Contains(item.ResultExcerpt, "hunter2") {
		t.Fatal("ResultExcerpt not sanitized: secret value still present")
	}
}

func TestMergeProviderTimelinePersistenceError(t *testing.T) {
	// Store error suppresses broadcast messages.
	store := &testStructuredChatStore{
		replaceErr: errors.New("disk full"),
	}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{ID: "jsonl-user:u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 100, Provider: ChatItemProviderClaude, Status: ChatItemStatusCompleted, Text: "hello"},
	}
	msgs, err := controller.MergeProviderTimeline("s1", providerItems)
	if err == nil {
		t.Fatal("expected error from persistence failure")
	}
	if msgs != nil {
		t.Fatalf("expected nil messages on error, got %d", len(msgs))
	}
}

// TestStructuredChatControllerLoadSnapshotFiltersLeadingAssistant verifies that
// LoadSnapshotMessage filters out leading assistant messages (system prompt dumps).
func TestStructuredChatControllerLoadSnapshotFiltersLeadingAssistant(t *testing.T) {
	assistantItem := ChatItem{
		ID:        "a1",
		SessionID: "s1",
		Kind:      ChatItemKindAssistant,
		CreatedAt: 1,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Markdown:  "system prompt dump here",
	}
	userItem := ChatItem{
		ID:        "u1",
		SessionID: "s1",
		Kind:      ChatItemKindUserMessage,
		CreatedAt: 2,
		Provider:  ChatItemProviderCodex,
		Status:    ChatItemStatusCompleted,
		Text:      "hello",
	}

	store := &testStructuredChatStore{
		snapshots: map[string]*storage.StructuredChatSnapshot{
			"s1": {
				SessionID: "s1",
				Revision:  1,
				Items: []storage.StructuredChatStoredItem{
					testServerStructuredChatStoredItemFromChatItem(assistantItem, 0),
					testServerStructuredChatStoredItemFromChatItem(userItem, 1),
				},
			},
		},
	}
	controller := NewStructuredChatController(store)

	msg, err := controller.LoadSnapshotMessage("s1")
	if err != nil {
		t.Fatalf("LoadSnapshotMessage failed: %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message")
	}

	payload := msg.Payload.(ChatSnapshotPayload)
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item after filter, got %d", len(payload.Items))
	}
	if payload.Items[0].ID != "u1" {
		t.Errorf("expected item ID 'u1', got %q", payload.Items[0].ID)
	}
}

// TestStructuredChatControllerMergeFiltersLeadingAssistant verifies that
// MergeProviderTimeline filters out leading assistant messages.
func TestStructuredChatControllerMergeFiltersLeadingAssistant(t *testing.T) {
	store := &testStructuredChatStore{
		snapshots: make(map[string]*storage.StructuredChatSnapshot),
	}
	controller := NewStructuredChatController(store)

	providerItems := []ChatItem{
		{ID: "jsonl-a1", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 1, Provider: ChatItemProviderCodex, Status: ChatItemStatusCompleted, Markdown: "system dump"},
		{ID: "jsonl-u1", SessionID: "s1", Kind: ChatItemKindUserMessage, CreatedAt: 2, Provider: ChatItemProviderCodex, Status: ChatItemStatusCompleted, Text: "hi"},
		{ID: "jsonl-a2", SessionID: "s1", Kind: ChatItemKindAssistant, CreatedAt: 3, Provider: ChatItemProviderCodex, Status: ChatItemStatusCompleted, Markdown: "reply"},
	}

	msgs, err := controller.MergeProviderTimeline("s1", providerItems)
	if err != nil {
		t.Fatalf("MergeProviderTimeline failed: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}

	// Find the upsert message and verify the leading assistant was filtered
	var upsertPayload ChatUpsertPayload
	for _, m := range msgs {
		if m.Type == MessageTypeChatUpsert {
			upsertPayload = m.Payload.(ChatUpsertPayload)
			break
		}
	}

	for _, item := range upsertPayload.Items {
		if item.ID == "jsonl-a1" {
			t.Error("leading assistant item 'jsonl-a1' should have been filtered")
		}
	}

	// Verify the post-user assistant is still present in stored snapshot
	snapshot := store.snapshots["s1"]
	if snapshot == nil {
		t.Fatal("expected stored snapshot")
	}
	foundPostUserAssistant := false
	for _, stored := range snapshot.Items {
		if stored.ItemID == "jsonl-a2" {
			foundPostUserAssistant = true
		}
		if stored.ItemID == "jsonl-a1" {
			t.Error("stored snapshot should not contain filtered item 'jsonl-a1'")
		}
	}
	if !foundPostUserAssistant {
		t.Error("stored snapshot should contain post-user assistant 'jsonl-a2'")
	}
}

// testServerStructuredChatStoredItemFromChatItem creates a stored item from a ChatItem.
func testServerStructuredChatStoredItemFromChatItem(item ChatItem, position int) storage.StructuredChatStoredItem {
	payloadJSON, _ := json.Marshal(item)
	return storage.StructuredChatStoredItem{
		ItemID:      item.ID,
		Position:    position,
		PayloadJSON: string(payloadJSON),
		CreatedAt:   time.UnixMilli(item.CreatedAt).UTC(),
	}
}
