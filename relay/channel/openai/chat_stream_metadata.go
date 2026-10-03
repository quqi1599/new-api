package openai

// chatStreamMetadataOnly is deliberately conservative: unknown nonempty delta
// fields retain the existing usage behavior. Audio, refusal and legacy function
// output must not be mistaken for an empty stream just because the text-token
// counter does not understand them.
func chatStreamMetadataOnly(event map[string]any) bool {
	choices, exists := event["choices"]
	if !exists || choices == nil {
		return true
	}
	items, ok := choices.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		choice, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if !emptyChatStreamValue(choice["text"]) {
			return false
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			for key, value := range delta {
				if key != "role" && !emptyChatStreamValue(value) {
					return false
				}
			}
		} else if !emptyChatStreamValue(choice["delta"]) {
			return false
		}
	}
	return true
}

func emptyChatStreamValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}
