package openai

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Adapted from upstream #7427. Explicit failures retain their refund policy;
// incomplete terminals retain only reported usage. Estimate other missing fields
// only from generated output, never merely from response.created metadata.
type responsesUsageEstimate struct {
	output             strings.Builder
	promptReported     bool
	completionReported bool
	// Converted Chat routes historically use the model heuristic; preserve
	// that pricing behavior while sharing terminal and field-presence rules.
	useModelEstimate bool
}

func (e *responsesUsageEstimate) observe(event *dto.ResponsesStreamResponse, data string, usage *dto.Usage) {
	switch event.Type {
	case "response.output_text.delta", "response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.custom_tool_call_input.delta":
		e.output.WriteString(event.Delta)
	}
	if event.Response != nil {
		e.observeResponse(event.Response, data, "response.", usage)
	}
}

func (e *responsesUsageEstimate) observeResponse(response *dto.OpenAIResponsesResponse, data, prefix string, usage *dto.Usage) {
	if response == nil {
		return
	}
	if response.Usage != nil {
		// A later partial snapshot must not erase earlier reported cache or
		// token fields. Use the same per-field merge as Chat usage trailers.
		var fields map[string]json.RawMessage
		if common.UnmarshalJsonStr(gjson.Get(data, prefix+"usage").Raw, &fields) == nil {
			for from, to := range map[string]string{"input_tokens": "prompt_tokens", "output_tokens": "completion_tokens", "input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
				if raw, ok := fields[from]; ok {
					fields[to] = raw
					delete(fields, from)
				}
			}
			mergeChatUsageFields(usage, fields)
		}
		if gjson.Get(data, prefix+"usage.input_tokens").Type == gjson.Number {
			e.promptReported = true
		}
		if gjson.Get(data, prefix+"usage.output_tokens").Type == gjson.Number {
			e.completionReported = true
		}
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.InputTokens, usage.OutputTokens = usage.PromptTokens, usage.CompletionTokens
	if e.completionReported || e.output.Len() != 0 {
		return // terminal snapshots must not count the preceding deltas twice
	}
	for _, item := range response.Output {
		switch item.Type {
		case "function_call", "custom_tool_call":
			e.output.WriteString(item.ArgumentsString())
		case "reasoning":
			for _, part := range item.Summary {
				e.output.WriteString(part.Text)
			}
		case "message":
			if item.Role != "" && item.Role != "assistant" {
				continue
			}
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					e.output.WriteString(part.Text)
				case "refusal":
					e.output.WriteString(part.Refusal)
				}
			}
		}
	}
}

func (e *responsesUsageEstimate) recordSources(info *relaycommon.RelayInfo, inputEstimated, outputEstimated bool) {
	if info.ResponsesOutcome == nil {
		return
	}
	if e.promptReported {
		info.ResponsesOutcome.InputTokensSource = "reported"
	} else if inputEstimated {
		info.ResponsesOutcome.InputTokensSource = "estimated"
	}
	if e.completionReported {
		info.ResponsesOutcome.OutputTokensSource = "reported"
	} else if outputEstimated {
		info.ResponsesOutcome.OutputTokensSource = "estimated"
	}
}

// Called only after explicit protocol failures have taken the existing refund
// path. Local estimates are observable using the existing log/UI marker.
func (e *responsesUsageEstimate) finish(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	// Incomplete output can be used and charged when usage was actually
	// reported. An absent field remains unknown; do not invent a full prompt
	// charge or an output estimate for a generation that did not finish.
	inputEstimated, outputEstimated := false, false
	if !info.IsResponsesIncomplete() && !e.completionReported && e.output.Len() > 0 {
		if e.useModelEstimate {
			usage.CompletionTokens = service.EstimateTokenByModel(info.UpstreamModelName, e.output.String())
		} else {
			usage.CompletionTokens = service.CountTextToken(e.output.String(), info.UpstreamModelName)
		}
		outputEstimated = usage.CompletionTokens > 0
	}
	if !info.IsResponsesIncomplete() && !e.promptReported && usage.CompletionTokens > 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
		inputEstimated = usage.PromptTokens > 0
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.InputTokens = usage.PromptTokens
	usage.OutputTokens = usage.CompletionTokens
	e.recordSources(info, inputEstimated, outputEstimated)
	if inputEstimated || outputEstimated {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	}
}
