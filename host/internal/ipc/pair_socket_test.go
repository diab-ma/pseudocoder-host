package ipc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairSocketServer_StartStop(t *testing.T) {
	path := tempSocketPath(t)
	server := NewPairSocketServer(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil)

	if err := server.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("socket permissions = %o, want 0600", info.Mode().Perm())
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path should be removed, stat error: %v", err)
	}
}

func TestPairSocketServer_StaleSocketCleanup(t *testing.T) {
	path := tempSocketPath(t)

	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			t.Skip("socket file removed on close; stale cleanup not applicable")
		}
		t.Fatalf("expected stale socket file, got stat error: %v", err)
	}

	server := NewPairSocketServer(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil)

	if err := server.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer server.Stop()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket path is not a socket")
	}
}

func TestPairSocketServer_AlreadyRunning(t *testing.T) {
	path := tempSocketPath(t)

	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	server := NewPairSocketServer(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil)

	if err := server.Start(); err == nil {
		_ = server.Stop()
		t.Fatal("Start() expected error for already running socket")
	} else if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("Start() error = %v, want already in use", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket should remain, stat error: %v", err)
	}
}

func TestPairSocketServer_RequestFlow(t *testing.T) {
	path := tempSocketPath(t)
	server := NewPairSocketServer(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pair/generate" {
			http.NotFound(w, r)
			return
		}
		payload := map[string]string{"code": "123456"}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}), nil)

	if err := server.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer server.Stop()

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", path)
			},
		},
	}

	var resp *http.Response
	var err error
	for i := 0; i < 3; i++ {
		resp, err = client.Post("http://unix/pair/generate", "application/json", nil)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Post() error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(body))
	}
}

func tempSocketPath(t *testing.T) string {
	baseDir, err := os.MkdirTemp("/tmp", "pseudocoder-ipc-")
	if err != nil {
		baseDir = t.TempDir()
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(baseDir)
	})
	return filepath.Join(baseDir, "pair.sock")
}
