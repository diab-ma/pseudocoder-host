package pty

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// =============================================================================
// Unit 9.1: Session ID and Metadata Tests
// =============================================================================

// TestSession_IDField verifies the ID field is set from config.
func TestSession_IDField(t *testing.T) {
	customID := "my-session-id"
	s := NewSession(SessionConfig{ID: customID})

	if s.ID != customID {
		t.Errorf("Expected ID %q, got %q", customID, s.ID)
	}
}

// TestSession_IDFieldEmpty verifies ID defaults to empty string.
func TestSession_IDFieldEmpty(t *testing.T) {
	s := NewSession(SessionConfig{})

	if s.ID != "" {
		t.Errorf("Expected empty ID by default, got %q", s.ID)
	}
}

// TestSession_CommandAndArgs verifies Command and Args are set during Start.
func TestSession_CommandAndArgs(t *testing.T) {
	s := NewSession(SessionConfig{})

	if err := s.Start("/bin/sh", "-c", "echo test"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if s.Command != "/bin/sh" {
		t.Errorf("Expected Command '/bin/sh', got %q", s.Command)
	}

	expectedArgs := []string{"-c", "echo test"}
	if len(s.Args) != len(expectedArgs) {
		t.Fatalf("Expected %d args, got %d", len(expectedArgs), len(s.Args))
	}
	for i, arg := range expectedArgs {
		if s.Args[i] != arg {
			t.Errorf("Arg %d: expected %q, got %q", i, arg, s.Args[i])
		}
	}

	waitDone(t, s)
}

// TestSession_CreatedAt verifies CreatedAt is set during Start.
func TestSession_CreatedAt(t *testing.T) {
	s := NewSession(SessionConfig{})

	// CreatedAt should be zero before start
	if !s.CreatedAt.IsZero() {
		t.Error("CreatedAt should be zero before Start")
	}

	before := time.Now()
	if err := s.Start("/bin/sh", "-c", "echo test"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	after := time.Now()

	if s.CreatedAt.Before(before) || s.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v should be between %v and %v", s.CreatedAt, before, after)
	}

	waitDone(t, s)
}

// TestSession_OnOutputWithIDCallback verifies OnOutputWithID is called with session ID.
// Note: With chunked output, callbacks may receive data in 1 or more chunks,
// so we verify combined content rather than exact callback count.
func TestSession_OnOutputWithIDCallback(t *testing.T) {
	var mu sync.Mutex
	var receivedID string
	var receivedChunks []string

	sessionID := "test-session-123"
	s := NewSession(SessionConfig{
		ID: sessionID,
		OnOutputWithID: func(id, chunk string) {
			mu.Lock()
			receivedID = id
			receivedChunks = append(receivedChunks, normalizeNewlines(chunk))
			mu.Unlock()
		},
	})

	if err := s.Start("/bin/sh", "-c", "printf 'line1\nline2\n'"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	defer mu.Unlock()

	if receivedID != sessionID {
		t.Errorf("Expected session ID %q, got %q", sessionID, receivedID)
	}

	// Verify combined content (chunks may arrive in 1 or more callbacks)
	combined := strings.Join(receivedChunks, "")
	if combined != "line1\nline2\n" {
		t.Errorf("Expected combined output 'line1\\nline2\\n', got %q", combined)
	}

	// Also verify ring buffer still has correct lines
	lines := s.Lines()
	if len(lines) != 2 {
		t.Errorf("Expected 2 lines in buffer, got %d: %v", len(lines), lines)
	}
}

// TestSession_OnOutputWithIDPrecedence verifies OnOutputWithID takes precedence over OnOutput.
func TestSession_OnOutputWithIDPrecedence(t *testing.T) {
	onOutputCalled := false
	onOutputWithIDCalled := false

	s := NewSession(SessionConfig{
		ID: "test-session",
		OnOutput: func(line string) {
			onOutputCalled = true
		},
		OnOutputWithID: func(id, line string) {
			onOutputWithIDCalled = true
		},
	})

	if err := s.Start("/bin/sh", "-c", "echo test"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	if onOutputCalled {
		t.Error("OnOutput should NOT be called when OnOutputWithID is set")
	}
	if !onOutputWithIDCalled {
		t.Error("OnOutputWithID SHOULD be called")
	}
}

// TestSession_OnOutputFallback verifies OnOutput is called when OnOutputWithID is nil.
func TestSession_OnOutputFallback(t *testing.T) {
	var mu sync.Mutex
	var receivedLines []string

	s := NewSession(SessionConfig{
		ID: "test-session",
		OnOutput: func(line string) {
			mu.Lock()
			receivedLines = append(receivedLines, line)
			mu.Unlock()
		},
		// OnOutputWithID is nil
	})

	if err := s.Start("/bin/sh", "-c", "echo fallback"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	defer mu.Unlock()

	if len(receivedLines) == 0 {
		t.Error("OnOutput should be called as fallback")
	}
}

func waitDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit in time")
	}
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func TestSessionCapturesOutputAndRingBuffer(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 2})
	if err := s.Start("/bin/sh", "-c", "printf 'one\ntwo\nthree\n'"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	lines := s.Lines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%#v)", len(lines), lines)
	}
	if normalizeNewlines(lines[0]) != "two\n" || normalizeNewlines(lines[1]) != "three\n" {
		t.Fatalf("unexpected ring buffer contents: %#v", lines)
	}
}

func TestSessionRejectsSecondStart(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/bin/sh", "-c", "sleep 1"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	if err := s.Start("/bin/sh", "-c", "echo nope"); err == nil {
		t.Fatal("expected error on second Start, got nil")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	waitDone(t, s)
}

func TestSessionStopEndsProcess(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/bin/sh", "-c", "sleep 5"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	waitDone(t, s)
}

func TestSessionWriteWithoutStart(t *testing.T) {
	s := NewSession(SessionConfig{})
	if _, err := s.Write([]byte("hi")); err == nil {
		t.Fatal("expected error when writing before start")
	}
}

func TestSessionStartInvalidCommand(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/not/a/command"); err == nil {
		t.Fatal("expected error for invalid command")
	}
}

func TestSessionSanitizesNonUTF8(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/bin/sh", "-c", "printf '\\xff\\n'"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	lines := s.Lines()
	if len(lines) == 0 {
		t.Fatal("expected at least one line")
	}
	if !utf8.ValidString(lines[0]) {
		t.Fatalf("expected valid UTF-8 output, got %#v", lines[0])
	}
}

// TestSessionOnOutputCallback verifies the OnOutput callback is invoked with output data.
// Note: With chunked output, callbacks may receive data in 1 or more chunks,
// so we verify combined content rather than exact callback count.
func TestSessionOnOutputCallback(t *testing.T) {
	var mu sync.Mutex
	var receivedChunks []string
	s := NewSession(SessionConfig{
		OnOutput: func(chunk string) {
			mu.Lock()
			receivedChunks = append(receivedChunks, normalizeNewlines(chunk))
			mu.Unlock()
		},
	})

	if err := s.Start("/bin/sh", "-c", "printf 'line1\nline2\nline3\n'"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	combined := strings.Join(receivedChunks, "")
	mu.Unlock()

	// Verify combined content
	if combined != "line1\nline2\nline3\n" {
		t.Fatalf("expected combined output 'line1\\nline2\\nline3\\n', got %q", combined)
	}

	// Also verify ring buffer has correct lines
	lines := s.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines in buffer, got %d: %v", len(lines), lines)
	}
}

// TestSessionOnOutputCallbackOrder verifies output data is delivered in order.
// Note: With chunked output, callbacks may receive data in 1 or more chunks,
// so we verify combined content order rather than exact line-by-line matching.
func TestSessionOnOutputCallbackOrder(t *testing.T) {
	var mu sync.Mutex
	var receivedChunks []string
	s := NewSession(SessionConfig{
		OnOutput: func(chunk string) {
			mu.Lock()
			receivedChunks = append(receivedChunks, normalizeNewlines(chunk))
			mu.Unlock()
		},
	})

	if err := s.Start("/bin/sh", "-c", "printf 'first\nsecond\nthird\n'"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	combined := strings.Join(receivedChunks, "")
	mu.Unlock()

	// Verify combined content preserves order
	expected := "first\nsecond\nthird\n"
	if combined != expected {
		t.Errorf("expected combined output %q, got %q", expected, combined)
	}

	// Also verify ring buffer has correct lines in order
	lines := s.Lines()
	expectedLines := []string{"first\n", "second\n", "third\n"}
	if len(lines) != len(expectedLines) {
		t.Fatalf("expected %d lines in buffer, got %d: %v", len(expectedLines), len(lines), lines)
	}
	for i, exp := range expectedLines {
		if normalizeNewlines(lines[i]) != exp {
			t.Errorf("buffer line %d: expected %q, got %q", i, exp, lines[i])
		}
	}
}

// =============================================================================
// Unit 16.4: Chunked Output Tests
// =============================================================================

// TestSession_ChunkedOutputPartialLine verifies partial lines (no newline) are
// forwarded to callbacks immediately and flushed to ring buffer at EOF.
func TestSession_ChunkedOutputPartialLine(t *testing.T) {
	var mu sync.Mutex
	var receivedChunks []string

	s := NewSession(SessionConfig{
		HistoryLines: 100,
		OnOutput: func(chunk string) {
			mu.Lock()
			receivedChunks = append(receivedChunks, chunk)
			mu.Unlock()
		},
	})

	// Output a partial line (no trailing newline) - simulates cursor movement
	if err := s.Start("/bin/sh", "-c", "printf 'partial'"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	combined := strings.Join(receivedChunks, "")
	mu.Unlock()

	// Callback should receive the partial line
	if combined != "partial" {
		t.Errorf("Expected callback to receive 'partial', got %q", combined)
	}

	// Buffer should also have it (flushed at EOF)
	lines := s.Lines()
	if len(lines) != 1 || lines[0] != "partial" {
		t.Errorf("Buffer should contain partial line 'partial', got %v", lines)
	}
}

// TestSession_RingBufferStoresCompleteLines verifies ring buffer stores only
// complete lines during normal operation (partial lines buffered until newline).
func TestSession_RingBufferStoresCompleteLines(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	// Output: complete line, then partial
	if err := s.Start("/bin/sh", "-c", "printf 'complete\npartial'"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	lines := s.Lines()
	// Should have 2 entries: "complete\n" and "partial" (flushed at EOF)
	if len(lines) != 2 {
		t.Fatalf("Expected 2 lines, got %d: %v", len(lines), lines)
	}
	if normalizeNewlines(lines[0]) != "complete\n" {
		t.Errorf("First line should be 'complete\\n', got %q", lines[0])
	}
	if lines[1] != "partial" {
		t.Errorf("Second line should be 'partial', got %q", lines[1])
	}
}

// TestSession_ChunkedOutputMixedLines verifies mixed complete/partial lines
// are handled correctly - complete lines stored immediately, partials at EOF.
func TestSession_ChunkedOutputMixedLines(t *testing.T) {
	var mu sync.Mutex
	var receivedChunks []string

	s := NewSession(SessionConfig{
		HistoryLines: 100,
		OnOutput: func(chunk string) {
			mu.Lock()
			receivedChunks = append(receivedChunks, normalizeNewlines(chunk))
			mu.Unlock()
		},
	})

	// Mix of complete lines and partial line at end
	if err := s.Start("/bin/sh", "-c", "printf 'line1\nline2\npartial'"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	combined := strings.Join(receivedChunks, "")
	mu.Unlock()

	// Callback should receive all output
	expected := "line1\nline2\npartial"
	if combined != expected {
		t.Errorf("Expected callback combined %q, got %q", expected, combined)
	}

	// Ring buffer should have 3 entries
	lines := s.Lines()
	if len(lines) != 3 {
		t.Fatalf("Expected 3 lines in buffer, got %d: %v", len(lines), lines)
	}
	if normalizeNewlines(lines[0]) != "line1\n" {
		t.Errorf("Buffer line 0: expected 'line1\\n', got %q", lines[0])
	}
	if normalizeNewlines(lines[1]) != "line2\n" {
		t.Errorf("Buffer line 1: expected 'line2\\n', got %q", lines[1])
	}
	if lines[2] != "partial" {
		t.Errorf("Buffer line 2: expected 'partial', got %q", lines[2])
	}
}

// TestSessionErrorAfterNormalExit verifies error handling after normal exit.
// Note: On Linux, when a PTY slave closes, subsequent reads from the master
// may return an I/O error - this is expected PTY behavior, not a real failure.
func TestSessionErrorAfterNormalExit(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/bin/sh", "-c", "echo done"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	err := s.Error()
	// Allow nil or I/O error (expected on PTY close)
	if err != nil && !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("expected nil or I/O error after normal exit, got %v", err)
	}
}

// TestSessionIsRunningState verifies IsRunning returns correct state.
func TestSessionIsRunningState(t *testing.T) {
	s := NewSession(SessionConfig{})

	// Before start
	if s.IsRunning() {
		t.Error("expected IsRunning false before start")
	}

	if err := s.Start("/bin/sh", "-c", "sleep 0.5"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// While running
	if !s.IsRunning() {
		t.Error("expected IsRunning true after start")
	}

	waitDone(t, s)

	// After exit
	if s.IsRunning() {
		t.Error("expected IsRunning false after exit")
	}
}

// TestSessionStopIdempotent verifies Stop can be called multiple times safely.
func TestSessionStopIdempotent(t *testing.T) {
	s := NewSession(SessionConfig{})
	if err := s.Start("/bin/sh", "-c", "sleep 5"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Call Stop multiple times
	if err := s.Stop(); err != nil {
		t.Errorf("first stop failed: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("second stop failed: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("third stop failed: %v", err)
	}

	waitDone(t, s)
}

// TestSessionBufferDirect verifies Buffer() returns the underlying ring buffer.
func TestSessionBufferDirect(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	buf := s.Buffer()
	if buf == nil {
		t.Fatal("Buffer() returned nil")
	}

	if buf.Capacity() != 100 {
		t.Errorf("expected capacity 100, got %d", buf.Capacity())
	}
}

// =============================================================================
// Unit 7.8: PTY Lifecycle Race Condition Tests
// =============================================================================

// TestSession_StopDuringOutput verifies that calling Stop() while the session
// is producing output does not cause a panic.
func TestSession_StopDuringOutput(t *testing.T) {
	s := NewSession(SessionConfig{})

	// Start a command that produces continuous output
	// Using a loop that echoes rapidly
	if err := s.Start("/bin/sh", "-c", "while true; do echo x; done"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Wait briefly for output to start
	time.Sleep(10 * time.Millisecond)

	// Stop while output is being produced
	if err := s.Stop(); err != nil {
		t.Errorf("stop failed: %v", err)
	}

	// Wait for session to end
	select {
	case <-s.Done():
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit in time")
	}
}

// TestSession_ConcurrentStops verifies that calling Stop() from multiple
// goroutines does not cause a panic (double-close protection).
func TestSession_ConcurrentStops(t *testing.T) {
	s := NewSession(SessionConfig{})

	if err := s.Start("/bin/sh", "-c", "sleep 5"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	var wg sync.WaitGroup

	// Call Stop from multiple goroutines
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Stop()
		}()
	}

	wg.Wait()
	waitDone(t, s)
	// If we get here without panic, the test passes
}

// TestSession_NaturalExitDuringStop verifies that when a process exits naturally
// at the same moment Stop() is called, there is no panic from double-close.
func TestSession_NaturalExitDuringStop(t *testing.T) {
	for i := 0; i < 10; i++ {
		// Run multiple times to increase chance of hitting the race
		t.Run(strings.Repeat("try", i+1), func(t *testing.T) {
			s := NewSession(SessionConfig{})

			// Start a very short-lived command
			if err := s.Start("/bin/sh", "-c", "exit 0"); err != nil {
				t.Fatalf("start failed: %v", err)
			}

			// Immediately call Stop (may race with natural exit)
			_ = s.Stop()

			select {
			case <-s.Done():
				// Success
			case <-time.After(2 * time.Second):
				t.Fatal("session did not exit in time")
			}
		})
	}
}

// TestSession_OutputCallbackDuringStop verifies that the OnOutput callback
// does not panic when Stop() is called while output is being processed.
func TestSession_OutputCallbackDuringStop(t *testing.T) {
	var outputCount int
	var mu sync.Mutex

	s := NewSession(SessionConfig{
		OnOutput: func(line string) {
			mu.Lock()
			outputCount++
			mu.Unlock()
			// Simulate slow callback
			time.Sleep(time.Millisecond)
		},
	})

	if err := s.Start("/bin/sh", "-c", "for i in 1 2 3 4 5; do echo line$i; done"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Give callback some time to start processing
	time.Sleep(5 * time.Millisecond)

	// Stop while callbacks may be running
	if err := s.Stop(); err != nil {
		t.Errorf("stop failed: %v", err)
	}

	waitDone(t, s)

	mu.Lock()
	t.Logf("received %d output lines before stop", outputCount)
	mu.Unlock()
}

// TestSession_WriteDuringStop verifies that Write() during Stop() does not panic.
func TestSession_WriteDuringStop(t *testing.T) {
	s := NewSession(SessionConfig{})

	if err := s.Start("/bin/sh"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Start writing in a goroutine
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = s.Write([]byte("echo test\n"))
			time.Sleep(time.Millisecond)
		}
	}()

	// Wait briefly then stop
	time.Sleep(10 * time.Millisecond)
	if err := s.Stop(); err != nil {
		t.Errorf("stop failed: %v", err)
	}

	waitDone(t, s)
}

// =============================================================================
// Unit 12.3: TmuxSession Field Tests
// =============================================================================

// TestSession_TmuxSessionField verifies the TmuxSession field can be set and read.
func TestSession_TmuxSessionField(t *testing.T) {
	s := NewSession(SessionConfig{})

	// Initially empty
	if s.TmuxSession != "" {
		t.Errorf("expected empty TmuxSession by default, got %q", s.TmuxSession)
	}

	// Set the field
	s.TmuxSession = "main"
	if s.TmuxSession != "main" {
		t.Errorf("expected TmuxSession 'main', got %q", s.TmuxSession)
	}
}

// TestSession_GetTmuxSession verifies the thread-safe getter.
func TestSession_GetTmuxSession(t *testing.T) {
	s := NewSession(SessionConfig{})

	// Initially empty
	if got := s.GetTmuxSession(); got != "" {
		t.Errorf("expected empty TmuxSession, got %q", got)
	}

	// Set and verify through getter
	s.TmuxSession = "dev-session"
	if got := s.GetTmuxSession(); got != "dev-session" {
		t.Errorf("expected 'dev-session', got %q", got)
	}
}

// TestSession_IsTmuxSession verifies IsTmuxSession returns correct values.
func TestSession_IsTmuxSession(t *testing.T) {
	s := NewSession(SessionConfig{})

	// Initially false (empty string)
	if s.IsTmuxSession() {
		t.Error("expected IsTmuxSession false for empty TmuxSession")
	}

	// Set and verify
	s.TmuxSession = "my-tmux"
	if !s.IsTmuxSession() {
		t.Error("expected IsTmuxSession true after setting TmuxSession")
	}

	// Clear and verify
	s.TmuxSession = ""
	if s.IsTmuxSession() {
		t.Error("expected IsTmuxSession false after clearing TmuxSession")
	}
}

// TestSession_TmuxSessionConcurrentAccess verifies thread safety of TmuxSession access.
func TestSession_TmuxSessionConcurrentAccess(t *testing.T) {
	s := NewSession(SessionConfig{})
	s.TmuxSession = "concurrent-session"

	var wg sync.WaitGroup

	// Multiple goroutines reading TmuxSession
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.GetTmuxSession()
				_ = s.IsTmuxSession()
			}
		}()
	}

	wg.Wait()
	// If we get here without race detector errors, the test passes
}

// =============================================================================
// Unit 12.6: PrependToBuffer Tests
// =============================================================================

// TestPrependToBuffer_Simple verifies single line is added to buffer.
func TestPrependToBuffer_Simple(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	s.PrependToBuffer("hello world")

	lines := s.Lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}

	if lines[0] != "hello world\n" {
		t.Errorf("expected 'hello world\\n', got %q", lines[0])
	}
}

// TestPrependToBuffer_MultiLine verifies multiple lines are split and added.
func TestPrependToBuffer_MultiLine(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	s.PrependToBuffer("line1\nline2\nline3")

	lines := s.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}

	expected := []string{"line1\n", "line2\n", "line3\n"}
	for i, exp := range expected {
		if lines[i] != exp {
			t.Errorf("line %d: expected %q, got %q", i, exp, lines[i])
		}
	}
}

// TestPrependToBuffer_EmptyContent verifies empty string is a no-op.
func TestPrependToBuffer_EmptyContent(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	s.PrependToBuffer("")

	lines := s.Lines()
	if len(lines) != 0 {
		t.Errorf("expected 0 lines for empty content, got %d", len(lines))
	}
}

// TestPrependToBuffer_TrailingNewlines verifies only the trailing Split artifact is skipped.
// Blank lines in content are preserved as they may represent legitimate terminal output.
func TestPrependToBuffer_TrailingNewlines(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	// Content with trailing newlines: "line1\nline2\n\n\n"
	// Split gives: ["line1", "line2", "", "", ""]
	// Only the last "" (Split artifact) is skipped, blank lines preserved
	s.PrependToBuffer("line1\nline2\n\n\n")

	lines := s.Lines()
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (blank lines preserved), got %d: %v", len(lines), lines)
	}

	expected := []string{"line1\n", "line2\n", "\n", "\n"}
	for i, exp := range expected {
		if lines[i] != exp {
			t.Errorf("line %d: expected %q, got %q", i, exp, lines[i])
		}
	}
}

// TestPrependToBuffer_OnlyNewlines verifies blank lines are preserved.
// "\n\n\n" represents 3 blank lines in terminal output.
func TestPrependToBuffer_OnlyNewlines(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	// "\n\n\n" split gives ["", "", "", ""], keep all except last
	s.PrependToBuffer("\n\n\n")

	lines := s.Lines()
	if len(lines) != 3 {
		t.Errorf("expected 3 blank lines, got %d: %v", len(lines), lines)
	}

	// Each should be a blank line
	for i, line := range lines {
		if line != "\n" {
			t.Errorf("line %d: expected blank line '\\n', got %q", i, line)
		}
	}
}

// TestPrependToBuffer_BlankLinesInMiddle verifies legitimate blank lines are preserved.
func TestPrependToBuffer_BlankLinesInMiddle(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	// Simulate tmux output with blank line between commands
	// "$ echo hello\nhello\n\n$ echo world\nworld\n"
	s.PrependToBuffer("$ echo hello\nhello\n\n$ echo world\nworld\n")

	lines := s.Lines()
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (including blank), got %d: %v", len(lines), lines)
	}

	expected := []string{
		"$ echo hello\n",
		"hello\n",
		"\n", // Legitimate blank line - must be preserved
		"$ echo world\n",
		"world\n",
	}
	for i, exp := range expected {
		if lines[i] != exp {
			t.Errorf("line %d: expected %q, got %q", i, exp, lines[i])
		}
	}
}

// TestPrependToBuffer_WithTerminalOutput verifies prepended content appears before PTY output.
func TestPrependToBuffer_WithTerminalOutput(t *testing.T) {
	s := NewSession(SessionConfig{HistoryLines: 100})

	// Prepend some history
	s.PrependToBuffer("history1\nhistory2")

	// Start a command that produces output
	if err := s.Start("/bin/sh", "-c", "printf 'new1\nnew2\n'"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	waitDone(t, s)

	lines := s.Lines()
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (2 history + 2 new), got %d: %v", len(lines), lines)
	}

	// History should come first (prepended before PTY started)
	if lines[0] != "history1\n" {
		t.Errorf("expected first line 'history1\\n', got %q", lines[0])
	}
	if lines[1] != "history2\n" {
		t.Errorf("expected second line 'history2\\n', got %q", lines[1])
	}
}
