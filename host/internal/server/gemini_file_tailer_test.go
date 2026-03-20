package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	testGeminiTailPollInterval = 10 * time.Millisecond
	testGeminiTailWait         = 150 * time.Millisecond
)

// writeGeminiJSON marshals a geminiSessionFile and writes it to the given path.
func writeGeminiJSON(t *testing.T, path string, session geminiSessionFile) {
	t.Helper()
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func makeGeminiSession(sessionID string, msgs ...geminiMessage) geminiSessionFile {
	return geminiSessionFile{
		SessionID:   sessionID,
		ProjectHash: "abc123",
		StartTime:   1700000000.0,
		LastUpdated: 1700000001.0,
		Messages:    msgs,
	}
}

func makeGeminiMsg(msgType, text string) geminiMessage {
	return geminiMessage{
		Type:    msgType,
		Content: text,
	}
}

// TestGeminiFileTailer_InitialRead verifies that OnMessages is called for the
// initial read when Start() is invoked.
func TestGeminiFileTailer_InitialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
		makeGeminiMsg("model", "hi there"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var deliveredMsgs []geminiMessage
	var deliveredSessionID string

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliveredMsgs = messages
			deliveredSessionID = sessionID
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(deliveredMsgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(deliveredMsgs))
	}
	if deliveredSessionID != "sess-1" {
		t.Fatalf("got sessionID %q, want %q", deliveredSessionID, "sess-1")
	}
	if deliveredMsgs[0].Type != "user" {
		t.Fatalf("first message type=%q, want %q", deliveredMsgs[0].Type, "user")
	}
	if deliveredMsgs[1].Content != "hi there" {
		t.Fatalf("second message content=%q, want %q", deliveredMsgs[1].Content, "hi there")
	}
}

// TestGeminiFileTailer_MtimeSizeChange verifies that updated messages are
// delivered when the file mtime or size changes.
func TestGeminiFileTailer_MtimeSizeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var deliverCount int
	var lastMsgs []geminiMessage

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			lastMsgs = messages
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Update the file with an additional message.
	session2 := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
		makeGeminiMsg("model", "world"),
	)
	writeGeminiJSON(t, path, session2)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if deliverCount < 2 {
		t.Fatalf("deliverCount=%d, want >= 2 (initial + update)", deliverCount)
	}
	if len(lastMsgs) != 2 {
		t.Fatalf("last delivery had %d messages, want 2", len(lastMsgs))
	}
}

// TestGeminiFileTailer_InvalidJSON verifies that invalid JSON (partial write)
// does not crash, does not call OnReset, and the next valid write delivers.
func TestGeminiFileTailer_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	// Start with a valid file.
	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var deliverCount int
	var resetCount int
	var lastMsgs []geminiMessage

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			lastMsgs = messages
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Write invalid JSON (simulating a partial write).
	if err := os.WriteFile(path, []byte(`{"sessionId":"sess-1","messages":[{"role":"us`), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(testGeminiTailWait)

	mu.Lock()
	countAfterInvalid := deliverCount
	resetAfterInvalid := resetCount
	mu.Unlock()

	// Write valid JSON again.
	session2 := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
		makeGeminiMsg("model", "recovered"),
	)
	writeGeminiJSON(t, path, session2)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	// Should NOT have called OnReset for invalid JSON.
	if resetAfterInvalid != 0 {
		t.Fatalf("resetCount after invalid JSON = %d, want 0", resetAfterInvalid)
	}

	// Should have only delivered once for initial read (not for invalid JSON).
	if countAfterInvalid != 1 {
		t.Fatalf("deliverCount after invalid JSON = %d, want 1 (initial only)", countAfterInvalid)
	}

	// After recovery, should have delivered again.
	if deliverCount < 2 {
		t.Fatalf("deliverCount after recovery = %d, want >= 2", deliverCount)
	}
	if len(lastMsgs) != 2 {
		t.Fatalf("last delivery had %d messages, want 2", len(lastMsgs))
	}
}

// TestGeminiFileTailer_InodeChange verifies that replacing the file (new inode)
// triggers OnReset and re-reads from beginning.
func TestGeminiFileTailer_InodeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "original"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var resetCount int
	var deliverCount int
	var lastMsgs []geminiMessage

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			lastMsgs = messages
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Replace the file via rename (creates a new inode at the same path).
	tmpPath := filepath.Join(dir, "session.json.tmp")
	session2 := makeGeminiSession("sess-2",
		makeGeminiMsg("user", "replaced"),
		makeGeminiMsg("model", "new content"),
	)
	writeGeminiJSON(t, tmpPath, session2)
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if resetCount < 1 {
		t.Fatalf("resetCount=%d, want >= 1", resetCount)
	}
	if deliverCount < 2 {
		t.Fatalf("deliverCount=%d, want >= 2 (initial + post-inode-change)", deliverCount)
	}
	if len(lastMsgs) != 2 {
		t.Fatalf("last delivery had %d messages, want 2", len(lastMsgs))
	}
}

// TestGeminiFileTailer_Truncation verifies that file truncation (size shrinks)
// triggers OnReset and re-reads.
func TestGeminiFileTailer_Truncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	// Write a large-ish initial session.
	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "first message with some extra content to make it bigger"),
		makeGeminiMsg("model", "response with even more content to ensure larger size"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var resetCount int
	var deliverCount int

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Truncate: write a smaller file (same path = same inode on most OS).
	smallSession := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hi"),
	)
	writeGeminiJSON(t, path, smallSession)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if resetCount < 1 {
		t.Fatalf("resetCount=%d, want >= 1 (truncation should trigger reset)", resetCount)
	}
	if deliverCount < 2 {
		t.Fatalf("deliverCount=%d, want >= 2 (initial + post-truncation)", deliverCount)
	}
}

// TestGeminiFileTailer_IdentityMismatch verifies that a different sessionID
// triggers OnReset before delivering messages.
func TestGeminiFileTailer_IdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-A",
		makeGeminiMsg("user", "hello A"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var resetCount int
	var deliverCount int
	var deliveredSessionIDs []string

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			deliveredSessionIDs = append(deliveredSessionIDs, sessionID)
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Overwrite with a different session ID (same file, different identity).
	session2 := makeGeminiSession("sess-B",
		makeGeminiMsg("user", "hello B"),
	)
	writeGeminiJSON(t, path, session2)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	// Identity mismatch should trigger OnReset.
	if resetCount < 1 {
		t.Fatalf("resetCount=%d, want >= 1 (identity mismatch should trigger reset)", resetCount)
	}
	if deliverCount < 2 {
		t.Fatalf("deliverCount=%d, want >= 2", deliverCount)
	}
	// Verify both sessions were delivered.
	foundA, foundB := false, false
	for _, id := range deliveredSessionIDs {
		if id == "sess-A" {
			foundA = true
		}
		if id == "sess-B" {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Fatalf("expected both sess-A and sess-B to be delivered, got %v", deliveredSessionIDs)
	}
}

// TestGeminiFileTailer_SameSessionNoReset verifies that updates with the same
// sessionID do NOT trigger OnReset.
func TestGeminiFileTailer_SameSessionNoReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-stable",
		makeGeminiMsg("user", "hello"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var resetCount int
	var deliverCount int

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			mu.Unlock()
		},
		OnReset: func() {
			mu.Lock()
			resetCount++
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Update file with same sessionID but new content.
	session2 := makeGeminiSession("sess-stable",
		makeGeminiMsg("user", "hello"),
		makeGeminiMsg("model", "world"),
	)
	writeGeminiJSON(t, path, session2)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if resetCount != 0 {
		t.Fatalf("resetCount=%d, want 0 (same sessionID should not trigger reset)", resetCount)
	}
	if deliverCount < 2 {
		t.Fatalf("deliverCount=%d, want >= 2", deliverCount)
	}
}

// TestGeminiFileTailer_CleanShutdown verifies that Stop() returns promptly.
func TestGeminiFileTailer_CleanShutdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
	)
	writeGeminiJSON(t, path, session)

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	done := make(chan struct{})
	go func() {
		tailer.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within timeout — goroutine leak")
	}
}

// TestGeminiFileTailer_DoubleStop verifies that calling Stop() twice is safe.
func TestGeminiFileTailer_DoubleStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "hello"),
	)
	writeGeminiJSON(t, path, session)

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	tailer.Stop()
	// Second stop must not panic or hang.
	tailer.Stop()
}

// TestGeminiFileTailer_FileNotFound verifies that a nonexistent file triggers
// OnError without panicking.
func TestGeminiFileTailer_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	var mu sync.Mutex
	var errors []error

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnError: func(err error) {
			mu.Lock()
			errors = append(errors, err)
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(errors) == 0 {
		t.Fatal("expected errors for nonexistent file, got none")
	}
}

// TestGeminiFileTailer_RapidWrites verifies that when multiple writes happen
// between polls, only the latest state is delivered.
func TestGeminiFileTailer_RapidWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "v1"),
	)
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var deliveredTexts []string

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: 50 * time.Millisecond, // Slower poll to allow rapid writes between polls.
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			if len(messages) > 0 {
				deliveredTexts = append(deliveredTexts, messages[len(messages)-1].Content)
			}
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)

	// Rapid-fire writes (both should complete before the next poll).
	session2 := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "v2"),
	)
	writeGeminiJSON(t, path, session2)

	session3 := makeGeminiSession("sess-1",
		makeGeminiMsg("user", "v3"),
	)
	writeGeminiJSON(t, path, session3)

	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	// The last delivered text should be "v3" (the latest state).
	if len(deliveredTexts) == 0 {
		t.Fatal("no deliveries occurred")
	}
	lastText := deliveredTexts[len(deliveredTexts)-1]
	if lastText != "v3" {
		t.Fatalf("last delivered text=%q, want %q", lastText, "v3")
	}
}

// TestGeminiFileTailer_EmptyMessages verifies that an empty messages array is
// delivered (not skipped).
func TestGeminiFileTailer_EmptyMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	// Session file with zero messages.
	session := makeGeminiSession("sess-empty")
	writeGeminiJSON(t, path, session)

	var mu sync.Mutex
	var deliverCount int
	var lastMsgCount int = -1

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			mu.Lock()
			deliverCount++
			lastMsgCount = len(messages)
			mu.Unlock()
		},
	})
	tailer.Start()
	time.Sleep(testGeminiTailWait)
	tailer.Stop()

	mu.Lock()
	defer mu.Unlock()

	if deliverCount == 0 {
		t.Fatal("expected at least 1 delivery for empty messages, got 0")
	}
	if lastMsgCount != 0 {
		t.Fatalf("expected 0 messages in delivery, got %d", lastMsgCount)
	}
}

// TestGeminiFileTailer_OnMessagesCalledOnInitialRead verifies that the
// OnMessages callback fires for the initial read (not just subsequent polls).
func TestGeminiFileTailer_OnMessagesCalledOnInitialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	session := makeGeminiSession("sess-init",
		makeGeminiMsg("user", "first"),
	)
	writeGeminiJSON(t, path, session)

	delivered := make(chan struct{}, 1)

	tailer := NewGeminiFileTailer(GeminiFileTailerConfig{
		FilePath:     path,
		PollInterval: testGeminiTailPollInterval,
		OnMessages: func(messages []geminiMessage, sessionID string) {
			select {
			case delivered <- struct{}{}:
			default:
			}
		},
	})
	tailer.Start()

	// OnMessages should be called almost immediately for the initial read.
	select {
	case <-delivered:
		// Success: initial read delivered.
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessages was not called for initial read within timeout")
	}

	tailer.Stop()
}
