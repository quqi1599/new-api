package dto

import "github.com/QuantumNous/new-api/common"

// CPATerminal preserves generation state when Responses is translated to Chat.
// This is response metadata, never a client request control.
type CPATerminal struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type CPAUsagePresence struct {
	InputReported  *bool `json:"input_reported,omitempty"`
	OutputReported *bool `json:"output_reported,omitempty"`
}

// ResponsesTerminal separates a valid protocol terminal from transport health.
// Only fixed protocol reasons may enter logs; unknown provider strings may
// contain user content and are deliberately reduced to "unknown".
func ResponsesTerminal(eventType string, response *OpenAIResponsesResponse) (state, reason string) {
	if response != nil {
		_ = common.Unmarshal(response.Status, &state)
	}
	if eventType == "response.incomplete" {
		state = "incomplete"
	} else if state == "" && (eventType == "response.completed" || eventType == "response.done") {
		state = "completed"
	}
	if state != "incomplete" {
		return state, ""
	}
	reason = "unknown"
	if response != nil && response.IncompleteDetails != nil {
		switch response.IncompleteDetails.Reason {
		case "max_output_tokens", "content_filter":
			reason = response.IncompleteDetails.Reason
		}
	}
	return state, reason
}

// ResponsesFinishReason never invents a successful stop for incomplete output.
// Unknown reasons use an explicit extension instead of claiming a token limit.
func ResponsesFinishReason(eventType string, response *OpenAIResponsesResponse, hasTools bool) string {
	state, reason := ResponsesTerminal(eventType, response)
	if state == "incomplete" {
		switch reason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "incomplete"
		}
	}
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}
