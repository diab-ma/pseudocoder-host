package storage

// approval_audit.go contains SQLiteStore methods for approval audit CRUD operations.
// Approval audit entries record CLI approval decisions for compliance and debugging.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// ApprovalAuditEntry represents an audit log entry for CLI approval decisions.
// Every approval request (approved, denied, or timed out) creates an audit entry.
type ApprovalAuditEntry struct {
	// ID is the unique identifier for this audit entry.
	ID string

	// RequestID is the approval request ID (UUID).
	RequestID string

	// Command is the command that was approved/denied.
	Command string

	// Cwd is the working directory where the command would run.
	Cwd string

	// Repo is the repository path for context.
	Repo string

	// Rationale is the explanation for why the command needs to run.
	Rationale string

	// Decision is "approved" or "denied".
	Decision string

	// DecidedAt is when the decision was made.
	DecidedAt time.Time

	// ExpiresAt is the temporary allow expiration time (nil if not a temp allow).
	ExpiresAt *time.Time

	// DeviceID is the ID of the device that made the decision (empty for timeout/rule).
	DeviceID string

	// Source indicates how the decision was made:
	// - "mobile": User approved/denied via mobile app
	// - "rule": Auto-approved by a temporary allow rule
	// - "timeout": Auto-denied due to request timeout
	Source string
}

// SaveApprovalAudit persists an approval audit entry to the database.
func (s *SQLiteStore) SaveApprovalAudit(entry *ApprovalAuditEntry) error {
	if entry == nil {
		return errors.New("audit entry cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving audit entry %s (request=%s, decision=%s, source=%s)",
		entry.ID, entry.RequestID, entry.Decision, entry.Source)

	decidedAt := entry.DecidedAt.Format(time.RFC3339Nano)
	var expiresAt sql.NullString
	if entry.ExpiresAt != nil {
		expiresAt = sql.NullString{String: entry.ExpiresAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT INTO approval_audit
			(id, request_id, command, cwd, repo, rationale, decision, decided_at, expires_at, device_id, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		entry.ID,
		entry.RequestID,
		entry.Command,
		entry.Cwd,
		entry.Repo,
		entry.Rationale,
		entry.Decision,
		decidedAt,
		expiresAt,
		entry.DeviceID,
		entry.Source,
	)
	if err != nil {
		return fmt.Errorf("save audit entry: %w", err)
	}

	return nil
}

// ListApprovalAudit returns audit entries in reverse chronological order (newest first).
// The limit parameter controls the maximum number of entries returned.
// Use limit <= 0 to return all entries.
func (s *SQLiteStore) ListApprovalAudit(limit int) ([]*ApprovalAuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []interface{}

	if limit > 0 {
		query = `
			SELECT id, request_id, command, cwd, repo, rationale, decision, decided_at, expires_at, device_id, source
			FROM approval_audit
			ORDER BY decided_at DESC
			LIMIT ?
		`
		args = append(args, limit)
	} else {
		query = `
			SELECT id, request_id, command, cwd, repo, rationale, decision, decided_at, expires_at, device_id, source
			FROM approval_audit
			ORDER BY decided_at DESC
		`
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit entries: %w", err)
	}
	defer rows.Close()

	var entries []*ApprovalAuditEntry
	for rows.Next() {
		entry, err := s.scanAuditRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit rows: %w", err)
	}

	log.Printf("storage: listed %d audit entries", len(entries))
	return entries, nil
}

// scanAuditRow scans a row from sql.Rows into an ApprovalAuditEntry.
func (s *SQLiteStore) scanAuditRow(rows *sql.Rows) (*ApprovalAuditEntry, error) {
	var (
		entry     ApprovalAuditEntry
		decidedAt string
		expiresAt sql.NullString
	)

	err := rows.Scan(
		&entry.ID,
		&entry.RequestID,
		&entry.Command,
		&entry.Cwd,
		&entry.Repo,
		&entry.Rationale,
		&entry.Decision,
		&decidedAt,
		&expiresAt,
		&entry.DeviceID,
		&entry.Source,
	)
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	entry.DecidedAt = t

	if expiresAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, expiresAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse expires_at: %w", err)
		}
		entry.ExpiresAt = &t
	}

	return &entry, nil
}
