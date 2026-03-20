package server

import "sync"

// sessionOutputGate buffers PTY output until the create path decides whether
// the session is ready to stream live output. This prevents early chunks from
// racing ahead of metadata persistence.
type sessionOutputGate struct {
	forward func(sessionID, chunk string)

	mu       sync.Mutex
	active   bool
	draining bool
	dropped  bool
	buffered []bufferedSessionChunk
}

type bufferedSessionChunk struct {
	sessionID string
	chunk     string
}

func newSessionOutputGate(forward func(sessionID, chunk string)) *sessionOutputGate {
	return &sessionOutputGate{forward: forward}
}

func (g *sessionOutputGate) Callback(sessionID, chunk string) {
	if g == nil || g.forward == nil || chunk == "" {
		return
	}

	g.mu.Lock()
	switch {
	case g.dropped:
		g.mu.Unlock()
		return
	case g.draining || !g.active:
		g.buffered = append(g.buffered, bufferedSessionChunk{sessionID: sessionID, chunk: chunk})
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	g.forward(sessionID, chunk)
}

func (g *sessionOutputGate) Activate() {
	if g == nil || g.forward == nil {
		return
	}

	g.mu.Lock()
	if g.dropped || g.active {
		g.mu.Unlock()
		return
	}
	g.draining = true
	for {
		if g.dropped {
			g.draining = false
			g.buffered = nil
			g.mu.Unlock()
			return
		}

		buffered := append([]bufferedSessionChunk(nil), g.buffered...)
		g.buffered = nil
		g.mu.Unlock()

		for _, item := range buffered {
			g.forward(item.sessionID, item.chunk)
		}

		g.mu.Lock()
		if len(g.buffered) == 0 {
			g.draining = false
			g.active = true
			g.mu.Unlock()
			return
		}
	}
}

func (g *sessionOutputGate) Drop() {
	if g == nil {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropped = true
	g.buffered = nil
}
