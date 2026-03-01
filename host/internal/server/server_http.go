package server

import (
	"log"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// createMux creates the HTTP mux with all endpoints.
func (s *Server) createMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Handle WebSocket connections at the /ws endpoint
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Health check endpoint for monitoring
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Pairing endpoints
	s.mu.RLock()
	pairHandler := s.pairHandler
	generateCodeHandler := s.generateCodeHandler
	revokeHandler := s.revokeDeviceHandler
	approveHandler := s.approveHandler
	statusHandler := s.statusHandler
	s.mu.RUnlock()

	if pairHandler != nil {
		mux.Handle("/pair", pairHandler)
		log.Printf("Pairing endpoint registered at /pair")
	}

	if generateCodeHandler != nil {
		mux.Handle("/pair/generate", generateCodeHandler)
		log.Printf("Generate code endpoint registered at /pair/generate")
	}

	// Device revocation endpoint: /devices/{id}/revoke
	// This allows the CLI to signal the running host to close connections
	// for revoked devices immediately.
	if revokeHandler != nil {
		mux.Handle("/devices/", revokeHandler)
		log.Printf("Device revocation endpoint registered at /devices/{id}/revoke")
	}

	// CLI approval endpoint: /approve
	// This allows CLI tools to request approval for commands via HTTP.
	// Phase 6.1b: Enables CLI command approval broker.
	if approveHandler != nil {
		mux.Handle("/approve", approveHandler)
		log.Printf("Approval endpoint registered at /approve")
	}

	// Status endpoint: /status
	// This allows the CLI to query host status (uptime, clients, etc).
	// Phase 7.5: Enables "pseudocoder host status" CLI command.
	if statusHandler != nil {
		mux.Handle("/status", statusHandler)
		log.Printf("Status endpoint registered at /status")
	}

	// Session API endpoints: /api/session/*
	// This allows the CLI to manage sessions (new, list, kill, rename).
	// Phase 9.5: Enables CLI session commands.
	sessionAPIHandler := s.sessionAPIHandler
	if sessionAPIHandler != nil {
		mux.Handle("/api/session/", sessionAPIHandler)
		log.Printf("Session API registered at /api/session/")
	}

	// Tmux API endpoints: /api/tmux/*
	// This allows the CLI to manage tmux sessions (list-tmux, attach-tmux, detach).
	// Phase 12.9: Enables CLI tmux commands.
	tmuxAPIHandler := s.tmuxAPIHandler
	if tmuxAPIHandler != nil {
		mux.Handle("/api/tmux/", tmuxAPIHandler)
		log.Printf("Tmux API registered at /api/tmux/")
	}

	// Keep-awake policy API endpoint: /api/keep-awake/policy
	// This allows the CLI to enable/disable keep-awake policy.
	// Phase 18: Enables CLI keep-awake policy mutation.
	keepAwakePolicyHandler := s.keepAwakePolicyHandler
	if keepAwakePolicyHandler != nil {
		mux.Handle("/api/keep-awake/policy", keepAwakePolicyHandler)
		log.Printf("Keep-awake policy API registered at /api/keep-awake/policy")
	}

	return mux
}

// handleWebSocket upgrades an HTTP connection to a WebSocket connection.
// This is called by the HTTP server for each new connection to /ws.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check authentication if required
	s.mu.RLock()
	requireAuth := s.requireAuth
	tokenValidator := s.tokenValidator
	s.mu.RUnlock()

	var deviceID string

	if requireAuth && tokenValidator != nil {
		// Extract token from Authorization header
		// Expected format: "Bearer <token>"
		token := extractBearerToken(r)
		if token == "" {
			log.Printf("WebSocket connection rejected: missing authorization token")
			http.Error(w, "Unauthorized: missing token", http.StatusUnauthorized)
			return
		}

		// Validate the token
		var err error
		deviceID, err = tokenValidator(token)
		if err != nil {
			log.Printf("WebSocket connection rejected: invalid token: %v", err)
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}

		log.Printf("WebSocket connection authenticated for device %s", deviceID)
	}

	// Upgrade the HTTP connection to a WebSocket connection.
	// This performs the WebSocket handshake (HTTP 101 Switching Protocols).
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// Create a new client with a buffered send channel.
	// The buffer allows the client to fall behind temporarily
	// without blocking the broadcaster.
	client := &Client{
		conn:     conn,
		send:     make(chan Message, channelBufferSize),
		done:     make(chan struct{}),
		server:   s,
		deviceID: deviceID,
		// Rate limiter for terminal input: 1000 messages/sec with burst of 10.
		// This prevents mobile clients from flooding the PTY with input.
		inputLimiter: rate.NewLimiter(rate.Limit(1000), 10),
	}

	// Register the client
	s.mu.Lock()
	s.clients[client] = true
	s.mu.Unlock()
	s.onKeepAwakeClientConnected(client)

	log.Printf("Client connected (%d total)", s.ClientCount())

	// Send a session status message to the new client
	client.send <- NewSessionStatusMessage(s.sessionID, "running")

	// Start writePump BEFORE calling reconnect handler. This is critical because
	// the reconnect handler may send thousands of messages (terminal history lines
	// and pending cards). If writePump isn't running, the channel buffer (256 msgs)
	// fills up and most history is dropped. With writePump running, it actively
	// drains the channel while the handler adds messages.
	go client.writePump()

	// Call the reconnect handler to replay history and pending cards.
	// writePump is already running, so it will drain messages as we add them.
	s.mu.RLock()
	reconnectHandler := s.reconnectHandler
	s.mu.RUnlock()

	if reconnectHandler != nil {
		// Create a function that sends directly to this client's send channel.
		// This avoids broadcasting to all clients.
		//
		// Unlike regular broadcasts which use non-blocking sends (and may drop
		// messages for slow clients), history replay uses a BLOCKING send with
		// a timeout. This ensures all history is delivered as long as the client
		// is responsive. We use a generous timeout (5s per message) to handle
		// temporary slowdowns while still failing fast for dead clients.
		sendToClient := func(msg Message) {
			select {
			case <-client.done:
				// Client is shutting down - skip message
				return
			case client.send <- msg:
				// Message queued successfully
			case <-time.After(5 * time.Second):
				// Client is too slow or dead - give up on this message
				log.Printf("Warning: timeout sending history to client, skipping message")
			}
		}
		reconnectHandler(sendToClient)
	}

	// Start readPump for receiving client messages.
	go client.readPump()
}

// extractBearerToken extracts the token from an Authorization header.
// Returns empty string if no valid bearer token is found.
// Supports both "Bearer <token>" header and "token" query parameter as fallback.
func extractBearerToken(r *http.Request) string {
	// Try Authorization header first
	auth := r.Header.Get("Authorization")
	if auth != "" {
		// Check for "Bearer " prefix (case-insensitive)
		const bearerPrefix = "Bearer "
		if len(auth) > len(bearerPrefix) {
			prefix := auth[:len(bearerPrefix)]
			if prefix == bearerPrefix || prefix == "bearer " {
				return auth[len(bearerPrefix):]
			}
		}
	}

	// Fallback to query parameter for WebSocket connections
	// (some WebSocket clients don't support custom headers)
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}

	return ""
}
