//go:build darwin

package keepawake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

// NewDefaultAdapter returns the default macOS keep-awake adapter.
func NewDefaultAdapter() Adapter {
	return &darwinAdapter{
		hostPID: os.Getpid(),
		execCmd: exec.Command,
	}
}

type darwinAdapter struct {
	hostPID int
	execCmd func(name string, args ...string) *exec.Cmd
}

func (a *darwinAdapter) Acquire(ctx context.Context) (Handle, error) {
	if a.execCmd == nil {
		return nil, hostErrors.New(hostErrors.CodeKeepAwakeAcquireFailed, "keep-awake command runner is unavailable")
	}

	// Conservative mode: idle-only inhibit. Bind lifecycle to the host PID so
	// crash/restart exits this inhibitor process automatically.
	cmd := a.execCmd("caffeinate", "-i", "-w", strconv.Itoa(a.hostPID))
	if err := cmd.Start(); err != nil {
		var ex *exec.Error
		if errors.As(err, &ex) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, hostErrors.Wrap(hostErrors.CodeKeepAwakeUnsupportedEnvironment, "caffeinate is unavailable", err)
		}
		return nil, hostErrors.Wrap(hostErrors.CodeKeepAwakeAcquireFailed, "failed to start caffeinate", err)
	}

	h := &darwinHandle{
		cmd:  cmd,
		done: make(chan struct{}),
	}
	go h.wait()
	return h, nil
}

type darwinHandle struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	done     chan struct{}
	err      error
	released bool
	once     sync.Once
}

func (h *darwinHandle) wait() {
	err := h.cmd.Wait()

	h.mu.Lock()
	if h.released {
		err = nil
	}
	h.err = err
	h.mu.Unlock()

	close(h.done)
}

func (h *darwinHandle) Done() <-chan struct{} {
	return h.done
}

func (h *darwinHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *darwinHandle) Release(ctx context.Context) error {
	if h.cmd == nil || h.cmd.Process == nil {
		return nil
	}

	h.once.Do(func() {
		h.mu.Lock()
		h.released = true
		h.mu.Unlock()
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
	})

	select {
	case <-ctx.Done():
		// Escalate to SIGKILL on timeout to reduce orphan-process risk.
		_ = h.cmd.Process.Kill()
		select {
		case <-h.done:
		case <-time.After(200 * time.Millisecond):
		}
		return fmt.Errorf("release timed out waiting for caffeinate exit: %w", ctx.Err())
	case <-h.done:
		return nil
	}
}
