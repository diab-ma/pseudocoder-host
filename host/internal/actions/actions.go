// Package actions provides git operations for accepting and rejecting review cards.
// It uses git commands to stage (accept) or restore (reject) file changes based
// on user decisions from the mobile client.
//
// This package is organized into several files:
//   - actions.go: Package entry point (this file)
//   - processor.go: Processor struct, constructor, and setters
//   - decision_file.go: File-level accept/reject (ProcessDecision)
//   - decision_chunk.go: Chunk-level accept/reject (ProcessChunkDecision)
//   - delete.go: Untracked file deletion (DeleteUntrackedFile)
//   - patch.go: Patch building and application (BuildPatch, applyPatch)
//   - undo.go: Undo operations (ProcessUndo, ProcessChunkUndo)
package actions
