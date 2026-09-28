package openai

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Adapted from upstream #7427 without changing this fork's refund/stream outcome
// policy. Estimate only missing fields and only from generated output, never
// merely because response.created or another metadata event was received.
type responsesUsageEstimate struct {
	output             strings.Builder
	promptReported     bool
	completionReported bool
}

func (e *responsesUsageEstimate) observe(event *dto.ResponsesStreamResponse, data string, usage *dto.Usage) {
	switch event.Type {
	case "response.output_text.delta", "response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		e.output.WriteString(event.Delta)
	case "response.completed", "response.done":
		if event.Response == nil {
			return
		}
		if reported := event.Response.Usage; reported != nil {
			// Field presence matters: a reported zero is not a missing estimate.
			if gjson.Get(data, "response.usage.input_tokens").Type == gjson.Number {
				e.promptReported = true
				usage.PromptTokens = reported.InputTokens
			}
			if gjson.Get(data, "response.usage.output_tokens").Type == gjson.Number {
				e.completionReported = true
				usage.CompletionTokens = reported.OutputTokens
			}
			if reported.InputTokensDetails != nil {
				usage.PromptTokensDetails = *reported.InputTokensDetails
			}
		}
		if e.completionReported || e.output.Len() != 0 {
			return // authoritative usage wins; terminal snapshots must not repeat deltas
		}
		for _, item := range event.Response.Output {
			switch item.Type {
			case "function_call":
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
}

// Called only after explicit protocol failures have taken the existing refund
// path. Local estimates are observable using the existing log/UI marker.
func (e *responsesUsageEstimate) finish(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	estimated := false
	if !e.completionReported && e.output.Len() > 0 {
		usage.CompletionTokens = service.CountTextToken(e.output.String(), info.UpstreamModelName)
		estimated = usage.CompletionTokens > 0
	}
	if !e.promptReported && usage.CompletionTokens > 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
		estimated = estimated || usage.PromptTokens > 0
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if estimated {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	}
}
