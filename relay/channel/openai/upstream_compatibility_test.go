package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Regression cases adapted from upstream #7427, with this fork's failure/refund
// boundary retained: metadata alone is not evidence of generated output.
func TestResponsesMissingUsageCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name, events, text        string
		wantError, estimated      bool
		prompt, completion, cache int
	}{
		{name: "tool arguments interrupted", events: `{"type":"response.function_call_arguments.delta","delta":"hello world"}`, text: "hello world", wantError: true, estimated: true, prompt: 100},
		{name: "reasoning summary interrupted", events: `{"type":"response.reasoning_summary_text.delta","delta":"hello world"}`, text: "hello world", wantError: true, estimated: true, prompt: 100},
		{name: "reasoning interrupted", events: `{"type":"response.reasoning_text.delta","delta":"hello world"}`, text: "hello world", wantError: true, estimated: true, prompt: 100},
		{name: "refusal text interrupted", events: `{"type":"response.refusal.delta","delta":"hello world"}`, text: "hello world", wantError: true, estimated: true, prompt: 100},
		{name: "terminal text only", events: `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}]}}`, text: "hello world", estimated: true, prompt: 100},
		{name: "terminal tool only", events: `{"type":"response.completed","response":{"output":[{"type":"function_call","arguments":"hello world"}]}}`, text: "hello world", estimated: true, prompt: 100},
		{name: "terminal summary only", events: `{"type":"response.completed","response":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"hello world"}]}]}}`, text: "hello world", estimated: true, prompt: 100},
		{name: "terminal refusal only", events: `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"refusal","refusal":"hello world"}]}]}}`, text: "hello world", estimated: true, prompt: 100},
		{name: "metadata only interrupted", events: `{"type":"response.created"}`, wantError: true},
		{name: "empty successful response", events: `{"type":"response.completed","response":{"output":[]}}`},
		{name: "explicit failure", events: `{"type":"response.function_call_arguments.delta","delta":"hello world"}` + "\n" + `{"type":"response.failed","error":{"message":"broken"}}`, wantError: true},
		{name: "explicit incomplete", events: `{"type":"response.output_text.delta","delta":"hello world"}` + "\n" + `{"type":"response.incomplete"}`, wantError: true},
		{name: "authoritative usage wins", events: `{"type":"response.reasoning_text.delta","delta":"hello world"}` + "\n" + `{"type":"response.completed","response":{"usage":{"input_tokens":30,"output_tokens":2,"total_tokens":32,"input_tokens_details":{"cached_tokens":20}}}}`, prompt: 30, completion: 2, cache: 20},
		{name: "authoritative zero wins", events: `{"type":"response.output_text.delta","delta":"hello world"}` + "\n" + `{"type":"response.completed","response":{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
		{name: "partial usage missing input", events: `{"type":"response.completed","response":{"usage":{"output_tokens":2}}}`, prompt: 100, completion: 2, estimated: true},
		{name: "delta and terminal not counted twice", events: `{"type":"response.output_text.delta","delta":"hello world"}` + "\n" + `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"hello world"}]}]}}`, text: "hello world", estimated: true, prompt: 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, recorder := newResponsesTestContext()
			info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAIResponses, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
			info.SetEstimatePromptTokens(100)
			body := "data: " + strings.ReplaceAll(tt.events, "\n", "\n\ndata: ") + "\n\n"
			usage, handlerErr := OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
			if tt.wantError {
				require.NotNil(t, handlerErr)
				require.True(t, types.IsSkipRetryError(handlerErr))
				require.NotContains(t, recorder.Body.String(), "[DONE]")
			} else {
				require.Nil(t, handlerErr)
			}
			require.NotNil(t, usage)
			completion := tt.completion
			if tt.text != "" {
				completion = service.CountTextToken(tt.text, "test-model")
			}
			require.Equal(t, tt.prompt, usage.PromptTokens)
			require.Equal(t, completion, usage.CompletionTokens)
			require.Equal(t, tt.prompt+completion, usage.TotalTokens)
			require.Equal(t, tt.cache, usage.PromptTokensDetails.CachedTokens)
			require.Equal(t, tt.estimated, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
			if strings.Contains(tt.events, `"response.failed"`) || strings.Contains(tt.events, `"response.incomplete"`) {
				require.NotSame(t, handlerErr, info.PartialStreamError, "explicit failures must retain the refund path")
			} else if tt.wantError {
				require.Same(t, handlerErr, info.PartialStreamError)
			}
		})
	}
}

func TestTerminalUsageChunkCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name, choices string
		keep          bool
	}{
		{"usage only", `[]`, false},
		{"empty choice", `[{"delta":{}}]`, false},
		{"empty finish reason", `[{"delta":{},"finish_reason":""}]`, false},
		{"stop", `[{"delta":{},"finish_reason":"stop"}]`, true},
		{"tool finish", `[{"delta":{},"finish_reason":"tool_calls"}]`, true},
		{"tool arguments", `[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]`, true},
		{"text", `[{"delta":{"content":"hi"}}]`, true},
		{"reasoning", `[{"delta":{"reasoning_content":"hi"}}]`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, includeUsage := range []bool{false, true} {
				info := &relaycommon.RelayInfo{ShouldIncludeUsage: includeUsage}
				var id, fingerprint, model string
				var created int64
				usage := &dto.Usage{}
				contains, send := false, true
				body := `{"id":"test","choices":` + tt.choices + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
				require.NoError(t, handleLastResponse(body, &id, &created, &fingerprint, &model, &usage, &contains, info, &send))
				require.Equal(t, tt.keep || includeUsage, send)
				require.True(t, contains)
				require.Equal(t, 12, usage.TotalTokens)
			}
		})
	}
}

func TestGPT6ChatCompatibility(t *testing.T) {
	for _, tt := range []struct {
		model, effort, normalized string
		sampling                  bool
	}{
		{"gpt-6-sol", "", "gpt-6-sol", true},
		{"gpt-6-luna", "none", "gpt-6-luna", true},
		{"gpt-6-sol", "high", "gpt-6-sol", false},
		{"gpt-6-luna-high", "none", "gpt-6-luna", false},
		{"gpt-6-sol-none", "high", "gpt-6-sol", true},
		{"gpt-6-luna-2026-09-03", "", "gpt-6-luna-2026-09-03", true},
		{"gpt-6-astra", "none", "gpt-6-astra", false},
		{"gpt-6-astra-2026-09-03-high", "", "gpt-6-astra-2026-09-03", false},
	} {
		t.Run(tt.model+"/"+tt.effort, func(t *testing.T) {
			for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
				c, _ := setupOpenAITestContext()
				info := &relaycommon.RelayInfo{OriginModelName: "customer-alias", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType, UpstreamModelName: tt.model}}
				request := &dto.GeneralOpenAIRequest{Model: tt.model, MaxTokens: common.GetPointer(uint(0)), ReasoningEffort: tt.effort,
					Temperature: common.GetPointer(0.0), TopP: common.GetPointer(0.0), LogProbs: common.GetPointer(false), TopLogProbs: common.GetPointer(0),
					Messages: []dto.Message{{Role: "system", Content: "instruction"}, {Role: "user", Content: "hi"}}}
				_, err := (&Adaptor{}).ConvertOpenAIRequest(c, info, request)
				require.NoError(t, err)
				require.Nil(t, request.MaxTokens)
				require.Equal(t, common.GetPointer(uint(0)), request.MaxCompletionTokens)
				require.Equal(t, "developer", request.Messages[0].Role)
				require.Equal(t, tt.normalized, request.Model)
				require.Equal(t, tt.normalized, info.UpstreamModelName)
				require.Equal(t, "customer-alias", info.OriginModelName)
				require.Equal(t, tt.sampling, request.Temperature != nil)
				require.Equal(t, tt.sampling, request.TopP != nil)
				require.Equal(t, tt.sampling, request.LogProbs != nil)
				require.Equal(t, tt.sampling, request.TopLogProbs != nil)
			}
		})
	}
}

func TestChatLimitPresenceCompatibility(t *testing.T) {
	for _, tt := range []struct{ fields, want string }{
		{``, `{}`},
		{`,"max_tokens":0`, `{"max_completion_tokens":0}`},
		{`,"max_tokens":32`, `{"max_completion_tokens":32}`},
		{`,"max_completion_tokens":0`, `{"max_completion_tokens":0}`},
		{`,"max_tokens":32,"max_completion_tokens":0`, `{"max_completion_tokens":0}`},
		{`,"max_tokens":32,"max_completion_tokens":8`, `{"max_completion_tokens":8}`},
	} {
		t.Run(tt.fields, func(t *testing.T) {
			c, _ := setupOpenAITestContext()
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-6-sol"}}
			var req dto.GeneralOpenAIRequest
			require.NoError(t, common.UnmarshalJsonStr(`{"model":"gpt-6-sol"`+tt.fields+`}`, &req))
			_, err := (&Adaptor{}).ConvertOpenAIRequest(c, info, &req)
			require.NoError(t, err)
			got := req.ToMap()
			delete(got, "model")
			encoded, err := common.Marshal(got)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(encoded))
		})
	}
}

func TestChatUnknownMappedModelCompatibility(t *testing.T) {
	for _, modelName := range []string{"qwen3.8-max", "ollama-model", "gpt-6-sol-custom", "gpt-6-sol-invalid-high"} {
		c, _ := setupOpenAITestContext()
		info := &relaycommon.RelayInfo{OriginModelName: "gpt-6-sol", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: modelName}}
		req := &dto.GeneralOpenAIRequest{Model: modelName, MaxTokens: common.GetPointer(uint(8)), Temperature: common.GetPointer(0.0), TopP: common.GetPointer(0.0), Messages: []dto.Message{{Role: "system", Content: "instruction"}}}
		before, err := common.Marshal(req)
		require.NoError(t, err)
		_, err = (&Adaptor{}).ConvertOpenAIRequest(c, info, req)
		require.NoError(t, err)
		after, err := common.Marshal(req)
		require.NoError(t, err)
		require.JSONEq(t, string(before), string(after), "use the mapped model's capabilities, not the public alias")
	}
}

func TestOpenRouterReasoningSamplingCompatibility(t *testing.T) {
	for _, fields := range []string{
		`"reasoning_effort":"high"`,
		`"reasoning":{"effort":"high"}`,
		`"reasoning":{"enabled":true}`,
	} {
		c, _ := setupOpenAITestContext()
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "gpt-6-sol"}}
		var req dto.GeneralOpenAIRequest
		require.NoError(t, common.UnmarshalJsonStr(`{"model":"gpt-6-sol","temperature":0.5,"top_p":0.8,`+fields+`}`, &req))
		_, err := (&Adaptor{}).ConvertOpenAIRequest(c, info, &req)
		require.NoError(t, err)
		require.Nil(t, req.Temperature)
		require.Nil(t, req.TopP)
		require.NotEmpty(t, req.Reasoning)
	}
}

func TestChatTerminalToolChunkReachesClient(t *testing.T) {
	c, recorder := setupOpenAITestContext()
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	body := "data: " + `{"choices":[{"delta":{"role":"assistant"}}]}` + "\n\ndata: " +
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\ndata: [DONE]\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	require.Nil(t, err)
	require.Equal(t, 12, usage.TotalTokens)
	require.Contains(t, recorder.Body.String(), `"finish_reason":"tool_calls"`)
	require.Contains(t, recorder.Body.String(), `"arguments":"}"`)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "[DONE]"))
}

func TestResponsesEstimatedUsageRemainsFailedOnDisconnect(t *testing.T) {
	w := &postOutputDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: 1}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAIResponses, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"}}
	info.SetEstimatePromptTokens(100)
	body := "data: " + `{"type":"response.created"}` + "\n\ndata: " + `{"type":"response.function_call_arguments.delta","delta":"hello world"}` + "\n\ndata: " + `{"type":"response.completed"}` + "\n\n"
	usage, err := OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	require.NotNil(t, err)
	require.Equal(t, 499, err.StatusCode)
	require.True(t, types.IsSkipRetryError(err))
	require.False(t, types.IsChannelPenaltyAllowed(err))
	require.Same(t, err, info.PartialStreamError)
	require.Equal(t, 100, usage.PromptTokens)
	require.Positive(t, usage.CompletionTokens)
	require.Equal(t, 2, w.writes)
	require.NotContains(t, w.Body.String(), "response.completed")
}
