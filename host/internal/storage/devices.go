package storage

// devices.go contains SQLiteStore methods for device CRUD operations.
// Devices are paired mobile devices used for authentication.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// Device represents a paired mobile device for auth package.
// This is a storage-level struct that matches the auth.Device interface.
type Device struct {
	ID        string
	Name      string
	TokenHash string
	CreatedAt time.Time
	LastSeen  time.Time
}

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
