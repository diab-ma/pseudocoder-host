package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Common errors for the pairing flow.
var (
	// ErrCodeExpired is returned when a pairing code has expired.
	ErrCodeExpired = errors.New("pairing code has expired")

	// ErrCodeInvalid is returned when the code doesn't match any active pairing.
	ErrCodeInvalid = errors.New("invalid pairing code")

	// ErrCodeUsed is returned when trying to use a code that was already redeemed.
	ErrCodeUsed = errors.New("pairing code already used")

	// ErrRateLimited is returned when too many pairing attempts are made.
	ErrRateLimited = errors.New("too many pairing attempts, try again later")

	// ErrDeviceNotFound is returned when a device lookup fails.
	ErrDeviceNotFound = errors.New("device not found")
)

// PairingConfig holds configuration for the pairing manager.
type PairingConfig struct {
	// CodeExpiry is how long a pairing code remains valid.
	// Default: 2 minutes.
	CodeExpiry time.Duration

	// MaxAttemptsPerMinute is the rate limit for pairing attempts.
	// Default: 5 attempts per minute.
	MaxAttemptsPerMinute int

	// DeviceStore is where paired devices are persisted.
	// Required.
	DeviceStore DeviceStore

	// TimeNow returns the current time. Useful for testing.
	// Default: time.Now.
	TimeNow func() time.Time
}

// PairingManager handles pairing code generation and validation.
// It enforces rate limits and code expiry to prevent brute force attacks.
type PairingManager struct {
	mu sync.Mutex

	// config holds the pairing configuration
	config PairingConfig

	// activeCode is the current pending pairing code.
	// Only one code can be active at a time.
	activeCode *pairingCode

	// attempts tracks recent pairing attempts for rate limiting.
	// Maps timestamp (truncated to second) to count.
	attempts map[int64]int
}

// pairingCode represents an active pairing code waiting to be redeemed.
type pairingCode struct {
	// code is the 6-digit code shown to the user.
	code string

	// expiresAt is when this code becomes invalid.
	expiresAt time.Time

	// used indicates the code has been redeemed.
	used bool
}

// NewPairingManager creates a new pairing manager with the given config.
func NewPairingManager(config PairingConfig) *PairingManager {
	// Apply defaults
	if config.CodeExpiry == 0 {
		config.CodeExpiry = 2 * time.Minute
	}
	if config.MaxAttemptsPerMinute == 0 {
		config.MaxAttemptsPerMinute = 5
	}
	if config.TimeNow == nil {
		config.TimeNow = time.Now
	}

	return &PairingManager{
		config:   config,
		attempts: make(map[int64]int),
	}
}

// GenerateCode creates a new 6-digit pairing code.
// Any previously active code is invalidated.
// Returns the code string to display to the user.
func (pm *PairingManager) GenerateCode() (string, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Generate a cryptographically random 6-digit code.
	// Using crypto/rand ensures the code is unpredictable.
	code, err := generateRandomCode(6)
	if err != nil {
		return "", fmt.Errorf("generate code: %w", err)
	}

	now := pm.config.TimeNow()
	pm.activeCode = &pairingCode{
		code:      code,
		expiresAt: now.Add(pm.config.CodeExpiry),
		used:      false,
	}

	log.Printf("auth: generated pairing code (expires at %s)", pm.activeCode.expiresAt.Format(time.RFC3339))

	return code, nil
}

// ValidateCode checks if the given code is valid and exchanges it for a device token.
// Returns the device ID and access token on success.
// The code is marked as used after successful validation (replay prevention).
//
// deviceName is a friendly name for the device (e.g., "iPhone 15 Pro").
func (pm *PairingManager) ValidateCode(code, deviceName string) (deviceID, token string, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := pm.config.TimeNow()

	// Check rate limit
	if err := pm.checkRateLimit(now); err != nil {
		return "", "", err
	}

	// Record this attempt
	pm.recordAttempt(now)

	// Validate the code
	if pm.activeCode == nil {
		log.Printf("auth: pairing attempt with no active code")
		return "", "", ErrCodeInvalid
	}

	if pm.activeCode.used {
		log.Printf("auth: pairing attempt with already-used code")
		return "", "", ErrCodeUsed
	}

	if now.After(pm.activeCode.expiresAt) {
		log.Printf("auth: pairing attempt with expired code")
		return "", "", ErrCodeExpired
	}

	if pm.activeCode.code != code {
		log.Printf("auth: pairing attempt with incorrect code")
		return "", "", ErrCodeInvalid
	}

	// Code is valid - mark as used immediately (before creating device)
	// This ensures replay prevention even if device creation fails.
	pm.activeCode.used = true

	// Generate device ID and token
	deviceID = uuid.New().String()
	token = generateSecureToken()

	// Hash the token for storage
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		return "", "", fmt.Errorf("hash token: %w", err)
	}

	// Create and store the device
	device := &Device{
		ID:        deviceID,
		Name:      deviceName,
		TokenHash: string(hash),
		CreatedAt: now,
		LastSeen:  now,
	}

	if err := pm.config.DeviceStore.SaveDevice(device); err != nil {
		return "", "", fmt.Errorf("save device: %w", err)
	}

	log.Printf("auth: paired device %s (%s)", deviceID, deviceName)

	return deviceID, token, nil
}

// HasActiveCode returns true if there's a non-expired, unused code.
func (pm *PairingManager) HasActiveCode() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.activeCode == nil {
		return false
	}

	now := pm.config.TimeNow()
	return !pm.activeCode.used && now.Before(pm.activeCode.expiresAt)
}

// GetCodeExpiry returns when the active code expires.
// Returns zero time if no active code exists.
func (pm *PairingManager) GetCodeExpiry() time.Time {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.activeCode == nil {
		return time.Time{}
	}
	return pm.activeCode.expiresAt
}

// checkRateLimit returns ErrRateLimited if too many attempts in the last minute.
// Must be called with pm.mu held.
func (pm *PairingManager) checkRateLimit(now time.Time) error {
	// Clean up old entries (older than 1 minute)
	cutoff := now.Add(-1 * time.Minute).Unix()
	for ts := range pm.attempts {
		if ts < cutoff {
			delete(pm.attempts, ts)
		}
	}

	// Count attempts in the last minute
	var count int
	for _, c := range pm.attempts {
		count += c
	}

	if count >= pm.config.MaxAttemptsPerMinute {
		log.Printf("auth: rate limit exceeded (%d attempts in last minute)", count)
		return ErrRateLimited
	}

	return nil
}

// recordAttempt records a pairing attempt for rate limiting.
// Must be called with pm.mu held.
func (pm *PairingManager) recordAttempt(now time.Time) {
	// Truncate to second for grouping
	key := now.Unix()
	pm.attempts[key]++
}

// generateRandomCode generates a random numeric code of the given length.
// Uses crypto/rand for security.
func generateRandomCode(length int) (string, error) {
	const digits = "0123456789"
	code := make([]byte, length)

	for i := range code {
		// Generate a random index into the digits string
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", err
		}
		code[i] = digits[n.Int64()]
	}

	return string(code), nil
}

// generateSecureToken generates a secure random token for device authentication.
// Returns a hex-encoded string suitable for use as a bearer token.
func generateSecureToken() string {
	// 32 bytes = 256 bits of entropy
	const tokenBytes = 32

	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		// This should never happen with crypto/rand
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}

	// Encode as hex for easy transport
	return fmt.Sprintf("%x", b)
}
