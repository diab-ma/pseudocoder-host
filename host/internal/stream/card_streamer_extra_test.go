package stream

import (
	"testing"
)

// TestGroupChunksByProximity_NestedChunks verifies that when a chunk is fully contained
// within the previous chunk's range (nested), the group's LineEnd remains correctly set
// to the outer chunk's end (i.e., it doesn't shrink).
func TestGroupChunksByProximity_NestedChunks(t *testing.T) {
	// Chunk 1: lines 10-30 (NewStart 10, NewCount 21)
	// Chunk 2: lines 15-20 (NewStart 15, NewCount 6)
	// Chunk 2 is inside Chunk 1.
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 21}, // Ends at 10+21-1 = 30
		{Index: 1, NewStart: 15, NewCount: 6},  // Ends at 15+6-1 = 20
	}

	groups, indexed := GroupChunksByProximity(chunks, 20)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group for nested chunks, got %d", len(groups))
	}

	// LineStart should be 10 (from Chunk 1)
	if groups[0].LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", groups[0].LineStart)
	}

	// LineEnd should be 30 (from Chunk 1), NOT 20 (from Chunk 2)
	// The algorithm does: groups.last.line_end = max(groups.last.line_end, current_end)
	if groups[0].LineEnd != 30 {
		t.Errorf("expected LineEnd 30 (outer chunk end), got %d", groups[0].LineEnd)
	}

	if groups[0].ChunkCount != 2 {
		t.Errorf("expected 2 chunks in group, got %d", groups[0].ChunkCount)
	}

	// Verify both chunks are in group 0
	if indexed[0].GroupIndex != 0 || indexed[1].GroupIndex != 0 {
		t.Error("both chunks should be in group 0")
	}
}

// TestGroupChunksByProximity_OverlappingChunks verifies that overlapping chunks
// are always grouped together, even if proximity is 0.
func TestGroupChunksByProximity_OverlappingChunks(t *testing.T) {
	// Chunk 1: lines 10-20
	// Chunk 2: lines 15-25
	// Overlap of 5 lines.
	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 11}, // Ends at 20
		{Index: 1, NewStart: 15, NewCount: 11}, // Ends at 25
	}

	// Use proximity 0 to enforce strict adjacency/overlap
	groups, indexed := GroupChunksByProximity(chunks, 0)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group for overlapping chunks, got %d", len(groups))
	}

	// LineStart 10, LineEnd 25
	if groups[0].LineStart != 10 {
		t.Errorf("expected LineStart 10, got %d", groups[0].LineStart)
	}
	if groups[0].LineEnd != 25 {
		t.Errorf("expected LineEnd 25, got %d", groups[0].LineEnd)
	}

	if indexed[0].GroupIndex != 0 || indexed[1].GroupIndex != 0 {
		t.Error("both chunks should be in group 0")
	}
}

// TestGroupChunksByProximity_NegativeProximity verifies behavior with negative proximity.
// Logic: chunkRef <= lastGroup.LineEnd + proximity.
// If proximity is negative, chunks must overlap by at least that amount to group.
func TestGroupChunksByProximity_NegativeProximity(t *testing.T) {
	// Chunk 1: lines 10-20
	// Chunk 2: lines 15-25
	// End of Chunk 1 is 20.
	// Ref of Chunk 2 is 15.
	// With proximity -2: 15 <= 20 + (-2) = 18. True. Should group.

	chunks := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 11}, // Ends at 20
		{Index: 1, NewStart: 15, NewCount: 11}, // Starts at 15
	}

	groups, _ := GroupChunksByProximity(chunks, -2)
	if len(groups) != 1 {
		t.Errorf("expected 1 group with negative proximity overlap, got %d", len(groups))
	}

	// Chunk 3: lines 20-30.
	// End of Chunk 1 is 20.
	// Ref of Chunk 3 is 20.
	// With proximity -2: 20 <= 20 + (-2) = 18. False. Should split.
	chunks2 := []ChunkInfo{
		{Index: 0, NewStart: 10, NewCount: 11}, // Ends at 20
		{Index: 1, NewStart: 20, NewCount: 11}, // Starts at 20
	}

	groups2, _ := GroupChunksByProximity(chunks2, -2)
	if len(groups2) != 2 {
		t.Errorf("expected 2 groups when overlap is insufficient for negative proximity, got %d", len(groups2))
	}
}
