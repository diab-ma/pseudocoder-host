package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pseudocoder/host/internal/storage"
)

var (
	claudeStructuredCapabilities = &ChatCapabilities{
		StructuredTimeline: true,
		Markdown:           true,
		ToolCards:          true,
		ApprovalInline:     true,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewChat,
	}
	claudeDowngradedCapabilities = &ChatCapabilities{
		StructuredTimeline: false,
		Markdown:           false,
		ToolCards:          false,
		ApprovalInline:     false,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewTerminal,
	}
	codexStructuredCapabilities = &ChatCapabilities{
		StructuredTimeline: true,
		Markdown:           true,
		ToolCards:          true,
		ApprovalInline:     true,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewChat,
	}
	codexDowngradedCapabilities = &ChatCapabilities{
		StructuredTimeline: false,
		Markdown:           false,
		ToolCards:          false,
		ApprovalInline:     false,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewTerminal,
	}
	geminiStructuredCapabilities = &ChatCapabilities{
		StructuredTimeline: true,
		Markdown:           true,
		ToolCards:          true,
		ApprovalInline:     true,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewChat,
	}
	geminiDowngradedCapabilities = &ChatCapabilities{
		StructuredTimeline: false,
		Markdown:           false,
		ToolCards:          false,
		ApprovalInline:     false,
		TranscriptFallback: true,
		DefaultView:        ChatDefaultViewTerminal,
	}
)


// SessionInfoFromStoredSession converts persisted storage metadata into wire-facing session info.
func SessionInfoFromStoredSession(session *storage.Session) (SessionInfo, error) {
	if session == nil {
		return SessionInfo{}, fmt.Errorf("session cannot be nil")
	}

	capabilities, err := decodePersistedChatCapabilities(session.ChatCapabilitiesJSON)
	if err != nil {
		return SessionInfo{}, err
	}

	return SessionInfo{
		ID:               session.ID,
		Repo:             session.Repo,
		Branch:           session.Branch,
		StartedAt:        session.StartedAt.UnixMilli(),
		LastSeen:         session.LastSeen.UnixMilli(),
		LastCommit:       session.LastCommit,
		Status:           string(session.Status),
		IsSystem:         session.IsSystem,
		SessionKind:      decodePersistedSessionKind(session.SessionKind),
		AgentProvider:    decodePersistedAgentProvider(session.AgentProvider),
		ChatCapabilities: capabilities,
	}, nil
}

// ApplySessionMetadataToStorage persists server-owned classification fields without
// introducing a server import into the storage package.
func ApplySessionMetadataToStorage(session *storage.Session, metadata SessionInfo) error {
	if session == nil {
		return fmt.Errorf("session cannot be nil")
	}

	capabilitiesJSON, err := encodePersistedChatCapabilities(metadata.ChatCapabilities)
	if err != nil {
		return err
	}

	session.SessionKind = string(metadata.SessionKind)
	session.AgentProvider = string(metadata.AgentProvider)
	session.ChatCapabilitiesJSON = capabilitiesJSON
	return nil
}

// ClassifySessionMetadata returns the authoritative structured-chat classification metadata.
func ClassifySessionMetadata(command string, args []string, tmuxSession string) SessionInfo {
	return classifyStructuredChatSession(command, args, tmuxSession)
}

func classifyStructuredChatSession(command string, args []string, tmuxSession string) SessionInfo {
	if tmuxSession != "" {
		return SessionInfo{
			SessionKind:   SessionKindTmux,
			AgentProvider: AgentProviderNone,
		}
	}

	executable := normalizedCommandBasename(command, args)
	if executable == "claude" {
		return SessionInfo{
			SessionKind:      SessionKindAgent,
			AgentProvider:    AgentProviderClaude,
			ChatCapabilities: cloneChatCapabilities(claudeStructuredCapabilities),
		}
	}
	if executable == "codex" {
		return SessionInfo{
			SessionKind:      SessionKindAgent,
			AgentProvider:    AgentProviderCodex,
			ChatCapabilities: cloneChatCapabilities(codexStructuredCapabilities),
		}
	}
	if executable == "gemini" {
		return SessionInfo{
			SessionKind:      SessionKindAgent,
			AgentProvider:    AgentProviderGemini,
			ChatCapabilities: cloneChatCapabilities(geminiStructuredCapabilities),
		}
	}

	return SessionInfo{
		SessionKind:   SessionKindShell,
		AgentProvider: AgentProviderUnknown,
	}
}

func normalizedCommandBasename(command string, args []string) string {
	token := firstCommandToken(command)
	if token == "" && len(args) > 0 {
		token = args[0]
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return strings.ToLower(filepath.Base(token))
}

func firstCommandToken(command string) string {
	fields := strings.Fields(command)
	for _, field := range fields {
		if isEnvAssignment(field) {
			continue
		}
		return field
	}
	return command
}

func isEnvAssignment(token string) bool {
	parts := strings.SplitN(token, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return false
	}
	for i, r := range parts[0] {
		if i == 0 {
			if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func encodePersistedChatCapabilities(capabilities *ChatCapabilities) (string, error) {
	if capabilities == nil {
		return "", nil
	}
	payload, err := json.Marshal(capabilities)
	if err != nil {
		return "", fmt.Errorf("marshal chat capabilities: %w", err)
	}
	return string(payload), nil
}

func decodePersistedChatCapabilities(raw string) (*ChatCapabilities, error) {
	if raw == "" {
		return nil, nil
	}
	var capabilities ChatCapabilities
	if err := json.Unmarshal([]byte(raw), &capabilities); err != nil {
		return nil, fmt.Errorf("decode chat capabilities: %w", err)
	}
	return &capabilities, nil
}

func decodePersistedSessionKind(raw string) SessionKind {
	switch SessionKind(raw) {
	case SessionKindShell, SessionKindAgent, SessionKindTmux, SessionKindUnknown:
		return SessionKind(raw)
	default:
		return SessionKind(raw)
	}
}

func decodePersistedAgentProvider(raw string) AgentProvider {
	switch AgentProvider(raw) {
	case AgentProviderClaude, AgentProviderCodex, AgentProviderGemini, AgentProviderNone, AgentProviderUnknown:
		return AgentProvider(raw)
	default:
		return AgentProvider(raw)
	}
}

func cloneChatCapabilities(capabilities *ChatCapabilities) *ChatCapabilities {
	if capabilities == nil {
		return nil
	}
	clone := *capabilities
	return &clone
}

func persistSessionRecord(store storage.SessionStore, sessionID, command string, args []string, startedAt time.Time) error {
	if store == nil {
		return nil
	}

	session := &storage.Session{
		ID:        sessionID,
		StartedAt: startedAt.UTC(),
		LastSeen:  startedAt.UTC(),
		Status:    storage.SessionStatusRunning,
	}
	if err := ApplySessionMetadataToStorage(session, ClassifySessionMetadata(command, args, "")); err != nil {
		return err
	}
	if err := store.SaveSession(session); err != nil {
		return fmt.Errorf("save session record: %w", err)
	}
	return nil
}

// jsonlPipelineState tracks the phase of the JSONL pipeline lifecycle.
type jsonlPipelineState int

const (
	// jsonlPipelineStateInitialReplay: accumulating items from initial file read.
	jsonlPipelineStateInitialReplay jsonlPipelineState = iota
	// jsonlPipelineStateResetReplay: accumulating items from post-reset re-read.
	jsonlPipelineStateResetReplay
	// jsonlPipelineStateLive: processing items incrementally.
	jsonlPipelineStateLive
)

type structuredChatRuntime struct {
	store      storage.SessionStore
	controller StructuredChatController
	broadcast  func(Message)

	// workingDir is the host's working directory, used to derive the Claude
	// project slug for JSONL file discovery.
	workingDir string

	mu        sync.Mutex
	pipelines map[string]structuredPipeline
}

// StructuredChatRuntimeOption configures optional behavior on the runtime.
type StructuredChatRuntimeOption func(*structuredChatRuntime)

// WithWorkingDir sets the host working directory for JSONL project slug derivation.
func WithWorkingDir(dir string) StructuredChatRuntimeOption {
	return func(r *structuredChatRuntime) {
		r.workingDir = dir
	}
}

// NewStructuredChatRuntime creates the structured-chat runtime for supported providers.
func NewStructuredChatRuntime(store storage.SessionStore, controller StructuredChatController, broadcast func(Message), opts ...StructuredChatRuntimeOption) StructuredChatRuntime {
	r := &structuredChatRuntime{
		store:      store,
		controller: controller,
		broadcast:  broadcast,
		pipelines:  make(map[string]structuredPipeline),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *structuredChatRuntime) ObservePTYOutput(sessionID, chunk string, observedAt int64) {
	if sessionID == "" || chunk == "" || r.store == nil || r.controller == nil || r.broadcast == nil {
		return
	}

	_, info, err := r.loadStructuredSession(sessionID)
	if err != nil {
		return
	}

	// All providers use file-based pipelines — skip PTY observation but
	// eagerly start the pipeline so chat mode works immediately.
	r.ensurePipeline(sessionID, info.AgentProvider, observedAt)
}

func (r *structuredChatRuntime) SeedPrompt(sessionID, text, requestID string, observedAt int64) error {
	if sessionID == "" || strings.TrimSpace(text) == "" || requestID == "" || r.store == nil || r.controller == nil || r.broadcast == nil {
		return fmt.Errorf("invalid prompt seed input")
	}

	_, info, err := r.loadStructuredSession(sessionID)
	if err != nil {
		return err
	}

	r.ensurePipeline(sessionID, info.AgentProvider, observedAt)

	pipe := r.getPipeline(sessionID)
	if pipe == nil {
		return fmt.Errorf("structured pipeline unavailable")
	}

	return pipe.SeedPrompt(text, requestID, observedAt)
}

func (r *structuredChatRuntime) RollbackPrompt(sessionID, requestID string) error {
	if sessionID == "" || requestID == "" || r.store == nil || r.controller == nil || r.broadcast == nil {
		return fmt.Errorf("invalid prompt rollback input")
	}

	pipe := r.getPipeline(sessionID)
	if pipe == nil {
		return fmt.Errorf("structured pipeline unavailable")
	}

	return pipe.RollbackPrompt(requestID)
}

func (r *structuredChatRuntime) loadStructuredSession(sessionID string) (*storage.Session, SessionInfo, error) {
	session, err := r.store.GetSession(sessionID)
	if err != nil || session == nil {
		return nil, SessionInfo{}, fmt.Errorf("load session")
	}
	info, err := SessionInfoFromStoredSession(session)
	if err != nil {
		return nil, SessionInfo{}, err
	}
	if info.ChatCapabilities == nil || !info.ChatCapabilities.StructuredTimeline {
		return nil, SessionInfo{}, fmt.Errorf("structured timeline unavailable")
	}
	if info.AgentProvider != AgentProviderClaude && info.AgentProvider != AgentProviderCodex && info.AgentProvider != AgentProviderGemini {
		return nil, SessionInfo{}, fmt.Errorf("unsupported structured provider")
	}
	return session, info, nil
}

func (r *structuredChatRuntime) handleProviderDowngrade(sessionID string, provider AgentProvider, observedAt int64) {
	session, _, err := r.loadStructuredSession(sessionID)
	if err != nil || session == nil {
		return
	}

	session.SessionKind = string(SessionKindAgent)
	session.AgentProvider = string(provider)
	session.LastSeen = time.UnixMilli(observedAt).UTC()
	session.ChatCapabilitiesJSON = mustPersistedCapabilitiesJSON(downgradedCapabilitiesForProvider(provider))
	if err := r.store.SaveSession(session); err != nil {
		return
	}

	// Clean up pipeline entry.
	r.mu.Lock()
	delete(r.pipelines, sessionID)
	r.mu.Unlock()

	revision, err := r.controller.NextRevision(sessionID)
	if err != nil {
		return
	}
	eventIDPrefix, eventMessage := providerDowngradeEvent(provider)
	event := ChatItem{
		ID:        fmt.Sprintf("%s:%s:%d", eventIDPrefix, sessionID, revision),
		SessionID: sessionID,
		Kind:      ChatItemKindSystemEvent,
		CreatedAt: observedAt,
		Provider:  ChatItemProviderSystem,
		Status:    ChatItemStatusFailed,
		Code:      "adapter_downgrade",
		Message:   eventMessage,
	}
	r.broadcast(NewChatUpsertMessage(sessionID, r.controller.ServerBootID(), revision, []ChatItem{event}))

	// Allocate a separate revision for the reset so the client's strict
	// rev > record.revision gate doesn't drop it after the upsert above
	// already bumped the revision to the same value.
	resetRevision, err := r.controller.NextRevision(sessionID)
	if err != nil {
		return
	}
	r.broadcast(NewChatResetMessage(sessionID, r.controller.ServerBootID(), resetRevision, ChatResetReasonAdapterDowngrade))
}

func downgradedCapabilitiesForProvider(provider AgentProvider) *ChatCapabilities {
	switch provider {
	case AgentProviderClaude:
		return cloneChatCapabilities(claudeDowngradedCapabilities)
	case AgentProviderCodex:
		return cloneChatCapabilities(codexDowngradedCapabilities)
	case AgentProviderGemini:
		return cloneChatCapabilities(geminiDowngradedCapabilities)
	default:
		return nil
	}
}

func providerDowngradeEvent(provider AgentProvider) (string, string) {
	switch provider {
	case AgentProviderCodex:
		return "codex-downgrade", "Codex structured parsing failed; falling back to terminal view."
	case AgentProviderGemini:
		return "gemini-downgrade", "Gemini structured parsing failed; falling back to terminal view."
	case AgentProviderClaude:
		fallthrough
	default:
		return "claude-downgrade", "Claude structured parsing failed; falling back to terminal view."
	}
}

func mustPersistedCapabilitiesJSON(capabilities *ChatCapabilities) string {
	payload, err := encodePersistedChatCapabilities(capabilities)
	if err != nil {
		return ""
	}
	return payload
}

// ensurePipeline lazily starts the provider-specific pipeline for a session.
// Subsequent calls for the same session are no-ops.
func (r *structuredChatRuntime) ensurePipeline(sessionID string, provider AgentProvider, launchTime int64) {
	r.mu.Lock()
	if _, exists := r.pipelines[sessionID]; exists {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	workingDir := r.workingDir
	if workingDir == "" {
		workingDir, _ = os.Getwd()
	}

	onDowngrade := func() {
		log.Printf("structured_pipeline: adapter_downgrade session=%s provider=%s", sessionID, provider)
		r.handleProviderDowngrade(sessionID, provider, time.Now().UnixMilli())
	}

	var pipe structuredPipeline
	var err error

	switch provider {
	case AgentProviderClaude:
		pipe, err = newClaudePipeline(sessionID, workingDir, launchTime, r.controller, r.broadcast, onDowngrade)
	case AgentProviderCodex:
		pipe, err = newCodexPipeline(sessionID, workingDir, launchTime, r.controller, r.broadcast, onDowngrade)
	case AgentProviderGemini:
		pipe, err = newGeminiPipeline(sessionID, workingDir, launchTime, r.controller, r.broadcast, onDowngrade)
	default:
		log.Printf("structured_pipeline: unsupported provider %q for session %s", provider, sessionID)
		return
	}

	if err != nil {
		log.Printf("structured_pipeline: failed to create pipeline for session %s: %v", sessionID, err)
		onDowngrade()
		return
	}

	r.mu.Lock()
	// Double-check to prevent race with concurrent callers.
	if _, exists := r.pipelines[sessionID]; exists {
		r.mu.Unlock()
		pipe.Stop()
		return
	}
	r.pipelines[sessionID] = pipe
	r.mu.Unlock()
}

// getPipeline returns the active pipeline for a session, or nil.
func (r *structuredChatRuntime) getPipeline(sessionID string) structuredPipeline {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pipelines[sessionID]
}

// StopSession cleans up pipeline resources for a closed session.
func (r *structuredChatRuntime) StopSession(sessionID string) {
	r.mu.Lock()
	pipe, ok := r.pipelines[sessionID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.pipelines, sessionID)
	r.mu.Unlock()

	pipe.Stop()
}
