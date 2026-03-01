// Package keepawake manages host-local keep-awake runtime lifecycle.
//
// This package provides a host-internal state machine and OS adapter boundary
// for process-scoped sleep inhibitors. It intentionally does not expose any
// protocol handlers or mobile-facing message contracts in this unit.
package keepawake

import (
	"context"
	"time"
)

// State is the host-authoritative keep-awake runtime state.
type State string

const (
	// StateOff indicates no active keep-awake inhibitor is held.
	StateOff State = "OFF"
	// StatePending indicates an inhibitor acquire is in progress.
	StatePending State = "PENDING"
	// StateOn indicates keep-awake inhibitor is currently active.
	StateOn State = "ON"
	// StateDegraded indicates desired keep-awake could not be maintained.
	StateDegraded State = "DEGRADED"
)

// DegradedReason identifies why keep-awake entered degraded mode.
type DegradedReason string

const (
	// DegradedReasonUnsupportedEnvironment means the OS/runtime cannot provide
	// a supported process-scoped inhibitor path.
	DegradedReasonUnsupportedEnvironment DegradedReason = "unsupported_environment"
	// DegradedReasonAcquireFailed means inhibitor acquisition failed.
	DegradedReasonAcquireFailed DegradedReason = "acquire_failed"
	// DegradedReasonIntegrityLost means an acquired inhibitor exited/lost
	// integrity unexpectedly while desired state remained enabled.
	DegradedReasonIntegrityLost DegradedReason = "integrity_lost"
	// DegradedReasonReleaseFailed means inhibitor release failed.
	DegradedReasonReleaseFailed DegradedReason = "release_failed"
)

// Status is a snapshot of keep-awake runtime state.
type Status struct {
	// State is the current host runtime keep-awake state.
	State State
	// DesiredEnabled is the current desired keep-awake target.
	DesiredEnabled bool
	// Reason is set when state is DEGRADED.
	Reason DegradedReason
	// LastError stores the most recent lifecycle failure.
	LastError string
	// UpdatedAt is the timestamp of the latest state transition.
	UpdatedAt time.Time
	// Revision increments monotonically on state transitions.
	Revision int64
}

// Handle represents an acquired process-scoped inhibitor.
type Handle interface {
	// Done is closed when the inhibitor exits.
	Done() <-chan struct{}
	// Err returns the terminal inhibitor exit error after Done closes.
	Err() error
	// Release requests inhibitor shutdown.
	Release(ctx context.Context) error
}

// Adapter acquires OS-specific process-scoped inhibitors.
type Adapter interface {
	Acquire(ctx context.Context) (Handle, error)
}

// Options configures manager behavior.
type Options struct {
	// Now returns current time; defaults to time.Now.
	Now func() time.Time
}

// PowerSnapshot is a point-in-time reading of host power state.
// Nil pointer fields indicate unknown/unavailable readings.
type PowerSnapshot struct {
	OnBattery     *bool
	BatteryPercent *int
	ExternalPower *bool
}

// PowerProvider returns the current power state of the host.
type PowerProvider interface {
	Snapshot() PowerSnapshot
}
