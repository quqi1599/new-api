package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func isDecodeRecoveryFailure(err *types.NewAPIError) bool {
	// Keep the risk gate after a channel maps the original 502 to another 5xx.
	// This does not override the existing status-based retry decision.
	return isDecodeFailureCode(err) && err.StatusCode >= http.StatusInternalServerError && err.StatusCode <= 599
}

func isDecodeFailureCode(err *types.NewAPIError) bool {
	return err != nil && string(err.GetErrorCode()) == "upstream_response_decode_failed"
}

// Decode recovery is an explicit, bounded choice to regenerate an uncommitted
// text answer. Inspect the original JSON: protocol DTOs may omit risky fields.
func decodeRecoveryRequestSafe(c *gin.Context, info *relaycommon.RelayInfo, format types.RelayFormat) bool {
	if c == nil || c.Request == nil || info == nil || info.ChannelMeta == nil || len(info.ParamOverride) != 0 || info.UseRuntimeHeadersOverride {
		return false
	}
	if c.Request.Method != http.MethodPost || c.Request.Context().Err() != nil || relayProgressStarted(c, info) {
		return false
	}
	path := c.Request.URL.Path
	if !((format == types.RelayFormatOpenAI && path == "/v1/chat/completions") || (format == types.RelayFormatOpenAIResponses && path == "/v1/responses")) {
		return false
	}
	// A text-shaped request can still generate images through its model. Keep
	// both the attempted model and the next selected channel's mapping in scope:
	// changing a customer alias must not reopen recovery after image generation.
	if decodeRecoveryModelMayGenerateImages(info.OriginModelName, info.ChannelType) || decodeRecoveryModelMayGenerateImages(info.UpstreamModelName, info.ChannelType) {
		return false
	}
	selectedChannelType := info.ChannelType
	if channelType, exists := c.Get(string(constant.ContextKeyChannelType)); exists {
		selectedChannelType, _ = channelType.(int)
	}
	mapped := &relaycommon.RelayInfo{OriginModelName: info.OriginModelName, RelayMode: info.RelayMode,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: info.OriginModelName}}
	if err := helper.ModelMappedHelper(c, mapped, nil); err != nil || decodeRecoveryModelMayGenerateImages(mapped.UpstreamModelName, selectedChannelType) {
		return false
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return false
	}
	body, err := storage.Bytes()
	if err != nil {
		return false
	}
	if decodeRecoveryModelMayGenerateImages(gjson.GetBytes(body, "model").String(), info.ChannelType) {
		return false
	}
	return decodeRecoveryPayloadSafe(body, format)
}

func decodeRecoveryModelMayGenerateImages(model string, channelType int) bool {
	model = strings.TrimSpace(model)
	// Only these adaptors auto-enable image output from the configurable list.
	// A custom text model with the same name on an OpenAI channel is unrelated.
	gemini := channelType == constant.ChannelTypeGemini || channelType == constant.ChannelTypeVertexAi
	if gemini && model_setting.IsGeminiModelSupportImagine(model) {
		return true
	}
	// Namespaced provider models retain the same generation capability.
	if index := strings.LastIndexByte(model, '/'); index >= 0 {
		model = model[index+1:]
		if gemini && model_setting.IsGeminiModelSupportImagine(model) {
			return true
		}
	}
	model = strings.ToLower(model)
	return strings.HasPrefix(model, "gpt-image") || model == "dall-e" || strings.HasPrefix(model, "dall-e-") ||
		model == "imagen" || strings.HasPrefix(model, "imagen-") || strings.HasPrefix(model, "nano-banana") ||
		strings.HasPrefix(model, "nano_banana") || (strings.HasPrefix(model, "gemini-") && strings.Contains(model, "image"))
}

func decodeRecoveryPayloadSafe(body []byte, format types.RelayFormat) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return false
	}
	allowed := true
	seenKeys := map[string]bool{}
	root.ForEach(func(k, v gjson.Result) bool {
		if seenKeys[k.String()] {
			allowed = false
			return false
		}
		seenKeys[k.String()] = true
		switch k.String() {
		// Generation controls and explicit complete text/schema are local values.
		case "model", "messages", "input", "instructions", "stream", "stream_options", "max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "top_k", "stop", "n", "seed", "frequency_penalty", "presence_penalty", "logit_bias", "logprobs", "top_logprobs", "reasoning_effort", "reasoning", "thinking", "thinking_budget", "enable_thinking", "verbosity", "response_format", "text", "metadata", "user", "service_tier", "safety_identifier", "prompt_cache_key", "prompt_cache_retention", "truncation", "parallel_tool_calls", "max_tool_calls":
		case "tools":
			allowed = recoveryFunctionTools(v, format)
		case "tool_choice":
			allowed = recoveryFunctionChoice(v, format)
		case "store", "background":
			allowed = v.Type == gjson.Null || v.Type == gjson.False
		default:
			// Includes previous_response_id, conversation, opaque include/context
			// controls, legacy extensions, hosted tools and media generation options.
			allowed = false
		}
		return allowed
	})
	if !allowed {
		return false
	}
	if format == types.RelayFormatOpenAI {
		return recoveryVisibleHistory(root.Get("messages"), false)
	}
	input := root.Get("input")
	if input.Type == gjson.String {
		return true
	}
	return recoveryVisibleHistory(input, true)
}

func recoveryFunctionTools(value gjson.Result, format types.RelayFormat) bool {
	if value.Type == gjson.Null {
		return true
	}
	if !value.IsArray() {
		return false
	}
	for _, tool := range value.Array() {
		if !tool.IsObject() || tool.Get("type").String() != "function" {
			return false
		}
		definition := tool
		if format == types.RelayFormatOpenAI {
			if !recoveryObjectKeys(tool, "type", "function") {
				return false
			}
			definition = tool.Get("function")
		}
		if !definition.IsObject() || definition.Get("name").Type != gjson.String || definition.Get("name").String() == "" || !recoveryObjectKeys(definition, "type", "name", "description", "parameters", "strict") {
			return false
		}
	}
	return true
}

func recoveryFunctionChoice(value gjson.Result, format types.RelayFormat) bool {
	if value.Type == gjson.Null {
		return true
	}
	if value.Type == gjson.String {
		switch value.String() {
		case "auto", "none", "required":
			return true
		}
		return false
	}
	if !value.IsObject() || value.Get("type").String() != "function" {
		return false
	}
	if format == types.RelayFormatOpenAI {
		return recoveryObjectKeys(value, "type", "function") && recoveryObjectKeys(value.Get("function"), "name") && value.Get("function.name").Type == gjson.String
	}
	return recoveryObjectKeys(value, "type", "name") && value.Get("name").Type == gjson.String
}

func recoveryObjectKeys(value gjson.Result, keys ...string) bool {
	if !value.IsObject() {
		return false
	}
	valid := true
	seen := map[string]bool{}
	value.ForEach(func(k, _ gjson.Result) bool {
		if seen[k.String()] {
			valid = false
			return false
		}
		seen[k.String()] = true
		valid = false
		for _, key := range keys {
			if k.String() == key {
				valid = true
				break
			}
		}
		return valid
	})
	return valid
}

func recoveryTextContent(value gjson.Result) bool {
	if value.Type == gjson.Null || value.Type == gjson.String {
		return true
	}
	if !value.IsArray() {
		return false
	}
	for _, part := range value.Array() {
		if !recoveryObjectKeys(part, "type", "text") || part.Get("text").Type != gjson.String {
			return false
		}
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
		default:
			return false
		}
	}
	return true
}

func recoveryVisibleHistory(value gjson.Result, responses bool) bool {
	if !value.IsArray() || len(value.Array()) == 0 {
		return false
	}
	pending, seen := map[string]bool{}, map[string]bool{}
	addCall := func(id string) bool {
		if id == "" || seen[id] {
			return false
		}
		pending[id], seen[id] = true, true
		return true
	}
	result := func(id string) bool {
		if !pending[id] {
			return false
		}
		delete(pending, id)
		return true
	}
	for _, item := range value.Array() {
		if !item.IsObject() {
			return false
		}
		if !responses && item.Get("type").Exists() {
			return false
		}
		if responses {
			switch item.Get("type").String() {
			case "function_call":
				if !recoveryObjectKeys(item, "type", "id", "call_id", "name", "arguments", "status") || item.Get("name").Type != gjson.String || item.Get("arguments").Type != gjson.String || !addCall(item.Get("call_id").String()) {
					return false
				}
				continue
			case "function_call_output":
				if !recoveryObjectKeys(item, "type", "id", "call_id", "output", "status") || item.Get("output").Type != gjson.String || !result(item.Get("call_id").String()) {
					return false
				}
				continue
			case "", "message":
			default:
				return false
			}
		}
		if !recoveryObjectKeys(item, "type", "role", "content", "name", "tool_calls", "tool_call_id") || !recoveryTextContent(item.Get("content")) {
			return false
		}
		role := item.Get("role").String()
		switch role {
		case "system", "developer", "user", "assistant":
			if item.Get("tool_call_id").Exists() {
				return false
			}
		case "tool":
			if responses || !result(item.Get("tool_call_id").String()) {
				return false
			}
		default:
			return false
		}
		calls := item.Get("tool_calls")
		if calls.Type != gjson.Null {
			if responses || role != "assistant" || !calls.IsArray() {
				return false
			}
			for _, call := range calls.Array() {
				if !recoveryObjectKeys(call, "id", "type", "function") || call.Get("type").String() != "function" || !recoveryObjectKeys(call.Get("function"), "name", "arguments") || call.Get("function.name").Type != gjson.String || call.Get("function.arguments").Type != gjson.String || !addCall(call.Get("id").String()) {
					return false
				}
			}
		}
	}
	return len(pending) == 0
}
