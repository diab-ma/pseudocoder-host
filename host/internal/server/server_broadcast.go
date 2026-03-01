package server

import (
	"log"

	// Stream package provides CardBroadcaster types for streaming diff cards.
	"github.com/pseudocoder/host/internal/stream"
)

// Broadcast sends a message to all connected clients.
// This method is non-blocking; messages are queued for delivery.
// If the server has been stopped, this method does nothing.
func (s *Server) Broadcast(msg Message) {
	// Hold RLock while checking stopped AND sending to avoid race with Stop().
	// Stop() takes the write lock, sets stopped=true, then closes the channel.
	// By holding RLock through the send, we ensure the channel can't be closed
	// while we're sending to it.
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.stopped {
		return
	}

	// Use select with default to make this non-blocking.
	// If the broadcast channel is full, we log and drop the message
	// rather than blocking the caller (the PTY output goroutine).
	select {
	case s.broadcast <- msg:
	default:
		log.Printf("Warning: broadcast channel full, dropping message")
	}
}

// BroadcastTerminalOutput is a convenience method for sending terminal output.
// This is the main method called from the PTY session's OnOutput callback.
func (s *Server) BroadcastTerminalOutput(chunk string) {
	s.Broadcast(NewTerminalAppendMessage(s.sessionID, chunk))
}

// BroadcastTerminalOutputWithID sends a terminal output chunk with an explicit session ID.
// This is used by the SessionManager to route output from multiple sessions.
// Phase 9.5: Multi-session terminal output broadcasting.
func (s *Server) BroadcastTerminalOutputWithID(sessionID, chunk string) {
	s.Broadcast(NewTerminalAppendMessage(sessionID, chunk))
}

// BroadcastDiffCard sends a review card to all connected clients.
// This is called when the diff poller detects new or changed chunks.
// The card is sent as a diff.card message per the WebSocket protocol.
// The chunks parameter provides per-chunk metadata for granular decisions.
// The chunkGroups parameter provides proximity grouping metadata (nil when disabled).
// The semanticGroups parameter provides semantic grouping metadata (nil when disabled).
// The isBinary flag indicates per-chunk actions should be disabled.
// The isDeleted flag indicates this is a file deletion (use file-level actions).
// The stats parameter provides size metrics for large diff warnings.
func (s *Server) BroadcastDiffCard(cardID, file, diff string, chunks []stream.ChunkInfo, chunkGroups []stream.ChunkGroupInfo, semanticGroups []stream.SemanticGroupInfo, isBinary, isDeleted bool, stats *stream.DiffStats, createdAt int64) {
	// Convert stream types to server types using dedicated mappers
	serverChunks := mapChunksToServer(chunks)
	serverChunkGroups := mapChunkGroupsToServer(chunkGroups)
	serverSemanticGroups := mapSemanticGroupsToServer(semanticGroups)
	serverStats := mapStatsToServer(stats)

	s.Broadcast(NewDiffCardMessage(cardID, file, diff, serverChunks, serverChunkGroups, serverSemanticGroups, isBinary, isDeleted, serverStats, createdAt))
}

// BroadcastCardRemoved notifies clients that a card was removed.
// This is called when changes are staged/reverted externally (e.g., VS Code).
func (s *Server) BroadcastCardRemoved(cardID string) {
	s.Broadcast(NewCardRemovedMessage(cardID))
}

// BroadcastFlags sends the current flag state to all connected clients.
// This is called on flag mutation to keep mobile clients in sync.
func (s *Server) BroadcastFlags(payload ServerFlagsPayload) {
	s.Broadcast(NewServerFlagsChangedMessage(payload))
}

// BroadcastFileWatch notifies clients that a file changed.
func (s *Server) BroadcastFileWatch(path, change, version string) {
	s.Broadcast(NewFileWatchMessage(path, change, version))
}

// runBroadcaster reads from the broadcast channel and sends to all clients.
// This runs in its own goroutine started by Start().
func (s *Server) runBroadcaster() {
	for msg := range s.broadcast {
		s.mu.RLock()
		for client := range s.clients {
			// Try to send to each client, but don't block if their buffer is full
			// or if the client is shutting down.
			select {
			case <-client.done:
				// Client is shutting down - skip
			case client.send <- msg:
			default:
				// Client is too slow; we could disconnect them here,
				// but for now we just drop the message for this client.
				log.Printf("Warning: client send buffer full, dropping message")
			}
		}
		s.mu.RUnlock()
	}
}
