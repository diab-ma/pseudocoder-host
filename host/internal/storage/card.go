// Package storage provides SQLite persistence for review cards and decisions.
// It handles saving and loading review cards across host restarts, ensuring
// pending cards survive application lifecycle events.
package storage

import "time"

// CardStatus represents the current state of a review card.
// Cards start as Pending and transition to Accepted or Rejected
// when the user makes a decision.
type CardStatus string

const (
	// CardPending means the card awaits user review.
	CardPending CardStatus = "pending"

	// CardAccepted means the user approved the change.
	CardAccepted CardStatus = "accepted"

	// CardRejected means the user rejected the change.
	CardRejected CardStatus = "rejected"

	// CardCommitted means the accepted change was committed to git.
	// This status is used for undo tracking - cards transition from
	// accepted to committed when their changes are included in a commit.
	CardCommitted CardStatus = "committed"
)

// ReviewCard represents a diff chunk that requires user review.
// Each card corresponds to a single chunk from a git diff and tracks
// the user's decision once made.
type ReviewCard struct {
	// ID is the unique identifier for this card, derived from chunk content.
	ID string `json:"id"`

	// SessionID identifies which session this card belongs to.
	// Empty string means no session association (standalone).
	SessionID string `json:"session_id"`

	// File is the path to the file that was changed.
	File string `json:"file"`

	// Diff contains the raw diff content including context and +/- lines.
	Diff string `json:"diff"`

	// Status is the current state of the card (pending, accepted, rejected).
	Status CardStatus `json:"status"`

	// CreatedAt is when the card was first detected.
	CreatedAt time.Time `json:"created_at"`

	// DecidedAt is when the user made a decision (nil if pending).
	DecidedAt *time.Time `json:"decided_at,omitempty"`

	// Comment is an optional user comment on the decision.
	Comment string `json:"comment,omitempty"`
}

// Decision represents a user's action on a review card.
type Decision struct {
	// CardID is the ID of the card this decision applies to.
	CardID string `json:"card_id"`

	// Status is whether the change was accepted or rejected.
	Status CardStatus `json:"status"`

	// Comment is an optional explanation for the decision.
	Comment string `json:"comment,omitempty"`

	// Timestamp is when the decision was made.
	Timestamp time.Time `json:"timestamp"`
}

// CardStore defines the interface for persisting review cards.
// Implementations must be safe for concurrent access.
type CardStore interface {
	// SaveCard persists a review card to storage.
	// If a card with the same ID exists, it is updated.
	SaveCard(card *ReviewCard) error

	// GetCard retrieves a card by ID.
	// Returns nil, nil if the card does not exist.
	GetCard(id string) (*ReviewCard, error)

	// ListPending returns all cards with status "pending".
	ListPending() ([]*ReviewCard, error)

	// ListAll returns all cards regardless of status.
	ListAll() ([]*ReviewCard, error)

	// RecordDecision updates a card's status and records the decision.
	// Returns an error if the card does not exist.
	RecordDecision(decision *Decision) error

	// DeleteCard removes a card from storage.
	// Returns nil if the card does not exist.
	DeleteCard(id string) error

	// Close releases any resources held by the store.
	Close() error
}

// ChunkStatus tracks the decision status of an individual chunk within a file card.
// This enables per-chunk accept/reject decisions while the parent card tracks
// overall file status.
type ChunkStatus struct {
	// CardID is the ID of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based position of this chunk within the card's diff.
	ChunkIndex int `json:"chunk_index"`

	// Content is the raw chunk content including the @@ header.
	// This is needed to construct the patch for git apply.
	Content string `json:"content"`

	// Status is the current state (pending, accepted, rejected).
	Status CardStatus `json:"status"`

	// DecidedAt is when the decision was made (nil if pending).
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// ChunkDecision represents a user's action on a single chunk.
type ChunkDecision struct {
	// CardID is the ID of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based position of this chunk.
	ChunkIndex int `json:"chunk_index"`

	// Status is whether the chunk was accepted or rejected.
	Status CardStatus `json:"status"`

	// Timestamp is when the decision was made.
	Timestamp time.Time `json:"timestamp"`
}

// ChunkStore defines the interface for persisting per-chunk decisions.
// This extends CardStore for granular chunk-level control.
type ChunkStore interface {
	// SaveChunks persists all chunks for a card.
	// Replaces any existing chunks for that card.
	SaveChunks(cardID string, chunks []*ChunkStatus) error

	// GetChunks retrieves all chunks for a card.
	// Returns empty slice if no chunks exist.
	GetChunks(cardID string) ([]*ChunkStatus, error)

	// GetChunk retrieves a specific chunk by card ID and index.
	// Returns nil, nil if the chunk does not exist.
	GetChunk(cardID string, chunkIndex int) (*ChunkStatus, error)

	// RecordChunkDecision updates a chunk's status.
	// Returns ErrCardNotFound if the chunk does not exist.
	// Returns ErrAlreadyDecided if the chunk is not pending.
	RecordChunkDecision(decision *ChunkDecision) error

	// DeleteChunks removes all chunks for a card.
	DeleteChunks(cardID string) error

	// CountPendingChunks returns the number of pending chunks for a card.
	CountPendingChunks(cardID string) (int, error)
}

// -----------------------------------------------------------------------------
// Decided Card Storage (Undo Support)
// -----------------------------------------------------------------------------

// DecidedCard represents an archived card after a decision is made.
// These cards are retained for undo functionality - they store the patch
// that was applied at decision time so it can be reversed if needed.
// Cards transition through: pending -> accepted/rejected -> committed (for accepts).
type DecidedCard struct {
	// ID is the unique identifier, same as the original card ID.
	ID string `json:"id"`

	// SessionID identifies which session this card belonged to.
	SessionID string `json:"session_id"`

	// File is the path to the file that was changed.
	File string `json:"file"`

	// Patch is the git patch that was applied at decision time.
	// For accept: the patch that was staged (can be reverse-applied to undo).
	// For reject: the patch that was discarded (can be forward-applied to undo).
	Patch string `json:"patch"`

	// Status is the current state (accepted, rejected, or committed).
	Status CardStatus `json:"status"`

	// DecidedAt is when the user made the decision.
	DecidedAt time.Time `json:"decided_at"`

	// CommitHash is the git commit hash if this card was committed.
	// Only populated when Status is CardCommitted.
	CommitHash string `json:"commit_hash,omitempty"`

	// CommittedAt is when the card was committed (nil if not committed).
	CommittedAt *time.Time `json:"committed_at,omitempty"`

	// OriginalDiff is the full diff content for re-review after undo.
	// This preserves the original card content so it can be re-sent to mobile.
	OriginalDiff string `json:"original_diff"`
}

// DecidedChunk represents an archived chunk after a per-chunk decision.
// Similar to DecidedCard but for individual chunks within a file.
type DecidedChunk struct {
	// CardID is the ID of the parent file card.
	CardID string `json:"card_id"`

	// ChunkIndex is the zero-based position of this chunk.
	ChunkIndex int `json:"chunk_index"`

	// Patch is the git patch for this chunk at decision time.
	Patch string `json:"patch"`

	// Status is the current state (accepted, rejected, or committed).
	Status CardStatus `json:"status"`

	// DecidedAt is when the decision was made.
	DecidedAt time.Time `json:"decided_at"`

	// CommitHash is the git commit hash if this chunk was committed.
	CommitHash string `json:"commit_hash,omitempty"`

	// CommittedAt is when the chunk was committed (nil if not committed).
	CommittedAt *time.Time `json:"committed_at,omitempty"`
}

// DecidedCardStore defines the interface for archiving decided cards.
// This enables undo functionality by preserving decision history and patches.
// Implementations must be safe for concurrent access.
type DecidedCardStore interface {
	// SaveDecidedCard archives a card after a decision is made.
	// This is called when a card transitions from pending to accepted/rejected.
	SaveDecidedCard(card *DecidedCard) error

	// GetDecidedCard retrieves an archived card by ID.
	// Returns nil, nil if the card does not exist.
	GetDecidedCard(id string) (*DecidedCard, error)

	// ListDecidedByStatus returns all decided cards with the given status.
	// Results are ordered by decided_at (newest first).
	ListDecidedByStatus(status CardStatus) ([]*DecidedCard, error)

	// ListAllDecidedCards returns all decided cards regardless of status.
	// This is used for commit association to find all cards with accepted chunks.
	// Results are ordered by decided_at (newest first).
	ListAllDecidedCards() ([]*DecidedCard, error)

	// ListDecidedByFile returns all decided cards for a given file path.
	// Results are ordered by decided_at (newest first).
	ListDecidedByFile(file string) ([]*DecidedCard, error)

	// MarkCardsCommitted updates accepted cards to committed status.
	// This is called after a successful git commit to associate cards
	// with their commit hash.
	MarkCardsCommitted(cardIDs []string, commitHash string) error

	// DeleteDecidedCard removes an archived card.
	// This is called after a successful undo operation.
	DeleteDecidedCard(id string) error

	// SaveDecidedChunk archives a chunk after a per-chunk decision.
	SaveDecidedChunk(chunk *DecidedChunk) error

	// GetDecidedChunk retrieves an archived chunk by card ID and index.
	// Returns nil, nil if the chunk does not exist.
	GetDecidedChunk(cardID string, chunkIndex int) (*DecidedChunk, error)

	// GetDecidedChunks retrieves all archived chunks for a card.
	// Results are ordered by chunk_index.
	GetDecidedChunks(cardID string) ([]*DecidedChunk, error)

	// MarkChunksCommitted updates accepted chunks to committed status.
	MarkChunksCommitted(cardID string, chunkIndexes []int, commitHash string) error

	// DeleteDecidedChunk removes a single archived chunk.
	// This is used when undoing a single chunk decision.
	DeleteDecidedChunk(cardID string, chunkIndex int) error

	// DeleteDecidedChunks removes all archived chunks for a card.
	DeleteDecidedChunks(cardID string) error
}

// -----------------------------------------------------------------------------
// Session Storage
// -----------------------------------------------------------------------------

// SessionStatus represents the state of a terminal session.
type SessionStatus string

const (
	// SessionStatusRunning means the session is currently active.
	SessionStatusRunning SessionStatus = "running"

	// SessionStatusComplete means the session ended normally.
	SessionStatusComplete SessionStatus = "complete"

	// SessionStatusError means the session ended with an error.
	SessionStatusError SessionStatus = "error"
)

// Session represents a terminal session for workflow tracking.
// Sessions are created when the host starts and updated as work progresses.
// They enable mobile clients to see session history and switch contexts.
type Session struct {
	// ID is the unique identifier for this session (e.g., "session-1234567890").
	ID string `json:"id"`

	// Repo is the repository path where this session runs.
	Repo string `json:"repo"`

	// Branch is the git branch name when the session started.
	Branch string `json:"branch"`

	// StartedAt is when the session began.
	StartedAt time.Time `json:"started_at"`

	// LastSeen is the most recent activity timestamp.
	LastSeen time.Time `json:"last_seen"`

	// LastCommit is the most recent commit hash (optional, updated on commits).
	LastCommit string `json:"last_commit,omitempty"`

	// Status is the current state of the session (running, complete, error).
	Status SessionStatus `json:"status"`
}

// SessionStore defines the interface for persisting session history.
// Implementations must be safe for concurrent access and enforce retention.
type SessionStore interface {
	// SaveSession persists a session to storage.
	// If a session with the same ID exists, it is updated.
	// Enforces retention: keeps only the most recent N sessions.
	SaveSession(session *Session) error

	// GetSession retrieves a session by ID.
	// Returns nil, nil if the session does not exist.
	GetSession(id string) (*Session, error)

	// ListSessions returns recent sessions ordered by started_at (newest first).
	// The limit parameter controls how many sessions to return (0 = default limit).
	ListSessions(limit int) ([]*Session, error)

	// UpdateSessionLastSeen updates the last_seen timestamp for a session.
	UpdateSessionLastSeen(id string) error

	// UpdateSessionStatus updates the status field for a session.
	UpdateSessionStatus(id string, status SessionStatus) error
}
