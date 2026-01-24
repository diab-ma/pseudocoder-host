// Package ipc provides local IPC helpers for the host.
// It is used to expose sensitive handlers over a Unix socket with
// restrictive filesystem permissions.
package ipc

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PairSocketServer hosts the pairing code generator over a Unix socket.
// It ensures the socket directory and file permissions are locked down.
type PairSocketServer struct {
	// path is the filesystem location of the Unix socket.
	path string

	// handler is the HTTP handler served over the socket.
	handler http.Handler

	// server is the HTTP server serving the handler.
	server *http.Server

	// listener is the Unix socket listener.
	listener net.Listener

	// logger emits background errors from the server.
	logger *log.Logger

	// mu guards start/stop operations.
	mu sync.Mutex
}

// NewPairSocketServer creates a new pairing IPC server for the given path.
// If logger is nil, logs are discarded.
func NewPairSocketServer(path string, handler http.Handler, logger *log.Logger) *PairSocketServer {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &PairSocketServer{
		path:    path,
		handler: handler,
		logger:  logger,
	}
}

// Start begins listening on the configured Unix socket.
// It removes stale socket files, but fails if another process is active.
func (s *PairSocketServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		return fmt.Errorf("pairing IPC already started")
	}
	if s.path == "" {
		return fmt.Errorf("pairing IPC socket path is empty")
	}
	if err := validatePairSocketPath(s.path); err != nil {
		return err
	}
	if s.handler == nil {
		return fmt.Errorf("pairing IPC handler is nil")
	}

	if err := s.prepareSocketDir(); err != nil {
		return err
	}

	if err := s.ensureSocketAvailable(); err != nil {
		return err
	}

	listener, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("failed to listen on pairing IPC socket: %w", err)
	}

	if err := os.Chmod(s.path, 0600); err != nil {
		listener.Close()
		_ = os.Remove(s.path)
		return fmt.Errorf("failed to set pairing socket permissions: %w", err)
	}

	s.listener = listener
	s.server = &http.Server{Handler: s.handler}

	go func() {
		err := s.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Printf("pairing IPC server stopped: %v", err)
		}
	}()

	return nil
}

// Stop shuts down the IPC server and removes the socket file.
func (s *PairSocketServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var stopErr error
	if s.server != nil {
		if err := s.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			stopErr = fmt.Errorf("failed to stop pairing IPC server: %w", err)
		}
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) && stopErr == nil {
			stopErr = fmt.Errorf("failed to remove pairing IPC socket: %w", err)
		}
	}

	s.server = nil
	s.listener = nil

	return stopErr
}

func (s *PairSocketServer) prepareSocketDir() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create pairing socket directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("failed to set pairing socket directory permissions: %w", err)
	}
	return nil
}

func (s *PairSocketServer) ensureSocketAvailable() error {
	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat pairing socket: %w", err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("pairing socket path is not a socket: %s", s.path)
	}

	conn, err := net.DialTimeout("unix", s.path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("pairing IPC socket already in use: %s", s.path)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("permission denied accessing pairing IPC socket: %w", err)
	}

	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale pairing IPC socket: %w", err)
	}

	return nil
}
