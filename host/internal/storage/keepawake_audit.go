package storage

// keepawake_audit.go contains SQLiteStore methods for keep-awake audit CRUD operations.
// Keep-awake audit entries record keep-awake mutations for compliance and debugging.

import (
	"fmt"
	"log"
	"time"
)

// KeepAwakeAuditEntry represents a durable keep-awake audit record.
type KeepAwakeAuditEntry struct {
	ID             int64
	Operation      string
	RequestID      string
	ActorDeviceID  string
	TargetDeviceID string
	SessionID      string
	LeaseID        string
	Reason         string
	At             time.Time
}

// SaveAndPruneKeepAwakeAudit inserts an audit entry and prunes oldest beyond maxRows in a single tx.
func (s *SQLiteStore) SaveAndPruneKeepAwakeAudit(entry *KeepAwakeAuditEntry, maxRows int) error {
	if entry == nil {
		return fmt.Errorf("keep-awake audit entry cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	at := entry.At.Format(time.RFC3339Nano)

	const insertQuery = `
		INSERT INTO keep_awake_audit
			(operation, request_id, actor_device_id, target_device_id, session_id, lease_id, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = tx.Exec(insertQuery,
		entry.Operation,
		entry.RequestID,
		entry.ActorDeviceID,
		entry.TargetDeviceID,
		entry.SessionID,
		entry.LeaseID,
		entry.Reason,
		at,
	)
	if err != nil {
		return fmt.Errorf("insert keep-awake audit: %w", err)
	}

	if maxRows > 0 {
		const pruneQuery = `
			DELETE FROM keep_awake_audit
			WHERE id NOT IN (SELECT id FROM keep_awake_audit ORDER BY id DESC LIMIT ?)
		`
		if _, err := tx.Exec(pruneQuery, maxRows); err != nil {
			return fmt.Errorf("prune keep-awake audit: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit keep-awake audit: %w", err)
	}

	log.Printf("storage: saved keep-awake audit op=%s request_id=%s", entry.Operation, entry.RequestID)
	return nil
}

// ListKeepAwakeAudit returns audit entries in reverse chronological order (newest first).
func (s *SQLiteStore) ListKeepAwakeAudit(limit int) ([]*KeepAwakeAuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []interface{}

	if limit > 0 {
		query = `
			SELECT id, operation, request_id, actor_device_id, target_device_id, session_id, lease_id, reason, at
			FROM keep_awake_audit
			ORDER BY id DESC
			LIMIT ?
		`
		args = append(args, limit)
	} else {
		query = `
			SELECT id, operation, request_id, actor_device_id, target_device_id, session_id, lease_id, reason, at
			FROM keep_awake_audit
			ORDER BY id DESC
		`
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query keep-awake audit: %w", err)
	}
	defer rows.Close()

	var entries []*KeepAwakeAuditEntry
	for rows.Next() {
		var (
			entry KeepAwakeAuditEntry
			atStr string
		)
		err := rows.Scan(
			&entry.ID,
			&entry.Operation,
			&entry.RequestID,
			&entry.ActorDeviceID,
			&entry.TargetDeviceID,
			&entry.SessionID,
			&entry.LeaseID,
			&entry.Reason,
			&atStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan keep-awake audit row: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, atStr)
		if err != nil {
			return nil, fmt.Errorf("parse keep-awake audit at: %w", err)
		}
		entry.At = t
		entries = append(entries, &entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keep-awake audit rows: %w", err)
	}

	return entries, nil
}

// ProbeKeepAwakeAuditWrite verifies keep-awake audit storage is writable.
// It performs an insert+delete within one transaction to ensure:
// 1) the table exists after migration, and
// 2) writes are currently permitted by the storage backend.
func (s *SQLiteStore) ProbeKeepAwakeAuditWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339Nano)
	res, err := tx.Exec(
		`INSERT INTO keep_awake_audit
			(operation, request_id, actor_device_id, target_device_id, session_id, lease_id, reason, at)
		  VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"startup_probe",
		"",
		"system",
		"",
		"",
		"",
		"startup_writability_check",
		now,
	)
	if err != nil {
		return fmt.Errorf("insert probe row: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM keep_awake_audit WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete probe row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit probe: %w", err)
	}
	return nil
}
