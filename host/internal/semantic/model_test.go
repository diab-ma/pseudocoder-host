package semantic

import (
	"strings"
	"testing"
)

func TestNormalizeSemanticKind(t *testing.T) {
	tests := []struct {
		raw  string
		want SemanticKind
	}{
		{"import", KindImport},
		{"function", KindFunction},
		{"generic", KindGeneric},
		{"binary", KindBinary},
		{"deleted", KindDeleted},
		{"renamed", KindRenamed},
		// Unknown/empty/case-mismatch -> generic
		{"", KindGeneric},
		{"IMPORT", KindImport},   // case-insensitive
		{"Function", KindFunction}, // case-insensitive
		{"unknown", KindGeneric},
		{"class", KindGeneric},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := NormalizeKind(tt.raw)
			if got != tt.want {
				t.Errorf("NormalizeKind(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSemanticLabelFallback(t *testing.T) {
	// Group label fallbacks
	groupTests := []struct {
		kind     SemanticKind
		explicit string
		want     string
	}{
		{KindImport, "", "Imports"},
		{KindFunction, "", "Function changes"},
		{KindGeneric, "", "Code changes"},
		{KindBinary, "", "Binary change"},
		{KindDeleted, "", "Deletion"},
		{KindRenamed, "", "Rename"},
		// Explicit override
		{KindImport, "Custom Group", "Custom Group"},
		{KindGeneric, "My Label", "My Label"},
	}

	for _, tt := range groupTests {
		t.Run("group_"+string(tt.kind)+"_"+tt.explicit, func(t *testing.T) {
			got := GroupLabel(tt.kind, tt.explicit)
			if got != tt.want {
				t.Errorf("GroupLabel(%q, %q) = %q, want %q", tt.kind, tt.explicit, got, tt.want)
			}
		})
	}

	// Chunk label fallbacks
	chunkTests := []struct {
		kind     SemanticKind
		explicit string
		want     string
	}{
		{KindImport, "", "Import"},
		{KindFunction, "", "Function"},
		{KindGeneric, "", "Changes"},
		{KindBinary, "", "Binary"},
		{KindDeleted, "", "Deleted"},
		{KindRenamed, "", "Renamed"},
		// Explicit override
		{KindFunction, "Custom Chunk", "Custom Chunk"},
	}

	for _, tt := range chunkTests {
		t.Run("chunk_"+string(tt.kind)+"_"+tt.explicit, func(t *testing.T) {
			got := ChunkLabel(tt.kind, tt.explicit)
			if got != tt.want {
				t.Errorf("ChunkLabel(%q, %q) = %q, want %q", tt.kind, tt.explicit, got, tt.want)
			}
		})
	}
}

func TestBuildSemanticGroupID(t *testing.T) {
	// Determinism: same inputs produce same output
	id1 := BuildGroupID("card-1", KindImport, []string{"m1", "m2"})
	id2 := BuildGroupID("card-1", KindImport, []string{"m1", "m2"})
	if id1 != id2 {
		t.Errorf("BuildGroupID not deterministic: %q != %q", id1, id2)
	}

	// Order-independence: different member order produces same ID
	id3 := BuildGroupID("card-1", KindImport, []string{"m2", "m1"})
	if id1 != id3 {
		t.Errorf("BuildGroupID not order-independent: %q != %q", id1, id3)
	}

	// Different inputs produce different IDs
	id4 := BuildGroupID("card-2", KindImport, []string{"m1", "m2"})
	if id1 == id4 {
		t.Errorf("Different cardID should produce different ID")
	}

	id5 := BuildGroupID("card-1", KindFunction, []string{"m1", "m2"})
	if id1 == id5 {
		t.Errorf("Different kind should produce different ID")
	}

	id6 := BuildGroupID("card-1", KindImport, []string{"m1", "m3"})
	if id1 == id6 {
		t.Errorf("Different memberIDs should produce different ID")
	}

	// Format: "sg-" + 12 hex chars
	if !strings.HasPrefix(id1, "sg-") {
		t.Errorf("Expected prefix 'sg-', got %q", id1)
	}
	hex := strings.TrimPrefix(id1, "sg-")
	if len(hex) != 12 {
		t.Errorf("Expected 12 hex chars after 'sg-', got %d: %q", len(hex), hex)
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("Non-hex character in ID: %c", c)
		}
	}
}

func TestMemberID(t *testing.T) {
	// Non-empty hash returns hash directly
	got := MemberID("abc123", 5)
	if got != "abc123" {
		t.Errorf("MemberID with hash = %q, want %q", got, "abc123")
	}

	// Empty hash returns idx:N
	got = MemberID("", 3)
	if got != "idx:3" {
		t.Errorf("MemberID without hash = %q, want %q", got, "idx:3")
	}

	got = MemberID("", 0)
	if got != "idx:0" {
		t.Errorf("MemberID without hash at 0 = %q, want %q", got, "idx:0")
	}
}
