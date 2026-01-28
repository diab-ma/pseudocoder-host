package server

import (
	"encoding/json"
	"log"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"
)

// handleReviewDecision processes a review.decision message from the client.
// It validates the payload, calls the decision handler, and sends a result
// message back to the client with standardized error codes.
func (c *Client) handleReviewDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType           `json:"type"`
		ID      string                `json:"id,omitempty"`
		Payload ReviewDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.decision payload: %v", err)
		c.sendDecisionResult("", "", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.decision missing card_id")
		c.sendDecisionResult("", payload.Action, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if payload.Action != "accept" && payload.Action != "reject" {
		log.Printf("review.decision invalid action: %s", payload.Action)
		c.sendDecisionResult(payload.CardID, payload.Action, false,
			apperrors.CodeActionInvalid, "action must be 'accept' or 'reject'")
		return
	}

	// Get the decision handler from the server
	c.server.mu.RLock()
	handler := c.server.decisionHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No decision handler registered, ignoring decision for card %s", payload.CardID)
		c.sendDecisionResult(payload.CardID, payload.Action, false,
			apperrors.CodeServerHandlerMissing, "decision handler not configured")
		return
	}

	// Call the handler to apply the decision
	if err := handler(payload.CardID, payload.Action, payload.Comment); err != nil {
		log.Printf("Decision handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendDecisionResult(payload.CardID, payload.Action, false, code, message)
		return
	}

	log.Printf("Decision applied: card=%s action=%s", payload.CardID, payload.Action)

	// Broadcast the decision result to all connected clients so they can
	// update their UI (e.g., remove the card from pending list).
	// This reaches the sender too, so no separate direct send needed.
	c.server.Broadcast(NewDecisionResultMessage(payload.CardID, payload.Action, true, "", ""))
}

// handleChunkDecision processes a chunk.decision message from the client.
// It validates the payload, calls the chunk decision handler, and sends a result
// message back to the client with standardized error codes.
func (c *Client) handleChunkDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType          `json:"type"`
		ID      string               `json:"id,omitempty"`
		Payload ChunkDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse chunk.decision payload: %v", err)
		c.sendChunkDecisionResult("", -1, "", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	if payload.CardID == "" {
		log.Printf("chunk.decision missing card_id")
		c.sendChunkDecisionResult("", payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if payload.ChunkIndex < 0 {
		log.Printf("chunk.decision invalid chunk_index: %d", payload.ChunkIndex)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerInvalidMessage, "chunk_index must be >= 0")
		return
	}

	if payload.Action != "accept" && payload.Action != "reject" {
		log.Printf("chunk.decision invalid action: %s", payload.Action)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeActionInvalid, "action must be 'accept' or 'reject'")
		return
	}

	// Get the chunk decision handler from the server
	c.server.mu.RLock()
	handler := c.server.chunkDecisionHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No chunk decision handler registered, ignoring decision for card %s chunk %d",
			payload.CardID, payload.ChunkIndex)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false,
			apperrors.CodeServerHandlerMissing, "chunk decision handler not configured")
		return
	}

	// Call the handler to apply the decision
	// Pass contentHash for stale detection (empty means skip validation for backward compat)
	if err := handler(payload.CardID, payload.ChunkIndex, payload.Action, payload.ContentHash); err != nil {
		log.Printf("Chunk decision handler error for card %s chunk %d: %v",
			payload.CardID, payload.ChunkIndex, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendChunkDecisionResult(payload.CardID, payload.ChunkIndex, payload.Action, false, code, message)
		return
	}

	log.Printf("Chunk decision applied: card=%s chunk=%d action=%s",
		payload.CardID, payload.ChunkIndex, payload.Action)

	// Broadcast the decision result to all connected clients so they can
	// update their UI (e.g., mark the chunk as decided).
	c.server.Broadcast(NewChunkDecisionResultMessage(
		payload.CardID, payload.ChunkIndex, payload.Action, true, "", ""))
}

// handleReviewDelete processes a review.delete message from the client.
// This is used to delete untracked files that cannot be restored via git.
// It validates the payload, calls the delete handler, and sends a result.
func (c *Client) handleReviewDelete(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType         `json:"type"`
		ID      string              `json:"id,omitempty"`
		Payload ReviewDeletePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse review.delete payload: %v", err)
		c.sendDeleteResult("", false,
			apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload
	if payload.CardID == "" {
		log.Printf("review.delete missing card_id")
		c.sendDeleteResult("", false,
			apperrors.CodeServerInvalidMessage, "card_id is required")
		return
	}

	if !payload.Confirmed {
		log.Printf("review.delete missing confirmation for card %s", payload.CardID)
		c.sendDeleteResult(payload.CardID, false,
			apperrors.CodeServerInvalidMessage, "deletion requires confirmed=true")
		return
	}

	// Get the delete handler from the server
	c.server.mu.RLock()
	handler := c.server.deleteHandler
	c.server.mu.RUnlock()

	if handler == nil {
		log.Printf("No delete handler registered, ignoring delete for card %s", payload.CardID)
		c.sendDeleteResult(payload.CardID, false,
			apperrors.CodeServerHandlerMissing, "delete handler not configured")
		return
	}

	// Call the handler to delete the file
	if err := handler(payload.CardID); err != nil {
		log.Printf("Delete handler error for card %s: %v", payload.CardID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendDeleteResult(payload.CardID, false, code, message)
		return
	}

	log.Printf("File deleted: card=%s", payload.CardID)

	// Broadcast the delete result to all connected clients so they can
	// update their UI (e.g., remove the card from pending list).
	c.server.Broadcast(NewDeleteResultMessage(payload.CardID, true, "", ""))
}
