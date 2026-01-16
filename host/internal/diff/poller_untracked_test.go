package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPoller_DetectsUntrackedFile verifies that untracked files are detected
// and included in the diff output when IncludeUntracked is true.
func TestPoller_DetectsUntrackedFile(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Create a new untracked file
	untrackedFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(untrackedFile, []byte("new content\n"), 0644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	// 1. Test WITHOUT IncludeUntracked - should NOT see the file
	p1 := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: false,
	})

	chunks1, err := p1.PollOnce()
	if err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	if len(chunks1) != 0 {
		t.Errorf("expected 0 chunks without IncludeUntracked, got %d", len(chunks1))
	}

	// 2. Test WITH IncludeUntracked - SHOULD see the file
	p2 := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
	})

	chunks2, rawDiff, err := p2.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	// Should see 1 chunk for new.txt
	if len(chunks2) != 1 {
		t.Fatalf("expected 1 chunk with IncludeUntracked, got %d", len(chunks2))
	}

	if chunks2[0].File != "new.txt" {
		t.Errorf("expected file 'new.txt', got '%s'", chunks2[0].File)
	}

	// Verify content looks like a diff addition
	// Parser typically includes a trailing newline in chunk content
	expectedContent := "@@ -0,0 +1,1 @@\n+new content\n"
	if chunks2[0].Content != expectedContent {
		t.Errorf("unexpected chunk content:\ngot: %q\nwant: %q", chunks2[0].Content, expectedContent)
	}

	// Verify raw diff contains standard git headers
	if !strings.Contains(rawDiff, "diff --git a/new.txt b/new.txt") {
		t.Error("raw diff missing diff header")
	}
	if !strings.Contains(rawDiff, "new file mode") {
		t.Error("raw diff missing new file mode")
	}
}

// TestPoller_UntrackedBinaryFileHashChanges verifies that untracked binary files
// produce different synthetic diffs when their content changes. This is critical
// for CardStreamer to detect updates via ExtractFileDiffSection hashing.
func TestPoller_UntrackedBinaryFileHashChanges(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	binaryFile := filepath.Join(dir, "image.png")

	// Create initial binary file (contains null byte to trigger binary detection)
	binaryContentV1 := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0x03}
	if err := os.WriteFile(binaryFile, binaryContentV1, 0644); err != nil {
		t.Fatalf("failed to write binary file: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
	})

	// Get first diff
	_, rawDiffV1, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	// Verify it's detected as binary
	if !strings.Contains(rawDiffV1, "Binary files") {
		t.Fatal("expected binary file detection in v1 diff")
	}

	// Verify it has an index line with hash
	if !strings.Contains(rawDiffV1, "index 0000000..") {
		t.Error("v1 diff missing index line with hash")
	}

	// Extract the file section for hashing (simulates what CardStreamer does)
	sectionV1 := ExtractFileDiffSection(rawDiffV1, "image.png")
	if sectionV1 == "" {
		t.Fatal("failed to extract file section from v1 diff")
	}

	// Modify the binary file with different content
	binaryContentV2 := []byte{0x89, 'P', 'N', 'G', 0x00, 0xFF, 0xFE, 0xFD}
	if err := os.WriteFile(binaryFile, binaryContentV2, 0644); err != nil {
		t.Fatalf("failed to update binary file: %v", err)
	}

	// Get second diff
	_, rawDiffV2, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	sectionV2 := ExtractFileDiffSection(rawDiffV2, "image.png")
	if sectionV2 == "" {
		t.Fatal("failed to extract file section from v2 diff")
	}

	// Critical assertion: the sections must be different because content changed
	if sectionV1 == sectionV2 {
		t.Errorf("expected different file sections for different binary content\nv1: %s\nv2: %s", sectionV1, sectionV2)
	}

	// Verify both have index lines but with different hashes
	if !strings.Contains(sectionV1, "index 0000000..") {
		t.Error("v1 section missing index line")
	}
	if !strings.Contains(sectionV2, "index 0000000..") {
		t.Error("v2 section missing index line")
	}
}

// TestPoller_UntrackedSymlink verifies that untracked symbolic links are detected
// and represented correctly with mode 120000 and the link target as content.
// This matches git's behavior for symlinks.
func TestPoller_UntrackedSymlink(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Create a symlink pointing to an existing file
	targetFile := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(targetFile, []byte("target content\n"), 0644); err != nil {
		t.Fatalf("failed to write target file: %v", err)
	}

	// Stage and commit target so it's not untracked
	cmd := exec.Command("git", "add", "target.txt")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "add target")
	cmd.Dir = dir
	cmd.Run()

	// Create untracked symlink
	symlinkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
	})

	_, rawDiff, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	// Verify symlink mode 120000 is emitted
	if !strings.Contains(rawDiff, "new file mode 120000") {
		t.Errorf("expected symlink mode 120000 in diff:\n%s", rawDiff)
	}

	// Verify content is the link target path, NOT the target file's content
	if !strings.Contains(rawDiff, "+target.txt") {
		t.Errorf("expected symlink target path in diff content:\n%s", rawDiff)
	}

	// Verify we don't see the target file's content
	if strings.Contains(rawDiff, "target content") {
		t.Errorf("diff should contain link target path, not target file content:\n%s", rawDiff)
	}

	// Verify "No newline at end of file" marker (git always emits this for symlinks)
	if !strings.Contains(rawDiff, "\\ No newline at end of file") {
		t.Errorf("expected 'No newline at end of file' marker for symlink:\n%s", rawDiff)
	}
}

// TestPoller_UntrackedBrokenSymlink verifies that broken symlinks (pointing to
// non-existent targets) are still included in the diff with correct mode and content.
func TestPoller_UntrackedBrokenSymlink(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	// Create a symlink pointing to a non-existent file
	symlinkPath := filepath.Join(dir, "broken_link.txt")
	if err := os.Symlink("/nonexistent/path/file.txt", symlinkPath); err != nil {
		t.Fatalf("failed to create broken symlink: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
	})

	_, rawDiff, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	// Broken symlinks should still be detected and included
	if !strings.Contains(rawDiff, "broken_link.txt") {
		t.Fatalf("broken symlink should be included in diff:\n%s", rawDiff)
	}

	// Verify symlink mode
	if !strings.Contains(rawDiff, "new file mode 120000") {
		t.Errorf("expected symlink mode 120000 for broken symlink:\n%s", rawDiff)
	}

	// Verify content is the (non-existent) target path
	if !strings.Contains(rawDiff, "+/nonexistent/path/file.txt") {
		t.Errorf("expected broken symlink target path in diff:\n%s", rawDiff)
	}
}

// TestPoller_UntrackedSymlinkHashChanges verifies that symlink target changes
// produce different hashes for CardStreamer change detection.
func TestPoller_UntrackedSymlinkHashChanges(t *testing.T) {
	dir := setupTestRepo(t)
	defer os.RemoveAll(dir)

	symlinkPath := filepath.Join(dir, "link.txt")

	// Create symlink pointing to target1
	if err := os.Symlink("target1.txt", symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	p := NewPoller(PollerConfig{
		RepoPath:         dir,
		PollInterval:     50 * time.Millisecond,
		IncludeUntracked: true,
	})

	_, rawDiffV1, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	sectionV1 := ExtractFileDiffSection(rawDiffV1, "link.txt")
	if sectionV1 == "" {
		t.Fatal("failed to extract symlink section from v1 diff")
	}

	// Change symlink to point to different target
	os.Remove(symlinkPath)
	if err := os.Symlink("target2.txt", symlinkPath); err != nil {
		t.Fatalf("failed to recreate symlink: %v", err)
	}

	_, rawDiffV2, err := p.PollOnceRaw()
	if err != nil {
		t.Fatalf("PollOnceRaw failed: %v", err)
	}

	sectionV2 := ExtractFileDiffSection(rawDiffV2, "link.txt")
	if sectionV2 == "" {
		t.Fatal("failed to extract symlink section from v2 diff")
	}

	// Sections must be different for CardStreamer to detect the change
	if sectionV1 == sectionV2 {
		t.Errorf("expected different sections for different symlink targets\nv1: %s\nv2: %s", sectionV1, sectionV2)
	}
}
