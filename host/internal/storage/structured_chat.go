package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const structuredChatSnapshotItemLimit = 200

// LoadStructuredChatSnapshot loads one stored structured-chat snapshot.
// Returns nil, nil when the session has no stored snapshot.
func (s *SQLiteStore) LoadStructuredChatSnapshot(sessionID string) (*StructuredChatSnapshot, error) {
	if sessionID == "" {
		return nil, errors.New("session_id cannot be empty")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	const sessionQuery = `
		SELECT revision, updated_at
		FROM structured_chat_sessions
		WHERE session_id = ?
	`

	var (
		revision  int64
		updatedAt string
	)
	err := s.db.QueryRow(sessionQuery, sessionID).Scan(&revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load structured chat session: %w", err)
	}

	parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse structured chat updated_at: %w", err)
	}

	const itemsQuery = `
		SELECT item_id, position, payload_json, created_at
		FROM structured_chat_items
		WHERE session_id = ?
		ORDER BY position ASC
	`
	rows, err := s.db.Query(itemsQuery, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load structured chat items: %w", err)
	}
	defer rows.Close()

	items := make([]StructuredChatStoredItem, 0)
	for rows.Next() {
		var (
			item      StructuredChatStoredItem
			createdAt string
		)
		if err := rows.Scan(&item.ItemID, &item.Position, &item.PayloadJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan structured chat item: %w", err)
		}
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse structured chat item created_at: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate structured chat items: %w", err)
	}

	return &StructuredChatSnapshot{
		SessionID: sessionID,
		Revision:  revision,
		UpdatedAt: parsedUpdatedAt,
		Items:     items,
	}, nil
}

// ReplaceStructuredChatSnapshot atomically overwrites one stored structured-chat snapshot.
func (s *SQLiteStore) ReplaceStructuredChatSnapshot(snapshot *StructuredChatSnapshot) error {
	if err := validateStructuredChatSnapshot(snapshot); err != nil {
		return err
	}

	retained := trimStructuredChatSnapshotItems(snapshot.Items, structuredChatSnapshotItemLimit)
	updatedAt := snapshot.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin structured chat replace transaction: %w", err)
	}

	const upsertSession = `
		INSERT INTO structured_chat_sessions (session_id, revision, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			revision = excluded.revision,
			updated_at = excluded.updated_at
	`
	if _, err := tx.Exec(
		upsertSession,
		snapshot.SessionID,
		snapshot.Revision,
		updatedAt.Format(time.RFC3339Nano),
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("upsert structured chat session: %w", err)
	}

	const deleteItems = `DELETE FROM structured_chat_items WHERE session_id = ?`
	if _, err := tx.Exec(deleteItems, snapshot.SessionID); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete structured chat items: %w", err)
	}

	const insertItem = `
		INSERT INTO structured_chat_items (session_id, item_id, position, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`
	for position, item := range retained {
		if _, err := tx.Exec(
			insertItem,
			snapshot.SessionID,
			item.ItemID,
			position,
			item.PayloadJSON,
			item.CreatedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert structured chat item %q: %w", item.ItemID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit structured chat replace transaction: %w", err)
	}

	return nil
}

func trimStructuredChatSnapshotItems(items []StructuredChatStoredItem, limit int) []StructuredChatStoredItem {
	if limit <= 0 || len(items) <= limit {
		trimmed := make([]StructuredChatStoredItem, len(items))
		copy(trimmed, items)
		for i := range trimmed {
			trimmed[i].Position = i
		}
		return trimmed
	}

	start := len(items) - limit
	trimmed := make([]StructuredChatStoredItem, limit)
	copy(trimmed, items[start:])
	for i := range trimmed {
		trimmed[i].Position = i
	}
	return trimmed
}

func validateStructuredChatSnapshot(snapshot *StructuredChatSnapshot) error {
	if snapshot == nil {
		return errors.New("structured chat snapshot cannot be nil")
	}
	if snapshot.SessionID == "" {
		return errors.New("structured chat snapshot session_id cannot be empty")
	}

	seenIDs := make(map[string]struct{}, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.ItemID == "" {
			return errors.New("structured chat snapshot item_id cannot be empty")
		}
		if _, exists := seenIDs[item.ItemID]; exists {
			return fmt.Errorf("duplicate structured chat item_id %q", item.ItemID)
		}
		seenIDs[item.ItemID] = struct{}{}
		if err := validateStructuredChatPayload(snapshot.SessionID, item); err != nil {
			return err
		}
	}

	return nil
}

func validateStructuredChatPayload(sessionID string, item StructuredChatStoredItem) error {
	if !json.Valid([]byte(item.PayloadJSON)) {
		return fmt.Errorf("structured chat item %q has invalid payload_json", item.ItemID)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item.PayloadJSON), &raw); err != nil {
		return fmt.Errorf("decode structured chat item %q: %w", item.ItemID, err)
	}

	var payloadSessionID string
	if err := decodeStructuredChatStringField(raw, "session_id", &payloadSessionID); err != nil {
		return fmt.Errorf("structured chat item %q session_id: %w", item.ItemID, err)
	}
	if payloadSessionID != sessionID {
		return fmt.Errorf("structured chat item %q session_id mismatch: got %q want %q", item.ItemID, payloadSessionID, sessionID)
	}

	var payloadItemID string
	if err := decodeStructuredChatStringField(raw, "id", &payloadItemID); err != nil {
		return fmt.Errorf("structured chat item %q id: %w", item.ItemID, err)
	}
	if payloadItemID != item.ItemID {
		return fmt.Errorf("structured chat item %q payload id mismatch: got %q", item.ItemID, payloadItemID)
	}

	var createdAt int64
	if err := decodeStructuredChatInt64Field(raw, "created_at", &createdAt); err != nil {
		return fmt.Errorf("structured chat item %q created_at: %w", item.ItemID, err)
	}
	if createdAt <= 0 {
		return fmt.Errorf("structured chat item %q created_at must be > 0", item.ItemID)
	}

	if err := validateStructuredChatCanonicalShape(item.ItemID, raw); err != nil {
		return err
	}

	return nil
}

func validateStructuredChatCanonicalShape(itemID string, raw map[string]json.RawMessage) error {
	kind, err := decodeStructuredChatEnumField(raw, "kind", structuredChatCanonicalKinds)
	if err != nil {
		return fmt.Errorf("structured chat item %q kind: %w", itemID, err)
	}
	if _, err := decodeStructuredChatEnumField(raw, "provider", structuredChatCanonicalProviders); err != nil {
		return fmt.Errorf("structured chat item %q provider: %w", itemID, err)
	}
	if _, err := decodeStructuredChatEnumField(raw, "status", structuredChatCanonicalStatuses); err != nil {
		return fmt.Errorf("structured chat item %q status: %w", itemID, err)
	}

	allowedFields := structuredChatAllowedFieldsForKind(kind)
	for field := range raw {
		if !slices.Contains(allowedFields, field) {
			return fmt.Errorf("structured chat item %q field %q is not valid for kind %q", itemID, field, kind)
		}
	}

	requiredFields := structuredChatRequiredFieldsForKind(kind)
	for _, field := range requiredFields {
		if _, ok := raw[field]; !ok {
			return fmt.Errorf("structured chat item %q missing required field %q for kind %q", itemID, field, kind)
		}
	}

	if kind == "approval_request" {
		var expiresAt string
		if err := decodeStructuredChatStringField(raw, "expires_at", &expiresAt); err != nil {
			return fmt.Errorf("structured chat item %q expires_at: %w", itemID, err)
		}
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			return fmt.Errorf("structured chat item %q expires_at: invalid RFC3339 timestamp", itemID)
		}
	}

	return nil
}

var structuredChatCanonicalKinds = []string{
	"user_message",
	"assistant_message",
	"thinking",
	"tool_call",
	"approval_request",
	"system_event",
	"transcript_block",
}

var structuredChatCanonicalProviders = []string{
	"claude",
	"codex",
	"gemini",
	"system",
	"unknown",
}

var structuredChatCanonicalStatuses = []string{
	"in_progress",
	"completed",
	"failed",
	"pending",
}

var structuredChatBaseFields = []string{
	"id",
	"session_id",
	"kind",
	"created_at",
	"provider",
	"status",
	"request_id",
	"parent_id",
}

func structuredChatAllowedFieldsForKind(kind string) []string {
	fields := append([]string{}, structuredChatBaseFields...)
	switch kind {
	case "user_message", "thinking", "transcript_block":
		return append(fields, "text")
	case "assistant_message":
		return append(fields, "markdown")
	case "tool_call":
		return append(fields, "tool_name", "summary", "input_excerpt", "result_excerpt", "is_error", "started_at", "completed_at")
	case "approval_request":
		return append(fields, "approval_request_id", "command", "reason", "expires_at", "decision")
	case "system_event":
		return append(fields, "code", "message")
	default:
		return fields
	}
}

func structuredChatRequiredFieldsForKind(kind string) []string {
	switch kind {
	case "user_message", "thinking", "transcript_block":
		return []string{"text"}
	case "assistant_message":
		return []string{"markdown"}
	case "tool_call":
		return []string{"tool_name", "summary", "input_excerpt", "result_excerpt", "is_error"}
	case "approval_request":
		return []string{"approval_request_id", "command", "reason", "expires_at"}
	case "system_event":
		return []string{"code", "message"}
	default:
		return nil
	}
}

func decodeStructuredChatStringField(raw map[string]json.RawMessage, field string, dest *string) error {
	value, ok := raw[field]
	if !ok {
		return fmt.Errorf("missing field %q", field)
	}
	if err := json.Unmarshal(value, dest); err != nil {
		return fmt.Errorf("invalid string field %q", field)
	}
	if *dest == "" {
		return fmt.Errorf("empty field %q", field)
	}
	return nil
}

func decodeStructuredChatInt64Field(raw map[string]json.RawMessage, field string, dest *int64) error {
	value, ok := raw[field]
	if !ok {
		return fmt.Errorf("missing field %q", field)
	}
	if err := json.Unmarshal(value, dest); err != nil {
		return fmt.Errorf("invalid integer field %q", field)
	}
	return nil
}

func decodeStructuredChatEnumField(raw map[string]json.RawMessage, field string, allowed []string) (string, error) {
	var value string
	if err := decodeStructuredChatStringField(raw, field, &value); err != nil {
		return "", err
	}
	if !slices.Contains(allowed, value) {
		return "", fmt.Errorf("unsupported value %q", value)
	}
	return value, nil
}
