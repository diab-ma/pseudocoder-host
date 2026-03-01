package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	apperrors "github.com/pseudocoder/host/internal/errors"
)

// fileOpsImpl implements FileOperations against the local filesystem.
type fileOpsImpl struct {
	repoPath     string
	sizeCapBytes int64
}

// NewFileOperations creates a FileOperations scoped to repoPath.
// Files larger than sizeCapBytes are returned as read-only (too_large).
func NewFileOperations(repoPath string, sizeCapBytes int64) FileOperations {
	return &fileOpsImpl{repoPath: repoPath, sizeCapBytes: sizeCapBytes}
}

// canonicalizePath normalizes a request path to a clean repo-relative path.
// Empty or "." returns ".". Absolute paths and traversal escapes are rejected.
func canonicalizePath(reqPath string) (string, error) {
	if reqPath == "" || reqPath == "." {
		return ".", nil
	}

	// Reject absolute paths.
	if filepath.IsAbs(reqPath) {
		return "", apperrors.New(apperrors.CodeActionInvalid, "absolute paths are not allowed")
	}

	cleaned := filepath.Clean(reqPath)

	// Reject traversal escapes (paths that resolve outside the repo root).
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", apperrors.New(apperrors.CodeActionInvalid, "path escapes repository boundary")
	}

	return cleaned, nil
}

// resolveAndCheckBoundary resolves symlinks and verifies the target is within the repo.
func (f *fileOpsImpl) resolveAndCheckBoundary(relPath string) (string, error) {
	var absPath string
	if relPath == "." {
		absPath = f.repoPath
	} else {
		absPath = filepath.Join(f.repoPath, relPath)
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("path not found: %s", relPath))
		}
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve path", err)
	}

	// Verify resolved path is within the repo root.
	repoResolved, err := filepath.EvalSymlinks(f.repoPath)
	if err != nil {
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve repo root", err)
	}

	// Ensure the resolved path starts with the repo root.
	if resolved != repoResolved && !strings.HasPrefix(resolved, repoResolved+string(filepath.Separator)) {
		return "", apperrors.New(apperrors.CodeActionInvalid, "path escapes repository boundary via symlink")
	}

	return resolved, nil
}

// List returns sorted directory entries for the given repo-relative path.
func (f *fileOpsImpl) List(path string) ([]FileEntry, string, error) {
	canonPath, err := canonicalizePath(path)
	if err != nil {
		return nil, "", err
	}

	resolved, err := f.resolveAndCheckBoundary(canonPath)
	if err != nil {
		return nil, "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("directory not found: %s", canonPath))
		}
		return nil, "", apperrors.Wrap(apperrors.CodeInternal, "failed to stat directory", err)
	}
	if !info.IsDir() {
		return nil, "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("path is not a directory: %s", canonPath))
	}

	dirEntries, err := os.ReadDir(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("directory not found: %s", canonPath))
		}
		return nil, "", apperrors.Wrap(apperrors.CodeInternal, "failed to read directory", err)
	}

	entries := make([]FileEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		entry := FileEntry{
			Name: de.Name(),
		}
		if canonPath == "." {
			entry.Path = de.Name()
		} else {
			entry.Path = canonPath + "/" + de.Name()
		}

		switch {
		case de.Type()&os.ModeSymlink != 0:
			entry.Kind = "symlink"
		case de.IsDir():
			entry.Kind = "dir"
		default:
			entry.Kind = "file"
		}

		// Get file info for size and modification time.
		if info, err := de.Info(); err == nil {
			if entry.Kind == "file" {
				sz := info.Size()
				entry.SizeBytes = &sz
			}
			if entry.Kind != "symlink" {
				mt := info.ModTime().UnixMilli()
				entry.ModifiedAt = &mt
			}
		}

		entries = append(entries, entry)
	}

	// Sort: dirs first, then files, then symlinks. Within each group, by name.
	sort.Slice(entries, func(i, j int) bool {
		ki := kindOrder(entries[i].Kind)
		kj := kindOrder(entries[j].Kind)
		if ki != kj {
			return ki < kj
		}
		return entries[i].Name < entries[j].Name
	})

	return entries, canonPath, nil
}

// kindOrder returns sort priority: dirs=0, files=1, symlinks=2.
func kindOrder(kind string) int {
	switch kind {
	case "dir":
		return 0
	case "file":
		return 1
	default:
		return 2
	}
}

// Read returns file contents and metadata for the given repo-relative path.
func (f *fileOpsImpl) Read(path string) (*FileReadData, string, error) {
	canonPath, err := canonicalizePath(path)
	if err != nil {
		return nil, "", err
	}

	if canonPath == "" || canonPath == "." {
		return nil, "", apperrors.New(apperrors.CodeActionInvalid, "path is required for file read")
	}

	resolved, err := f.resolveAndCheckBoundary(canonPath)
	if err != nil {
		return nil, "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("file not found: %s", canonPath))
		}
		return nil, "", apperrors.Wrap(apperrors.CodeInternal, "failed to stat file", err)
	}

	if info.IsDir() {
		return nil, "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("path is a directory: %s", canonPath))
	}

	// Check size cap.
	if info.Size() > f.sizeCapBytes {
		// Keep version semantics content-derived for oversized files by hashing
		// with a streaming read instead of materializing content into memory.
		version, err := hashFileVersion(resolved)
		if err != nil {
			return nil, "", apperrors.Wrap(apperrors.CodeInternal, "failed to hash file", err)
		}
		return &FileReadData{
			Version:        version,
			ReadOnlyReason: "too_large",
		}, canonPath, nil
	}

	content, err := os.ReadFile(resolved)
	if err != nil {
		return nil, "", apperrors.Wrap(apperrors.CodeInternal, "failed to read file", err)
	}

	// Check for binary content: NUL bytes or invalid UTF-8.
	isBinary := bytes.Contains(content, []byte{0}) || !utf8.Valid(content)

	version := computeVersion(content)

	if isBinary {
		return &FileReadData{
			Version:        version,
			ReadOnlyReason: "binary",
		}, canonPath, nil
	}

	lineEnding := detectLineEnding(content)

	return &FileReadData{
		Content:    string(content),
		Encoding:   "utf-8",
		LineEnding: lineEnding,
		Version:    version,
	}, canonPath, nil
}

// detectLineEnding returns "crlf" if every \n is preceded by \r, otherwise "lf".
func detectLineEnding(data []byte) string {
	lfCount := bytes.Count(data, []byte{'\n'})
	if lfCount == 0 {
		return "lf"
	}
	crlfCount := bytes.Count(data, []byte{'\r', '\n'})
	if crlfCount == lfCount {
		return "crlf"
	}
	return "lf"
}

// hashFileVersion returns a sha256 version token for a file without loading
// the entire content into memory.
func hashFileVersion(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// computeVersion returns a sha256 version token for in-memory content.
func computeVersion(content []byte) string {
	h := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(h[:])
}

// isGitPath returns true if the canonicalized path is ".git" or under ".git/".
func isGitPath(canonPath string) bool {
	return canonPath == ".git" || strings.HasPrefix(canonPath, ".git"+string(filepath.Separator))
}

// isWithinPath reports whether candidate equals parent or is a descendant of it.
func isWithinPath(candidate, parent string) bool {
	return candidate == parent || strings.HasPrefix(candidate, parent+string(filepath.Separator))
}

// resolvedGitDir resolves the repository's .git directory path.
// Returns an empty string if the repo has no .git directory.
func (f *fileOpsImpl) resolvedGitDir() (string, error) {
	repoResolved, err := filepath.EvalSymlinks(f.repoPath)
	if err != nil {
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve repo root", err)
	}

	gitResolved, err := filepath.EvalSymlinks(filepath.Join(repoResolved, ".git"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve .git directory", err)
	}

	return gitResolved, nil
}

// isResolvedGitPath returns true when a resolved path targets .git or one of its descendants.
func (f *fileOpsImpl) isResolvedGitPath(resolvedPath string) (bool, error) {
	gitResolved, err := f.resolvedGitDir()
	if err != nil {
		return false, err
	}
	if gitResolved == "" {
		return false, nil
	}
	return isWithinPath(resolvedPath, gitResolved), nil
}

// normalizeLineEndings normalizes content to the target line ending style.
// First strips all \r\n to \n, then if style is "crlf", converts \n to \r\n.
func normalizeLineEndings(content string, style string) string {
	// Normalize to LF first.
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if style == "crlf" {
		normalized = strings.ReplaceAll(normalized, "\n", "\r\n")
	}
	return normalized
}

// atomicWriteFile writes content to targetPath atomically using a temp file + rename.
// Preserves the given file permissions.
func atomicWriteFile(targetPath string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".pseudocoder-write-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up on any error.
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	success = true
	return nil
}

// resolveParentBoundary resolves the parent directory of relPath and verifies
// it is within the repo. Returns the resolved parent directory path.
// Used for Create where the target file doesn't exist yet.
func (f *fileOpsImpl) resolveParentBoundary(relPath string) (string, error) {
	parentRel := filepath.Dir(relPath)
	if parentRel == "." {
		parentRel = "."
	}

	absParent := filepath.Join(f.repoPath, parentRel)
	resolvedParent, err := filepath.EvalSymlinks(absParent)
	if err != nil {
		if os.IsNotExist(err) {
			return "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("parent directory not found: %s", parentRel))
		}
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve parent directory", err)
	}

	// Verify within repo.
	repoResolved, err := filepath.EvalSymlinks(f.repoPath)
	if err != nil {
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve repo root", err)
	}

	if resolvedParent != repoResolved && !strings.HasPrefix(resolvedParent, repoResolved+string(filepath.Separator)) {
		return "", apperrors.New(apperrors.CodeActionInvalid, "path escapes repository boundary via symlink")
	}

	// Verify it's a directory.
	info, err := os.Stat(resolvedParent)
	if err != nil {
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to stat parent directory", err)
	}
	if !info.IsDir() {
		return "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("parent is not a directory: %s", parentRel))
	}

	return resolvedParent, nil
}

// isBinaryContent returns true if content contains NUL bytes or invalid UTF-8.
func isBinaryContent(data []byte) bool {
	return bytes.Contains(data, []byte{0}) || !utf8.Valid(data)
}

// Write replaces file content with conflict detection via base_version.
func (f *fileOpsImpl) Write(path, content, baseVersion string) (string, string, error) {
	canonPath, err := canonicalizePath(path)
	if err != nil {
		return "", "", err
	}

	if canonPath == "." {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "path is required for file write")
	}

	if isGitPath(canonPath) {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "writes to .git paths are not allowed")
	}

	resolved, err := f.resolveAndCheckBoundary(canonPath)
	if err != nil {
		return "", "", err
	}
	if inGit, err := f.isResolvedGitPath(resolved); err != nil {
		return "", "", err
	} else if inGit {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "writes to .git paths are not allowed")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("file not found: %s", canonPath))
		}
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to stat file", err)
	}

	if info.IsDir() {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("path is a directory: %s", canonPath))
	}

	// Check size cap.
	if info.Size() > f.sizeCapBytes {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("file too large to write: %s", canonPath))
	}

	// Read current content.
	currentBytes, err := os.ReadFile(resolved)
	if err != nil {
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to read file", err)
	}

	// Reject binary files.
	if isBinaryContent(currentBytes) {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("cannot write binary file: %s", canonPath))
	}

	// Check version for conflict detection.
	currentVersion := computeVersion(currentBytes)
	if currentVersion != baseVersion {
		return "", "", apperrors.New(apperrors.CodeConflictDetected,
			fmt.Sprintf("file %s has been modified (expected %s, got %s)", canonPath, baseVersion, currentVersion))
	}

	// Detect and preserve line endings.
	lineStyle := detectLineEnding(currentBytes)
	normalizedContent := normalizeLineEndings(content, lineStyle)
	normalizedBytes := []byte(normalizedContent)

	// Atomic write preserving original permissions.
	if err := atomicWriteFile(resolved, normalizedBytes, info.Mode()); err != nil {
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to write file", err)
	}

	newVersion := computeVersion(normalizedBytes)
	return canonPath, newVersion, nil
}

// Create creates a new file with the given content.
func (f *fileOpsImpl) Create(path, content string) (string, string, error) {
	canonPath, err := canonicalizePath(path)
	if err != nil {
		return "", "", err
	}

	if canonPath == "." {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "path is required for file create")
	}

	if isGitPath(canonPath) {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "creates in .git paths are not allowed")
	}

	// Verify parent directory exists and is within repo.
	resolvedParent, err := f.resolveParentBoundary(canonPath)
	if err != nil {
		return "", "", err
	}
	if inGit, err := f.isResolvedGitPath(resolvedParent); err != nil {
		return "", "", err
	} else if inGit {
		return "", "", apperrors.New(apperrors.CodeActionInvalid, "creates in .git paths are not allowed")
	}

	absPath := filepath.Join(f.repoPath, canonPath)

	contentBytes := []byte(content)
	// Create with O_EXCL to make existence checks atomic under concurrency.
	file, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return "", "", apperrors.New(apperrors.CodeStorageAlreadyExists, fmt.Sprintf("file already exists: %s", canonPath))
		}
		if os.IsNotExist(err) {
			return "", "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("parent directory not found: %s", filepath.Dir(canonPath)))
		}
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to create file", err)
	}

	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		_ = file.Close()
		_ = os.Remove(absPath)
	}()

	if _, err := file.Write(contentBytes); err != nil {
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to create file", err)
	}
	if err := file.Close(); err != nil {
		return "", "", apperrors.Wrap(apperrors.CodeInternal, "failed to create file", err)
	}
	cleanupNeeded = false

	version := computeVersion(contentBytes)
	return canonPath, version, nil
}

// Delete removes a file from the filesystem.
func (f *fileOpsImpl) Delete(path string, confirmed bool) (string, error) {
	canonPath, err := canonicalizePath(path)
	if err != nil {
		return "", err
	}

	if canonPath == "." {
		return "", apperrors.New(apperrors.CodeActionInvalid, "path is required for file delete")
	}

	if isGitPath(canonPath) {
		return "", apperrors.New(apperrors.CodeActionInvalid, "deletes in .git paths are not allowed")
	}

	if !confirmed {
		return "", apperrors.New(apperrors.CodeActionInvalid, "delete requires confirmed=true")
	}

	// Resolve parent boundary (not target — symlink may point outside or be broken).
	if _, err := f.resolveParentBoundary(canonPath); err != nil {
		return "", err
	}

	absPath := filepath.Join(f.repoPath, canonPath)

	// Use Lstat to check the link itself, not its target.
	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", apperrors.New(apperrors.CodeStorageNotFound, fmt.Sprintf("file not found: %s", canonPath))
		}
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to stat file", err)
	}

	if info.IsDir() {
		return "", apperrors.New(apperrors.CodeActionInvalid, fmt.Sprintf("path is a directory: %s", canonPath))
	}
	if info.Mode()&os.ModeSymlink == 0 {
		resolved, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", apperrors.Wrap(apperrors.CodeInternal, "failed to resolve file path", err)
		}
		if inGit, err := f.isResolvedGitPath(resolved); err != nil {
			return "", err
		} else if inGit {
			return "", apperrors.New(apperrors.CodeActionInvalid, "deletes in .git paths are not allowed")
		}
	}

	// os.Remove removes the link itself, preserving the target.
	if err := os.Remove(absPath); err != nil {
		return "", apperrors.Wrap(apperrors.CodeInternal, "failed to delete file", err)
	}

	return canonPath, nil
}
