package openai

import (
	"encoding/json"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/tidwall/gjson"
	"strings"
)

// Called exclusively with parsed upstream response frames. Request fields and
// text inside messages cannot influence the generation outcome or billing.
func (e *responsesUsageEstimate) observeChatTerminal(info *relaycommon.RelayInfo, data string, usage *dto.Usage) {
	root := gjson.Parse(data)
	state, reason := "", ""
	for _, choice := range root.Get("choices").Array() {
		switch choice.Get("finish_reason").String() {
		case "length":
			state, reason = "incomplete", "max_output_tokens"
		case "content_filter":
			state, reason = "incomplete", "content_filter"
		case "incomplete":
			state, reason = "incomplete", "unknown"
		case "stop", "tool_calls", "function_call":
			if state == "" {
				state = "completed"
			}
		}
	}
	if root.Get("cpa_terminal.status").String() == "incomplete" {
		state, reason = "incomplete", "unknown"
		switch root.Get("cpa_terminal.reason").String() {
		case "max_output_tokens", "content_filter":
			reason = root.Get("cpa_terminal.reason").String()
		}
	}
	if state != "" && !info.IsResponsesIncomplete() {
		info.ResponsesOutcome = &relaycommon.ResponsesOutcome{State: state, Reason: reason,
			InputTokensSource: "unreported", OutputTokensSource: "unreported"}
	}
	if reported := root.Get("usage"); reported.IsObject() {
		// Usage trailers are partial snapshots. Merge each supplied field,
		// including nested cache/audio details, instead of clearing omitted data.
		var patch map[string]json.RawMessage
		if common.UnmarshalJsonStr(reported.Raw, &patch) == nil {
			if root.Get("cpa_usage.input_reported").Type == gjson.False {
				delete(patch, "prompt_tokens")
			}
			if root.Get("cpa_usage.output_reported").Type == gjson.False {
				delete(patch, "completion_tokens")
			}
			mergeChatUsageFields(usage, patch)
			if reported.Get("prompt_tokens").Type == gjson.Number && root.Get("cpa_usage.input_reported").Type != gjson.False {
				e.promptReported = true
			}
			if reported.Get("completion_tokens").Type == gjson.Number && root.Get("cpa_usage.output_reported").Type != gjson.False {
				e.completionReported = true
			}
		}
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	e.recordSources(info, false, false)
}

func mergeChatUsageFields(usage *dto.Usage, patch map[string]json.RawMessage) {
	previous, err := common.Marshal(usage)
	if err != nil {
		return
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(previous, &fields) != nil {
		return
	}
	for key, value := range patch {
		if strings.HasSuffix(key, "_details") && common.GetJsonType(value) == "object" {
			details := map[string]json.RawMessage{}
			_ = common.Unmarshal(fields[key], &details)
			if details == nil {
				details = map[string]json.RawMessage{}
			}
			var changes map[string]json.RawMessage
			if common.Unmarshal(value, &changes) == nil {
				for field, datum := range changes {
					details[field] = datum
				}
				value, _ = common.Marshal(details)
			}
		}
		fields[key] = value
	}
	merged, err := common.Marshal(fields)
	if err == nil {
		_ = common.Unmarshal(merged, usage)
	}
}
