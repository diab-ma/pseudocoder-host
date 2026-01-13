package diff

import (
	"strings"
	"testing"
)

func TestParser_Parse_EmptyInput(t *testing.T) {
	p := NewParser()
	chunks := p.Parse("")
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty input, got %d", len(chunks))
	}
}

func TestParser_Parse_SingleFileOneChunk(t *testing.T) {
	diffOutput := `diff --git a/README.md b/README.md
index abc123..def456 100644
--- a/README.md
+++ b/README.md
@@ -1,3 +1,4 @@
 # Title
+New line added
 Some content
 More content`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "README.md" {
		t.Errorf("expected file 'README.md', got '%s'", h.File)
	}
	if h.OldStart != 1 {
		t.Errorf("expected OldStart 1, got %d", h.OldStart)
	}
	if h.OldCount != 3 {
		t.Errorf("expected OldCount 3, got %d", h.OldCount)
	}
	if h.NewStart != 1 {
		t.Errorf("expected NewStart 1, got %d", h.NewStart)
	}
	if h.NewCount != 4 {
		t.Errorf("expected NewCount 4, got %d", h.NewCount)
	}
	if !strings.Contains(h.Content, "+New line added") {
		t.Errorf("expected content to contain '+New line added', got '%s'", h.Content)
	}
}

func TestParser_Parse_SingleFileMultipleChunks(t *testing.T) {
	diffOutput := `diff --git a/main.go b/main.go
index abc123..def456 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"

 func main() {
@@ -10,5 +11,6 @@
 func helper() {
     // existing code
+    // new comment
     return
 }`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}

	// First chunk
	if chunks[0].OldStart != 1 {
		t.Errorf("chunk 0: expected OldStart 1, got %d", chunks[0].OldStart)
	}

	// Second chunk
	if chunks[1].OldStart != 10 {
		t.Errorf("chunk 1: expected OldStart 10, got %d", chunks[1].OldStart)
	}
}

func TestParser_Parse_MultipleFiles(t *testing.T) {
	diffOutput := `diff --git a/file1.go b/file1.go
index abc123..def456 100644
--- a/file1.go
+++ b/file1.go
@@ -1,2 +1,3 @@
 package file1
+// comment

diff --git a/file2.go b/file2.go
index 111222..333444 100644
--- a/file2.go
+++ b/file2.go
@@ -5,3 +5,4 @@
 func foo() {
+    bar()
 }`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (one per file), got %d", len(chunks))
	}

	if chunks[0].File != "file1.go" {
		t.Errorf("chunk 0: expected file 'file1.go', got '%s'", chunks[0].File)
	}
	if chunks[1].File != "file2.go" {
		t.Errorf("chunk 1: expected file 'file2.go', got '%s'", chunks[1].File)
	}
}

func TestParser_Parse_NewFile(t *testing.T) {
	diffOutput := `diff --git a/newfile.txt b/newfile.txt
new file mode 100644
index 0000000..abc1234
--- /dev/null
+++ b/newfile.txt
@@ -0,0 +1,3 @@
+line 1
+line 2
+line 3`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "newfile.txt" {
		t.Errorf("expected file 'newfile.txt', got '%s'", h.File)
	}
	if h.OldStart != 0 {
		t.Errorf("expected OldStart 0, got %d", h.OldStart)
	}
	if h.OldCount != 0 {
		t.Errorf("expected OldCount 0, got %d", h.OldCount)
	}
	if h.NewStart != 1 {
		t.Errorf("expected NewStart 1, got %d", h.NewStart)
	}
	if h.NewCount != 3 {
		t.Errorf("expected NewCount 3, got %d", h.NewCount)
	}
}

func TestParser_Parse_DeletedLines(t *testing.T) {
	diffOutput := `diff --git a/config.yaml b/config.yaml
index abc123..def456 100644
--- a/config.yaml
+++ b/config.yaml
@@ -2,4 +2,2 @@
 setting1: true
-setting2: false
-setting3: 42
 setting4: "value"`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	h := chunks[0]
	if !strings.Contains(h.Content, "-setting2: false") {
		t.Errorf("expected deleted line in content")
	}
}

func TestParser_Parse_NoNewlineAtEnd(t *testing.T) {
	diffOutput := `diff --git a/file.txt b/file.txt
index abc123..def456 100644
--- a/file.txt
+++ b/file.txt
@@ -1,2 +1,2 @@
 line1
-line2
+line2 modified
\ No newline at end of file`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	// The "\ No newline at end of file" marker should be included
	if !strings.Contains(chunks[0].Content, "No newline") {
		t.Errorf("expected 'No newline' marker in content")
	}
}

func TestParser_Parse_BinaryFile(t *testing.T) {
	// Binary files have no @@ chunk header, so they should produce no chunks.
	// This is intentional - binary diffs are not useful for code review.
	diffOutput := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	// Binary files should NOT produce any chunks
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for binary file, got %d", len(chunks))
	}
}

func TestParser_Parse_MixedBinaryAndText(t *testing.T) {
	// When a diff contains both binary and text files, only text files
	// should produce chunks.
	diffOutput := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ
diff --git a/readme.txt b/readme.txt
index abc123..def456 100644
--- a/readme.txt
+++ b/readme.txt
@@ -1,2 +1,3 @@
 Hello
+World
 Goodbye`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	// Should only have 1 chunk (from readme.txt, not from image.png)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for mixed binary/text, got %d", len(chunks))
	}

	if chunks[0].File != "readme.txt" {
		t.Errorf("expected file 'readme.txt', got '%s'", chunks[0].File)
	}
}

// TestParser_Parse_RenamedFile verifies that renamed files are parsed correctly.
// Git shows renames with similarity index and rename from/to headers.
func TestParser_Parse_RenamedFile(t *testing.T) {
	diffOutput := `diff --git a/old_name.go b/new_name.go
similarity index 95%
rename from old_name.go
rename to new_name.go
index abc123..def456 100644
--- a/old_name.go
+++ b/new_name.go
@@ -1,3 +1,4 @@
 package main
+// renamed file
 func main() {}`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for renamed file, got %d", len(chunks))
	}

	// The parser should use the "b/" path (new name)
	if chunks[0].File != "new_name.go" {
		t.Errorf("expected file 'new_name.go', got '%s'", chunks[0].File)
	}
}

// TestParser_Parse_RenamedFileNoChanges verifies handling of pure renames (no content changes).
// In this case, there's no @@ chunk header, so no chunks should be produced.
func TestParser_Parse_RenamedFileNoChanges(t *testing.T) {
	diffOutput := `diff --git a/old.txt b/new.txt
similarity index 100%
rename from old.txt
rename to new.txt`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	// Pure rename with no content changes has no chunks
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for pure rename, got %d", len(chunks))
	}
}

// TestParser_Parse_ModeChange verifies handling of permission-only changes.
// Mode changes without content changes have no @@ chunk header.
func TestParser_Parse_ModeChange(t *testing.T) {
	diffOutput := `diff --git a/script.sh b/script.sh
old mode 100644
new mode 100755`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	// Pure mode change has no chunks (no @@ header)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for mode-only change, got %d", len(chunks))
	}
}

// TestParser_Parse_ModeChangeWithContent verifies mode change combined with content change.
func TestParser_Parse_ModeChangeWithContent(t *testing.T) {
	diffOutput := `diff --git a/script.sh b/script.sh
old mode 100644
new mode 100755
index abc123..def456
--- a/script.sh
+++ b/script.sh
@@ -1,2 +1,3 @@
 #!/bin/bash
+echo "Hello"
 exit 0`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for mode+content change, got %d", len(chunks))
	}

	if chunks[0].File != "script.sh" {
		t.Errorf("expected file 'script.sh', got '%s'", chunks[0].File)
	}
}

// TestParser_Parse_DeletedFile verifies handling of deleted files.
func TestParser_Parse_DeletedFile(t *testing.T) {
	diffOutput := `diff --git a/removed.txt b/removed.txt
deleted file mode 100644
index abc1234..0000000
--- a/removed.txt
+++ /dev/null
@@ -1,3 +0,0 @@
-line 1
-line 2
-line 3`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for deleted file, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "removed.txt" {
		t.Errorf("expected file 'removed.txt', got '%s'", h.File)
	}
	if h.NewCount != 0 {
		t.Errorf("expected NewCount 0 for deleted file, got %d", h.NewCount)
	}
}

// TestParser_Parse_CopiedFile verifies handling of copied files.
func TestParser_Parse_CopiedFile(t *testing.T) {
	diffOutput := `diff --git a/original.go b/copy.go
similarity index 90%
copy from original.go
copy to copy.go
index abc123..def456 100644
--- a/original.go
+++ b/copy.go
@@ -1,3 +1,4 @@
 package main
+// this is a copy
 func main() {}`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for copied file, got %d", len(chunks))
	}

	// Parser should use b/ path (the copy destination)
	if chunks[0].File != "copy.go" {
		t.Errorf("expected file 'copy.go', got '%s'", chunks[0].File)
	}
}

// TestParser_Parse_EmptyChunk verifies handling of diffs with headers but empty chunk content.
func TestParser_Parse_EmptyChunk(t *testing.T) {
	// This is an edge case - a chunk header with no following content lines
	diffOutput := `diff --git a/test.txt b/test.txt
index abc123..def456 100644
--- a/test.txt
+++ b/test.txt
@@ -1,0 +1,0 @@`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	// Should produce a chunk even if it's empty (the header itself is content)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for empty content, got %d", len(chunks))
	}
}

// TestParseBinaryFiles verifies detection of binary files in diff output.
func TestParseBinaryFiles(t *testing.T) {
	diffOutput := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ
diff --git a/doc.pdf b/doc.pdf
index abc123..def456 100644
Binary files a/doc.pdf and b/doc.pdf differ
diff --git a/readme.txt b/readme.txt
index 111111..222222 100644
--- a/readme.txt
+++ b/readme.txt
@@ -1 +1,2 @@
 Hello
+World`

	binaryFiles := ParseBinaryFiles(diffOutput)

	if len(binaryFiles) != 2 {
		t.Fatalf("expected 2 binary files, got %d", len(binaryFiles))
	}

	if !binaryFiles["image.png"] {
		t.Error("expected image.png to be detected as binary")
	}

	if !binaryFiles["doc.pdf"] {
		t.Error("expected doc.pdf to be detected as binary")
	}

	if binaryFiles["readme.txt"] {
		t.Error("readme.txt should NOT be detected as binary")
	}
}

// TestExtractFileDiffSection verifies extraction of diff section for a specific file.
func TestExtractFileDiffSection(t *testing.T) {
	diffOutput := `diff --git a/image.png b/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/image.png differ
diff --git a/readme.txt b/readme.txt
index 111111..222222 100644
--- a/readme.txt
+++ b/readme.txt
@@ -1 +1,2 @@
 Hello
+World`

	// Extract binary file section
	section := ExtractFileDiffSection(diffOutput, "image.png")
	if !strings.Contains(section, "diff --git a/image.png b/image.png") {
		t.Error("expected section to contain diff header")
	}
	if !strings.Contains(section, "index 0000000..abc1234") {
		t.Error("expected section to contain index line with blob hashes")
	}
	if !strings.Contains(section, "Binary files") {
		t.Error("expected section to contain Binary files line")
	}
	if strings.Contains(section, "readme.txt") {
		t.Error("section should not contain other file's content")
	}

	// Extract text file section
	section = ExtractFileDiffSection(diffOutput, "readme.txt")
	if !strings.Contains(section, "diff --git a/readme.txt") {
		t.Error("expected section to contain diff header for readme.txt")
	}
	if !strings.Contains(section, "+World") {
		t.Error("expected section to contain added line")
	}
	if strings.Contains(section, "image.png") {
		t.Error("section should not contain other file's content")
	}

	// Extract non-existent file
	section = ExtractFileDiffSection(diffOutput, "nonexistent.txt")
	if section != "" {
		t.Errorf("expected empty section for nonexistent file, got: %s", section)
	}
}

// TestExtractFileDiffSection_BinaryFileHashChanges verifies that blob hash changes
// are captured in extracted section for binary files.
func TestExtractFileDiffSection_BinaryFileHashChanges(t *testing.T) {
	// First version of binary file
	diffV1 := `diff --git a/image.png b/image.png
index 0000000..abc1234 100644
Binary files /dev/null and b/image.png differ`

	// Second version (blob hash changed)
	diffV2 := `diff --git a/image.png b/image.png
index abc1234..def5678 100644
Binary files a/image.png and b/image.png differ`

	sectionV1 := ExtractFileDiffSection(diffV1, "image.png")
	sectionV2 := ExtractFileDiffSection(diffV2, "image.png")

	// Sections should be different because index line changed
	if sectionV1 == sectionV2 {
		t.Error("expected different sections for different binary file versions")
	}

	// Each should contain its respective index line
	if !strings.Contains(sectionV1, "0000000..abc1234") {
		t.Error("v1 section should contain v1 index")
	}
	if !strings.Contains(sectionV2, "abc1234..def5678") {
		t.Error("v2 section should contain v2 index")
	}
}

// TestCalculateDiffStats verifies diff statistics calculation.
func TestCalculateDiffStats(t *testing.T) {
	diff := `@@ -1,3 +1,5 @@
 line1
+added1
+added2
 line2
-deleted
 line3`

	stats := CalculateDiffStats(diff)

	if stats.ByteSize != len(diff) {
		t.Errorf("expected ByteSize %d, got %d", len(diff), stats.ByteSize)
	}

	// Count lines (split by \n)
	expectedLines := 8 // 7 content lines + 1 for potential trailing empty
	if stats.LineCount < 7 || stats.LineCount > expectedLines {
		t.Errorf("expected LineCount ~7-8, got %d", stats.LineCount)
	}

	if stats.AddedLines != 2 {
		t.Errorf("expected 2 added lines, got %d", stats.AddedLines)
	}

	if stats.DeletedLines != 1 {
		t.Errorf("expected 1 deleted line, got %d", stats.DeletedLines)
	}
}

// TestCalculateDiffStats_EmptyDiff tests stats for empty diff.
func TestCalculateDiffStats_EmptyDiff(t *testing.T) {
	stats := CalculateDiffStats("")

	if stats.ByteSize != 0 {
		t.Errorf("expected ByteSize 0, got %d", stats.ByteSize)
	}
	if stats.AddedLines != 0 {
		t.Errorf("expected 0 added lines, got %d", stats.AddedLines)
	}
	if stats.DeletedLines != 0 {
		t.Errorf("expected 0 deleted lines, got %d", stats.DeletedLines)
	}
}

// TestCalculateDiffStats_OnlyAdditions tests stats for additions-only diff.
func TestCalculateDiffStats_OnlyAdditions(t *testing.T) {
	diff := `@@ -0,0 +1,3 @@
+line1
+line2
+line3`

	stats := CalculateDiffStats(diff)

	if stats.AddedLines != 3 {
		t.Errorf("expected 3 added lines, got %d", stats.AddedLines)
	}
	if stats.DeletedLines != 0 {
		t.Errorf("expected 0 deleted lines, got %d", stats.DeletedLines)
	}
}

// TestCalculateDiffStats_OnlyDeletions tests stats for deletions-only diff.
func TestCalculateDiffStats_OnlyDeletions(t *testing.T) {
	diff := `@@ -1,3 +0,0 @@
-line1
-line2
-line3`

	stats := CalculateDiffStats(diff)

	if stats.AddedLines != 0 {
		t.Errorf("expected 0 added lines, got %d", stats.AddedLines)
	}
	if stats.DeletedLines != 3 {
		t.Errorf("expected 3 deleted lines, got %d", stats.DeletedLines)
	}
}

// TestIsLargeDiff_ByBytes tests large diff detection by byte size.
func TestIsLargeDiff_ByBytes(t *testing.T) {
	// Small diff - not large
	smallStats := &DiffStats{
		ByteSize:  100,
		LineCount: 10,
	}
	if IsLargeDiff(smallStats) {
		t.Error("small diff should not be marked as large")
	}

	// Large by bytes (>1MB)
	largeByBytes := &DiffStats{
		ByteSize:  2 * 1024 * 1024, // 2MB
		LineCount: 100,
	}
	if !IsLargeDiff(largeByBytes) {
		t.Error("2MB diff should be marked as large")
	}

	// Exactly at threshold - NOT large (threshold is exclusive)
	atThreshold := &DiffStats{
		ByteSize:  LargeDiffByteThreshold,
		LineCount: 100,
	}
	if IsLargeDiff(atThreshold) {
		t.Error("diff at exact byte threshold should NOT be marked as large (threshold is exclusive)")
	}

	// Just over threshold - IS large
	overThreshold := &DiffStats{
		ByteSize:  LargeDiffByteThreshold + 1,
		LineCount: 100,
	}
	if !IsLargeDiff(overThreshold) {
		t.Error("diff over byte threshold should be marked as large")
	}
}

// TestIsLargeDiff_ByLines tests large diff detection by line count.
func TestIsLargeDiff_ByLines(t *testing.T) {
	// Small diff - not large
	smallStats := &DiffStats{
		ByteSize:  100,
		LineCount: 10,
	}
	if IsLargeDiff(smallStats) {
		t.Error("small diff should not be marked as large")
	}

	// Large by lines (>2000)
	largeByLines := &DiffStats{
		ByteSize:  1000,
		LineCount: 3000,
	}
	if !IsLargeDiff(largeByLines) {
		t.Error("3000-line diff should be marked as large")
	}

	// Exactly at threshold - NOT large (threshold is exclusive)
	atThreshold := &DiffStats{
		ByteSize:  100,
		LineCount: LargeDiffLineThreshold,
	}
	if IsLargeDiff(atThreshold) {
		t.Error("diff at exact line threshold should NOT be marked as large (threshold is exclusive)")
	}

	// Just over threshold - IS large
	overThreshold := &DiffStats{
		ByteSize:  100,
		LineCount: LargeDiffLineThreshold + 1,
	}
	if !IsLargeDiff(overThreshold) {
		t.Error("diff over line threshold should be marked as large")
	}
}

// TestIsLargeDiff_NilStats tests that nil stats don't cause panic.
func TestIsLargeDiff_NilStats(t *testing.T) {
	// Should not panic
	result := IsLargeDiff(nil)
	if result {
		t.Error("nil stats should return false")
	}
}

// -----------------------------------------------------------------------------
// File Path Edge Cases (Unit 5.4)
// -----------------------------------------------------------------------------

// TestParser_Parse_FilePathWithSpaces verifies file names with spaces are parsed correctly.
func TestParser_Parse_FilePathWithSpaces(t *testing.T) {
	diffOutput := `diff --git a/path with spaces/my file.txt b/path with spaces/my file.txt
index abc123..def456 100644
--- a/path with spaces/my file.txt
+++ b/path with spaces/my file.txt
@@ -1,2 +1,3 @@
 Hello
+World
 Goodbye`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	if chunks[0].File != "path with spaces/my file.txt" {
		t.Errorf("expected file 'path with spaces/my file.txt', got '%s'", chunks[0].File)
	}
}

// TestParser_Parse_FilePathWithBInName documents a known limitation:
// Paths containing " b/" are ambiguous in the git diff header format.
// The parser uses the header regex which is greedy, so it may misparse these paths.
// Git itself uses the +++ line as the authoritative source.
// TODO: Consider parsing +++ line for edge cases like this.
func TestParser_Parse_FilePathWithBInName(t *testing.T) {
	diffOutput := `diff --git a/foo b/bar.txt b/foo b/bar.txt
index abc123..def456 100644
--- a/foo b/bar.txt
+++ b/foo b/bar.txt
@@ -1 +1 @@
-old
+new`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	// Known limitation: the parser extracts "bar.txt" instead of "foo b/bar.txt"
	// because the regex greedily matches to the last " b/" delimiter.
	// This is documented here to prevent regression if the behavior changes.
	if chunks[0].File != "bar.txt" {
		t.Errorf("expected file 'bar.txt' (known limitation), got '%s'", chunks[0].File)
	}
}

// -----------------------------------------------------------------------------
// Submodule Edge Cases (Unit 7.9)
// -----------------------------------------------------------------------------

// TestParser_Parse_SubmoduleChange verifies handling of submodule pointer updates.
// Submodule entries have mode 160000 and show "Subproject commit" lines.
// Unlike binary files, submodule changes DO have @@ chunk headers and produce chunks.
func TestParser_Parse_SubmoduleChange(t *testing.T) {
	diffOutput := `diff --git a/vendor/lib b/vendor/lib
index abc1234..def5678 160000
--- a/vendor/lib
+++ b/vendor/lib
@@ -1 +1 @@
-Subproject commit abc1234567890abcdef1234567890abcdef123456
+Subproject commit def5678901234567890abcdef1234567890abcdef`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for submodule change, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "vendor/lib" {
		t.Errorf("expected file 'vendor/lib', got '%s'", h.File)
	}

	// Verify content contains the subproject commit lines
	if !strings.Contains(h.Content, "Subproject commit") {
		t.Errorf("expected content to contain 'Subproject commit', got: %s", h.Content)
	}
}

// TestParser_Parse_SubmoduleAdd verifies handling of new submodule additions.
// New submodules have mode 160000 and show the initial commit.
func TestParser_Parse_SubmoduleAdd(t *testing.T) {
	diffOutput := `diff --git a/vendor/newlib b/vendor/newlib
new file mode 160000
index 0000000..abc1234
--- /dev/null
+++ b/vendor/newlib
@@ -0,0 +1 @@
+Subproject commit abc1234567890abcdef1234567890abcdef123456`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for new submodule, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "vendor/newlib" {
		t.Errorf("expected file 'vendor/newlib', got '%s'", h.File)
	}
	if h.OldCount != 0 {
		t.Errorf("expected OldCount 0 for new submodule, got %d", h.OldCount)
	}
	if h.NewCount != 1 {
		t.Errorf("expected NewCount 1 for new submodule, got %d", h.NewCount)
	}
}

// TestParser_Parse_SubmoduleRemove verifies handling of submodule removals.
func TestParser_Parse_SubmoduleRemove(t *testing.T) {
	diffOutput := `diff --git a/vendor/oldlib b/vendor/oldlib
deleted file mode 160000
index abc1234..0000000
--- a/vendor/oldlib
+++ /dev/null
@@ -1 +0,0 @@
-Subproject commit abc1234567890abcdef1234567890abcdef123456`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for deleted submodule, got %d", len(chunks))
	}

	h := chunks[0]
	if h.File != "vendor/oldlib" {
		t.Errorf("expected file 'vendor/oldlib', got '%s'", h.File)
	}
	if h.NewCount != 0 {
		t.Errorf("expected NewCount 0 for deleted submodule, got %d", h.NewCount)
	}
}

// TestParser_Parse_MixedSubmoduleAndRegularFiles verifies that regular file
// chunks are correctly parsed when mixed with submodule changes.
func TestParser_Parse_MixedSubmoduleAndRegularFiles(t *testing.T) {
	diffOutput := `diff --git a/README.md b/README.md
index 111111..222222 100644
--- a/README.md
+++ b/README.md
@@ -1,2 +1,3 @@
 # Project
+Updated readme
 Content
diff --git a/vendor/lib b/vendor/lib
index abc1234..def5678 160000
--- a/vendor/lib
+++ b/vendor/lib
@@ -1 +1 @@
-Subproject commit abc1234567890abcdef1234567890abcdef123456
+Subproject commit def5678901234567890abcdef1234567890abcdef
diff --git a/main.go b/main.go
index 333333..444444 100644
--- a/main.go
+++ b/main.go
@@ -5,3 +5,4 @@
 func main() {
+    fmt.Println("hello")
 }`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (readme, submodule, main.go), got %d", len(chunks))
	}

	// Verify all files are correctly identified
	files := map[string]bool{
		"README.md":  false,
		"vendor/lib": false,
		"main.go":    false,
	}
	for _, h := range chunks {
		files[h.File] = true
	}
	for file, found := range files {
		if !found {
			t.Errorf("expected to find chunk for %s", file)
		}
	}
}

// TestParser_Parse_SubmoduleDirtyState documents handling of submodule dirty state.
// When a submodule has uncommitted changes, git may show "-dirty" suffix.
// This is typically shown in git status/diff output but the parser handles it.
func TestParser_Parse_SubmoduleDirtyState(t *testing.T) {
	// Note: The "-dirty" suffix appears in some git diff configurations
	// The parser treats this like any other submodule content
	diffOutput := `diff --git a/vendor/lib b/vendor/lib
index abc1234..def5678 160000
--- a/vendor/lib
+++ b/vendor/lib
@@ -1 +1 @@
-Subproject commit abc1234567890abcdef1234567890abcdef123456
+Subproject commit def5678901234567890abcdef1234567890abcdef-dirty`

	p := NewParser()
	chunks := p.Parse(diffOutput)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for dirty submodule, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0].Content, "-dirty") {
		t.Errorf("expected content to contain '-dirty' suffix")
	}
}

// TestExtractFileDiffSection_WithSpaces verifies binary file section extraction with spaces.
func TestExtractFileDiffSection_WithSpaces(t *testing.T) {
	fullDiff := `diff --git a/text.txt b/text.txt
index 111111..222222 100644
--- a/text.txt
+++ b/text.txt
@@ -1 +1 @@
-old
+new
diff --git a/path with spaces/image.png b/path with spaces/image.png
new file mode 100644
index 0000000..abc1234
Binary files /dev/null and b/path with spaces/image.png differ
diff --git a/another.txt b/another.txt
index 333333..444444 100644
--- a/another.txt
+++ b/another.txt
@@ -1 +1 @@
-foo
+bar`

	section := ExtractFileDiffSection(fullDiff, "path with spaces/image.png")

	if section == "" {
		t.Fatal("expected to find diff section for file with spaces")
	}

	// Should contain the index line (for hash uniqueness)
	if !strings.Contains(section, "index 0000000..abc1234") {
		t.Error("section should contain the index line")
	}

	// Should contain the Binary files line
	if !strings.Contains(section, "Binary files") {
		t.Error("section should contain Binary files marker")
	}

	// Should NOT contain content from other files
	if strings.Contains(section, "text.txt") || strings.Contains(section, "another.txt") {
		t.Error("section should not contain other files")
	}
}
