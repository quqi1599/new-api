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
	if info == nil {
		return true
	}
	if info.ManagedWeb != nil && info.ManagedWeb.ModelRounds == 0 {
		return false
	}
	info.FinalizeGenerationOutcome()
	return info.GenerationOutcome == nil || info.GenerationOutcome.State == "completed"
}

// Call after stream scanning and final downstream writes have stopped. Delivery
// errors override a prior model terminal, but retain its usage provenance.
func (info *RelayInfo) FinalizeGenerationOutcome() {
	if info == nil {
		return
	}
	defer func() {
		if info.ManagedWeb != nil && (info.ManagedWeb.Outcome == "failed" || info.ManagedWeb.Outcome == "cancelled") {
			if info.GenerationOutcome == nil {
				info.GenerationOutcome = &ResponsesOutcome{InputTokensSource: "unreported", OutputTokensSource: "unreported"}
			}
			info.GenerationOutcome.State = info.ManagedWeb.Outcome
			info.GenerationOutcome.Reason = "managed_web_request_" + info.ManagedWeb.Outcome
		}
	}()
	if info.ResponsesOutcome != nil {
		outcome := *info.ResponsesOutcome
		info.GenerationOutcome = &outcome
	}
	if !info.IsStream || info.StreamStatus == nil {
		return
	}
	ss := info.StreamStatus
	if ss.IsNormalEnd() && !ss.HasErrors() {
		return
	}
	if ss.EndReason == StreamEndReasonNone {
		return
	}
	state, reason := "failed", string(ss.EndReason)
	if ss.EndReason == StreamEndReasonClientGone {
		state, reason = "cancelled", "client_gone"
	}
	if info.GenerationOutcome == nil {
		info.GenerationOutcome = &ResponsesOutcome{InputTokensSource: "unreported", OutputTokensSource: "unreported"}
	}
	info.GenerationOutcome.State, info.GenerationOutcome.Reason = state, reason
}
