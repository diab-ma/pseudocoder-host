package semantic

import (
	"context"
	"testing"
	"time"
)

func TestTreeSitterCLI_UnsupportedExtension(t *testing.T) {
	hint := TryParseChunk(context.Background(), ".py", []string{"import os"}, 20*time.Millisecond)
	if hint.OK {
		t.Error("expected OK=false for unsupported extension")
	}
}

func TestTreeSitterCLI_EmptyChangedLines(t *testing.T) {
	hint := TryParseChunk(context.Background(), ".go", nil, 20*time.Millisecond)
	if hint.OK {
		t.Error("expected OK=false for empty changed lines")
	}
}

func TestTreeSitterCLI_CommandMissing(t *testing.T) {
	old := treeSitterBinary
	treeSitterBinary = "nonexistent-binary-xyzzy"
	defer func() { treeSitterBinary = old }()

	hint := TryParseChunk(context.Background(), ".go", []string{"package main"}, 100*time.Millisecond)
	if hint.OK {
		t.Error("expected OK=false when tree-sitter binary is missing")
	}
	if hint.Kind != "" {
		t.Errorf("expected empty Kind when binary is missing, got %s", hint.Kind)
	}
}

func TestTreeSitterCLI_NonZeroExit(t *testing.T) {
	old := treeSitterBinary
	treeSitterBinary = "false" // /usr/bin/false always exits 1
	defer func() { treeSitterBinary = old }()

	hint := TryParseChunk(context.Background(), ".go", []string{"func main() {}"}, 100*time.Millisecond)
	if hint.OK {
		t.Error("expected OK=false for non-zero exit")
	}
	if hint.Kind != "" {
		t.Errorf("expected empty Kind on non-zero exit, got %s", hint.Kind)
	}
	if hint.Label != "" {
		t.Errorf("expected empty Label on non-zero exit, got %s", hint.Label)
	}
}

func TestTreeSitterCLI_Timeout(t *testing.T) {
	// Use an extremely short timeout to force a timeout.
	hint := TryParseChunk(context.Background(), ".go", []string{"package main"}, 1*time.Nanosecond)
	if hint.OK {
		t.Error("expected OK=false for timeout")
	}
}

func TestTreeSitterCLI_FallbackCorrectnessKPI(t *testing.T) {
	runWithBinary := func(binary string, run func() ParserHint) ParserHint {
		old := treeSitterBinary
		treeSitterBinary = binary
		defer func() { treeSitterBinary = old }()
		return run()
	}

	cases := []struct {
		name string
		run  func() ParserHint
	}{
		{
			name: "unsupported_extension",
			run: func() ParserHint {
				return TryParseChunk(context.Background(), ".py", []string{"import os"}, 20*time.Millisecond)
			},
		},
		{
			name: "missing_binary",
			run: func() ParserHint {
				return runWithBinary("nonexistent-binary-xyzzy", func() ParserHint {
					return TryParseChunk(context.Background(), ".go", []string{"package main"}, 100*time.Millisecond)
				})
			},
		},
		{
			name: "timeout",
			run: func() ParserHint {
				return TryParseChunk(context.Background(), ".go", []string{"package main"}, 1*time.Nanosecond)
			},
		},
		{
			name: "non_zero_exit",
			run: func() ParserHint {
				return runWithBinary("false", func() ParserHint {
					return TryParseChunk(context.Background(), ".go", []string{"func main() {}"}, 100*time.Millisecond)
				})
			},
		},
		{
			name: "invalid_utf8_output",
			run: func() ParserHint {
				return mapParseOutputBytes([]byte{0xff, 0xfe, 0xfd}, "go", []string{"func main() {}"})
			},
		},
		{
			name: "parse_output_mismatch",
			run: func() ParserHint {
				return mapParseOutput("(source_file\n  (comment))", "go", []string{"// comment"})
			},
		},
	}

	denominator := len(cases)
	if denominator != 6 {
		t.Fatalf("fallback corpus drift: got %d cases, want 6", denominator)
	}

	numerator := 0
	for _, tc := range cases {
		hint := tc.run()
		if !hint.OK && hint.Kind == "" && hint.Label == "" {
			numerator++
			continue
		}
		t.Logf("%s: expected deterministic fallback, got %+v", tc.name, hint)
	}

	correctness := float64(numerator) / float64(denominator)
	t.Logf(
		"fallback correctness KPI: %d/%d (%.2f)",
		numerator, denominator, correctness,
	)
	if correctness != 1.0 {
		t.Fatalf(
			"fallback correctness KPI failed: got %d/%d (%.2f), want 1.00",
			numerator, denominator, correctness,
		)
	}
}

func TestTreeSitterCLI_BoundedAttempts(t *testing.T) {
	old := parseChunkForEnrichment
	calls := 0
	parseChunkForEnrichment = func(_ context.Context, _ string, _ []string, _ time.Duration) ParserHint {
		calls++
		return ParserHint{Kind: KindFunction, Label: "Stub", OK: true}
	}
	defer func() { parseChunkForEnrichment = old }()

	// Create more chunks than maxParserAttempts
	chunks := make([]ChunkInput, 10)
	for i := range chunks {
		chunks[i] = ChunkInput{
			Index:   i,
			Content: "@@ -1,1 +1,2 @@\n+func f() {}",
		}
	}

	hints := TryEnrichChunks(context.Background(), ".go", chunks, time.Second)

	if len(hints) != 10 {
		t.Fatalf("expected 10 hints (one per chunk), got %d", len(hints))
	}

	if calls != maxParserAttempts {
		t.Fatalf("parser called %d times, want %d", calls, maxParserAttempts)
	}

	for i := 0; i < maxParserAttempts; i++ {
		if !hints[i].OK {
			t.Fatalf("hint[%d] should be parser-populated", i)
		}
	}
	for i := maxParserAttempts; i < len(hints); i++ {
		if hints[i].OK || hints[i].Kind != "" || hints[i].Label != "" {
			t.Fatalf("hint[%d] should remain fallback zero-value, got %+v", i, hints[i])
		}
	}
}

func TestTreeSitterCLI_MapParseOutput(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		lang         string
		changedLines []string
		want         ParserHint
	}{
		{
			"go import",
			"(source_file\n  (import_declaration\n    path: (import_spec_list)))",
			"go",
			nil,
			ParserHint{Kind: KindImport, Label: "", OK: true},
		},
		{
			"go function no positions",
			"(source_file\n  (function_declaration\n    name: (identifier)))",
			"go",
			nil,
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"go method no positions",
			"(source_file\n  (method_declaration\n    name: (field_identifier)))",
			"go",
			nil,
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"dart import",
			"(program\n  (import_or_export\n    (import_specification)))",
			"dart",
			nil,
			ParserHint{Kind: KindImport, Label: "", OK: true},
		},
		{
			"dart function no positions",
			"(program\n  (function_signature\n    name: (identifier)))",
			"dart",
			nil,
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"dart method declaration no positions",
			"(program\n  (method_declaration\n    name: (identifier)))",
			"dart",
			nil,
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"empty output",
			"",
			"go",
			nil,
			ParserHint{},
		},
		{
			"no recognized nodes",
			"(source_file\n  (comment))",
			"go",
			nil,
			ParserHint{},
		},
		// Identifier extraction from positions:
		{
			"go function with identifier position",
			"(source_file [0, 0] - [1, 0]\n  (function_declaration [0, 0] - [0, 25]\n    name: (identifier) [0, 5] - [0, 18])",
			"go",
			[]string{"func HandleRequest() {"},
			ParserHint{Kind: KindFunction, Label: "HandleRequest", OK: true},
		},
		{
			"go function position out of range",
			"(source_file [0, 0] - [1, 0]\n  (function_declaration [0, 0] - [0, 10]\n    name: (identifier) [0, 5] - [0, 99])",
			"go",
			[]string{"func F() {"},
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"go method receiver blocks extraction",
			"(source_file [0, 0] - [1, 0]\n  (method_declaration [0, 0] - [0, 30]\n    receiver: (parameter_list [0, 5] - [0, 17])))",
			"go",
			[]string{"func (s *Server) Handle() {"},
			ParserHint{Kind: KindFunction, Label: "", OK: true},
		},
		{
			"dart function with nested identifier",
			"(program [0, 0] - [1, 0]\n  (method_declaration [0, 0] - [0, 38]\n    (function_signature [0, 7] - [0, 36]\n      name: (identifier) [0, 7] - [0, 16])))",
			"dart",
			[]string{"Widget buildCard(BuildContext c) {"},
			ParserHint{Kind: KindFunction, Label: "buildCard", OK: true},
		},
		{
			"go field_identifier with positions",
			"(source_file [0, 0] - [1, 0]\n  (function_declaration [0, 0] - [0, 20]\n    name: (field_identifier) [0, 5] - [0, 11])",
			"go",
			[]string{"func Foobar() {}"},
			ParserHint{Kind: KindFunction, Label: "Foobar", OK: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapParseOutput(tt.output, tt.lang, tt.changedLines)
			if got.OK != tt.want.OK {
				t.Errorf("OK: got %v, want %v", got.OK, tt.want.OK)
			}
			if got.Kind != tt.want.Kind {
				t.Errorf("Kind: got %v, want %v", got.Kind, tt.want.Kind)
			}
			if got.Label != tt.want.Label {
				t.Errorf("Label: got %q, want %q", got.Label, tt.want.Label)
			}
		})
	}
}

func TestExtractNodeType(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"(source_file", "source_file"},
		{"  (import_declaration", "import_declaration"},
		{"  (function_declaration name: (identifier))", "function_declaration"},
		{"no parens", ""},
		{"()", ""},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got := extractNodeType(tt.line)
			if got != tt.want {
				t.Errorf("extractNodeType(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestIsSupportedExtension(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"lib/widget.dart", true},
		{"script.py", false},
		{"README.md", false},
		{"Makefile", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsSupportedExtension(tt.path)
			if got != tt.want {
				t.Errorf("IsSupportedExtension(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsCleanLabel(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"HandleRequest", true},
		{"", true},
		{" leading", false},
		{"trailing ", false},
		{"has\ttab", false},
		{"has\nnewline", false},
		{"has\x00null", false},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := isCleanLabel(tt.label)
			if got != tt.want {
				t.Errorf("isCleanLabel(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

func TestTreeSitterCLI_MapParseOutputBytes_InvalidUTF8(t *testing.T) {
	output := []byte{0xff, 0xfe, 0xfd}
	hint := mapParseOutputBytes(output, "go", []string{"func main() {}"})
	if hint.OK {
		t.Fatal("expected OK=false for invalid UTF-8 output")
	}
	if hint.Kind != "" {
		t.Fatalf("expected empty kind for invalid UTF-8 output, got %q", hint.Kind)
	}
	if hint.Label != "" {
		t.Fatalf("expected empty label for invalid UTF-8 output, got %q", hint.Label)
	}
}
