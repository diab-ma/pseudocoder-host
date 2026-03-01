package storage

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// MetricsStore defines the interface for rollout metrics collection.
// Implementations record crash events, terminal latency, and pairing attempts
// for use by the rollout auto-disable monitor.
type MetricsStore interface {
	RecordCrash(source, message string) error
	RecordLatency(source string, durationMs int64) error
	RecordPairingAttempt(deviceID string, success bool) error
	QueryCrashWindow(window time.Duration) (total int, err error)
	QueryLatencyP95Window(window time.Duration) (p95Ms int64, sampleCount int, err error)
	QueryPairingWindow(window time.Duration) (total, successes int, err error)
	RecordPromotionEvidence(phase int, stage string, passed bool, snapshot string) error
	Cleanup(retention time.Duration) (deleted int64, err error)
}

// metricsSchema is the SQL for creating the metrics tables.
// Applied via schema migration.
const metricsSchema = `
CREATE TABLE IF NOT EXISTS metrics_crash_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_crash_recorded_at ON metrics_crash_events(recorded_at);

CREATE TABLE IF NOT EXISTS metrics_latency_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source TEXT NOT NULL,
	duration_ms INTEGER NOT NULL,
	recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_latency_recorded_at ON metrics_latency_samples(recorded_at);

CREATE TABLE IF NOT EXISTS metrics_pairing_attempts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id TEXT NOT NULL DEFAULT '',
	success INTEGER NOT NULL DEFAULT 0,
	recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairing_recorded_at ON metrics_pairing_attempts(recorded_at);

CREATE TABLE IF NOT EXISTS promotion_evidence (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	phase INTEGER NOT NULL,
	stage TEXT NOT NULL,
	passed INTEGER NOT NULL DEFAULT 0,
	snapshot TEXT NOT NULL DEFAULT '',
	recorded_at TEXT NOT NULL
);
`

// SQLiteMetricsStore implements MetricsStore using SQLite.
type SQLiteMetricsStore struct {
	db *sql.DB
}

// NewSQLiteMetricsStore opens (or reuses) a SQLite database and ensures
// the metrics tables exist. Pass the same DB path as the main store to
// share the database file; use ":memory:" for testing.
func NewSQLiteMetricsStore(path string) (*SQLiteMetricsStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open metrics database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping metrics database: %w", err)
	}

	if _, err := db.Exec(metricsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create metrics tables: %w", err)
	}

	log.Printf("metrics: database ready at %s", path)
	return &SQLiteMetricsStore{db: db}, nil
}

// Close releases the database connection.
func (m *SQLiteMetricsStore) Close() error {
	return m.db.Close()
}

// RecordCrash inserts a crash/error event.
func (m *SQLiteMetricsStore) RecordCrash(source, message string) error {
	_, err := m.db.Exec(
		"INSERT INTO metrics_crash_events (source, message, recorded_at) VALUES (?, ?, ?)",
		source, message, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// RecordLatency inserts a terminal input latency sample.
func (m *SQLiteMetricsStore) RecordLatency(source string, durationMs int64) error {
	_, err := m.db.Exec(
		"INSERT INTO metrics_latency_samples (source, duration_ms, recorded_at) VALUES (?, ?, ?)",
		source, durationMs, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// RecordPairingAttempt inserts a pairing attempt record.
func (m *SQLiteMetricsStore) RecordPairingAttempt(deviceID string, success bool) error {
	s := 0
	if success {
		s = 1
	}
	_, err := m.db.Exec(
		"INSERT INTO metrics_pairing_attempts (device_id, success, recorded_at) VALUES (?, ?, ?)",
		deviceID, s, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// QueryCrashWindow returns the number of crash events in the given window.
func (m *SQLiteMetricsStore) QueryCrashWindow(window time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	var count int
	err := m.db.QueryRow(
		"SELECT COUNT(*) FROM metrics_crash_events WHERE recorded_at >= ?", cutoff,
	).Scan(&count)
	return count, err
}

// QueryLatencyP95Window returns the p95 latency and sample count in the given window.
// Uses NTILE to approximate p95 efficiently in SQLite.
func (m *SQLiteMetricsStore) QueryLatencyP95Window(window time.Duration) (int64, int, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)

	var sampleCount int
	err := m.db.QueryRow(
		"SELECT COUNT(*) FROM metrics_latency_samples WHERE recorded_at >= ?", cutoff,
	).Scan(&sampleCount)
	if err != nil {
		return 0, 0, err
	}
	if sampleCount == 0 {
		return 0, 0, nil
	}

	// p95: the value at index ceil(0.95 * count) when sorted ascending.
	offset := int(float64(sampleCount)*0.95) - 1
	if offset < 0 {
		offset = 0
	}

	var p95 int64
	err = m.db.QueryRow(
		"SELECT duration_ms FROM metrics_latency_samples WHERE recorded_at >= ? ORDER BY duration_ms ASC LIMIT 1 OFFSET ?",
		cutoff, offset,
	).Scan(&p95)
	if err != nil {
		return 0, sampleCount, err
	}

	return p95, sampleCount, nil
}

// QueryPairingWindow returns total and successful pairing attempts in the given window.
func (m *SQLiteMetricsStore) QueryPairingWindow(window time.Duration) (int, int, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	var total, successes int
	err := m.db.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(success), 0) FROM metrics_pairing_attempts WHERE recorded_at >= ?",
		cutoff,
	).Scan(&total, &successes)
	return total, successes, err
}

// RecordPromotionEvidence inserts a promotion evidence record.
func (m *SQLiteMetricsStore) RecordPromotionEvidence(phase int, stage string, passed bool, snapshot string) error {
	p := 0
	if passed {
		p = 1
	}
	_, err := m.db.Exec(
		"INSERT INTO promotion_evidence (phase, stage, passed, snapshot, recorded_at) VALUES (?, ?, ?, ?, ?)",
		phase, stage, p, snapshot, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// Cleanup deletes metrics rows older than the given retention duration.
// Returns the total number of rows deleted.
func (m *SQLiteMetricsStore) Cleanup(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	var total int64

	tables := []string{
		"metrics_crash_events",
		"metrics_latency_samples",
		"metrics_pairing_attempts",
	}

	for _, table := range tables {
		result, err := m.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE recorded_at < ?", table), cutoff,
		)
		if err != nil {
			return total, fmt.Errorf("cleanup %s: %w", table, err)
		}
		n, _ := result.RowsAffected()
		total += n
	}

	return total, nil
}
