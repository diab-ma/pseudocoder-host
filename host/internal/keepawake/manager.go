package keepawake

import (
	"context"
	"sync"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

// Manager owns host-local keep-awake state and inhibitor lifecycle.
type Manager struct {
	mu sync.Mutex

	adapter Adapter
	now     func() time.Time

	status  Status
	handle  Handle
	closed  bool
	monitor uint64
}

// NewManager creates a keep-awake manager with the given adapter.
func NewManager(adapter Adapter, opts Options) *Manager {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return &Manager{
		adapter: adapter,
		now:     nowFn,
		status: Status{
			State:     StateOff,
			UpdatedAt: nowFn(),
		},
	}
}

// Snapshot returns a copy of current keep-awake state.
func (m *Manager) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// SetDesiredEnabled reconciles current runtime state to desired keep-awake value.
func (m *Manager) SetDesiredEnabled(ctx context.Context, enabled bool) Status {
	if enabled {
		return m.enable(ctx)
	}
	return m.disable(ctx)
}

// Close releases held resources and blocks until release attempt completes.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.status.DesiredEnabled = false

	h := m.handle
	m.handle = nil
	m.monitor++
	m.transitionLocked(StateOff, "", "")
	m.mu.Unlock()

	if h == nil {
		return nil
	}

	if err := h.Release(ctx); err != nil {
		m.mu.Lock()
		m.recordLifecycleErrorLocked(err.Error())
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) enable(ctx context.Context) Status {
	m.mu.Lock()
	if m.closed {
		defer m.mu.Unlock()
		return m.status
	}

	if m.status.DesiredEnabled && m.status.State == StateOn && m.handle != nil {
		// Idempotent fast path for active keep-awake. If the handle already exited,
		// mark integrity loss and proceed to reacquire in this call.
		select {
		case <-m.handle.Done():
			msg := "inhibitor exited unexpectedly"
			if err := m.handle.Err(); err != nil {
				msg = err.Error()
			}
			m.handle = nil
			m.monitor++
			m.transitionLocked(StateDegraded, DegradedReasonIntegrityLost, msg)
		default:
			defer m.mu.Unlock()
			return m.status
		}
	}

	m.status.DesiredEnabled = true
	m.transitionLocked(StatePending, "", "")
	m.mu.Unlock()

	h, err := m.adapter.Acquire(ctx)
	if err != nil {
		m.mu.Lock()
		reason := classifyAcquireReason(err)
		m.transitionLocked(StateDegraded, reason, err.Error())
		st := m.status
		m.mu.Unlock()
		return st
	}

	m.mu.Lock()
	if !m.status.DesiredEnabled || m.closed {
		m.mu.Unlock()
		_ = h.Release(context.Background())
		m.mu.Lock()
		m.transitionLocked(StateOff, "", "")
		st := m.status
		m.mu.Unlock()
		return st
	}

	m.handle = h
	m.monitor++
	gen := m.monitor
	m.transitionLocked(StateOn, "", "")
	st := m.status
	m.mu.Unlock()

	go m.watchHandle(h, gen)
	return st
}

func (m *Manager) disable(ctx context.Context) Status {
	m.mu.Lock()
	if m.closed {
		defer m.mu.Unlock()
		return m.status
	}

	m.status.DesiredEnabled = false
	h := m.handle
	m.handle = nil
	m.monitor++
	m.transitionLocked(StateOff, "", "")
	st := m.status
	m.mu.Unlock()

	if h == nil {
		return st
	}

	if err := h.Release(ctx); err != nil {
		m.mu.Lock()
		m.recordLifecycleErrorLocked(err.Error())
		st = m.status
		m.mu.Unlock()
		return st
	}

	return st
}

func (m *Manager) watchHandle(h Handle, gen uint64) {
	<-h.Done()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.handle != h || m.monitor != gen {
		return
	}
	if !m.status.DesiredEnabled || m.closed {
		return
	}

	msg := "inhibitor exited unexpectedly"
	if err := h.Err(); err != nil {
		msg = err.Error()
	}

	m.handle = nil
	m.transitionLocked(StateDegraded, DegradedReasonIntegrityLost, msg)
}

func (m *Manager) transitionLocked(next State, reason DegradedReason, lastErr string) {
	m.status.State = next
	m.status.Reason = reason
	m.status.LastError = lastErr
	m.status.UpdatedAt = m.now()
	m.status.Revision++
}

func (m *Manager) recordLifecycleErrorLocked(lastErr string) {
	// Explicit disable/close should settle at OFF while still preserving
	// diagnostics about release failures.
	m.status.Reason = ""
	m.status.LastError = lastErr
	m.status.UpdatedAt = m.now()
	m.status.Revision++
}

func classifyAcquireReason(err error) DegradedReason {
	code := hostErrors.GetCode(err)
	if code == hostErrors.CodeKeepAwakeUnsupportedEnvironment {
		return DegradedReasonUnsupportedEnvironment
	}
	return DegradedReasonAcquireFailed
}
