package storage

// chunks.go contains SQLiteStore methods for chunk CRUD operations.
// Chunks are individual diff hunks within a file card that can be
// accepted or rejected independently.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// SaveChunks persists all chunks for a card.
// This replaces any existing chunks for the card (delete + insert).
// Use this when saving or updating a file card with its parsed chunks.
func (s *SQLiteStore) SaveChunks(cardID string, chunks []*ChunkStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use a transaction to ensure atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() // No-op if committed

	// Delete existing chunks for this card
	if _, err := tx.Exec("DELETE FROM card_chunks WHERE card_id = ?", cardID); err != nil {
		return fmt.Errorf("delete existing chunks: %w", err)
	}

	// Insert new chunks
	const insertQuery = `
		INSERT INTO card_chunks (card_id, chunk_index, content, status, decided_at)
		VALUES (?, ?, ?, ?, ?)
	`
	for _, h := range chunks {
		var decidedAt sql.NullString
		if h.DecidedAt != nil {
			decidedAt = sql.NullString{String: h.DecidedAt.Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := tx.Exec(insertQuery, cardID, h.ChunkIndex, h.Content, string(h.Status), decidedAt); err != nil {
			return fmt.Errorf("insert chunk %d: %w", h.ChunkIndex, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	log.Printf("storage: saved %d chunks for card %s", len(chunks), cardID)
	return nil
}

// GetChunks retrieves all chunks for a card, ordered by chunk_index.
// Returns empty slice if no chunks exist.
func (s *SQLiteStore) GetChunks(cardID string) ([]*ChunkStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, content, status, decided_at
		FROM card_chunks
		WHERE card_id = ?
		ORDER BY chunk_index ASC
	`

	rows, err := s.db.Query(query, cardID)
	if err != nil {
		return nil, fmt.Errorf("query chunks: %w", err)
	}
	defer rows.Close()

	var chunks []*ChunkStatus
	for rows.Next() {
		h, err := s.scanChunkRows(rows)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunk rows: %w", err)
	}

	return chunks, nil
}

// GetChunk retrieves a specific chunk by card ID and index.
// Returns nil, nil if the chunk does not exist.
func (s *SQLiteStore) GetChunk(cardID string, chunkIndex int) (*ChunkStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT card_id, chunk_index, content, status, decided_at
		FROM card_chunks
		WHERE card_id = ? AND chunk_index = ?
	`

	row := s.db.QueryRow(query, cardID, chunkIndex)
	h, err := s.scanChunkRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get chunk: %w", err)
	}

	return h, nil
}

// RecordChunkDecision updates a chunk's status.
// Returns ErrChunkNotFound if the chunk does not exist.
// Returns ErrAlreadyDecided if the chunk is not pending.
func (s *SQLiteStore) RecordChunkDecision(decision *ChunkDecision) error {
	if decision == nil {
		return errors.New("decision cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: recording chunk decision for card %s chunk %d (status=%s)",
		decision.CardID, decision.ChunkIndex, decision.Status)

	decidedAt := decision.Timestamp.Format(time.RFC3339Nano)

	// Atomic update: only updates if chunk exists AND is still pending
	const query = `
		UPDATE card_chunks
		SET status = ?, decided_at = ?
		WHERE card_id = ? AND chunk_index = ? AND status = 'pending'
	`

	result, err := s.db.Exec(query, string(decision.Status), decidedAt, decision.CardID, decision.ChunkIndex)
	if err != nil {
		return fmt.Errorf("record chunk decision: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		// No rows updated - check which error case
		var status string
		err := s.db.QueryRow(
			"SELECT status FROM card_chunks WHERE card_id = ? AND chunk_index = ?",
			decision.CardID, decision.ChunkIndex,
		).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChunkNotFound
		}
		if err != nil {
			return fmt.Errorf("check chunk status: %w", err)
		}
		// Chunk exists but wasn't pending
		return ErrAlreadyDecided
	}

	return nil
}

// DeleteChunks removes all chunks for a card.
// Returns nil if no chunks exist (idempotent).
func (s *SQLiteStore) DeleteChunks(cardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting chunks for card %s", cardID)

	_, err := s.db.Exec("DELETE FROM card_chunks WHERE card_id = ?", cardID)
	if err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}

	return nil
}

// CountPendingChunks returns the number of pending chunks for a card.
func (s *SQLiteStore) CountPendingChunks(cardID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM card_chunks WHERE card_id = ? AND status = 'pending'",
		cardID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending chunks: %w", err)
	}

	return count, nil
}

// scanChunkRow scans a single row into a ChunkStatus.
func (s *SQLiteStore) scanChunkRow(row *sql.Row) (*ChunkStatus, error) {
	var (
		h         ChunkStatus
		status    string
		decidedAt sql.NullString
	)

	err := row.Scan(
		&h.CardID,
		&h.ChunkIndex,
		&h.Content,
		&status,
		&decidedAt,
	)
	if err != nil {
		return nil, err
	}

	h.Status = CardStatus(status)

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		h.DecidedAt = &t
	}

	return &h, nil
}

// scanChunkRows scans a row from sql.Rows into a ChunkStatus.
func (s *SQLiteStore) scanChunkRows(rows *sql.Rows) (*ChunkStatus, error) {
	var (
		h         ChunkStatus
		status    string
		decidedAt sql.NullString
	)

	err := rows.Scan(
		&h.CardID,
		&h.ChunkIndex,
		&h.Content,
		&status,
		&decidedAt,
	)
	if err != nil {
		return nil, err
	}

	h.Status = CardStatus(status)

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		h.DecidedAt = &t
	}

	return &h, nil
}
