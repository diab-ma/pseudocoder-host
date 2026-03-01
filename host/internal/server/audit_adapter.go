// Package server provides the WebSocket server for client connections.
// This file implements audit store adapters that bridge the storage
// and server packages for approval and keep-awake audit logging.
package server

import (
	"log"

	"github.com/pseudocoder/host/internal/storage"
)

// AuditStoreAdapter adapts SQLiteStore to the ApprovalAuditStore interface.
// This is necessary because the server and storage packages define their own
// ApprovalAuditEntry types to avoid import cycles.
type AuditStoreAdapter struct {
	store *storage.SQLiteStore
}

// NewAuditStoreAdapter creates a new adapter wrapping the given SQLite store.
func NewAuditStoreAdapter(store *storage.SQLiteStore) *AuditStoreAdapter {
	return &AuditStoreAdapter{store: store}
}

// SaveApprovalAudit converts server.ApprovalAuditEntry to storage.ApprovalAuditEntry
// and persists it to the database.
func (a *AuditStoreAdapter) SaveApprovalAudit(entry *ApprovalAuditEntry) error {
	// Convert from server type to storage type
	storageEntry := &storage.ApprovalAuditEntry{
		ID:        entry.ID,
		RequestID: entry.RequestID,
		Command:   entry.Command,
		Cwd:       entry.Cwd,
		Repo:      entry.Repo,
		Rationale: entry.Rationale,
		Decision:  entry.Decision,
		DecidedAt: entry.DecidedAt,
		ExpiresAt: entry.ExpiresAt,
		DeviceID:  entry.DeviceID,
		Source:    entry.Source,
	}

	return a.store.SaveApprovalAudit(storageEntry)
}

// KeepAwakeAuditStoreAdapter wraps SQLiteStore to implement KeepAwakeAuditWriter
// with durable persistence and log observability.
type KeepAwakeAuditStoreAdapter struct {
	store   *storage.SQLiteStore
	maxRows int
}

// NewKeepAwakeAuditStoreAdapter creates a durable keep-awake audit adapter.
func NewKeepAwakeAuditStoreAdapter(store *storage.SQLiteStore, maxRows int) *KeepAwakeAuditStoreAdapter {
	return &KeepAwakeAuditStoreAdapter{store: store, maxRows: maxRows}
}

// WriteKeepAwakeAudit persists the event to SQLite and logs for observability.
func (a *KeepAwakeAuditStoreAdapter) WriteKeepAwakeAudit(event KeepAwakeAuditEvent) error {
	entry := &storage.KeepAwakeAuditEntry{
		Operation:      event.Operation,
		RequestID:      event.RequestID,
		ActorDeviceID:  event.ActorDeviceID,
		TargetDeviceID: event.TargetDeviceID,
		SessionID:      event.SessionID,
		LeaseID:        event.LeaseID,
		Reason:         event.Reason,
		At:             event.At,
	}
	err := a.store.SaveAndPruneKeepAwakeAudit(entry, a.maxRows)
	if err != nil {
		log.Printf("keep-awake audit: durable write failed: %v", err)
		return err
	}
	log.Printf(
		"keep-awake audit: op=%s request_id=%s actor=%s target=%s session=%s lease=%s reason=%s",
		event.Operation, event.RequestID, event.ActorDeviceID,
		event.TargetDeviceID, event.SessionID, event.LeaseID, event.Reason,
	)
	return nil
}
