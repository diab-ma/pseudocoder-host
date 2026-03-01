package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
)

// TLSConfig holds the TLS configuration for the server.
type TLSConfig struct {
	// CertPath is the path to the TLS certificate file.
	CertPath string
	// KeyPath is the path to the TLS private key file.
	KeyPath string
}

// Start begins listening for WebSocket connections.
// This method blocks, so call it in a goroutine if you need to do other work.
// For non-blocking startup with error handling, use StartAsync() instead.
func (s *Server) Start() error {
	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the HTTP server
	s.httpServer = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	// Start the broadcast goroutine that sends messages to all clients
	go s.runBroadcaster()

	log.Printf("WebSocket server listening on %s", s.addr)

	// ListenAndServe blocks until the server is stopped or an error occurs.
	// It returns http.ErrServerClosed on graceful shutdown.
	return s.httpServer.ListenAndServe()
}

// StartAsync starts the server in a goroutine and returns any startup errors.
// This is useful when you need to verify the server started successfully
// before proceeding with other initialization (e.g., starting a PTY session).
//
// The returned channel receives nil if startup succeeded, or an error if
// the listener could not be created (e.g., port already in use).
// After receiving from the channel, the server is either running or failed.
func (s *Server) StartAsync() <-chan error {
	errCh := make(chan error, 1)

	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the listener first to detect port conflicts immediately.
	// net.Listen returns an error if the port is already in use.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		errCh <- fmt.Errorf("failed to listen on %s: %w", s.addr, err)
		close(errCh)
		return errCh
	}

	// Create the HTTP server
	s.httpServer = &http.Server{
		Handler: mux,
	}

	// Start the broadcast goroutine
	go s.runBroadcaster()

	// Start serving in a goroutine
	go func() {
		log.Printf("WebSocket server listening on %s", s.addr)
		// Signal successful startup
		errCh <- nil
		close(errCh)

		// Serve blocks until the server is stopped
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("WebSocket server error: %v", err)
		}
	}()

	return errCh
}

// StartAsyncTLS starts the server with TLS in a goroutine and returns any startup errors.
// This is the TLS-enabled version of StartAsync. When TLS is configured, the server
// only accepts HTTPS/WSS connections, rejecting any plaintext HTTP/WS attempts.
//
// The returned channel receives nil if startup succeeded, or an error if
// the listener could not be created or TLS configuration failed.
func (s *Server) StartAsyncTLS(tlsCfg TLSConfig) <-chan error {
	errCh := make(chan error, 1)

	// Create an HTTP mux (router) for handling requests
	mux := s.createMux()

	// Create the listener first to detect port conflicts immediately.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		errCh <- fmt.Errorf("failed to listen on %s: %w", s.addr, err)
		close(errCh)
		return errCh
	}

	// Load TLS certificate and key
	cert, err := tls.LoadX509KeyPair(tlsCfg.CertPath, tlsCfg.KeyPath)
	if err != nil {
		ln.Close()
		errCh <- fmt.Errorf("failed to load TLS certificate: %w", err)
		close(errCh)
		return errCh
	}

	// Configure TLS with secure defaults.
	// MinVersion TLS 1.2 is widely supported and excludes older insecure versions.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Wrap the listener with TLS
	tlsLn := tls.NewListener(ln, tlsConfig)

	// Create the HTTP server
	s.httpServer = &http.Server{
		Handler: mux,
	}

	// Start the broadcast goroutine
	go s.runBroadcaster()

	// Start serving in a goroutine
	go func() {
		log.Printf("WebSocket server listening on %s (TLS enabled)", s.addr)
		// Signal successful startup
		errCh <- nil
		close(errCh)

		// Serve blocks until the server is stopped.
		// With TLS, only encrypted connections are accepted.
		if err := s.httpServer.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
			log.Printf("WebSocket server error: %v", err)
		}
	}()

	return errCh
}

// Stop gracefully shuts down the server.
// It sends close frames to all clients, closes connections, and stops
// accepting new ones. This also closes the broadcast channel to allow
// the runBroadcaster goroutine to exit cleanly.
func (s *Server) Stop() error {
	s.mu.Lock()

	// Mark server as stopped to prevent new broadcasts
	if s.stopped {
		s.mu.Unlock()
		return nil // Already stopped
	}
	s.stopped = true

	// Signal all clients to stop by closing their send channels.
	// writePump will send a close frame and close the connection when
	// it sees the closed channel. We don't write directly here to avoid
	// racing with writePump.
	for client := range s.clients {
		// Close the send channel to signal writePump to exit.
		// writePump will send the close frame and close the connection.
		client.closeSend()
	}

	// Clear the clients map
	s.clients = make(map[*Client]bool)

	// Close the broadcast channel to allow runBroadcaster to exit.
	// This must happen after setting stopped=true to prevent panics
	// from concurrent Broadcast() calls.
	close(s.broadcast)
	s.stopKeepAwake()

	s.mu.Unlock()

	// Shutdown the HTTP server
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}
