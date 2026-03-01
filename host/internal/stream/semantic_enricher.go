package stream

import (
	"log"

	"github.com/pseudocoder/host/internal/semantic"
)

// EnrichChunksWithSemantics runs semantic analysis on chunks and returns
// enriched chunks plus semantic groups. This is the single entry point
// used by all four emit paths (live, replay, reconnect, undo).
//
// The function is wrapped in defer/recover to guarantee that semantic
// analysis never prevents card emission.
func EnrichChunksWithSemantics(cardID, filePath string, chunks []ChunkInfo, isBinary, isDeleted bool) (enriched []ChunkInfo, groups []SemanticGroupInfo) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Semantic enrichment panic recovered for card %s: %v", cardID, r)
			enriched = chunks
			groups = nil
		}
	}()

	if len(chunks) == 0 && !isBinary && !isDeleted {
		return chunks, nil
	}

	// For binary/deleted cards with no chunks, create a synthetic chunk
	// for the analyzer to produce a group, but return original (empty)
	// chunks to preserve the binary/deleted card contract.
	syntheticBinaryDeleted := len(chunks) == 0 && (isBinary || isDeleted)
	analyzerChunks := chunks
	if syntheticBinaryDeleted {
		analyzerChunks = []ChunkInfo{{Index: 0}}
	}

	// Convert stream types to analyzer input.
	chunkInputs := make([]semantic.ChunkInput, len(analyzerChunks))
	for i, c := range analyzerChunks {
		chunkInputs[i] = semantic.ChunkInput{
			Index:       c.Index,
			Content:     c.Content,
			OldStart:    c.OldStart,
			OldCount:    c.OldCount,
			NewStart:    c.NewStart,
			NewCount:    c.NewCount,
			ContentHash: c.ContentHash,
		}
	}

	input := semantic.AnalyzerInput{
		CardID:    cardID,
		FilePath:  filePath,
		Chunks:    chunkInputs,
		IsBinary:  isBinary,
		IsDeleted: isDeleted,
	}

	output := semantic.Analyze(input)

	// For synthetic binary/deleted, return original empty chunks but keep groups.
	if syntheticBinaryDeleted {
		enriched = chunks
	} else {
		// Map annotations back onto chunk fields.
		enriched = make([]ChunkInfo, len(chunks))
		copy(enriched, chunks)
		for i, ann := range output.Chunks {
			if i < len(enriched) {
				enriched[i].SemanticKind = string(ann.Kind)
				enriched[i].SemanticLabel = ann.Label
				enriched[i].SemanticGroupID = ann.GroupID
			}
		}
	}

	// Convert analyzer groups to stream types.
	if len(output.Groups) > 0 {
		groups = make([]SemanticGroupInfo, len(output.Groups))
		for i, g := range output.Groups {
			var indexes []int
			if g.ChunkIndexes != nil {
				indexes = make([]int, len(g.ChunkIndexes))
				copy(indexes, g.ChunkIndexes)
			}
			groups[i] = SemanticGroupInfo{
				GroupID:      g.GroupID,
				Label:        g.Label,
				Kind:         string(g.Kind),
				LineStart:    g.LineStart,
				LineEnd:      g.LineEnd,
				ChunkIndexes: indexes,
				RiskLevel:    g.RiskLevel,
			}
		}
	}

	return enriched, groups
}
