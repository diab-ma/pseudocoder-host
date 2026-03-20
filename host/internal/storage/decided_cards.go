package storage

// decided_cards.go contains SQLiteStore methods for decided card CRUD.
// Decided cards are archived after user decisions and enable undo functionality
// by preserving patches and original diffs.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// SaveDecidedCard archives a card after a decision is made.
// Uses INSERT OR REPLACE to handle updates (e.g., marking as committed).
func (s *SQLiteStore) SaveDecidedCard(card *DecidedCard) error {
	if card == nil {
		return errors.New("decided card cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: saving decided card %s (file=%s, status=%s)", card.ID, card.File, card.Status)

	decidedAt := card.DecidedAt.Format(time.RFC3339Nano)
	var commitHash sql.NullString
	if card.CommitHash != "" {
		commitHash = sql.NullString{String: card.CommitHash, Valid: true}
	}
	var committedAt sql.NullString
	if card.CommittedAt != nil {
		committedAt = sql.NullString{String: card.CommittedAt.Format(time.RFC3339Nano), Valid: true}
	}

	const query = `
		INSERT OR REPLACE INTO decided_cards
			(id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query,
		card.ID,
		card.SessionID,
		card.File,
		card.Patch,
		string(card.Status),
		decidedAt,
		commitHash,
		committedAt,
		card.OriginalDiff,
	)
	if err != nil {
		return fmt.Errorf("save decided card: %w", err)
	}

	return nil
}

// GetDecidedCard retrieves an archived card by ID.
// Returns nil, nil if the card does not exist.
func (s *SQLiteStore) GetDecidedCard(id string) (*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE id = ?
	`

	row := s.db.QueryRow(query, id)
	card, err := s.scanDecidedCardRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get decided card: %w", err)
	}

	return card, nil
}

// ListDecidedByStatus returns all decided cards with the given status.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListDecidedByStatus(status CardStatus) ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE status = ?
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query, string(status))
	if err != nil {
		return nil, fmt.Errorf("query decided cards by status: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d decided cards with status %s", len(cards), status)
	return cards, nil
}

// ListAllDecidedCards returns all decided cards regardless of status.
// This is used for commit association to find all accepted cards.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListAllDecidedCards() ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query all decided cards: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d total decided cards", len(cards))
	return cards, nil
}

// ListDecidedByFile returns all decided cards for a given file path.
// Results are ordered by decided_at (newest first).
func (s *SQLiteStore) ListDecidedByFile(file string) ([]*DecidedCard, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const query = `
		SELECT id, session_id, file, patch, status, decided_at, commit_hash, committed_at, original_diff
		FROM decided_cards
		WHERE file = ?
		ORDER BY decided_at DESC
	`

	rows, err := s.db.Query(query, file)
	if err != nil {
		return nil, fmt.Errorf("query decided cards by file: %w", err)
	}
	defer rows.Close()

	var cards []*DecidedCard
	for rows.Next() {
		card, err := s.scanDecidedCardRows(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decided card rows: %w", err)
	}

	log.Printf("storage: listed %d decided cards for file %s", len(cards), file)
	return cards, nil
}

// MarkCardsCommitted updates accepted cards to committed status.
// This is called after a successful git commit to associate cards with their commit hash.
func (s *SQLiteStore) MarkCardsCommitted(cardIDs []string, commitHash string) error {
	if len(cardIDs) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: marking %d cards as committed (hash=%s)", len(cardIDs), commitHash)

	committedAt := time.Now().Format(time.RFC3339Nano)

	// Build query with placeholders for all card IDs
	placeholders := make([]string, len(cardIDs))
	args := make([]interface{}, len(cardIDs)+3)
	args[0] = string(CardCommitted)
	args[1] = commitHash
	args[2] = committedAt
	for i, id := range cardIDs {
		placeholders[i] = "?"
		args[i+3] = id
	}

	query := fmt.Sprintf(`
		UPDATE decided_cards
		SET status = ?, commit_hash = ?, committed_at = ?
		WHERE status = 'accepted' AND id IN (%s)
	`, strings.Join(placeholders, ", "))

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark cards committed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("storage: marked %d cards as committed", rowsAffected)

	return nil
}

// DeleteDecidedCard removes an archived card.
// This is called after a successful undo operation.
func (s *SQLiteStore) DeleteDecidedCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("storage: deleting decided card %s", id)

	_, err := s.db.Exec("DELETE FROM decided_cards WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete decided card: %w", err)
	}

	return nil
}

// scanDecidedCardRow scans a single row into a DecidedCard.
func (s *SQLiteStore) scanDecidedCardRow(row *sql.Row) (*DecidedCard, error) {
	var (
		card        DecidedCard
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := row.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
		&card.OriginalDiff,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	card.DecidedAt = t

	if commitHash.Valid {
		card.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		card.CommittedAt = &t
	}

	return &card, nil
}

// scanDecidedCardRows scans a row from sql.Rows into a DecidedCard.
func (s *SQLiteStore) scanDecidedCardRows(rows *sql.Rows) (*DecidedCard, error) {
	var (
		card        DecidedCard
		status      string
		decidedAt   string
		commitHash  sql.NullString
		committedAt sql.NullString
	)

	err := rows.Scan(
		&card.ID,
		&card.SessionID,
		&card.File,
		&card.Patch,
		&status,
		&decidedAt,
		&commitHash,
		&committedAt,
		&card.OriginalDiff,
	)
	if err != nil {
		return nil, err
	}

	card.Status = CardStatus(status)

	t, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return nil, fmt.Errorf("parse decided_at: %w", err)
	}
	card.DecidedAt = t

	if commitHash.Valid {
		card.CommitHash = commitHash.String
	}

	if committedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, committedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse committed_at: %w", err)
		}
		card.CommittedAt = &t
	}

	return &card, nil
}

