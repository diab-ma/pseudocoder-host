package main

import (
	"testing"
	"time"

	"github.com/pseudocoder/host/internal/diff"
	"github.com/pseudocoder/host/internal/server"
	"github.com/pseudocoder/host/internal/storage"
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

	msg := buildReconnectDiffCardMessage(card)

	if msg.Type != server.MessageTypeDiffCard {
		t.Fatalf("expected diff.card message, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(server.DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if payload.IsBinary {
		t.Fatal("non-binary card should not set is_binary")
	}
	if payload.Stats == nil {
		t.Fatal("expected stats for non-binary reconnect card")
	}
	if len(payload.SemanticGroups) == 0 {
		t.Fatal("expected semantic_groups in reconnect payload")
	}
	if payload.SemanticGroups[0].Kind == "" {
		t.Fatal("expected semantic group kind")
	}
	if payload.Summary == "" {
		t.Fatal("expected non-empty summary for non-binary reconnect card")
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

	msg := buildReconnectDiffCardMessage(card)

	if msg.Type != server.MessageTypeDiffCard {
		t.Fatalf("expected diff.card message, got %s", msg.Type)
	}

	payload, ok := msg.Payload.(server.DiffCardPayload)
	if !ok {
		t.Fatalf("expected DiffCardPayload, got %T", msg.Payload)
	}

	if !payload.IsBinary {
		t.Fatal("expected binary reconnect card")
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
