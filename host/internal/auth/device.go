// Package auth provides authentication and device management for the host.
// It handles pairing codes, device tokens, and access control for WebSocket connections.
//
// The pairing flow works as follows:
// 1. User runs `pseudocoder pair` to generate a 6-digit code (valid for 2 minutes)
// 2. Mobile app enters the code and POSTs to /pair endpoint
// 3. Host validates the code, generates a device token, and stores the device
// 4. Mobile app uses the token for all subsequent WebSocket connections
//
// Security considerations:
// - Pairing codes are short-lived (2 minute expiry)
// - Codes can only be used once (replay prevention)
// - Rate limiting prevents brute force attacks (max 5 attempts per minute)
// - Tokens are hashed before storage (bcrypt)
// - All WebSocket connections require valid token
package auth

import (
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

// Device is an alias for storage.Device to avoid import cycles.
// This allows the auth package to work with devices without duplicating the struct.
type Device = storage.Device

// DeviceStore defines the interface for persisting paired devices.
// This interface is implemented by storage.SQLiteStore.
// Implementations must be safe for concurrent access.
type DeviceStore interface {
	// SaveDevice persists a device to storage.
	// If a device with the same ID exists, it is updated.
	SaveDevice(device *Device) error

	// GetDevice retrieves a device by ID.
	// Returns nil, nil if the device does not exist.
	GetDevice(id string) (*Device, error)

	// GetDeviceByTokenHash finds a device by its token hash.
	// This is used to validate tokens during authentication.
	// Returns nil, nil if no matching device exists.
	GetDeviceByTokenHash(hash string) (*Device, error)

	// ListDevices returns all paired devices.
	ListDevices() ([]*Device, error)

	// DeleteDevice removes a device from storage.
	// Returns nil if the device does not exist (idempotent).
	DeleteDevice(id string) error

	// UpdateLastSeen updates the last_seen timestamp for a device.
	// Returns ErrDeviceNotFound if the device does not exist.
	UpdateLastSeen(id string, t time.Time) error
}
