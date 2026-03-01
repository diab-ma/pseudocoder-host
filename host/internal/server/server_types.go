package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	// gorilla/websocket is the most popular WebSocket library for Go.
	// It provides a complete implementation of the WebSocket protocol
	// with support for reading/writing messages, ping/pong, and close handling.
	"github.com/gorilla/websocket"

	// PTY package provides session management for terminal sessions.
	"github.com/pseudocoder/host/internal/pty"

	// Storage package provides card and session persistence.
	"github.com/pseudocoder/host/internal/storage"

	// Tmux package provides integration with tmux terminal multiplexer.
	"github.com/pseudocoder/host/internal/tmux"

	// Rate limiting for terminal input to prevent message flooding.
	"golang.org/x/time/rate"
)

// channelBufferSize is the buffer size for the broadcast channel and per-client
// send channels. This value balances memory usage against the ability to absorb
// bursts of messages without blocking senders. If the buffer fills up, messages
// may be dropped for slow clients.
const channelBufferSize = 256

// DecisionHandler processes review decisions from clients.
// It is called when a client sends a review.decision message.
// Implementations should apply the decision (accept/reject) to the
// repository and storage, returning any error that occurred.
type DecisionHandler func(cardID, action, comment string) error

// ChunkDecisionHandler processes per-chunk decisions from clients.
// It is called when a client sends a chunk.decision message.
// The handler should apply the decision to the specific chunk within the card.
// The contentHash parameter is the hash of the chunk content when the user viewed it.
// If non-empty, the handler should validate it matches current content before applying.
type ChunkDecisionHandler func(cardID string, chunkIndex int, action string, contentHash string) error

// DeleteHandler processes file deletion requests from clients.
// It is called when a client sends a review.delete message for an untracked file.
// The handler should delete the file from the filesystem and remove the card from storage.
type DeleteHandler func(cardID string) error

// ReconnectHandler is called when a client connects to replay history.
// It should send terminal history and pending cards to the client.
// The handler receives a function to send messages directly to the client.
type ReconnectHandler func(sendToClient func(msg Message))

// TokenValidator validates authentication tokens for WebSocket connections.
// Returns the device ID if the token is valid, or an error if not.
type TokenValidator func(token string) (deviceID string, err error)

// DeviceActivityTracker is called to update device activity timestamps.
// The server calls this when a message is received from an authenticated client.
type DeviceActivityTracker func(deviceID string)

// UndoHandler processes undo requests for file-level cards.
// It reverses a previous accept/reject decision, restoring the card to pending.
// Returns the restored card for re-emission to clients.
// Phase 20.2: Enables undo flow from mobile.
type UndoHandler func(cardID string, confirmed bool) (*UndoResult, error)

// ChunkUndoHandler processes undo requests for chunk-level decisions.
// It reverses a previous chunk accept/reject decision.
// Returns the restored chunk info for re-emission to clients.
// Phase 20.2: Enables per-chunk undo flow from mobile.
// contentHash is preferred over chunkIndex for stable identity (indices can shift after staging).
type ChunkUndoHandler func(cardID string, chunkIndex int, contentHash string, confirmed bool) (*UndoResult, error)

// FileReadData holds the metadata returned by a file read operation.
type FileReadData struct {
	Content        string
	Encoding       string
	LineEnding     string
	Version        string
	ReadOnlyReason string
}

// FileOperations provides file listing, reading, and mutation within the repo boundary.
type FileOperations interface {
	List(path string) (entries []FileEntry, canonPath string, err error)
	Read(path string) (data *FileReadData, canonPath string, err error)
	Write(path, content, baseVersion string) (canonPath, newVersion string, err error)
	Create(path, content string) (canonPath, version string, err error)
	Delete(path string, confirmed bool) (canonPath string, err error)
}

// UndoResult carries the restored card/chunk information after a successful undo.
// This is used to re-emit the card to connected clients.
type UndoResult struct {
	CardID       string
	ChunkIndex   int    // -1 for file-level undo
	ContentHash  string // Stable chunk identifier (returned for client to clear pending state)
	File         string
	Diff         string
	OriginalDiff string // Full diff for card re-emission
}

// Server manages WebSocket connections and broadcasts messages to clients.
// It handles multiple concurrent clients and ensures messages are delivered
// to all connected clients without blocking the sender.
type Server struct {
	// addr is the address to listen on (e.g., "127.0.0.1:7070")
	addr string

	// upgrader converts HTTP connections to WebSocket connections.
	// We configure it to accept connections from any origin for development.
	upgrader websocket.Upgrader

	// clients tracks all connected WebSocket clients.
	// The map key is a pointer to the client, value is always true.
	// Using a map makes add/remove O(1) operations.
	clients map[*Client]bool

	// mu protects the clients map and stopped flag from concurrent access.
	mu sync.RWMutex

	// stopped indicates whether the server has been stopped.
	// This prevents sending to a closed broadcast channel.
	stopped bool

	// broadcast receives messages to send to all clients.
	// Using a channel decouples message production from delivery.
	broadcast chan Message

	// httpServer is the underlying HTTP server for graceful shutdown.
	httpServer *http.Server

	// sessionID is the current session identifier.
	// For now we only support one session at a time.
	sessionID string

	// decisionHandler is called when a client sends a review.decision message.
	// If nil, decisions are logged but not processed.
	decisionHandler DecisionHandler

	// chunkDecisionHandler is called when a client sends a chunk.decision message.
	// If nil, chunk decisions are logged but not processed.
	chunkDecisionHandler ChunkDecisionHandler

	// deleteHandler is called when a client sends a review.delete message.
	// If nil, delete requests are logged but not processed.
	deleteHandler DeleteHandler

	// undoHandler is called when a client sends a review.undo message.
	// If nil, undo requests are logged but not processed.
	// Phase 20.2: Enables undo flow from mobile.
	undoHandler UndoHandler

	// chunkUndoHandler is called when a client sends a chunk.undo message.
	// If nil, chunk undo requests are logged but not processed.
	// Phase 20.2: Enables per-chunk undo flow from mobile.
	chunkUndoHandler ChunkUndoHandler

	// reconnectHandler is called when a new client connects to replay history.
	// If nil, no history is replayed on connect.
	reconnectHandler ReconnectHandler

	// tokenValidator validates tokens for WebSocket authentication.
	// If nil, authentication is disabled (open access).
	tokenValidator TokenValidator

	// requireAuth controls whether authentication is required for WebSocket connections.
	// When true and tokenValidator is set, connections without valid tokens are rejected.
	requireAuth bool

	// pairHandler handles the /pair endpoint for code-to-token exchange.
	// Set via SetPairHandler.
	pairHandler http.Handler

	// generateCodeHandler handles the /pair/generate endpoint.
	// Set via SetGenerateCodeHandler.
	generateCodeHandler http.Handler

	// revokeDeviceHandler handles the /devices/{id}/revoke endpoint.
	// Set via SetRevokeDeviceHandler.
	revokeDeviceHandler http.Handler

	// deviceActivityTracker is called when a message is received from an
	// authenticated client. This allows updating last_seen timestamps.
	deviceActivityTracker DeviceActivityTracker

	// ptyWriter is the PTY session for writing terminal input.
	// Set via SetPTYWriter. If nil, terminal input messages are rejected.
	// Phase 5.5: Enables bidirectional terminal from mobile.
	ptyWriter io.Writer

	// approvalQueue manages pending approval requests from CLI tools.
	// Set via SetApprovalQueue. If nil, approval decisions are rejected.
	// Phase 6: Enables CLI command approval through mobile app.
	approvalQueue *ApprovalQueue

	// approveHandler handles the /approve endpoint for CLI approval requests.
	// Set via SetApproveHandler.
	// Phase 6.1b: HTTP endpoint for CLI command approval.
	approveHandler http.Handler

	// gitOps handles git operations for commit/push workflows.
	// Set via SetGitOperations. If nil, repo messages are rejected.
	// Phase 6.4: Enables commit workflow from mobile.
	gitOps *GitOperations

	// decidedStore provides access to decided cards for commit association.
	// Set via SetDecidedStore. Used to mark accepted cards as committed.
	// Phase 20.2: Enables commit association for undo support.
	decidedStore storage.DecidedCardStore

	// statusHandler handles the /status endpoint for CLI status queries.
	// Set via SetStatusHandler. Provides host info like uptime, clients, etc.
	// Phase 7.5: Enables "pseudocoder host status" CLI command.
	statusHandler http.Handler

	// sessionManager manages multiple concurrent PTY sessions.
	// Set via SetSessionManager. If nil, session management messages are rejected.
	// Phase 9.3: Enables multi-session PTY support from mobile.
	sessionManager *pty.SessionManager

	// sessionStore persists session history for session.list and clear-history operations.
	// Set via SetSessionStore. If nil, history mutation handlers are rejected.
	sessionStore storage.SessionStore

	// metricsStore records rollout metrics (crashes, latency, pairing).
	// Set via SetMetricsStore. If nil, metrics recording is silently skipped.
	// Phase 7: Rollout metrics collection.
	metricsStore storage.MetricsStore

	// createdSessionSequence tracks successful session.create operations.
	// This provides stable auto-name numbering independent of manager size,
	// pre-registered legacy sessions, close/reopen churn, and tmux attachments.
	createdSessionSequence int

	// sessionAPIHandler handles the /api/session/ endpoints for CLI session management.
	// Set via SetSessionAPIHandler.
	// Phase 9.5: Enables CLI session commands (new, list, kill, rename).
	sessionAPIHandler http.Handler

	// tmuxManager handles tmux session discovery and attachment.
	// Set via SetTmuxManager. If nil, tmux messages are rejected.
	// Phase 12: Enables tmux session integration from mobile.
	tmuxManager *tmux.Manager

	// tmuxAPIHandler handles the /api/tmux/ endpoints for CLI tmux management.
	// Set via SetTmuxAPIHandler.
	// Phase 12.9: Enables CLI tmux commands (list-tmux, attach-tmux, detach).
	tmuxAPIHandler http.Handler

	// fileOps handles file listing and reading for the file explorer.
	// Set via SetFileOperations. If nil, file messages return handler_missing.
	// Phase 3: Enables file explorer protocol.
	fileOps FileOperations

	// repoMutationMu is a process-wide repo mutation gate.
	// Uses TryLock() for non-blocking reject of concurrent mutations.
	// P4U2: Shared across repo.commit, repo.push, repo.branch_create, repo.branch_switch.
	repoMutationMu sync.Mutex

	// keepAwake stores Phase 17 keep-awake control-plane state.
	// It is process-scoped and intentionally non-persistent.
	keepAwake *keepAwakeControlPlane

	// keepAwakePolicyHandler handles the /api/keep-awake/policy endpoint.
	// Set via SetKeepAwakePolicyHandler. Phase 18: CLI-driven policy mutation.
	keepAwakePolicyHandler http.Handler

}

// maxIdempotencyEntries is the maximum number of cached mutation results per client.
const maxIdempotencyEntries = 256

// idempotencyCacheKey uniquely identifies a mutation request.
type idempotencyCacheKey struct {
	OpType    MessageType
	RequestID string
}

// idempotencyCacheEntry stores the fingerprint and result for a cached mutation.
type idempotencyCacheEntry struct {
	Fingerprint string
	Result      Message
}

// inFlightMutation tracks a currently executing mutation request.
// Exact duplicates with the same fingerprint wait for done and reuse Result.
type inFlightMutation struct {
	Fingerprint string
	Result      Message
	done        chan struct{}
}

// idempotencyCache stores recent mutation results per client for replay.
type idempotencyCache struct {
	entries map[idempotencyCacheKey]*idempotencyCacheEntry
	order   []idempotencyCacheKey
	maxSize int
}

// newIdempotencyCache creates a new idempotency cache.
func newIdempotencyCache() *idempotencyCache {
	return &idempotencyCache{
		entries: make(map[idempotencyCacheKey]*idempotencyCacheEntry),
		maxSize: maxIdempotencyEntries,
	}
}

// get returns a cached entry if it exists.
func (c *idempotencyCache) get(key idempotencyCacheKey) (*idempotencyCacheEntry, bool) {
	entry, ok := c.entries[key]
	return entry, ok
}

// store adds or replaces an entry with FIFO eviction.
func (c *idempotencyCache) store(key idempotencyCacheKey, entry *idempotencyCacheEntry) {
	if _, exists := c.entries[key]; !exists {
		if len(c.order) >= c.maxSize {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = entry
}

// computeFingerprint returns a SHA256 hex digest of null-separated fields.
func computeFingerprint(fields ...string) string {
	h := sha256.New()
	for i, f := range fields {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeFingerprint creates a fingerprint for a file write operation.
func writeFingerprint(canonPath, content, baseVersion string) string {
	return computeFingerprint("write", canonPath, content, baseVersion)
}

// createFingerprint creates a fingerprint for a file create operation.
func createFingerprint(canonPath, content string) string {
	return computeFingerprint("create", canonPath, content)
}

// deleteFingerprint creates a fingerprint for a file delete operation.
func deleteFingerprint(canonPath string, confirmed *bool) string {
	c := "<missing>"
	if confirmed != nil {
		c = "false"
		if *confirmed {
			c = "true"
		}
	}
	return computeFingerprint("delete", canonPath, c)
}

// branchCreateFingerprint creates a fingerprint for a branch create operation.
// Normalizes name to lowercase for case-insensitive dedup.
func branchCreateFingerprint(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return computeFingerprint("branch_create", normalized)
}

// branchSwitchFingerprint creates a fingerprint for a branch switch operation.
// Normalizes name to lowercase for case-insensitive dedup.
func branchSwitchFingerprint(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return computeFingerprint("branch_switch", normalized)
}

// commitFingerprint creates a fingerprint for a commit operation.
// Includes all mutation-affecting fields.
func commitFingerprint(message string, noVerify, noGpgSign, overrideWarnings bool) string {
	return computeFingerprint("commit", message,
		fmt.Sprintf("%t", noVerify),
		fmt.Sprintf("%t", noGpgSign),
		fmt.Sprintf("%t", overrideWarnings))
}

// pushFingerprint creates a fingerprint for a push operation.
func pushFingerprint(remote, branch string, forceWithLease bool) string {
	return computeFingerprint("push", remote, branch,
		fmt.Sprintf("%t", forceWithLease))
}

// fetchFingerprint creates a fingerprint for a fetch operation.
// Constant: no payload fields beyond request_id.
func fetchFingerprint() string {
	return computeFingerprint("fetch")
}

// pullFingerprint creates a fingerprint for a pull operation.
// Constant: no payload fields beyond request_id.
func pullFingerprint() string {
	return computeFingerprint("pull")
}

// prCreateFingerprint creates a fingerprint for a PR create operation.
// Normalizes title/baseBranch via TrimSpace, treats omitted draft as false,
// and includes whether base_branch was provided to avoid aliasing omitted
// vs whitespace-only invalid payloads.
func prCreateFingerprint(title, body, baseBranch string, draft bool, baseBranchProvided bool) string {
	return computeFingerprint("pr_create",
		strings.TrimSpace(title),
		body,
		strings.TrimSpace(baseBranch),
		fmt.Sprintf("%t", draft),
		fmt.Sprintf("%t", baseBranchProvided))
}

// prCheckoutFingerprint creates a fingerprint for a PR checkout operation.
func prCheckoutFingerprint(number int) string {
	return computeFingerprint("pr_checkout", fmt.Sprintf("%d", number))
}

// idempotencyCheckOrBegin resolves a mutation idempotency key:
// - completed exact match -> replay
// - in-flight exact match -> coalesce by waiting on returned inFlight
// - miss -> caller becomes owner and must call idempotencyComplete
// - mismatch -> error
func (c *Client) idempotencyCheckOrBegin(opType MessageType, requestID, fingerprint string) (result Message, replay bool, inFlight *inFlightMutation, err error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()

	cache := c.ensureMutationCacheLocked()
	key := idempotencyCacheKey{OpType: opType, RequestID: requestID}
	if entry, ok := cache.get(key); ok {
		if entry.Fingerprint != fingerprint {
			return Message{}, false, nil, &idempotencyMismatchError{requestID: requestID}
		}
		return entry.Result, true, nil, nil
	}

	if c.inFlightMutations == nil {
		c.inFlightMutations = make(map[idempotencyCacheKey]*inFlightMutation)
	}
	if existing, ok := c.inFlightMutations[key]; ok {
		if existing.Fingerprint != fingerprint {
			return Message{}, false, nil, &idempotencyMismatchError{requestID: requestID}
		}
		return Message{}, false, existing, nil
	}

	c.inFlightMutations[key] = &inFlightMutation{
		Fingerprint: fingerprint,
		done:        make(chan struct{}),
	}
	return Message{}, false, nil, nil
}

// idempotencyComplete stores the final mutation result and releases any
// coalesced duplicates waiting on the same in-flight key.
func (c *Client) idempotencyComplete(opType MessageType, requestID, fingerprint string, result Message) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()

	cache := c.ensureMutationCacheLocked()
	key := idempotencyCacheKey{OpType: opType, RequestID: requestID}
	cache.store(key, &idempotencyCacheEntry{
		Fingerprint: fingerprint,
		Result:      result,
	})

	if inFlight, ok := c.inFlightMutations[key]; ok {
		inFlight.Result = result
		delete(c.inFlightMutations, key)
		close(inFlight.done)
	}
}

// bestEffortCanon attempts canonicalization for fingerprinting.
// Returns the input unchanged on error (fingerprint will still be consistent).
func bestEffortCanon(path string) string {
	canon, err := canonicalizePath(path)
	if err != nil {
		return path
	}
	return canon
}

// idempotencyCheck checks the idempotency cache and returns the cached result if hit.
// Returns (result, true) for replay, (_, false) for miss.
// Returns an error message for fingerprint mismatch.
func (c *Client) idempotencyCheck(opType MessageType, requestID, fingerprint string) (Message, bool, error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()

	cache := c.ensureMutationCacheLocked()
	key := idempotencyCacheKey{OpType: opType, RequestID: requestID}
	entry, ok := cache.get(key)
	if !ok {
		return Message{}, false, nil
	}
	if entry.Fingerprint != fingerprint {
		return Message{}, false, &idempotencyMismatchError{requestID: requestID}
	}
	return entry.Result, true, nil
}

// idempotencyStore stores a result in the idempotency cache.
func (c *Client) idempotencyStore(opType MessageType, requestID, fingerprint string, result Message) {
	c.idempotencyComplete(opType, requestID, fingerprint, result)
}

// idempotencyMismatchError indicates a request_id collision with different parameters.
type idempotencyMismatchError struct {
	requestID string
}

func (e *idempotencyMismatchError) Error() string {
	return "request_id " + e.requestID + " already used with different parameters"
}

// ensureMutationCacheLocked lazily initializes the per-client mutation cache.
// Caller must hold c.mutationMu.
func (c *Client) ensureMutationCacheLocked() *idempotencyCache {
	if c.mutationCache == nil {
		c.mutationCache = newIdempotencyCache()
	}
	return c.mutationCache
}

// ensureMutationCache keeps backward compatibility for existing callers that
// only need best-effort cache access.
func (c *Client) ensureMutationCache() *idempotencyCache {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	return c.ensureMutationCacheLocked()
}

// Client represents a single WebSocket connection.
// Each client has its own goroutine for writing messages,
// which prevents slow clients from blocking the broadcast.
type Client struct {
	// conn is the underlying WebSocket connection.
	conn *websocket.Conn

	// send is a buffered channel for outgoing messages.
	// The write goroutine reads from this and sends to the WebSocket.
	// Buffering prevents blocking when the client is slow.
	send chan Message

	// done is closed to signal the client should shut down.
	// Used to coordinate clean shutdown without racing on send channel.
	done chan struct{}

	// sendOnce ensures the send channel is only closed once.
	// Both Stop() and readPump() may try to close it, so we use
	// sync.Once to prevent a "close of closed channel" panic.
	sendOnce sync.Once

	// server is a reference back to the parent server.
	server *Server

	// deviceID is the ID of the paired device for this connection.
	// Set during WebSocket upgrade if authentication is enabled.
	// Empty string means unauthenticated (allowed when requireAuth is false).
	deviceID string

	// inputLimiter rate-limits terminal input messages to prevent flooding.
	// Configured at 1000 messages/sec with a burst of 10.
	// Phase 5.5: Protects PTY from excessive input.
	inputLimiter *rate.Limiter

	// activeSessionID is the PTY session this client is currently viewing.
	// Empty string means the client is viewing the default/main session.
	// Used for session.switch to replay correct terminal buffer.
	// Phase 9.3: Multi-session PTY support.
	//
	// Threading model:
	// - Written only by the client's own readPump goroutine (in handleSessionSwitch)
	// - Read by handleSessionClose when resetting clients viewing a closed session
	//   (protected by server.mu during iteration over clients map)
	// - This is safe because writes happen in a single goroutine and reads are
	//   protected by server.mu when accessing from other goroutines.
	activeSessionID string

	// mutationCache stores recent file mutation results for idempotent replay.
	// Lazy-initialized on first mutation. Guarded by mutationMu.
	// Phase 3 P3U2 + Phase 4 P4U2: Enables idempotent replay/coalescing.
	mutationMu    sync.Mutex
	mutationCache *idempotencyCache

	// inFlightMutations tracks active mutation requests by operation+request_id.
	// Exact duplicates wait for completion and reuse the final result.
	// Guarded by mutationMu.
	inFlightMutations map[idempotencyCacheKey]*inFlightMutation
}

// NewServer creates a new WebSocket server.
// Call Start() to begin accepting connections.
func NewServer(addr string) *Server {
	return &Server{
		addr:                   addr,
		clients:                make(map[*Client]bool),
		broadcast:              make(chan Message, channelBufferSize),
		sessionID:              "session-" + formatTimestamp(time.Now()),
		createdSessionSequence: 0,
		upgrader: websocket.Upgrader{
			// Allow connections from any origin during development.
			// In production with TLS and auth, this is less critical.
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			// Buffer sizes for reading and writing WebSocket frames.
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
		keepAwake: newKeepAwakeControlPlane(),
	}
}

// formatTimestamp formats a time as Unix seconds for session IDs.
// This is a helper to avoid importing fmt in the constructor.
func formatTimestamp(t time.Time) string {
	// Simple integer-to-string conversion for session ID suffix
	n := t.Unix()
	if n == 0 {
		return "0"
	}
	// Build string in reverse
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
