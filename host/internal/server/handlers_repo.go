package server

import (
	"encoding/json"
	"log"
	"time"

	// Internal error codes package for standardized error handling.
	apperrors "github.com/pseudocoder/host/internal/errors"

	// Storage package provides card and session persistence.
	"github.com/pseudocoder/host/internal/storage"
)

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
	msg := NewRepoStatusMessage(status.Branch, status.Upstream, status.StagedCount, status.StagedFiles, status.UnstagedCount, status.LastCommit)
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
	// Parse the message payload
	var msg struct {
		Type    MessageType       `json:"type"`
		ID      string            `json:"id,omitempty"`
		Payload RepoCommitPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.commit message: %v", err)
		c.sendCommitResult(false, "", "", apperrors.CodeServerInvalidMessage, "invalid JSON")
		return
	}

	payload := msg.Payload

	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.commit request")
		c.sendCommitResult(false, "", "", apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Create the commit
	hash, summary, err := gitOps.Commit(payload.Message, payload.NoVerify, payload.NoGpgSign)
	if err != nil {
		log.Printf("Commit failed: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendCommitResult(false, "", "", code, message)
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
		acceptedCards, err := decidedStore.ListDecidedByStatus(storage.CardAccepted)
		if err != nil {
			log.Printf("repo.commit: failed to list accepted cards: %v", err)
		} else if len(acceptedCards) > 0 {
			// Collect card IDs to mark as committed
			cardIDs := make([]string, len(acceptedCards))
			for i, card := range acceptedCards {
				cardIDs[i] = card.ID
			}

			if err := decidedStore.MarkCardsCommitted(cardIDs, hash); err != nil {
				log.Printf("repo.commit: failed to mark cards as committed: %v", err)
			} else {
				log.Printf("repo.commit: marked %d cards as committed with hash %s", len(cardIDs), hash)
			}
		}

		// Mark any accepted chunks as committed.
		// IMPORTANT: We must iterate ALL decided cards, not just accepted ones.
		// For chunk-level decisions, the parent DecidedCard.Status reflects the
		// first chunk's decision, which may have been "rejected" even if later
		// chunks were accepted and staged. Those accepted chunks need to be
		// marked as committed regardless of the parent card's status.
		allCards, err := decidedStore.ListAllDecidedCards()
		if err != nil {
			log.Printf("repo.commit: failed to list all decided cards for chunk processing: %v", err)
		} else {
			for _, card := range allCards {
				chunks, err := decidedStore.GetDecidedChunks(card.ID)
				if err != nil {
					log.Printf("repo.commit: failed to get chunks for card %s: %v", card.ID, err)
					continue
				}

				// Collect accepted chunk indexes
				var acceptedIndexes []int
				for _, chunk := range chunks {
					if chunk.Status == storage.CardAccepted {
						acceptedIndexes = append(acceptedIndexes, chunk.ChunkIndex)
					}
				}

				if len(acceptedIndexes) > 0 {
					if err := decidedStore.MarkChunksCommitted(card.ID, acceptedIndexes, hash); err != nil {
						log.Printf("repo.commit: failed to mark chunks as committed for card %s: %v", card.ID, err)
					}
				}
			}
		}
	}

	// Broadcast success to all clients
	c.server.Broadcast(NewRepoCommitResultMessage(true, hash, summary, "", ""))
}

// handleRepoPush processes a repo.push message from the client.
// It pushes commits to the remote and returns the result.
func (c *Client) handleRepoPush(data []byte) {
	// Parse the message payload
	var msg struct {
		Type    MessageType     `json:"type"`
		ID      string          `json:"id,omitempty"`
		Payload RepoPushPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse repo.push message: %v", err)
		c.sendPushResult(false, "", apperrors.CodeServerInvalidMessage, "invalid JSON")
		return
	}

	payload := msg.Payload

	// Get git operations from server
	c.server.mu.RLock()
	gitOps := c.server.gitOps
	c.server.mu.RUnlock()

	if gitOps == nil {
		log.Printf("No git operations configured, ignoring repo.push request")
		c.sendPushResult(false, "", apperrors.CodeServerHandlerMissing, "git operations not configured")
		return
	}

	// Push to remote
	output, err := gitOps.Push(payload.Remote, payload.Branch, payload.ForceWithLease)
	if err != nil {
		log.Printf("Push failed: %v", err)
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendPushResult(false, "", code, message)
		return
	}

	log.Printf("Push succeeded: output=%s device=%s", output, c.deviceID)

	// Broadcast success to all clients
	c.server.Broadcast(NewRepoPushResultMessage(true, output, "", ""))
}
