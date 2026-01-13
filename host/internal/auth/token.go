package auth

import (
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TokenValidator validates device tokens for authentication.
// It looks up tokens in the device store and updates last-seen timestamps.
type TokenValidator struct {
	store   DeviceStore
	timeNow func() time.Time
}

// NewTokenValidator creates a new token validator.
func NewTokenValidator(store DeviceStore) *TokenValidator {
	return &TokenValidator{
		store:   store,
		timeNow: time.Now,
	}
}

// ValidateToken checks if the given token is valid.
// On success, returns the device and updates its last_seen timestamp.
// Returns ErrDeviceNotFound if the token is invalid.
//
// Note: This does a linear scan of all devices to find a matching hash.
// For a small number of devices (typical home use), this is acceptable.
// For larger deployments, consider caching or indexing.
func (tv *TokenValidator) ValidateToken(token string) (*Device, error) {
	devices, err := tv.store.ListDevices()
	if err != nil {
		return nil, err
	}

	// Try each device's hash against the provided token
	for _, device := range devices {
		// bcrypt.CompareHashAndPassword handles timing-safe comparison
		if err := bcrypt.CompareHashAndPassword([]byte(device.TokenHash), []byte(token)); err == nil {
			// Token matches this device
			log.Printf("auth: validated token for device %s (%s)", device.ID, device.Name)

			// Update last seen timestamp
			now := tv.timeNow()
			if err := tv.store.UpdateLastSeen(device.ID, now); err != nil {
				// Log but don't fail - validation succeeded
				log.Printf("auth: failed to update last_seen for device %s: %v", device.ID, err)
			}

			return device, nil
		}
	}

	log.Printf("auth: token validation failed (no matching device)")
	return nil, ErrDeviceNotFound
}

// ValidateDeviceID checks if a device ID exists.
// This is used for device management operations.
func (tv *TokenValidator) ValidateDeviceID(id string) (*Device, error) {
	device, err := tv.store.GetDevice(id)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, ErrDeviceNotFound
	}
	return device, nil
}
