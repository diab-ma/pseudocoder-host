package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSaveSession verifies that a session can be saved and retrieved.
func TestSaveSession(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create a test session
	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:        "session-123",
		Repo:      "/path/to/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    SessionStatusRunning,
	}

	// Save the session
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Retrieve the session
	got, err := store.GetSession("session-123")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}

	// Verify fields
	if got.ID != session.ID {
		t.Errorf("ID = %q, want %q", got.ID, session.ID)
	}
	if got.Repo != session.Repo {
		t.Errorf("Repo = %q, want %q", got.Repo, session.Repo)
	}
	if got.Branch != session.Branch {
		t.Errorf("Branch = %q, want %q", got.Branch, session.Branch)
	}
	if got.Status != session.Status {
		t.Errorf("Status = %q, want %q", got.Status, session.Status)
	}
	// Compare times with tolerance for parsing roundtrip
	if got.StartedAt.Sub(session.StartedAt).Abs() > time.Millisecond {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, session.StartedAt)
	}
	if got.LastSeen.Sub(session.LastSeen).Abs() > time.Millisecond {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, session.LastSeen)
	}
}

// TestSaveSessionWithLastCommit verifies that last_commit is stored correctly.
func TestSaveSessionWithLastCommit(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:         "session-456",
		Repo:       "/path/to/repo",
		Branch:     "feature",
		StartedAt:  now,
		LastSeen:   now,
		LastCommit: "abc123def",
		Status:     SessionStatusRunning,
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	got, err := store.GetSession("session-456")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if got.LastCommit != session.LastCommit {
		t.Errorf("LastCommit = %q, want %q", got.LastCommit, session.LastCommit)
	}
}

// TestSaveSessionUpdate verifies that saving an existing session updates it.
func TestSaveSessionUpdate(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:        "session-789",
		Repo:      "/path/to/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    SessionStatusRunning,
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Update the session
	session.LastSeen = now.Add(time.Hour)
	session.LastCommit = "newcommit123"
	session.Status = SessionStatusComplete

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession update failed: %v", err)
	}

	got, err := store.GetSession("session-789")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if got.Status != SessionStatusComplete {
		t.Errorf("Status = %q, want %q", got.Status, SessionStatusComplete)
	}
	if got.LastCommit != "newcommit123" {
		t.Errorf("LastCommit = %q, want %q", got.LastCommit, "newcommit123")
	}
}

// TestGetSessionNotFound verifies that GetSession returns nil for non-existent sessions.
func TestGetSessionNotFound(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	got, err := store.GetSession("nonexistent")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for non-existent session, got %+v", got)
	}
}

// TestListSessions verifies that sessions are listed in order (newest first).
func TestListSessions(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create sessions with different start times
	baseTime := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		session := &Session{
			ID:        "session-" + string(rune('A'+i)),
			Repo:      "/path/to/repo",
			Branch:    "main",
			StartedAt: baseTime.Add(time.Duration(i) * time.Minute),
			LastSeen:  baseTime.Add(time.Duration(i) * time.Minute),
			Status:    SessionStatusRunning,
		}
		if err := store.SaveSession(session); err != nil {
			t.Fatalf("SaveSession failed: %v", err)
		}
	}

	// List all sessions
	sessions, err := store.ListSessions(0)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	if len(sessions) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(sessions))
	}

	// Verify order (newest first)
	if sessions[0].ID != "session-E" {
		t.Errorf("first session = %q, want session-E (newest)", sessions[0].ID)
	}
	if sessions[4].ID != "session-A" {
		t.Errorf("last session = %q, want session-A (oldest)", sessions[4].ID)
	}
}

// TestListSessionsLimit verifies that the limit parameter works.
func TestListSessionsLimit(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create 10 sessions
	baseTime := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 10; i++ {
		session := &Session{
			ID:        "session-" + string(rune('0'+i)),
			Repo:      "/path/to/repo",
			Branch:    "main",
			StartedAt: baseTime.Add(time.Duration(i) * time.Minute),
			LastSeen:  baseTime.Add(time.Duration(i) * time.Minute),
			Status:    SessionStatusRunning,
		}
		if err := store.SaveSession(session); err != nil {
			t.Fatalf("SaveSession failed: %v", err)
		}
	}

	// List with limit
	sessions, err := store.ListSessions(3)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	if len(sessions) != 3 {
		t.Errorf("expected 3 sessions with limit, got %d", len(sessions))
	}

	// Should be the 3 newest sessions
	if sessions[0].ID != "session-9" {
		t.Errorf("first session = %q, want session-9", sessions[0].ID)
	}
}

// TestSessionRetention verifies that old sessions are deleted when limit is exceeded.
func TestSessionRetention(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Create more than maxSessions (20) sessions
	baseTime := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 25; i++ {
		session := &Session{
			ID:        "session-" + itoa(i),
			Repo:      "/path/to/repo",
			Branch:    "main",
			StartedAt: baseTime.Add(time.Duration(i) * time.Minute),
			LastSeen:  baseTime.Add(time.Duration(i) * time.Minute),
			Status:    SessionStatusRunning,
		}
		if err := store.SaveSession(session); err != nil {
			t.Fatalf("SaveSession failed: %v", err)
		}
	}

	// List all sessions
	sessions, err := store.ListSessions(0)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	// Should only have maxSessions (20) sessions
	if len(sessions) != maxSessions {
		t.Errorf("expected %d sessions after retention, got %d", maxSessions, len(sessions))
	}

	// The oldest sessions (0-4) should be deleted
	for _, s := range sessions {
		for i := 0; i < 5; i++ {
			if s.ID == "session-"+itoa(i) {
				t.Errorf("session-%d should have been deleted by retention", i)
			}
		}
	}
}

// TestUpdateSessionLastSeen verifies that last_seen can be updated.
func TestUpdateSessionLastSeen(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:        "session-lastseen",
		Repo:      "/path/to/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    SessionStatusRunning,
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Wait a bit and update last_seen
	time.Sleep(10 * time.Millisecond)

	if err := store.UpdateSessionLastSeen("session-lastseen"); err != nil {
		t.Fatalf("UpdateSessionLastSeen failed: %v", err)
	}

	got, err := store.GetSession("session-lastseen")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	// LastSeen should be newer than original
	if !got.LastSeen.After(now) {
		t.Errorf("LastSeen = %v, should be after %v", got.LastSeen, now)
	}
}

// TestUpdateSessionStatus verifies that status can be updated.
func TestUpdateSessionStatus(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:        "session-status",
		Repo:      "/path/to/repo",
		Branch:    "main",
		StartedAt: now,
		LastSeen:  now,
		Status:    SessionStatusRunning,
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Update status to complete
	if err := store.UpdateSessionStatus("session-status", SessionStatusComplete); err != nil {
		t.Fatalf("UpdateSessionStatus failed: %v", err)
	}

	got, err := store.GetSession("session-status")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if got.Status != SessionStatusComplete {
		t.Errorf("Status = %q, want %q", got.Status, SessionStatusComplete)
	}

	// Update status to error
	if err := store.UpdateSessionStatus("session-status", SessionStatusError); err != nil {
		t.Fatalf("UpdateSessionStatus failed: %v", err)
	}

	got, err = store.GetSession("session-status")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if got.Status != SessionStatusError {
		t.Errorf("Status = %q, want %q", got.Status, SessionStatusError)
	}
}

// TestSessionMigrationV5 verifies that the V5 migration creates the sessions table.
func TestSessionMigrationV5(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Verify schema version is 5
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("schema version = %d, want %d", version, currentSchemaVersion)
	}

	// Verify we can use session methods (table exists)
	sessions, err := store.ListSessions(0)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if sessions == nil {
		// Empty is fine, nil would indicate an error
		t.Log("sessions list is nil, but should be empty slice")
	}
}

// TestSessionPersistenceAcrossRestart verifies that sessions survive store close/reopen.
func TestSessionPersistenceAcrossRestart(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	// Create store and add a session
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore (first) failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:         "persist-session",
		Repo:       "/path/to/repo",
		Branch:     "feature-branch",
		StartedAt:  now,
		LastSeen:   now,
		LastCommit: "abc123",
		Status:     SessionStatusRunning,
	}
	if err := store1.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
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

	// Verify the session persisted
	got, err := store2.GetSession("persist-session")
	if err != nil {
		t.Fatalf("GetSession after restart failed: %v", err)
	}
	if got == nil {
		t.Fatal("session did not persist across restart")
	}

	// Verify all fields
	if got.ID != session.ID {
		t.Errorf("ID = %q, want %q", got.ID, session.ID)
	}
	if got.Repo != session.Repo {
		t.Errorf("Repo = %q, want %q", got.Repo, session.Repo)
	}
	if got.Branch != session.Branch {
		t.Errorf("Branch = %q, want %q", got.Branch, session.Branch)
	}
	if got.LastCommit != session.LastCommit {
		t.Errorf("LastCommit = %q, want %q", got.LastCommit, session.LastCommit)
	}
	if got.Status != session.Status {
		t.Errorf("Status = %q, want %q", got.Status, session.Status)
	}
	if got.StartedAt.Sub(session.StartedAt).Abs() > time.Millisecond {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, session.StartedAt)
	}
	if got.LastSeen.Sub(session.LastSeen).Abs() > time.Millisecond {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, session.LastSeen)
	}
}

// TestListSessionsEmpty verifies that ListSessions returns empty slice for empty database.
func TestListSessionsEmpty(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	sessions, err := store.ListSessions(0)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	// Should return empty slice, not nil
	if sessions == nil {
		t.Error("ListSessions returned nil, expected empty slice")
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

// TestSaveSessionNil verifies that saving nil session returns error.
func TestSaveSessionNil(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	err = store.SaveSession(nil)
	if err == nil {
		t.Error("expected error for nil session, got nil")
	}
}

// itoa is a simple integer to string conversion.
func itoa(i int) string {
	if i < 10 {
		return string('0' + rune(i))
	}
	return itoa(i/10) + string('0'+rune(i%10))
}
