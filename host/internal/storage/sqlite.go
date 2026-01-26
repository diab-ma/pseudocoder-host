package storage

import (
	"errors"
	"fmt"
	"log"
	"sync"

	// SQLite driver - imported for side effects (registers the driver).
	// Using modernc.org/sqlite which is a pure-Go implementation that
	// doesn't require CGO, making cross-compilation and testing easier.
	"database/sql"

	_ "modernc.org/sqlite"
)

// ErrCardNotFound is returned when an operation targets a non-existent card.
var ErrCardNotFound = errors.New("card not found")

// ErrAlreadyDecided is returned when trying to decide a card that is not pending.
// This ensures atomic decision processing - only one concurrent request can succeed.
var ErrAlreadyDecided = errors.New("card already has a decision")

// ErrChunkNotFound is returned when a chunk lookup fails.
var ErrChunkNotFound = errors.New("chunk not found")

// ErrDeviceNotFound is returned when a device lookup fails.
var ErrDeviceNotFound = errors.New("device not found")

// ErrDecidedCardNotFound is returned when a decided card lookup fails.
var ErrDecidedCardNotFound = errors.New("decided card not found")

// SQLiteStore implements CardStore using SQLite for persistence.
// It creates the database and tables on first use and supports
// concurrent access through internal locking.
type SQLiteStore struct {
	db *sql.DB      // Database connection handle.
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

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	log.Printf("storage: closing database")
	return s.db.Close()
}
