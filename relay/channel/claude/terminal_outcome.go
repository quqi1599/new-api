package claude

import (
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/tidwall/gjson"
)

// Inspect only parsed upstream frames, never request data or error text. Keep
// the public stop_reason unchanged while recording protocol-neutral completion.
func observeClaudeTerminal(info *relaycommon.RelayInfo, ci *ClaudeResponseInfo, response *dto.ClaudeResponse, data string) {
	if info == nil || ci == nil || response == nil {
		return
	}
	stop := response.StopReason
	if response.Delta != nil && response.Delta.StopReason != nil {
		stop = *response.Delta.StopReason
	}
	root := gjson.Parse(data)
	if stop != "" {
		state, reason := "", ""
		switch stop {
		case "end_turn", "stop_sequence", "tool_use":
			state, reason = "completed", ""
		case "max_tokens":
			reason = "max_output_tokens"
		case "refusal":
			reason = "content_filter"
		case "incomplete":
			state, reason = "incomplete", "unknown"
		}
		if stop == "max_tokens" || stop == "refusal" {
			state = "incomplete"
		}
		if state != "" && !info.IsResponsesIncomplete() {
			info.ResponsesOutcome = &relaycommon.ResponsesOutcome{State: state, Reason: reason, InputTokensSource: "unreported", OutputTokensSource: "unreported"}
		}
	}
	if root.Get("cpa_terminal.status").String() == "incomplete" {
		reason := root.Get("cpa_terminal.reason").String()
		if reason != "max_output_tokens" && reason != "content_filter" {
			reason = "unknown"
		}
		info.ResponsesOutcome = &relaycommon.ResponsesOutcome{State: "incomplete", Reason: reason, InputTokensSource: "unreported", OutputTokensSource: "unreported"}
	}
	if ci.Usage == nil {
		ci.Usage = &dto.Usage{}
	}
	prefix := "usage."
	if response.Type == "message_start" {
		prefix = "message.usage."
	}
	// CPA's bridge starts Messages with required placeholder zeros. Its exact
	// terminal marker is authoritative when those fields were never reported.
	if root.Get("cpa_usage.input_reported").Type == gjson.False {
		ci.inputReported, ci.inputEstimated = false, false
		ci.Usage.PromptTokens = 0
	}
	if root.Get("cpa_usage.output_reported").Type == gjson.False {
		ci.outputReported, ci.outputEstimated = false, false
		ci.Usage.CompletionTokens = 0
	}
	if input := root.Get(prefix + "input_tokens"); input.Type == gjson.Number && root.Get("cpa_usage.input_reported").Type != gjson.False {
		ci.Usage.PromptTokens, ci.inputReported = int(input.Int()), true
		ci.inputEstimated = false
	}
	if output := root.Get(prefix + "output_tokens"); output.Type == gjson.Number && root.Get("cpa_usage.output_reported").Type != gjson.False {
		ci.Usage.CompletionTokens, ci.outputReported = int(output.Int()), true
		ci.outputEstimated = false
	}
	ci.Usage.TotalTokens = ci.Usage.PromptTokens + ci.Usage.CompletionTokens
	recordClaudeUsageSources(info, ci)
}

func recordClaudeUsageSources(info *relaycommon.RelayInfo, ci *ClaudeResponseInfo) {
	if info == nil || ci == nil {
		return
	}
	for _, outcome := range []*relaycommon.ResponsesOutcome{info.ResponsesOutcome, info.GenerationOutcome} {
		if outcome == nil {
			continue
		}
		if ci.inputEstimated {
			outcome.InputTokensSource = "estimated"
		} else if ci.inputReported {
			outcome.InputTokensSource = "reported"
		}
		if ci.outputEstimated {
			outcome.OutputTokensSource = "estimated"
		} else if ci.outputReported {
			outcome.OutputTokensSource = "reported"
		}
	}
}
