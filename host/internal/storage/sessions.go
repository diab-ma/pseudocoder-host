package storage

// sessions.go contains SQLiteStore methods for session CRUD operations.
// Sessions track terminal sessions for workflow history and context switching.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// maxSessions is the maximum number of sessions to retain.
// Older sessions are deleted when this limit is exceeded.
const maxSessions = 20

// SaveSession persists a session to the database.
// Uses INSERT OR REPLACE to handle both new sessions and updates.
// Enforces retention: keeps only the most recent maxSessions sessions.
func (s *SQLiteStore) SaveSession(session *Session) error {
	if session == nil {
		return errors.New("session cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving session %s (repo=%s, status=%s)", session.ID, session.Repo, session.Status)

	// Convert times to RFC3339 format for storage.
	startedAt := session.StartedAt.Format(time.RFC3339Nano)
	lastSeen := session.LastSeen.Format(time.RFC3339Nano)

	// Handle optional last_commit field.
	var lastCommit sql.NullString
	if session.LastCommit != "" {
		lastCommit = sql.NullString{String: session.LastCommit, Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO sessions
			(id, repo, branch, started_at, last_seen, last_commit, status, is_system, session_kind, agent_provider, chat_capabilities_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	isSystem := 0
	if session.IsSystem {
		isSystem = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save session transaction: %w", err)
	}

	_, err = tx.Exec(query,
		session.ID,
		session.Repo,
		session.Branch,
		startedAt,
		lastSeen,
		lastCommit,
		string(session.Status),
		isSystem,
		session.SessionKind,
		session.AgentProvider,
		session.ChatCapabilitiesJSON,
	)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("save session: %w", err)
	}

	// Enforce retention: delete oldest sessions beyond limit.
	// Uses subquery to select sessions to delete (all beyond first maxSessions by started_at).
	const cleanupTargetQuery = `
		SELECT id FROM sessions ORDER BY started_at DESC LIMIT -1 OFFSET ?
	`
	const cleanupStructuredQuery = `
		DELETE FROM structured_chat_sessions WHERE session_id IN (` + cleanupTargetQuery + `)
	`
	if _, err := tx.Exec(cleanupStructuredQuery, maxSessions); err != nil {
		tx.Rollback()
		return fmt.Errorf("enforce session retention structured snapshots: %w", err)
	}

	const cleanupQuery = `
		DELETE FROM sessions WHERE id IN (` + cleanupTargetQuery + `)
	`
	_, err = tx.Exec(cleanupQuery, maxSessions)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("enforce session retention: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save session transaction: %w", err)
	}

	return nil
}

// GetSession retrieves a session by ID.
// Returns nil, nil if the session does not exist.
func (s *SQLiteStore) GetSession(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, repo, branch, started_at, last_seen, last_commit, status, is_system, session_kind, agent_provider, chat_capabilities_json
		FROM sessions
		WHERE id = ?
	`

	row := s.db.QueryRow(query, id)
	session, err := s.scanSessionRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	return session, nil
}

// ListSessions returns recent sessions ordered by started_at (newest first).
// The limit parameter controls how many sessions to return (0 = default limit).
func (s *SQLiteStore) ListSessions(limit int) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = maxSessions
	}

	const query = `
		SELECT id, repo, branch, started_at, last_seen, last_commit, status, is_system, session_kind, agent_provider, chat_capabilities_json
		FROM sessions
		ORDER BY started_at DESC
		LIMIT ?
	`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]*Session, 0)
	for rows.Next() {
		session, err := s.scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session rows: %w", err)
	}

	log.Printf("storage: listed %d sessions", len(sessions))
	return sessions, nil
}

// UpdateSessionLastSeen updates the last_seen timestamp for a session.
func (s *SQLiteStore) UpdateSessionLastSeen(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `UPDATE sessions SET last_seen = ? WHERE id = ?`
	_, err := s.db.Exec(query, time.Now().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update session last_seen: %w", err)
	}
	return nil
}

// UpdateSessionStatus updates the status field for a session.
func (s *SQLiteStore) UpdateSessionStatus(id string, status SessionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `UPDATE sessions SET status = ? WHERE id = ?`
	_, err := s.db.Exec(query, string(status), id)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	return nil
}

// ClearArchivedSessions deletes archived session rows from storage.
// Archived sessions are those with status "complete" or "error".
// Returns the number of deleted rows.
func (s *SQLiteStore) ClearArchivedSessions() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin clear archived sessions transaction: %w", err)
	}

	const deleteStructuredSnapshots = `
		DELETE FROM structured_chat_sessions
		WHERE session_id IN (
			SELECT id FROM sessions
			WHERE status IN (?, ?)
			  AND is_system = 0
		)
	`
	if _, err := tx.Exec(
		deleteStructuredSnapshots,
		string(SessionStatusComplete),
		string(SessionStatusError),
	); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear archived structured snapshots: %w", err)
	}

	const deleteSessions = `
		DELETE FROM sessions
		WHERE status IN (?, ?)
		  AND is_system = 0
	`
	result, err := tx.Exec(
		deleteSessions,
		string(SessionStatusComplete),
		string(SessionStatusError),
	)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear archived sessions: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear archived sessions rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear archived sessions: %w", err)
	}

	return int(rowsAffected), nil
}

// ClearAllSessions deletes all non-system sessions from storage,
// regardless of status, including associated structured chat data.
// Returns the number of deleted rows.
func (s *SQLiteStore) ClearAllSessions() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin clear all sessions transaction: %w", err)
	}

	const deleteStructuredItems = `
		DELETE FROM structured_chat_items
		WHERE session_id IN (
			SELECT id FROM sessions WHERE is_system = 0
		)
	`
	if _, err := tx.Exec(deleteStructuredItems); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear all structured chat items: %w", err)
	}

	const deleteStructuredSnapshots = `
		DELETE FROM structured_chat_sessions
		WHERE session_id IN (
			SELECT id FROM sessions WHERE is_system = 0
		)
	`
	if _, err := tx.Exec(deleteStructuredSnapshots); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear all structured chat snapshots: %w", err)
	}

	const deleteSessions = `DELETE FROM sessions WHERE is_system = 0`
	result, err := tx.Exec(deleteSessions)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear all sessions: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("clear all sessions rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear all sessions: %w", err)
	}

	return int(rowsAffected), nil
}

// DeleteSession deletes a single session by ID, including associated
// structured chat snapshots and items. Returns an error if the session
// does not exist.
func (s *SQLiteStore) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete session transaction: %w", err)
	}

	// Delete associated structured chat data first (foreign-key-like cleanup).
	const deleteStructuredItems = `DELETE FROM structured_chat_items WHERE session_id = ?`
	if _, err := tx.Exec(deleteStructuredItems, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete structured chat items for session %s: %w", id, err)
	}

	const deleteStructuredSessions = `DELETE FROM structured_chat_sessions WHERE session_id = ?`
	if _, err := tx.Exec(deleteStructuredSessions, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete structured chat session for session %s: %w", id, err)
	}

	const deleteSession = `DELETE FROM sessions WHERE id = ?`
	result, err := tx.Exec(deleteSession, id)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("delete session %s: %w", id, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("delete session rows affected: %w", err)
	}
	if rowsAffected == 0 {
		tx.Rollback()
		return fmt.Errorf("session not found: %s", id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete session transaction: %w", err)
	}

	log.Printf("storage: deleted session %s", id)
	return nil
}

// scanSessionRow scans a single row from sql.Row into a Session.
func (s *SQLiteStore) scanSessionRow(row *sql.Row) (*Session, error) {
	var (
		session    Session
		startedAt  string
		lastSeen   string
		lastCommit sql.NullString
		status     string
		isSystem   int
	)

	err := row.Scan(
		&session.ID,
		&session.Repo,
		&session.Branch,
		&startedAt,
		&lastSeen,
		&lastCommit,
		&status,
		&isSystem,
		&session.SessionKind,
		&session.AgentProvider,
		&session.ChatCapabilitiesJSON,
	)
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return nil, fmt.Errorf("parse started_at: %w", err)
	}
	session.StartedAt = t

	t, err = time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("parse last_seen: %w", err)
	}
	session.LastSeen = t

	if lastCommit.Valid {
		session.LastCommit = lastCommit.String
	}

	session.Status = SessionStatus(status)
	session.IsSystem = isSystem == 1

	return &session, nil
}

// scanSessionRows scans a row from sql.Rows into a Session.
func (s *SQLiteStore) scanSessionRows(rows *sql.Rows) (*Session, error) {
	var (
		session    Session
		startedAt  string
		lastSeen   string
		lastCommit sql.NullString
		status     string
		isSystem   int
	)

	err := rows.Scan(
		&session.ID,
		&session.Repo,
		&session.Branch,
		&startedAt,
		&lastSeen,
		&lastCommit,
		&status,
		&isSystem,
		&session.SessionKind,
		&session.AgentProvider,
		&session.ChatCapabilitiesJSON,
	)
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return nil, fmt.Errorf("parse started_at: %w", err)
	}
	session.StartedAt = t

	t, err = time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("parse last_seen: %w", err)
	}
	session.LastSeen = t

	if lastCommit.Valid {
		session.LastCommit = lastCommit.String
	}

	session.Status = SessionStatus(status)
	session.IsSystem = isSystem == 1

	return &session, nil
}
