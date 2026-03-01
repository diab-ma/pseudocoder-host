// Package server provides mapping helpers for converting stream types to server types.
// These functions are used when emitting diff.card messages to clients.
package server

import (
	"github.com/pseudocoder/host/internal/stream"
)

// mapChunksToServer converts a slice of stream.ChunkInfo to server.ChunkInfo.
// This mapping is necessary because the stream and server packages define
// their own ChunkInfo types to avoid import cycles. The types have identical
// fields; this function performs a straightforward field-by-field copy.
func mapChunksToServer(chunks []stream.ChunkInfo) []ChunkInfo {
	if len(chunks) == 0 {
		return nil
	}
	serverChunks := make([]ChunkInfo, len(chunks))
	for i, h := range chunks {
		serverChunks[i] = ChunkInfo{
			Index:           h.Index,
			OldStart:        h.OldStart,
			OldCount:        h.OldCount,
			NewStart:        h.NewStart,
			NewCount:        h.NewCount,
			Offset:          h.Offset,
			Length:          h.Length,
			Content:         h.Content,
			ContentHash:     h.ContentHash,
			GroupIndex:      h.GroupIndex,
			SemanticKind:    h.SemanticKind,
			SemanticLabel:   h.SemanticLabel,
			SemanticGroupID: h.SemanticGroupID,
		}
	}
	return serverChunks
}

// mapChunkGroupsToServer converts a slice of stream.ChunkGroupInfo to server.ChunkGroupInfo.
// This mapping is necessary because the stream and server packages define
// their own ChunkGroupInfo types to avoid import cycles. The types have identical
// fields; this function performs a straightforward field-by-field copy.
// Returns nil if the input is nil (grouping disabled).
func mapChunkGroupsToServer(groups []stream.ChunkGroupInfo) []ChunkGroupInfo {
	if groups == nil {
		return nil
	}
	serverGroups := make([]ChunkGroupInfo, len(groups))
	for i, g := range groups {
		serverGroups[i] = ChunkGroupInfo{
			GroupIndex: g.GroupIndex,
			LineStart:  g.LineStart,
			LineEnd:    g.LineEnd,
			ChunkCount: g.ChunkCount,
		}
	}
	return serverGroups
}

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
