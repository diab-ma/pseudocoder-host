package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apperrors "github.com/pseudocoder/host/internal/errors"
)

// makeRepo creates a temp directory with test files and returns the path.
func makeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create directory structure.
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)

	// Create files.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "guide.txt"), []byte("Guide content\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]\n"), 0o644)

	return dir
}

func TestFileOpsList_RootDir(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	entries, canonPath, err := ops.List("")
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if canonPath != "." {
		t.Errorf("expected canon path '.', got %q", canonPath)
	}

	// Expect dirs first (sorted), then files.
	// Dirs: .git, docs, src. Files: README.md
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(entries), entries)
	}

	// First three should be dirs.
	for i := 0; i < 3; i++ {
		if entries[i].Kind != "dir" {
			t.Errorf("entries[%d] expected dir, got %s (%s)", i, entries[i].Kind, entries[i].Name)
		}
	}
	if entries[0].Name != ".git" {
		t.Errorf("expected .git first dir, got %s", entries[0].Name)
	}
	if entries[3].Kind != "file" || entries[3].Name != "README.md" {
		t.Errorf("expected README.md file last, got %s (%s)", entries[3].Name, entries[3].Kind)
	}
	if entries[3].ModifiedAt == nil {
		t.Fatal("expected README.md modified_at to be present")
	}
	if *entries[3].ModifiedAt < 1000000000000 {
		t.Errorf("expected modified_at unix millis, got %d", *entries[3].ModifiedAt)
	}
}

func TestFileOpsList_NestedDir(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	entries, canonPath, err := ops.List("src")
	if err != nil {
		t.Fatalf("List src: %v", err)
	}
	if canonPath != "src" {
		t.Errorf("expected canon path 'src', got %q", canonPath)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "main.go" || entries[0].Path != "src/main.go" {
		t.Errorf("unexpected entry: %+v", entries[0])
	}
}

func TestFileOpsList_EmptyPath(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	entries, canonPath, err := ops.List("")
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if canonPath != "." {
		t.Errorf("expected '.', got %q", canonPath)
	}
	if len(entries) == 0 {
		t.Fatal("expected non-empty entries")
	}
}

func TestFileOpsList_DotPath(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	entries, canonPath, err := ops.List(".")
	if err != nil {
		t.Fatalf("List dot: %v", err)
	}
	if canonPath != "." {
		t.Errorf("expected '.', got %q", canonPath)
	}
	if len(entries) == 0 {
		t.Fatal("expected non-empty entries")
	}
}

func TestFileOpsList_AbsolutePathRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.List("/etc")
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsList_TraversalRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.List("../../")
	if err == nil {
		t.Fatal("expected error for traversal")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsList_NotFound(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.List("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsList_FilePathRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.List("README.md")
	if err == nil {
		t.Fatal("expected error for file path")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsList_GitDir(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	entries, _, err := ops.List(".git")
	if err != nil {
		t.Fatalf("List .git: %v", err)
	}
	// Should contain HEAD and config at minimum.
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["HEAD"] || !names["config"] {
		t.Errorf("expected HEAD and config in .git, got %v", names)
	}
}

func TestFileOpsList_SymlinkEscape(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	// Create symlink pointing outside repo.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "escape"))

	_, _, err := ops.List("escape")
	if err == nil {
		t.Fatal("expected error for symlink escape")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsList_SymlinkInRepo(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	// Create symlink within repo.
	os.Symlink(filepath.Join(dir, "src"), filepath.Join(dir, "link_to_src"))

	entries, _, err := ops.List("link_to_src")
	if err != nil {
		t.Fatalf("List symlink in repo: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "main.go" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestFileOpsRead_TextFile(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	data, canonPath, err := ops.Read("README.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if canonPath != "README.md" {
		t.Errorf("expected canon path 'README.md', got %q", canonPath)
	}
	if data.Content != "# Hello\n" {
		t.Errorf("unexpected content: %q", data.Content)
	}
	if data.Encoding != "utf-8" {
		t.Errorf("expected utf-8, got %q", data.Encoding)
	}
	if data.LineEnding != "lf" {
		t.Errorf("expected lf, got %q", data.LineEnding)
	}
	if !strings.HasPrefix(data.Version, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", data.Version)
	}
	if data.ReadOnlyReason != "" {
		t.Errorf("expected empty read_only_reason, got %q", data.ReadOnlyReason)
	}
}

func TestFileOpsRead_CRLFFile(t *testing.T) {
	dir := makeRepo(t)
	os.WriteFile(filepath.Join(dir, "crlf.txt"), []byte("line1\r\nline2\r\n"), 0o644)
	ops := NewFileOperations(dir, 1048576)

	data, _, err := ops.Read("crlf.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if data.LineEnding != "crlf" {
		t.Errorf("expected crlf, got %q", data.LineEnding)
	}
}

func TestFileOpsRead_MixedLineEndings(t *testing.T) {
	dir := makeRepo(t)
	os.WriteFile(filepath.Join(dir, "mixed.txt"), []byte("line1\r\nline2\nline3\r\n"), 0o644)
	ops := NewFileOperations(dir, 1048576)

	data, _, err := ops.Read("mixed.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if data.LineEnding != "lf" {
		t.Errorf("expected lf for mixed endings, got %q", data.LineEnding)
	}
}

func TestFileOpsRead_BinaryFile(t *testing.T) {
	dir := makeRepo(t)
	os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x01, 0x02}, 0o644)
	ops := NewFileOperations(dir, 1048576)

	data, _, err := ops.Read("bin.dat")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if data.ReadOnlyReason != "binary" {
		t.Errorf("expected binary, got %q", data.ReadOnlyReason)
	}
	if data.Content != "" {
		t.Errorf("expected empty content for binary, got %q", data.Content)
	}
	if !strings.HasPrefix(data.Version, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", data.Version)
	}
}

func TestFileOpsRead_TooLargeFile(t *testing.T) {
	dir := makeRepo(t)
	// Create a file just over the size cap (1KB for this test).
	ops := NewFileOperations(dir, 1024)
	bigContent := make([]byte, 2048)
	for i := range bigContent {
		bigContent[i] = 'A'
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), bigContent, 0o644)

	data, _, err := ops.Read("big.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if data.ReadOnlyReason != "too_large" {
		t.Errorf("expected too_large, got %q", data.ReadOnlyReason)
	}
	if data.Content != "" {
		t.Errorf("expected empty content for too_large, got len=%d", len(data.Content))
	}
}

func TestFileOpsRead_TooLargeFileVersionUsesContentHash(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1024)

	contentA := bytes.Repeat([]byte("A"), 2048)
	contentB := bytes.Repeat([]byte("B"), 2048)

	os.WriteFile(filepath.Join(dir, "big-a.txt"), contentA, 0o644)
	os.WriteFile(filepath.Join(dir, "big-b.txt"), contentB, 0o644)

	dataA1, _, err := ops.Read("big-a.txt")
	if err != nil {
		t.Fatalf("Read big-a first: %v", err)
	}
	dataA2, _, err := ops.Read("big-a.txt")
	if err != nil {
		t.Fatalf("Read big-a second: %v", err)
	}
	dataB, _, err := ops.Read("big-b.txt")
	if err != nil {
		t.Fatalf("Read big-b: %v", err)
	}

	if dataA1.ReadOnlyReason != "too_large" || dataB.ReadOnlyReason != "too_large" {
		t.Fatalf("expected too_large read-only reason, got %q and %q", dataA1.ReadOnlyReason, dataB.ReadOnlyReason)
	}
	if dataA1.Version != dataA2.Version {
		t.Fatalf("expected stable version for same file, got %q and %q", dataA1.Version, dataA2.Version)
	}
	if dataA1.Version == dataB.Version {
		t.Fatalf("expected different versions for different same-size contents, both %q", dataA1.Version)
	}
}

func TestFileOpsRead_DirectoryFails(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Read("src")
	if err == nil {
		t.Fatal("expected error for directory read")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsRead_NotFound(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Read("nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsRead_SymlinkEscape(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "escape_file"))

	_, _, err := ops.Read("escape_file")
	if err == nil {
		t.Fatal("expected error for symlink escape")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsRead_GitFile(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	data, _, err := ops.Read(".git/HEAD")
	if err != nil {
		t.Fatalf("Read .git/HEAD: %v", err)
	}
	if data.Content != "ref: refs/heads/main\n" {
		t.Errorf("unexpected content: %q", data.Content)
	}
}

func TestFileOpsRead_TraversalNormalized(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	data, canonPath, err := ops.Read("src/../.git/config")
	if err != nil {
		t.Fatalf("Read normalized: %v", err)
	}
	if canonPath != ".git/config" {
		t.Errorf("expected '.git/config', got %q", canonPath)
	}
	if data.Content != "[core]\n" {
		t.Errorf("unexpected content: %q", data.Content)
	}
}

// --- Write tests ---

func TestFileOpsWrite_Success(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	// Read to get current version.
	readData, _, err := ops.Read("README.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	canonPath, newVersion, err := ops.Write("README.md", "# Updated\n", readData.Version)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if canonPath != "README.md" {
		t.Errorf("expected canon path 'README.md', got %q", canonPath)
	}
	if !strings.HasPrefix(newVersion, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", newVersion)
	}
	if newVersion == readData.Version {
		t.Error("expected different version after write")
	}

	// Verify file was actually written.
	content, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "# Updated\n" {
		t.Errorf("unexpected file content: %q", string(content))
	}
}

func TestFileOpsWrite_StaleBaseVersion(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write("README.md", "new content", "sha256:stale")
	if err == nil {
		t.Fatal("expected error for stale base version")
	}
	if !apperrors.IsCode(err, apperrors.CodeConflictDetected) {
		t.Errorf("expected conflict.detected, got %s", apperrors.GetCode(err))
	}

	// Verify file was NOT changed.
	content, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(content) != "# Hello\n" {
		t.Errorf("file should be unchanged, got %q", string(content))
	}
}

func TestFileOpsWrite_PreserveCRLF(t *testing.T) {
	dir := makeRepo(t)
	os.WriteFile(filepath.Join(dir, "crlf.txt"), []byte("line1\r\nline2\r\n"), 0o644)
	ops := NewFileOperations(dir, 1048576)

	readData, _, err := ops.Read("crlf.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Write with LF content — should be converted to CRLF.
	_, _, err = ops.Write("crlf.txt", "new1\nnew2\n", readData.Version)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(dir, "crlf.txt"))
	if string(content) != "new1\r\nnew2\r\n" {
		t.Errorf("expected CRLF preserved, got %q", string(content))
	}
}

func TestFileOpsWrite_PreserveLF(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	readData, _, err := ops.Read("README.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Write with CRLF content — should be normalized to LF.
	_, _, err = ops.Write("README.md", "updated\r\n", readData.Version)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(content) != "updated\n" {
		t.Errorf("expected LF preserved, got %q", string(content))
	}
}

func TestFileOpsWrite_GitPathBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write(".git/HEAD", "bad", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for .git path write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_GitSymlinkAliasBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	if err := os.Symlink(".git", filepath.Join(dir, "gitlink")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	readData, _, err := ops.Read("gitlink/HEAD")
	if err != nil {
		t.Fatalf("Read through git symlink alias: %v", err)
	}

	_, _, err = ops.Write("gitlink/HEAD", "mutated\n", readData.Version)
	if err == nil {
		t.Fatal("expected error for .git symlink alias write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_DotPathRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write(".", "content", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for dot path write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_DirectoryRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write("src", "content", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for directory write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_BinaryFileRejected(t *testing.T) {
	dir := makeRepo(t)
	binContent := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x01}
	os.WriteFile(filepath.Join(dir, "image.png"), binContent, 0o644)
	ops := NewFileOperations(dir, 1048576)

	readData, _, _ := ops.Read("image.png")

	_, _, err := ops.Write("image.png", "text", readData.Version)
	if err == nil {
		t.Fatal("expected error for binary file write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_TooLargeRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1024)
	bigContent := bytes.Repeat([]byte("A"), 2048)
	os.WriteFile(filepath.Join(dir, "big.txt"), bigContent, 0o644)

	_, _, err := ops.Write("big.txt", "new", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for too-large file write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_AbsolutePathRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write("/etc/passwd", "content", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for absolute path write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_TraversalRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Write("../../escape.txt", "content", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for traversal write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_SymlinkEscapeRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "target.txt"), []byte("original"), 0o644)
	os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "escape_link"))

	_, _, err := ops.Write("escape_link", "hacked", "sha256:fake")
	if err == nil {
		t.Fatal("expected error for symlink escape write")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsWrite_FilesystemFailure(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)
	beforeContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("Read README before write failure: %v", err)
	}

	readData, _, err := ops.Read("README.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	readmeDir := filepath.Join(dir)
	if err := os.Chmod(readmeDir, 0o555); err != nil {
		t.Fatalf("Chmod read-only dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(readmeDir, 0o755)
	})

	_, _, err = ops.Write("README.md", "cannot write\n", readData.Version)
	if err == nil {
		t.Fatal("expected filesystem write error")
	}
	if !apperrors.IsCode(err, apperrors.CodeInternal) {
		t.Errorf("expected internal, got %s", apperrors.GetCode(err))
	}
	afterContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("Read README after write failure: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Fatal("README.md changed despite write failure")
	}
}

// --- Create tests ---

func TestFileOpsCreate_Success(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	canonPath, version, err := ops.Create("src/new.go", "package main\n")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if canonPath != "src/new.go" {
		t.Errorf("expected canon path 'src/new.go', got %q", canonPath)
	}
	if !strings.HasPrefix(version, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", version)
	}

	content, err := os.ReadFile(filepath.Join(dir, "src", "new.go"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "package main\n" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestFileOpsCreate_AlreadyExists(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	beforeContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("Read README before create: %v", err)
	}

	_, _, err = ops.Create("README.md", "content")
	if err == nil {
		t.Fatal("expected error for already-existing file")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageAlreadyExists) {
		t.Errorf("expected storage.already_exists, got %s", apperrors.GetCode(err))
	}

	afterContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("Read README after create: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Fatal("README.md changed despite create already-exists failure")
	}
}

func TestFileOpsCreate_ParentMissing(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Create("nonexistent/new.txt", "content")
	if err == nil {
		t.Fatal("expected error for missing parent directory")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsCreate_EmptyContent(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	canonPath, version, err := ops.Create("empty.txt", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if canonPath != "empty.txt" {
		t.Errorf("expected 'empty.txt', got %q", canonPath)
	}
	if !strings.HasPrefix(version, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", version)
	}

	content, _ := os.ReadFile(filepath.Join(dir, "empty.txt"))
	if len(content) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(content))
	}
}

func TestFileOpsCreate_GitPathBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, _, err := ops.Create(".git/hooks/pre-commit", "#!/bin/sh\n")
	if err == nil {
		t.Fatal("expected error for .git path create")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsCreate_FilesystemFailure(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	srcDir := filepath.Join(dir, "src")
	if err := os.Chmod(srcDir, 0o555); err != nil {
		t.Fatalf("Chmod read-only dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(srcDir, 0o755)
	})

	_, _, err := ops.Create("src/cannot-create.txt", "content")
	if err == nil {
		t.Fatal("expected filesystem create error")
	}
	if !apperrors.IsCode(err, apperrors.CodeInternal) {
		t.Errorf("expected internal, got %s", apperrors.GetCode(err))
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "cannot-create.txt")); !os.IsNotExist(err) {
		t.Fatalf("cannot-create.txt should not exist after create failure, stat err: %v", err)
	}
}

func TestFileOpsCreate_GitSymlinkAliasBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	if err := os.Symlink(".git", filepath.Join(dir, "gitlink")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, _, err := ops.Create("gitlink/new.txt", "content")
	if err == nil {
		t.Fatal("expected error for .git symlink alias create")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

// --- Delete tests ---

func TestFileOpsDelete_Success(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	canonPath, err := ops.Delete("docs/guide.txt", true)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if canonPath != "docs/guide.txt" {
		t.Errorf("expected 'docs/guide.txt', got %q", canonPath)
	}

	if _, err := os.Stat(filepath.Join(dir, "docs", "guide.txt")); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestFileOpsDelete_Unconfirmed(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, err := ops.Delete("docs/guide.txt", false)
	if err == nil {
		t.Fatal("expected error for unconfirmed delete")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}

	// File should still exist.
	if _, err := os.Stat(filepath.Join(dir, "docs", "guide.txt")); err != nil {
		t.Error("file should still exist after unconfirmed delete")
	}
}

func TestFileOpsDelete_DirectoryRejected(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, err := ops.Delete("src", true)
	if err == nil {
		t.Fatal("expected error for directory delete")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsDelete_NotFound(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, err := ops.Delete("nonexistent.txt", true)
	if err == nil {
		t.Fatal("expected error for nonexistent file delete")
	}
	if !apperrors.IsCode(err, apperrors.CodeStorageNotFound) {
		t.Errorf("expected storage.not_found, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsDelete_GitPathBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	_, err := ops.Delete(".git/HEAD", true)
	if err == nil {
		t.Fatal("expected error for .git path delete")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsDelete_FilesystemFailure(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)
	beforeContent, err := os.ReadFile(filepath.Join(dir, "docs", "guide.txt"))
	if err != nil {
		t.Fatalf("Read guide before delete failure: %v", err)
	}

	docsDir := filepath.Join(dir, "docs")
	if err := os.Chmod(docsDir, 0o555); err != nil {
		t.Fatalf("Chmod read-only dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(docsDir, 0o755)
	})

	_, err = ops.Delete("docs/guide.txt", true)
	if err == nil {
		t.Fatal("expected filesystem delete error")
	}
	if !apperrors.IsCode(err, apperrors.CodeInternal) {
		t.Errorf("expected internal, got %s", apperrors.GetCode(err))
	}
	afterContent, err := os.ReadFile(filepath.Join(dir, "docs", "guide.txt"))
	if err != nil {
		t.Fatalf("Read guide after delete failure: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Fatal("docs/guide.txt changed despite delete failure")
	}
}

func TestFileOpsDelete_GitSymlinkAliasBlocked(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	if err := os.Symlink(".git", filepath.Join(dir, "gitlink")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := ops.Delete("gitlink/HEAD", true)
	if err == nil {
		t.Fatal("expected error for .git symlink alias delete")
	}
	if !apperrors.IsCode(err, apperrors.CodeActionInvalid) {
		t.Errorf("expected action.invalid, got %s", apperrors.GetCode(err))
	}
}

func TestFileOpsDelete_SymlinkRemovesLinkOnly(t *testing.T) {
	dir := makeRepo(t)
	ops := NewFileOperations(dir, 1048576)

	// Create a file and a symlink to it within the repo.
	targetPath := filepath.Join(dir, "src", "target.txt")
	os.WriteFile(targetPath, []byte("target content"), 0o644)
	os.Symlink(targetPath, filepath.Join(dir, "src", "link.txt"))

	_, err := ops.Delete("src/link.txt", true)
	if err != nil {
		t.Fatalf("Delete symlink: %v", err)
	}

	// Symlink should be gone.
	if _, err := os.Lstat(filepath.Join(dir, "src", "link.txt")); !os.IsNotExist(err) {
		t.Error("symlink should have been removed")
	}

	// Target should still exist.
	if _, err := os.Stat(targetPath); err != nil {
		t.Error("target file should still exist after symlink delete")
	}
}
