package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// currentSchemaVersion is the current database schema version.
// Increment this when making schema changes and add migration logic.
const currentSchemaVersion = 8

// initSchema creates the required tables if they don't exist.
// Uses IF NOT EXISTS to make the operation idempotent.
func (s *SQLiteStore) initSchema() error {
	// Schema version table tracks database migrations.
	// This allows future schema changes to be applied incrementally.
	const schemaVersionTable = `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
	`

	if _, err := s.db.Exec(schemaVersionTable); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Check current version
	var version int
	err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	// Apply migrations based on current version
	if version < 1 {
		if err := s.migrateToV1(); err != nil {
			return fmt.Errorf("migrate to v1: %w", err)
		}
	}

	if version < 2 {
		if err := s.migrateToV2(); err != nil {
			return fmt.Errorf("migrate to v2: %w", err)
		}
	}

	if version < 3 {
		if err := s.migrateToV3(); err != nil {
			return fmt.Errorf("migrate to v3: %w", err)
		}
	}

	if version < 4 {
		if err := s.migrateToV4(); err != nil {
			return fmt.Errorf("migrate to v4: %w", err)
		}
	}

	if version < 5 {
		if err := s.migrateToV5(); err != nil {
			return fmt.Errorf("migrate to v5: %w", err)
		}
	}

	if version < 6 {
		if err := s.migrateToV6(); err != nil {
			return fmt.Errorf("migrate to v6: %w", err)
		}
	}

	if version < 7 {
		if err := s.migrateToV7(); err != nil {
			return fmt.Errorf("migrate to v7: %w", err)
		}
	}

	if version < 8 {
		if err := s.migrateToV8(); err != nil {
			return fmt.Errorf("migrate to v8: %w", err)
		}
	}

	// Safety net: ensure per-chunk storage table exists even if prior migrations
	// were marked applied (e.g., older DBs with missing tables).
	if err := s.ensureChunksTable(); err != nil {
		return fmt.Errorf("ensure card_chunks table: %w", err)
	}

	return nil
}

// migrateToV1 creates the initial schema (review_cards table).
func (s *SQLiteStore) migrateToV1() error {
	log.Printf("storage: applying migration to schema version 1")

	// The schema stores all card fields as columns.
	// Timestamps are stored as RFC3339 strings for readability and portability.
	const cardsTable = `
		CREATE TABLE IF NOT EXISTS review_cards (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			file TEXT NOT NULL,
			diff TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			decided_at TEXT,
			comment TEXT NOT NULL DEFAULT ''
		);

		-- Index for efficient pending card queries (most common access pattern).
		CREATE INDEX IF NOT EXISTS idx_cards_status ON review_cards(status);
	`

	if _, err := s.db.Exec(cardsTable); err != nil {
		return fmt.Errorf("create review_cards table: %w", err)
	}

	// Record the migration
	_, err := s.db.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		1,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return nil
}

// migrateToV2 adds the devices table for pairing/authentication.
func (s *SQLiteStore) migrateToV2() error {
	log.Printf("storage: applying migration to schema version 2")

	// The devices table stores paired mobile devices.
	// Each device has a unique ID and a bcrypt-hashed token for authentication.
	// The token_hash is never exposed; only the raw token is sent to the device once.
	const devicesTable = `
		CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_seen TEXT NOT NULL
		);
	`

	if _, err := s.db.Exec(devicesTable); err != nil {
		return fmt.Errorf("create devices table: %w", err)
	}

	// Record the migration
	_, err := s.db.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		2,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return nil
}

// migrateToV3 adds the card_chunks table for per-chunk decision tracking.
// This enables accepting or rejecting individual chunks within a file card
// rather than the entire file at once.
func (s *SQLiteStore) migrateToV3() error {
	log.Printf("storage: applying migration to schema version 3")

	// The card_chunks table stores individual chunk statuses within a file card.
	// Each row represents one chunk from the parent card's diff.
	// The composite primary key (card_id, chunk_index) ensures uniqueness.
	const chunksTable = `
		CREATE TABLE IF NOT EXISTS card_chunks (
			card_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			decided_at TEXT,
			PRIMARY KEY (card_id, chunk_index),
			FOREIGN KEY (card_id) REFERENCES review_cards(id) ON DELETE CASCADE
		);

		-- Index for efficient pending chunk queries.
		CREATE INDEX IF NOT EXISTS idx_chunks_status ON card_chunks(status);

		-- Index for looking up chunks by card.
		CREATE INDEX IF NOT EXISTS idx_chunks_card ON card_chunks(card_id);
	`

	if _, err := s.db.Exec(chunksTable); err != nil {
		return fmt.Errorf("create card_chunks table: %w", err)
	}

	// Record the migration
	_, err := s.db.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		3,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return nil
}

// migrateToV4 adds the approval_audit table for CLI approval audit logging.
// This records all approval decisions for compliance and debugging purposes.
func (s *SQLiteStore) migrateToV4() error {
	log.Printf("storage: applying migration to schema version 4")

	// The approval_audit table stores a record of every approval decision.
	// This provides an audit trail for:
	// - Manual approvals/denials from mobile app
	// - Auto-approvals from temporary allow rules
	// - Timeouts (auto-deny)
	//
	// The source field indicates how the decision was made:
	// - "mobile": User approved/denied via mobile app
	// - "rule": Auto-approved by a temporary allow rule
	// - "timeout": Auto-denied due to request timeout
	const auditTable = `
		CREATE TABLE IF NOT EXISTS approval_audit (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			command TEXT NOT NULL,
			cwd TEXT NOT NULL,
			repo TEXT NOT NULL,
			rationale TEXT NOT NULL,
			decision TEXT NOT NULL,
			decided_at TEXT NOT NULL,
			expires_at TEXT,
			device_id TEXT,
			source TEXT NOT NULL DEFAULT 'mobile'
		);

		-- Index for efficient chronological queries (newest first).
		CREATE INDEX IF NOT EXISTS idx_audit_decided_at ON approval_audit(decided_at);
	`

	if _, err := s.db.Exec(auditTable); err != nil {
		return fmt.Errorf("create approval_audit table: %w", err)
	}

	// Record the migration
	_, err := s.db.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		4,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return nil
}

// migrateToV5 adds the sessions table for session history tracking.
// This enables storing terminal session metadata for mobile clients
// to view session history and switch between sessions.
func (s *SQLiteStore) migrateToV5() error {
	log.Printf("storage: applying migration to schema version 5")

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// The sessions table stores terminal session metadata.
	// Each row represents one host session with repo, branch, and status info.
	// Retention is enforced by deleting oldest sessions beyond maxSessions.
	const sessionsTable = `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			branch TEXT NOT NULL,
			started_at TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			last_commit TEXT,
			status TEXT NOT NULL DEFAULT 'running'
		);

		-- Index for efficient listing by started_at (newest first).
		CREATE INDEX IF NOT EXISTS idx_sessions_started_at ON sessions(started_at);
	`

	if _, err := tx.Exec(sessionsTable); err != nil {
		return fmt.Errorf("create sessions table: %w", err)
	}

	// Record the migration
	_, err = tx.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		5,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit()
}

// migrateToV6 adds the decided_cards and decided_chunks tables for undo support.
// These tables archive decided cards with their patches so undo operations can
// reverse-apply (for accepted cards) or forward-apply (for rejected cards).
func (s *SQLiteStore) migrateToV6() error {
	log.Printf("storage: applying migration to schema version 6")

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// The decided_cards table archives cards after a decision is made.
	// This enables undo functionality by preserving:
	// - The patch that was applied (for reverse-apply on undo)
	// - The original diff (for re-sending to mobile after undo)
	// - The commit hash (to track which cards were part of which commit)
	//
	// Cards transition: pending -> accepted/rejected -> committed (for accepts).
	// Rejected cards stay in rejected status until undo or cleanup.
	const decidedCardsTable = `
		CREATE TABLE IF NOT EXISTS decided_cards (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			file TEXT NOT NULL,
			patch TEXT NOT NULL,
			status TEXT NOT NULL,
			decided_at TEXT NOT NULL,
			commit_hash TEXT,
			committed_at TEXT,
			original_diff TEXT NOT NULL
		);

		-- Index for efficient status-based queries (list accepted, rejected, committed).
		CREATE INDEX IF NOT EXISTS idx_decided_cards_status ON decided_cards(status);

		-- Index for efficient commit association lookups.
		CREATE INDEX IF NOT EXISTS idx_decided_cards_commit ON decided_cards(commit_hash);

		-- Index for efficient file-based lookups (find decided cards for a file).
		CREATE INDEX IF NOT EXISTS idx_decided_cards_file ON decided_cards(file);
	`

	if _, err := tx.Exec(decidedCardsTable); err != nil {
		return fmt.Errorf("create decided_cards table: %w", err)
	}

	// The decided_chunks table archives per-chunk decisions.
	// Similar to decided_cards but for individual chunks within a file.
	// The card_id references decided_cards for the parent file card.
	const decidedChunksTable = `
		CREATE TABLE IF NOT EXISTS decided_chunks (
			card_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			patch TEXT NOT NULL,
			status TEXT NOT NULL,
			decided_at TEXT NOT NULL,
			commit_hash TEXT,
			committed_at TEXT,
			PRIMARY KEY (card_id, chunk_index)
		);

		-- Index for efficient status-based queries.
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_status ON decided_chunks(status);

		-- Index for efficient commit association lookups.
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_commit ON decided_chunks(commit_hash);
	`

	if _, err := tx.Exec(decidedChunksTable); err != nil {
		return fmt.Errorf("create decided_chunks table: %w", err)
	}

	// Record the migration
	_, err = tx.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		6,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit()
}

// ensureChunksTable recreates the card_chunks table if it is missing.
// This guards against older or corrupted databases where the schema version
// claims to be up to date but the per-chunk table was never created.
func (s *SQLiteStore) ensureChunksTable() error {
	exists, err := s.tableExists("card_chunks")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	log.Printf("storage: card_chunks table missing; recreating")

	const chunksTable = `
		CREATE TABLE IF NOT EXISTS card_chunks (
			card_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			decided_at TEXT,
			PRIMARY KEY (card_id, chunk_index),
			FOREIGN KEY (card_id) REFERENCES review_cards(id) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_chunks_status ON card_chunks(status);
		CREATE INDEX IF NOT EXISTS idx_chunks_card ON card_chunks(card_id);
	`

	if _, err := s.db.Exec(chunksTable); err != nil {
		return fmt.Errorf("create card_chunks table: %w", err)
	}

	return nil
}

// tableExists reports whether a table exists in the current database.
func (s *SQLiteStore) tableExists(name string) (bool, error) {
	var table string
	err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?",
		name,
	).Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return table == name, nil
}

// SchemaVersion returns the current database schema version.
// This is useful for diagnostics and testing.
func (s *SQLiteStore) SchemaVersion() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var version int
	err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get schema version: %w", err)
	}
	return version, nil
}

// migrateToV7 adds content_hash column to decided_chunks for stable chunk identity.
// Chunk indices can change when the diff is refreshed after staging; content_hash
// provides a stable identifier for undo operations.
func (s *SQLiteStore) migrateToV7() error {
	log.Printf("storage: applying migration to schema version 7")

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Add content_hash column to decided_chunks table.
	// This column stores the SHA256 hash (first 16 hex chars) of the chunk content.
	// Nullable because existing rows won't have this value until a new decision.
	const addColumn = `ALTER TABLE decided_chunks ADD COLUMN content_hash TEXT`
	if _, err := tx.Exec(addColumn); err != nil {
		// SQLite doesn't support IF NOT EXISTS for ALTER TABLE, so we check for error
		// If the column already exists, we continue
		if err.Error() != "duplicate column name: content_hash" {
			return fmt.Errorf("add content_hash column: %w", err)
		}
		log.Printf("storage: content_hash column already exists, skipping")
	}

	// Create unique index on (card_id, content_hash) for efficient hash-based lookups.
	// This index allows looking up chunks by their stable content hash instead of
	// the potentially shifting chunk_index.
	const createIndex = `
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_content_hash
		ON decided_chunks(card_id, content_hash)
	`
	if _, err := tx.Exec(createIndex); err != nil {
		return fmt.Errorf("create content_hash index: %w", err)
	}

	// Record the migration
	_, err = tx.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		7,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit()
}

// migrateToV8 restructures decided_chunks to use content_hash as primary identity.
// The old schema used (card_id, chunk_index) as PRIMARY KEY, which prevented storing
// two chunks with the same index but different content_hash (happens when indices
// shift after staging). The new schema uses auto-increment id with UNIQUE constraint
// on (card_id, content_hash).
func (s *SQLiteStore) migrateToV8() error {
	log.Printf("storage: applying migration to schema version 8")

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Create new table with auto-increment id as primary key.
	// This allows multiple rows per (card_id, chunk_index) when content_hash differs.
	const createNewTable = `
		CREATE TABLE IF NOT EXISTS decided_chunks_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			card_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			content_hash TEXT,
			patch TEXT NOT NULL,
			status TEXT NOT NULL,
			decided_at TEXT NOT NULL,
			commit_hash TEXT,
			committed_at TEXT
		)
	`
	if _, err := tx.Exec(createNewTable); err != nil {
		return fmt.Errorf("create decided_chunks_new table: %w", err)
	}

	// Copy existing data from old table
	const copyData = `
		INSERT INTO decided_chunks_new
			(card_id, chunk_index, content_hash, patch, status, decided_at, commit_hash, committed_at)
		SELECT card_id, chunk_index, content_hash, patch, status, decided_at, commit_hash, committed_at
		FROM decided_chunks
	`
	if _, err := tx.Exec(copyData); err != nil {
		return fmt.Errorf("copy data to decided_chunks_new: %w", err)
	}

	// Drop old table
	if _, err := tx.Exec("DROP TABLE decided_chunks"); err != nil {
		return fmt.Errorf("drop old decided_chunks table: %w", err)
	}

	// Rename new table to old name
	if _, err := tx.Exec("ALTER TABLE decided_chunks_new RENAME TO decided_chunks"); err != nil {
		return fmt.Errorf("rename decided_chunks_new: %w", err)
	}

	// Create unique index on (card_id, content_hash) for stable identity lookups.
	// This allows efficient lookup by content_hash and prevents duplicate hashes per card.
	const createHashIndex = `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_decided_chunks_card_hash
		ON decided_chunks(card_id, content_hash) WHERE content_hash IS NOT NULL
	`
	if _, err := tx.Exec(createHashIndex); err != nil {
		return fmt.Errorf("create unique hash index: %w", err)
	}

	// Create index on (card_id, chunk_index) for backward-compatible lookups
	// when content_hash is not available.
	const createIndexIndex = `
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_card_index
		ON decided_chunks(card_id, chunk_index)
	`
	if _, err := tx.Exec(createIndexIndex); err != nil {
		return fmt.Errorf("create chunk index index: %w", err)
	}

	// Recreate status and commit indexes
	const createStatusIndex = `
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_status
		ON decided_chunks(status)
	`
	if _, err := tx.Exec(createStatusIndex); err != nil {
		return fmt.Errorf("create status index: %w", err)
	}

	const createCommitIndex = `
		CREATE INDEX IF NOT EXISTS idx_decided_chunks_commit
		ON decided_chunks(commit_hash)
	`
	if _, err := tx.Exec(createCommitIndex); err != nil {
		return fmt.Errorf("create commit index: %w", err)
	}

	// Record the migration
	_, err = tx.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		8,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit()
}
