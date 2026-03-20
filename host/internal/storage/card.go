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
	// This is used for commit association to find all accepted cards.
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

	// IsSystem marks host-managed internal sessions that should be hidden from
	// user-facing session lists in mobile UI.
	IsSystem bool `json:"is_system,omitempty"`

	// SessionKind stores the canonical lowercase session classification string.
	// Empty means legacy rows that predate structured-chat metadata.
	SessionKind string `json:"session_kind,omitempty"`

	// AgentProvider stores the canonical lowercase provider string.
	// Empty means no persisted provider metadata is available.
	AgentProvider string `json:"agent_provider,omitempty"`

	// ChatCapabilitiesJSON stores the canonical capabilities JSON object.
	// Empty means the session has no persisted structured-chat capabilities.
	ChatCapabilitiesJSON string `json:"chat_capabilities_json,omitempty"`
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

	// ClearArchivedSessions deletes archived sessions from history storage.
	// Archived means status is "complete" or "error".
	// Returns the number of deleted rows.
	ClearArchivedSessions() (int, error)

	// DeleteSession deletes a single session by ID, including associated
	// structured chat data. Returns an error if the session does not exist.
	DeleteSession(id string) error

	// ClearAllSessions deletes all non-system sessions from storage,
	// regardless of status. Returns the number of deleted rows.
	ClearAllSessions() (int, error)
}

// StructuredChatStoredItem is one persisted structured-chat item payload.
// PayloadJSON stores the canonical wire JSON so replay stays stable across restarts.
type StructuredChatStoredItem struct {
	// ItemID is the canonical chat item id.
	ItemID string `json:"item_id"`

	// Position preserves authoritative snapshot ordering.
	Position int `json:"position"`

	// PayloadJSON is the serialized canonical ChatItem payload.
	PayloadJSON string `json:"payload_json"`

	// CreatedAt mirrors the item's created_at for deterministic ordering checks.
	CreatedAt time.Time `json:"created_at"`
}

// StructuredChatSnapshot is the durable snapshot state for one session.
type StructuredChatSnapshot struct {
	// SessionID identifies which session owns the snapshot.
	SessionID string `json:"session_id"`

	// Revision is the persisted monotonic revision for this session.
	Revision int64 `json:"revision"`

	// UpdatedAt is when the snapshot was last replaced.
	UpdatedAt time.Time `json:"updated_at"`

	// Items is the ordered retained snapshot window.
	Items []StructuredChatStoredItem `json:"items"`
}

// StructuredChatStore defines persistence for host-authored structured chat snapshots.
type StructuredChatStore interface {
	// LoadStructuredChatSnapshot returns the stored snapshot for one session.
	// Returns nil, nil when no snapshot exists.
	LoadStructuredChatSnapshot(sessionID string) (*StructuredChatSnapshot, error)

	// ReplaceStructuredChatSnapshot atomically replaces the stored snapshot for one session.
	ReplaceStructuredChatSnapshot(snapshot *StructuredChatSnapshot) error
}
