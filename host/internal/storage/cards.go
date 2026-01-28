package storage

// cards.go contains SQLiteStore methods for review card CRUD operations.
// Review cards track pending diffs awaiting user decisions.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// SaveCard persists a review card to the database.
// Uses INSERT OR REPLACE to handle both new cards and updates.
func (s *SQLiteStore) SaveCard(card *ReviewCard) error {
	if card == nil {
		return errors.New("card cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving card %s (file=%s, status=%s)", card.ID, card.File, card.Status)

	// Convert times to RFC3339 format for storage.
	createdAt := card.CreatedAt.Format(time.RFC3339Nano)
	var decidedAt sql.NullString
	if card.DecidedAt != nil {
		decidedAt = sql.NullString{String: card.DecidedAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO review_cards
			(id, session_id, file, diff, status, created_at, decided_at, comment)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		card.ID,
		card.SessionID,
		card.File,
		card.Diff,
		string(card.Status),
		createdAt,
		decidedAt,
		card.Comment,
	)
	if err != nil {
		return fmt.Errorf("save card: %w", err)
	}

	return nil
}

// GetCard retrieves a card by ID.
// Returns nil, nil if the card does not exist.
func (s *SQLiteStore) GetCard(id string) (*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		WHERE id = ?
	`

	row := s.db.QueryRow(query, id)

	// Use a rowScanner adapter so we can reuse parseCardRow
	card, err := parseCardRow(rowAdapter{row})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get card: %w", err)
	}

	return card, nil
}

// ListPending returns all cards with status "pending".
// Results are ordered by creation time (oldest first) for FIFO processing.
func (s *SQLiteStore) ListPending() ([]*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		WHERE status = 'pending'
		ORDER BY created_at ASC
	`

	cards, err := s.queryCards(query)
	if err != nil {
		return nil, err
	}

	log.Printf("storage: listed %d pending cards", len(cards))
	return cards, nil
}

// ListAll returns all cards regardless of status.
// Results are ordered by creation time (oldest first).
func (s *SQLiteStore) ListAll() ([]*ReviewCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, diff, status, created_at, decided_at, comment
		FROM review_cards
		ORDER BY created_at ASC
	`

	cards, err := s.queryCards(query)
	if err != nil {
		return nil, err
	}

	log.Printf("storage: listed %d total cards", len(cards))
	return cards, nil
}

// RecordDecision updates a card's status and records the decision.
// Returns ErrCardNotFound if the card does not exist.
// Returns ErrAlreadyDecided if the card is not in pending status.
// This method is atomic - only one concurrent decision can succeed for a given card.
func (s *SQLiteStore) RecordDecision(decision *Decision) error {
	if decision == nil {
		return errors.New("decision cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: recording decision for card %s (status=%s)", decision.CardID, decision.Status)

	decidedAt := decision.Timestamp.Format(time.RFC3339Nano)

	// Atomic update: only updates if card exists AND is still pending.
	// This prevents race conditions where two concurrent decisions could
	// both check status, both apply git operations, then both update storage.
	const query = `
		UPDATE review_cards
		SET status = ?, decided_at = ?, comment = ?
		WHERE id = ? AND status = 'pending'
	`

	result, err := s.db.Exec(query, string(decision.Status), decidedAt, decision.Comment, decision.CardID)
	if err != nil {
		return fmt.Errorf("record decision: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		// No rows updated - either card doesn't exist or was already decided.
		// Check which case it is for a more specific error.
		var status string
		err := s.db.QueryRow("SELECT status FROM review_cards WHERE id = ?", decision.CardID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return fmt.Errorf("check card status: %w", err)
		}
		// Card exists but wasn't pending
		return ErrAlreadyDecided
	}

	return nil
}

// recordDecisionTx updates a card's status inside an existing transaction.
// The caller must hold the store mutex via WithTransaction.
func (s *SQLiteStore) recordDecisionTx(tx *sql.Tx, decision *Decision) error {
	if decision == nil {
		return errors.New("decision cannot be nil")
	}

	log.Printf("storage: recording decision (tx) for card %s (status=%s)", decision.CardID, decision.Status)

	decidedAt := decision.Timestamp.Format(time.RFC3339Nano)
	const query = `
		UPDATE review_cards
		SET status = ?, decided_at = ?, comment = ?
		WHERE id = ? AND status = 'pending'
	`

	result, err := tx.Exec(query, string(decision.Status), decidedAt, decision.Comment, decision.CardID)
	if err != nil {
		return fmt.Errorf("record decision: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		var status string
		err := tx.QueryRow("SELECT status FROM review_cards WHERE id = ?", decision.CardID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return fmt.Errorf("check card status: %w", err)
		}
		return ErrAlreadyDecided
	}

	return nil
}

// deleteCardTx removes a card inside an existing transaction.
// The caller must hold the store mutex via WithTransaction.
func (s *SQLiteStore) deleteCardTx(tx *sql.Tx, id string) error {
	log.Printf("storage: deleting card (tx) %s", id)

	_, err := tx.Exec("DELETE FROM review_cards WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete card: %w", err)
	}

	return nil
}

// DeleteCard removes a card from storage.
// Returns nil if the card does not exist (idempotent delete).
func (s *SQLiteStore) DeleteCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting card %s", id)

	_, err := s.db.Exec("DELETE FROM review_cards WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete card: %w", err)
	}

	return nil
}

// queryCards executes a query and returns all matching cards.
func (s *SQLiteStore) queryCards(query string, args ...interface{}) ([]*ReviewCard, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query cards: %w", err)
	}
	defer rows.Close()

	var cards []*ReviewCard
	for rows.Next() {
		// Use rowsAdapter so we can reuse parseCardRow
		card, err := parseCardRow(rowsAdapter{rows})
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return cards, nil
}

// scanner is an interface that abstracts over sql.Row and sql.Rows.
// This allows us to use a single parseCardRow function for both cases,
// eliminating code duplication (DRY principle).
type scanner interface {
	Scan(dest ...interface{}) error
}

// rowAdapter wraps *sql.Row to implement the scanner interface.
type rowAdapter struct {
	row *sql.Row
}

func (r rowAdapter) Scan(dest ...interface{}) error {
	return r.row.Scan(dest...)
}

// rowsAdapter wraps *sql.Rows to implement the scanner interface.
type rowsAdapter struct {
	rows *sql.Rows
}

func (r rowsAdapter) Scan(dest ...interface{}) error {
	return r.rows.Scan(dest...)
}

// parseCardRow scans a database row into a ReviewCard.
// Works with both *sql.Row and *sql.Rows through the scanner interface.
func parseCardRow(s scanner) (*ReviewCard, error) {
	var (
		card      ReviewCard
		status    string
		createdAt string
		decidedAt sql.NullString
	)

	err := s.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Diff,
		&status,
		&createdAt,
		&decidedAt,
		&card.Comment,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	// Parse timestamps from RFC3339 format.
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	card.CreatedAt = t

	if decidedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse decided_at: %w", err)
		}
		card.DecidedAt = &t
	}

	return &card, nil
}
