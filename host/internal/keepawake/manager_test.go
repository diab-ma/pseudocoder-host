package keepawake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

type fakeAdapter struct {
	acquire func(context.Context) (Handle, error)
}

func (a *fakeAdapter) Acquire(ctx context.Context) (Handle, error) {
	if a.acquire == nil {
		return nil, nil
	}
	return a.acquire(ctx)
}

type fakeHandle struct {
	done    chan struct{}
	err     error
	release func(context.Context) error
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{done: make(chan struct{})}
}

func (h *fakeHandle) Done() <-chan struct{} { return h.done }
func (h *fakeHandle) Err() error            { return h.err }
func (h *fakeHandle) Release(ctx context.Context) error {
	if h.release != nil {
		return h.release(ctx)
	}
	select {
	case <-h.done:
	default:
		close(h.done)
	}
	return nil
}

func TestStateConstants(t *testing.T) {
	if StateOff != "OFF" || StatePending != "PENDING" || StateOn != "ON" || StateDegraded != "DEGRADED" {
		t.Fatal("state constants mismatch")
	}
}

func TestSetDesired_EnableSuccessToOn(t *testing.T) {
	h := newFakeHandle()
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})

	st := m.SetDesiredEnabled(context.Background(), true)
	if st.State != StateOn {
		t.Fatalf("state=%s want ON", st.State)
	}
	if !st.DesiredEnabled {
		t.Fatal("expected desired enabled")
	}
}

func TestSetDesired_AcquireFailureDegraded(t *testing.T) {
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return nil, errors.New("boom")
	}}, Options{})

	st := m.SetDesiredEnabled(context.Background(), true)
	if st.State != StateDegraded {
		t.Fatalf("state=%s want DEGRADED", st.State)
	}
	if st.Reason != DegradedReasonAcquireFailed {
		t.Fatalf("reason=%s want acquire_failed", st.Reason)
	}
}

func TestSetDesired_UnsupportedEnvironmentDegraded(t *testing.T) {
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return nil, hostErrors.New(hostErrors.CodeKeepAwakeUnsupportedEnvironment, "unsupported")
	}}, Options{})

	st := m.SetDesiredEnabled(context.Background(), true)
	if st.State != StateDegraded {
		t.Fatalf("state=%s want DEGRADED", st.State)
	}
	if st.Reason != DegradedReasonUnsupportedEnvironment {
		t.Fatalf("reason=%s want unsupported_environment", st.Reason)
	}
}

func TestSetDesired_DisableReleaseFailureStillOffWithError(t *testing.T) {
	h := newFakeHandle()
	h.release = func(context.Context) error { return errors.New("release failed") }
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)

	st := m.SetDesiredEnabled(context.Background(), false)
	if st.State != StateOff {
		t.Fatalf("state=%s want OFF", st.State)
	}
	if st.Reason != "" {
		t.Fatalf("reason=%s want empty", st.Reason)
	}
	if st.LastError == "" {
		t.Fatal("expected lifecycle error text")
	}
}

func TestIntegrityLossTransitionsToDegraded(t *testing.T) {
	h := newFakeHandle()
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)

	h.err = errors.New("exited")
	close(h.done)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := m.Snapshot()
		if st.State == StateDegraded {
			if st.Reason != DegradedReasonIntegrityLost {
				t.Fatalf("reason=%s want integrity_lost", st.Reason)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected degraded transition after unexpected exit")
}

func TestDisableDoesNotMisclassifyIntegrityLoss(t *testing.T) {
	h := newFakeHandle()
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)
	st := m.SetDesiredEnabled(context.Background(), false)
	if st.State != StateOff {
		t.Fatalf("state=%s want OFF", st.State)
	}
}

func TestCloseReleasesHandle(t *testing.T) {
	h := newFakeHandle()
	released := false
	h.release = func(context.Context) error {
		released = true
		close(h.done)
		return nil
	}
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)

	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !released {
		t.Fatal("expected release on close")
	}
}

func TestSetDesired_IdempotentEnableNoDuplicateAcquire(t *testing.T) {
	h := newFakeHandle()
	calls := 0
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		calls++
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)
	_ = m.SetDesiredEnabled(context.Background(), true)

	if calls != 1 {
		t.Fatalf("acquire calls=%d want 1", calls)
	}
}

func TestSetDesired_StaleOnHandleReacquires(t *testing.T) {
	h1 := newFakeHandle()
	h2 := newFakeHandle()
	calls := 0
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		calls++
		if calls == 1 {
			return h1, nil
		}
		return h2, nil
	}}, Options{})

	st := m.SetDesiredEnabled(context.Background(), true)
	if st.State != StateOn {
		t.Fatalf("state=%s want ON", st.State)
	}

	h1.err = errors.New("unexpected exit")
	close(h1.done)

	st = m.SetDesiredEnabled(context.Background(), true)
	if st.State != StateOn {
		t.Fatalf("state=%s want ON after reacquire", st.State)
	}
	if calls != 2 {
		t.Fatalf("acquire calls=%d want 2", calls)
	}
}

func TestCloseReleaseFailureKeepsOffAndReturnsError(t *testing.T) {
	h := newFakeHandle()
	h.release = func(context.Context) error { return errors.New("release failed") }
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})
	_ = m.SetDesiredEnabled(context.Background(), true)

	err := m.Close(context.Background())
	if err == nil {
		t.Fatal("expected close error")
	}
	st := m.Snapshot()
	if st.State != StateOff {
		t.Fatalf("state=%s want OFF", st.State)
	}
	if st.LastError == "" {
		t.Fatal("expected last error populated")
	}
}

func TestSetDesired_ConcurrentNoPanic(t *testing.T) {
	h := newFakeHandle()
	m := NewManager(&fakeAdapter{acquire: func(context.Context) (Handle, error) {
		return h, nil
	}}, Options{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = m.SetDesiredEnabled(context.Background(), true)
			} else {
				_ = m.SetDesiredEnabled(context.Background(), false)
			}
		}(i)
	}
	wg.Wait()
}
