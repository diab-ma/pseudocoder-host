package server

import (
	"reflect"
	"sync"
	"testing"
)

func TestSessionOutputGateActivatePreservesBufferedOrderAcrossConcurrentCallback(t *testing.T) {
	var (
		gate      *sessionOutputGate
		mu        sync.Mutex
		forwarded []string
	)

	gate = newSessionOutputGate(func(sessionID, chunk string) {
		mu.Lock()
		forwarded = append(forwarded, sessionID+":"+chunk)
		mu.Unlock()

		if chunk == "old" {
			gate.Callback("session-1", "new")
		}
	})

	gate.Callback("session-1", "old")
	gate.Activate()

	mu.Lock()
	defer mu.Unlock()

	want := []string{"session-1:old", "session-1:new"}
	if !reflect.DeepEqual(forwarded, want) {
		t.Fatalf("forwarded = %#v, want %#v", forwarded, want)
	}
}
