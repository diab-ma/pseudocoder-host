package server

// filterProviderItems removes assistant messages that appear before the
// first user message in the timeline. This filters system prompt dumps
// (e.g. Codex emitting AGENTS.md as an assistant-role response_item).
// The filter is idempotent: filtering already-filtered items is a no-op.
func filterProviderItems(items []ChatItem) []ChatItem {
	filtered := make([]ChatItem, 0, len(items))
	seenUser := false
	for _, item := range items {
		if item.Kind == ChatItemKindUserMessage {
			seenUser = true
			filtered = append(filtered, item)
			continue
		}
		if !seenUser && item.Kind == ChatItemKindAssistant {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}
