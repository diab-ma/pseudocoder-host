package pty

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// SessionManager CRUD Tests
// =============================================================================

// TestSessionManager_CreateAndGet verifies basic Create/Get workflow.
func TestSessionManager_CreateAndGet(t *testing.T) {
	mgr := NewSessionManager()

	session, err := mgr.Create(SessionConfig{HistoryLines: 100})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if session == nil {
		t.Fatal("Create returned nil session")
	}

	if session.ID == "" {
		t.Error("Session ID should not be empty")
	}

	// Get should return the same session
	retrieved := mgr.Get(session.ID)
	if retrieved != session {
		t.Error("Get did not return the same session instance")
	}
}

// TestSessionManager_CreateWithID verifies creating with a specific ID.
func TestSessionManager_CreateWithID(t *testing.T) {
	mgr := NewSessionManager()

	customID := "my-custom-session-id"
	session, err := mgr.CreateWithID(customID, SessionConfig{})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}

	if session.ID != customID {
		t.Errorf("Expected ID %q, got %q", customID, session.ID)
	}

	// Get should return the session
	retrieved := mgr.Get(customID)
	if retrieved != session {
		t.Error("Get did not return the session with custom ID")
	}
}

// TestSessionManager_CreateDuplicateID verifies that duplicate IDs are rejected.
func TestSessionManager_CreateDuplicateID(t *testing.T) {
	mgr := NewSessionManager()

	id := "duplicate-id"
	_, err := mgr.CreateWithID(id, SessionConfig{})
	if err != nil {
		t.Fatalf("First CreateWithID failed: %v", err)
	}

	// Second create with same ID should fail
	_, err = mgr.CreateWithID(id, SessionConfig{})
	if err != ErrSessionExists {
		t.Errorf("Expected ErrSessionExists, got %v", err)
	}
}

// TestSessionManager_GetNonexistent verifies Get returns nil for unknown IDs.
func TestSessionManager_GetNonexistent(t *testing.T) {
	mgr := NewSessionManager()

	session := mgr.Get("nonexistent-id")
	if session != nil {
		t.Error("Expected nil for nonexistent session")
	}
}

// TestSessionManager_Close verifies Close stops session and removes from manager.
func TestSessionManager_Close(t *testing.T) {
	mgr := NewSessionManager()

	session, err := mgr.Create(SessionConfig{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	id := session.ID

	// Start a long-running command
	if err := session.Start("/bin/sh", "-c", "sleep 10"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Verify it's running
	if !session.IsRunning() {
		t.Error("Session should be running")
	}

	// Close should stop and remove
	if err := mgr.Close(id); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Session should no longer be in manager
	if mgr.Get(id) != nil {
		t.Error("Session should be removed after Close")
	}

	// Session should be stopped
	if session.IsRunning() {
		t.Error("Session should not be running after Close")
	}
}

// TestSessionManager_CloseNonexistent verifies Close returns error for unknown IDs.
func TestSessionManager_CloseNonexistent(t *testing.T) {
	mgr := NewSessionManager()

	err := mgr.Close("nonexistent-id")
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound, got %v", err)
	}
}

// TestSessionManager_CloseNotStarted verifies Close works on sessions that never started.
func TestSessionManager_CloseNotStarted(t *testing.T) {
	mgr := NewSessionManager()

	session, err := mgr.Create(SessionConfig{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Close without starting should succeed
	if err := mgr.Close(session.ID); err != nil {
		t.Errorf("Close failed for unstarted session: %v", err)
	}

	if mgr.Get(session.ID) != nil {
		t.Error("Session should be removed after Close")
	}
}

// TestSessionManager_List verifies List returns all session info.
func TestSessionManager_List(t *testing.T) {
	mgr := NewSessionManager()

	// Create a few sessions
	s1, _ := mgr.CreateWithID("session-1", SessionConfig{})
	s2, _ := mgr.CreateWithID("session-2", SessionConfig{})
	s3, _ := mgr.CreateWithID("session-3", SessionConfig{})

	// Start one of them
	if err := s2.Start("/bin/sh", "-c", "echo hello"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	<-s2.Done() // Wait for it to finish

	// List should return all three
	infos := mgr.List()
	if len(infos) != 3 {
		t.Fatalf("Expected 3 sessions in list, got %d", len(infos))
	}

	// Build a map for easier checking
	infoMap := make(map[string]SessionInfo)
	for _, info := range infos {
		infoMap[info.ID] = info
	}

	if _, ok := infoMap["session-1"]; !ok {
		t.Error("session-1 not found in list")
	}
	if _, ok := infoMap["session-2"]; !ok {
		t.Error("session-2 not found in list")
	}
	if _, ok := infoMap["session-3"]; !ok {
		t.Error("session-3 not found in list")
	}

	// session-2 should have command info
	info2 := infoMap["session-2"]
	if info2.Command != "/bin/sh" {
		t.Errorf("Expected command '/bin/sh', got %q", info2.Command)
	}

	// Cleanup
	_ = s1.Stop()
	_ = s3.Stop()
}

// TestSessionManager_CloseAll verifies CloseAll stops all sessions.
func TestSessionManager_CloseAll(t *testing.T) {
	mgr := NewSessionManager()

	// Create and start multiple sessions
	for i := 0; i < 3; i++ {
		session, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		if err := session.Start("/bin/sh", "-c", "sleep 10"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
	}

	if mgr.Count() != 3 {
		t.Fatalf("Expected 3 sessions, got %d", mgr.Count())
	}

	// CloseAll should stop everything
	mgr.CloseAll()

	if mgr.Count() != 0 {
		t.Errorf("Expected 0 sessions after CloseAll, got %d", mgr.Count())
	}
}

// TestSessionManager_Count verifies Count returns correct session count.
func TestSessionManager_Count(t *testing.T) {
	mgr := NewSessionManager()

	if mgr.Count() != 0 {
		t.Error("New manager should have 0 sessions")
	}

	mgr.Create(SessionConfig{})
	if mgr.Count() != 1 {
		t.Error("Expected 1 session after Create")
	}

	s2, _ := mgr.Create(SessionConfig{})
	if mgr.Count() != 2 {
		t.Error("Expected 2 sessions after second Create")
	}

	mgr.Close(s2.ID)
	if mgr.Count() != 1 {
		t.Error("Expected 1 session after Close")
	}
}

// =============================================================================
// Concurrency Tests
// =============================================================================

// TestSessionManager_ConcurrentCreate verifies multiple goroutines can create sessions.
func TestSessionManager_ConcurrentCreate(t *testing.T) {
	mgr := NewSessionManager()

	const numGoroutines = 10
	var wg sync.WaitGroup

	sessions := make(chan *Session, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := mgr.Create(SessionConfig{})
			if err != nil {
				t.Errorf("Concurrent Create failed: %v", err)
				return
			}
			sessions <- session
		}()
	}

	wg.Wait()
	close(sessions)

	// Collect all session IDs
	ids := make(map[string]bool)
	for session := range sessions {
		if ids[session.ID] {
			t.Error("Duplicate session ID created")
		}
		ids[session.ID] = true
	}

	if len(ids) != numGoroutines {
		t.Errorf("Expected %d unique sessions, got %d", numGoroutines, len(ids))
	}

	if mgr.Count() != numGoroutines {
		t.Errorf("Manager should have %d sessions, got %d", numGoroutines, mgr.Count())
	}
}

// TestSessionManager_ConcurrentAccess verifies concurrent Get/List while Create/Close.
func TestSessionManager_ConcurrentAccess(t *testing.T) {
	mgr := NewSessionManager()

	// Pre-create some sessions
	for i := 0; i < 5; i++ {
		session, _ := mgr.Create(SessionConfig{})
		session.Start("/bin/sh", "-c", "sleep 1")
	}

	var wg sync.WaitGroup

	// Readers - continuously List and Get
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				infos := mgr.List()
				for _, info := range infos {
					_ = mgr.Get(info.ID)
				}
			}
		}()
	}

	// Writers - Create and Close sessions
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				session, err := mgr.Create(SessionConfig{})
				if err != nil {
					continue
				}
				time.Sleep(time.Millisecond)
				mgr.Close(session.ID)
			}
		}()
	}

	wg.Wait()
	mgr.CloseAll()
}

// =============================================================================
// Multiple Sessions Tests
// =============================================================================

// TestSessionManager_MultipleSessions verifies multiple sessions run independently.
func TestSessionManager_MultipleSessions(t *testing.T) {
	mgr := NewSessionManager()

	// Create two sessions with different commands
	s1, err := mgr.CreateWithID("session-1", SessionConfig{})
	if err != nil {
		t.Fatalf("Create s1 failed: %v", err)
	}

	s2, err := mgr.CreateWithID("session-2", SessionConfig{})
	if err != nil {
		t.Fatalf("Create s2 failed: %v", err)
	}

	// Start them with different outputs
	if err := s1.Start("/bin/sh", "-c", "echo session-1-output"); err != nil {
		t.Fatalf("Start s1 failed: %v", err)
	}
	if err := s2.Start("/bin/sh", "-c", "echo session-2-output"); err != nil {
		t.Fatalf("Start s2 failed: %v", err)
	}

	// Wait for both to complete
	<-s1.Done()
	<-s2.Done()

	// Check output is independent
	lines1 := s1.Lines()
	lines2 := s2.Lines()

	if len(lines1) == 0 {
		t.Error("Session 1 should have output")
	}
	if len(lines2) == 0 {
		t.Error("Session 2 should have output")
	}

	// Verify output content is correct for each
	foundS1Output := false
	for _, line := range lines1 {
		if strings.Contains(line, "session-1-output") {
			foundS1Output = true
		}
		if strings.Contains(line, "session-2-output") {
			t.Error("Session 1 should not have session-2 output")
		}
	}
	if !foundS1Output {
		t.Error("Session 1 should have 'session-1-output'")
	}

	foundS2Output := false
	for _, line := range lines2 {
		if strings.Contains(line, "session-2-output") {
			foundS2Output = true
		}
		if strings.Contains(line, "session-1-output") {
			t.Error("Session 2 should not have session-1 output")
		}
	}
	if !foundS2Output {
		t.Error("Session 2 should have 'session-2-output'")
	}
}

// =============================================================================
// Callback Tests
// =============================================================================

// TestSessionManager_OutputCallbackWithID verifies OnOutputWithID receives session ID.
func TestSessionManager_OutputCallbackWithID(t *testing.T) {
	mgr := NewSessionManager()

	var mu sync.Mutex
	received := make(map[string][]string) // sessionID -> lines

	// Create sessions with OnOutputWithID callback
	callback := func(sessionID, line string) {
		mu.Lock()
		received[sessionID] = append(received[sessionID], line)
		mu.Unlock()
	}

	s1, _ := mgr.CreateWithID("session-A", SessionConfig{
		OnOutputWithID: callback,
	})
	s2, _ := mgr.CreateWithID("session-B", SessionConfig{
		OnOutputWithID: callback,
	})

	// Start with different outputs
	if err := s1.Start("/bin/sh", "-c", "echo from-A"); err != nil {
		t.Fatalf("Start s1 failed: %v", err)
	}
	if err := s2.Start("/bin/sh", "-c", "echo from-B"); err != nil {
		t.Fatalf("Start s2 failed: %v", err)
	}

	<-s1.Done()
	<-s2.Done()

	mu.Lock()
	defer mu.Unlock()

	// Verify callback received lines with correct session IDs
	linesA := received["session-A"]
	linesB := received["session-B"]

	if len(linesA) == 0 {
		t.Error("Should have received lines for session-A")
	}
	if len(linesB) == 0 {
		t.Error("Should have received lines for session-B")
	}

	// Check content is routed correctly
	foundA := false
	for _, line := range linesA {
		if strings.Contains(line, "from-A") {
			foundA = true
		}
	}
	if !foundA {
		t.Error("session-A callback should receive 'from-A'")
	}

	foundB := false
	for _, line := range linesB {
		if strings.Contains(line, "from-B") {
			foundB = true
		}
	}
	if !foundB {
		t.Error("session-B callback should receive 'from-B'")
	}
}

// TestSessionManager_OnOutputWithIDTakesPrecedence verifies OnOutputWithID is used over OnOutput.
func TestSessionManager_OnOutputWithIDTakesPrecedence(t *testing.T) {
	mgr := NewSessionManager()

	onOutputCalled := false
	onOutputWithIDCalled := false

	session, _ := mgr.Create(SessionConfig{
		OnOutput: func(line string) {
			onOutputCalled = true
		},
		OnOutputWithID: func(sessionID, line string) {
			onOutputWithIDCalled = true
		},
	})

	if err := session.Start("/bin/sh", "-c", "echo test"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	<-session.Done()

	if onOutputCalled {
		t.Error("OnOutput should NOT be called when OnOutputWithID is set")
	}
	if !onOutputWithIDCalled {
		t.Error("OnOutputWithID SHOULD be called")
	}
}

// =============================================================================
// Session Metadata Tests
// =============================================================================

// TestSessionManager_SessionInfoCommand verifies command info is captured.
func TestSessionManager_SessionInfoCommand(t *testing.T) {
	mgr := NewSessionManager()

	session, _ := mgr.Create(SessionConfig{})
	if err := session.Start("/bin/sh", "-c", "echo hello", "--extra"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	<-session.Done()

	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(infos))
	}

	info := infos[0]
	if info.Command != "/bin/sh" {
		t.Errorf("Expected command '/bin/sh', got %q", info.Command)
	}
	if len(info.Args) != 3 || info.Args[0] != "-c" {
		t.Errorf("Expected args ['-c', 'echo hello', '--extra'], got %v", info.Args)
	}
}

// TestSessionManager_SessionInfoCreatedAt verifies CreatedAt is set.
func TestSessionManager_SessionInfoCreatedAt(t *testing.T) {
	mgr := NewSessionManager()

	before := time.Now()
	session, _ := mgr.Create(SessionConfig{})
	if err := session.Start("/bin/sh", "-c", "echo test"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	after := time.Now()
	<-session.Done()

	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(infos))
	}

	createdAt := infos[0].CreatedAt
	if createdAt.Before(before) || createdAt.After(after) {
		t.Errorf("CreatedAt %v should be between %v and %v", createdAt, before, after)
	}
}

// TestSessionManager_SessionInfoRunning verifies Running status.
func TestSessionManager_SessionInfoRunning(t *testing.T) {
	mgr := NewSessionManager()

	session, _ := mgr.Create(SessionConfig{})

	// Before start
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(infos))
	}
	if infos[0].Running {
		t.Error("Session should not be running before Start")
	}

	// Start a long command
	if err := session.Start("/bin/sh", "-c", "sleep 1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// While running
	infos = mgr.List()
	if !infos[0].Running {
		t.Error("Session should be running after Start")
	}

	// Wait for completion
	<-session.Done()

	// After completion
	infos = mgr.List()
	if infos[0].Running {
		t.Error("Session should not be running after completion")
	}
}

// =============================================================================
// Stress Tests (addressing review gaps)
// =============================================================================

// TestSessionManager_CloseAllDuringActiveIO verifies CloseAll works when
// sessions are actively producing output. This tests the race condition
// between output capture goroutines and CloseAll.
func TestSessionManager_CloseAllDuringActiveIO(t *testing.T) {
	mgr := NewSessionManager()

	// Create sessions that produce continuous output
	for i := 0; i < 5; i++ {
		session, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		// Continuous output until killed
		if err := session.Start("/bin/sh", "-c", "while true; do echo output; done"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
	}

	// Let them produce some output
	time.Sleep(50 * time.Millisecond)

	// CloseAll should handle active I/O gracefully
	done := make(chan struct{})
	go func() {
		mgr.CloseAll()
		close(done)
	}()

	// Should complete within reasonable time
	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("CloseAll did not complete in time")
	}

	if mgr.Count() != 0 {
		t.Errorf("Expected 0 sessions after CloseAll, got %d", mgr.Count())
	}
}

// TestSessionManager_ManySessionsStress verifies the manager handles many
// sessions without issues (file descriptor limits, map performance, etc.).
func TestSessionManager_ManySessionsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Use a manager with higher limit for stress testing
	const numSessions = 50
	mgr := NewSessionManagerWithLimit(numSessions)

	// Create many sessions
	sessions := make([]*Session, 0, numSessions)
	for i := 0; i < numSessions; i++ {
		session, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}
		sessions = append(sessions, session)
	}

	if mgr.Count() != numSessions {
		t.Fatalf("Expected %d sessions, got %d", numSessions, mgr.Count())
	}

	// Start all sessions with quick commands
	for i, session := range sessions {
		if err := session.Start("/bin/sh", "-c", "echo session-"+string(rune('A'+i%26))); err != nil {
			t.Fatalf("Start %d failed: %v", i, err)
		}
	}

	// Wait for all to complete
	for i, session := range sessions {
		select {
		case <-session.Done():
			// Good
		case <-time.After(5 * time.Second):
			t.Fatalf("Session %d did not complete in time", i)
		}
	}

	// List should return all sessions
	infos := mgr.List()
	if len(infos) != numSessions {
		t.Errorf("List returned %d sessions, expected %d", len(infos), numSessions)
	}

	// Close all
	mgr.CloseAll()

	if mgr.Count() != 0 {
		t.Errorf("Expected 0 sessions after CloseAll, got %d", mgr.Count())
	}
}

// TestSessionManager_RapidCreateClose verifies rapid session creation and
// closure doesn't cause issues.
func TestSessionManager_RapidCreateClose(t *testing.T) {
	mgr := NewSessionManager()

	for i := 0; i < 20; i++ {
		session, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}

		if err := session.Start("/bin/sh", "-c", "exit 0"); err != nil {
			t.Fatalf("Start %d failed: %v", i, err)
		}

		// Close immediately (may or may not have finished)
		if err := mgr.Close(session.ID); err != nil {
			t.Fatalf("Close %d failed: %v", i, err)
		}
	}

	if mgr.Count() != 0 {
		t.Errorf("Expected 0 sessions, got %d", mgr.Count())
	}
}

// TestSessionManager_ListConcurrentWithStart verifies that List() and Start()
// can be called concurrently without data races. This is a regression test for
// the race condition where List() read session metadata without acquiring the
// session's mutex while Start() wrote those fields under lock.
func TestSessionManager_ListConcurrentWithStart(t *testing.T) {
	mgr := NewSessionManager()

	// Create sessions but don't start them yet
	const numSessions = 10
	sessions := make([]*Session, numSessions)
	for i := 0; i < numSessions; i++ {
		session, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		sessions[i] = session
	}

	// Concurrently Start sessions and List
	var wg sync.WaitGroup

	// Goroutines that start sessions
	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Small random delay to increase chance of race
			time.Sleep(time.Duration(idx) * time.Millisecond)
			_ = sessions[idx].Start("/bin/sh", "-c", "echo test")
		}(i)
	}

	// Goroutines that call List concurrently
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				infos := mgr.List()
				// Access the returned data to ensure it's valid
				for _, info := range infos {
					_ = info.Command
					_ = info.Args
					_ = info.CreatedAt
					_ = info.Running
				}
			}
		}()
	}

	wg.Wait()

	// Wait for all sessions to complete
	for _, session := range sessions {
		<-session.Done()
	}

	// Verify final state
	infos := mgr.List()
	if len(infos) != numSessions {
		t.Errorf("Expected %d sessions, got %d", numSessions, len(infos))
	}

	mgr.CloseAll()
}

// =============================================================================
// SessionManager Rename Tests (Unit 9.5)
// =============================================================================

// TestSessionManager_Rename verifies renaming a session.
func TestSessionManager_Rename(t *testing.T) {
	mgr := NewSessionManager()

	session, err := mgr.Create(SessionConfig{Name: "original-name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify initial name
	if session.GetName() != "original-name" {
		t.Errorf("Expected initial name 'original-name', got %q", session.GetName())
	}

	// Rename the session
	if err := mgr.Rename(session.ID, "new-name"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	// Verify the name was changed
	if session.GetName() != "new-name" {
		t.Errorf("Expected name 'new-name' after rename, got %q", session.GetName())
	}

	// Verify List() reflects the new name
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(infos))
	}
	if infos[0].Name != "new-name" {
		t.Errorf("Expected name 'new-name' in List(), got %q", infos[0].Name)
	}
}

// TestSessionManager_Rename_NotFound verifies error for nonexistent session.
func TestSessionManager_Rename_NotFound(t *testing.T) {
	mgr := NewSessionManager()

	err := mgr.Rename("nonexistent-id", "new-name")
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound, got %v", err)
	}
}

// TestSessionManager_Rename_EmptyName verifies renaming to empty name.
func TestSessionManager_Rename_EmptyName(t *testing.T) {
	mgr := NewSessionManager()

	session, _ := mgr.Create(SessionConfig{Name: "has-name"})

	// Renaming to empty should succeed (empty is a valid name)
	if err := mgr.Rename(session.ID, ""); err != nil {
		t.Fatalf("Rename to empty failed: %v", err)
	}

	if session.GetName() != "" {
		t.Errorf("Expected empty name, got %q", session.GetName())
	}
}

// TestSessionManager_List_IncludesName verifies List() includes session names.
func TestSessionManager_List_IncludesName(t *testing.T) {
	mgr := NewSessionManager()

	// Create sessions with different names
	s1, _ := mgr.Create(SessionConfig{Name: "session-alpha"})
	s2, _ := mgr.Create(SessionConfig{Name: "session-beta"})
	_, _ = mgr.Create(SessionConfig{}) // No name

	// Start sessions to populate command
	s1.Start("echo", "one")
	s2.Start("echo", "two")

	infos := mgr.List()
	if len(infos) != 3 {
		t.Fatalf("Expected 3 sessions, got %d", len(infos))
	}

	// Check that names are included
	names := make(map[string]bool)
	for _, info := range infos {
		names[info.Name] = true
	}

	if !names["session-alpha"] {
		t.Error("Expected to find 'session-alpha' in list")
	}
	if !names["session-beta"] {
		t.Error("Expected to find 'session-beta' in list")
	}
	if !names[""] {
		t.Error("Expected to find empty name in list")
	}

	mgr.CloseAll()
}

// =============================================================================
// Session Limit Tests (Unit 9.5 - addressing review gaps)
// =============================================================================

// TestSessionManager_MaxSessionsReached verifies the session limit is enforced.
func TestSessionManager_MaxSessionsReached(t *testing.T) {
	// Create manager with small limit for testing
	mgr := NewSessionManagerWithLimit(3)

	// Create sessions up to the limit
	for i := 0; i < 3; i++ {
		_, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}
	}

	// Next create should fail
	_, err := mgr.Create(SessionConfig{})
	if err != ErrMaxSessionsReached {
		t.Errorf("Expected ErrMaxSessionsReached, got %v", err)
	}

	// After closing one, should be able to create again
	infos := mgr.List()
	if len(infos) == 0 {
		t.Fatal("Expected at least one session")
	}
	if err := mgr.Close(infos[0].ID); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	_, err = mgr.Create(SessionConfig{})
	if err != nil {
		t.Errorf("Create after close should succeed: %v", err)
	}

	mgr.CloseAll()
}

// TestSessionManager_DefaultMaxSessions verifies the default limit.
func TestSessionManager_DefaultMaxSessions(t *testing.T) {
	mgr := NewSessionManager()

	// Create sessions up to the default limit
	for i := 0; i < DefaultMaxSessions; i++ {
		_, err := mgr.Create(SessionConfig{})
		if err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}
	}

	// Next create should fail
	_, err := mgr.Create(SessionConfig{})
	if err != ErrMaxSessionsReached {
		t.Errorf("Expected ErrMaxSessionsReached at default limit (%d), got %v", DefaultMaxSessions, err)
	}

	mgr.CloseAll()
}

// TestSessionManager_ConcurrentAllOperations tests all session operations
// happening concurrently: create, list, rename, close. This is a comprehensive
// test for race conditions under realistic usage patterns.
func TestSessionManager_ConcurrentAllOperations(t *testing.T) {
	// Use a higher limit since we're creating many sessions concurrently
	mgr := NewSessionManagerWithLimit(100)

	var wg sync.WaitGroup
	const numWorkers = 5
	const opsPerWorker = 20

	// Track created sessions for cleanup
	var sessionsMu sync.Mutex
	createdIDs := make([]string, 0)

	// Create workers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				// Create a session
				session, err := mgr.Create(SessionConfig{
					Name: "worker-session",
				})
				if err != nil {
					// May hit limit, that's okay
					continue
				}

				sessionsMu.Lock()
				createdIDs = append(createdIDs, session.ID)
				sessionsMu.Unlock()

				// Start it
				_ = session.Start("/bin/sh", "-c", "echo test")

				// Random operations
				switch i % 4 {
				case 0:
					// List
					_ = mgr.List()
				case 1:
					// Rename
					_ = mgr.Rename(session.ID, "renamed")
				case 2:
					// Get
					_ = mgr.Get(session.ID)
				case 3:
					// Close some sessions
					if i%2 == 0 {
						_ = mgr.Close(session.ID)
					}
				}
			}
		}(w)
	}

	// Also run list operations concurrently
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				infos := mgr.List()
				for _, info := range infos {
					_ = info.Name
					_ = info.Command
					_ = info.Running
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// Cleanup
	mgr.CloseAll()

	if mgr.Count() != 0 {
		t.Errorf("Expected 0 sessions after CloseAll, got %d", mgr.Count())
	}
}

// =============================================================================
// TmuxSession Field in SessionInfo Tests (Unit 12.3)
// =============================================================================

// TestSessionManager_SessionInfo_TmuxSession verifies TmuxSession is included in List().
func TestSessionManager_SessionInfo_TmuxSession(t *testing.T) {
	mgr := NewSessionManager()

	// Create a regular session
	s1, _ := mgr.CreateWithID("regular-session", SessionConfig{})

	// Create a tmux session by setting TmuxSession field
	s2, _ := mgr.CreateWithID("tmux-session", SessionConfig{})
	s2.TmuxSession = "main" // Mark as tmux session

	// Verify List() includes TmuxSession field
	infos := mgr.List()
	if len(infos) != 2 {
		t.Fatalf("Expected 2 sessions, got %d", len(infos))
	}

	infoMap := make(map[string]SessionInfo)
	for _, info := range infos {
		infoMap[info.ID] = info
	}

	// Regular session should have empty TmuxSession
	regularInfo := infoMap["regular-session"]
	if regularInfo.TmuxSession != "" {
		t.Errorf("Expected empty TmuxSession for regular session, got %q", regularInfo.TmuxSession)
	}

	// Tmux session should have TmuxSession set
	tmuxInfo := infoMap["tmux-session"]
	if tmuxInfo.TmuxSession != "main" {
		t.Errorf("Expected TmuxSession 'main', got %q", tmuxInfo.TmuxSession)
	}

	_ = s1
	mgr.CloseAll()
}

// =============================================================================
// Register Method Tests (Unit 12.4)
// =============================================================================

// TestSessionManager_Register tests registering externally-created sessions.
func TestSessionManager_Register(t *testing.T) {
	mgr := NewSessionManager()

	// Create a session externally (as TmuxManager would)
	cfg := SessionConfig{ID: "external-session-1"}
	session := NewSession(cfg)

	// Register should succeed
	err := mgr.Register(session)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Session should be accessible via Get
	got := mgr.Get("external-session-1")
	if got == nil {
		t.Error("Expected to find registered session")
	}
	if got != session {
		t.Error("Get returned different session object")
	}

	// Count should include registered session
	if mgr.Count() != 1 {
		t.Errorf("Expected count 1, got %d", mgr.Count())
	}

	mgr.CloseAll()
}

// TestSessionManager_Register_NilSession tests registering a nil session.
func TestSessionManager_Register_NilSession(t *testing.T) {
	mgr := NewSessionManager()

	err := mgr.Register(nil)
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound, got %v", err)
	}
}

// TestSessionManager_Register_DuplicateID tests registering with existing ID.
func TestSessionManager_Register_DuplicateID(t *testing.T) {
	mgr := NewSessionManager()

	// Create first session via CreateWithID
	_, err := mgr.CreateWithID("dup-id", SessionConfig{})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}

	// Try to register another session with same ID
	cfg := SessionConfig{ID: "dup-id"}
	session := NewSession(cfg)

	err = mgr.Register(session)
	if err != ErrSessionExists {
		t.Errorf("Expected ErrSessionExists, got %v", err)
	}

	mgr.CloseAll()
}

// TestSessionManager_Register_MaxSessionsReached tests registering at capacity.
func TestSessionManager_Register_MaxSessionsReached(t *testing.T) {
	// Create manager with limit of 2
	mgr := NewSessionManagerWithLimit(2)

	// Fill up with created sessions
	_, err := mgr.CreateWithID("s1", SessionConfig{})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}
	_, err = mgr.CreateWithID("s2", SessionConfig{})
	if err != nil {
		t.Fatalf("CreateWithID failed: %v", err)
	}

	// Try to register another session
	cfg := SessionConfig{ID: "s3"}
	session := NewSession(cfg)

	err = mgr.Register(session)
	if err != ErrMaxSessionsReached {
		t.Errorf("Expected ErrMaxSessionsReached, got %v", err)
	}

	mgr.CloseAll()
}

// TestSessionManager_Register_TmuxSession tests registering a tmux session.
func TestSessionManager_Register_TmuxSession(t *testing.T) {
	mgr := NewSessionManager()

	// Create a session marked as tmux (as TmuxManager.AttachToTmux would)
	cfg := SessionConfig{ID: "tmux-attached"}
	session := NewSession(cfg)
	session.TmuxSession = "dev" // Mark as tmux session

	err := mgr.Register(session)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Verify List includes the tmux session with correct metadata
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(infos))
	}

	info := infos[0]
	if info.ID != "tmux-attached" {
		t.Errorf("Expected ID 'tmux-attached', got %q", info.ID)
	}
	if info.TmuxSession != "dev" {
		t.Errorf("Expected TmuxSession 'dev', got %q", info.TmuxSession)
	}

	mgr.CloseAll()
}
