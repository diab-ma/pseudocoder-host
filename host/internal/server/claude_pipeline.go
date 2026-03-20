package server

import (
	"log"
	"os"
	"sync"
	"time"
)

// claudePipeline manages the JSONL discovery→tail→parse→merge lifecycle for a Claude session.
// It implements the structuredPipeline interface.
type claudePipeline struct {
	sessionID  string
	workingDir string
	launchTime int64
	parser     *jsonlEventParser
	watcher    *JSONLSessionWatcher
	reader     *JSONLTailReader

	state      jsonlPipelineState
	generation uint64
	prompted   bool

	mu          sync.Mutex
	controller  StructuredChatController
	broadcast   func(Message)
	onDowngrade func()
}

// newClaudePipeline creates and starts the watcher→tail reader→parser→controller
// pipeline for a Claude JSONL session. It sends an initial empty snapshot so
// mobile clients transition out of "waiting" immediately.
func newClaudePipeline(sessionID, workingDir string, launchTime int64, controller StructuredChatController, broadcast func(Message), onDowngrade func()) (*claudePipeline, error) {
	parser := newJSONLEventParser(sessionID)

	if workingDir == "" {
		workingDir, _ = os.Getwd()
	}

	p := &claudePipeline{
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
	// "Waiting for the host timeline" immediately.
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
func (p *claudePipeline) onBound(filePath string) {
	log.Printf("claude_pipeline: bound session=%s file=%s", p.sessionID, filePath)

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
				p.mu.Unlock()
				return
			}

			items := p.parser.timeline()
			p.mu.Unlock()

			messages, err := p.controller.MergeProviderTimeline(p.sessionID, items)
			if err != nil {
				log.Printf("claude_pipeline: merge error session=%s: %v", p.sessionID, err)
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
			log.Printf("claude_pipeline: provider_reset session=%s", p.sessionID)
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
					log.Printf("claude_pipeline: reset revision error session=%s: %v", p.sessionID, err)
					return
				}
				p.broadcast(NewChatResetMessage(p.sessionID, p.controller.ServerBootID(), revN, ChatResetReasonProviderReset))
			}

			if _, err := p.controller.MergeProviderTimeline(p.sessionID, items); err != nil {
				log.Printf("claude_pipeline: batch merge error session=%s: %v", p.sessionID, err)
				return
			}

			snapshotMsg, err := p.controller.LoadSnapshotMessage(p.sessionID)
			if err != nil {
				log.Printf("claude_pipeline: snapshot load error session=%s: %v", p.sessionID, err)
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

// startWatcher creates and starts the JSONL session watcher. Pre-prompt
// watchers swallow timeouts (nil the watcher for SeedPrompt to restart);
// post-prompt watchers trigger the real onDowngrade and extend the deadline.
func (p *claudePipeline) startWatcher(launchTime int64, prePrompt bool) {
	config := JSONLSessionWatcherConfig{
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
				log.Printf("claude_pipeline: pre-prompt timeout session=%s, watcher cleared", p.sessionID)
				return
			}
			if p.onDowngrade != nil {
				p.onDowngrade()
			}
		},
	}
	// Extend the discovery deadline so it is at least 60s from now.
	// Needed for both pre-prompt restarts (launchTime now in the past)
	// and post-prompt watchers.
	if elapsed := time.Since(time.UnixMilli(launchTime)); elapsed > 0 {
		config.DiscoveryTimeout = elapsed + 60*time.Second
	}
	watcher, err := NewJSONLSessionWatcher(config)
	if err != nil {
		log.Printf("claude_pipeline: failed to create watcher session=%s: %v", p.sessionID, err)
		if p.onDowngrade != nil {
			p.onDowngrade()
		}
		return
	}

	p.mu.Lock()
	if p.watcher != nil || p.reader != nil {
		// Another goroutine beat us, or file already bound — discard ours.
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

// SeedPrompt implements structuredPipeline.
func (p *claudePipeline) SeedPrompt(text, requestID string, createdAt int64) error {
	p.mu.Lock()
	firstPrompt := !p.prompted
	p.prompted = true
	oldWatcher := p.watcher
	bound := p.reader != nil
	// Replace the watcher when the file hasn't been bound yet and either this
	// is the first prompt (swap pre-prompt → post-prompt watcher) or the
	// watcher was already nil'd by a pre-prompt timeout swallow.
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

// RollbackPrompt implements structuredPipeline.
func (p *claudePipeline) RollbackPrompt(requestID string) error {
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

// Stop implements structuredPipeline.
func (p *claudePipeline) Stop() {
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
