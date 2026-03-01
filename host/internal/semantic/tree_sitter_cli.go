package semantic

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ParserHint carries the result of a tree-sitter parse attempt for one chunk.
type ParserHint struct {
	Kind  SemanticKind
	Label string
	OK    bool
}

// supportedExtensions maps file extensions to tree-sitter language names.
var supportedExtensions = map[string]string{
	".go":   "go",
	".dart": "dart",
}

// goImportNodes are tree-sitter node types that indicate Go imports.
var goImportNodes = map[string]bool{
	"import_declaration": true,
	"import_spec":        true,
}

// goFunctionNodes are tree-sitter node types that indicate Go functions.
var goFunctionNodes = map[string]bool{
	"function_declaration": true,
	"method_declaration":   true,
}

// dartImportNodes are tree-sitter node types that indicate Dart imports.
var dartImportNodes = map[string]bool{
	"import_or_export": true,
	"library_import":   true,
}

// dartFunctionNodes are tree-sitter node types that indicate Dart functions.
var dartFunctionNodes = map[string]bool{
	"function_signature": true,
	"method_signature":   true,
	"method_declaration": true,
}

// identifierPosRe matches tree-sitter name lines with position ranges.
// Example: name: (identifier) [0, 5] - [0, 18]
var identifierPosRe = regexp.MustCompile(`name:\s+\((?:identifier|field_identifier)\)\s+\[(\d+),\s*(\d+)\]\s*-\s*\[(\d+),\s*(\d+)\]`)

// treeSitterBinary is the tree-sitter CLI binary name. Package-level
// variable so tests can substitute a fake binary for non-zero exit tests.
var treeSitterBinary = "tree-sitter"

// parseChunkForEnrichment is used by TryEnrichChunks so tests can inject
// deterministic parser behavior and assert bounded-attempt semantics.
var parseChunkForEnrichment = TryParseChunk

// TryParseChunk runs tree-sitter parse on the given changed lines and returns
// a parser hint. Any failure (missing CLI, timeout, parse error) returns
// OK: false so the caller can fall back to baseline heuristics.
func TryParseChunk(ctx context.Context, ext string, changedLines []string, timeout time.Duration) ParserHint {
	lang, ok := supportedExtensions[ext]
	if !ok {
		return ParserHint{}
	}

	if len(changedLines) == 0 {
		return ParserHint{}
	}

	// Write changed lines to temp file with correct extension.
	tmpFile, err := os.CreateTemp("", "semantic-*"+ext)
	if err != nil {
		return ParserHint{}
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	content := strings.Join(changedLines, "\n") + "\n"
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return ParserHint{}
	}
	tmpFile.Close()

	// Run tree-sitter parse with timeout.
	parseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(parseCtx, treeSitterBinary, "parse", "--quiet", tmpPath)
	output, err := cmd.Output()
	if err != nil {
		return ParserHint{}
	}

	return mapParseOutputBytes(output, lang, changedLines)
}

// maxParserAttempts is the maximum number of chunks to try parsing per card.
const maxParserAttempts = 6

// defaultPerCardBudget is the total time budget for parser enrichment per card.
const defaultPerCardBudget = 120 * time.Millisecond

// defaultPerInvocationTimeout is the timeout for a single tree-sitter invocation.
const defaultPerInvocationTimeout = 20 * time.Millisecond

// TryEnrichChunks runs tree-sitter enrichment on eligible chunks, returning
// a hint per chunk. Chunks beyond maxParserAttempts or after the budget is
// exhausted get OK: false.
func TryEnrichChunks(ctx context.Context, ext string, chunks []ChunkInput, perCardBudget time.Duration) []ParserHint {
	hints := make([]ParserHint, len(chunks))

	if _, ok := supportedExtensions[ext]; !ok {
		return hints
	}

	if perCardBudget <= 0 {
		perCardBudget = defaultPerCardBudget
	}

	deadline := time.Now().Add(perCardBudget)
	attempts := 0

	for i, chunk := range chunks {
		if attempts >= maxParserAttempts {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		timeout := defaultPerInvocationTimeout
		if remaining < timeout {
			timeout = remaining
		}

		changedLines := extractChangedLines(chunk.Content)
		if len(changedLines) == 0 {
			continue
		}

		hints[i] = parseChunkForEnrichment(ctx, ext, changedLines, timeout)
		attempts++
	}

	return hints
}

// mapParseOutputBytes validates parse output as UTF-8 before text parsing.
// Invalid UTF-8 is treated as parse-output mismatch and falls back.
func mapParseOutputBytes(output []byte, lang string, changedLines []string) ParserHint {
	if len(output) == 0 {
		return ParserHint{}
	}
	if !utf8.Valid(output) {
		return ParserHint{}
	}
	return mapParseOutput(string(output), lang, changedLines)
}

// mapParseOutput maps tree-sitter parse output to a ParserHint.
// changedLines provides the source text for identifier extraction from positions.
func mapParseOutput(output, lang string, changedLines []string) ParserHint {
	if output == "" {
		return ParserHint{}
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Extract node type: first token before space or parenthesis.
		nodeType := extractNodeType(line)
		if nodeType == "" {
			continue
		}

		switch lang {
		case "go":
			if goImportNodes[nodeType] {
				return ParserHint{Kind: KindImport, Label: "", OK: true}
			}
			if goFunctionNodes[nodeType] {
				label := extractFunctionName(scanner, changedLines)
				return ParserHint{Kind: KindFunction, Label: label, OK: true}
			}
		case "dart":
			if dartImportNodes[nodeType] {
				return ParserHint{Kind: KindImport, Label: "", OK: true}
			}
			if dartFunctionNodes[nodeType] {
				label := extractFunctionName(scanner, changedLines)
				return ParserHint{Kind: KindFunction, Label: label, OK: true}
			}
		}
	}

	return ParserHint{}
}

// extractNodeType gets the tree-sitter node type from a parse output line.
// Lines look like: "  (function_declaration ..." or "(source_file ..."
func extractNodeType(line string) string {
	// Find first '(' and extract word after it
	idx := strings.Index(line, "(")
	if idx < 0 {
		return ""
	}
	rest := line[idx+1:]
	// Node type is the word up to the next space, paren, or end
	end := strings.IndexAny(rest, " \t()")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// extractFunctionName scans subsequent tree-sitter output lines for a
// name identifier with position range, then extracts the actual name
// from changedLines. Returns "" if extraction is not possible.
func extractFunctionName(scanner *bufio.Scanner, changedLines []string) string {
	for i := 0; i < 5 && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		// Stop if we enter sibling fields that precede name in some node types
		// (e.g. Go method_declaration has receiver: before name:).
		if strings.HasPrefix(line, "receiver:") ||
			strings.HasPrefix(line, "parameters:") ||
			strings.HasPrefix(line, "body:") ||
			strings.HasPrefix(line, "result:") {
			return ""
		}

		matches := identifierPosRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		startRow, err := strconv.Atoi(matches[1])
		if err != nil {
			return ""
		}
		startCol, err := strconv.Atoi(matches[2])
		if err != nil {
			return ""
		}
		endRow, err := strconv.Atoi(matches[3])
		if err != nil {
			return ""
		}
		endCol, err := strconv.Atoi(matches[4])
		if err != nil {
			return ""
		}

		// Only extract single-line identifiers.
		if startRow != endRow {
			return ""
		}
		if startRow < 0 || startRow >= len(changedLines) {
			return ""
		}

		srcLine := changedLines[startRow]
		if startCol < 0 || endCol > len(srcLine) || startCol >= endCol {
			return ""
		}

		name := srcLine[startCol:endCol]
		if isCleanLabel(name) {
			return name
		}
		return ""
	}
	return ""
}

// IsSupportedExtension returns true if the file extension is supported by
// the tree-sitter enrichment pipeline.
func IsSupportedExtension(filePath string) bool {
	ext := filepath.Ext(filePath)
	_, ok := supportedExtensions[ext]
	return ok
}

// ExtensionOf returns the file extension from a path.
func ExtensionOf(filePath string) string {
	return filepath.Ext(filePath)
}

// isCleanLabel checks that a label has no control characters and no
// leading/trailing whitespace.
func isCleanLabel(s string) bool {
	if s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
