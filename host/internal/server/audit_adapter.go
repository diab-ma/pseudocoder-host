// Package server provides the WebSocket server for client connections.
// This file implements the audit store adapter that bridges the storage
// and server packages for approval audit logging.
package server

import (
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
