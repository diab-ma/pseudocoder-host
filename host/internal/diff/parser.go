package diff

import (
	"regexp"
	"strconv"
	"strings"
)

// Parser splits raw git diff output into individual chunks.
//
// Binary file handling: Binary files in git diff output appear with a
// "Binary files differ" message instead of a chunk header. Since there is
// no @@ chunk header, binary files will NOT produce any chunks. This is
// intentional - binary diffs are not useful for code review.
//
// If you need to detect binary file changes, check for files that appear
// in the diff header but produce no chunks.
type Parser struct{}

// NewParser creates a new diff parser.
func NewParser() *Parser {
	return &Parser{}
}

// chunkHeaderRegex matches git diff chunk headers like:
// @@ -1,5 +1,7 @@
// @@ -0,0 +1,10 @@ (new file)
// @@ -1,10 +0,0 @@ (deleted file)
var chunkHeaderRegex = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// fileHeaderRegex matches the diff file header like:
// diff --git a/path/to/file b/path/to/file
var fileHeaderRegex = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)

// Parse takes raw git diff output and returns a slice of Chunks.
// Each chunk represents a contiguous change in a file.
func (p *Parser) Parse(diffOutput string) []*Chunk {
	if diffOutput == "" {
		return nil
	}

	var chunks []*Chunk
	var currentFile string
	var currentChunkLines []string
	var currentOldStart, currentOldCount, currentNewStart, currentNewCount int
	inChunk := false

	lines := strings.Split(diffOutput, "\n")

	for _, line := range lines {
		// Check for new file header
		if matches := fileHeaderRegex.FindStringSubmatch(line); matches != nil {
			// Save previous chunk if exists
			if inChunk && len(currentChunkLines) > 0 {
				chunks = append(chunks, NewChunk(
					currentFile,
					currentOldStart, currentOldCount,
					currentNewStart, currentNewCount,
					strings.Join(currentChunkLines, "\n"),
				))
			}
			currentFile = matches[2] // Use the "b/" path (new file path)
			currentChunkLines = nil
			inChunk = false
			continue
		}

		// Check for chunk header
		if matches := chunkHeaderRegex.FindStringSubmatch(line); matches != nil {
			// Save previous chunk if exists
			if inChunk && len(currentChunkLines) > 0 {
				chunks = append(chunks, NewChunk(
					currentFile,
					currentOldStart, currentOldCount,
					currentNewStart, currentNewCount,
					strings.Join(currentChunkLines, "\n"),
				))
			}

			// Parse new chunk header
			currentOldStart, _ = strconv.Atoi(matches[1])
			currentOldCount = 1 // default if not specified
			if matches[2] != "" {
				currentOldCount, _ = strconv.Atoi(matches[2])
			}
			currentNewStart, _ = strconv.Atoi(matches[3])
			currentNewCount = 1 // default if not specified
			if matches[4] != "" {
				currentNewCount, _ = strconv.Atoi(matches[4])
			}

			currentChunkLines = []string{line}
			inChunk = true
			continue
		}

		// Accumulate chunk content (context, additions, deletions)
		if inChunk {
			// Include lines that are part of the chunk content
			if len(line) == 0 || line[0] == ' ' || line[0] == '+' || line[0] == '-' || line[0] == '\\' {
				currentChunkLines = append(currentChunkLines, line)
			}
		}
	}

	// Don't forget the last chunk
	if inChunk && len(currentChunkLines) > 0 {
		chunks = append(chunks, NewChunk(
			currentFile,
			currentOldStart, currentOldCount,
			currentNewStart, currentNewCount,
			strings.Join(currentChunkLines, "\n"),
		))
	}

	return chunks
}

// AggregateByFile groups chunks by their file path.
// Returns a map from file path to all chunks for that file.
func AggregateByFile(chunks []*Chunk) map[string][]*Chunk {
	result := make(map[string][]*Chunk)
	for _, h := range chunks {
		result[h.File] = append(result[h.File], h)
	}
	return result
}

// ConcatChunkContent joins chunk contents for a file.
// Preserves chunk headers (@@ lines) so the mobile app can display them properly.
func ConcatChunkContent(chunks []*Chunk) string {
	var parts []string
	for _, h := range chunks {
		parts = append(parts, h.Content)
	}
	return strings.Join(parts, "\n")
}

// binaryFileRegex matches binary file markers in git diff output.
var binaryFileRegex = regexp.MustCompile(`^Binary files .+ and .+ differ`)

// ParseBinaryFiles returns a set of file paths that are binary in the diff output.
// Binary files have a "Binary files ... differ" message instead of chunk headers.
func ParseBinaryFiles(diffOutput string) map[string]bool {
	result := make(map[string]bool)
	var currentFile string

	lines := strings.Split(diffOutput, "\n")
	for _, line := range lines {
		// Check for new file header
		if matches := fileHeaderRegex.FindStringSubmatch(line); matches != nil {
			currentFile = matches[2] // Use the "b/" path (new file path)
			continue
		}

		// Check for binary marker
		if binaryFileRegex.MatchString(line) && currentFile != "" {
			result[currentFile] = true
		}
	}

	return result
}

// IsBinaryFile checks if a specific file is binary in the diff output.
func IsBinaryFile(diffOutput, file string) bool {
	binaryFiles := ParseBinaryFiles(diffOutput)
	return binaryFiles[file]
}

var deletedFileRegex = regexp.MustCompile(`^deleted file mode`)

// ParseDeletedFiles returns a set of file paths that are deletions in the diff output.
// Deleted files have a "deleted file mode" line after the header.
func ParseDeletedFiles(diffOutput string) map[string]bool {
	result := make(map[string]bool)
	var currentFile string

	lines := strings.Split(diffOutput, "\n")
	for _, line := range lines {
		// Check for new file header
		if matches := fileHeaderRegex.FindStringSubmatch(line); matches != nil {
			currentFile = matches[2] // Use the "b/" path
			continue
		}

		// Check for deleted file marker
		if deletedFileRegex.MatchString(line) && currentFile != "" {
			result[currentFile] = true
		}
	}

	return result
}

// IsDeletedFile checks if a specific file is a deletion in the diff output.
func IsDeletedFile(diffOutput, file string) bool {
	deletedFiles := ParseDeletedFiles(diffOutput)
	return deletedFiles[file]
}

// ExtractFileDiffSection extracts the raw diff section for a specific file.
// This includes the diff header, index line (with blob hashes), and any content.
// For binary files, this includes the "Binary files differ" line.
// Returns empty string if file not found in diff output.
func ExtractFileDiffSection(diffOutput, file string) string {
	if diffOutput == "" || file == "" {
		return ""
	}

	lines := strings.Split(diffOutput, "\n")
	var result []string
	inFile := false

	for _, line := range lines {
		// Check for new file header
		if matches := fileHeaderRegex.FindStringSubmatch(line); matches != nil {
			if inFile {
				// Hit next file, stop
				break
			}
			// Check if this is our target file
			if matches[2] == file {
				inFile = true
				result = append(result, line)
			}
			continue
		}

		if inFile {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// DiffStats contains size metrics for a diff.
type DiffStats struct {
	ByteSize     int
	LineCount    int
	AddedLines   int
	DeletedLines int
}

// Large diff thresholds for UI warnings.
const (
	// LargeDiffByteThreshold is 1MB - diffs larger than this trigger a warning.
	LargeDiffByteThreshold = 1 * 1024 * 1024

	// LargeDiffLineThreshold is 2000 lines - diffs with more lines trigger a warning.
	LargeDiffLineThreshold = 2000
)

// BinaryDiffPlaceholder is the placeholder text shown for binary file diffs.
// Binary files cannot display meaningful text diffs, so this is shown instead.
const BinaryDiffPlaceholder = "(binary file - content not shown)"

// CalculateDiffStats computes size metrics for a diff string.
// Returns statistics about byte size, line counts, and additions/deletions.
func CalculateDiffStats(diff string) *DiffStats {
	stats := &DiffStats{
		ByteSize: len(diff),
	}

	lines := strings.Split(diff, "\n")
	stats.LineCount = len(lines)

	for _, line := range lines {
		if len(line) > 0 {
			switch line[0] {
			case '+':
				// Don't count chunk header lines as additions (+++ line)
				if !strings.HasPrefix(line, "+++") {
					stats.AddedLines++
				}
			case '-':
				// Don't count chunk header lines as deletions (--- line)
				if !strings.HasPrefix(line, "---") {
					stats.DeletedLines++
				}
			}
		}
	}

	return stats
}

// IsLargeDiff checks if the given stats indicate a large diff.
// Returns true if the diff exceeds either the byte or line threshold.
func IsLargeDiff(stats *DiffStats) bool {
	if stats == nil {
		return false
	}
	return stats.ByteSize > LargeDiffByteThreshold || stats.LineCount > LargeDiffLineThreshold
}
