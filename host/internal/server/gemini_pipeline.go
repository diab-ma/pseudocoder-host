package server

import (
	"log"
	"sync"
	"time"
)

// geminiPipeline manages the Gemini session file discovery -> tail -> parse -> merge
// lifecycle for one session. It wires together GeminiSessionWatcher (Unit 4),
// GeminiFileTailer (Unit 5), and geminiEventParser (Unit 3).
type geminiPipeline struct {
	sessionID  string
	workingDir string
	launchTime int64
	parser     *geminiEventParser
	watcher    *GeminiSessionWatcher
	tailer     *GeminiFileTailer

	state    jsonlPipelineState // reuse existing state enum: InitialReplay, ResetReplay, Live
	stopped  bool
	prompted bool

	mu          sync.Mutex
	controller  StructuredChatController
	broadcast   func(Message)
	onDowngrade func() // called on watcher downgrade
}

// newGeminiPipeline creates and starts a Gemini pipeline for the given session.
// It creates the parser and watcher, sends an initial empty snapshot, and starts
// the discovery loop. The onBound callback will create the tailer when a session
// file is discovered.
func newGeminiPipeline(
	sessionID string,
	workingDir string,
	launchTime int64,
	controller StructuredChatController,
	broadcast func(Message),
	onDowngrade func(),
) (*geminiPipeline, error) {
	parser := newGeminiEventParser(sessionID)

	p := &geminiPipeline{
		sessionID:   sessionID,
		workingDir:  workingDir,
		launchTime:  launchTime,
		parser:      parser,
		state:       jsonlPipelineStateInitialReplay,
		controller:  controller,
		broadcast:   broadcast,
		onDowngrade: onDowngrade,
	}

	// Send an initial empty snapshot so mobile clients transition out of
	// "Waiting for the host timeline" immediately. The real snapshot with
	// parsed items arrives once the watcher binds and reads the file.
	if rev, err := controller.NextRevision(sessionID); err == nil {
		broadcast(NewChatSnapshotMessage(sessionID, controller.ServerBootID(), rev, nil))
	}

	// Start watcher immediately so existing session files are discovered.
	// If discovery times out before the first SeedPrompt, the downgrade is
	// swallowed and the watcher restarts when a prompt arrives.
	p.startWatcher(launchTime, true)

	return p, nil
}

// onBound is called when the session watcher discovers and binds a Gemini session file.
// It creates and starts a GeminiFileTailer that reads the JSON file and delivers
// parsed messages through handleMessages/handleReset.
func (p *geminiPipeline) onBound(filePath string, watcherSessionID string) {
	log.Printf("gemini_pipeline: bound session=%s file=%s watcherSession=%s",
		p.sessionID, filePath, watcherSessionID)

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath: filePath,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			p.handleMessages(messages, sessionID)
		},
		OnReset: func() {
			p.handleReset()
		},
		OnError: func(err error) {
			log.Printf("gemini_pipeline: tailer error session=%s: %v", p.sessionID, err)
		},
	})

	p.mu.Lock()
	p.tailer = tailer
	p.mu.Unlock()

	tailer.Start()
}

// handleMessages processes a complete set of messages from the Gemini file tailer.
// Unlike the JSONL pipeline which processes lines incrementally, Gemini does a full
// rebuild each time since the JSON file contains the complete conversation.
//
// State transitions:
//   - First call (InitialReplay -> Live): merge + snapshot
//   - Subsequent calls (Live -> Live): merge only (incremental diff handled by controller)
//   - After reset (ResetReplay -> Live): reset message + merge + snapshot
func (p *geminiPipeline) handleMessages(messages []geminiMessage, sessionID string) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	items := p.parser.RebuildTimeline(messages)
	prevState := p.state
	p.state = jsonlPipelineStateLive
	p.mu.Unlock()

	log.Printf("gemini_pipeline: handleMessages session=%s messages=%d items=%d prevState=%d",
		p.sessionID, len(messages), len(items), prevState)

	if prevState == jsonlPipelineStateResetReplay {
		// Provider reset sequence: send chat.reset at revision N before merging.
		revN, err := p.controller.NextRevision(p.sessionID)
		if err != nil {
			log.Printf("gemini_pipeline: reset revision error session=%s: %v", p.sessionID, err)
			return
		}
		p.broadcast(NewChatResetMessage(p.sessionID, p.controller.ServerBootID(), revN, ChatResetReasonProviderReset))
	}

	// Merge all items via the controller.
	mergeMessages, err := p.controller.MergeProviderTimeline(p.sessionID, items)
	if err != nil {
		log.Printf("gemini_pipeline: merge error session=%s: %v", p.sessionID, err)
		return
	}

	if prevState != jsonlPipelineStateLive {
		// Initial/reset replay: send a full snapshot after merging so clients
		// get the complete timeline. The incremental diff messages from merge
		// are not broadcast (the snapshot supersedes them).
		snapshotMsg, err := p.controller.LoadSnapshotMessage(p.sessionID)
		if err != nil {
			log.Printf("gemini_pipeline: snapshot load error session=%s: %v", p.sessionID, err)
			return
		}
		if snapshotMsg != nil {
			p.broadcast(*snapshotMsg)
		}
	} else {
		// Live mode: broadcast the incremental diff messages from merge.
		for _, msg := range mergeMessages {
			p.broadcast(msg)
		}
	}
}

// handleReset is called when the file tailer detects a session identity change,
// inode change, or truncation. It clears parser state and sets the pipeline to
// ResetReplay so the next handleMessages sends the reset sequence.
func (p *geminiPipeline) handleReset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parser.Reset()
	p.state = jsonlPipelineStateResetReplay
	log.Printf("gemini_pipeline: provider_reset session=%s", p.sessionID)
}

// startWatcher creates and starts the Gemini session watcher.
// startWatcher creates and starts the Gemini session watcher. Pre-prompt
// watchers swallow timeouts (nil the watcher for SeedPrompt to restart);
// post-prompt watchers trigger the real onDowngrade and extend the deadline.
func (p *geminiPipeline) startWatcher(launchTime int64, prePrompt bool) {
	config := GeminiSessionWatcherConfig{
		WorkingDir: p.workingDir,
		LaunchTime: launchTime,
		OnBound: func(filePath string, watcherSessionID string) {
			p.onBound(filePath, watcherSessionID)
		},
		OnDowngrade: func() {
			if prePrompt {
				p.mu.Lock()
				p.watcher = nil
				p.mu.Unlock()
				log.Printf("gemini_pipeline: pre-prompt timeout session=%s, watcher cleared", p.sessionID)
				return
			}
			if p.onDowngrade != nil {
				p.onDowngrade()
			}
		},
	}
	if elapsed := time.Since(time.UnixMilli(launchTime)); elapsed > 0 {
		config.DiscoveryTimeout = elapsed + 60*time.Second
	}
	watcher, err := NewGeminiSessionWatcher(config)
	if err != nil {
		log.Printf("gemini_pipeline: failed to create watcher session=%s: %v", p.sessionID, err)
		if p.onDowngrade != nil {
			p.onDowngrade()
		}
		return
	}

	p.mu.Lock()
	if p.watcher != nil || p.tailer != nil {
		p.mu.Unlock()
		watcher.Stop()
		return
	}
	if p.stopped {
		// Stop() was already called — don't start anything.
		p.mu.Unlock()
		watcher.Stop()
		return
	}
	p.watcher = watcher
	p.mu.Unlock()

	watcher.Start()
}

// SeedPrompt registers a pending seed and merges the updated timeline.
// The seed appears as an optimistic user bubble immediately; when the Gemini
// file later echoes the same text, the seed is consumed (deduplicated).
func (p *geminiPipeline) SeedPrompt(text, requestID string, createdAt int64) error {
	p.mu.Lock()
	firstPrompt := !p.prompted
	p.prompted = true
	oldWatcher := p.watcher
	bound := p.tailer != nil
	needsRestart := !bound && (firstPrompt || p.watcher == nil)
	if needsRestart {
		p.watcher = nil
	}
	p.mu.Unlock()

	if needsRestart {
		if oldWatcher != nil {
			oldWatcher.Stop()
		}
		p.startWatcher(p.launchTime, false)
	}

	p.mu.Lock()
	p.parser.SeedPromptWithItem(text, requestID, createdAt)
	items := p.parser.timeline()
	p.mu.Unlock()

	messages, err := p.controller.MergeProviderTimeline(p.sessionID, items)
	if err != nil {
		return err
	}
	log.Printf("gemini_pipeline: SeedPrompt session=%s items=%d broadcasts=%d",
		p.sessionID, len(items), len(messages))
	for _, msg := range messages {
		p.broadcast(msg)
	}
	return nil
}

// RollbackPrompt removes a previously seeded prompt and merges the updated timeline.
func (p *geminiPipeline) RollbackPrompt(requestID string) error {
	p.mu.Lock()
	p.parser.RollbackSeedItem(requestID)
	items := p.parser.timeline()
	p.mu.Unlock()

	messages, err := p.controller.MergeProviderTimeline(p.sessionID, items)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		p.broadcast(msg)
	}
	return nil
}

// Stop shuts down the pipeline's tailer and watcher, and sets the stopped flag
// to suppress any late callbacks from background goroutines.
func (p *geminiPipeline) Stop() {
	p.mu.Lock()
	p.stopped = true
	tailer := p.tailer
	watcher := p.watcher
	p.mu.Unlock()

	if tailer != nil {
		tailer.Stop()
	}
	if watcher != nil {
		watcher.Stop()
	}
}
