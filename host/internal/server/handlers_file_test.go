package server

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// sendFileMsg sends a file protocol message over ws and returns immediately.
func sendFileMsg(t *testing.T, conn *websocket.Conn, msgType MessageType, payload interface{}) {
	t.Helper()
	msg := Message{Type: msgType, Payload: payload}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readFileResult reads messages until it finds one of the given type, with timeout.
func readFileResult(t *testing.T, conn *websocket.Conn, wantType MessageType) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env struct {
			Type    MessageType     `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Type == wantType {
			return env.Payload
		}
	}
	t.Fatalf("timeout waiting for %s", wantType)
	return nil
}

// assertNoMessageType ensures a forbidden message type is never observed within
// the given window, while safely ignoring unrelated background frames.
func assertNoMessageType(t *testing.T, conn *websocket.Conn, forbidden MessageType, wait time.Duration) {
	t.Helper()

	conn.SetReadDeadline(time.Now().Add(wait))
	defer conn.SetReadDeadline(time.Time{})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return
			}
			t.Fatalf("read while asserting no %s: %v", forbidden, err)
		}

		var env struct {
			Type MessageType `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal while asserting no %s: %v", forbidden, err)
		}
		if env.Type == forbidden {
			t.Fatalf("unexpected %s received", forbidden)
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func TestHandleFileList_Success(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileList, FileListPayload{
		RequestID: "req-1",
		Path:      "",
	})

	raw := readFileResult(t, conn, MessageTypeFileListResult)
	var result FileListResultPayload
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if !result.Success {
		t.Fatalf("expected success, got error: %s %s", result.ErrorCode, result.Error)
	}
	if result.RequestID != "req-1" {
		t.Errorf("expected request_id 'req-1', got %q", result.RequestID)
	}
	if result.Path != "." {
		t.Errorf("expected path '.', got %q", result.Path)
	}
	if len(result.Entries) == 0 {
		t.Fatal("expected non-empty entries")
	}

	// Verify sorting: dirs should come before files.
	lastKindOrder := -1
	for _, e := range result.Entries {
		ko := kindOrder(e.Kind)
		if ko < lastKindOrder {
			t.Errorf("entries not sorted: %s (%s) after higher kind", e.Name, e.Kind)
		}
		lastKindOrder = ko
	}
}

func TestHandleFileList_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileList, FileListPayload{
		RequestID: "",
		Path:      "",
	})

	raw := readFileResult(t, conn, MessageTypeFileListResult)
	var result FileListResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing request_id")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileList_NoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set file operations.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileList, FileListPayload{
		RequestID: "req-2",
		Path:      "",
	})

	raw := readFileResult(t, conn, MessageTypeFileListResult)
	var result FileListResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for nil handler")
	}
	if result.ErrorCode != "server.handler_missing" {
		t.Errorf("expected server.handler_missing, got %q", result.ErrorCode)
	}
}

func TestHandleFileList_MalformedPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send raw malformed JSON with correct type wrapper.
	raw := `{"type":"file.list","payload":"not-an-object"}`
	conn.WriteMessage(websocket.TextMessage, []byte(raw))

	result := readFileResult(t, conn, MessageTypeFileListResult)
	var payload FileListResultPayload
	json.Unmarshal(result, &payload)

	if payload.Success {
		t.Error("expected failure for malformed payload")
	}
}

func TestHandleFileList_PathIsFile(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileList, FileListPayload{
		RequestID: "req-file-path",
		Path:      "README.md",
	})

	raw := readFileResult(t, conn, MessageTypeFileListResult)
	var result FileListResultPayload
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.Success {
		t.Fatal("expected failure for file path list request")
	}
	if result.RequestID != "req-file-path" {
		t.Errorf("expected request_id req-file-path, got %q", result.RequestID)
	}
	if result.ErrorCode != "action.invalid" {
		t.Errorf("expected action.invalid, got %q", result.ErrorCode)
	}
}

func TestHandleFileList_RequesterOnly(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	// Send request on conn1.
	sendFileMsg(t, conn1, MessageTypeFileList, FileListPayload{
		RequestID: "req-only",
		Path:      "",
	})

	// conn1 should get the result.
	readFileResult(t, conn1, MessageTypeFileListResult)

	// conn2 should never receive the requester-scoped result.
	assertNoMessageType(t, conn2, MessageTypeFileListResult, 750*time.Millisecond)
}

func TestHandleFileRead_Success(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "read-1",
		Path:      "README.md",
	})

	raw := readFileResult(t, conn, MessageTypeFileReadResult)
	var result FileReadResultPayload
	json.Unmarshal(raw, &result)

	if !result.Success {
		t.Fatalf("expected success, got: %s %s", result.ErrorCode, result.Error)
	}
	if result.RequestID != "read-1" {
		t.Errorf("expected request_id 'read-1', got %q", result.RequestID)
	}
	if result.Content != "# Hello\n" {
		t.Errorf("unexpected content: %q", result.Content)
	}
	if result.Encoding != "utf-8" {
		t.Errorf("expected utf-8, got %q", result.Encoding)
	}
	if !strings.HasPrefix(result.Version, "sha256:") {
		t.Errorf("expected sha256: version, got %q", result.Version)
	}
}

func TestHandleFileRead_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "",
		Path:      "foo.txt",
	})

	raw := readFileResult(t, conn, MessageTypeFileReadResult)
	var result FileReadResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing request_id")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileRead_MissingPath(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "read-2",
		Path:      "",
	})

	raw := readFileResult(t, conn, MessageTypeFileReadResult)
	var result FileReadResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing path")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileRead_NoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "read-3",
		Path:      "foo.txt",
	})

	raw := readFileResult(t, conn, MessageTypeFileReadResult)
	var result FileReadResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for nil handler")
	}
	if result.ErrorCode != "server.handler_missing" {
		t.Errorf("expected server.handler_missing, got %q", result.ErrorCode)
	}
}

func TestHandleFileRead_MalformedPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw := `{"type":"file.read","payload":42}`
	conn.WriteMessage(websocket.TextMessage, []byte(raw))

	result := readFileResult(t, conn, MessageTypeFileReadResult)
	var payload FileReadResultPayload
	json.Unmarshal(result, &payload)

	if payload.Success {
		t.Error("expected failure for malformed payload")
	}
}

func TestHandleFileRead_RequesterOnly(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	sendFileMsg(t, conn1, MessageTypeFileRead, FileReadPayload{
		RequestID: "read-only",
		Path:      "README.md",
	})

	readFileResult(t, conn1, MessageTypeFileReadResult)

	// conn2 should never receive the requester-scoped result.
	assertNoMessageType(t, conn2, MessageTypeFileReadResult, 750*time.Millisecond)
}

// --- File Write handler tests ---

func TestHandleFileWrite_Success(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// First read to get the current version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "read-ver",
		Path:      "README.md",
	})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Write with correct base version.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "write-1",
		Path:        "README.md",
		Content:     "# Updated\n",
		BaseVersion: readResult.Version,
	})

	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if !result.Success {
		t.Fatalf("expected success, got: %s %s", result.ErrorCode, result.Error)
	}
	if result.RequestID != "write-1" {
		t.Errorf("expected request_id 'write-1', got %q", result.RequestID)
	}
	if !strings.HasPrefix(result.Version, "sha256:") {
		t.Errorf("expected sha256: version, got %q", result.Version)
	}
}

func TestHandleFileWrite_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		Path:        "test.txt",
		Content:     "hello",
		BaseVersion: "sha256:fake",
	})

	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing request_id")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileWrite_MissingPath(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "w-no-path",
		Content:     "hello",
		BaseVersion: "sha256:fake",
	})

	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing path")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileWrite_MissingBaseVersion(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID: "w-no-ver",
		Path:      "test.txt",
		Content:   "hello",
	})

	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing base_version")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileWrite_MalformedPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw := `{"type":"file.write","payload":"not-an-object"}`
	conn.WriteMessage(websocket.TextMessage, []byte(raw))

	result := readFileResult(t, conn, MessageTypeFileWriteResult)
	var payload FileWriteResultPayload
	json.Unmarshal(result, &payload)

	if payload.Success {
		t.Error("expected failure for malformed payload")
	}
}

func TestHandleFileWrite_NoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	// Don't set file operations.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "w-no-handler",
		Path:        "test.txt",
		Content:     "hello",
		BaseVersion: "sha256:fake",
	})

	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for nil handler")
	}
	if result.ErrorCode != "server.handler_missing" {
		t.Errorf("expected server.handler_missing, got %q", result.ErrorCode)
	}
}

// --- File Create handler tests ---

func TestHandleFileCreate_Success(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "create-1",
		Path:      "new.txt",
		Content:   "new content\n",
	})

	raw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result FileCreateResultPayload
	json.Unmarshal(raw, &result)

	if !result.Success {
		t.Fatalf("expected success, got: %s %s", result.ErrorCode, result.Error)
	}
	if result.RequestID != "create-1" {
		t.Errorf("expected request_id 'create-1', got %q", result.RequestID)
	}
	if !strings.HasPrefix(result.Version, "sha256:") {
		t.Errorf("expected sha256: version, got %q", result.Version)
	}
}

func TestHandleFileCreate_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		Path:    "new.txt",
		Content: "content",
	})

	raw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result FileCreateResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing request_id")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileCreate_MissingPath(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "c-no-path",
		Content:   "content",
	})

	raw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result FileCreateResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing path")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileCreate_MalformedPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw := `{"type":"file.create","payload":123}`
	conn.WriteMessage(websocket.TextMessage, []byte(raw))

	result := readFileResult(t, conn, MessageTypeFileCreateResult)
	var payload FileCreateResultPayload
	json.Unmarshal(result, &payload)

	if payload.Success {
		t.Error("expected failure for malformed payload")
	}
}

func TestHandleFileCreate_NoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "c-no-handler",
		Path:      "new.txt",
		Content:   "content",
	})

	raw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result FileCreateResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for nil handler")
	}
	if result.ErrorCode != "server.handler_missing" {
		t.Errorf("expected server.handler_missing, got %q", result.ErrorCode)
	}
}

// --- File Delete handler tests ---

func TestHandleFileDelete_Success(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "delete-1",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})

	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if !result.Success {
		t.Fatalf("expected success, got: %s %s", result.ErrorCode, result.Error)
	}
	if result.RequestID != "delete-1" {
		t.Errorf("expected request_id 'delete-1', got %q", result.RequestID)
	}
}

func TestHandleFileDelete_MissingRequestID(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		Path:      "old.txt",
		Confirmed: boolPtr(true),
	})

	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing request_id")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileDelete_MissingPath(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "d-no-path",
		Confirmed: boolPtr(true),
	})

	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing path")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileDelete_MalformedPayload(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw := `{"type":"file.delete","payload":null}`
	conn.WriteMessage(websocket.TextMessage, []byte(raw))

	result := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var payload FileDeleteResultPayload
	json.Unmarshal(result, &payload)

	if payload.Success {
		t.Error("expected failure for malformed payload")
	}
}

func TestHandleFileDelete_MissingConfirmed(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(t.TempDir(), 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, map[string]interface{}{
		"request_id": "d-no-confirmed",
		"path":       "docs/guide.txt",
	})

	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for missing confirmed")
	}
	if result.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result.ErrorCode)
	}
}

func TestHandleFileDelete_NoHandler(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "d-no-handler",
		Path:      "old.txt",
		Confirmed: boolPtr(true),
	})

	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Error("expected failure for nil handler")
	}
	if result.ErrorCode != "server.handler_missing" {
		t.Errorf("expected server.handler_missing, got %q", result.ErrorCode)
	}
}

// --- Idempotency tests ---

func TestHandleFileWrite_IdempotentMismatchAfterValidationFailure(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	originalBytes, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read original README: %v", err)
	}

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// First request is deterministic validation failure (missing base_version).
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID: "idem-invalid-write",
		Path:      "README.md",
		Content:   "# Should Not Write\n",
	})
	invalidRaw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var invalidResult FileWriteResultPayload
	json.Unmarshal(invalidRaw, &invalidResult)
	if invalidResult.Success {
		t.Fatal("expected missing base_version validation failure")
	}
	if invalidResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", invalidResult.ErrorCode)
	}

	// Same request_id with changed payload must be rejected as idempotency mismatch.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-invalid-write",
		Path:        "README.md",
		Content:     "# Should Not Write\n",
		BaseVersion: readResult.Version,
	})
	mismatchRaw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var mismatchResult FileWriteResultPayload
	json.Unmarshal(mismatchRaw, &mismatchResult)
	if mismatchResult.Success {
		t.Fatal("expected mismatch replay failure")
	}
	if mismatchResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", mismatchResult.ErrorCode)
	}

	currentBytes, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read current README: %v", err)
	}
	if string(currentBytes) != string(originalBytes) {
		t.Fatal("README.md changed after idempotency mismatch")
	}
}

func TestHandleFileCreate_IdempotentMismatchAfterValidationFailure(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-invalid-create",
		Content:   "new content\n",
	})
	invalidRaw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var invalidResult FileCreateResultPayload
	json.Unmarshal(invalidRaw, &invalidResult)
	if invalidResult.Success {
		t.Fatal("expected missing path validation failure")
	}
	if invalidResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", invalidResult.ErrorCode)
	}

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-invalid-create",
		Path:      "src/blocked-create.txt",
		Content:   "new content\n",
	})
	mismatchRaw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var mismatchResult FileCreateResultPayload
	json.Unmarshal(mismatchRaw, &mismatchResult)
	if mismatchResult.Success {
		t.Fatal("expected mismatch replay failure")
	}
	if mismatchResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", mismatchResult.ErrorCode)
	}

	if _, err := os.Stat(filepath.Join(dir, "src", "blocked-create.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected blocked-create.txt to not exist, stat err: %v", err)
	}
}

func TestHandleFileDelete_IdempotentMismatchAfterValidationFailure(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-invalid-delete",
		Confirmed: boolPtr(true),
	})
	invalidRaw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var invalidResult FileDeleteResultPayload
	json.Unmarshal(invalidRaw, &invalidResult)
	if invalidResult.Success {
		t.Fatal("expected missing path validation failure")
	}
	if invalidResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", invalidResult.ErrorCode)
	}

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-invalid-delete",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})
	mismatchRaw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var mismatchResult FileDeleteResultPayload
	json.Unmarshal(mismatchRaw, &mismatchResult)
	if mismatchResult.Success {
		t.Fatal("expected mismatch replay failure")
	}
	if mismatchResult.ErrorCode != "server.invalid_message" {
		t.Fatalf("expected server.invalid_message, got %q", mismatchResult.ErrorCode)
	}

	if _, err := os.Stat(filepath.Join(dir, "docs", "guide.txt")); err != nil {
		t.Fatalf("expected docs/guide.txt to still exist, got err: %v", err)
	}
}

func TestHandleFileWrite_IdempotentReplay(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// First write.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-1",
		Path:        "README.md",
		Content:     "# Idempotent\n",
		BaseVersion: readResult.Version,
	})
	raw1 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result1 FileWriteResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("first write failed: %s", result1.ErrorCode)
	}

	// Replay same request — should get identical result from cache.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-1",
		Path:        "README.md",
		Content:     "# Idempotent\n",
		BaseVersion: readResult.Version,
	})
	raw2 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result2 FileWriteResultPayload
	json.Unmarshal(raw2, &result2)

	if !result2.Success {
		t.Fatalf("replay should succeed, got: %s", result2.ErrorCode)
	}
	if result2.Version != result1.Version {
		t.Errorf("replay version mismatch: %s vs %s", result2.Version, result1.Version)
	}
}

func TestHandleFileWrite_ConcurrentDuplicateReplay(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Send two identical requests back-to-back before reading responses.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-concurrent",
		Path:        "README.md",
		Content:     "# Concurrent\n",
		BaseVersion: readResult.Version,
	})
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-concurrent",
		Path:        "README.md",
		Content:     "# Concurrent\n",
		BaseVersion: readResult.Version,
	})

	raw1 := readFileResult(t, conn, MessageTypeFileWriteResult)
	raw2 := readFileResult(t, conn, MessageTypeFileWriteResult)

	var result1, result2 FileWriteResultPayload
	json.Unmarshal(raw1, &result1)
	json.Unmarshal(raw2, &result2)

	if !result1.Success || !result2.Success {
		t.Fatalf("expected both write results to succeed, got %v/%v", result1.ErrorCode, result2.ErrorCode)
	}
	if result1.Version != result2.Version {
		t.Fatalf("expected identical replayed versions, got %s vs %s", result1.Version, result2.Version)
	}
}

func TestHandleFileWrite_IdempotentMismatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// First write.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-mm",
		Path:        "README.md",
		Content:     "# First\n",
		BaseVersion: readResult.Version,
	})
	raw1 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result1 FileWriteResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("first write failed: %s", result1.ErrorCode)
	}

	// Same request_id, different content — should be rejected.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-mm",
		Path:        "README.md",
		Content:     "# DIFFERENT\n",
		BaseVersion: readResult.Version,
	})
	raw2 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result2 FileWriteResultPayload
	json.Unmarshal(raw2, &result2)

	if result2.Success {
		t.Fatal("mismatch replay should fail")
	}
	if result2.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result2.ErrorCode)
	}
}

func TestHandleFileCreate_IdempotentReplay(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// First create.
	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-c1",
		Path:      "idem-new.txt",
		Content:   "content",
	})
	raw1 := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result1 FileCreateResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("first create failed: %s", result1.ErrorCode)
	}

	// Replay — should return cached result, not already_exists error.
	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-c1",
		Path:      "idem-new.txt",
		Content:   "content",
	})
	raw2 := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result2 FileCreateResultPayload
	json.Unmarshal(raw2, &result2)

	if !result2.Success {
		t.Fatalf("replay should succeed, got: %s", result2.ErrorCode)
	}
	if result2.Version != result1.Version {
		t.Errorf("replay version mismatch: %s vs %s", result2.Version, result1.Version)
	}
}

func TestHandleFileCreate_IdempotentMismatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-cm",
		Path:      "idem-mm.txt",
		Content:   "original",
	})
	raw1 := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result1 FileCreateResultPayload
	json.Unmarshal(raw1, &result1)

	// Same request_id, different content.
	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "idem-cm",
		Path:      "idem-mm.txt",
		Content:   "DIFFERENT",
	})
	raw2 := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result2 FileCreateResultPayload
	json.Unmarshal(raw2, &result2)

	if result2.Success {
		t.Fatal("mismatch replay should fail")
	}
	if result2.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result2.ErrorCode)
	}
}

func TestHandleFileDelete_IdempotentReplay(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-d1",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})
	raw1 := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result1 FileDeleteResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("first delete failed: %s", result1.ErrorCode)
	}

	// Replay — should return cached result, not not_found error.
	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-d1",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})
	raw2 := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result2 FileDeleteResultPayload
	json.Unmarshal(raw2, &result2)

	if !result2.Success {
		t.Fatalf("replay should succeed, got: %s", result2.ErrorCode)
	}
}

func TestHandleFileDelete_IdempotentMismatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// First delete (confirmed=true).
	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-dm",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})
	readFileResult(t, conn, MessageTypeFileDeleteResult)

	// Same request_id, different confirmed flag.
	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "idem-dm",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(false),
	})
	raw2 := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result2 FileDeleteResultPayload
	json.Unmarshal(raw2, &result2)

	if result2.Success {
		t.Fatal("mismatch replay should fail")
	}
	if result2.ErrorCode != "server.invalid_message" {
		t.Errorf("expected server.invalid_message, got %q", result2.ErrorCode)
	}
}

// --- Cross-client isolation tests ---

func TestHandleFile_CrossClientIndependence(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	// Create file on conn1.
	sendFileMsg(t, conn1, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "cross-1",
		Path:      "cross-test.txt",
		Content:   "hello",
	})
	raw1 := readFileResult(t, conn1, MessageTypeFileCreateResult)
	var result1 FileCreateResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("conn1 create failed: %s", result1.ErrorCode)
	}

	// conn2 uses same request_id — should NOT get cached result from conn1.
	// Since file now exists, this should fail with already_exists.
	sendFileMsg(t, conn2, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "cross-1",
		Path:      "cross-test.txt",
		Content:   "hello",
	})
	raw2 := readFileResult(t, conn2, MessageTypeFileCreateResult)
	var result2 FileCreateResultPayload
	json.Unmarshal(raw2, &result2)

	if result2.Success {
		t.Fatal("conn2 should not replay conn1 cache")
	}
	if result2.ErrorCode != "storage.already_exists" {
		t.Errorf("expected storage.already_exists, got %q", result2.ErrorCode)
	}
}

func TestHandleFile_CrossOpKeyIsolation(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Create a file with request_id "shared-id".
	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "shared-id",
		Path:      "cross-op.txt",
		Content:   "original",
	})
	createRaw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var createResult FileCreateResultPayload
	json.Unmarshal(createRaw, &createResult)

	if !createResult.Success {
		t.Fatalf("create failed: %s", createResult.ErrorCode)
	}

	// Delete with same request_id "shared-id" — different op type, should NOT conflict.
	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "shared-id",
		Path:      "cross-op.txt",
		Confirmed: boolPtr(true),
	})
	deleteRaw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var deleteResult FileDeleteResultPayload
	json.Unmarshal(deleteRaw, &deleteResult)

	if !deleteResult.Success {
		t.Fatalf("delete should succeed (different op type): %s", deleteResult.ErrorCode)
	}
}

func TestHandleFileWrite_EvictionTreatedAsNew(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Write the file.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "evict-target",
		Path:        "README.md",
		Content:     "# Evict test\n",
		BaseVersion: readResult.Version,
	})
	raw1 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result1 FileWriteResultPayload
	json.Unmarshal(raw1, &result1)

	if !result1.Success {
		t.Fatalf("initial write failed: %s", result1.ErrorCode)
	}

	// Flood the cache with maxIdempotencyEntries unique creates to evict our write.
	for i := 0; i < maxIdempotencyEntries; i++ {
		path := filepath.Join("src", filepath.Base(filepath.Join("evict", strings.Replace(
			filepath.Base(filepath.Join("file", string(rune('a'+i%26))+"_"+filepath.Base(filepath.Join("n", string(rune('0'+i/26)))))),
			string(filepath.Separator), "", -1)+".txt")))
		sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
			RequestID: "evict-flood-" + string(rune('A'+i%26)) + string(rune('0'+i/26)),
			Path:      path,
			Content:   "flood",
		})
		readFileResult(t, conn, MessageTypeFileCreateResult)
	}

	// Replay the original write — cache should be evicted.
	// The file was modified so base version no longer matches → conflict.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "evict-target",
		Path:        "README.md",
		Content:     "# Evict test\n",
		BaseVersion: readResult.Version,
	})
	raw2 := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result2 FileWriteResultPayload
	json.Unmarshal(raw2, &result2)

	// After eviction, this is treated as a new request.
	// It should fail with conflict.detected because baseVersion is stale.
	if result2.Success {
		t.Fatal("expected failure after eviction (stale version)")
	}
	if result2.ErrorCode != "conflict.detected" {
		t.Errorf("expected conflict.detected after eviction, got %q", result2.ErrorCode)
	}
}

func TestHandleFileWrite_RequesterOnly(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	// Read version on conn1.
	sendFileMsg(t, conn1, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn1, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Write on conn1.
	sendFileMsg(t, conn1, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "req-only-w",
		Path:        "README.md",
		Content:     "# Requester\n",
		BaseVersion: readResult.Version,
	})
	readFileResult(t, conn1, MessageTypeFileWriteResult)

	// conn2 should never receive the write result.
	assertNoMessageType(t, conn2, MessageTypeFileWriteResult, 750*time.Millisecond)
}

func TestHandleFileWrite_FailureIsWriteResult(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write with stale version to trigger failure.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "envelope-1",
		Path:        "README.md",
		Content:     "fail",
		BaseVersion: "sha256:stale",
	})

	// Should get file.write_result (not error or other type).
	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Fatal("expected failure")
	}
	if result.ErrorCode != "conflict.detected" {
		t.Errorf("expected conflict.detected, got %q", result.ErrorCode)
	}
}

func TestHandleFileWrite_FilesystemFailureMapped(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)
	beforeContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read README before failure: %v", err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod read-only repo: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "fs-write",
		Path:        "README.md",
		Content:     "x\n",
		BaseVersion: readResult.Version,
	})
	raw := readFileResult(t, conn, MessageTypeFileWriteResult)
	var result FileWriteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Fatal("expected filesystem failure")
	}
	if result.ErrorCode != "error.internal" {
		t.Errorf("expected error.internal, got %q", result.ErrorCode)
	}
	afterContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read README after failure: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Fatal("README.md changed despite write failure")
	}
}

func TestHandleFileCreate_FilesystemFailureMapped(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	srcDir := filepath.Join(dir, "src")
	if err := os.Chmod(srcDir, 0o555); err != nil {
		t.Fatalf("chmod read-only src dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(srcDir, 0o755)
	})

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "fs-create",
		Path:      "src/new.txt",
		Content:   "x\n",
	})
	raw := readFileResult(t, conn, MessageTypeFileCreateResult)
	var result FileCreateResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Fatal("expected filesystem failure")
	}
	if result.ErrorCode != "error.internal" {
		t.Errorf("expected error.internal, got %q", result.ErrorCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("src/new.txt should not exist after create failure, stat err: %v", err)
	}
}

func TestHandleFileDelete_FilesystemFailureMapped(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	docsDir := filepath.Join(dir, "docs")
	if err := os.Chmod(docsDir, 0o555); err != nil {
		t.Fatalf("chmod read-only docs dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(docsDir, 0o755)
	})

	beforeContent, err := os.ReadFile(filepath.Join(dir, "docs", "guide.txt"))
	if err != nil {
		t.Fatalf("read guide before failure: %v", err)
	}

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "fs-delete",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})
	raw := readFileResult(t, conn, MessageTypeFileDeleteResult)
	var result FileDeleteResultPayload
	json.Unmarshal(raw, &result)

	if result.Success {
		t.Fatal("expected filesystem failure")
	}
	if result.ErrorCode != "error.internal" {
		t.Errorf("expected error.internal, got %q", result.ErrorCode)
	}
	afterContent, err := os.ReadFile(filepath.Join(dir, "docs", "guide.txt"))
	if err != nil {
		t.Fatalf("read guide after failure: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Fatal("docs/guide.txt changed despite delete failure")
	}
}

func TestHandleFileRead_BinaryFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "image.png"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00}, 0o644)

	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{
		RequestID: "bin-read",
		Path:      "image.png",
	})

	raw := readFileResult(t, conn, MessageTypeFileReadResult)
	var result FileReadResultPayload
	json.Unmarshal(raw, &result)

	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorCode)
	}
	if result.ReadOnlyReason != "binary" {
		t.Errorf("expected binary, got %q", result.ReadOnlyReason)
	}
	if result.Content != "" {
		t.Errorf("expected empty content for binary file")
	}
}

// --- file.watch emission tests ---

// readFileWatch reads messages until it finds a file.watch, with timeout.
func readFileWatch(t *testing.T, conn *websocket.Conn) FileWatchPayload {
	t.Helper()
	raw := readFileResult(t, conn, MessageTypeFileWatch)
	var payload FileWatchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal file.watch payload: %v", err)
	}
	return payload
}

func TestHandleFileWrite_EmitsWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	// Read version.
	sendFileMsg(t, conn1, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn1, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Write on conn1.
	sendFileMsg(t, conn1, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "w-watch-1",
		Path:        "README.md",
		Content:     "# Watched\n",
		BaseVersion: readResult.Version,
	})

	// conn1 gets write result.
	writeRaw := readFileResult(t, conn1, MessageTypeFileWriteResult)
	var writeResult FileWriteResultPayload
	json.Unmarshal(writeRaw, &writeResult)
	if !writeResult.Success {
		t.Fatalf("write failed: %s", writeResult.ErrorCode)
	}

	// Both clients should receive file.watch.
	watch1 := readFileWatch(t, conn1)
	if watch1.Path != "README.md" {
		t.Errorf("conn1 watch path: expected README.md, got %q", watch1.Path)
	}
	if watch1.Change != "modified" {
		t.Errorf("conn1 watch change: expected modified, got %q", watch1.Change)
	}
	if watch1.Version != writeResult.Version {
		t.Errorf("conn1 watch version: expected %q, got %q", writeResult.Version, watch1.Version)
	}

	watch2 := readFileWatch(t, conn2)
	if watch2.Path != "README.md" {
		t.Errorf("conn2 watch path: expected README.md, got %q", watch2.Path)
	}
	if watch2.Change != "modified" {
		t.Errorf("conn2 watch change: expected modified, got %q", watch2.Change)
	}
}

func TestHandleFileCreate_EmitsWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	sendFileMsg(t, conn1, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "c-watch-1",
		Path:      "watched-new.txt",
		Content:   "hello\n",
	})

	createRaw := readFileResult(t, conn1, MessageTypeFileCreateResult)
	var createResult FileCreateResultPayload
	json.Unmarshal(createRaw, &createResult)
	if !createResult.Success {
		t.Fatalf("create failed: %s", createResult.ErrorCode)
	}

	watch1 := readFileWatch(t, conn1)
	if watch1.Path != "watched-new.txt" {
		t.Errorf("conn1 watch path: expected watched-new.txt, got %q", watch1.Path)
	}
	if watch1.Change != "created" {
		t.Errorf("conn1 watch change: expected created, got %q", watch1.Change)
	}
	if watch1.Version != createResult.Version {
		t.Errorf("conn1 watch version: expected %q, got %q", createResult.Version, watch1.Version)
	}

	watch2 := readFileWatch(t, conn2)
	if watch2.Path != "watched-new.txt" {
		t.Errorf("conn2 watch path: expected watched-new.txt, got %q", watch2.Path)
	}
	if watch2.Change != "created" {
		t.Errorf("conn2 watch change: expected created, got %q", watch2.Change)
	}
}

func TestHandleFileDelete_EmitsWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()

	sendFileMsg(t, conn1, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "d-watch-1",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})

	deleteRaw := readFileResult(t, conn1, MessageTypeFileDeleteResult)
	var deleteResult FileDeleteResultPayload
	json.Unmarshal(deleteRaw, &deleteResult)
	if !deleteResult.Success {
		t.Fatalf("delete failed: %s", deleteResult.ErrorCode)
	}

	watch1 := readFileWatch(t, conn1)
	if watch1.Path != filepath.ToSlash("docs/guide.txt") {
		t.Errorf("conn1 watch path: expected docs/guide.txt, got %q", watch1.Path)
	}
	if watch1.Change != "deleted" {
		t.Errorf("conn1 watch change: expected deleted, got %q", watch1.Change)
	}
	if watch1.Version != "" {
		t.Errorf("conn1 watch version: expected empty, got %q", watch1.Version)
	}

	watch2 := readFileWatch(t, conn2)
	if watch2.Path != filepath.ToSlash("docs/guide.txt") {
		t.Errorf("conn2 watch path: expected docs/guide.txt, got %q", watch2.Path)
	}
	if watch2.Change != "deleted" {
		t.Errorf("conn2 watch change: expected deleted, got %q", watch2.Change)
	}
}

func TestHandleFileWrite_FailureNoWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write with stale version to trigger conflict.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "w-fail-watch",
		Path:        "README.md",
		Content:     "fail",
		BaseVersion: "sha256:stale",
	})

	readFileResult(t, conn, MessageTypeFileWriteResult)
	assertNoMessageType(t, conn, MessageTypeFileWatch, 500*time.Millisecond)
}

func TestHandleFileCreate_AlreadyExistsNoWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "c-dup-watch",
		Path:      "README.md",
		Content:   "dup",
	})

	readFileResult(t, conn, MessageTypeFileCreateResult)
	assertNoMessageType(t, conn, MessageTypeFileWatch, 500*time.Millisecond)
}

func TestHandleFileDelete_UnconfirmedNoWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "d-unconfirmed-watch",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(false),
	})

	readFileResult(t, conn, MessageTypeFileDeleteResult)
	assertNoMessageType(t, conn, MessageTypeFileWatch, 500*time.Millisecond)
}

func TestHandleFileWrite_IdempotentReplayNoExtraWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// First write — generates one watch.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-watch",
		Path:        "README.md",
		Content:     "# Idem Watch\n",
		BaseVersion: readResult.Version,
	})
	readFileResult(t, conn, MessageTypeFileWriteResult)
	readFileWatch(t, conn) // consume the first watch

	// Replay — should get cached result, no extra watch.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "idem-watch",
		Path:        "README.md",
		Content:     "# Idem Watch\n",
		BaseVersion: readResult.Version,
	})
	readFileResult(t, conn, MessageTypeFileWriteResult)
	assertNoMessageType(t, conn, MessageTypeFileWatch, 500*time.Millisecond)
}

func TestHandleFileWrite_ResultBeforeWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read version.
	sendFileMsg(t, conn, MessageTypeFileRead, FileReadPayload{RequestID: "rv", Path: "README.md"})
	readRaw := readFileResult(t, conn, MessageTypeFileReadResult)
	var readResult FileReadResultPayload
	json.Unmarshal(readRaw, &readResult)

	// Write.
	sendFileMsg(t, conn, MessageTypeFileWrite, FileWritePayload{
		RequestID:   "order-check",
		Path:        "README.md",
		Content:     "# Order\n",
		BaseVersion: readResult.Version,
	})

	// Read messages in order. First should be write_result, then file.watch.
	deadline := time.Now().Add(3 * time.Second)
	var types []MessageType
	for time.Now().Before(deadline) && len(types) < 2 {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env struct {
			Type MessageType `json:"type"`
		}
		json.Unmarshal(data, &env)
		if env.Type == MessageTypeFileWriteResult || env.Type == MessageTypeFileWatch {
			types = append(types, env.Type)
		}
	}

	if len(types) < 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(types), types)
	}
	if types[0] != MessageTypeFileWriteResult {
		t.Errorf("expected first message to be %s, got %s", MessageTypeFileWriteResult, types[0])
	}
	if types[1] != MessageTypeFileWatch {
		t.Errorf("expected second message to be %s, got %s", MessageTypeFileWatch, types[1])
	}
}

func TestHandleFileCreate_ResultBeforeWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileCreate, FileCreatePayload{
		RequestID: "create-order-check",
		Path:      "create-order.txt",
		Content:   "create order content\n",
	})

	assertResultBeforeWatch(t, conn, MessageTypeFileCreateResult)
}

func TestHandleFileDelete_ResultBeforeWatch(t *testing.T) {
	dir := makeRepo(t)
	s, ts := newTestServer()
	defer ts.Close()
	defer s.Stop()

	s.SetFileOperations(NewFileOperations(dir, 1048576))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sendFileMsg(t, conn, MessageTypeFileDelete, FileDeletePayload{
		RequestID: "delete-order-check",
		Path:      "docs/guide.txt",
		Confirmed: boolPtr(true),
	})

	assertResultBeforeWatch(t, conn, MessageTypeFileDeleteResult)
}

func assertResultBeforeWatch(t *testing.T, conn *websocket.Conn, resultType MessageType) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	var types []MessageType
	for time.Now().Before(deadline) && len(types) < 2 {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env struct {
			Type MessageType `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type == resultType || env.Type == MessageTypeFileWatch {
			types = append(types, env.Type)
		}
	}

	if len(types) < 2 {
		t.Fatalf("expected 2 messages (%s then %s), got %d: %v", resultType, MessageTypeFileWatch, len(types), types)
	}
	if types[0] != resultType {
		t.Errorf("expected first message to be %s, got %s", resultType, types[0])
	}
	if types[1] != MessageTypeFileWatch {
		t.Errorf("expected second message to be %s, got %s", MessageTypeFileWatch, types[1])
	}
}
