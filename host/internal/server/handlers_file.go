package server

import (
	"encoding/json"
	"log"

	apperrors "github.com/pseudocoder/host/internal/errors"
)

// handleFileList processes a file.list message from the client.
func (c *Client) handleFileList(data []byte) {
	var msg struct {
		Type    MessageType     `json:"type"`
		Payload FileListPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse file.list payload: %v", err)
		c.sendFileListResult(NewFileListResultMessage("", "", false, nil,
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if payload.RequestID == "" {
		c.sendFileListResult(NewFileListResultMessage("", "", false, nil,
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	c.server.mu.RLock()
	ops := c.server.fileOps
	c.server.mu.RUnlock()

	if ops == nil {
		c.sendFileListResult(NewFileListResultMessage(payload.RequestID, payload.Path, false, nil,
			apperrors.CodeServerHandlerMissing, "file operations not configured"))
		return
	}

	entries, canonPath, err := ops.List(payload.Path)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendFileListResult(NewFileListResultMessage(payload.RequestID, payload.Path, false, nil, code, message))
		return
	}

	c.sendFileListResult(NewFileListResultMessage(payload.RequestID, canonPath, true, entries, "", ""))
}

// handleFileRead processes a file.read message from the client.
func (c *Client) handleFileRead(data []byte) {
	var msg struct {
		Type    MessageType     `json:"type"`
		Payload FileReadPayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse file.read payload: %v", err)
		c.sendFileReadResult(NewFileReadResultMessage("", "", false, "", "", "", "", "",
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if payload.RequestID == "" {
		c.sendFileReadResult(NewFileReadResultMessage("", "", false, "", "", "", "", "",
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	if payload.Path == "" {
		c.sendFileReadResult(NewFileReadResultMessage(payload.RequestID, "", false, "", "", "", "", "",
			apperrors.CodeServerInvalidMessage, "path is required"))
		return
	}

	c.server.mu.RLock()
	ops := c.server.fileOps
	c.server.mu.RUnlock()

	if ops == nil {
		c.sendFileReadResult(NewFileReadResultMessage(payload.RequestID, payload.Path, false, "", "", "", "", "",
			apperrors.CodeServerHandlerMissing, "file operations not configured"))
		return
	}

	readData, canonPath, err := ops.Read(payload.Path)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		c.sendFileReadResult(NewFileReadResultMessage(payload.RequestID, payload.Path, false, "", "", "", "", "", code, message))
		return
	}

	c.sendFileReadResult(NewFileReadResultMessage(
		payload.RequestID, canonPath, true,
		readData.Content, readData.Encoding, readData.LineEnding,
		readData.Version, readData.ReadOnlyReason, "", ""))
}

// handleFileWrite processes a file.write message from the client.
func (c *Client) handleFileWrite(data []byte) {
	var msg struct {
		Type    MessageType      `json:"type"`
		Payload FileWritePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse file.write payload: %v", err)
		c.sendFileWriteResult(NewFileWriteResultMessage("", "", false, "",
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if payload.RequestID == "" {
		c.sendFileWriteResult(NewFileWriteResultMessage("", "", false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	// Idempotency check and cache key setup happen before other validation so
	// deterministic validation failures are cached by request_id as well.
	canon := bestEffortCanon(payload.Path)
	fp := writeFingerprint(canon, payload.Content, payload.BaseVersion)
	if cached, hit, err := c.idempotencyCheck(MessageTypeFileWrite, payload.RequestID, fp); hit {
		c.sendFileWriteResult(cached)
		return
	} else if err != nil {
		c.sendFileWriteResult(NewFileWriteResultMessage(payload.RequestID, payload.Path, false, "",
			apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}

	if payload.Path == "" {
		result := NewFileWriteResultMessage(payload.RequestID, "", false, "",
			apperrors.CodeServerInvalidMessage, "path is required")
		c.idempotencyStore(MessageTypeFileWrite, payload.RequestID, fp, result)
		c.sendFileWriteResult(result)
		return
	}

	if payload.BaseVersion == "" {
		result := NewFileWriteResultMessage(payload.RequestID, payload.Path, false, "",
			apperrors.CodeServerInvalidMessage, "base_version is required")
		c.idempotencyStore(MessageTypeFileWrite, payload.RequestID, fp, result)
		c.sendFileWriteResult(result)
		return
	}

	c.server.mu.RLock()
	ops := c.server.fileOps
	c.server.mu.RUnlock()

	if ops == nil {
		result := NewFileWriteResultMessage(payload.RequestID, payload.Path, false, "",
			apperrors.CodeServerHandlerMissing, "file operations not configured")
		c.idempotencyStore(MessageTypeFileWrite, payload.RequestID, fp, result)
		c.sendFileWriteResult(result)
		return
	}

	canonPath, newVersion, err := ops.Write(payload.Path, payload.Content, payload.BaseVersion)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewFileWriteResultMessage(payload.RequestID, payload.Path, false, "", code, message)
		c.idempotencyStore(MessageTypeFileWrite, payload.RequestID, fp, result)
		c.sendFileWriteResult(result)
		return
	}

	result := NewFileWriteResultMessage(payload.RequestID, canonPath, true, newVersion, "", "")
	c.idempotencyStore(MessageTypeFileWrite, payload.RequestID, fp, result)
	c.sendFileWriteResult(result)
	c.server.BroadcastFileWatch(canonPath, "modified", newVersion)
}

// handleFileCreate processes a file.create message from the client.
func (c *Client) handleFileCreate(data []byte) {
	var msg struct {
		Type    MessageType       `json:"type"`
		Payload FileCreatePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse file.create payload: %v", err)
		c.sendFileCreateResult(NewFileCreateResultMessage("", "", false, "",
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if payload.RequestID == "" {
		c.sendFileCreateResult(NewFileCreateResultMessage("", "", false, "",
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	// Idempotency check and cache key setup happen before other validation so
	// deterministic validation failures are cached by request_id as well.
	canon := bestEffortCanon(payload.Path)
	fp := createFingerprint(canon, payload.Content)
	if cached, hit, err := c.idempotencyCheck(MessageTypeFileCreate, payload.RequestID, fp); hit {
		c.sendFileCreateResult(cached)
		return
	} else if err != nil {
		c.sendFileCreateResult(NewFileCreateResultMessage(payload.RequestID, payload.Path, false, "",
			apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}

	if payload.Path == "" {
		result := NewFileCreateResultMessage(payload.RequestID, "", false, "",
			apperrors.CodeServerInvalidMessage, "path is required")
		c.idempotencyStore(MessageTypeFileCreate, payload.RequestID, fp, result)
		c.sendFileCreateResult(result)
		return
	}

	c.server.mu.RLock()
	ops := c.server.fileOps
	c.server.mu.RUnlock()

	if ops == nil {
		result := NewFileCreateResultMessage(payload.RequestID, payload.Path, false, "",
			apperrors.CodeServerHandlerMissing, "file operations not configured")
		c.idempotencyStore(MessageTypeFileCreate, payload.RequestID, fp, result)
		c.sendFileCreateResult(result)
		return
	}

	canonPath, version, err := ops.Create(payload.Path, payload.Content)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewFileCreateResultMessage(payload.RequestID, payload.Path, false, "", code, message)
		c.idempotencyStore(MessageTypeFileCreate, payload.RequestID, fp, result)
		c.sendFileCreateResult(result)
		return
	}

	result := NewFileCreateResultMessage(payload.RequestID, canonPath, true, version, "", "")
	c.idempotencyStore(MessageTypeFileCreate, payload.RequestID, fp, result)
	c.sendFileCreateResult(result)
	c.server.BroadcastFileWatch(canonPath, "created", version)
}

// handleFileDelete processes a file.delete message from the client.
func (c *Client) handleFileDelete(data []byte) {
	var msg struct {
		Type    MessageType       `json:"type"`
		Payload FileDeletePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to parse file.delete payload: %v", err)
		c.sendFileDeleteResult(NewFileDeleteResultMessage("", "", false,
			apperrors.CodeServerInvalidMessage, "invalid message format"))
		return
	}

	payload := msg.Payload
	if payload.RequestID == "" {
		c.sendFileDeleteResult(NewFileDeleteResultMessage("", "", false,
			apperrors.CodeServerInvalidMessage, "request_id is required"))
		return
	}

	// Idempotency check and cache key setup happen before other validation so
	// deterministic validation failures are cached by request_id as well.
	canon := bestEffortCanon(payload.Path)
	fp := deleteFingerprint(canon, payload.Confirmed)
	if cached, hit, err := c.idempotencyCheck(MessageTypeFileDelete, payload.RequestID, fp); hit {
		c.sendFileDeleteResult(cached)
		return
	} else if err != nil {
		c.sendFileDeleteResult(NewFileDeleteResultMessage(payload.RequestID, payload.Path, false,
			apperrors.CodeServerInvalidMessage, err.Error()))
		return
	}

	if payload.Path == "" {
		result := NewFileDeleteResultMessage(payload.RequestID, "", false,
			apperrors.CodeServerInvalidMessage, "path is required")
		c.idempotencyStore(MessageTypeFileDelete, payload.RequestID, fp, result)
		c.sendFileDeleteResult(result)
		return
	}
	if payload.Confirmed == nil {
		result := NewFileDeleteResultMessage(payload.RequestID, payload.Path, false,
			apperrors.CodeServerInvalidMessage, "confirmed is required")
		c.idempotencyStore(MessageTypeFileDelete, payload.RequestID, fp, result)
		c.sendFileDeleteResult(result)
		return
	}

	c.server.mu.RLock()
	ops := c.server.fileOps
	c.server.mu.RUnlock()

	if ops == nil {
		result := NewFileDeleteResultMessage(payload.RequestID, payload.Path, false,
			apperrors.CodeServerHandlerMissing, "file operations not configured")
		c.idempotencyStore(MessageTypeFileDelete, payload.RequestID, fp, result)
		c.sendFileDeleteResult(result)
		return
	}

	canonPath, err := ops.Delete(payload.Path, *payload.Confirmed)
	if err != nil {
		code, message := apperrors.ToCodeAndMessage(err)
		result := NewFileDeleteResultMessage(payload.RequestID, payload.Path, false, code, message)
		c.idempotencyStore(MessageTypeFileDelete, payload.RequestID, fp, result)
		c.sendFileDeleteResult(result)
		return
	}

	result := NewFileDeleteResultMessage(payload.RequestID, canonPath, true, "", "")
	c.idempotencyStore(MessageTypeFileDelete, payload.RequestID, fp, result)
	c.sendFileDeleteResult(result)
	c.server.BroadcastFileWatch(canonPath, "deleted", "")
}
