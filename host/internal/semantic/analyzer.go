package semantic

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// AnalyzerInput is the input to the semantic analyzer.
type AnalyzerInput struct {
	CardID    string
	FilePath  string
	Chunks    []ChunkInput
	IsBinary  bool
	IsDeleted bool
}

// ChunkInput describes a single diff chunk for analysis.
type ChunkInput struct {
	Index    int
	Content  string // raw chunk text including @@ header
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	// ContentHash is used as MemberID when non-empty.
	ContentHash string
}

// AnalyzerOutput is the result of semantic analysis.
type AnalyzerOutput struct {
	Chunks []ChunkAnnotation
	Groups []GroupInfo
}

// ChunkAnnotation carries per-chunk semantic metadata.
type ChunkAnnotation struct {
	Index   int
	Kind    SemanticKind
	Label   string
	GroupID string
}

// GroupInfo describes a semantic group of related chunks.
type GroupInfo struct {
	GroupID      string
	Label        string
	Kind         SemanticKind
	LineStart    int
	LineEnd      int
	ChunkIndexes []int
	RiskLevel    string
}

// Regex patterns for baseline heuristic classification.
var (
	importRe   = regexp.MustCompile(`^\s*(import\s+|from\s+\S+\s+import\s+)`)
	functionRe = regexp.MustCompile(`^\s*(func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(|(?:[A-Za-z_][A-Za-z0-9_<>,\[\]\?]*\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\([^)\n]*\)\s*(\{|=>))`)
)

// Analyze performs deterministic semantic analysis on diff chunks.
// It classifies each chunk, extracts labels, assembles groups, and
// assigns risk levels. The analysis is non-blocking and deterministic:
// same inputs always produce the same outputs.
func Analyze(input AnalyzerInput) AnalyzerOutput {
	annotations := make([]ChunkAnnotation, len(input.Chunks))

	// Phase 1: Baseline classification per chunk.
	for i, chunk := range input.Chunks {
		kind, label := classifyChunk(chunk, input.IsBinary, input.IsDeleted)
		annotations[i] = ChunkAnnotation{
			Index: chunk.Index,
			Kind:  kind,
			Label: label,
		}
	}

	// Phase 1.5: Tree-sitter enrichment (best-effort, supported extensions only).
	ext := filepath.Ext(input.FilePath)
	if !input.IsBinary && !input.IsDeleted && IsSupportedExtension(input.FilePath) {
		hints := TryEnrichChunks(context.Background(), ext, input.Chunks, defaultPerCardBudget)
		for i, hint := range hints {
			applyParserHint(&annotations[i], hint)
		}
	}

	// Phase 2: Group assembly (after enrichment so groups reflect parser upgrades).
	groups := assembleGroups(input, annotations)

	// Phase 3: Assign group IDs back to chunks.
	groupIDByIndex := make(map[int]string)
	for _, g := range groups {
		for _, idx := range g.ChunkIndexes {
			groupIDByIndex[idx] = g.GroupID
		}
	}
	for i := range annotations {
		annotations[i].GroupID = groupIDByIndex[annotations[i].Index]
	}

	return AnalyzerOutput{
		Chunks: annotations,
		Groups: groups,
	}
}

// classifyChunk determines the semantic kind and label for a single chunk
// using baseline heuristics (no parser).
func classifyChunk(chunk ChunkInput, isBinary, isDeleted bool) (SemanticKind, string) {
	// Precedence: binary > deleted > import > function > generic
	if isBinary {
		return KindBinary, ChunkLabel(KindBinary, "")
	}
	if isDeleted {
		return KindDeleted, ChunkLabel(KindDeleted, "")
	}

	changedLines := extractChangedLines(chunk.Content)

	// Check for import statements
	for _, line := range changedLines {
		if importRe.MatchString(line) {
			return KindImport, ChunkLabel(KindImport, "")
		}
	}

	// Check for function/method declarations
	for _, line := range changedLines {
		matches := functionRe.FindStringSubmatch(line)
		if matches != nil {
			// Extract function name from capture groups.
			// Group 2 is Go func name, Group 3 is general function name.
			funcName := matches[2]
			if funcName == "" {
				funcName = matches[3]
			}
			if funcName != "" {
				return KindFunction, funcName
			}
			return KindFunction, ChunkLabel(KindFunction, "")
		}
	}

	return KindGeneric, ChunkLabel(KindGeneric, "")
}

// isFallbackLabel returns true if the label is one of the standard
// chunk fallback labels (e.g., "Import", "Function", "Changes").
func isFallbackLabel(label string) bool {
	for _, fb := range chunkLabelFallback {
		if label == fb {
			return true
		}
	}
	return false
}

// applyParserHint merges a parser hint into a chunk annotation.
// It preserves explicit baseline labels (e.g., "HandleRequest") when the
// parser confirms the kind but provides no label of its own.
func applyParserHint(ann *ChunkAnnotation, hint ParserHint) {
	if !hint.OK {
		return
	}
	ann.Kind = hint.Kind
	if hint.Label != "" && isCleanLabel(hint.Label) {
		ann.Label = hint.Label
	} else if ann.Label == "" || isFallbackLabel(ann.Label) {
		ann.Label = ChunkLabel(hint.Kind, "")
	}
	// else: keep existing explicit baseline label
}

// extractChangedLines returns only the added/removed lines from chunk content,
// stripping the one-character diff prefix. Lines starting with +++ or --- are
// excluded (file headers).
func extractChangedLines(content string) []string {
	var result []string
	for _, line := range strings.Split(content, "\n") {
		if len(line) == 0 {
			continue
		}
		if (line[0] == '+' || line[0] == '-') &&
			!strings.HasPrefix(line, "+++") &&
			!strings.HasPrefix(line, "---") {
			// Strip the one-character diff prefix.
			result = append(result, line[1:])
		}
	}
	return result
}

// assembleGroups builds semantic groups from classified chunks.
// Group key is kind + "|" + label. Groups are deterministic and ordered.
func assembleGroups(input AnalyzerInput, annotations []ChunkAnnotation) []GroupInfo {
	// Collect chunks by group key.
	type groupData struct {
		kind      SemanticKind
		label     string
		indexes   []int
		memberIDs []string
		lineStart int
		lineEnd   int
	}
	groupMap := make(map[string]*groupData)
	var groupKeys []string // preserve first-seen order

	for i, ann := range annotations {
		key := string(ann.Kind) + "|" + ann.Label
		chunk := input.Chunks[i]

		// Compute line start/end with deletion-safe handling.
		ls, le := chunkLineRange(chunk)

		if gd, ok := groupMap[key]; ok {
			gd.indexes = append(gd.indexes, chunk.Index)
			gd.memberIDs = append(gd.memberIDs, MemberID(chunk.ContentHash, chunk.Index))
			if ls < gd.lineStart {
				gd.lineStart = ls
			}
			if le > gd.lineEnd {
				gd.lineEnd = le
			}
		} else {
			groupKeys = append(groupKeys, key)
			groupMap[key] = &groupData{
				kind:      ann.Kind,
				label:     ann.Label,
				indexes:   []int{chunk.Index},
				memberIDs: []string{MemberID(chunk.ContentHash, chunk.Index)},
				lineStart: ls,
				lineEnd:   le,
			}
		}
	}

	// Build groups in deterministic order (first-seen key order).
	groups := make([]GroupInfo, 0, len(groupKeys))
	for _, key := range groupKeys {
		gd := groupMap[key]

		// Ensure chunk_indexes are ascending and unique.
		sort.Ints(gd.indexes)
		gd.indexes = uniqueInts(gd.indexes)

		groupID := BuildGroupID(input.CardID, gd.kind, gd.memberIDs)
		riskLevel := computeGroupRisk(gd.kind, input.FilePath)

		groups = append(groups, GroupInfo{
			GroupID:      groupID,
			Label:        GroupLabel(gd.kind, gd.label),
			Kind:         gd.kind,
			LineStart:    gd.lineStart,
			LineEnd:      gd.lineEnd,
			ChunkIndexes: gd.indexes,
			RiskLevel:    riskLevel,
		})
	}

	return groups
}

// chunkLineRange returns start and end line numbers for a chunk,
// with deletion-safe handling (uses old-file lines when new-file is 0).
func chunkLineRange(chunk ChunkInput) (start, end int) {
	if chunk.NewCount > 0 {
		start = chunk.NewStart
		end = chunk.NewStart + chunk.NewCount - 1
	} else if chunk.OldCount > 0 {
		start = chunk.OldStart
		end = chunk.OldStart + chunk.OldCount - 1
	} else {
		start = chunk.NewStart
		end = chunk.NewStart
	}
	return
}

// computeGroupRisk determines the risk level for a semantic group
// based on the packet's locked risk contract.
func computeGroupRisk(kind SemanticKind, filePath string) string {
	sensitive := IsSensitivePath(filePath)

	switch {
	case sensitive && (kind == KindDeleted || kind == KindBinary):
		return "critical"
	case kind == KindDeleted || kind == KindBinary || (sensitive && kind == KindFunction):
		return "high"
	case kind == KindFunction || (sensitive && (kind == KindImport || kind == KindGeneric)):
		return "medium"
	default:
		return "low"
	}
}

// uniqueInts returns a deduplicated sorted slice.
func uniqueInts(sorted []int) []int {
	if len(sorted) <= 1 {
		return sorted
	}
	result := make([]int, 0, len(sorted))
	result = append(result, sorted[0])
	for i := 1; i < len(sorted); i++ {
		if sorted[i] != sorted[i-1] {
			result = append(result, sorted[i])
		}
	}
	return result
}
