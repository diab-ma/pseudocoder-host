// Package semantic provides types and helpers for semantic diff metadata.
// It defines the data model that C2 (extraction) and C3 (UI rendering)
// will build on. All fields are optional with omitempty semantics to
// ensure backward compatibility.
package semantic

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// SemanticKind classifies a diff chunk or group by its semantic role.
type SemanticKind string

const (
	KindImport  SemanticKind = "import"
	KindFunction SemanticKind = "function"
	KindGeneric  SemanticKind = "generic"
	KindBinary   SemanticKind = "binary"
	KindDeleted  SemanticKind = "deleted"
	KindRenamed  SemanticKind = "renamed"
)

// knownKinds is the set of recognized semantic kinds.
var knownKinds = map[SemanticKind]bool{
	KindImport:   true,
	KindFunction: true,
	KindGeneric:  true,
	KindBinary:   true,
	KindDeleted:  true,
	KindRenamed:  true,
}

// NormalizeKind returns a known SemanticKind for the given raw string.
// Unknown, empty, or case-mismatched values return KindGeneric.
func NormalizeKind(raw string) SemanticKind {
	kind := SemanticKind(strings.ToLower(raw))
	if knownKinds[kind] {
		return kind
	}
	return KindGeneric
}

// groupLabelFallback maps each kind to its default group label.
var groupLabelFallback = map[SemanticKind]string{
	KindImport:   "Imports",
	KindFunction: "Function changes",
	KindGeneric:  "Code changes",
	KindBinary:   "Binary change",
	KindDeleted:  "Deletion",
	KindRenamed:  "Rename",
}

// chunkLabelFallback maps each kind to its default chunk label.
var chunkLabelFallback = map[SemanticKind]string{
	KindImport:   "Import",
	KindFunction: "Function",
	KindGeneric:  "Changes",
	KindBinary:   "Binary",
	KindDeleted:  "Deleted",
	KindRenamed:  "Renamed",
}

// GroupLabel returns the display label for a semantic group.
// If explicit is non-empty it is returned directly; otherwise the
// fallback for the given kind is used.
func GroupLabel(kind SemanticKind, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if label, ok := groupLabelFallback[kind]; ok {
		return label
	}
	return groupLabelFallback[KindGeneric]
}

// ChunkLabel returns the display label for a single chunk.
// If explicit is non-empty it is returned directly; otherwise the
// fallback for the given kind is used.
func ChunkLabel(kind SemanticKind, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if label, ok := chunkLabelFallback[kind]; ok {
		return label
	}
	return chunkLabelFallback[KindGeneric]
}

// BuildGroupID produces a deterministic, order-independent group identifier.
// The ID is built from cardID, kind, and sorted memberIDs using SHA-256.
// Format: "sg-" + first 12 hex chars of SHA-256.
func BuildGroupID(cardID string, kind SemanticKind, memberIDs []string) string {
	sorted := make([]string, len(memberIDs))
	copy(sorted, memberIDs)
	sort.Strings(sorted)

	seed := fmt.Sprintf("%s|%s|%s", cardID, string(kind), strings.Join(sorted, ","))
	hash := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("sg-%x", hash[:6])
}

// MemberID returns a stable identifier for a chunk within a group.
// If contentHash is non-empty it is returned directly; otherwise
// the fallback "idx:<index>" is used.
func MemberID(contentHash string, chunkIndex int) string {
	if contentHash != "" {
		return contentHash
	}
	return fmt.Sprintf("idx:%d", chunkIndex)
}
