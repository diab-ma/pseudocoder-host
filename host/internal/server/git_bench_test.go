package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Cached large repos for benchmarks (created once per test binary run).
var (
	largeHistoryRepo     string
	largeHistoryRepoOnce sync.Once

	largeBranchesRepo     string
	largeBranchesRepoOnce sync.Once
)

func setupLargeHistoryRepo(b *testing.B) string {
	b.Helper()
	largeHistoryRepoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bench-history-*")
		if err != nil {
			b.Fatalf("mkdtemp: %v", err)
		}

		benchGitCmd(b, dir, "init")
		benchGitCmd(b, dir, "config", "user.email", "bench@example.com")
		benchGitCmd(b, dir, "config", "user.name", "Bench User")

		// Create initial file
		f := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(f, []byte("initial"), 0644); err != nil {
			b.Fatalf("write: %v", err)
		}
		benchGitCmd(b, dir, "add", "file.txt")
		benchGitCmd(b, dir, "commit", "-m", "Initial commit")

		// Create exactly 10000 total commits (initial + 9999 additional commits).
		for i := 1; i < 10000; i++ {
			if err := os.WriteFile(f, []byte(fmt.Sprintf("commit %d", i)), 0644); err != nil {
				b.Fatalf("write: %v", err)
			}
			benchGitCmd(b, dir, "add", "file.txt")
			benchGitCmd(b, dir, "commit", "-m", fmt.Sprintf("Commit %d", i))
		}
		if got := benchCommitCount(b, dir); got != 10000 {
			b.Fatalf("history fixture commit count=%d, want 10000", got)
		}

		largeHistoryRepo = dir
	})
	return largeHistoryRepo
}

func setupLargeBranchesRepo(b *testing.B) string {
	b.Helper()
	largeBranchesRepoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bench-branches-*")
		if err != nil {
			b.Fatalf("mkdtemp: %v", err)
		}

		benchGitCmd(b, dir, "init")
		benchGitCmd(b, dir, "config", "user.email", "bench@example.com")
		benchGitCmd(b, dir, "config", "user.name", "Bench User")

		f := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(f, []byte("initial"), 0644); err != nil {
			b.Fatalf("write: %v", err)
		}
		benchGitCmd(b, dir, "add", "file.txt")
		benchGitCmd(b, dir, "commit", "-m", "Initial commit")

		// Create exactly 500 local branches total:
		// default branch + 499 created branches.
		for i := 0; i < 499; i++ {
			benchGitCmd(b, dir, "branch", fmt.Sprintf("local/branch-%04d", i))
		}

		// Create a bare "remote" and push to populate refs/remotes
		remoteDir, err := os.MkdirTemp("", "bench-remote-*")
		if err != nil {
			b.Fatalf("mkdtemp: %v", err)
		}
		benchGitCmd(b, remoteDir, "init", "--bare")
		benchGitCmd(b, dir, "remote", "add", "origin", remoteDir)

		// Push main first
		branch := "main"
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err == nil {
			branch = string(out[:len(out)-1])
		}
		benchGitCmd(b, dir, "push", "origin", branch)

		// Push the 499 created local branches so tracked remote branches are:
		// default upstream + 499 branch refs = 500 total tracked remotes.
		for i := 0; i < 499; i++ {
			branchName := fmt.Sprintf("local/branch-%04d", i)
			benchGitCmd(b, dir, "push", "origin", branchName)
		}

		// Fetch to populate refs/remotes
		benchGitCmd(b, dir, "fetch", "origin")
		localCount, trackedRemoteCount := benchBranchCounts(b, dir)
		if localCount != 500 {
			b.Fatalf("branches fixture local count=%d, want 500", localCount)
		}
		if trackedRemoteCount != 500 {
			b.Fatalf("branches fixture tracked remote count=%d, want 500", trackedRemoteCount)
		}

		largeBranchesRepo = dir
	})
	return largeBranchesRepo
}

func benchGitCmd(b *testing.B, dir string, args ...string) {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func benchGitCmdOutput(b *testing.B, dir string, args ...string) string {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func benchCommitCount(b *testing.B, dir string) int {
	b.Helper()
	count := strings.TrimSpace(benchGitCmdOutput(b, dir, "rev-list", "--count", "HEAD"))
	var parsed int
	if _, err := fmt.Sscanf(count, "%d", &parsed); err != nil {
		b.Fatalf("parse commit count %q: %v", count, err)
	}
	return parsed
}

func benchBranchCounts(b *testing.B, dir string) (int, int) {
	b.Helper()
	localOutput := benchGitCmdOutput(b, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	trackedOutput := benchGitCmdOutput(b, dir, "for-each-ref", "--format=%(refname:short)", "refs/remotes/")

	localCount := 0
	for _, line := range strings.Split(strings.TrimSpace(localOutput), "\n") {
		if strings.TrimSpace(line) != "" {
			localCount++
		}
	}

	trackedCount := 0
	for _, line := range strings.Split(strings.TrimSpace(trackedOutput), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.HasSuffix(ref, "/HEAD") || !strings.Contains(ref, "/") {
			continue
		}
		trackedCount++
	}
	return localCount, trackedCount
}

func BenchmarkRepoHistoryLargeRepo(b *testing.B) {
	dir := setupLargeHistoryRepo(b)
	ops := NewGitOperations(dir, false, false, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := ops.GetHistory("", 50)
		if err != nil {
			b.Fatalf("GetHistory: %v", err)
		}
	}
}

func BenchmarkRepoBranchesLargeRepo(b *testing.B) {
	dir := setupLargeBranchesRepo(b)
	ops := NewGitOperations(dir, false, false, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, err := ops.GetBranches()
		if err != nil {
			b.Fatalf("GetBranches: %v", err)
		}
	}
}
