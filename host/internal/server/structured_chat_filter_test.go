package server

import (
	"testing"
)

func TestFilterProviderItems(t *testing.T) {
	tests := []struct {
		name     string
		items    []ChatItem
		wantIDs  []string
	}{
		{
			name:    "empty input yields empty output",
			items:   nil,
			wantIDs: nil,
		},
		{
			name: "only assistants are all dropped",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "a2", Kind: ChatItemKindAssistant},
			},
			wantIDs: nil,
		},
		{
			name: "assistant before user is skipped",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "u1", Kind: ChatItemKindUserMessage},
			},
			wantIDs: []string{"u1"},
		},
		{
			name: "multiple assistants before user are all skipped",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "a2", Kind: ChatItemKindAssistant},
				{ID: "a3", Kind: ChatItemKindAssistant},
				{ID: "u1", Kind: ChatItemKindUserMessage},
			},
			wantIDs: []string{"u1"},
		},
		{
			name: "system/tool/thinking before user are preserved",
			items: []ChatItem{
				{ID: "s1", Kind: ChatItemKindSystemEvent},
				{ID: "t1", Kind: ChatItemKindToolCall},
				{ID: "th1", Kind: ChatItemKindThinking},
				{ID: "u1", Kind: ChatItemKindUserMessage},
			},
			wantIDs: []string{"s1", "t1", "th1", "u1"},
		},
		{
			name: "assistant and system events with no user — assistant skipped, system kept",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "s1", Kind: ChatItemKindSystemEvent},
				{ID: "a2", Kind: ChatItemKindAssistant},
			},
			wantIDs: []string{"s1"},
		},
		{
			name: "mixed timeline — only leading assistants affected",
			items: []ChatItem{
				{ID: "s1", Kind: ChatItemKindSystemEvent},
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "u1", Kind: ChatItemKindUserMessage},
				{ID: "a2", Kind: ChatItemKindAssistant},
				{ID: "t1", Kind: ChatItemKindToolCall},
			},
			wantIDs: []string{"s1", "u1", "a2", "t1"},
		},
		{
			name: "assistants before AND after user — only pre-user removed",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "a2", Kind: ChatItemKindAssistant},
				{ID: "u1", Kind: ChatItemKindUserMessage},
				{ID: "a3", Kind: ChatItemKindAssistant},
				{ID: "a4", Kind: ChatItemKindAssistant},
			},
			wantIDs: []string{"u1", "a3", "a4"},
		},
		{
			name: "idempotent — filtering twice yields same result",
			items: []ChatItem{
				{ID: "a1", Kind: ChatItemKindAssistant},
				{ID: "u1", Kind: ChatItemKindUserMessage},
				{ID: "a2", Kind: ChatItemKindAssistant},
			},
			wantIDs: []string{"u1", "a2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterProviderItems(tt.items)
			gotIDs := make([]string, len(got))
			for i, item := range got {
				gotIDs[i] = item.ID
			}

			if len(gotIDs) == 0 && len(tt.wantIDs) == 0 {
				return // both empty
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("len = %d, want %d; got IDs = %v", len(gotIDs), len(tt.wantIDs), gotIDs)
			}
			for i, wantID := range tt.wantIDs {
				if gotIDs[i] != wantID {
					t.Errorf("item[%d] = %q, want %q", i, gotIDs[i], wantID)
				}
			}

			// Verify idempotency for the last test case
			if tt.name == "idempotent — filtering twice yields same result" {
				got2 := filterProviderItems(got)
				if len(got2) != len(got) {
					t.Fatalf("second filter: len = %d, want %d", len(got2), len(got))
				}
				for i := range got {
					if got2[i].ID != got[i].ID {
						t.Errorf("second filter item[%d] = %q, want %q", i, got2[i].ID, got[i].ID)
					}
				}
			}
		})
	}
}
