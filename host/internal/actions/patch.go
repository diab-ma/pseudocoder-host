package actions

import (
	"fmt"
	"os/exec"
	"strings"
)

// BuildPatch constructs a git patch from a file path and chunk content.
// The chunk content should include the @@ header line and all diff lines.
// This patch can be applied using git apply.
// If isNewFile is true, uses /dev/null as the old file (for new files).
//
// Example output (existing file):
//
//	diff --git a/file.txt b/file.txt
//	--- a/file.txt
//	+++ b/file.txt
//	@@ -1,4 +1,4 @@
//	-old line
//	+new line
//	 context
//
// Example output (new file):
//
//	diff --git a/file.txt b/file.txt
//	new file mode 100644
//	--- /dev/null
//	+++ b/file.txt
//	@@ -0,0 +1,2 @@
//	+new line
func BuildPatch(file, chunkContent string, isNewFile bool) string {
	var sb strings.Builder
	sb.WriteString("diff --git a/")
	sb.WriteString(file)
	sb.WriteString(" b/")
	sb.WriteString(file)
	sb.WriteString("\n")
	if isNewFile {
		sb.WriteString("new file mode 100644\n")
		sb.WriteString("--- /dev/null\n")
	} else {
		sb.WriteString("--- a/")
		sb.WriteString(file)
		sb.WriteString("\n")
	}
	sb.WriteString("+++ b/")
	sb.WriteString(file)
	sb.WriteString("\n")
	sb.WriteString(chunkContent)
	// Ensure patch ends with newline for git apply
	if !strings.HasSuffix(chunkContent, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// applyPatch applies a patch using git apply.
// If cached is true, applies to the index only (--cached flag).
// If reverse is true, applies the patch in reverse (--reverse flag).
func (p *Processor) applyPatch(patch string, cached, reverse bool) error {
	args := []string{"apply"}
	if cached {
		args = append(args, "--cached")
	}
	if reverse {
		args = append(args, "--reverse")
	}
	// Read patch from stdin
	args = append(args, "-")

	cmd := exec.Command("git", args...)
	cmd.Dir = p.repoPath
	cmd.Stdin = strings.NewReader(patch)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
