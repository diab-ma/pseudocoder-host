// Package diff provides git diff polling and chunk parsing for review cards.
// It monitors a git repository for changes and splits diffs into
// chunk-sized cards that can be reviewed individually.
package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Chunk represents a single diff chunk from a file change.
// Each chunk becomes a review card that can be accepted or rejected.
type Chunk struct {
	// ID is a unique identifier for this chunk, derived from content hash.
	ID string `json:"id"`

	// File is the path to the file that was changed.
	File string `json:"file"`

	// OldStart is the starting line number in the original file.
	OldStart int `json:"old_start"`

	// OldCount is the number of lines in the original file.
	OldCount int `json:"old_count"`

	// NewStart is the starting line number in the new file.
	NewStart int `json:"new_start"`

	// NewCount is the number of lines in the new file.
	NewCount int `json:"new_count"`

	// Content is the raw diff content including context and +/- lines.
	Content string `json:"content"`

	// CreatedAt is when this chunk was detected (Unix milliseconds).
	CreatedAt int64 `json:"created_at"`
}

// NewChunk creates a Chunk with a generated ID based on file and content.
// The ID is a truncated SHA256 hash for uniqueness without excessive length.
func NewChunk(file string, oldStart, oldCount, newStart, newCount int, content string) *Chunk {
	h := &Chunk{
		File:      file,
		OldStart:  oldStart,
		OldCount:  oldCount,
		NewStart:  newStart,
		NewCount:  newCount,
		Content:   content,
		CreatedAt: time.Now().UnixMilli(),
	}
	h.ID = h.generateID()
	return h
}

// generateID creates a unique ID from file path and content hash.
// Uses first 12 chars of SHA256 for readability while maintaining uniqueness.
func (h *Chunk) generateID() string {
	data := fmt.Sprintf("%s:%d:%d:%s", h.File, h.OldStart, h.NewStart, h.Content)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:12]
}

// FileCardID generates a deterministic card ID from a file path.
// The ID is stable regardless of diff content, allowing updates to the same card.
// Uses "fc-" prefix to distinguish file-based cards from chunk-based cards.
func FileCardID(file string) string {
	hash := sha256.Sum256([]byte(file))
	return "fc-" + hex.EncodeToString(hash[:])[:12]
}
