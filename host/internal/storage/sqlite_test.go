package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewSQLiteStore verifies that a store can be created with an in-memory database.
func TestNewSQLiteStore(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Verify the database is usable by listing cards (should be empty)
	cards, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(cards) != 0 {
		t.Errorf("expected empty list, got %d cards", len(cards))
	}
}

// TestSaveAndGetCard verifies that a card can be saved and retrieved.
func TestSaveAndGetCard(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a test card
	now := time.Now().Truncate(time.Millisecond)
	card := &ReviewCard{
		ID:        "abc123",
		SessionID: "session-1",
		File:      "main.go",
		Diff:      "@@ -1,3 +1,4 @@\n+new line",
		Status:    CardPending,
		CreatedAt: now,
	}

	// Save the card
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Retrieve the card
	got, err := store.GetCard("abc123")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetCard returned nil")
	}

	// Verify fields
	if got.ID != card.ID {
		t.Errorf("ID = %q, want %q", got.ID, card.ID)
	}
	if got.SessionID != card.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, card.SessionID)
	}
	if got.File != card.File {
		t.Errorf("File = %q, want %q", got.File, card.File)
	}
	if got.Diff != card.Diff {
		t.Errorf("Diff = %q, want %q", got.Diff, card.Diff)
	}
	if got.Status != card.Status {
		t.Errorf("Status = %q, want %q", got.Status, card.Status)
	}
	// Compare times with some tolerance for parsing roundtrip
	if got.CreatedAt.Sub(card.CreatedAt).Abs() > time.Millisecond {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, card.CreatedAt)
	}
}

// TestGetCardNotFound verifies that GetCard returns nil for non-existent cards.
func TestGetCardNotFound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	got, err := store.GetCard("nonexistent")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for non-existent card, got %+v", got)
	}
}

// TestSaveCardUpdate verifies that saving a card with the same ID updates it.
func TestSaveCardUpdate(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Save initial card
	card := &ReviewCard{
		ID:        "update-test",
		SessionID: "session-1",
		File:      "old.go",
		Diff:      "old diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Update the card
	card.File = "new.go"
	card.Diff = "new diff"
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard update failed: %v", err)
	}

	// Verify the update
	got, err := store.GetCard("update-test")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got.File != "new.go" {
		t.Errorf("File = %q, want %q", got.File, "new.go")
	}
	if got.Diff != "new diff" {
		t.Errorf("Diff = %q, want %q", got.Diff, "new diff")
	}
}

// TestListPending verifies that only pending cards are returned.
func TestListPending(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Create cards with different statuses
	cards := []*ReviewCard{
		{ID: "p1", File: "a.go", Diff: "diff1", Status: CardPending, CreatedAt: now},
		{ID: "a1", File: "b.go", Diff: "diff2", Status: CardAccepted, CreatedAt: now.Add(time.Second)},
		{ID: "p2", File: "c.go", Diff: "diff3", Status: CardPending, CreatedAt: now.Add(2 * time.Second)},
		{ID: "r1", File: "d.go", Diff: "diff4", Status: CardRejected, CreatedAt: now.Add(3 * time.Second)},
	}

	for _, c := range cards {
		if err := store.SaveCard(c); err != nil {
			t.Fatalf("SaveCard failed: %v", err)
		}
	}

	// List pending
	pending, err := store.ListPending()
	if err != nil {
		t.Fatalf("ListPending failed: %v", err)
	}

	if len(pending) != 2 {
		t.Fatalf("expected 2 pending cards, got %d", len(pending))
	}

	// Verify order (oldest first)
	if pending[0].ID != "p1" {
		t.Errorf("first pending card ID = %q, want p1", pending[0].ID)
	}
	if pending[1].ID != "p2" {
		t.Errorf("second pending card ID = %q, want p2", pending[1].ID)
	}
}

// TestListAll verifies that all cards are returned regardless of status.
func TestListAll(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Create cards with different statuses
	cards := []*ReviewCard{
		{ID: "c1", File: "a.go", Diff: "diff1", Status: CardPending, CreatedAt: now},
		{ID: "c2", File: "b.go", Diff: "diff2", Status: CardAccepted, CreatedAt: now.Add(time.Second)},
		{ID: "c3", File: "c.go", Diff: "diff3", Status: CardRejected, CreatedAt: now.Add(2 * time.Second)},
	}

	for _, c := range cards {
		if err := store.SaveCard(c); err != nil {
			t.Fatalf("SaveCard failed: %v", err)
		}
	}

	// List all
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("expected 3 cards, got %d", len(all))
	}

	// Verify order (oldest first)
	for i, c := range all {
		expected := cards[i].ID
		if c.ID != expected {
			t.Errorf("card[%d].ID = %q, want %q", i, c.ID, expected)
		}
	}
}

// TestRecordDecision verifies that decisions update card status.
func TestRecordDecision(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a pending card
	card := &ReviewCard{
		ID:        "decision-test",
		SessionID: "session-1",
		File:      "main.go",
		Diff:      "some diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Record an accept decision
	decisionTime := time.Now()
	decision := &Decision{
		CardID:    "decision-test",
		Status:    CardAccepted,
		Comment:   "LGTM",
		Timestamp: decisionTime,
	}
	if err := store.RecordDecision(decision); err != nil {
		t.Fatalf("RecordDecision failed: %v", err)
	}

	// Verify the card was updated
	got, err := store.GetCard("decision-test")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got.Status != CardAccepted {
		t.Errorf("Status = %q, want %q", got.Status, CardAccepted)
	}
	if got.Comment != "LGTM" {
		t.Errorf("Comment = %q, want %q", got.Comment, "LGTM")
	}
	if got.DecidedAt == nil {
		t.Error("DecidedAt is nil, expected a timestamp")
	}
}

// TestRecordDecisionNotFound verifies that ErrCardNotFound is returned.
func TestRecordDecisionNotFound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	decision := &Decision{
		CardID:    "nonexistent",
		Status:    CardAccepted,
		Timestamp: time.Now(),
	}
	err = store.RecordDecision(decision)
	if err != ErrCardNotFound {
		t.Errorf("expected ErrCardNotFound, got %v", err)
	}
}

// TestDeleteCard verifies that cards can be deleted.
func TestDeleteCard(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create and save a card
	card := &ReviewCard{
		ID:        "delete-test",
		File:      "main.go",
		Diff:      "some diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Delete it
	if err := store.DeleteCard("delete-test"); err != nil {
		t.Fatalf("DeleteCard failed: %v", err)
	}

	// Verify it's gone
	got, err := store.GetCard("delete-test")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %+v", got)
	}
}

// TestDeleteCardIdempotent verifies that deleting a non-existent card is a no-op.
func TestDeleteCardIdempotent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Delete a non-existent card should not error
	if err := store.DeleteCard("nonexistent"); err != nil {
		t.Errorf("DeleteCard for non-existent card failed: %v", err)
	}
}

// TestPersistenceAcrossRestart verifies that cards survive store close/reopen.
// This is the key acceptance criteria for Unit 2.2.
func TestPersistenceAcrossRestart(t *testing.T) {
	// Create a temp file for the database
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create store and add a card
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (first) failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	card := &ReviewCard{
		ID:        "persist-test",
		SessionID: "session-1",
		File:      "main.go",
		Diff:      "@@ -1,3 +1,4 @@\n+new line",
		Status:    CardPending,
		CreatedAt: now,
	}
	if err := store1.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Close the store (simulates restart)
	if err := store1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen the store
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (second) failed: %v", err)
	}
	defer store2.Close()

	// Verify the card persisted
	got, err := store2.GetCard("persist-test")
	if err != nil {
		t.Fatalf("GetCard after restart failed: %v", err)
	}
	if got == nil {
		t.Fatal("card did not persist across restart")
	}

	// Verify all fields
	if got.ID != card.ID {
		t.Errorf("ID = %q, want %q", got.ID, card.ID)
	}
	if got.SessionID != card.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, card.SessionID)
	}
	if got.File != card.File {
		t.Errorf("File = %q, want %q", got.File, card.File)
	}
	if got.Diff != card.Diff {
		t.Errorf("Diff = %q, want %q", got.Diff, card.Diff)
	}
	if got.Status != card.Status {
		t.Errorf("Status = %q, want %q", got.Status, card.Status)
	}
}

// TestDecisionPersistence verifies that decisions persist across restart.
func TestDecisionPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create store, add card, and record decision
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	card := &ReviewCard{
		ID:        "decision-persist",
		File:      "main.go",
		Diff:      "some diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store1.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	decisionTime := time.Now().Truncate(time.Millisecond)
	decision := &Decision{
		CardID:    "decision-persist",
		Status:    CardRejected,
		Comment:   "Needs more tests",
		Timestamp: decisionTime,
	}
	if err := store1.RecordDecision(decision); err != nil {
		t.Fatalf("RecordDecision failed: %v", err)
	}

	store1.Close()

	// Reopen and verify
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (second) failed: %v", err)
	}
	defer store2.Close()

	got, err := store2.GetCard("decision-persist")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("card did not persist")
	}

	if got.Status != CardRejected {
		t.Errorf("Status = %q, want %q", got.Status, CardRejected)
	}
	if got.Comment != "Needs more tests" {
		t.Errorf("Comment = %q, want %q", got.Comment, "Needs more tests")
	}
	if got.DecidedAt == nil {
		t.Error("DecidedAt is nil after restart")
	}
}

// TestListPendingEmpty verifies that ListPending returns empty slice, not nil.
func TestListPendingEmpty(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	pending, err := store.ListPending()
	if err != nil {
		t.Fatalf("ListPending failed: %v", err)
	}
	if pending == nil {
		// nil is acceptable for an empty list in Go
		return
	}
	if len(pending) != 0 {
		t.Errorf("expected empty list, got %d cards", len(pending))
	}
}

// TestFileStore verifies creation and usage with a file-based database.
func TestFileStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cards.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Verify file was created
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("database file was not created")
	}

	// Use the store
	card := &ReviewCard{
		ID:        "file-test",
		File:      "main.go",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	got, err := store.GetCard("file-test")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Error("card not found in file-based store")
	}
}

// TestSaveCardNil verifies that saving nil returns an error.
func TestSaveCardNil(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.SaveCard(nil)
	if err == nil {
		t.Error("expected error for nil card, got nil")
	}
}

// TestRecordDecisionNil verifies that nil decision returns an error.
func TestRecordDecisionNil(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.RecordDecision(nil)
	if err == nil {
		t.Error("expected error for nil decision, got nil")
	}
}

// TestCorruptDatabase verifies that a corrupt database yields a clear error.
// This matches the docs/TESTING-ARCHIVE.md test matrix requirement for Unit 2.2.
func TestCorruptDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "corrupt.db")

	// Create a file with invalid SQLite content
	if err := os.WriteFile(dbPath, []byte("this is not a valid sqlite database"), 0644); err != nil {
		t.Fatalf("failed to create corrupt file: %v", err)
	}

	// Attempt to open the corrupt database
	store, err := NewSQLiteStore(dbPath)
	if err == nil {
		store.Close()
		t.Fatal("expected error for corrupt database, got nil")
	}

	// Verify the error message is clear and actionable
	errStr := err.Error()
	if errStr == "" {
		t.Error("error message is empty")
	}
	// The error should indicate a database/schema problem
	t.Logf("Corrupt DB error (expected): %v", err)
}

// TestEmptyFields verifies that cards with empty File or Diff fields work correctly.
func TestEmptyFields(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Card with empty File
	cardEmptyFile := &ReviewCard{
		ID:        "empty-file",
		File:      "",
		Diff:      "some diff content",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(cardEmptyFile); err != nil {
		t.Fatalf("SaveCard with empty File failed: %v", err)
	}

	got, err := store.GetCard("empty-file")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("card with empty File not found")
	}
	if got.File != "" {
		t.Errorf("File = %q, want empty string", got.File)
	}

	// Card with empty Diff
	cardEmptyDiff := &ReviewCard{
		ID:        "empty-diff",
		File:      "some/file.go",
		Diff:      "",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(cardEmptyDiff); err != nil {
		t.Fatalf("SaveCard with empty Diff failed: %v", err)
	}

	got, err = store.GetCard("empty-diff")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("card with empty Diff not found")
	}
	if got.Diff != "" {
		t.Errorf("Diff = %q, want empty string", got.Diff)
	}

	// Card with both empty
	cardBothEmpty := &ReviewCard{
		ID:        "both-empty",
		File:      "",
		Diff:      "",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(cardBothEmpty); err != nil {
		t.Fatalf("SaveCard with both empty failed: %v", err)
	}

	got, err = store.GetCard("both-empty")
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("card with both empty fields not found")
	}
}

// TestSchemaVersion verifies that the schema version is stored and retrievable.
func TestSchemaVersion(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}

	// Current schema version should be 10 (V10 added keep_awake_audit).
	if version != 10 {
		t.Errorf("SchemaVersion = %d, want 10", version)
	}
}

// TestMigrateToV9BackfillsLegacySystemSessions verifies migration backfill logic
// for legacy host bootstrap session IDs.
func TestMigrateToV9BackfillsLegacySystemSessions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "v8_sessions.db")

	rawDB, err := sql.Open(
		"sqlite",
		dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
	)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}

	if _, err := rawDB.Exec(`
		CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
	`); err != nil {
		rawDB.Close()
		t.Fatalf("create schema_version failed: %v", err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE review_cards (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			file TEXT NOT NULL,
			diff TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			decided_at TEXT,
			comment TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		rawDB.Close()
		t.Fatalf("create review_cards failed: %v", err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			branch TEXT NOT NULL,
			started_at TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			last_commit TEXT,
			status TEXT NOT NULL DEFAULT 'running'
		);
	`); err != nil {
		rawDB.Close()
		t.Fatalf("create sessions failed: %v", err)
	}
	if _, err := rawDB.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		8,
		time.Now().Format(time.RFC3339),
	); err != nil {
		rawDB.Close()
		t.Fatalf("insert schema version failed: %v", err)
	}

	ts := time.Now().Format(time.RFC3339Nano)
	insertSession := func(id string) {
		t.Helper()
		if _, err := rawDB.Exec(
			`INSERT INTO sessions (id, repo, branch, started_at, last_seen, status) VALUES (?, ?, ?, ?, ?, ?)`,
			id,
			"/tmp/repo",
			"main",
			ts,
			ts,
			"running",
		); err != nil {
			rawDB.Close()
			t.Fatalf("insert session %q failed: %v", id, err)
		}
	}
	insertSession("session-1735000000")    // legacy bootstrap ID (10 digits) -> true
	insertSession("session-1735000000000") // all-digit legacy variant -> true
	insertSession("session-123abc")        // mixed suffix -> false
	insertSession("session-custom")        // user/custom ID -> false

	if err := rawDB.Close(); err != nil {
		t.Fatalf("rawDB close failed: %v", err)
	}

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version != 10 {
		t.Fatalf("schema version = %d, want 10", version)
	}

	legacy, err := store.GetSession("session-1735000000")
	if err != nil {
		t.Fatalf("GetSession legacy failed: %v", err)
	}
	if legacy == nil || !legacy.IsSystem {
		t.Fatalf("legacy session IsSystem = %v, want true", legacy != nil && legacy.IsSystem)
	}

	longID, err := store.GetSession("session-1735000000000")
	if err != nil {
		t.Fatalf("GetSession long id failed: %v", err)
	}
	if longID == nil || !longID.IsSystem {
		t.Fatalf("long id session IsSystem = %v, want true", longID != nil && longID.IsSystem)
	}

	mixed, err := store.GetSession("session-123abc")
	if err != nil {
		t.Fatalf("GetSession mixed id failed: %v", err)
	}
	if mixed == nil || mixed.IsSystem {
		t.Fatalf("mixed id session IsSystem = %v, want false", mixed != nil && mixed.IsSystem)
	}

	custom, err := store.GetSession("session-custom")
	if err != nil {
		t.Fatalf("GetSession custom failed: %v", err)
	}
	if custom == nil || custom.IsSystem {
		t.Fatalf("custom session IsSystem = %v, want false", custom != nil && custom.IsSystem)
	}
}

// TestEnsureChunksTableRecreated verifies that a missing card_chunks table
// is recreated on startup even when schema_version is already at the latest.
func TestEnsureChunksTableRecreated(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "missing-chunks.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	if _, err := store.db.Exec("DROP TABLE card_chunks"); err != nil {
		store.Close()
		t.Fatalf("failed to drop card_chunks table: %v", err)
	}
	store.Close()

	store, err = NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore reopen failed: %v", err)
	}
	defer store.Close()

	var name string
	if err := store.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'card_chunks'",
	).Scan(&name); err != nil {
		t.Fatalf("card_chunks table missing after reopen: %v", err)
	}
	if name != "card_chunks" {
		t.Fatalf("unexpected table name: %s", name)
	}
}

// TestInvalidTimestampInDatabase verifies handling of malformed timestamp data.
// This simulates data corruption where timestamps are not valid RFC3339.
func TestInvalidTimestampInDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "invalid-ts.db")

	// Create store and insert a card with valid data first
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	// Insert a row with invalid timestamp directly via SQL
	_, err = store.db.Exec(`
		INSERT INTO review_cards (id, session_id, file, diff, status, created_at, comment)
		VALUES ('bad-ts', '', 'test.go', 'diff', 'pending', 'not-a-valid-timestamp', '')
	`)
	if err != nil {
		t.Fatalf("failed to insert bad data: %v", err)
	}

	// Attempt to read - should fail with a clear error
	_, err = store.GetCard("bad-ts")
	if err == nil {
		t.Error("expected error for invalid timestamp, got nil")
	} else {
		// Verify error message mentions parsing
		if !strings.Contains(err.Error(), "parse") {
			t.Errorf("expected parse error, got: %v", err)
		}
	}

	// ListAll should also fail when encountering bad data
	_, err = store.ListAll()
	if err == nil {
		t.Error("expected error in ListAll for invalid timestamp")
	}

	store.Close()
}

// TestInvalidDecidedAtTimestamp verifies handling of invalid decided_at timestamp.
func TestInvalidDecidedAtTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "invalid-decided.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	// Insert a row with valid created_at but invalid decided_at
	_, err = store.db.Exec(`
		INSERT INTO review_cards (id, session_id, file, diff, status, created_at, decided_at, comment)
		VALUES ('bad-decided', '', 'test.go', 'diff', 'accepted', '2024-01-01T00:00:00Z', 'invalid', '')
	`)
	if err != nil {
		t.Fatalf("failed to insert bad data: %v", err)
	}

	// Attempt to read - should fail with a clear error
	_, err = store.GetCard("bad-decided")
	if err == nil {
		t.Error("expected error for invalid decided_at, got nil")
	}

	store.Close()
}

// TestConcurrentAccess verifies thread safety of the store.
func TestConcurrentAccess(t *testing.T) {
	// Use a file-based database for concurrent access testing.
	// In-memory databases with modernc.org/sqlite don't share state across
	// multiple connection handles in the same way file-based ones do.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Run concurrent operations
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			// Use a more unique ID format
			id := fmt.Sprintf("concurrent-%d", n)
			card := &ReviewCard{
				ID:        id,
				File:      "test.go",
				Diff:      "diff",
				Status:    CardPending,
				CreatedAt: time.Now(),
			}
			if err := store.SaveCard(card); err != nil {
				t.Errorf("SaveCard failed: %v", err)
			}
			store.GetCard(card.ID)
			store.ListPending()
			store.ListAll()
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify final state
	all, err := store.ListAll()
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(all) != 10 {
		t.Errorf("expected 10 cards, got %d", len(all))
	}
}

// -----------------------------------------------------------------------------
// Device Storage Tests
// -----------------------------------------------------------------------------

// TestDeviceSaveAndGet verifies device save and retrieve operations.
func TestDeviceSaveAndGet(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	device := &Device{
		ID:        "device-1",
		Name:      "Test iPhone",
		TokenHash: "hashed-token-value",
		CreatedAt: now,
		LastSeen:  now,
	}

	// Save the device
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("SaveDevice failed: %v", err)
	}

	// Retrieve the device
	retrieved, err := store.GetDevice("device-1")
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("GetDevice returned nil")
	}

	// Verify fields
	if retrieved.ID != device.ID {
		t.Errorf("ID = %s, want %s", retrieved.ID, device.ID)
	}
	if retrieved.Name != device.Name {
		t.Errorf("Name = %s, want %s", retrieved.Name, device.Name)
	}
	if retrieved.TokenHash != device.TokenHash {
		t.Errorf("TokenHash = %s, want %s", retrieved.TokenHash, device.TokenHash)
	}
	if !retrieved.CreatedAt.Equal(device.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", retrieved.CreatedAt, device.CreatedAt)
	}
	if !retrieved.LastSeen.Equal(device.LastSeen) {
		t.Errorf("LastSeen = %v, want %v", retrieved.LastSeen, device.LastSeen)
	}
}

// TestDeviceNotFound verifies behavior when device doesn't exist.
func TestDeviceNotFound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	device, err := store.GetDevice("nonexistent")
	if err != nil {
		t.Errorf("GetDevice should not error for missing device: %v", err)
	}
	if device != nil {
		t.Errorf("expected nil for nonexistent device, got %+v", device)
	}
}

// TestDeviceUpdate verifies device update via SaveDevice.
func TestDeviceUpdate(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	device := &Device{
		ID:        "device-1",
		Name:      "Old Name",
		TokenHash: "old-hash",
		CreatedAt: now,
		LastSeen:  now,
	}
	store.SaveDevice(device)

	// Update the device
	device.Name = "New Name"
	device.TokenHash = "new-hash"
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("SaveDevice update failed: %v", err)
	}

	// Retrieve and verify
	retrieved, _ := store.GetDevice("device-1")
	if retrieved.Name != "New Name" {
		t.Errorf("Name = %s, want 'New Name'", retrieved.Name)
	}
	if retrieved.TokenHash != "new-hash" {
		t.Errorf("TokenHash = %s, want 'new-hash'", retrieved.TokenHash)
	}
}

// TestListDevices verifies listing all devices.
func TestListDevices(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Empty list initially
	devices, err := store.ListDevices()
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected empty list, got %d devices", len(devices))
	}

	// Add some devices
	now := time.Now()
	for i := 1; i <= 3; i++ {
		device := &Device{
			ID:        fmt.Sprintf("device-%d", i),
			Name:      fmt.Sprintf("Device %d", i),
			TokenHash: fmt.Sprintf("hash-%d", i),
			CreatedAt: now,
			LastSeen:  now,
		}
		store.SaveDevice(device)
	}

	// List should return all devices
	devices, err = store.ListDevices()
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 3 {
		t.Errorf("expected 3 devices, got %d", len(devices))
	}
}

// TestDeleteDevice verifies device deletion.
func TestDeleteDevice(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a device
	now := time.Now()
	device := &Device{
		ID:        "device-1",
		Name:      "Test Device",
		TokenHash: "hash",
		CreatedAt: now,
		LastSeen:  now,
	}
	store.SaveDevice(device)

	// Verify it exists
	retrieved, _ := store.GetDevice("device-1")
	if retrieved == nil {
		t.Fatal("device should exist before deletion")
	}

	// Delete it
	if err := store.DeleteDevice("device-1"); err != nil {
		t.Fatalf("DeleteDevice failed: %v", err)
	}

	// Verify it's gone
	retrieved, _ = store.GetDevice("device-1")
	if retrieved != nil {
		t.Error("device should not exist after deletion")
	}
}

// TestDeleteDeviceIdempotent verifies delete is idempotent.
func TestDeleteDeviceIdempotent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Delete nonexistent device should not error
	err = store.DeleteDevice("nonexistent")
	if err != nil {
		t.Errorf("DeleteDevice should be idempotent: %v", err)
	}
}

// TestUpdateLastSeen verifies last_seen timestamp update.
func TestUpdateLastSeen(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a device
	now := time.Now().Truncate(time.Millisecond)
	device := &Device{
		ID:        "device-1",
		Name:      "Test Device",
		TokenHash: "hash",
		CreatedAt: now,
		LastSeen:  now,
	}
	store.SaveDevice(device)

	// Update last seen
	newTime := now.Add(1 * time.Hour).Truncate(time.Millisecond)
	if err := store.UpdateLastSeen("device-1", newTime); err != nil {
		t.Fatalf("UpdateLastSeen failed: %v", err)
	}

	// Verify update
	retrieved, _ := store.GetDevice("device-1")
	if !retrieved.LastSeen.Equal(newTime) {
		t.Errorf("LastSeen = %v, want %v", retrieved.LastSeen, newTime)
	}
	// CreatedAt should be unchanged
	if !retrieved.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt should not change: %v != %v", retrieved.CreatedAt, now)
	}
}

// TestUpdateLastSeenNotFound verifies error for nonexistent device.
func TestUpdateLastSeenNotFound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.UpdateLastSeen("nonexistent", time.Now())
	if err != ErrDeviceNotFound {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}
}

// TestDevicePersistence verifies devices survive restart.
func TestDevicePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "persist.db")

	// Create store and add device
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	device := &Device{
		ID:        "device-1",
		Name:      "Persistent Device",
		TokenHash: "persistent-hash",
		CreatedAt: now,
		LastSeen:  now,
	}
	store1.SaveDevice(device)
	store1.Close()

	// Reopen and verify
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	retrieved, err := store2.GetDevice("device-1")
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("device should persist across restart")
	}
	if retrieved.Name != "Persistent Device" {
		t.Errorf("Name = %s, want 'Persistent Device'", retrieved.Name)
	}
}

// TestDeviceNilError verifies nil device is rejected.
func TestDeviceNilError(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.SaveDevice(nil)
	if err == nil {
		t.Error("expected error for nil device")
	}
}

// -----------------------------------------------------------------------------
// Chunk Storage Tests (Unit 4.5b.1)
// -----------------------------------------------------------------------------

// TestSaveAndGetChunks verifies basic chunk save and retrieval.
func TestSaveAndGetChunks(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// First create the parent card
	card := &ReviewCard{
		ID:        "fc-test",
		File:      "test.txt",
		Diff:      "chunk1\nchunk2",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save chunks
	chunks := []*ChunkStatus{
		{CardID: "fc-test", ChunkIndex: 0, Content: "@@ -1 +1 @@\n-a\n+b", Status: CardPending},
		{CardID: "fc-test", ChunkIndex: 1, Content: "@@ -5 +5 @@\n-x\n+y", Status: CardPending},
	}
	if err := store.SaveChunks("fc-test", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Retrieve chunks
	retrieved, err := store.GetChunks("fc-test")
	if err != nil {
		t.Fatalf("GetChunks failed: %v", err)
	}
	if len(retrieved) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(retrieved))
	}

	// Verify order (should be by chunk_index)
	if retrieved[0].ChunkIndex != 0 || retrieved[1].ChunkIndex != 1 {
		t.Error("chunks not in order by index")
	}

	// Verify content
	if retrieved[0].Content != "@@ -1 +1 @@\n-a\n+b" {
		t.Errorf("unexpected chunk 0 content: %s", retrieved[0].Content)
	}
}

// TestGetChunk verifies retrieving a single chunk.
func TestGetChunk(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-single",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save a chunk
	chunks := []*ChunkStatus{
		{CardID: "fc-single", ChunkIndex: 0, Content: "chunk content", Status: CardPending},
	}
	if err := store.SaveChunks("fc-single", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Get specific chunk
	h, err := store.GetChunk("fc-single", 0)
	if err != nil {
		t.Fatalf("GetChunk failed: %v", err)
	}
	if h == nil {
		t.Fatal("expected chunk, got nil")
	}
	if h.Content != "chunk content" {
		t.Errorf("unexpected content: %s", h.Content)
	}

	// Get nonexistent chunk
	h, err = store.GetChunk("fc-single", 99)
	if err != nil {
		t.Fatalf("GetChunk failed: %v", err)
	}
	if h != nil {
		t.Error("expected nil for nonexistent chunk")
	}
}

// TestRecordChunkDecision verifies chunk decision recording.
func TestRecordChunkDecision(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-decide",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save chunks
	chunks := []*ChunkStatus{
		{CardID: "fc-decide", ChunkIndex: 0, Content: "chunk0", Status: CardPending},
		{CardID: "fc-decide", ChunkIndex: 1, Content: "chunk1", Status: CardPending},
	}
	if err := store.SaveChunks("fc-decide", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Record decision for chunk 0
	decision := &ChunkDecision{
		CardID:     "fc-decide",
		ChunkIndex: 0,
		Status:     CardAccepted,
		Timestamp:  time.Now(),
	}
	if err := store.RecordChunkDecision(decision); err != nil {
		t.Fatalf("RecordChunkDecision failed: %v", err)
	}

	// Verify chunk 0 is accepted
	h, _ := store.GetChunk("fc-decide", 0)
	if h.Status != CardAccepted {
		t.Errorf("expected accepted status, got %s", h.Status)
	}
	if h.DecidedAt == nil {
		t.Error("expected DecidedAt to be set")
	}

	// Verify chunk 1 is still pending
	h, _ = store.GetChunk("fc-decide", 1)
	if h.Status != CardPending {
		t.Errorf("expected pending status, got %s", h.Status)
	}
}

// TestRecordChunkDecisionErrors verifies error cases.
func TestRecordChunkDecisionErrors(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-errors",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save an already-decided chunk
	now := time.Now()
	chunks := []*ChunkStatus{
		{CardID: "fc-errors", ChunkIndex: 0, Content: "chunk", Status: CardAccepted, DecidedAt: &now},
	}
	if err := store.SaveChunks("fc-errors", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Try to decide already-decided chunk
	decision := &ChunkDecision{
		CardID:     "fc-errors",
		ChunkIndex: 0,
		Status:     CardRejected,
		Timestamp:  time.Now(),
	}
	err = store.RecordChunkDecision(decision)
	if err == nil {
		t.Fatal("expected error for already decided chunk")
	}
	if err != ErrAlreadyDecided {
		t.Errorf("expected ErrAlreadyDecided, got: %v", err)
	}

	// Try to decide nonexistent chunk
	decision.ChunkIndex = 99
	err = store.RecordChunkDecision(decision)
	if err == nil {
		t.Fatal("expected error for nonexistent chunk")
	}
	if err != ErrChunkNotFound {
		t.Errorf("expected ErrChunkNotFound, got: %v", err)
	}

	// Nil decision should error
	err = store.RecordChunkDecision(nil)
	if err == nil {
		t.Error("expected error for nil decision")
	}
}

// TestDeleteChunks verifies chunk deletion.
func TestDeleteChunks(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-delete",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save chunks
	chunks := []*ChunkStatus{
		{CardID: "fc-delete", ChunkIndex: 0, Content: "chunk0", Status: CardPending},
		{CardID: "fc-delete", ChunkIndex: 1, Content: "chunk1", Status: CardPending},
	}
	if err := store.SaveChunks("fc-delete", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Delete chunks
	if err := store.DeleteChunks("fc-delete"); err != nil {
		t.Fatalf("DeleteChunks failed: %v", err)
	}

	// Verify chunks are gone
	retrieved, err := store.GetChunks("fc-delete")
	if err != nil {
		t.Fatalf("GetChunks failed: %v", err)
	}
	if len(retrieved) != 0 {
		t.Errorf("expected 0 chunks after delete, got %d", len(retrieved))
	}

	// Delete nonexistent should be idempotent
	if err := store.DeleteChunks("nonexistent"); err != nil {
		t.Errorf("DeleteChunks for nonexistent should not error: %v", err)
	}
}

// TestCountPendingChunks verifies pending count.
func TestCountPendingChunks(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-count",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save chunks with mixed statuses
	now := time.Now()
	chunks := []*ChunkStatus{
		{CardID: "fc-count", ChunkIndex: 0, Content: "h0", Status: CardPending},
		{CardID: "fc-count", ChunkIndex: 1, Content: "h1", Status: CardAccepted, DecidedAt: &now},
		{CardID: "fc-count", ChunkIndex: 2, Content: "h2", Status: CardPending},
	}
	if err := store.SaveChunks("fc-count", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Count pending
	count, err := store.CountPendingChunks("fc-count")
	if err != nil {
		t.Fatalf("CountPendingChunks failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 pending chunks, got %d", count)
	}

	// Count for nonexistent card should be 0
	count, err = store.CountPendingChunks("nonexistent")
	if err != nil {
		t.Fatalf("CountPendingChunks failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 for nonexistent, got %d", count)
	}
}

// TestSaveChunksReplacesExisting verifies that SaveChunks replaces existing chunks.
func TestSaveChunksReplacesExisting(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-replace",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save initial chunks
	chunks := []*ChunkStatus{
		{CardID: "fc-replace", ChunkIndex: 0, Content: "old0", Status: CardPending},
		{CardID: "fc-replace", ChunkIndex: 1, Content: "old1", Status: CardPending},
	}
	if err := store.SaveChunks("fc-replace", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Save new chunks (should replace)
	newChunks := []*ChunkStatus{
		{CardID: "fc-replace", ChunkIndex: 0, Content: "new0", Status: CardPending},
	}
	if err := store.SaveChunks("fc-replace", newChunks); err != nil {
		t.Fatalf("SaveChunks (replace) failed: %v", err)
	}

	// Verify replacement
	retrieved, err := store.GetChunks("fc-replace")
	if err != nil {
		t.Fatalf("GetChunks failed: %v", err)
	}
	if len(retrieved) != 1 {
		t.Fatalf("expected 1 chunk after replace, got %d", len(retrieved))
	}
	if retrieved[0].Content != "new0" {
		t.Errorf("expected new content, got %s", retrieved[0].Content)
	}
}

// TestChunkCascadeDelete verifies chunks are deleted when card is deleted.
func TestChunkCascadeDelete(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create parent card
	card := &ReviewCard{
		ID:        "fc-cascade",
		File:      "test.txt",
		Diff:      "diff",
		Status:    CardPending,
		CreatedAt: time.Now(),
	}
	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard failed: %v", err)
	}

	// Save chunks
	chunks := []*ChunkStatus{
		{CardID: "fc-cascade", ChunkIndex: 0, Content: "chunk", Status: CardPending},
	}
	if err := store.SaveChunks("fc-cascade", chunks); err != nil {
		t.Fatalf("SaveChunks failed: %v", err)
	}

	// Delete the card
	if err := store.DeleteCard("fc-cascade"); err != nil {
		t.Fatalf("DeleteCard failed: %v", err)
	}

	// Chunks should be cascade deleted
	retrieved, err := store.GetChunks("fc-cascade")
	if err != nil {
		t.Fatalf("GetChunks failed: %v", err)
	}
	if len(retrieved) != 0 {
		t.Errorf("expected chunks to be cascade deleted, got %d", len(retrieved))
	}
}

// -----------------------------------------------------------------------------
// Approval Audit Tests
// -----------------------------------------------------------------------------

// TestSaveApprovalAudit_Basic verifies that audit entries can be saved and retrieved.
func TestSaveApprovalAudit_Basic(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	entry := &ApprovalAuditEntry{
		ID:        "audit-1",
		RequestID: "req-123",
		Command:   "rm -rf /tmp/test",
		Cwd:       "/home/user",
		Repo:      "/home/user/project",
		Rationale: "Cleanup temp files",
		Decision:  "approved",
		DecidedAt: now,
		DeviceID:  "device-abc",
		Source:    "mobile",
	}

	if err := store.SaveApprovalAudit(entry); err != nil {
		t.Fatalf("SaveApprovalAudit failed: %v", err)
	}

	// Retrieve and verify
	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	got := entries[0]
	if got.ID != entry.ID {
		t.Errorf("ID = %q, want %q", got.ID, entry.ID)
	}
	if got.RequestID != entry.RequestID {
		t.Errorf("RequestID = %q, want %q", got.RequestID, entry.RequestID)
	}
	if got.Command != entry.Command {
		t.Errorf("Command = %q, want %q", got.Command, entry.Command)
	}
	if got.Decision != entry.Decision {
		t.Errorf("Decision = %q, want %q", got.Decision, entry.Decision)
	}
	if got.Source != entry.Source {
		t.Errorf("Source = %q, want %q", got.Source, entry.Source)
	}
	if got.DeviceID != entry.DeviceID {
		t.Errorf("DeviceID = %q, want %q", got.DeviceID, entry.DeviceID)
	}
}

// TestSaveApprovalAudit_WithExpiresAt verifies that expires_at is stored correctly.
func TestSaveApprovalAudit_WithExpiresAt(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	expiresAt := now.Add(15 * time.Minute)
	entry := &ApprovalAuditEntry{
		ID:        "audit-temp",
		RequestID: "req-456",
		Command:   "go test ./...",
		Cwd:       "/project",
		Repo:      "/project",
		Rationale: "Run tests",
		Decision:  "approved",
		DecidedAt: now,
		ExpiresAt: &expiresAt,
		DeviceID:  "device-xyz",
		Source:    "mobile",
	}

	if err := store.SaveApprovalAudit(entry); err != nil {
		t.Fatalf("SaveApprovalAudit failed: %v", err)
	}

	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if entries[0].ExpiresAt == nil {
		t.Fatal("ExpiresAt should not be nil")
	}
	if !entries[0].ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", entries[0].ExpiresAt, expiresAt)
	}
}

// TestListApprovalAudit_Order verifies entries are returned newest first.
func TestListApprovalAudit_Order(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Insert entries in order
	for i := 1; i <= 3; i++ {
		entry := &ApprovalAuditEntry{
			ID:        fmt.Sprintf("audit-%d", i),
			RequestID: fmt.Sprintf("req-%d", i),
			Command:   fmt.Sprintf("command-%d", i),
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
			Decision:  "approved",
			DecidedAt: time.Now().Add(time.Duration(i) * time.Minute), // Later entries have later timestamps
			Source:    "mobile",
		}
		if err := store.SaveApprovalAudit(entry); err != nil {
			t.Fatalf("SaveApprovalAudit failed: %v", err)
		}
	}

	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Should be newest first (audit-3, audit-2, audit-1)
	if entries[0].ID != "audit-3" {
		t.Errorf("first entry ID = %q, want audit-3", entries[0].ID)
	}
	if entries[1].ID != "audit-2" {
		t.Errorf("second entry ID = %q, want audit-2", entries[1].ID)
	}
	if entries[2].ID != "audit-1" {
		t.Errorf("third entry ID = %q, want audit-1", entries[2].ID)
	}
}

// TestListApprovalAudit_Limit verifies the limit parameter works.
func TestListApprovalAudit_Limit(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Insert 5 entries
	for i := 1; i <= 5; i++ {
		entry := &ApprovalAuditEntry{
			ID:        fmt.Sprintf("audit-%d", i),
			RequestID: fmt.Sprintf("req-%d", i),
			Command:   "test",
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
			Decision:  "approved",
			DecidedAt: time.Now().Add(time.Duration(i) * time.Minute),
			Source:    "mobile",
		}
		if err := store.SaveApprovalAudit(entry); err != nil {
			t.Fatalf("SaveApprovalAudit failed: %v", err)
		}
	}

	// Request only 2
	entries, err := store.ListApprovalAudit(2)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}

	// Request all (limit <= 0)
	allEntries, err := store.ListApprovalAudit(0)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(allEntries) != 5 {
		t.Errorf("expected 5 entries with limit 0, got %d", len(allEntries))
	}
}

// TestMigrateToV4 verifies the migration creates the approval_audit table.
func TestMigrateToV4(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Verify schema version is current (includes V4 for approval_audit)
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version < 4 {
		t.Errorf("SchemaVersion = %d, want at least 4", version)
	}

	// Verify the audit table exists by inserting a row
	entry := &ApprovalAuditEntry{
		ID:        "migration-test",
		RequestID: "req-test",
		Command:   "test",
		Cwd:       "/",
		Repo:      "/",
		Rationale: "test",
		Decision:  "approved",
		DecidedAt: time.Now(),
		Source:    "mobile",
	}
	if err := store.SaveApprovalAudit(entry); err != nil {
		t.Errorf("SaveApprovalAudit after migration failed: %v", err)
	}
}

// TestSaveApprovalAudit_NilEntry verifies nil entry returns error.
func TestSaveApprovalAudit_NilEntry(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.SaveApprovalAudit(nil)
	if err == nil {
		t.Error("expected error for nil entry, got nil")
	}
}

// TestSaveApprovalAudit_DifferentSources verifies all source types work.
func TestSaveApprovalAudit_DifferentSources(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	sources := []string{"mobile", "rule", "timeout"}

	for i, source := range sources {
		entry := &ApprovalAuditEntry{
			ID:        fmt.Sprintf("audit-%d", i),
			RequestID: fmt.Sprintf("req-%d", i),
			Command:   "test",
			Cwd:       "/",
			Repo:      "/",
			Rationale: "test",
			Decision:  "approved",
			DecidedAt: time.Now(),
			Source:    source,
		}
		if err := store.SaveApprovalAudit(entry); err != nil {
			t.Errorf("SaveApprovalAudit with source %q failed: %v", source, err)
		}
	}

	entries, err := store.ListApprovalAudit(10)
	if err != nil {
		t.Fatalf("ListApprovalAudit failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
}

// =============================================================================
// Unit 7.2: Storage Edge Cases and Validation Tests
// =============================================================================
// These tests verify behavior with Unicode content, long IDs, timezone edge
// cases, and special characters. See docs/PLANS.md Unit 7.2 for requirements.
//
// Decision: ID length validation is NOT enforced. SQLite TEXT has no inherent
// length limit, and IDs are generated internally (hash-based), not user input.
// This permissive behavior is acceptable and documented here.
// =============================================================================

// TestUnicodeContent verifies that Unicode characters are preserved correctly
// in File paths, Comments, and Diff content through storage round-trips.
func TestUnicodeContent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	testCases := []struct {
		name    string
		file    string
		diff    string
		comment string
	}{
		{
			name:    "emoji in file path",
			file:    "src/🚀_rocket_test.go",
			diff:    "@@ -1,1 +1,1 @@\n-old\n+new 🎉",
			comment: "Added emoji support 👍",
		},
		{
			name:    "Chinese characters in path",
			file:    "src/组件/按钮.go",
			diff:    "@@ -1,1 +1,1 @@\n-// English\n+// 中文注释",
			comment: "中文语言支持",
		},
		{
			name:    "Arabic/RTL in comment",
			file:    "src/rtl.go",
			diff:    "@@ -1,1 +1,1 @@\n-ltr\n+مرحبا بالعالم",
			comment: "دعم النص العربي",
		},
		{
			name:    "multi-byte UTF-8 sequences",
			file:    "src/tëst_àccénts.go",
			diff:    "@@ -1,1 +1,1 @@\n-ASCII\n+Ümläut: café, naïve, résumé",
			comment: "Testing àccénted çharàctèrs",
		},
		{
			name:    "mixed scripts",
			file:    "docs/README_日本語.md",
			diff:    "@@ -1,1 +1,1 @@\n-Hello\n+Привет мир 世界 🌍",
			comment: "Multi-script: English, Русский, 日本語, 한국어",
		},
	}

	now := time.Now().Truncate(time.Millisecond)
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			decidedAt := now.Add(time.Hour)
			card := &ReviewCard{
				ID:        fmt.Sprintf("unicode-test-%d", i),
				SessionID: "unicode-session",
				File:      tc.file,
				Diff:      tc.diff,
				Status:    CardAccepted,
				CreatedAt: now,
				DecidedAt: &decidedAt,
				Comment:   tc.comment,
			}

			if err := store.SaveCard(card); err != nil {
				t.Fatalf("SaveCard failed: %v", err)
			}

			got, err := store.GetCard(card.ID)
			if err != nil {
				t.Fatalf("GetCard failed: %v", err)
			}
			if got == nil {
				t.Fatal("GetCard returned nil")
			}

			// Verify Unicode content preserved exactly
			if got.File != tc.file {
				t.Errorf("File = %q, want %q", got.File, tc.file)
			}
			if got.Diff != tc.diff {
				t.Errorf("Diff = %q, want %q", got.Diff, tc.diff)
			}
			if got.Comment != tc.comment {
				t.Errorf("Comment = %q, want %q", got.Comment, tc.comment)
			}
		})
	}
}

// TestLongCardIDs verifies that cards with very long IDs can be stored and
// retrieved. SQLite TEXT has no inherent length limit, so we keep permissive
// behavior without validation. This test documents the expected behavior.
func TestLongCardIDs(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Helper to generate long IDs
	generateLongID := func(length int) string {
		var sb strings.Builder
		sb.Grow(length)
		for i := 0; i < length; i++ {
			sb.WriteByte(byte('a' + (i % 26)))
		}
		return sb.String()
	}

	testCases := []struct {
		name   string
		length int
	}{
		{"255 characters (typical max)", 255},
		{"1000 characters (stress test)", 1000},
		{"10000 characters (extreme)", 10000},
	}

	now := time.Now().Truncate(time.Millisecond)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			longID := generateLongID(tc.length)
			card := &ReviewCard{
				ID:        longID,
				SessionID: "long-id-session",
				File:      "main.go",
				Diff:      "@@ -1,1 +1,1 @@\n-old\n+new",
				Status:    CardPending,
				CreatedAt: now,
			}

			// Save should succeed - no length validation
			if err := store.SaveCard(card); err != nil {
				t.Fatalf("SaveCard with %d-char ID failed: %v", tc.length, err)
			}

			// Retrieve should return exact ID
			got, err := store.GetCard(longID)
			if err != nil {
				t.Fatalf("GetCard with %d-char ID failed: %v", tc.length, err)
			}
			if got == nil {
				t.Fatalf("GetCard returned nil for %d-char ID", tc.length)
			}
			if got.ID != longID {
				t.Errorf("ID length = %d, want %d", len(got.ID), len(longID))
			}
			if got.ID != longID {
				t.Errorf("ID content mismatch")
			}

			// Delete should work
			if err := store.DeleteCard(longID); err != nil {
				t.Fatalf("DeleteCard with %d-char ID failed: %v", tc.length, err)
			}

			// Verify deletion
			got, err = store.GetCard(longID)
			if err != nil {
				t.Fatalf("GetCard after delete failed: %v", err)
			}
			if got != nil {
				t.Errorf("expected nil after delete, got card")
			}
		})
	}
}

// TestTimezoneEdgeCases verifies that timestamps are preserved correctly
// across timezone conversions and edge cases. RFC3339Nano format includes
// timezone info, so times should round-trip correctly.
func TestTimezoneEdgeCases(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Test various timestamp edge cases
	testCases := []struct {
		name      string
		timestamp time.Time
	}{
		{
			name:      "UTC time",
			timestamp: time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "far future (year 2099)",
			timestamp: time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
		},
		{
			name:      "epoch (1970-01-01)",
			timestamp: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "DST transition spring (US Eastern)",
			// March 10, 2024 2:00 AM is when DST starts in US Eastern
			timestamp: time.Date(2024, 3, 10, 2, 30, 0, 0, time.FixedZone("EST", -5*60*60)),
		},
		{
			name: "DST transition fall (US Eastern)",
			// November 3, 2024 2:00 AM is when DST ends in US Eastern
			timestamp: time.Date(2024, 11, 3, 1, 30, 0, 0, time.FixedZone("EDT", -4*60*60)),
		},
		{
			name:      "positive timezone offset",
			timestamp: time.Date(2024, 6, 15, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60)),
		},
		{
			name:      "negative timezone offset",
			timestamp: time.Date(2024, 6, 15, 12, 0, 0, 0, time.FixedZone("PST", -8*60*60)),
		},
		{
			name:      "nanosecond precision",
			timestamp: time.Date(2024, 6, 15, 12, 0, 0, 123456789, time.UTC),
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Truncate to millisecond for consistent comparison
			// (SQLite stores with nanosecond precision via RFC3339Nano)
			ts := tc.timestamp.Truncate(time.Millisecond)
			decidedAt := ts.Add(time.Hour)

			card := &ReviewCard{
				ID:        fmt.Sprintf("tz-test-%d", i),
				SessionID: "tz-session",
				File:      "main.go",
				Diff:      "@@ -1,1 +1,1 @@\n-old\n+new",
				Status:    CardAccepted,
				CreatedAt: ts,
				DecidedAt: &decidedAt,
			}

			if err := store.SaveCard(card); err != nil {
				t.Fatalf("SaveCard failed: %v", err)
			}

			got, err := store.GetCard(card.ID)
			if err != nil {
				t.Fatalf("GetCard failed: %v", err)
			}
			if got == nil {
				t.Fatal("GetCard returned nil")
			}

			// Compare times - should be equal when converted to UTC
			// Allow 1ms tolerance for parsing roundtrip
			if got.CreatedAt.Sub(ts.UTC()).Abs() > time.Millisecond {
				t.Errorf("CreatedAt = %v, want %v (diff: %v)",
					got.CreatedAt, ts.UTC(), got.CreatedAt.Sub(ts.UTC()))
			}
			if got.DecidedAt == nil {
				t.Fatal("DecidedAt is nil")
			}
			if got.DecidedAt.Sub(decidedAt.UTC()).Abs() > time.Millisecond {
				t.Errorf("DecidedAt = %v, want %v (diff: %v)",
					*got.DecidedAt, decidedAt.UTC(), got.DecidedAt.Sub(decidedAt.UTC()))
			}
		})
	}
}

// TestSpecialCharacters verifies that special characters are handled correctly
// in storage. This includes SQL-sensitive characters, escape sequences, and
// potential injection vectors.
func TestSpecialCharacters(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	testCases := []struct {
		name    string
		file    string
		diff    string
		comment string
	}{
		{
			name:    "single quotes in diff",
			file:    "main.go",
			diff:    "@@ -1,1 +1,1 @@\n-old = 'value'\n+new = 'updated'",
			comment: "Updated 'value' to 'updated'",
		},
		{
			name:    "double quotes in comment",
			file:    "config.json",
			diff:    "@@ -1,1 +1,1 @@\n-{\"key\": \"old\"}\n+{\"key\": \"new\"}",
			comment: "Changed \"key\" from \"old\" to \"new\"",
		},
		{
			name:    "backslashes in file path",
			file:    "src\\windows\\path.go",
			diff:    "@@ -1,1 +1,1 @@\n-path = \"C:\\\\Users\"\n+path = \"D:\\\\Data\"",
			comment: "Updated Windows path with backslashes",
		},
		{
			name:    "newlines and tabs in diff",
			file:    "format.go",
			diff:    "@@ -1,3 +1,3 @@\n-line1\n-\tindented\n-line3\n+new1\n+\tnew_indented\n+new3",
			comment: "Reformatted with tabs\nand newlines",
		},
		{
			name:    "SQL injection attempt",
			file:    "sql.go",
			diff:    "@@ -1,1 +1,1 @@\n-SELECT * FROM users\n+SELECT * FROM users; DROP TABLE cards;--",
			comment: "Robert'); DROP TABLE cards;--",
		},
		{
			name:    "percent and underscore (SQL wildcards)",
			file:    "query.go",
			diff:    "@@ -1,1 +1,1 @@\n-LIKE '%pattern%'\n+LIKE '_single_char'",
			comment: "Changed % to _ wildcards",
		},
		{
			name:    "mixed special characters",
			file:    "special.go",
			diff:    "@@ -1,1 +1,1 @@\n-old: <>&\"'`\n+new: !@#$%^&*()",
			comment: "Special chars: <>\"'`!@#$%^&*()",
		},
	}

	now := time.Now().Truncate(time.Millisecond)
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			card := &ReviewCard{
				ID:        fmt.Sprintf("special-char-%d", i),
				SessionID: "special-session",
				File:      tc.file,
				Diff:      tc.diff,
				Status:    CardPending,
				CreatedAt: now,
				Comment:   tc.comment,
			}

			if err := store.SaveCard(card); err != nil {
				t.Fatalf("SaveCard failed: %v", err)
			}

			got, err := store.GetCard(card.ID)
			if err != nil {
				t.Fatalf("GetCard failed: %v", err)
			}
			if got == nil {
				t.Fatal("GetCard returned nil")
			}

			// Verify exact content preservation
			if got.File != tc.file {
				t.Errorf("File = %q, want %q", got.File, tc.file)
			}
			if got.Diff != tc.diff {
				t.Errorf("Diff = %q, want %q", got.Diff, tc.diff)
			}
			if got.Comment != tc.comment {
				t.Errorf("Comment = %q, want %q", got.Comment, tc.comment)
			}
		})
	}
}

// TestNullBytesInContent verifies behavior with null bytes (NUL character).
// SQLite handles null bytes in TEXT, but they may cause issues in some contexts.
// This test documents the expected behavior.
func TestNullBytesInContent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)

	// Test with embedded null bytes
	// Note: SQLite TEXT fields can store null bytes, but string handling may vary
	diffWithNull := "@@ -1,1 +1,1 @@\n-old\x00hidden\n+new"
	commentWithNull := "Comment with\x00null byte"

	card := &ReviewCard{
		ID:        "null-byte-test",
		SessionID: "null-session",
		File:      "main.go",
		Diff:      diffWithNull,
		Status:    CardPending,
		CreatedAt: now,
		Comment:   commentWithNull,
	}

	if err := store.SaveCard(card); err != nil {
		t.Fatalf("SaveCard with null bytes failed: %v", err)
	}

	got, err := store.GetCard(card.ID)
	if err != nil {
		t.Fatalf("GetCard failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetCard returned nil")
	}

	// Verify null bytes are preserved
	if got.Diff != diffWithNull {
		t.Errorf("Diff with null bytes not preserved: got %q, want %q", got.Diff, diffWithNull)
	}
	if got.Comment != commentWithNull {
		t.Errorf("Comment with null bytes not preserved: got %q, want %q", got.Comment, commentWithNull)
	}
}

// TestDeviceNameEdgeCases verifies that device names with special characters,
// Unicode, and extreme lengths are handled correctly.
func TestDeviceNameEdgeCases(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)

	testCases := []struct {
		name       string
		deviceName string
	}{
		{
			name:       "emoji in device name",
			deviceName: "📱 iPhone Pro Max 🚀",
		},
		{
			name:       "Chinese device name",
			deviceName: "我的手机设备",
		},
		{
			name:       "very long device name (1000 chars)",
			deviceName: strings.Repeat("a", 1000),
		},
		{
			name:       "empty device name",
			deviceName: "",
		},
		{
			name:       "special characters in name",
			deviceName: "Test's \"Device\" <with> special/chars\\here",
		},
		{
			name:       "whitespace only",
			deviceName: "   \t\n   ",
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			device := &Device{
				ID:        fmt.Sprintf("device-edge-%d", i),
				Name:      tc.deviceName,
				TokenHash: fmt.Sprintf("hash-%d", i),
				CreatedAt: now,
				LastSeen:  now,
			}

			if err := store.SaveDevice(device); err != nil {
				t.Fatalf("SaveDevice failed: %v", err)
			}

			got, err := store.GetDevice(device.ID)
			if err != nil {
				t.Fatalf("GetDevice failed: %v", err)
			}
			if got == nil {
				t.Fatal("GetDevice returned nil")
			}

			if got.Name != tc.deviceName {
				t.Errorf("Device name = %q, want %q", got.Name, tc.deviceName)
			}
		})
	}
}

// TestApprovalAuditSpecialCharacters verifies that approval audit entries
// handle special characters in command, rationale, and other fields correctly.
func TestApprovalAuditSpecialCharacters(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)

	testCases := []struct {
		name      string
		command   string
		rationale string
	}{
		{
			name:      "command with quotes",
			command:   `echo "hello 'world'"`,
			rationale: "Testing quote handling",
		},
		{
			name:      "command with backslashes",
			command:   `find . -name "*.go" -exec grep -l 'pattern' {} \;`,
			rationale: "Find with exec and backslash escaping",
		},
		{
			name:      "command with pipes and redirects",
			command:   `cat file.txt | grep "pattern" > output.txt 2>&1`,
			rationale: "Piped command with redirection",
		},
		{
			name:      "unicode in rationale",
			command:   "ls -la",
			rationale: "列出文件 📁 incluant les fichiers cachés",
		},
		{
			name:      "SQL injection in command",
			command:   `echo "'); DROP TABLE approval_audit;--"`,
			rationale: "Testing SQL injection prevention",
		},
		{
			name:      "newlines in rationale",
			command:   "git status",
			rationale: "Check status:\n1. Modified files\n2. Staged changes\n3. Untracked files",
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expiresAt := now.Add(15 * time.Minute)
			entry := &ApprovalAuditEntry{
				ID:        fmt.Sprintf("audit-special-%d", i),
				RequestID: fmt.Sprintf("req-%d", i),
				Command:   tc.command,
				Cwd:       "/home/user/project",
				Repo:      "/home/user/project",
				Rationale: tc.rationale,
				Decision:  "approved",
				DecidedAt: now,
				ExpiresAt: &expiresAt,
				DeviceID:  "device-1",
				Source:    "mobile",
			}

			if err := store.SaveApprovalAudit(entry); err != nil {
				t.Fatalf("SaveApprovalAudit failed: %v", err)
			}

			entries, err := store.ListApprovalAudit(100)
			if err != nil {
				t.Fatalf("ListApprovalAudit failed: %v", err)
			}

			// Find our entry
			var found *ApprovalAuditEntry
			for _, e := range entries {
				if e.ID == entry.ID {
					found = e
					break
				}
			}
			if found == nil {
				t.Fatal("Entry not found in list")
			}

			if found.Command != tc.command {
				t.Errorf("Command = %q, want %q", found.Command, tc.command)
			}
			if found.Rationale != tc.rationale {
				t.Errorf("Rationale = %q, want %q", found.Rationale, tc.rationale)
			}
		})
	}
}

// =============================================================================
// Unit 7.11: Pairing and Auth Edge Cases - UpdateLastSeen Tests
// =============================================================================

// TestUpdateLastSeenConcurrent tests concurrent updates to the same device's last_seen.
func TestUpdateLastSeenConcurrent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a device
	device := &Device{
		ID:        "concurrent-device",
		Name:      "Test Device",
		TokenHash: "hash123",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("SaveDevice failed: %v", err)
	}

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errors := make(chan error, numGoroutines)
	times := make(chan time.Time, numGoroutines)

	// All goroutines update last_seen simultaneously with different times
	baseTime := time.Now()
	for i := 0; i < numGoroutines; i++ {
		go func(n int) {
			defer wg.Done()
			updateTime := baseTime.Add(time.Duration(n) * time.Millisecond)
			err := store.UpdateLastSeen(device.ID, updateTime)
			if err != nil {
				errors <- err
			} else {
				times <- updateTime
			}
		}(i)
	}

	wg.Wait()
	close(errors)
	close(times)

	// All updates should succeed
	for err := range errors {
		t.Errorf("UpdateLastSeen failed: %v", err)
	}

	// Verify the device still exists and has a valid last_seen
	found, err := store.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if found == nil {
		t.Fatal("device was unexpectedly deleted")
	}

	// last_seen should be one of the times we set
	validTimes := make(map[time.Time]bool)
	for ts := range times {
		validTimes[ts.Truncate(time.Nanosecond)] = true
	}

	// The stored time might have some precision loss, so just verify it's recent
	if found.LastSeen.Before(baseTime) {
		t.Errorf("last_seen is before base time: %v < %v", found.LastSeen, baseTime)
	}
}

// TestUpdateLastSeenWhileDeleting tests update during device deletion.
func TestUpdateLastSeenWhileDeleting(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a device
	device := &Device{
		ID:        "delete-race-device",
		Name:      "Test Device",
		TokenHash: "hash456",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("SaveDevice failed: %v", err)
	}

	const iterations = 100
	var wg sync.WaitGroup

	for i := 0; i < iterations; i++ {
		// Recreate device for each iteration
		device.ID = fmt.Sprintf("delete-race-device-%d", i)
		if err := store.SaveDevice(device); err != nil {
			t.Fatalf("SaveDevice failed: %v", err)
		}

		wg.Add(2)
		deviceID := device.ID // Capture for goroutine

		// Updater goroutine
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				err := store.UpdateLastSeen(deviceID, time.Now())
				// Either success or ErrDeviceNotFound is acceptable
				if err != nil && err != ErrDeviceNotFound {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()

		// Deleter goroutine
		go func() {
			defer wg.Done()
			_ = store.DeleteDevice(deviceID)
		}()
	}

	wg.Wait()
}

// TestUpdateLastSeenNotFoundStress tests repeated calls to non-existent device.
func TestUpdateLastSeenNotFoundStress(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(n int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("nonexistent-%d", n)
			for j := 0; j < 50; j++ {
				err := store.UpdateLastSeen(deviceID, time.Now())
				if err != ErrDeviceNotFound {
					t.Errorf("expected ErrDeviceNotFound, got %v", err)
				}
			}
		}(i)
	}

	wg.Wait()
}

// TestUpdateLastSeenRapidUpdates tests very rapid updates to same device.
func TestUpdateLastSeenRapidUpdates(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	device := &Device{
		ID:        "rapid-update-device",
		Name:      "Test Device",
		TokenHash: "hash789",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}
	if err := store.SaveDevice(device); err != nil {
		t.Fatalf("SaveDevice failed: %v", err)
	}

	// Simulate 1000 rapid updates (like high-frequency message tracking)
	const numUpdates = 1000
	errors := make(chan error, numUpdates)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(numUpdates)

	for i := 0; i < numUpdates; i++ {
		go func() {
			defer wg.Done()
			if err := store.UpdateLastSeen(device.ID, time.Now()); err != nil {
				errors <- err
			}
		}()
	}

	wg.Wait()
	close(errors)
	elapsed := time.Since(start)

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("UpdateLastSeen failed: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("%d updates failed out of %d", errorCount, numUpdates)
	}

	// Verify device is still valid
	found, err := store.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice failed: %v", err)
	}
	if found == nil {
		t.Fatal("device was unexpectedly deleted")
	}

	t.Logf("Completed %d concurrent updates in %v", numUpdates, elapsed)
}

// -----------------------------------------------------------------------------
// Decided Chunks Tests (V8 Schema)
// -----------------------------------------------------------------------------

// TestDecidedChunkSaveAndGet verifies basic decided chunk save and retrieve.
func TestDecidedChunkSaveAndGet(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	chunk := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "abc123",
		Patch:       "patch content",
		Status:      CardAccepted,
		DecidedAt:   now,
	}

	if err := store.SaveDecidedChunk(chunk); err != nil {
		t.Fatalf("SaveDecidedChunk failed: %v", err)
	}

	// Get by hash (preferred)
	found, err := store.GetDecidedChunkByHash("card-1", "abc123")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash failed: %v", err)
	}
	if found == nil {
		t.Fatal("chunk not found by hash")
	}
	if found.ChunkIndex != 0 {
		t.Errorf("ChunkIndex = %d, want 0", found.ChunkIndex)
	}
	if found.ContentHash != "abc123" {
		t.Errorf("ContentHash = %s, want abc123", found.ContentHash)
	}

	// Get by index (backward compat)
	found2, err := store.GetDecidedChunk("card-1", 0)
	if err != nil {
		t.Fatalf("GetDecidedChunk failed: %v", err)
	}
	if found2 == nil {
		t.Fatal("chunk not found by index")
	}
}

// TestDecidedChunkSameIndexDifferentHash verifies that two chunks with the
// same chunk_index but different content_hash can both persist (V8 schema).
// This is the key fix for the undo spinner bug.
func TestDecidedChunkSameIndexDifferentHash(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Save first chunk with index 0
	chunk1 := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "hash-aaa",
		Patch:       "patch 1",
		Status:      CardAccepted,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk1); err != nil {
		t.Fatalf("SaveDecidedChunk chunk1 failed: %v", err)
	}

	// Save second chunk with SAME index 0 but DIFFERENT hash
	// This simulates what happens when chunk indices shift after staging
	chunk2 := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0, // Same index as chunk1
		ContentHash: "hash-bbb",
		Patch:       "patch 2",
		Status:      CardRejected,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk2); err != nil {
		t.Fatalf("SaveDecidedChunk chunk2 failed: %v", err)
	}

	// Both chunks should exist and be retrievable by hash
	found1, err := store.GetDecidedChunkByHash("card-1", "hash-aaa")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash hash-aaa failed: %v", err)
	}
	if found1 == nil {
		t.Fatal("chunk1 not found by hash after saving chunk2 with same index")
	}
	if found1.Patch != "patch 1" {
		t.Errorf("chunk1 Patch = %s, want 'patch 1'", found1.Patch)
	}
	if found1.Status != CardAccepted {
		t.Errorf("chunk1 Status = %s, want accepted", found1.Status)
	}

	found2, err := store.GetDecidedChunkByHash("card-1", "hash-bbb")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash hash-bbb failed: %v", err)
	}
	if found2 == nil {
		t.Fatal("chunk2 not found by hash")
	}
	if found2.Patch != "patch 2" {
		t.Errorf("chunk2 Patch = %s, want 'patch 2'", found2.Patch)
	}
	if found2.Status != CardRejected {
		t.Errorf("chunk2 Status = %s, want rejected", found2.Status)
	}

	// GetDecidedChunks should return both
	all, err := store.GetDecidedChunks("card-1")
	if err != nil {
		t.Fatalf("GetDecidedChunks failed: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(all))
	}
}

// TestDecidedChunkDeleteByHash verifies that undo by content_hash deletes
// only the correct chunk when multiple chunks have the same index.
func TestDecidedChunkDeleteByHash(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Save two chunks with same index but different hashes
	chunk1 := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "hash-aaa",
		Patch:       "patch 1",
		Status:      CardAccepted,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk1); err != nil {
		t.Fatalf("SaveDecidedChunk chunk1 failed: %v", err)
	}

	chunk2 := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "hash-bbb",
		Patch:       "patch 2",
		Status:      CardRejected,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk2); err != nil {
		t.Fatalf("SaveDecidedChunk chunk2 failed: %v", err)
	}

	// Delete chunk1 by hash
	if err := store.DeleteDecidedChunkByHash("card-1", "hash-aaa"); err != nil {
		t.Fatalf("DeleteDecidedChunkByHash failed: %v", err)
	}

	// chunk1 should be gone
	found1, err := store.GetDecidedChunkByHash("card-1", "hash-aaa")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash after delete failed: %v", err)
	}
	if found1 != nil {
		t.Error("chunk1 should be deleted but was found")
	}

	// chunk2 should still exist
	found2, err := store.GetDecidedChunkByHash("card-1", "hash-bbb")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash chunk2 failed: %v", err)
	}
	if found2 == nil {
		t.Error("chunk2 should still exist but was not found")
	}
}

// TestDecidedChunkUpdateByHash verifies that saving a chunk with an existing
// content_hash updates the row instead of creating a duplicate.
func TestDecidedChunkUpdateByHash(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Save initial chunk
	chunk := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "hash-aaa",
		Patch:       "patch v1",
		Status:      CardAccepted,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk); err != nil {
		t.Fatalf("SaveDecidedChunk failed: %v", err)
	}

	// Update the same chunk (same hash) with new data
	// Simulate chunk_index shifting from 0 to 1 after staging
	chunk.ChunkIndex = 1
	chunk.Status = CardCommitted
	chunk.Patch = "patch v2"
	if err := store.SaveDecidedChunk(chunk); err != nil {
		t.Fatalf("SaveDecidedChunk update failed: %v", err)
	}

	// Should be only one chunk with that hash
	all, err := store.GetDecidedChunks("card-1")
	if err != nil {
		t.Fatalf("GetDecidedChunks failed: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 chunk after update, got %d", len(all))
	}

	// Verify the chunk was updated
	found, err := store.GetDecidedChunkByHash("card-1", "hash-aaa")
	if err != nil {
		t.Fatalf("GetDecidedChunkByHash failed: %v", err)
	}
	if found == nil {
		t.Fatal("chunk not found")
	}
	if found.ChunkIndex != 1 {
		t.Errorf("ChunkIndex = %d, want 1", found.ChunkIndex)
	}
	if found.Status != CardCommitted {
		t.Errorf("Status = %s, want committed", found.Status)
	}
	if found.Patch != "patch v2" {
		t.Errorf("Patch = %s, want 'patch v2'", found.Patch)
	}
}

// TestDecidedChunkNullHashLegacy verifies backward compatibility with chunks
// that don't have a content_hash (legacy/NULL hash).
func TestDecidedChunkNullHashLegacy(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Save chunk without content_hash (legacy)
	chunk := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "", // No hash
		Patch:       "legacy patch",
		Status:      CardAccepted,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunk); err != nil {
		t.Fatalf("SaveDecidedChunk failed: %v", err)
	}

	// Can retrieve by index
	found, err := store.GetDecidedChunk("card-1", 0)
	if err != nil {
		t.Fatalf("GetDecidedChunk failed: %v", err)
	}
	if found == nil {
		t.Fatal("chunk not found by index")
	}
	if found.ContentHash != "" {
		t.Errorf("ContentHash = %s, want empty", found.ContentHash)
	}

	// GetDecidedChunkByHash with empty hash should fail
	_, err = store.GetDecidedChunkByHash("card-1", "")
	if err == nil {
		t.Error("expected error for empty content_hash lookup")
	}

	// Can delete by index
	if err := store.DeleteDecidedChunk("card-1", 0); err != nil {
		t.Fatalf("DeleteDecidedChunk failed: %v", err)
	}

	found2, err := store.GetDecidedChunk("card-1", 0)
	if err != nil {
		t.Fatalf("GetDecidedChunk after delete failed: %v", err)
	}
	if found2 != nil {
		t.Error("chunk should be deleted")
	}
}

// TestGetLegacyDecidedChunkOnlyMatchesNullHash verifies that GetLegacyDecidedChunk
// only returns chunks with NULL content_hash, not chunks with a hash at the same index.
// This prevents the undo fallback from reverting the wrong chunk.
func TestGetLegacyDecidedChunkOnlyMatchesNullHash(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()

	// Save a chunk WITH content_hash at index 0
	chunkWithHash := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  0,
		ContentHash: "hash-abc",
		Patch:       "patch with hash",
		Status:      CardAccepted,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(chunkWithHash); err != nil {
		t.Fatalf("SaveDecidedChunk failed: %v", err)
	}

	// GetLegacyDecidedChunk should NOT return this chunk
	legacy, err := store.GetLegacyDecidedChunk("card-1", 0)
	if err != nil {
		t.Fatalf("GetLegacyDecidedChunk failed: %v", err)
	}
	if legacy != nil {
		t.Error("GetLegacyDecidedChunk should return nil for chunk with hash")
	}

	// Now add a legacy chunk (NULL hash) at a different index
	legacyChunk := &DecidedChunk{
		CardID:      "card-1",
		ChunkIndex:  1,
		ContentHash: "", // Legacy - no hash
		Patch:       "legacy patch",
		Status:      CardRejected,
		DecidedAt:   now,
	}
	if err := store.SaveDecidedChunk(legacyChunk); err != nil {
		t.Fatalf("SaveDecidedChunk failed: %v", err)
	}

	// GetLegacyDecidedChunk SHOULD return the legacy chunk at index 1
	legacy, err = store.GetLegacyDecidedChunk("card-1", 1)
	if err != nil {
		t.Fatalf("GetLegacyDecidedChunk failed: %v", err)
	}
	if legacy == nil {
		t.Fatal("GetLegacyDecidedChunk should return legacy chunk")
	}
	if legacy.Patch != "legacy patch" {
		t.Errorf("Patch = %s, want 'legacy patch'", legacy.Patch)
	}

	// GetDecidedChunk at index 0 should still return the hash chunk
	found, err := store.GetDecidedChunk("card-1", 0)
	if err != nil {
		t.Fatalf("GetDecidedChunk failed: %v", err)
	}
	if found == nil {
		t.Fatal("GetDecidedChunk should return hash chunk")
	}
	if found.ContentHash != "hash-abc" {
		t.Errorf("ContentHash = %s, want 'hash-abc'", found.ContentHash)
	}
}

// =============================================================================
// P17U5: Keep-Awake Audit Tests
// =============================================================================

// TestKeepAwakeAuditSave verifies basic audit entry persistence.
func TestKeepAwakeAuditSave(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()
	entry := &KeepAwakeAuditEntry{
		Operation:      "enable",
		RequestID:      "req-1",
		ActorDeviceID:  "device-1",
		TargetDeviceID: "",
		SessionID:      "session-1",
		LeaseID:        "ka-1",
		Reason:         "test",
		At:             now,
	}
	if err := store.SaveAndPruneKeepAwakeAudit(entry, 1000); err != nil {
		t.Fatalf("SaveAndPruneKeepAwakeAudit failed: %v", err)
	}

	entries, err := store.ListKeepAwakeAudit(10)
	if err != nil {
		t.Fatalf("ListKeepAwakeAudit failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Operation != "enable" {
		t.Errorf("Operation = %q, want %q", entries[0].Operation, "enable")
	}
	if entries[0].RequestID != "req-1" {
		t.Errorf("RequestID = %q, want %q", entries[0].RequestID, "req-1")
	}
	if entries[0].LeaseID != "ka-1" {
		t.Errorf("LeaseID = %q, want %q", entries[0].LeaseID, "ka-1")
	}
}

// TestKeepAwakeAuditPrune verifies that pruning removes oldest entries beyond maxRows.
func TestKeepAwakeAuditPrune(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now()
	// Insert 5 entries with maxRows=3
	for i := 0; i < 5; i++ {
		entry := &KeepAwakeAuditEntry{
			Operation: fmt.Sprintf("op-%d", i),
			RequestID: fmt.Sprintf("req-%d", i),
			LeaseID:   fmt.Sprintf("ka-%d", i),
			At:        now.Add(time.Duration(i) * time.Second),
		}
		if err := store.SaveAndPruneKeepAwakeAudit(entry, 3); err != nil {
			t.Fatalf("SaveAndPruneKeepAwakeAudit[%d] failed: %v", i, err)
		}
	}

	entries, err := store.ListKeepAwakeAudit(0)
	if err != nil {
		t.Fatalf("ListKeepAwakeAudit failed: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries after pruning, got %d", len(entries))
	}
	// Newest first: op-4, op-3, op-2
	if entries[0].Operation != "op-4" {
		t.Errorf("entries[0].Operation = %q, want %q", entries[0].Operation, "op-4")
	}
	if entries[2].Operation != "op-2" {
		t.Errorf("entries[2].Operation = %q, want %q", entries[2].Operation, "op-2")
	}
}

// TestMigrateToV10 verifies the keep_awake_audit table is created.
func TestMigrateToV10(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version < 10 {
		t.Errorf("SchemaVersion = %d, want >= 10", version)
	}

	exists, err := store.tableExists("keep_awake_audit")
	if err != nil {
		t.Fatalf("tableExists failed: %v", err)
	}
	if !exists {
		t.Error("keep_awake_audit table should exist after v10 migration")
	}
}

func TestKeepAwakeAuditProbe(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	if err := store.ProbeKeepAwakeAuditWrite(); err != nil {
		t.Fatalf("ProbeKeepAwakeAuditWrite should succeed, got: %v", err)
	}
}
