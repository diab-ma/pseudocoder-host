// Package pty provides PTY session management for running commands in
// pseudo-terminals. This package handles the creation, lifecycle, and
// multiplexing of terminal sessions.
package pty

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound is returned when attempting to access a session
// that doesn't exist in the manager.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionExists is returned when attempting to create a session
// with an ID that already exists.
var ErrSessionExists = errors.New("session already exists")

// ErrMaxSessionsReached is returned when the session limit has been reached.
var ErrMaxSessionsReached = errors.New("maximum number of sessions reached")

// DefaultMaxSessions is the default maximum number of concurrent sessions.
// This prevents resource exhaustion from creating too many PTY sessions.
const DefaultMaxSessions = 20

// SessionInfo contains metadata about a session for reporting purposes.
// This is returned by List() to provide information without exposing
// the full Session object.
type SessionInfo struct {
	// ID is the unique session identifier.
	ID string

	// Name is a human-readable name for the session.
	// May be empty if not set.
	Name string

	// Command is the command running in this session.
	Command string

	// Args are the command arguments.
	Args []string

	// Running indicates whether the session is currently executing.
	Running bool

	// CreatedAt is when the session was started.
	CreatedAt time.Time

	// TmuxSession is the tmux session name if this is a tmux session.
	// Empty string for regular PTY sessions.
	TmuxSession string
}

// SessionManager manages multiple concurrent PTY sessions.
//
// It provides a thread-safe way to create, access, and close sessions.
// Each session is identified by a unique ID (typically a UUID).
//
// Example usage:
//
//	mgr := NewSessionManager()
//	session, err := mgr.Create(SessionConfig{...})
//	if err != nil {
//	    return err
//	}
//	if err := session.Start("/bin/bash"); err != nil {
//	    mgr.Close(session.ID)
//	    return err
//	}
//	// ... use session ...
//	mgr.CloseAll() // On shutdown
type SessionManager struct {
	// sessions maps session IDs to Session objects.
	// Protected by mu for concurrent access.
	sessions map[string]*Session

	// maxSessions is the maximum number of concurrent sessions allowed.
	// 0 means use DefaultMaxSessions.
	maxSessions int

	// mu protects the sessions map.
	// We use RWMutex for better concurrency: multiple readers can access
	// simultaneously, but writes require exclusive access.
	mu sync.RWMutex
}

// NewSessionManager creates a new session manager with an empty session map
// and default maximum session limit.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:    make(map[string]*Session),
		maxSessions: DefaultMaxSessions,
	}
}

// NewSessionManagerWithLimit creates a session manager with a custom session limit.
// If maxSessions is 0 or negative, DefaultMaxSessions is used.
func NewSessionManagerWithLimit(maxSessions int) *SessionManager {
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	return &SessionManager{
		sessions:    make(map[string]*Session),
		maxSessions: maxSessions,
	}
}

// Create creates a new PTY session with a randomly generated UUID.
//
// The session is added to the manager but not started. The caller must
// call session.Start() to begin the PTY process.
//
// Returns the created session or an error if creation fails.
func (m *SessionManager) Create(cfg SessionConfig) (*Session, error) {
	id := uuid.New().String()
	return m.CreateWithID(id, cfg)
}

// CreateWithID creates a new PTY session with a specific ID.
//
// This is useful when you need a predictable ID, such as the default
// "main" session or for testing purposes.
//
// Returns ErrSessionExists if a session with the given ID already exists.
// Returns ErrMaxSessionsReached if the session limit has been reached.
func (m *SessionManager) CreateWithID(id string, cfg SessionConfig) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check session limit
	if len(m.sessions) >= m.maxSessions {
		return nil, ErrMaxSessionsReached
	}

	// Check for duplicate ID
	if _, exists := m.sessions[id]; exists {
		return nil, ErrSessionExists
	}

	// Set the ID in the config so the session knows its own ID
	cfg.ID = id

	session := NewSession(cfg)
	m.sessions[id] = session

	return session, nil
}

// Register adds an externally-created session to the manager.
//
// This is used when a session is created outside the SessionManager,
// such as when attaching to a tmux session via TmuxManager.AttachToTmux().
// The session must already be started and have a valid ID.
//
// Returns ErrSessionExists if a session with the same ID already exists.
// Returns ErrMaxSessionsReached if the session limit has been reached.
func (m *SessionManager) Register(session *Session) error {
	if session == nil {
		return ErrSessionNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check session limit
	if len(m.sessions) >= m.maxSessions {
		return ErrMaxSessionsReached
	}

	// Check for duplicate ID
	if _, exists := m.sessions[session.ID]; exists {
		return ErrSessionExists
	}

	m.sessions[session.ID] = session
	return nil
}

// Get retrieves a session by its ID.
//
// Returns nil if no session exists with the given ID. This allows
// callers to easily check existence:
//
//	if session := mgr.Get(id); session != nil {
//	    // session exists
//	}
func (m *SessionManager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.sessions[id]
}

// Close stops a session and removes it from the manager.
//
// This method:
// 1. Stops the PTY process (sending SIGKILL if necessary)
// 2. Waits for the session to finish
// 3. Removes the session from the manager
//
// Returns ErrSessionNotFound if no session exists with the given ID.
func (m *SessionManager) Close(id string) error {
	m.mu.Lock()
	session, exists := m.sessions[id]
	if !exists {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	// Remove from map immediately to prevent new access
	delete(m.sessions, id)
	m.mu.Unlock()

	// Stop the session outside the lock to avoid blocking other operations
	if session.IsRunning() {
		if err := session.Stop(); err != nil {
			return err
		}
		// Wait for the session to fully exit
		<-session.Done()
	}

	return nil
}

// Rename changes the human-readable name of a session.
//
// Returns ErrSessionNotFound if no session exists with the given ID.
func (m *SessionManager) Rename(id, name string) error {
	m.mu.RLock()
	session, exists := m.sessions[id]
	m.mu.RUnlock()

	if !exists {
		return ErrSessionNotFound
	}

	session.SetName(name)
	return nil
}

// List returns information about all active sessions.
//
// The returned SessionInfo slice is a snapshot of the current state.
// Sessions may be added, removed, or change state after this call returns.
func (m *SessionManager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]SessionInfo, 0, len(m.sessions))
	for id, session := range m.sessions {
		// Use thread-safe accessors to avoid data races with concurrent Start() calls.
		// Each accessor acquires the session's mutex before reading.
		infos = append(infos, SessionInfo{
			ID:          id,
			Name:        session.GetName(),
			Command:     session.GetCommand(),
			Args:        session.GetArgs(),
			Running:     session.IsRunning(),
			CreatedAt:   session.GetCreatedAt(),
			TmuxSession: session.GetTmuxSession(),
		})
	}

	return infos
}

// CloseAll stops all sessions and clears the manager.
//
// This is typically called during shutdown to ensure all PTY processes
// are terminated cleanly. It stops all sessions concurrently for faster
// shutdown.
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	// Take ownership of the sessions map and replace with empty
	sessions := m.sessions
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	// Stop all sessions concurrently
	var wg sync.WaitGroup
	for _, session := range sessions {
		if session.IsRunning() {
			wg.Add(1)
			go func(s *Session) {
				defer wg.Done()
				_ = s.Stop()
				<-s.Done()
			}(session)
		}
	}
	wg.Wait()
}

// Count returns the number of sessions in the manager.
// Useful for testing and monitoring.
func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// MaxSessions returns the configured maximum number of sessions.
func (m *SessionManager) MaxSessions() int {
	return m.maxSessions
}
