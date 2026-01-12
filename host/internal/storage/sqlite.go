package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	// SQLite driver - imported for side effects (registers the driver).
	// Using modernc.org/sqlite which is a pure-Go implementation that
	// doesn't require CGO, making cross-compilation and testing easier.
	_ "modernc.org/sqlite"
)

// currentSchemaVersion is the current database schema version.
// Increment this when making schema changes and add migration logic.
const currentSchemaVersion = 6

// maxSessions is the maximum number of sessions to retain.
// Older sessions are deleted when this limit is exceeded.
const maxSessions = 20

// ErrCardNotFound is returned when an operation targets a non-existent card.
var ErrCardNotFound = errors.New("card not found")

// ErrAlreadyDecided is returned when trying to decide a card that is not pending.
// This ensures atomic decision processing - only one concurrent request can succeed.
var ErrAlreadyDecided = errors.New("card already has a decision")

// SQLiteStore implements CardStore using SQLite for persistence.
// It creates the database and tables on first use and supports
// concurrent access through internal locking.
type SQLiteStore struct {
	db *sql.DB     // Database connection handle.
	mu sync.RWMutex // Guards all database operations for thread safety.
}

// NewSQLiteStore opens or creates a SQLite database at the given path.
// It initializes the schema if the tables don't exist.
// The path should be a file path like "/path/to/pseudocoder.db".
// Use ":memory:" for an in-memory database (useful for testing).
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	log.Printf("storage: opening database at %s", path)

	// Open database with foreign keys enabled for referential integrity.
	// The modernc.org/sqlite driver uses _pragma=foreign_keys(1) syntax.
	// We also set a busy_timeout of 5 seconds to handle concurrent access
	// from both the CLI and running host (e.g., during device revocation).
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Verify the connection is working.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	store := &SQLiteStore{db: db}

	// Create tables if they don't exist.
	if err := store.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	log.Printf("storage: database ready (schema version %d)", currentSchemaVersion)
	return store, nil
}

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

// SaveCard persists a review card to the database.
// Uses INSERT OR REPLACE to handle both new cards and updates.
func (s *SQLiteStore) SaveCard(card *ReviewCard) error {
	if card == nil {
		return errors.New("card cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving card %s (file=%s, status=%s)", card.ID, card.File, card.Status)

	// Convert times to RFC3339 format for storage.
	createdAt := card.CreatedAt.Format(time.RFC3339Nano)
	var decidedAt sql.NullString
	if card.DecidedAt != nil {
		decidedAt = sql.NullString{String: card.DecidedAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO review_cards
			(id, session_id, file, diff, status, created_at, decided_at, comment)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		card.ID,
		card.SessionID,
		card.File,
		card.Diff,
		string(card.Status),
		createdAt,
		decidedAt,
		card.Comment,
	)
	if err != nil {
		return fmt.Errorf("save card: %w", err)
	}

	return nil
}

// GetCard retrieves a card by ID.
// Returns nil, nil if the card does not exist.
func (s *SQLiteStore) GetCard(id string) (*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		WHERE id = ?
	`

	row := s.db.QueryRow(query, id)

	// Use a rowScanner adapter so we can reuse parseCardRow
	card, err := parseCardRow(rowAdapter{row})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get card: %w", err)
	}

	return card, nil
}

// ListPending returns all cards with status "pending".
// Results are ordered by creation time (oldest first) for FIFO processing.
func (s *SQLiteStore) ListPending() ([]*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		WHERE status = 'pending'
		ORDER BY created_at ASC
	`

	cards, err := s.queryCards(query)
	if err != nil {
		return nil, err
	}

	log.Printf("storage: listed %d pending cards", len(cards))
	return cards, nil
}

// ListAll returns all cards regardless of status.
// Results are ordered by creation time (oldest first).
func (s *SQLiteStore) ListAll() ([]*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		ORDER BY created_at ASC
	`

	cards, err := s.queryCards(query)
	if err != nil {
		return nil, err
	}

	log.Printf("storage: listed %d total cards", len(cards))
	return cards, nil
}

// RecordDecision updates a card's status and records the decision.
// Returns ErrCardNotFound if the card does not exist.
// Returns ErrAlreadyDecided if the card is not in pending status.
// This method is atomic - only one concurrent decision can succeed for a given card.
func (s *SQLiteStore) RecordDecision(decision *Decision) error {
	if decision == nil {
		return errors.New("decision cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: recording decision for card %s (status=%s)", decision.CardID, decision.Status)

	decidedAt := decision.Timestamp.Format(time.RFC3339Nano)

	// Atomic update: only updates if card exists AND is still pending.
	// This prevents race conditions where two concurrent decisions could
	// both check status, both apply git operations, then both update storage.
	const query = `
		UPDATE review_cards
		SET status = ?, decided_at = ?, comment = ?
		WHERE id = ? AND status = 'pending'
	`

	result, err := s.db.Exec(query, string(decision.Status), decidedAt, decision.Comment, decision.CardID)
	if err != nil {
		return fmt.Errorf("record decision: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		// No rows updated - either card doesn't exist or was already decided.
		// Check which case it is for a more specific error.
		var status string
		err := s.db.QueryRow("SELECT status FROM review_cards WHERE id = ?", decision.CardID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return fmt.Errorf("check card status: %w", err)
		}
		// Card exists but wasn't pending
		return ErrAlreadyDecided
	}

	return nil
}

// DeleteCard removes a card from storage.
// Returns nil if the card does not exist (idempotent delete).
func (s *SQLiteStore) DeleteCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting card %s", id)

	_, err := s.db.Exec("DELETE FROM review_cards WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete card: %w", err)
	}

	return nil
}

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	log.Printf("storage: closing database")
	return s.db.Close()
}

// queryCards executes a query and returns all matching cards.
func (s *SQLiteStore) queryCards(query string, args ...interface{}) ([]*ReviewCard, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query cards: %w", err)
	}
	defer rows.Close()

	var cards []*ReviewCard
	for rows.Next() {
		// Use rowsAdapter so we can reuse parseCardRow
		card, err := parseCardRow(rowsAdapter{rows})
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return cards, nil
}

// scanner is an interface that abstracts over sql.Row and sql.Rows.
// This allows us to use a single parseCardRow function for both cases,
// eliminating code duplication (DRY principle).
type scanner interface {
	Scan(dest ...interface{}) error
}

// rowAdapter wraps *sql.Row to implement the scanner interface.
type rowAdapter struct {
	row *sql.Row
}

func (r rowAdapter) Scan(dest ...interface{}) error {
	return r.row.Scan(dest...)
}

// rowsAdapter wraps *sql.Rows to implement the scanner interface.
type rowsAdapter struct {
	rows *sql.Rows
}

func (r rowsAdapter) Scan(dest ...interface{}) error {
	return r.rows.Scan(dest...)
}

// parseCardRow scans a database row into a ReviewCard.
// Works with both *sql.Row and *sql.Rows through the scanner interface.
func parseCardRow(s scanner) (*ReviewCard, error) {
	var (
		card      ReviewCard
		status    string
		createdAt string
		decidedAt sql.NullString
	)

	err := s.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Diff,
		&status,
		&createdAt,
		&decidedAt,
		&card.Comment,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	// Parse timestamps from RFC3339 format.
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	card.CreatedAt = t

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		card.DecidedAt = &t
	}

	return &card, nil
}

// -----------------------------------------------------------------------------
// Device Storage Methods
// -----------------------------------------------------------------------------

// Device represents a paired mobile device for auth package.
// This is a storage-level struct that matches the auth.Device interface.
type Device struct {
	ID        string
	Name      string
	TokenHash string
	CreatedAt time.Time
	LastSeen  time.Time
}

// ErrDeviceNotFound is returned when a device lookup fails.
var ErrDeviceNotFound = errors.New("device not found")

// SaveDevice persists a device to the database.
// Uses INSERT OR REPLACE to handle both new devices and updates.
func (s *SQLiteStore) SaveDevice(device *Device) error {
	if device == nil {
		return errors.New("device cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving device %s (%s)", device.ID, device.Name)

	const query = `
		INSERT OR REPLACE INTO devices
			(id, name, token_hash, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		device.ID,
		device.Name,
		device.TokenHash,
		device.CreatedAt.Format(time.RFC3339Nano),
		device.LastSeen.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save device: %w", err)
	}

	return nil
}

// GetDevice retrieves a device by ID.
// Returns nil, nil if the device does not exist.
func (s *SQLiteStore) GetDevice(id string) (*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, name, token_hash, created_at, last_seen
		FROM devices
		WHERE id = ?
	`

	device, err := s.scanDevice(s.db.QueryRow(query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}

	return device, nil
}

// GetDeviceByTokenHash finds a device by its token hash.
// This is used to validate tokens during authentication.
// Returns nil, nil if no matching device exists.
func (s *SQLiteStore) GetDeviceByTokenHash(hash string) (*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, name, token_hash, created_at, last_seen
		FROM devices
		WHERE token_hash = ?
	`

	device, err := s.scanDevice(s.db.QueryRow(query, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device by token hash: %w", err)
	}

	return device, nil
}

// ListDevices returns all paired devices.
func (s *SQLiteStore) ListDevices() ([]*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, name, token_hash, created_at, last_seen
		FROM devices
		ORDER BY created_at ASC
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()

	var devices []*Device
	for rows.Next() {
		device, err := s.scanDeviceRows(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device rows: %w", err)
	}

	log.Printf("storage: listed %d devices", len(devices))
	return devices, nil
}

// DeleteDevice removes a device from storage.
// Returns nil if the device does not exist (idempotent delete).
func (s *SQLiteStore) DeleteDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting device %s", id)

	_, err := s.db.Exec("DELETE FROM devices WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}

	return nil
}

// UpdateLastSeen updates the last_seen timestamp for a device.
// Returns ErrDeviceNotFound if the device does not exist.
func (s *SQLiteStore) UpdateLastSeen(id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `UPDATE devices SET last_seen = ? WHERE id = ?`

	result, err := s.db.Exec(query, t.Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update last_seen: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrDeviceNotFound
	}

	return nil
}

// scanDevice scans a single row into a Device.
func (s *SQLiteStore) scanDevice(row *sql.Row) (*Device, error) {
	var (
		device    Device
		createdAt string
		lastSeen  string
	)

	err := row.Scan(
		&device.ID,
		&device.Name,
		&device.TokenHash,
		&createdAt,
		&lastSeen,
	)
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	device.CreatedAt = t

	t, err = time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("parse last_seen: %w", err)
	}
	device.LastSeen = t

	return &device, nil
}

// scanDeviceRows scans a row from sql.Rows into a Device.
func (s *SQLiteStore) scanDeviceRows(rows *sql.Rows) (*Device, error) {
	var (
		device    Device
		createdAt string
		lastSeen  string
	)

	err := rows.Scan(
		&device.ID,
		&device.Name,
		&device.TokenHash,
		&createdAt,
		&lastSeen,
	)
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	device.CreatedAt = t

	t, err = time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("parse last_seen: %w", err)
	}
	device.LastSeen = t

	return &device, nil
}

// -----------------------------------------------------------------------------
// Chunk Storage Methods (ChunkStore interface)
// -----------------------------------------------------------------------------

// ErrChunkNotFound is returned when a chunk lookup fails.
var ErrChunkNotFound = errors.New("chunk not found")

// SaveChunks persists all chunks for a card.
// This replaces any existing chunks for the card (delete + insert).
// Use this when saving or updating a file card with its parsed chunks.
func (s *SQLiteStore) SaveChunks(cardID string, chunks []*ChunkStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use a transaction to ensure atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() // No-op if committed

	// Delete existing chunks for this card
	if _, err := tx.Exec("DELETE FROM card_chunks WHERE card_id = ?", cardID); err != nil {
		return fmt.Errorf("delete existing chunks: %w", err)
	}

	// Insert new chunks
	const insertQuery = `
		INSERT INTO card_chunks (card_id, chunk_index, content, status, decided_at)
		VALUES (?, ?, ?, ?, ?)
	`
	for _, h := range chunks {
		var decidedAt sql.NullString
		if h.DecidedAt != nil {
			decidedAt = sql.NullString{String: h.DecidedAt.Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := tx.Exec(insertQuery, cardID, h.ChunkIndex, h.Content, string(h.Status), decidedAt); err != nil {
			return fmt.Errorf("insert chunk %d: %w", h.ChunkIndex, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	log.Printf("storage: saved %d chunks for card %s", len(chunks), cardID)
	return nil
}

// GetChunks retrieves all chunks for a card, ordered by chunk_index.
// Returns empty slice if no chunks exist.
func (s *SQLiteStore) GetChunks(cardID string) ([]*ChunkStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, content, status, decided_at
		FROM card_chunks
		WHERE card_id = ?
		ORDER BY chunk_index ASC
	`

	rows, err := s.db.Query(query, cardID)
	if err != nil {
		return nil, fmt.Errorf("query chunks: %w", err)
	}
	defer rows.Close()

	var chunks []*ChunkStatus
	for rows.Next() {
		h, err := s.scanChunkRows(rows)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunk rows: %w", err)
	}

	return chunks, nil
}

// GetChunk retrieves a specific chunk by card ID and index.
// Returns nil, nil if the chunk does not exist.
func (s *SQLiteStore) GetChunk(cardID string, chunkIndex int) (*ChunkStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, content, status, decided_at
		FROM card_chunks
		WHERE card_id = ? AND chunk_index = ?
	`

	row := s.db.QueryRow(query, cardID, chunkIndex)
	h, err := s.scanChunkRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get chunk: %w", err)
	}

	return h, nil
}

// RecordChunkDecision updates a chunk's status.
// Returns ErrChunkNotFound if the chunk does not exist.
// Returns ErrAlreadyDecided if the chunk is not pending.
func (s *SQLiteStore) RecordChunkDecision(decision *ChunkDecision) error {
	if decision == nil {
		return errors.New("decision cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: recording chunk decision for card %s chunk %d (status=%s)",
		decision.CardID, decision.ChunkIndex, decision.Status)

	decidedAt := decision.Timestamp.Format(time.RFC3339Nano)

	// Atomic update: only updates if chunk exists AND is still pending
	const query = `
		UPDATE card_chunks
		SET status = ?, decided_at = ?
		WHERE card_id = ? AND chunk_index = ? AND status = 'pending'
	`

	result, err := s.db.Exec(query, string(decision.Status), decidedAt, decision.CardID, decision.ChunkIndex)
	if err != nil {
		return fmt.Errorf("record chunk decision: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		// No rows updated - check which error case
		var status string
		err := s.db.QueryRow(
			"SELECT status FROM card_chunks WHERE card_id = ? AND chunk_index = ?",
			decision.CardID, decision.ChunkIndex,
		).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChunkNotFound
		}
		if err != nil {
			return fmt.Errorf("check chunk status: %w", err)
		}
		// Chunk exists but wasn't pending
		return ErrAlreadyDecided
	}

	return nil
}

// DeleteChunks removes all chunks for a card.
// Returns nil if no chunks exist (idempotent).
func (s *SQLiteStore) DeleteChunks(cardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting chunks for card %s", cardID)

	_, err := s.db.Exec("DELETE FROM card_chunks WHERE card_id = ?", cardID)
	if err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}

	return nil
}

// CountPendingChunks returns the number of pending chunks for a card.
func (s *SQLiteStore) CountPendingChunks(cardID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM card_chunks WHERE card_id = ? AND status = 'pending'",
		cardID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending chunks: %w", err)
	}

	return count, nil
}

// scanChunkRow scans a single row into a ChunkStatus.
func (s *SQLiteStore) scanChunkRow(row *sql.Row) (*ChunkStatus, error) {
	var (
		h         ChunkStatus
		status    string
		decidedAt sql.NullString
	)

	err := row.Scan(
		&h.CardID,
		&h.ChunkIndex,
		&h.Content,
		&status,
		&decidedAt,
	)
	if err != nil {
		return nil, err
	}

	h.Status = CardStatus(status)

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		h.DecidedAt = &t
	}

	return &h, nil
}

// scanChunkRows scans a row from sql.Rows into a ChunkStatus.
func (s *SQLiteStore) scanChunkRows(rows *sql.Rows) (*ChunkStatus, error) {
	var (
		h         ChunkStatus
		status    string
		decidedAt sql.NullString
	)

	err := rows.Scan(
		&h.CardID,
		&h.ChunkIndex,
		&h.Content,
		&status,
		&decidedAt,
	)
	if err != nil {
		return nil, err
	}

	h.Status = CardStatus(status)

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		h.DecidedAt = &t
	}

	return &h, nil
}

// -----------------------------------------------------------------------------
// Approval Audit Storage Methods
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// Session Storage Methods (SessionStore interface)
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// Decided Card Storage Methods (DecidedCardStore interface)
// -----------------------------------------------------------------------------

// ErrDecidedCardNotFound is returned when a decided card lookup fails.
var ErrDecidedCardNotFound = errors.New("decided card not found")

// SaveDecidedCard archives a card after a decision is made.
// Uses INSERT OR REPLACE to handle updates (e.g., marking as committed).
func (s *SQLiteStore) SaveDecidedCard(card *DecidedCard) error {
	if card == nil {
		return errors.New("decided card cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving decided card %s (file=%s, status=%s)", card.ID, card.File, card.Status)

	decidedAt := card.DecidedAt.Format(time.RFC3339Nano)
	var commitHash sql.NullString
	if card.CommitHash != "" {
		commitHash = sql.NullString{String: card.CommitHash, Valid: true}
	}
	var committedAt sql.NullString
	if card.CommittedAt != nil {
		committedAt = sql.NullString{String: card.CommittedAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO decided_cards
			(id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		card.ID,
		card.SessionID,
		card.File,
		card.Patch,
		string(card.Status),
		decidedAt,
		commitHash,
		committedAt,
		card.OriginalDiff,
	)
	if err != nil {
		return fmt.Errorf("save decided card: %w", err)
	}

	return nil
}

// GetDecidedCard retrieves an archived card by ID.
// Returns nil, nil if the card does not exist.
func (s *SQLiteStore) GetDecidedCard(id string) (*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE id = ?
	`

	row := s.db.QueryRow(query, id)
	card, err := s.scanDecidedCardRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get decided card: %w", err)
	}

	return card, nil
}

// ListDecidedByStatus returns all decided cards with the given status.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListDecidedByStatus(status CardStatus) ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE status = ?
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query, string(status))
	if err != nil {
		return nil, fmt.Errorf("query decided cards by status: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d decided cards with status %s", len(cards), status)
	return cards, nil
}

// ListAllDecidedCards returns all decided cards regardless of status.
// This is used for commit association to find all cards with accepted chunks.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListAllDecidedCards() ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query all decided cards: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d total decided cards", len(cards))
	return cards, nil
}

// ListDecidedByFile returns all decided cards for a given file path.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListDecidedByFile(file string) ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE file = ?
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query, file)
	if err != nil {
		return nil, fmt.Errorf("query decided cards by file: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d decided cards for file %s", len(cards), file)
	return cards, nil
}

// MarkCardsCommitted updates accepted cards to committed status.
// This is called after a successful git commit to associate cards with their commit hash.
func (s *SQLiteStore) MarkCardsCommitted(cardIDs []string, commitHash string) error {
	if len(cardIDs) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: marking %d cards as committed (hash=%s)", len(cardIDs), commitHash)

	committedAt := time.Now().Format(time.RFC3339Nano)

	// Build query with placeholders for all card IDs
	placeholders := make([]string, len(cardIDs))
	args := make([]interface{}, len(cardIDs)+3)
	args[0] = string(CardCommitted)
	args[1] = commitHash
	args[2] = committedAt
	for i, id := range cardIDs {
		placeholders[i] = "?"
		args[i+3] = id
	}

	query := fmt.Sprintf(`
		UPDATE decided_cards
		SET status = ?, commit_hash = ?, committed_at = ?
		WHERE status = 'accepted' AND id IN (%s)
	`, strings.Join(placeholders, ", "))

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark cards committed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("storage: marked %d cards as committed", rowsAffected)

	return nil
}

// DeleteDecidedCard removes an archived card.
// This is called after a successful undo operation.
func (s *SQLiteStore) DeleteDecidedCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting decided card %s", id)

	_, err := s.db.Exec("DELETE FROM decided_cards WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete decided card: %w", err)
	}

	return nil
}

// SaveDecidedChunk archives a chunk after a per-chunk decision.
// Uses INSERT OR REPLACE to handle updates (e.g., marking as committed).
func (s *SQLiteStore) SaveDecidedChunk(chunk *DecidedChunk) error {
	if chunk == nil {
		return errors.New("decided chunk cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving decided chunk %s:%d (status=%s)", chunk.CardID, chunk.ChunkIndex, chunk.Status)

	decidedAt := chunk.DecidedAt.Format(time.RFC3339Nano)
	var commitHash sql.NullString
	if chunk.CommitHash != "" {
		commitHash = sql.NullString{String: chunk.CommitHash, Valid: true}
	}
	var committedAt sql.NullString
	if chunk.CommittedAt != nil {
		committedAt = sql.NullString{String: chunk.CommittedAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO decided_chunks
			(card_id, chunk_index, patch, status, decided_at, commit_hash, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		chunk.CardID,
		chunk.ChunkIndex,
		chunk.Patch,
		string(chunk.Status),
		decidedAt,
		commitHash,
		committedAt,
	)
	if err != nil {
		return fmt.Errorf("save decided chunk: %w", err)
	}

	return nil
}

// GetDecidedChunk retrieves an archived chunk by card ID and index.
// Returns nil, nil if the chunk does not exist.
func (s *SQLiteStore) GetDecidedChunk(cardID string, chunkIndex int) (*DecidedChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, patch, status, decided_at, commit_hash, committed_at
		FROM decided_chunks
		WHERE card_id = ? AND chunk_index = ?
	`

	row := s.db.QueryRow(query, cardID, chunkIndex)
	chunk, err := s.scanDecidedChunkRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get decided chunk: %w", err)
	}

	return chunk, nil
}

// GetDecidedChunks retrieves all archived chunks for a card.
// Results are ordered by chunk_index.
func (s *SQLiteStore) GetDecidedChunks(cardID string) ([]*DecidedChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, patch, status, decided_at, commit_hash, committed_at
		FROM decided_chunks
		WHERE card_id = ?
		ORDER BY chunk_index ASC
	`

	rows, err := s.db.Query(query, cardID)
	if err != nil {
		return nil, fmt.Errorf("query decided chunks: %w", err)
	}
	defer rows.Close()

	var chunks []*DecidedChunk
	for rows.Next() {
		chunk, err := s.scanDecidedChunkRows(rows)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided chunk rows: %w", err)
	}

	return chunks, nil
}

// MarkChunksCommitted updates accepted chunks to committed status.
func (s *SQLiteStore) MarkChunksCommitted(cardID string, chunkIndexes []int, commitHash string) error {
	if len(chunkIndexes) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: marking %d chunks as committed for card %s (hash=%s)", len(chunkIndexes), cardID, commitHash)

	committedAt := time.Now().Format(time.RFC3339Nano)

	// Build query with placeholders for all chunk indexes
	placeholders := make([]string, len(chunkIndexes))
	args := make([]interface{}, len(chunkIndexes)+4)
	args[0] = string(CardCommitted)
	args[1] = commitHash
	args[2] = committedAt
	args[3] = cardID
	for i, idx := range chunkIndexes {
		placeholders[i] = "?"
		args[i+4] = idx
	}

	query := fmt.Sprintf(`
		UPDATE decided_chunks
		SET status = ?, commit_hash = ?, committed_at = ?
		WHERE card_id = ? AND status = 'accepted' AND chunk_index IN (%s)
	`, strings.Join(placeholders, ", "))

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark chunks committed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("storage: marked %d chunks as committed", rowsAffected)

	return nil
}

// DeleteDecidedChunk removes a single archived chunk.
// This is used when undoing a single chunk decision.
func (s *SQLiteStore) DeleteDecidedChunk(cardID string, chunkIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting decided chunk %s:%d", cardID, chunkIndex)

	_, err := s.db.Exec("DELETE FROM decided_chunks WHERE card_id = ? AND chunk_index = ?", cardID, chunkIndex)
	if err != nil {
		return fmt.Errorf("delete decided chunk: %w", err)
	}

	return nil
}

// DeleteDecidedChunks removes all archived chunks for a card.
func (s *SQLiteStore) DeleteDecidedChunks(cardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting decided chunks for card %s", cardID)

	_, err := s.db.Exec("DELETE FROM decided_chunks WHERE card_id = ?", cardID)
	if err != nil {
		return fmt.Errorf("delete decided chunks: %w", err)
	}

	return nil
}

// scanDecidedCardRow scans a single row into a DecidedCard.
func (s *SQLiteStore) scanDecidedCardRow(row *sql.Row) (*DecidedCard, error) {
	var (
		card        DecidedCard
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := row.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
		&card.OriginalDiff,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	card.DecidedAt = t

	if commitHash.Valid {
		card.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		card.CommittedAt = &t
	}

	return &card, nil
}

// scanDecidedCardRows scans a row from sql.Rows into a DecidedCard.
func (s *SQLiteStore) scanDecidedCardRows(rows *sql.Rows) (*DecidedCard, error) {
	var (
		card        DecidedCard
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := rows.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
		&card.OriginalDiff,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	card.DecidedAt = t

	if commitHash.Valid {
		card.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		card.CommittedAt = &t
	}

	return &card, nil
}

// scanDecidedChunkRow scans a single row into a DecidedChunk.
func (s *SQLiteStore) scanDecidedChunkRow(row *sql.Row) (*DecidedChunk, error) {
	var (
		chunk       DecidedChunk
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := row.Scan(
		&chunk.CardID,
		&chunk.ChunkIndex,
		&chunk.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
	)
	if err != nil {
		return nil, err
	}

	chunk.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	chunk.DecidedAt = t

	if commitHash.Valid {
		chunk.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		chunk.CommittedAt = &t
	}

	return &chunk, nil
}

// scanDecidedChunkRows scans a row from sql.Rows into a DecidedChunk.
func (s *SQLiteStore) scanDecidedChunkRows(rows *sql.Rows) (*DecidedChunk, error) {
	var (
		chunk       DecidedChunk
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := rows.Scan(
		&chunk.CardID,
		&chunk.ChunkIndex,
		&chunk.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
	)
	if err != nil {
		return nil, err
	}

	chunk.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	chunk.DecidedAt = t

	if commitHash.Valid {
		chunk.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		chunk.CommittedAt = &t
	}

	return &chunk, nil
}
