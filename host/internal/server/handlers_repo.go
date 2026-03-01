package server

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// Storage package provides card and session persistence.
	"github.com/pseudocoder/host/internal/storage"
)

// extractRawRequestIDFromPayload returns request_id from a raw message payload.
// This preserves correlation semantics for malformed messages where typed
// payload unmarshal fails but request_id was still present in the raw JSON.
func extractRawRequestIDFromPayload(data []byte) string {
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Payload) == 0 {
		return ""
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return ""
	}

	rawRequestID, ok := payload["request_id"]
	if !ok {
		return ""
	}

	var requestID string
	if err := json.Unmarshal(rawRequestID, &requestID); err == nil {
		return requestID
	}

	return strings.TrimSpace(string(rawRequestID))
}

// handleRepoStatus processes a repo.status message from the client.
// It returns the current git repository status including branch, staged/unstaged counts.
func (c *Client) handleRepoStatus(data []byte) {
	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.status request")
		c.sendError(apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Get repository status
	status, err := gitOps.GetRepoStatus()
	if err != nil {
		log.Printf("Failed to get repo status: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendError(code, message)
		return
	}

	// Send status to the requesting client only (not broadcast)
	msg := NewRepoStatusMessage(
		status.Branch,
		status.Upstream,
		status.StagedCount,
		status.StagedFiles,
		status.UnstagedCount,
		status.LastCommit,
		status.ReadinessState,
		status.ReadinessBlockers,
		status.ReadinessWarnings,
		status.ReadinessActions,
	)
	select {
	case <-c.done:
		return
	case c.send <- msg:
		log.Printf("Sent repo.status: branch=%s staged=%d unstaged=%d", status.Branch, status.StagedCount, status.UnstagedCount)
	case <-time.After(5 * time.Second):
		log.Printf("Warning: timeout sending repo.status to client")
	}
}

// handleRepoCommit processes a repo.commit message from the client.
// It creates a git commit with the specified message and returns the result.
func (c *Client) handleRepoCommit(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload RepoCommitPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.commit message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendCommitResultMsg(NewRepoCommitResultMessage(rawID, false, "", "",
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendCommitResult(payload.RequestID, false, "", "",
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	// 2. Idempotency resolution (BEFORE gate)
	fp := commitFingerprint(payload.Message, payload.NoVerify, payload.NoGpgSign, payload.OverrideWarnings)
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoCommit, requestID, fp)
	if replay {
		c.sendCommitResultMsg(cached)
		return
	}
	if err != nil {
		c.sendCommitResult(requestID, false, "", "",
			apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendCommitResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoCommit, requestID, fp, result)
		c.sendCommitResultMsg(result)
	}

	// 3. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.commit request")
		result := NewRepoCommitResultMessage(requestID, false, "", "",
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// 4. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoCommitResultMessage(requestID, false, "", "",
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// Recompute readiness on each commit request so server-side gating is
	// authoritative even if client UI status is stale.
	status, statusErr := gitOps.GetRepoStatus()
	if statusErr != nil {
		log.Printf("Failed to get repo status before commit: %v", statusErr)
		code, message := apperrors.ToCodeAndMessage(statusErr)
		result := NewRepoCommitResultMessage(requestID, false, "", "", code, message)
		finish(result)
		return
	}

	if status.ReadinessState == CommitReadinessBlocked {
		appErr := apperrors.CommitReadinessBlocked(status.ReadinessBlockers)
		result := NewRepoCommitResultMessage(requestID, false, "", "", appErr.Code, appErr.Message)
		finish(result)
		return
	}

	if status.ReadinessState == CommitReadinessRisky && !payload.OverrideWarnings {
		appErr := apperrors.CommitOverrideRequired(status.ReadinessWarnings)
		result := NewRepoCommitResultMessage(requestID, false, "", "", appErr.Code, appErr.Message)
		finish(result)
		return
	}

	if status.ReadinessState == CommitReadinessRisky && payload.OverrideWarnings {
		log.Printf(
			"repo.commit: advisory override accepted warnings=%v device=%s",
			status.ReadinessWarnings,
			c.deviceID,
		)
	}

	// 5. Create the commit
	hash, summary, commitErr := gitOps.Commit(payload.Message, payload.NoVerify, payload.NoGpgSign)
	if commitErr != nil {
		log.Printf("Commit failed: %v", commitErr)
		code, message := apperrors.ToCodeAndMessage(commitErr)
		result := NewRepoCommitResultMessage(requestID, false, "", "", code, message)
		finish(result)
		return
	}

	log.Printf("Commit created: hash=%s summary=%s device=%s", hash, summary, c.deviceID)

	// Phase 20.2: Mark accepted cards as committed for undo support.
	// This associates the commit hash with any cards that were staged.
	c.server.mu.RLock()
	decidedStore := c.server.decidedStore
	c.server.mu.RUnlock()

	if decidedStore != nil {
		// Get all accepted (staged) file-level cards and mark them as committed
		acceptedCards, listErr := decidedStore.ListDecidedByStatus(storage.CardAccepted)
		if listErr != nil {
			log.Printf("repo.commit: failed to list accepted cards: %v", listErr)
		} else if len(acceptedCards) > 0 {
			// Collect card IDs to mark as committed
			cardIDs := make([]string, len(acceptedCards))
			for i, card := range acceptedCards {
				cardIDs[i] = card.ID
			}

			if markErr := decidedStore.MarkCardsCommitted(cardIDs, hash); markErr != nil {
				log.Printf("repo.commit: failed to mark cards as committed: %v", markErr)
			} else {
				log.Printf("repo.commit: marked %d cards as committed with hash %s", len(cardIDs), hash)
			}
		}

		// Mark any accepted chunks as committed.
		allCards, allErr := decidedStore.ListAllDecidedCards()
		if allErr != nil {
			log.Printf("repo.commit: failed to list all decided cards for chunk processing: %v", allErr)
		} else {
			for _, card := range allCards {
				chunks, chunkErr := decidedStore.GetDecidedChunks(card.ID)
				if chunkErr != nil {
					log.Printf("repo.commit: failed to get chunks for card %s: %v", card.ID, chunkErr)
					continue
				}

				var acceptedIndexes []int
				for _, chunk := range chunks {
					if chunk.Status == storage.CardAccepted {
						acceptedIndexes = append(acceptedIndexes, chunk.ChunkIndex)
					}
				}

				if len(acceptedIndexes) > 0 {
					if markErr := decidedStore.MarkChunksCommitted(card.ID, acceptedIndexes, hash); markErr != nil {
						log.Printf("repo.commit: failed to mark chunks as committed for card %s: %v", card.ID, markErr)
					}
				}
			}
		}
	}

	// 6. Requester-only success
	result := NewRepoCommitResultMessage(requestID, true, hash, summary, "", "")
	finish(result)
}

// handleRepoPush processes a repo.push message from the client.
// It pushes commits to the remote and returns the result.
func (c *Client) handleRepoPush(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType     `json:"type"`
		ID      string          `json:"id,omitempty"`
		Payload RepoPushPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.push message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPushResultMsg(NewRepoPushResultMessage(rawID, false, "",
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendPushResult(payload.RequestID, false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	// 2. Idempotency resolution (BEFORE gate)
	fp := pushFingerprint(payload.Remote, payload.Branch, payload.ForceWithLease)
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoPush, requestID, fp)
	if replay {
		c.sendPushResultMsg(cached)
		return
	}
	if err != nil {
		c.sendPushResult(requestID, false, "",
			apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendPushResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoPush, requestID, fp, result)
		c.sendPushResultMsg(result)
	}

	// 3. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.push request")
		result := NewRepoPushResultMessage(requestID, false, "",
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// 4. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoPushResultMessage(requestID, false, "",
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// 5. Push to remote
	output, pushErr := gitOps.Push(payload.Remote, payload.Branch, payload.ForceWithLease)
	if pushErr != nil {
		log.Printf("Push failed: %v", pushErr)
		code, message := apperrors.ToCodeAndMessage(pushErr)
		result := NewRepoPushResultMessage(requestID, false, "", code, message)
		finish(result)
		return
	}

	log.Printf("Push succeeded: output=%s device=%s", output, c.deviceID)

	// 6. Requester-only success
	result := NewRepoPushResultMessage(requestID, true, output, "", "")
	finish(result)
}

// handleRepoHistory processes a repo.history message from the client.
// It returns paginated commit history for the current branch.
func (c *Client) handleRepoHistory(data []byte) {
	var msg struct {
		Type    MessageType        `json:"type"`
		Payload RepoHistoryPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.history payload: %v", err)
		c.sendHistoryResult(NewRepoHistoryResultMessage(extractRawRequestIDFromPayload(data), false, nil, "",
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if strings.TrimSpace(payload.RequestID) == "" {
		c.sendHistoryResult(NewRepoHistoryResultMessage(payload.RequestID, false, nil, "",
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	pageSize := 50
	if payload.PageSize != nil {
		// Reject explicit out-of-range values; omitted page_size defaults to 50.
		if *payload.PageSize < 1 || *payload.PageSize > 200 {
			c.sendHistoryResult(NewRepoHistoryResultMessage(payload.RequestID, false, nil, "",
				apperrors.CodeServerInvalidMessage, "page_size must be between 1 and 200"))
			return
		}
		pageSize = *payload.PageSize
	}

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		c.sendHistoryResult(NewRepoHistoryResultMessage(payload.RequestID, false, nil, "",
			apperrors.CodeServerHandlerMissing, "git operations not configured"))
		return
	}

	entries, nextCursor, err := gitOps.GetHistory(payload.Cursor, pageSize)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendHistoryResult(NewRepoHistoryResultMessage(payload.RequestID, false, nil, "", code, message))
		return
	}

	c.sendHistoryResult(NewRepoHistoryResultMessage(payload.RequestID, true, entries, nextCursor, "", ""))
}

// handleRepoBranches processes a repo.branches message from the client.
// It returns the current branch, local branches, and tracked remote branches.
func (c *Client) handleRepoBranches(data []byte) {
	var msg struct {
		Type    MessageType         `json:"type"`
		Payload RepoBranchesPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.branches payload: %v", err)
		c.sendBranchesResult(NewRepoBranchesResultMessage(extractRawRequestIDFromPayload(data), false, "", nil, nil,
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if strings.TrimSpace(payload.RequestID) == "" {
		c.sendBranchesResult(NewRepoBranchesResultMessage(payload.RequestID, false, "", nil, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		c.sendBranchesResult(NewRepoBranchesResultMessage(payload.RequestID, false, "", nil, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured"))
		return
	}

	currentBranch, local, trackedRemote, err := gitOps.GetBranches()
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendBranchesResult(NewRepoBranchesResultMessage(payload.RequestID, false, "", nil, nil, code, message))
		return
	}

	c.sendBranchesResult(NewRepoBranchesResultMessage(payload.RequestID, true, currentBranch, local, trackedRemote, "", ""))
}

// handleRepoBranchCreate processes a repo.branch_create message from the client.
func (c *Client) handleRepoBranchCreate(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType             `json:"type"`
		Payload RepoBranchCreatePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.branch_create payload: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendBranchCreateResult(NewRepoBranchCreateResultMessage(rawID, false, "",
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendBranchCreateResult(NewRepoBranchCreateResultMessage(payload.RequestID, false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	name := strings.TrimSpace(payload.Name)

	// 2. Idempotency resolution (BEFORE gate)
	fp := branchCreateFingerprint(name)
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoBranchCreate, requestID, fp)
	if replay {
		c.sendBranchCreateResult(cached)
		return
	}
	if err != nil {
		c.sendBranchCreateResult(NewRepoBranchCreateResultMessage(requestID, false, "",
			apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendBranchCreateResult(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoBranchCreate, requestID, fp, result)
		c.sendBranchCreateResult(result)
	}

	// Validate name (after idempotency so empty-name failures are cached)
	if name == "" {
		result := NewRepoBranchCreateResultMessage(requestID, false, "",
			apperrors.CodeServerInvalidMessage, "name is required")
		finish(result)
		return
	}

	// 3. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoBranchCreateResultMessage(requestID, false, name,
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// 4. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		result := NewRepoBranchCreateResultMessage(requestID, false, name,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// Validate branch name format
	if err := gitOps.ValidateBranchName(name); err != nil {
		result := NewRepoBranchCreateResultMessage(requestID, false, name,
			apperrors.CodeActionInvalid, "invalid branch name: "+name)
		finish(result)
		return
	}

	// Check for empty repo
	isEmpty, err := gitOps.IsEmptyRepo()
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchCreateResultMessage(requestID, false, name, code, message)
		finish(result)
		return
	}
	if isEmpty {
		result := NewRepoBranchCreateResultMessage(requestID, false, name,
			apperrors.CodeActionInvalid, "cannot create branch in empty repository (no commits)")
		finish(result)
		return
	}

	// Check for duplicate local branch
	exists, err := gitOps.LocalBranchExists(name)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchCreateResultMessage(requestID, false, name, code, message)
		finish(result)
		return
	}
	if exists {
		result := NewRepoBranchCreateResultMessage(requestID, false, name,
			apperrors.CodeActionInvalid, "branch already exists: "+name)
		finish(result)
		return
	}

	// Create the branch
	if err := gitOps.CreateBranch(name); err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchCreateResultMessage(requestID, false, name, code, message)
		finish(result)
		return
	}

	// 5. Cache + send success
	result := NewRepoBranchCreateResultMessage(requestID, true, name, "", "")
	finish(result)
	log.Printf("Branch created: name=%s device=%s", name, c.deviceID)
}

// handleRepoBranchSwitch processes a repo.branch_switch message from the client.
func (c *Client) handleRepoBranchSwitch(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType             `json:"type"`
		Payload RepoBranchSwitchPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.branch_switch payload: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendBranchSwitchResult(NewRepoBranchSwitchResultMessage(rawID, false, "", nil,
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendBranchSwitchResult(NewRepoBranchSwitchResultMessage(payload.RequestID, false, "", nil,
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	name := strings.TrimSpace(payload.Name)

	// 2. Idempotency resolution (BEFORE gate)
	fp := branchSwitchFingerprint(name)
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoBranchSwitch, requestID, fp)
	if replay {
		c.sendBranchSwitchResult(cached)
		return
	}
	if err != nil {
		c.sendBranchSwitchResult(NewRepoBranchSwitchResultMessage(requestID, false, "", nil,
			apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendBranchSwitchResult(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoBranchSwitch, requestID, fp, result)
		c.sendBranchSwitchResult(result)
	}

	// Validate name
	if name == "" {
		result := NewRepoBranchSwitchResultMessage(requestID, false, "", nil,
			apperrors.CodeServerInvalidMessage, "name is required")
		finish(result)
		return
	}

	// 3. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil,
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// 4. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// Check for empty repo
	isEmpty, err := gitOps.IsEmptyRepo()
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil, code, message)
		finish(result)
		return
	}
	if isEmpty {
		// In empty repo, only same-branch no-op is allowed
		currentBranch, branchErr := gitOps.GetCurrentBranchName()
		if branchErr == nil && currentBranch == name {
			result := NewRepoBranchSwitchResultMessage(requestID, true, name, nil, "", "")
			finish(result)
			return
		}
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil,
			apperrors.CodeActionInvalid, "cannot switch branches in empty repository (no commits)")
		finish(result)
		return
	}

	// Check local branch existence
	exists, err := gitOps.LocalBranchExists(name)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil, code, message)
		finish(result)
		return
	}
	if !exists {
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil,
			apperrors.CodeActionInvalid, "branch not found: "+name)
		finish(result)
		return
	}

	// Perform switch (handles same-branch noop, dirty check, checkout, race fallback)
	blockers, err := gitOps.SwitchBranch(name)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, nil, code, message)
		finish(result)
		return
	}
	if len(blockers) > 0 {
		result := NewRepoBranchSwitchResultMessage(requestID, false, name, blockers,
			apperrors.CodeActionInvalid, "working tree is dirty")
		finish(result)
		return
	}

	// 5. Cache + send success
	result := NewRepoBranchSwitchResultMessage(requestID, true, name, nil, "", "")
	finish(result)
	log.Printf("Branch switched: name=%s device=%s", name, c.deviceID)
}

// handleRepoPrList processes a repo.pr_list message from the client.
// Read-only operation: no mutation gate, no idempotency.
func (c *Client) handleRepoPrList(data []byte) {
	var msg struct {
		Type    MessageType       `json:"type"`
		Payload RepoPrListPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.pr_list message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPrListResultMsg(NewRepoPrListResultMessage(rawID, false, nil,
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	requestID := strings.TrimSpace(msg.Payload.RequestID)
	if requestID == "" {
		c.sendPrListResult(msg.Payload.RequestID, false, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		c.sendPrListResult(requestID, false, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	entries, err := gitOps.PrList()
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendPrListResult(requestID, false, nil, code, message)
		return
	}

	c.sendPrListResult(requestID, true, entries, "", "")
}

// handleRepoPrView processes a repo.pr_view message from the client.
// Read-only operation: no mutation gate, no idempotency.
func (c *Client) handleRepoPrView(data []byte) {
	var msg struct {
		Type    MessageType       `json:"type"`
		Payload RepoPrViewPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.pr_view message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPrViewResultMsg(NewRepoPrViewResultMessage(rawID, false, nil,
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	requestID := strings.TrimSpace(msg.Payload.RequestID)
	if requestID == "" {
		c.sendPrViewResult(msg.Payload.RequestID, false, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	if msg.Payload.Number <= 0 {
		c.sendPrViewResult(requestID, false, nil,
			apperrors.CodePrValidationFailed, "number must be a positive integer")
		return
	}

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		c.sendPrViewResult(requestID, false, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	pr, err := gitOps.PrView(msg.Payload.Number)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendPrViewResult(requestID, false, nil, code, message)
		return
	}

	c.sendPrViewResult(requestID, true, pr, "", "")
}

// handleRepoPrCreate processes a repo.pr_create message from the client.
// Mutation operation: uses idempotency + mutation gate.
func (c *Client) handleRepoPrCreate(data []byte) {
	var msg struct {
		Type    MessageType         `json:"type"`
		Payload RepoPrCreatePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.pr_create message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPrCreateResultMsg(NewRepoPrCreateResultMessage(rawID, false, nil,
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	requestID := strings.TrimSpace(msg.Payload.RequestID)
	if requestID == "" {
		c.sendPrCreateResult(msg.Payload.RequestID, false, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	// Normalize fields before fingerprint
	title := strings.TrimSpace(msg.Payload.Title)
	baseBranch := strings.TrimSpace(msg.Payload.BaseBranch)
	baseBranchProvided := msg.Payload.BaseBranch != ""
	draft := false
	if msg.Payload.Draft != nil {
		draft = *msg.Payload.Draft
	}

	// Idempotency resolution (BEFORE gate)
	fp := prCreateFingerprint(title, msg.Payload.Body, baseBranch, draft, baseBranchProvided)
	cached, replay, inFlight, idErr := c.idempotencyCheckOrBegin(MessageTypeRepoPrCreate, requestID, fp)
	if replay {
		c.sendPrCreateResultMsg(cached)
		return
	}
	if idErr != nil {
		c.sendPrCreateResult(requestID, false, nil,
			apperrors.CodeServerInvalidMessage, idErr.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendPrCreateResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoPrCreate, requestID, fp, result)
		c.sendPrCreateResultMsg(result)
	}

	// Validate title
	if title == "" {
		result := NewRepoPrCreateResultMessage(requestID, false, nil,
			apperrors.CodePrValidationFailed, "title is required")
		finish(result)
		return
	}

	// Validate base_branch if provided
	if msg.Payload.BaseBranch != "" && baseBranch == "" {
		result := NewRepoPrCreateResultMessage(requestID, false, nil,
			apperrors.CodePrValidationFailed, "base_branch must be non-empty when provided")
		finish(result)
		return
	}

	// Mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoPrCreateResultMessage(requestID, false, nil,
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		result := NewRepoPrCreateResultMessage(requestID, false, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	pr, err := gitOps.PrCreate(title, msg.Payload.Body, baseBranch, draft)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoPrCreateResultMessage(requestID, false, nil, code, message)
		finish(result)
		return
	}

	log.Printf("PR created: #%d device=%s", pr.Number, c.deviceID)
	result := NewRepoPrCreateResultMessage(requestID, true, pr, "", "")
	finish(result)
}

// handleRepoPrCheckout processes a repo.pr_checkout message from the client.
// Mutation operation: uses idempotency + mutation gate + dirty preflight.
func (c *Client) handleRepoPrCheckout(data []byte) {
	var msg struct {
		Type    MessageType           `json:"type"`
		Payload RepoPrCheckoutPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.pr_checkout message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPrCheckoutResultMsg(NewRepoPrCheckoutResultMessage(rawID, false, "", false, nil,
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	requestID := strings.TrimSpace(msg.Payload.RequestID)
	if requestID == "" {
		c.sendPrCheckoutResult(msg.Payload.RequestID, false, "", false, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	if msg.Payload.Number <= 0 {
		c.sendPrCheckoutResult(requestID, false, "", false, nil,
			apperrors.CodePrValidationFailed, "number must be a positive integer")
		return
	}

	// Idempotency resolution (BEFORE gate)
	fp := prCheckoutFingerprint(msg.Payload.Number)
	cached, replay, inFlight, idErr := c.idempotencyCheckOrBegin(MessageTypeRepoPrCheckout, requestID, fp)
	if replay {
		c.sendPrCheckoutResultMsg(cached)
		return
	}
	if idErr != nil {
		c.sendPrCheckoutResult(requestID, false, "", false, nil,
			apperrors.CodeServerInvalidMessage, idErr.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendPrCheckoutResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoPrCheckout, requestID, fp, result)
		c.sendPrCheckoutResultMsg(result)
	}

	// Mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoPrCheckoutResultMessage(requestID, false, "", false, nil,
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		result := NewRepoPrCheckoutResultMessage(requestID, false, "", false, nil,
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// Dirty preflight
	blockers, blockerErr := gitOps.CheckDirtyBlockers()
	if blockerErr != nil {
		code, message := apperrors.ToCodeAndMessage(blockerErr)
		result := NewRepoPrCheckoutResultMessage(requestID, false, "", false, nil, code, message)
		finish(result)
		return
	}
	if len(blockers) > 0 {
		appErr := apperrors.PrCheckoutBlockedDirty(blockers)
		result := NewRepoPrCheckoutResultMessage(requestID, false, "", false, blockers, appErr.Code, appErr.Message)
		finish(result)
		return
	}

	branchName, changedBranch, err := gitOps.PrCheckout(msg.Payload.Number)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewRepoPrCheckoutResultMessage(requestID, false, "", false, nil, code, message)
		finish(result)
		return
	}

	log.Printf("PR checkout: branch=%s changed=%v device=%s", branchName, changedBranch, c.deviceID)
	result := NewRepoPrCheckoutResultMessage(requestID, true, branchName, changedBranch, nil, "", "")
	finish(result)
}

// handleRepoFetch processes a repo.fetch message from the client.
// It runs git fetch and returns the result.
func (c *Client) handleRepoFetch(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType      `json:"type"`
		ID      string           `json:"id,omitempty"`
		Payload RepoFetchPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.fetch message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendFetchResultMsg(NewRepoFetchResultMessage(rawID, false, "",
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendFetchResult(payload.RequestID, false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	// 2. Idempotency resolution (BEFORE gate)
	fp := fetchFingerprint()
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoFetch, requestID, fp)
	if replay {
		c.sendFetchResultMsg(cached)
		return
	}
	if err != nil {
		c.sendFetchResult(requestID, false, "",
			apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendFetchResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoFetch, requestID, fp, result)
		c.sendFetchResultMsg(result)
	}

	// 3. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.fetch request")
		result := NewRepoFetchResultMessage(requestID, false, "",
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// 4. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoFetchResultMessage(requestID, false, "",
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// 5. Execute fetch
	output, fetchErr := gitOps.Fetch()
	if fetchErr != nil {
		log.Printf("Fetch failed: %v", fetchErr)
		code, message := apperrors.ToCodeAndMessage(fetchErr)
		result := NewRepoFetchResultMessage(requestID, false, "", code, message)
		finish(result)
		return
	}

	log.Printf("Fetch succeeded: device=%s", c.deviceID)
	result := NewRepoFetchResultMessage(requestID, true, output, "", "")
	finish(result)
}

// handleRepoPull processes a repo.pull message from the client.
// It runs git pull --ff-only and returns the result.
func (c *Client) handleRepoPull(data []byte) {
	// 1. Parse + validate
	var msg struct {
		Type    MessageType     `json:"type"`
		ID      string          `json:"id,omitempty"`
		Payload RepoPullPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.pull message: %v", err)
		rawID := extractRawRequestIDFromPayload(data)
		c.sendPullResultMsg(NewRepoPullResultMessage(rawID, false, "",
			apperrors.CodeServerInvalidMessage, "invalid JSON"))
		return
	}

	payload := msg.Payload
	requestID := strings.TrimSpace(payload.RequestID)
	if requestID == "" {
		c.sendPullResult(payload.RequestID, false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required")
		return
	}

	// 2. Idempotency resolution (BEFORE gate)
	fp := pullFingerprint()
	cached, replay, inFlight, err := c.idempotencyCheckOrBegin(MessageTypeRepoPull, requestID, fp)
	if replay {
		c.sendPullResultMsg(cached)
		return
	}
	if err != nil {
		c.sendPullResult(requestID, false, "",
			apperrors.CodeServerInvalidMessage, err.Error())
		return
	}
	if inFlight != nil {
		select {
		case <-c.done:
			return
		case <-inFlight.done:
			c.sendPullResultMsg(inFlight.Result)
			return
		}
	}

	finish := func(result Message) {
		c.idempotencyComplete(MessageTypeRepoPull, requestID, fp, result)
		c.sendPullResultMsg(result)
	}

	// 3. Git operations
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.pull request")
		result := NewRepoPullResultMessage(requestID, false, "",
			apperrors.CodeServerHandlerMissing, "git operations not configured")
		finish(result)
		return
	}

	// 4. Repo mutation gate
	if !c.server.repoMutationMu.TryLock() {
		result := NewRepoPullResultMessage(requestID, false, "",
			apperrors.CodeActionInvalid, "repo mutation already in progress")
		finish(result)
		return
	}
	defer c.server.repoMutationMu.Unlock()

	// 5. Execute pull --ff-only
	output, pullErr := gitOps.PullFFOnly()
	if pullErr != nil {
		log.Printf("Pull failed: %v", pullErr)
		code, message := apperrors.ToCodeAndMessage(pullErr)
		result := NewRepoPullResultMessage(requestID, false, "", code, message)
		finish(result)
		return
	}

	log.Printf("Pull succeeded: device=%s", c.deviceID)
	result := NewRepoPullResultMessage(requestID, true, output, "", "")
	finish(result)
}
