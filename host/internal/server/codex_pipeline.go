package server

import (
	"log"
	"sync"
	"time"
)

// structuredPipeline manages the lifecycle of a provider-specific structured chat pipeline.
type structuredPipeline interface {
	SeedPrompt(text, requestID string, createdAt int64) error
	RollbackPrompt(requestID string) error
	Stop()
}

// codexPipeline wires together the Codex event parser, session watcher, and
// JSONL tail reader into a single lifecycle matching the Claude JSONL pipeline
// pattern in structured_chat_runtime.go.
type codexPipeline struct {
	sessionID  string
	workingDir string
	launchTime int64
	parser     *codexEventParser
	watcher    *CodexSessionWatcher
	reader     *JSONLTailReader

	state      jsonlPipelineState // reuse existing state enum
	generation uint64             // for FR26 late-callback suppression
	prompted   bool

	mu          sync.Mutex
	controller  StructuredChatController
	broadcast   func(Message)
	onDowngrade func()
}

// newCodexPipeline creates and starts a Codex pipeline for the given session.
// It creates the parser and watcher, starts discovery, and sends an initial
// empty snapshot so mobile clients transition out of "waiting" immediately.
func newCodexPipeline(
	sessionID string,
	workingDir string,
	launchTime int64,
	controller StructuredChatController,
	broadcast func(Message),
	onDowngrade func(),
) (*codexPipeline, error) {
	parser := newCodexEventParser(sessionID)

	p := &codexPipeline{
		sessionID:   sessionID,
		workingDir:  workingDir,
		launchTime:  launchTime,
		parser:      parser,
		state:       jsonlPipelineStateInitialReplay,
		controller:  controller,
		broadcast:   broadcast,
		onDowngrade: onDowngrade,
	}

	// Send initial empty snapshot so mobile transitions out of "waiting".
	if rev, err := controller.NextRevision(sessionID); err == nil {
		broadcast(NewChatSnapshotMessage(sessionID, controller.ServerBootID(), rev, nil))
	}

	// Start watcher immediately so existing JSONL files are discovered.
	// If discovery times out before the first SeedPrompt, the downgrade is
	// swallowed and the watcher restarts when a prompt arrives.
	p.startWatcher(launchTime, true)

	return p, nil
}

// onBound is called when the session watcher discovers and binds a JSONL file.
// It creates and starts the tail reader.
func (p *codexPipeline) onBound(filePath string) {
	log.Printf("codex_pipeline: bound session=%s file=%s", p.sessionID, filePath)

	p.mu.Lock()
	gen := p.generation
	p.mu.Unlock()

	reader := NewJSONLTailReader(JSONLTailReaderConfig{
		FilePath: filePath,
		OnLine: func(line []byte) {
			p.mu.Lock()
			if p.generation != gen {
				p.mu.Unlock()
				return
			}
			p.parser.ParseEvent(line)

			if p.state != jsonlPipelineStateLive {
				// During initial/reset replay: accumulate items without merging.
				p.mu.Unlock()
				return
			}

			items := p.parser.timeline()
			p.mu.Unlock()

			// Check generation hasn't changed (FR26).
			p.mu.Lock()
			if p.generation != gen {
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()

			messages, err := p.controller.MergeProviderTimeline(p.sessionID, items)
			if err != nil {
				log.Printf("codex_pipeline: merge error session=%s: %v", p.sessionID, err)
				return
			}
			for _, msg := range messages {
				p.broadcast(msg)
			}
		},
		OnReset: func() {
			p.mu.Lock()
			if p.generation != gen {
				p.mu.Unlock()
				return
			}
			p.parser.Reset()
			p.state = jsonlPipelineStateResetReplay
			p.mu.Unlock()
			log.Printf("codex_pipeline: provider_reset session=%s", p.sessionID)
		},
		OnReadComplete: func() {
			p.mu.Lock()
			if p.generation != gen {
				p.mu.Unlock()
				return
			}
			items := p.parser.timeline()
			prevState := p.state
			p.state = jsonlPipelineStateLive
			p.mu.Unlock()

			if prevState == jsonlPipelineStateResetReplay {
				revN, err := p.controller.NextRevision(p.sessionID)
				if err != nil {
					log.Printf("codex_pipeline: reset revision error session=%s: %v", p.sessionID, err)
					return
				}
				p.broadcast(NewChatResetMessage(p.sessionID, p.controller.ServerBootID(), revN, ChatResetReasonProviderReset))
			}

			// Merge all accumulated items at once.
			if _, err := p.controller.MergeProviderTimeline(p.sessionID, items); err != nil {
				log.Printf("codex_pipeline: batch merge error session=%s: %v", p.sessionID, err)
				return
			}

			// Send full snapshot to connected clients.
			snapshotMsg, err := p.controller.LoadSnapshotMessage(p.sessionID)
			if err != nil {
				log.Printf("codex_pipeline: snapshot load error session=%s: %v", p.sessionID, err)
				return
			}
			if snapshotMsg != nil {
				p.broadcast(*snapshotMsg)
			}
		},
	})

	p.mu.Lock()
	p.reader = reader
	p.mu.Unlock()

	reader.Start()
}

// startWatcher creates and starts the Codex session watcher.
// startWatcher creates and starts the Codex session watcher. Pre-prompt
// watchers swallow timeouts (nil the watcher for SeedPrompt to restart);
// post-prompt watchers trigger the real onDowngrade and extend the deadline.
func (p *codexPipeline) startWatcher(launchTime int64, prePrompt bool) {
	config := CodexSessionWatcherConfig{
		WorkingDir: p.workingDir,
		LaunchTime: launchTime,
		OnBound: func(filePath string) {
			p.onBound(filePath)
		},
		OnDowngrade: func() {
			if prePrompt {
				p.mu.Lock()
				p.watcher = nil
				p.mu.Unlock()
				log.Printf("codex_pipeline: pre-prompt timeout session=%s, watcher cleared", p.sessionID)
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
	watcher, err := NewCodexSessionWatcher(config)
	if err != nil {
		log.Printf("codex_pipeline: failed to create watcher session=%s: %v", p.sessionID, err)
		if p.onDowngrade != nil {
			p.onDowngrade()
		}
		return
	}

	p.mu.Lock()
	if p.watcher != nil || p.reader != nil {
		p.mu.Unlock()
		watcher.Stop()
		return
	}
	if p.generation > 0 {
		// Stop() was already called — don't start anything.
		p.mu.Unlock()
		watcher.Stop()
		return
	}
	p.watcher = watcher
	p.mu.Unlock()

	watcher.Start()
}

// SeedPrompt registers a pending seed and adds a seeded user item to the
// timeline, then merges and broadcasts.
func (p *codexPipeline) SeedPrompt(text, requestID string, createdAt int64) error {
	p.mu.Lock()
	firstPrompt := !p.prompted
	p.prompted = true
	oldWatcher := p.watcher
	bound := p.reader != nil
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
	for _, msg := range messages {
		p.broadcast(msg)
	}
	return nil
}

// RollbackPrompt clears the pending seed and removes the seeded user item,
// then merges and broadcasts.
func (p *codexPipeline) RollbackPrompt(requestID string) error {
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

// Stop halts the tail reader and session watcher, incrementing the generation
// to suppress any late callbacks.
func (p *codexPipeline) Stop() {
	p.mu.Lock()
	p.generation++
	reader := p.reader
	watcher := p.watcher
	p.mu.Unlock()

	if reader != nil {
		reader.Stop()
	}
	if watcher != nil {
		watcher.Stop()
	}
}
