// Package server provides the WebSocket server for client connections.
// This file implements the approval queue for CLI command approval requests.
// The queue manages pending approvals, forwards them to mobile clients,
// and waits for decisions with timeout support.
package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/pseudocoder/host/internal/errors"
)

// ApprovalRequest holds the data for an approval request from a CLI tool.
// This is what the caller provides when requesting approval for a command.
type ApprovalRequest struct {
	// RequestID is the unique identifier for this request.
	// If empty, a UUID will be generated automatically.
	RequestID string

	// Command is the command requesting approval.
	Command string

	// Cwd is the working directory where the command will run.
	Cwd string

	// Repo is the repository path for context.
	Repo string

	// Rationale explains why the command needs to run.
	Rationale string
}

// ApprovalResponse holds the response to an approval request.
// This is returned to the caller after a decision is made or timeout occurs.
type ApprovalResponse struct {
	// Decision is "approve" or "deny".
	Decision string

	// TemporaryAllowUntil is an optional time until which similar commands
	// should be auto-approved. This enables "allow for 15 minutes" behavior.
	// Nil means no temporary allow was granted.
	TemporaryAllowUntil *time.Time
}

// PendingApproval tracks an in-flight approval request.
// It holds the request data, expiration time, and a channel for the response.
type PendingApproval struct {
	// Request is the original approval request.
	Request ApprovalRequest

	// ExpiresAt is when this request will timeout and be auto-denied.
	ExpiresAt time.Time

	// ResponseCh receives the decision from the handler.
	// The channel is buffered (size 1) to prevent blocking the sender.
	ResponseCh chan ApprovalResponse
}

// TemporaryAllowRule represents a time-limited auto-approval rule.
// When a user approves a command with "allow for X minutes", a rule is created
// that auto-approves the exact same command until the expiration time.
//
// Design notes:
// - Exact command matching (not patterns) for security - prevents accidental auto-approvals
// - In-memory only - rules do not survive host restart, which is safer
type TemporaryAllowRule struct {
	// Command is the exact command string to match.
	Command string

	// ExpiresAt is when this rule expires.
	ExpiresAt time.Time

	// DeviceID is the ID of the device that granted temporary allow.
	DeviceID string
}

// ApprovalAuditStore is the interface for persisting approval audit entries.
// This is implemented by SQLiteStore and allows the queue to log all decisions.
type ApprovalAuditStore interface {
	SaveApprovalAudit(entry *ApprovalAuditEntry) error
}

// ApprovalAuditEntry represents an audit log entry.
// This is a copy of the storage type to avoid import cycles.
type ApprovalAuditEntry struct {
	ID        string
	RequestID string
	Command   string
	Cwd       string
	Repo      string
	Rationale string
	Decision  string
	DecidedAt time.Time
	ExpiresAt *time.Time
	DeviceID  string
	Source    string // "mobile", "rule", "timeout"
}

// ApprovalQueue manages pending approval requests.
// It provides a synchronous Queue method that blocks until a decision is made
// or the timeout expires. Decisions are received via the Decide method,
// which is called by the WebSocket message handler.
//
// Features:
// - Temporary allow rules: Auto-approve exact command matches until expiry
// - Audit logging: Record all decisions to SQLite for compliance/debugging
//
// Thread safety: All exported methods are safe for concurrent use.
type ApprovalQueue struct {
	// mu protects access to the pending map and allowRules slice.
	mu sync.RWMutex

	// pending maps request IDs to pending approvals.
	pending map[string]*PendingApproval

	// allowRules stores active temporary allow rules.
	// Rules are checked before queueing; matches are auto-approved.
	allowRules []TemporaryAllowRule

	// defaultTTL is the default time-to-live for approval requests.
	// If no decision is made within this time, the request is auto-denied.
	defaultTTL time.Duration

	// broadcaster sends messages to all connected clients.
	// This is used to forward approval requests to mobile clients.
	broadcaster func(Message)

	// auditStore persists audit entries. Nil means no audit logging.
	auditStore ApprovalAuditStore
}

// NewApprovalQueue creates a new approval queue.
// The ttl parameter sets the default timeout for approval requests.
// The broadcaster function is called to send approval.request messages to clients.
func NewApprovalQueue(ttl time.Duration, broadcaster func(Message)) *ApprovalQueue {
	return &ApprovalQueue{
		pending:     make(map[string]*PendingApproval),
		defaultTTL:  ttl,
		broadcaster: broadcaster,
	}
}

// Queue submits an approval request and blocks until a decision is made.
// It returns the ApprovalResponse containing the decision, or an error if:
// - The request times out (returns ApprovalTimeout error)
// - The context is cancelled
// - The request is explicitly denied (returns ApprovalDenied error)
//
// Before queueing, checks temporary allow rules for auto-approval.
// The request is forwarded to all connected mobile clients via WebSocket.
// The first client to respond wins (subsequent responses are ignored).
func (q *ApprovalQueue) Queue(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
	// Generate request ID if not provided
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}

	// Check temporary allow rules first (before queueing)
	if matched, rule := q.checkTemporaryAllow(req.Command); matched {
		log.Printf("approval: request %s auto-approved by temporary rule (expires %s)",
			req.RequestID, rule.ExpiresAt.Format(time.RFC3339))

		// Record audit entry for auto-approval
		q.recordAudit(req, "approved", &rule.ExpiresAt, rule.DeviceID, "rule")

		return ApprovalResponse{
			Decision:            "approve",
			TemporaryAllowUntil: &rule.ExpiresAt,
		}, nil
	}

	// Calculate expiration time
	expiresAt := time.Now().Add(q.defaultTTL)

	// Create response channel (buffered to prevent blocking)
	responseCh := make(chan ApprovalResponse, 1)

	// Create pending approval
	pending := &PendingApproval{
		Request:    req,
		ExpiresAt:  expiresAt,
		ResponseCh: responseCh,
	}

	// Register the pending approval (reject duplicates)
	q.mu.Lock()
	if _, exists := q.pending[req.RequestID]; exists {
		q.mu.Unlock()
		return ApprovalResponse{}, apperrors.ApprovalDuplicate(req.RequestID)
	}
	q.pending[req.RequestID] = pending
	q.mu.Unlock()

	// Send approval request to clients
	if q.broadcaster != nil {
		msg := NewApprovalRequestMessage(
			req.RequestID,
			req.Command,
			req.Cwd,
			req.Repo,
			req.Rationale,
			expiresAt,
		)
		q.broadcaster(msg)
		log.Printf("approval: request %s forwarded to clients (expires %s)", req.RequestID, expiresAt.Format(time.RFC3339))
	}

	// Wait for response with timeout
	timeout := time.Until(expiresAt)
	if timeout <= 0 {
		timeout = 1 * time.Millisecond // Minimum timeout
	}

	select {
	case response := <-responseCh:
		// Clean up
		q.mu.Lock()
		delete(q.pending, req.RequestID)
		q.mu.Unlock()

		// Check if denied
		if response.Decision == "deny" {
			log.Printf("approval: request %s denied by user", req.RequestID)
			return response, apperrors.ApprovalDenied(req.RequestID)
		}

		log.Printf("approval: request %s approved", req.RequestID)
		return response, nil

	case <-time.After(timeout):
		// Clean up
		q.mu.Lock()
		delete(q.pending, req.RequestID)
		q.mu.Unlock()

		// Record audit entry for timeout
		q.recordAudit(req, "denied", nil, "", "timeout")

		log.Printf("approval: request %s timed out", req.RequestID)
		return ApprovalResponse{Decision: "deny"}, apperrors.ApprovalTimeout(req.RequestID)

	case <-ctx.Done():
		// Clean up
		q.mu.Lock()
		delete(q.pending, req.RequestID)
		q.mu.Unlock()

		log.Printf("approval: request %s cancelled", req.RequestID)
		return ApprovalResponse{Decision: "deny"}, ctx.Err()
	}
}

// Decide processes a decision from a mobile client.
// It sends the response to the waiting Queue goroutine.
// Returns an error if the request is not found (already expired or decided).
//
// The decision parameter must be "approve" or "deny".
// The tempAllowUntil parameter is optional and enables temporary allow rules.
// The deviceID parameter identifies which device made the decision (for audit).
func (q *ApprovalQueue) Decide(requestID, decision string, tempAllowUntil *time.Time) error {
	return q.DecideWithDevice(requestID, decision, tempAllowUntil, "")
}

// DecideWithDevice processes a decision from a mobile client with device info.
// This is the same as Decide but also records the device ID for audit purposes.
func (q *ApprovalQueue) DecideWithDevice(requestID, decision string, tempAllowUntil *time.Time, deviceID string) error {
	q.mu.Lock()
	pending, ok := q.pending[requestID]
	if !ok {
		q.mu.Unlock()
		return apperrors.ApprovalNotFound(requestID)
	}
	// Capture request for audit before removing
	req := pending.Request

	// Remove from pending (the Queue goroutine will see the response and exit)
	delete(q.pending, requestID)
	q.mu.Unlock()

	// Record audit entry for mobile decision
	// Convert "approve"/"deny" to "approved"/"denied" for audit
	auditDecision := decision + "d" // "approve" -> "approved", "deny" -> "denied"
	if decision == "deny" {
		auditDecision = "denied"
	} else if decision == "approve" {
		auditDecision = "approved"
	}
	q.recordAudit(req, auditDecision, tempAllowUntil, deviceID, "mobile")

	// If approved with temporary allow, add the rule
	if decision == "approve" && tempAllowUntil != nil {
		q.AddTemporaryAllow(req.Command, *tempAllowUntil, deviceID)
	}

	// Send response to waiting goroutine (non-blocking since channel is buffered)
	response := ApprovalResponse{
		Decision:            decision,
		TemporaryAllowUntil: tempAllowUntil,
	}
	select {
	case pending.ResponseCh <- response:
		log.Printf("approval: decision received for request %s: %s", requestID, decision)
	default:
		// This should not happen with a buffered channel, but log if it does
		log.Printf("approval: warning: could not send response for request %s (channel full)", requestID)
	}

	return nil
}

// Cancel removes a pending approval without making a decision.
// This is useful for cleanup when the caller no longer needs the approval.
// It does not send a response to the waiting goroutine; the goroutine
// will timeout naturally if Cancel is called.
func (q *ApprovalQueue) Cancel(requestID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, ok := q.pending[requestID]; ok {
		delete(q.pending, requestID)
		log.Printf("approval: request %s cancelled", requestID)
	}
}

// GetPending returns a list of all pending approval requests.
// This can be used to display pending requests in the UI.
func (q *ApprovalQueue) GetPending() []ApprovalRequest {
	q.mu.RLock()
	defer q.mu.RUnlock()

	requests := make([]ApprovalRequest, 0, len(q.pending))
	for _, pending := range q.pending {
		requests = append(requests, pending.Request)
	}
	return requests
}

// PendingCount returns the number of pending approval requests.
func (q *ApprovalQueue) PendingCount() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.pending)
}

// -----------------------------------------------------------------------------
// Temporary Allow Rules
// -----------------------------------------------------------------------------

// SetAuditStore sets the audit store for recording approval decisions.
// Call this after creating the queue to enable audit logging.
func (q *ApprovalQueue) SetAuditStore(store ApprovalAuditStore) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.auditStore = store
}

// AddTemporaryAllow adds a rule that auto-approves the exact command until expiry.
// This is called when a user approves with "allow for X minutes".
func (q *ApprovalQueue) AddTemporaryAllow(command string, expiresAt time.Time, deviceID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if rule already exists for this command and update it
	for i, rule := range q.allowRules {
		if rule.Command == command {
			// Update existing rule with new expiration
			q.allowRules[i].ExpiresAt = expiresAt
			q.allowRules[i].DeviceID = deviceID
			log.Printf("approval: updated temporary allow rule for command (expires %s)", expiresAt.Format(time.RFC3339))
			return
		}
	}

	// Add new rule
	q.allowRules = append(q.allowRules, TemporaryAllowRule{
		Command:   command,
		ExpiresAt: expiresAt,
		DeviceID:  deviceID,
	})
	log.Printf("approval: added temporary allow rule for command (expires %s)", expiresAt.Format(time.RFC3339))
}

// checkTemporaryAllow checks if a command matches any active temporary allow rules.
// Returns (true, rule) if matched, (false, nil) if not matched or all rules expired.
// Also cleans up expired rules during the check.
func (q *ApprovalQueue) checkTemporaryAllow(command string) (bool, *TemporaryAllowRule) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()

	// Filter out expired rules and find matching rule
	var activeRules []TemporaryAllowRule
	var matchedRule *TemporaryAllowRule

	for _, rule := range q.allowRules {
		if rule.ExpiresAt.After(now) {
			// Rule is still active
			activeRules = append(activeRules, rule)
			if rule.Command == command && matchedRule == nil {
				// Found a match (exact command comparison)
				ruleCopy := rule // Copy to avoid pointer to loop variable
				matchedRule = &ruleCopy
			}
		} else {
			log.Printf("approval: expired temporary allow rule removed")
		}
	}

	// Update the rules slice (removes expired rules)
	q.allowRules = activeRules

	return matchedRule != nil, matchedRule
}

// CleanExpiredRules removes all expired temporary allow rules.
// This can be called periodically if needed, though rules are also
// cleaned during checkTemporaryAllow calls.
func (q *ApprovalQueue) CleanExpiredRules() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	var activeRules []TemporaryAllowRule
	removed := 0

	for _, rule := range q.allowRules {
		if rule.ExpiresAt.After(now) {
			activeRules = append(activeRules, rule)
		} else {
			removed++
		}
	}

	q.allowRules = activeRules

	if removed > 0 {
		log.Printf("approval: cleaned %d expired temporary allow rules", removed)
	}
	return removed
}

// GetTemporaryAllowRules returns a copy of all active temporary allow rules.
// Useful for debugging and testing.
func (q *ApprovalQueue) GetTemporaryAllowRules() []TemporaryAllowRule {
	q.mu.RLock()
	defer q.mu.RUnlock()

	// Return a copy to avoid races
	rules := make([]TemporaryAllowRule, len(q.allowRules))
	copy(rules, q.allowRules)
	return rules
}

// -----------------------------------------------------------------------------
// Audit Logging
// -----------------------------------------------------------------------------

// recordAudit logs an approval decision to the audit store.
// This is called for all decision types: mobile approvals, timeouts, and rule auto-approvals.
// If the audit store is nil or the write fails, an error is logged but not returned.
func (q *ApprovalQueue) recordAudit(req ApprovalRequest, decision string, expiresAt *time.Time, deviceID, source string) {
	// Read audit store with lock
	q.mu.RLock()
	store := q.auditStore
	q.mu.RUnlock()

	if store == nil {
		return // No audit store configured
	}

	entry := &ApprovalAuditEntry{
		ID:        uuid.New().String(),
		RequestID: req.RequestID,
		Command:   req.Command,
		Cwd:       req.Cwd,
		Repo:      req.Repo,
		Rationale: req.Rationale,
		Decision:  decision,
		DecidedAt: time.Now(),
		ExpiresAt: expiresAt,
		DeviceID:  deviceID,
		Source:    source,
	}

	if err := store.SaveApprovalAudit(entry); err != nil {
		log.Printf("approval: warning: failed to save audit entry: %v", err)
	}
}
