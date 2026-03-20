package stream

import (
	"log"

	"github.com/pseudocoder/host/internal/semantic"
)

// EnrichFileWithSemantics runs semantic analysis on a file's diff content and
// returns semantic groups. This is the single entry point used by all emit
// paths (live, replay, reconnect, undo).
//
// The function is wrapped in defer/recover to guarantee that semantic
// analysis never prevents card emission.
func EnrichFileWithSemantics(cardID, filePath string, diffContent string, isBinary, isDeleted bool) (groups []SemanticGroupInfo) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Semantic enrichment panic recovered for card %s: %v", cardID, r)
			groups = nil
		}
	}()

	if diffContent == "" && !isBinary && !isDeleted {
		return nil
	}

	// Create a single synthetic ChunkInput with the full diff content.
	var chunkInputs []semantic.ChunkInput
	if isBinary || isDeleted {
		// For binary/deleted cards, create a synthetic chunk for the analyzer.
		chunkInputs = []semantic.ChunkInput{{Index: 0}}
	} else {
		chunkInputs = []semantic.ChunkInput{{
			Index:   0,
			Content: diffContent,
		}}
	}

	input := semantic.AnalyzerInput{
		CardID:    cardID,
		FilePath:  filePath,
		Chunks:    chunkInputs,
		IsBinary:  isBinary,
		IsDeleted: isDeleted,
	}

	output := semantic.Analyze(input)

	// Convert analyzer groups to stream types with ChunkIndexes set to nil.
	if len(output.Groups) == 0 {
		return nil
	}

	groups = make([]SemanticGroupInfo, len(output.Groups))
	for i, g := range output.Groups {
		groups[i] = SemanticGroupInfo{
			GroupID:      g.GroupID,
			Label:        g.Label,
			Kind:         string(g.Kind),
			LineStart:    g.LineStart,
			LineEnd:      g.LineEnd,
			ChunkIndexes: nil,
			RiskLevel:    g.RiskLevel,
		}
	}

	return groups
}
