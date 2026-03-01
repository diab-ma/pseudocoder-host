package semantic

import (
	"sort"
	"testing"
)

func TestAnalyze_GoImport(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-1",
		FilePath: "host/internal/server/handler.go",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,3 +1,5 @@\n+import \"fmt\"\n+import \"os\"\n context line",
				NewStart: 1, NewCount: 5,
				OldStart: 1, OldCount: 3,
			},
		},
	}

	out := Analyze(input)

	if len(out.Chunks) != 1 {
		t.Fatalf("expected 1 chunk annotation, got %d", len(out.Chunks))
	}
	if out.Chunks[0].Kind != KindImport {
		t.Errorf("expected kind import, got %s", out.Chunks[0].Kind)
	}
	if out.Chunks[0].Label != "Import" {
		t.Errorf("expected label Import, got %s", out.Chunks[0].Label)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(out.Groups))
	}
	if out.Groups[0].Kind != KindImport {
		t.Errorf("expected group kind import, got %s", out.Groups[0].Kind)
	}
}

func TestAnalyze_GoFunction(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-2",
		FilePath: "host/internal/server/handler.go",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -10,5 +10,8 @@\n+func HandleRequest(\n+\tctx context.Context,\n+) error {\n context line",
				NewStart: 10, NewCount: 8,
				OldStart: 10, OldCount: 5,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindFunction {
		t.Errorf("expected kind function, got %s", out.Chunks[0].Kind)
	}
	if out.Chunks[0].Label != "HandleRequest" {
		t.Errorf("expected label HandleRequest, got %s", out.Chunks[0].Label)
	}
}

func TestAnalyze_DartImport(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-3",
		FilePath: "mobile/lib/widgets/card.dart",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,2 +1,3 @@\n+import 'package:flutter/material.dart';\n context",
				NewStart: 1, NewCount: 3,
				OldStart: 1, OldCount: 2,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindImport {
		t.Errorf("expected kind import, got %s", out.Chunks[0].Kind)
	}
}

func TestAnalyze_DartFunction(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-4",
		FilePath: "mobile/lib/widgets/card.dart",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -5,3 +5,6 @@\n+Widget buildCard(BuildContext context) {\n+  return Container();\n+}",
				NewStart: 5, NewCount: 6,
				OldStart: 5, OldCount: 3,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindFunction {
		t.Errorf("expected kind function, got %s", out.Chunks[0].Kind)
	}
	if out.Chunks[0].Label != "buildCard" {
		t.Errorf("expected label buildCard, got %s", out.Chunks[0].Label)
	}
}

func TestAnalyze_DartArrowFunction(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-5",
		FilePath: "mobile/lib/utils.dart",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,2 +1,3 @@\n+String greeting() =>\n+  'hello';",
				NewStart: 1, NewCount: 3,
				OldStart: 1, OldCount: 2,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindFunction {
		t.Errorf("expected kind function, got %s", out.Chunks[0].Kind)
	}
	if out.Chunks[0].Label != "greeting" {
		t.Errorf("expected label greeting, got %s", out.Chunks[0].Label)
	}
}

func TestAnalyze_SupportedLanguageCoverageKPI(t *testing.T) {
	buildInput := func(cardID, filePath, content string) AnalyzerInput {
		return AnalyzerInput{
			CardID:   cardID,
			FilePath: filePath,
			Chunks: []ChunkInput{
				{
					Index:    0,
					Content:  content,
					OldStart: 1, OldCount: 1,
					NewStart: 1, NewCount: 2,
				},
			},
		}
	}

	cases := []struct {
		name      string
		input     AnalyzerInput
		wantKind  SemanticKind
		wantLabel string
	}{
		{
			name: "go_import_basic",
			input: buildInput(
				"c4-go-import",
				"host/internal/server/handler.go",
				"@@ -1,1 +1,2 @@\n+import \"fmt\"",
			),
			wantKind:  KindImport,
			wantLabel: "Import",
		},
		{
			name: "go_function_basic",
			input: buildInput(
				"c4-go-function",
				"host/internal/server/handler.go",
				"@@ -10,1 +10,2 @@\n+func HandleRequest(ctx context.Context) error {",
			),
			wantKind:  KindFunction,
			wantLabel: "HandleRequest",
		},
		{
			name: "go_generic_basic",
			input: buildInput(
				"c4-go-generic",
				"host/internal/server/handler.go",
				"@@ -20,1 +20,2 @@\n+// generic change",
			),
			wantKind:  KindGeneric,
			wantLabel: "Changes",
		},
		{
			name: "dart_import_basic",
			input: buildInput(
				"c4-dart-import",
				"mobile/lib/widgets/card.dart",
				"@@ -1,1 +1,2 @@\n+import 'package:flutter/material.dart';",
			),
			wantKind:  KindImport,
			wantLabel: "Import",
		},
		{
			name: "dart_function_basic",
			input: buildInput(
				"c4-dart-function",
				"mobile/lib/widgets/card.dart",
				"@@ -5,1 +5,2 @@\n+Widget buildCard(BuildContext context) {",
			),
			wantKind:  KindFunction,
			wantLabel: "buildCard",
		},
		{
			name: "dart_generic_basic",
			input: buildInput(
				"c4-dart-generic",
				"mobile/lib/widgets/card.dart",
				"@@ -12,1 +12,2 @@\n+// generic change",
			),
			wantKind:  KindGeneric,
			wantLabel: "Changes",
		},
	}

	denominator := len(cases)
	if denominator != 6 {
		t.Fatalf("supported-language corpus drift: got %d cases, want 6", denominator)
	}

	numerator := 0
	for _, tc := range cases {
		out := Analyze(tc.input)
		if len(out.Chunks) != 1 {
			t.Fatalf("%s: expected exactly 1 chunk annotation, got %d", tc.name, len(out.Chunks))
		}
		got := out.Chunks[0]
		if got.Kind == tc.wantKind && got.Label == tc.wantLabel {
			numerator++
			continue
		}
		t.Logf(
			"%s: mismatch got kind=%q label=%q, want kind=%q label=%q",
			tc.name, got.Kind, got.Label, tc.wantKind, tc.wantLabel,
		)
	}

	coverage := float64(numerator) / float64(denominator)
	t.Logf(
		"supported-language semantic labeling KPI coverage: %d/%d (%.2f)",
		numerator, denominator, coverage,
	)
	if coverage < 0.80 {
		t.Fatalf(
			"supported-language semantic labeling KPI below threshold: got %d/%d (%.2f), want >= 0.80",
			numerator, denominator, coverage,
		)
	}
}

func TestAnalyze_UnsupportedLanguageBaseline(t *testing.T) {
	// Python file should still get baseline heuristics
	input := AnalyzerInput{
		CardID:   "card-6",
		FilePath: "scripts/deploy.py",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,3 +1,4 @@\n+from os import path\n context",
				NewStart: 1, NewCount: 4,
				OldStart: 1, OldCount: 3,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindImport {
		t.Errorf("expected kind import for python, got %s", out.Chunks[0].Kind)
	}
}

func TestAnalyze_GenericFallback(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-7",
		FilePath: "README.md",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,2 +1,3 @@\n+# Title\n context",
				NewStart: 1, NewCount: 3,
				OldStart: 1, OldCount: 2,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindGeneric {
		t.Errorf("expected kind generic, got %s", out.Chunks[0].Kind)
	}
	if out.Chunks[0].Label != "Changes" {
		t.Errorf("expected label Changes, got %s", out.Chunks[0].Label)
	}
}

func TestAnalyze_BinaryCard(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-8",
		FilePath: "assets/logo.png",
		IsBinary: true,
		Chunks: []ChunkInput{
			{Index: 0, Content: "Binary file changed"},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindBinary {
		t.Errorf("expected kind binary, got %s", out.Chunks[0].Kind)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(out.Groups))
	}
	if out.Groups[0].Kind != KindBinary {
		t.Errorf("expected group kind binary, got %s", out.Groups[0].Kind)
	}
}

func TestAnalyze_DeletedCard(t *testing.T) {
	input := AnalyzerInput{
		CardID:    "card-9",
		FilePath:  "old_file.go",
		IsDeleted: true,
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,10 +0,0 @@\n-package old\n-func OldFunc() {}",
				OldStart: 1, OldCount: 10,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindDeleted {
		t.Errorf("expected kind deleted, got %s", out.Chunks[0].Kind)
	}
}

func TestAnalyze_DeterministicGrouping(t *testing.T) {
	input := AnalyzerInput{
		CardID:   "card-10",
		FilePath: "main.go",
		Chunks: []ChunkInput{
			{
				Index: 0, Content: "@@ -1,3 +1,5 @@\n+import \"fmt\"\n+import \"os\"",
				NewStart: 1, NewCount: 5, OldStart: 1, OldCount: 3,
				ContentHash: "hash-a",
			},
			{
				Index: 1, Content: "@@ -10,3 +12,5 @@\n+import \"io\"",
				NewStart: 12, NewCount: 5, OldStart: 10, OldCount: 3,
				ContentHash: "hash-b",
			},
			{
				Index: 2, Content: "@@ -20,5 +24,8 @@\n+func Foo() {",
				NewStart: 24, NewCount: 8, OldStart: 20, OldCount: 5,
				ContentHash: "hash-c",
			},
		},
	}

	out1 := Analyze(input)
	out2 := Analyze(input)

	// Same inputs produce same group IDs
	if len(out1.Groups) != len(out2.Groups) {
		t.Fatalf("group count mismatch: %d vs %d", len(out1.Groups), len(out2.Groups))
	}
	for i := range out1.Groups {
		if out1.Groups[i].GroupID != out2.Groups[i].GroupID {
			t.Errorf("group %d ID mismatch: %s vs %s", i, out1.Groups[i].GroupID, out2.Groups[i].GroupID)
		}
	}

	// Verify groups: two import chunks group together, function is separate
	if len(out1.Groups) != 2 {
		t.Fatalf("expected 2 groups (import + function), got %d", len(out1.Groups))
	}

	importGroup := out1.Groups[0]
	funcGroup := out1.Groups[1]

	if importGroup.Kind != KindImport {
		t.Errorf("expected first group kind import, got %s", importGroup.Kind)
	}
	if len(importGroup.ChunkIndexes) != 2 {
		t.Errorf("expected 2 chunks in import group, got %d", len(importGroup.ChunkIndexes))
	}
	if funcGroup.Kind != KindFunction {
		t.Errorf("expected second group kind function, got %s", funcGroup.Kind)
	}
	if len(funcGroup.ChunkIndexes) != 1 {
		t.Errorf("expected 1 chunk in function group, got %d", len(funcGroup.ChunkIndexes))
	}

	// Chunk indexes must be ascending
	if !sort.IntsAreSorted(importGroup.ChunkIndexes) {
		t.Errorf("import group chunk_indexes not ascending: %v", importGroup.ChunkIndexes)
	}
}

func TestAnalyze_IDStability_OrderIndependent(t *testing.T) {
	// Two chunks of the same kind - reorder and verify same group ID
	chunksA := []ChunkInput{
		{Index: 0, Content: "@@ -1,3 +1,5 @@\n+import \"a\"", NewStart: 1, NewCount: 5, OldStart: 1, OldCount: 3, ContentHash: "h1"},
		{Index: 1, Content: "@@ -10,3 +12,5 @@\n+import \"b\"", NewStart: 12, NewCount: 5, OldStart: 10, OldCount: 3, ContentHash: "h2"},
	}
	chunksB := []ChunkInput{
		{Index: 0, Content: "@@ -10,3 +12,5 @@\n+import \"b\"", NewStart: 12, NewCount: 5, OldStart: 10, OldCount: 3, ContentHash: "h2"},
		{Index: 1, Content: "@@ -1,3 +1,5 @@\n+import \"a\"", NewStart: 1, NewCount: 5, OldStart: 1, OldCount: 3, ContentHash: "h1"},
	}

	outA := Analyze(AnalyzerInput{CardID: "card-x", FilePath: "f.go", Chunks: chunksA})
	outB := Analyze(AnalyzerInput{CardID: "card-x", FilePath: "f.go", Chunks: chunksB})

	if len(outA.Groups) != 1 || len(outB.Groups) != 1 {
		t.Fatalf("expected 1 group each, got %d and %d", len(outA.Groups), len(outB.Groups))
	}
	if outA.Groups[0].GroupID != outB.Groups[0].GroupID {
		t.Errorf("group IDs differ for reordered input: %s vs %s", outA.Groups[0].GroupID, outB.Groups[0].GroupID)
	}
}

func TestAnalyze_RiskLevels(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		kind     SemanticKind
		wantRisk string
	}{
		{"sensitive+deleted=critical", "config/auth/secret.go", KindDeleted, "critical"},
		{"sensitive+binary=critical", "assets/token.bin", KindBinary, "critical"},
		{"deleted=high", "old_file.go", KindDeleted, "high"},
		{"binary=high", "image.png", KindBinary, "high"},
		{"sensitive+function=high", "auth/handler.go", KindFunction, "high"},
		{"function=medium", "server/handler.go", KindFunction, "medium"},
		{"sensitive+import=medium", "auth/imports.go", KindImport, "medium"},
		{"sensitive+generic=medium", "auth/config.go", KindGeneric, "medium"},
		{"import=low", "utils/helper.go", KindImport, "low"},
		{"generic=low", "README.md", KindGeneric, "low"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeGroupRisk(tt.kind, tt.filePath)
			if got != tt.wantRisk {
				t.Errorf("computeGroupRisk(%s, %q) = %s, want %s", tt.kind, tt.filePath, got, tt.wantRisk)
			}
		})
	}
}

func TestExtractChangedLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			"basic add/remove",
			"@@ -1,3 +1,4 @@\n+added\n-removed\n context\n",
			[]string{"added", "removed"},
		},
		{
			"skips file headers",
			"--- a/file.go\n+++ b/file.go\n+real change\n",
			[]string{"real change"},
		},
		{
			"empty content",
			"",
			nil,
		},
		{
			"only context lines",
			"@@ -1,3 +1,3 @@\n context1\n context2\n",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChangedLines(tt.content)
			if len(got) != len(tt.want) {
				t.Fatalf("extractChangedLines: got %d lines, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestAnalyze_ImportPrecedenceOverFunction(t *testing.T) {
	// If a chunk has both an import and a function, import wins (higher precedence)
	input := AnalyzerInput{
		CardID:   "card-prec",
		FilePath: "main.go",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -1,3 +1,5 @@\n+import \"fmt\"\n+func init() {",
				NewStart: 1, NewCount: 5, OldStart: 1, OldCount: 3,
			},
		},
	}

	out := Analyze(input)

	if out.Chunks[0].Kind != KindImport {
		t.Errorf("import should have precedence over function, got %s", out.Chunks[0].Kind)
	}
}

func TestApplyParserHint(t *testing.T) {
	tests := []struct {
		name          string
		baselineKind  SemanticKind
		baselineLabel string
		hint          ParserHint
		wantKind      SemanticKind
		wantLabel     string
	}{
		{
			"parser label overwrites baseline fallback",
			KindGeneric, "Changes",
			ParserHint{Kind: KindFunction, Label: "MyFunc", OK: true},
			KindFunction, "MyFunc",
		},
		{
			"parser label overwrites baseline explicit",
			KindFunction, "OldName",
			ParserHint{Kind: KindFunction, Label: "NewName", OK: true},
			KindFunction, "NewName",
		},
		{
			"no parser label preserves baseline explicit",
			KindFunction, "HandleRequest",
			ParserHint{Kind: KindFunction, Label: "", OK: true},
			KindFunction, "HandleRequest",
		},
		{
			"no parser label applies fallback over baseline fallback",
			KindGeneric, "Changes",
			ParserHint{Kind: KindFunction, Label: "", OK: true},
			KindFunction, "Function",
		},
		{
			"no parser label applies fallback over empty baseline",
			KindGeneric, "",
			ParserHint{Kind: KindImport, Label: "", OK: true},
			KindImport, "Import",
		},
		{
			"parser not OK preserves baseline entirely",
			KindFunction, "HandleRequest",
			ParserHint{Kind: "", Label: "", OK: false},
			KindFunction, "HandleRequest",
		},
		{
			"unclean parser label treated as empty - preserves explicit",
			KindFunction, "HandleRequest",
			ParserHint{Kind: KindFunction, Label: " spaces ", OK: true},
			KindFunction, "HandleRequest",
		},
		{
			"unclean parser label treated as empty - applies fallback",
			KindGeneric, "Changes",
			ParserHint{Kind: KindFunction, Label: "\tnewline\n", OK: true},
			KindFunction, "Function",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ann := ChunkAnnotation{Kind: tt.baselineKind, Label: tt.baselineLabel}
			applyParserHint(&ann, tt.hint)
			if ann.Kind != tt.wantKind {
				t.Errorf("Kind: got %s, want %s", ann.Kind, tt.wantKind)
			}
			if ann.Label != tt.wantLabel {
				t.Errorf("Label: got %q, want %q", ann.Label, tt.wantLabel)
			}
		})
	}
}

func TestIsFallbackLabel(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"Import", true},
		{"Function", true},
		{"Changes", true},
		{"Binary", true},
		{"Deleted", true},
		{"Renamed", true},
		{"HandleRequest", false},
		{"buildCard", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := isFallbackLabel(tt.label)
			if got != tt.want {
				t.Errorf("isFallbackLabel(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

func TestAnalyze_DeletionSafeLineRange(t *testing.T) {
	// Deletion-only chunk (NewCount == 0) should use old-file lines
	input := AnalyzerInput{
		CardID:   "card-del",
		FilePath: "main.go",
		Chunks: []ChunkInput{
			{
				Index:    0,
				Content:  "@@ -5,3 +4,0 @@\n-removed line 1\n-removed line 2\n-removed line 3",
				OldStart: 5, OldCount: 3,
				NewStart: 4, NewCount: 0,
			},
		},
	}

	out := Analyze(input)

	if len(out.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(out.Groups))
	}
	g := out.Groups[0]
	if g.LineStart != 5 || g.LineEnd != 7 {
		t.Errorf("expected line range 5-7 for deletion chunk, got %d-%d", g.LineStart, g.LineEnd)
	}
}
