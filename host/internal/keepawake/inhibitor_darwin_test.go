//go:build darwin

package keepawake

import (
	"context"
	"os/exec"
	"testing"
	"time"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

func TestDarwinAcquireStartsCaffeinateAndReleases(t *testing.T) {
	adapter := &darwinAdapter{
		hostPID: 1,
		execCmd: exec.Command,
	}

	h, err := adapter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire error: %v", err)
	}

	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Release(releaseCtx); err != nil {
		t.Fatalf("Release error: %v", err)
	}
}

func TestDarwinAcquireUnsupportedWhenMissingBinary(t *testing.T) {
	adapter := &darwinAdapter{
		hostPID: 1,
		execCmd: func(name string, args ...string) *exec.Cmd {
			return exec.Command("/nonexistent-binary-for-keepawake-test")
		},
	}

	_, err := adapter.Acquire(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if hostErrors.GetCode(err) != hostErrors.CodeKeepAwakeUnsupportedEnvironment {
		t.Fatalf("code=%s want %s", hostErrors.GetCode(err), hostErrors.CodeKeepAwakeUnsupportedEnvironment)
	}
}

func TestDarwinReleaseTimeoutEscalatesKill(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}

	h := &darwinHandle{
		cmd:  cmd,
		done: make(chan struct{}),
	}
	go h.wait()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	err := h.Release(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expected process exit after kill escalation")
	}
}
