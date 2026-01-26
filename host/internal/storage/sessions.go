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
			(id, repo, branch, started_at, last_seen, last_commit, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		session.ID,
		session.Repo,
		session.Branch,
		startedAt,
		lastSeen,
		lastCommit,
		string(session.Status),
	)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	// Enforce retention: delete oldest sessions beyond limit.
	// Uses subquery to select sessions to delete (all beyond first maxSessions by started_at).
	const cleanupQuery = `
		DELETE FROM sessions WHERE id IN (
			SELECT id FROM sessions ORDER BY started_at DESC LIMIT -1 OFFSET ?
		)
	`
	_, err = s.db.Exec(cleanupQuery, maxSessions)
	if err != nil {
		return fmt.Errorf("enforce session retention: %w", err)
	}

	return nil
}

// GetSession retrieves a session by ID.
// Returns nil, nil if the session does not exist.
func (s *SQLiteStore) GetSession(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, repo, branch, started_at, last_seen, last_commit, status
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
		SELECT id, repo, branch, started_at, last_seen, last_commit, status
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

// scanSessionRow scans a single row from sql.Row into a Session.
func (s *SQLiteStore) scanSessionRow(row *sql.Row) (*Session, error) {
	var (
		session    Session
		startedAt  string
		lastSeen   string
		lastCommit sql.NullString
		status     string
	)

	err := row.Scan(
		&session.ID,
		&session.Repo,
		&session.Branch,
		&startedAt,
		&lastSeen,
		&lastCommit,
		&status,
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
	)

	err := rows.Scan(
		&session.ID,
		&session.Repo,
		&session.Branch,
		&startedAt,
		&lastSeen,
		&lastCommit,
		&status,
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

	return &session, nil
}
