package common

import "github.com/QuantumNous/new-api/dto"

// ResponsesOutcome records generation completion independently of successful
// HTTP delivery and billing. Missing usage is not a reported zero.
type ResponsesOutcome struct {
	State              string `json:"state"`
	Reason             string `json:"reason,omitempty"`
	InputTokensSource  string `json:"input_tokens_source"`
	OutputTokensSource string `json:"output_tokens_source"`
}

func (info *RelayInfo) ObserveResponsesTerminal(eventType string, response *dto.OpenAIResponsesResponse) {
	state, reason := dto.ResponsesTerminal(eventType, response)
	if state != "completed" && state != "incomplete" {
		return
	}
	info.ResponsesOutcome = &ResponsesOutcome{State: state, Reason: reason,
		InputTokensSource: "unreported", OutputTokensSource: "unreported"}
}

func (info *RelayInfo) IsResponsesIncomplete() bool {
	return info != nil && info.ResponsesOutcome != nil && info.ResponsesOutcome.State == "incomplete"
}

// A completed relay can terminate without retry while leaving channel health
// unchanged. Incomplete generation is neither recovery evidence nor failure.
func (info *RelayInfo) ShouldRecordChannelSuccess() bool {
	return !info.IsResponsesIncomplete()
}
