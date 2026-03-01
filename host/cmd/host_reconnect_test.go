package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/server"
	"github.com/pseudocoder/host/internal/storage"
	"github.com/pseudocoder/host/internal/stream"
)

func TestBuildReconnectDiffCardMessage_NonBinaryIncludesSemanticMetadata(t *testing.T) {
	card := &storage.ReviewCard{
		ID:        "card-reconnect-nonbinary",
		SessionID: "session-1",
		File:      "handler.go",
		Diff:      "@@ -10,2 +10,4 @@\n+func HandleRequest() {\n+  return nil\n+}\n context",
		Status:    storage.CardPending,
		CreatedAt: time.UnixMilli(1703500000000),
	}

	var reconciledCardID string
	var reconciledChunks []stream.ChunkInfo
	msg := buildReconnectDiffCardMessage(
		card,
		false,
		0,
		func(cardID string, chunks []stream.ChunkInfo) {
			reconciledCardID = cardID
			reconciledChunks = chunks
		},
	)

	if msg.Type != server.MessageTypeDiffCard {
		t.Fatalf("expected diff.card message, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(server.DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if reconciledCardID != card.ID {
		t.Fatalf("reconcile called with cardID %q, want %q", reconciledCardID, card.ID)
	}
	if len(reconciledChunks) == 0 {
		t.Fatal("expected reconcile callback to receive parsed chunks")
	}

	if payload.IsBinary {
		t.Fatal("non-binary card should not set is_binary")
	}
	if payload.Stats == nil {
		t.Fatal("expected stats for non-binary reconnect card")
	}
	if len(payload.Chunks) == 0 {
		t.Fatal("expected chunks in reconnect payload")
	}
	if payload.Chunks[0].SemanticKind == "" {
		t.Fatal("expected semantic_kind on reconnect chunk")
	}
	if payload.Chunks[0].SemanticLabel == "" {
		t.Fatal("expected semantic_label on reconnect chunk")
	}
	if payload.Chunks[0].SemanticGroupID == "" {
		t.Fatal("expected semantic_group_id on reconnect chunk")
	}
	if len(payload.SemanticGroups) == 0 {
		t.Fatal("expected semantic_groups in reconnect payload")
	}
	if payload.SemanticGroups[0].Kind == "" {
		t.Fatal("expected semantic group kind")
	}
	if !strings.Contains(payload.Summary, "chunk(s)") {
		t.Fatalf("expected semantic summary format with chunk count, got %q", payload.Summary)
	}
}

func TestBuildReconnectDiffCardMessage_BinaryIncludesSemanticGroups(t *testing.T) {
	card := &storage.ReviewCard{
		ID:        "card-reconnect-binary",
		SessionID: "session-1",
		File:      "logo.png",
		Diff:      diff.BinaryDiffPlaceholder,
		Status:    storage.CardPending,
		CreatedAt: time.UnixMilli(1703500000000),
	}

	reconcileCalled := false
	msg := buildReconnectDiffCardMessage(
		card,
		false,
		0,
		func(string, []stream.ChunkInfo) { reconcileCalled = true },
	)

	if msg.Type != server.MessageTypeDiffCard {
		t.Fatalf("expected diff.card message, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(server.DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if reconcileCalled {
		t.Fatal("reconcile callback should not be used for binary reconnect cards")
	}
	if !payload.IsBinary {
		t.Fatal("expected binary reconnect card")
	}
	if len(payload.Chunks) != 0 {
		t.Fatalf("binary reconnect card should not include chunks, got %d", len(payload.Chunks))
	}
	if len(payload.SemanticGroups) == 0 {
		t.Fatal("binary reconnect card should include semantic_groups")
	}
	if payload.SemanticGroups[0].Kind != "binary" {
		t.Fatalf("expected binary semantic group kind, got %q", payload.SemanticGroups[0].Kind)
	}
	if payload.Summary == "" {
		t.Fatal("expected summary for binary reconnect card")
	}
}
