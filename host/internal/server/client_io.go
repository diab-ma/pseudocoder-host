package server

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// closeSend safely signals the client to shut down exactly once.
// This is safe to call multiple times from different goroutines.
// We only close the done channel (not send) to avoid racing with
// ongoing send operations. All senders check done before sending.
func (c *Client) closeSend() {
	c.sendOnce.Do(func() {
		close(c.done)
	})
}

// writePump continuously sends messages from the send channel to the WebSocket.
// It also sends periodic pings to keep the connection alive.
func (c *Client) writePump() {
	// Set up a ticker for sending pings every 30 seconds.
	// Pings help detect dead connections and keep NAT/firewalls happy.
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case <-c.done:
			// Shutdown signaled; send close frame and exit.
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case msg, ok := <-c.send:
			// Set a write deadline to prevent hanging on slow connections
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			if !ok {
				// The send channel was closed; send a close frame.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Serialize the message to JSON
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Failed to marshal message: %v", err)
				continue
			}

			// Write the message to the WebSocket
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("Write error: %v", err)
				return
			}

		case <-ticker.C:
			// Send a ping to keep the connection alive
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads messages from the WebSocket and handles them.
// For now, we mainly use this to detect when the client disconnects.
// In future units, this will handle review.decision messages from clients.
func (c *Client) readPump() {
	defer func() {
		// Unregister the client when this goroutine exits
		c.server.mu.Lock()
		delete(c.server.clients, c)
		c.server.mu.Unlock()

		// Use closeSend() to safely close the channel.
		// Stop() may have already closed it during shutdown.
		// This signals writePump to exit, which will close the connection.
		c.closeSend()

		log.Printf("Client disconnected (%d remaining)", c.server.ClientCount())
	}()

	// Configure connection parameters
	c.conn.SetReadLimit(512 * 1024) // Max message size: 512KB
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Set up pong handler to reset the read deadline.
	// When we receive a pong (response to our ping), we know the client is alive.
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		// Read the next message from the WebSocket.
		// This blocks until a message arrives or an error occurs.
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// Check if this is a normal close (client disconnected)
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure) {
				log.Printf("Read error: %v", err)
			}
			return
		}

		// Track device activity for authenticated clients.
		// This updates the last_seen timestamp on each message received.
		if c.deviceID != "" {
			c.server.mu.RLock()
			tracker := c.server.deviceActivityTracker
			c.server.mu.RUnlock()

			if tracker != nil {
				tracker(c.deviceID)
			}
		}

		// Parse the message
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Failed to parse message: %v", err)
			continue
		}

		// Handle the message based on type
		switch msg.Type {
		case MessageTypeReviewDecision:
			c.handleReviewDecision(data)
		case MessageTypeChunkDecision:
			c.handleChunkDecision(data)
		case MessageTypeReviewDelete:
			c.handleReviewDelete(data)
		// Phase 20.2: Undo messages for review card state reversal
		case MessageTypeReviewUndo:
			c.handleReviewUndo(data)
		case MessageTypeChunkUndo:
			c.handleChunkUndo(data)
		case MessageTypeTerminalInput:
			c.handleTerminalInput(data)
		case MessageTypeTerminalResize:
			c.handleTerminalResize(data)
		case MessageTypeApprovalDecision:
			c.handleApprovalDecision(data)
		case MessageTypeRepoStatus:
			c.handleRepoStatus(data)
		case MessageTypeRepoCommit:
			c.handleRepoCommit(data)
		case MessageTypeRepoPush:
			c.handleRepoPush(data)
		// Phase 9.3: Multi-session PTY management messages
		case MessageTypeSessionCreate:
			c.handleSessionCreate(data)
		case MessageTypeSessionClose:
			c.handleSessionClose(data)
		case MessageTypeSessionSwitch:
			c.handleSessionSwitch(data)
		case MessageTypeSessionRename:
			c.handleSessionRename(data)
		// Phase 12: tmux session integration messages
		case MessageTypeTmuxList:
			c.handleTmuxList(data)
		case MessageTypeTmuxAttach:
			c.handleTmuxAttach(data)
		case MessageTypeTmuxDetach:
			c.handleTmuxDetach(data)
		default:
			log.Printf("Received message: type=%s", msg.Type)
		}
	}
}
