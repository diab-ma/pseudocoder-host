package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStructuredChatSnapshotRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := &StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  4,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-1", "item-1", 11, now),
			testStructuredChatStoredItem("session-1", "item-2", 12, now.Add(time.Second)),
		},
	}

	if err := store.ReplaceStructuredChatSnapshot(snapshot); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	got, err := store.LoadStructuredChatSnapshot("session-1")
	if err != nil {
		t.Fatalf("LoadStructuredChatSnapshot failed: %v", err)
	}
	if got == nil {
		t.Fatal("LoadStructuredChatSnapshot returned nil")
	}
	if got.SessionID != snapshot.SessionID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, snapshot.SessionID)
	}
	if got.Revision != snapshot.Revision {
		t.Fatalf("Revision = %d, want %d", got.Revision, snapshot.Revision)
	}
	if len(got.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(got.Items))
	}
	if got.Items[0].ItemID != "item-1" || got.Items[1].ItemID != "item-2" {
		t.Fatalf("unexpected item order: %+v", got.Items)
	}
}

func TestStructuredChatSnapshotReplaceOverwritesPriorState(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	first := &StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-1", "old-item", 1, now),
		},
	}
	second := &StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  2,
		UpdatedAt: now.Add(time.Second),
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-1", "new-item", 2, now.Add(time.Second)),
		},
	}

	if err := store.ReplaceStructuredChatSnapshot(first); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot(first) failed: %v", err)
	}
	if err := store.ReplaceStructuredChatSnapshot(second); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot(second) failed: %v", err)
	}

	got, err := store.LoadStructuredChatSnapshot("session-1")
	if err != nil {
		t.Fatalf("LoadStructuredChatSnapshot failed: %v", err)
	}
	if got.Revision != 2 {
		t.Fatalf("Revision = %d, want 2", got.Revision)
	}
	if len(got.Items) != 1 || got.Items[0].ItemID != "new-item" {
		t.Fatalf("Items = %+v, want only new-item", got.Items)
	}
}

func TestStructuredChatSnapshotTrimToBound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	items := make([]StructuredChatStoredItem, 0, structuredChatSnapshotItemLimit+5)
	for i := 0; i < structuredChatSnapshotItemLimit+5; i++ {
		items = append(items, testStructuredChatStoredItem("session-1", "item-"+itoa(i), int64(i+1), now.Add(time.Duration(i)*time.Millisecond)))
	}

	if err := store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  9,
		UpdatedAt: now,
		Items:     items,
	}); err != nil {
		t.Fatalf("ReplaceStructuredChatSnapshot failed: %v", err)
	}

	got, err := store.LoadStructuredChatSnapshot("session-1")
	if err != nil {
		t.Fatalf("LoadStructuredChatSnapshot failed: %v", err)
	}
	if len(got.Items) != structuredChatSnapshotItemLimit {
		t.Fatalf("len(Items) = %d, want %d", len(got.Items), structuredChatSnapshotItemLimit)
	}
	if got.Items[0].ItemID != "item-5" {
		t.Fatalf("first retained item = %q, want item-5", got.Items[0].ItemID)
	}
	if got.Items[len(got.Items)-1].ItemID != "item-204" {
		t.Fatalf("last retained item = %q, want item-204", got.Items[len(got.Items)-1].ItemID)
	}
}

func TestStructuredChatSnapshotRejectsDuplicateItemIDs(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-1", "dup", 1, now),
			testStructuredChatStoredItem("session-1", "dup", 2, now.Add(time.Second)),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate structured chat item_id") {
		t.Fatalf("expected duplicate item_id error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsMismatchedSessionID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-2", "item-1", 1, now),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "session_id mismatch") {
		t.Fatalf("expected session_id mismatch error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsNilSnapshot(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.ReplaceStructuredChatSnapshot(nil)
	if err == nil || !strings.Contains(err.Error(), "cannot be nil") {
		t.Fatalf("expected nil snapshot error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsEmptySessionID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "",
		Revision:  1,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{
			testStructuredChatStoredItem("session-1", "item-1", 1, now),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "session_id cannot be empty") {
		t.Fatalf("expected empty session_id error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsInvalidPayloadJSON(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items: []StructuredChatStoredItem{{
			ItemID:      "item-1",
			PayloadJSON: "{",
			CreatedAt:   now,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid payload_json") {
		t.Fatalf("expected invalid payload_json error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsPayloadIDMismatch(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := testStructuredChatStoredItem("session-1", "item-1", 1, now)
	item.PayloadJSON = strings.Replace(item.PayloadJSON, `"id":"item-1"`, `"id":"other-item"`, 1)

	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items:     []StructuredChatStoredItem{item},
	})
	if err == nil || !strings.Contains(err.Error(), "payload id mismatch") {
		t.Fatalf("expected payload id mismatch error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsInvalidCanonicalKind(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := testStructuredChatStoredItem("session-1", "item-1", 1, now)
	item.PayloadJSON = strings.Replace(item.PayloadJSON, `"kind":"user_message"`, `"kind":"bogus_kind"`, 1)

	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items:     []StructuredChatStoredItem{item},
	})
	if err == nil || !strings.Contains(err.Error(), `kind: unsupported value`) {
		t.Fatalf("expected invalid kind error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsInvalidCanonicalProvider(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := testStructuredChatStoredItem("session-1", "item-1", 1, now)
	item.PayloadJSON = strings.Replace(item.PayloadJSON, `"provider":"codex"`, `"provider":"not_a_provider"`, 1)

	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items:     []StructuredChatStoredItem{item},
	})
	if err == nil || !strings.Contains(err.Error(), `provider: unsupported value`) {
		t.Fatalf("expected invalid provider error, got %v", err)
	}
}

func TestStructuredChatSnapshotRejectsInvalidCanonicalShape(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := testStructuredChatStoredItem("session-1", "item-1", 1, now)

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	payload["kind"] = "assistant_message"
	payload["text"] = "should-not-exist"
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	item.PayloadJSON = string(data)

	err = store.ReplaceStructuredChatSnapshot(&StructuredChatSnapshot{
		SessionID: "session-1",
		Revision:  1,
		UpdatedAt: now,
		Items:     []StructuredChatStoredItem{item},
	})
	if err == nil || !strings.Contains(err.Error(), `field "text" is not valid for kind "assistant_message"`) {
		t.Fatalf("expected invalid canonical shape error, got %v", err)
	}
}

func testStructuredChatStoredItem(sessionID, itemID string, createdAtMillis int64, createdAt time.Time) StructuredChatStoredItem {
	payload := map[string]interface{}{
		"id":         itemID,
		"session_id": sessionID,
		"kind":       "user_message",
		"created_at": createdAtMillis,
		"provider":   "codex",
		"status":     "completed",
		"text":       "hello",
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}

	return StructuredChatStoredItem{
		ItemID:      itemID,
		PayloadJSON: string(payloadJSON),
		CreatedAt:   createdAt,
	}
}
