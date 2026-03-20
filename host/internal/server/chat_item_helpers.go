package server

// findChatItem returns a pointer to the first ChatItem with the given ID, or nil.
func findChatItem(items []ChatItem, itemID string) *ChatItem {
	for i := range items {
		if items[i].ID == itemID {
			return &items[i]
		}
	}
	return nil
}

// cloneChatItems returns a shallow copy of the ChatItem slice.
func cloneChatItems(items []ChatItem) []ChatItem {
	cloned := make([]ChatItem, len(items))
	copy(cloned, items)
	return cloned
}

// chatItemsEqual returns true if two ChatItem slices have identical elements.
func chatItemsEqual(left, right []ChatItem) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// removeChatItemByID returns a new slice with the item matching itemID removed.
func removeChatItemByID(items []ChatItem, itemID string) []ChatItem {
	filtered := items[:0]
	for _, item := range items {
		if item.ID == itemID {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}
