package server

import (
	"log"
	"time"
)

// trySend sends a message non-blocking, dropping it if the client buffer is full.
func (c *Client) trySend(msg Message, label string) {
	select {
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping %s", label)
	}
}

// trySendChecked sends a message non-blocking, but first checks if the client is done.
func (c *Client) trySendChecked(msg Message, label string) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	default:
		log.Printf("Warning: client send buffer full, dropping %s", label)
	}
}

// trySendTimeout sends a message with a timeout, checking if the client is done.
func (c *Client) trySendTimeout(msg Message, label string, d time.Duration) {
	select {
	case <-c.done:
		return
	case c.send <- msg:
	case <-time.After(d):
		log.Printf("Warning: timeout sending %s to client", label)
	}
}

// --- Pattern A: simple non-blocking sends ---

func (c *Client) sendDecisionResult(cardID, action string, success bool, errCode, errMsg string) {
	c.trySend(NewDecisionResultMessage(cardID, action, success, errCode, errMsg), "decision result")
}

func (c *Client) sendDeleteResult(cardID string, success bool, errCode, errMsg string) {
	c.trySend(NewDeleteResultMessage(cardID, success, errCode, errMsg), "delete result")
}

func (c *Client) sendUndoResultWithHash(cardID string, chunkIndex int, contentHash string, success bool, errCode, errMsg string) {
	c.trySend(NewUndoResultMessageWithHash(cardID, chunkIndex, contentHash, success, errCode, errMsg), "undo result")
}

func (c *Client) sendError(code, message string) {
	c.trySend(NewErrorMessage(code, message), "error message")
}

func (c *Client) sendFileListResult(msg Message)   { c.trySend(msg, "file.list_result") }
func (c *Client) sendFileReadResult(msg Message)   { c.trySend(msg, "file.read_result") }
func (c *Client) sendFileWriteResult(msg Message)  { c.trySend(msg, "file.write_result") }
func (c *Client) sendFileCreateResult(msg Message) { c.trySend(msg, "file.create_result") }
func (c *Client) sendFileDeleteResult(msg Message) { c.trySend(msg, "file.delete_result") }

// --- Pattern B: checked sends (respect c.done) ---

func (c *Client) sendCommitResult(requestID string, success bool, hash, summary, errCode, errMsg string) {
	c.trySendChecked(NewRepoCommitResultMessage(requestID, success, hash, summary, errCode, errMsg), "commit result")
}

func (c *Client) sendCommitResultMsg(msg Message) { c.trySendChecked(msg, "commit result") }

func (c *Client) sendPushResult(requestID string, success bool, output, errCode, errMsg string) {
	c.trySendChecked(NewRepoPushResultMessage(requestID, success, output, errCode, errMsg), "push result")
}

func (c *Client) sendPushResultMsg(msg Message) { c.trySendChecked(msg, "push result") }

func (c *Client) sendFetchResult(requestID string, success bool, output, errCode, errMsg string) {
	c.trySendChecked(NewRepoFetchResultMessage(requestID, success, output, errCode, errMsg), "fetch result")
}

func (c *Client) sendFetchResultMsg(msg Message) { c.trySendChecked(msg, "fetch result") }

func (c *Client) sendPullResult(requestID string, success bool, output, errCode, errMsg string) {
	c.trySendChecked(NewRepoPullResultMessage(requestID, success, output, errCode, errMsg), "pull result")
}

func (c *Client) sendPullResultMsg(msg Message) { c.trySendChecked(msg, "pull result") }

func (c *Client) sendHistoryResult(msg Message)      { c.trySendChecked(msg, "repo.history_result") }
func (c *Client) sendBranchesResult(msg Message)     { c.trySendChecked(msg, "repo.branches_result") }
func (c *Client) sendBranchCreateResult(msg Message) { c.trySendChecked(msg, "repo.branch_create_result") }
func (c *Client) sendBranchSwitchResult(msg Message) { c.trySendChecked(msg, "repo.branch_switch_result") }

func (c *Client) sendPrListResult(requestID string, success bool, entries []RepoPrEntryPayload, errCode, errMsg string) {
	c.trySendChecked(NewRepoPrListResultMessage(requestID, success, entries, errCode, errMsg), "repo.pr_list_result")
}

func (c *Client) sendPrListResultMsg(msg Message) { c.trySendChecked(msg, "repo.pr_list_result") }

func (c *Client) sendPrViewResult(requestID string, success bool, pr *RepoPrDetailPayload, errCode, errMsg string) {
	c.trySendChecked(NewRepoPrViewResultMessage(requestID, success, pr, errCode, errMsg), "repo.pr_view_result")
}

func (c *Client) sendPrViewResultMsg(msg Message) { c.trySendChecked(msg, "repo.pr_view_result") }

func (c *Client) sendPrCreateResult(requestID string, success bool, pr *RepoPrDetailPayload, errCode, errMsg string) {
	c.trySendChecked(NewRepoPrCreateResultMessage(requestID, success, pr, errCode, errMsg), "repo.pr_create_result")
}

func (c *Client) sendPrCreateResultMsg(msg Message) { c.trySendChecked(msg, "repo.pr_create_result") }

func (c *Client) sendPrCheckoutResult(requestID string, success bool, branchName string, changedBranch bool, blockers []string, errCode, errMsg string) {
	c.trySendChecked(NewRepoPrCheckoutResultMessage(requestID, success, branchName, changedBranch, blockers, errCode, errMsg), "repo.pr_checkout_result")
}

func (c *Client) sendPrCheckoutResultMsg(msg Message) { c.trySendChecked(msg, "repo.pr_checkout_result") }

// --- Pattern C: timeout sends ---

func (c *Client) sendTmuxSessions(msg Message) {
	c.trySendTimeout(msg, "tmux.sessions", 5*time.Second)
}
