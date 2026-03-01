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
// Deprecated: Use sendUndoResultWithHash for chunk-level undos to enable hash-based pending tracking.
func (c *Client) sendUndoResult(cardID string, chunkIndex int, success bool, errCode, errMsg string) {
	c.sendUndoResultWithHash(cardID, chunkIndex, "", success, errCode, errMsg)
}

// sendUndoResultWithHash sends an undo result message with content hash to this client.
// For file-level undos, set chunkIndex to -1 (omitted from JSON).
// For chunk-level undos, include contentHash so clients can clear hash-keyed pending state.
// For failures, provide both errCode and errMsg. For success, both should be empty.
func (c *Client) sendUndoResultWithHash(cardID string, chunkIndex int, contentHash string, success bool, errCode, errMsg string) {
	// Use non-blocking send to avoid blocking on slow clients
	select {
	case c.send <- NewUndoResultMessageWithHash(cardID, chunkIndex, contentHash, success, errCode, errMsg):
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
func (c *Client) sendCommitResult(requestID string, success bool, hash, summary, errCode, errMsg string) {
	msg := NewRepoCommitResultMessage(requestID, success, hash, summary, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping commit result")
	}
}

// sendCommitResultMsg sends a pre-built repo.commit_result message to the client.
func (c *Client) sendCommitResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping commit result")
	}
}

// sendPushResult sends a repo.push_result message to the client.
func (c *Client) sendPushResult(requestID string, success bool, output, errCode, errMsg string) {
	msg := NewRepoPushResultMessage(requestID, success, output, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping push result")
	}
}

// sendPushResultMsg sends a pre-built repo.push_result message to the client.
func (c *Client) sendPushResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping push result")
	}
}

// sendFetchResult sends a repo.fetch_result message to the client.
func (c *Client) sendFetchResult(requestID string, success bool, output, errCode, errMsg string) {
	msg := NewRepoFetchResultMessage(requestID, success, output, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping fetch result")
	}
}

// sendFetchResultMsg sends a pre-built repo.fetch_result message to the client.
func (c *Client) sendFetchResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping fetch result")
	}
}

// sendPullResult sends a repo.pull_result message to the client.
func (c *Client) sendPullResult(requestID string, success bool, output, errCode, errMsg string) {
	msg := NewRepoPullResultMessage(requestID, success, output, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping pull result")
	}
}

// sendPullResultMsg sends a pre-built repo.pull_result message to the client.
func (c *Client) sendPullResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping pull result")
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

// sendFileListResult sends a file.list_result message to this client.
func (c *Client) sendFileListResult(msg Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping file.list_result")
	}
}

// sendFileReadResult sends a file.read_result message to this client.
func (c *Client) sendFileReadResult(msg Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping file.read_result")
	}
}

// sendFileWriteResult sends a file.write_result message to this client.
func (c *Client) sendFileWriteResult(msg Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping file.write_result")
	}
}

// sendFileCreateResult sends a file.create_result message to this client.
func (c *Client) sendFileCreateResult(msg Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping file.create_result")
	}
}

// sendFileDeleteResult sends a file.delete_result message to this client.
func (c *Client) sendFileDeleteResult(msg Message) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping file.delete_result")
	}
}

// sendHistoryResult sends a repo.history_result message to this client.
func (c *Client) sendHistoryResult(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.history_result")
	}
}

// sendBranchesResult sends a repo.branches_result message to this client.
func (c *Client) sendBranchesResult(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.branches_result")
	}
}

// sendBranchCreateResult sends a repo.branch_create_result message to this client.
func (c *Client) sendBranchCreateResult(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.branch_create_result")
	}
}

// sendBranchSwitchResult sends a repo.branch_switch_result message to this client.
func (c *Client) sendBranchSwitchResult(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.branch_switch_result")
	}
}

// sendPrListResult sends a repo.pr_list_result message to the client.
func (c *Client) sendPrListResult(requestID string, success bool, entries []RepoPrEntryPayload, errCode, errMsg string) {
	msg := NewRepoPrListResultMessage(requestID, success, entries, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_list_result")
	}
}

// sendPrListResultMsg sends a pre-built repo.pr_list_result message to the client.
func (c *Client) sendPrListResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_list_result")
	}
}

// sendPrViewResult sends a repo.pr_view_result message to the client.
func (c *Client) sendPrViewResult(requestID string, success bool, pr *RepoPrDetailPayload, errCode, errMsg string) {
	msg := NewRepoPrViewResultMessage(requestID, success, pr, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_view_result")
	}
}

// sendPrViewResultMsg sends a pre-built repo.pr_view_result message to the client.
func (c *Client) sendPrViewResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_view_result")
	}
}

// sendPrCreateResult sends a repo.pr_create_result message to the client.
func (c *Client) sendPrCreateResult(requestID string, success bool, pr *RepoPrDetailPayload, errCode, errMsg string) {
	msg := NewRepoPrCreateResultMessage(requestID, success, pr, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_create_result")
	}
}

// sendPrCreateResultMsg sends a pre-built repo.pr_create_result message to the client.
func (c *Client) sendPrCreateResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_create_result")
	}
}

// sendPrCheckoutResult sends a repo.pr_checkout_result message to the client.
func (c *Client) sendPrCheckoutResult(requestID string, success bool, branchName string, changedBranch bool, blockers []string, errCode, errMsg string) {
	msg := NewRepoPrCheckoutResultMessage(requestID, success, branchName, changedBranch, blockers, errCode, errMsg)
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_checkout_result")
	}
}

// sendPrCheckoutResultMsg sends a pre-built repo.pr_checkout_result message to the client.
func (c *Client) sendPrCheckoutResultMsg(msg Message) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping repo.pr_checkout_result")
	}
}
