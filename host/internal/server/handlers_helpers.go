package server

import (
	"log"
	"time"
)

// sendDecisionResult sends a decision result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendDecisionResult(cardID, action string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewDecisionResultMessage(cardID, action, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping decision result")
	}
}

// sendChunkDecisionResult sends a chunk decision result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendChunkDecisionResult(cardID string, chunkIndex int, action string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewChunkDecisionResultMessage(cardID, chunkIndex, action, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping chunk decision result")
	}
}

// sendDeleteResult sends a delete result message to this client.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendDeleteResult(cardID string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewDeleteResultMessage(cardID, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping delete result")
	}
}

// sendUndoResult sends an undo result message to this client.
// For file-level undos, set chunkIndex to -1 (omitted from JSON).
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendUndoResult(cardID string, chunkIndex int, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewUndoResultMessage(cardID, chunkIndex, success, errCode, errMsg):
	default:
		log.Printf("Warning: client send buffer full, dropping undo result")
	}
}

// sendError sends an error message to this client.
// Uses non-blocking send to avoid blocking on slow clients.
func (c *Client) sendError(code, message string) {
	select {
	case c.send <- NewErrorMessage(code, message):
	default:
		log.Printf("Warning: client send buffer full, dropping error message")
	}
}

// sendCommitResult sends a repo.commit_result message to the client.
func (c *Client) sendCommitResult(success bool, hash, summary, errCode, errMsg string) {
	msg := NewRepoCommitResultMessage(success, hash, summary, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping commit result")
	}
}

// sendPushResult sends a repo.push_result message to the client.
func (c *Client) sendPushResult(success bool, output, errCode, errMsg string) {
	msg := NewRepoPushResultMessage(success, output, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping push result")
	}
}

// sendTmuxSessions sends a tmux.sessions message to this client.
func (c *Client) sendTmuxSessions(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending tmux.sessions to client")
	}
}
