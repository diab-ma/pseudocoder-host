// Package server provides mapping helpers for converting stream types to server types.
// These functions are used when emitting diff.card messages to clients.
package server

import (
	"github.com/pseudocoder/host/internal/stream"
)

// mapSemanticGroupsToServer converts a slice of stream.SemanticGroupInfo to server.SemanticGroupInfo.
// This mapping is necessary because the stream and server packages define
// their own SemanticGroupInfo types to avoid import cycles.
// Returns nil if the input is nil (semantic analysis disabled).
func mapSemanticGroupsToServer(groups []stream.SemanticGroupInfo) []SemanticGroupInfo {
	if groups == nil {
		return nil
	}
	serverGroups := make([]SemanticGroupInfo, len(groups))
	for i, g := range groups {
		var chunkIndexes []int
		if g.ChunkIndexes != nil {
			chunkIndexes = make([]int, len(g.ChunkIndexes))
			copy(chunkIndexes, g.ChunkIndexes)
		}
		serverGroups[i] = SemanticGroupInfo{
			GroupID:      g.GroupID,
			Label:        g.Label,
			Kind:         g.Kind,
			LineStart:    g.LineStart,
			LineEnd:      g.LineEnd,
			ChunkIndexes: chunkIndexes,
			RiskLevel:    g.RiskLevel,
		}
	}
	return serverGroups
}

// mapStatsToServer converts a stream.DiffStats to server.DiffStats.
// This mapping is necessary because the stream and server packages define
// their own DiffStats types to avoid import cycles. The types have identical
// fields; this function performs a straightforward field-by-field copy.
// Returns nil if the input is nil.
func mapStatsToServer(stats *stream.DiffStats) *DiffStats {
	if stats == nil {
		return nil
	}
	return &DiffStats{
		ByteSize:     stats.ByteSize,
		LineCount:    stats.LineCount,
		AddedLines:   stats.AddedLines,
		DeletedLines: stats.DeletedLines,
	}
}
