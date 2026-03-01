package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pseudocoder/host/internal/keepawake"
	"github.com/pseudocoder/host/internal/storage"
)

type fakeKeepAwakeManager struct {
	mu       sync.Mutex
	setCalls int
	closeErr error
	closeHit bool
}

func (m *fakeKeepAwakeManager) SetDesiredEnabled(ctx context.Context, enabled bool) keepawake.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCalls++
	return keepawake.Status{State: keepawake.StateOn, DesiredEnabled: enabled}
}

func (m *fakeKeepAwakeManager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeHit = true
	return m.closeErr
}

func TestRunHostStart_KeepAwakeTestEnableSeam(t *testing.T) {
	origFactory := newKeepAwakeController
	t.Cleanup(func() { newKeepAwakeController = origFactory })

	mgr := &fakeKeepAwakeManager{}
	newKeepAwakeController = func() keepAwakeController { return mgr }

	t.Setenv(keepAwakeTestEnableEnv, "1")

	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	code := runHostStart([]string{
		"--addr=invalid:address",
		"--require-auth=false",
		"--config=" + configPath,
		"--token-store=" + t.TempDir() + "/pseudocoder.db",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected startup failure with invalid addr")
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.setCalls == 0 {
		t.Fatal("expected keep-awake enable seam to call SetDesiredEnabled")
	}
	if !mgr.closeHit {
		t.Fatal("expected keep-awake manager Close on early return")
	}
}

func TestRunHostStart_KeepAwakeCloseWarningPath(t *testing.T) {
	origFactory := newKeepAwakeController
	t.Cleanup(func() { newKeepAwakeController = origFactory })

	mgr := &fakeKeepAwakeManager{closeErr: context.DeadlineExceeded}
	newKeepAwakeController = func() keepAwakeController { return mgr }

	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	_ = runHostStart([]string{
		"--addr=invalid:address",
		"--config=" + configPath,
		"--token-store=" + t.TempDir() + "/pseudocoder.db",
	}, &stdout, &stderr)

	if !bytes.Contains(stderr.Bytes(), []byte("keep-awake cleanup failed")) {
		t.Fatalf("expected keep-awake cleanup warning, stderr=%q", stderr.String())
	}
}

func TestRunHostStart_KeepAwakeAuditProbeFallbackWarning(t *testing.T) {
	origProbe := probeKeepAwakeAuditWrite
	t.Cleanup(func() { probeKeepAwakeAuditWrite = origProbe })
	probeKeepAwakeAuditWrite = func(store *storage.SQLiteStore) error {
		return errors.New("probe failed")
	}

	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	code := runHostStart([]string{
		"--addr=invalid:address",
		"--require-auth=false",
		"--config=" + configPath,
		"--token-store=" + t.TempDir() + "/pseudocoder.db",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected startup failure with invalid addr")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("keep-awake audit storage unavailable")) {
		t.Fatalf("expected keep-awake audit fallback warning, stderr=%q", stderr.String())
	}
}
