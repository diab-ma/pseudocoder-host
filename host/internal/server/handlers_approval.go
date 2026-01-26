package server

import (
	"encoding/json"
	"log"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"
)

// handleApprovalDecision processes an approval.decision message from the client.
// This is the response to an approval.request message sent by a CLI tool.
// It validates the payload and forwards the decision to the approval queue.
func (c *Client) handleApprovalDecision(data []byte) {
	// Parse the full message with the typed payload
	var msg struct {
		Type    MessageType             `json:"type"`
		ID      string                  `json:"id,omitempty"`
		Payload ApprovalDecisionPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse approval.decision payload: %v", err)
		c.sendError(apperrors.CodeServerInvalidMessage, "invalid message format")
		return
	}

	payload := msg.Payload

	// Validate required fields
	if payload.RequestID == "" {
		log.Printf("approval.decision missing request_id")
		c.sendError(apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	if payload.Decision != "approve" && payload.Decision != "deny" {
		log.Printf("approval.decision invalid decision: %s", payload.Decision)
		c.sendError(apperrors.CodeServerInvalidMessage, "decision must be 'approve' or 'deny'")
		return
	}

	// Get the approval queue from the server
	c.server.mu.RLock()
	queue := c.server.approvalQueue
	c.server.mu.RUnlock()

	if queue == nil {
		log.Printf("No approval queue configured, ignoring decision for request %s", payload.RequestID)
		c.sendError(apperrors.CodeServerHandlerMissing, "approval queue not configured")
		return
	}

	// Parse optional temporary allow until timestamp
	var tempAllowUntil *time.Time
	if payload.TemporaryAllowUntil != "" {
		t, err := time.Parse(time.RFC3339, payload.TemporaryAllowUntil)
		if err != nil {
			log.Printf("approval.decision invalid temporary_allow_until: %v", err)
			c.sendError(apperrors.CodeServerInvalidMessage, "invalid temporary_allow_until timestamp")
			return
		}
		tempAllowUntil = &t
	}

	// Forward decision to the queue with device ID for audit tracking
	if err := queue.DecideWithDevice(payload.RequestID, payload.Decision, tempAllowUntil, c.deviceID); err != nil {
		log.Printf("Approval decision error for request %s: %v", payload.RequestID, err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendError(code, message)
		return
	}

	log.Printf("Approval decision received: request=%s decision=%s device=%s", payload.RequestID, payload.Decision, c.deviceID)
}
