package openai

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestChatStreamMetadataOnly(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"choices":[{"delta":{"role":"assistant","content":"","reasoning_content":null}}]}`, true},
		{`{"choices":[],"usage":{"prompt_tokens":0}}`, true},
		{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, true},
		{`{"choices":[{"delta":{"content":"text"}}]}`, false},
		{`{"choices":[{"delta":{"reasoning":"thinking"}}]}`, false},
		{`{"choices":[{"delta":{"refusal":"cannot comply"}}]}`, false},
		{`{"choices":[{"delta":{"audio":{"data":"fixture"}}}]}`, false},
		{`{"choices":[{"delta":{"function_call":{"arguments":"{}"}}}]}`, false},
		{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`, false},
		{`{"choices":[{"text":"legacy output"}]}`, false},
	} {
		var event map[string]any
		require.NoError(t, common.UnmarshalJsonStr(tc.body, &event))
		require.Equal(t, tc.want, chatStreamMetadataOnly(event), tc.body)
	}
}

func TestChatFailedMetadataOnlyStreamNeverEstimatesPromptUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage        string
		prompt, completion int
	}{
		{"missing", "", 0, 0},
		{"explicit_zero", `,"usage":{"prompt_tokens":0,"completion_tokens":0}`, 0, 0},
		{"reported_prompt", `,"usage":{"prompt_tokens":17}`, 17, 0},
		{"reported_completion", `,"usage":{"completion_tokens":3}`, 0, 3},
		{"reported_usage", `,"usage":{"prompt_tokens":17,"completion_tokens":3}`, 17, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newResponsesTestContext()
			info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions,
				RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-5.3-flash"}}
			info.SetEstimatePromptTokens(191664)
			// A heartbeat can commit HTTP 200 while the role-only frame is buffered.
			// It is not evidence that the model generated any billable output.
			recorder.WriteHeader(http.StatusOK)
			_, _ = c.Writer.Write([]byte(": ping\n\n"))
			body := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]` + tc.usage + "}\n\n" +
				"data: {\"error\":{\"code\":\"1234\",\"message\":\"synthetic upstream network error\"}}\n\n"
			usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
			require.NotNil(t, err)
			require.True(t, types.IsSkipRetryError(err))
			require.Equal(t, types.ErrorCode("1234"), err.GetErrorCode())
			require.NotNil(t, usage)
			require.Equal(t, tc.prompt, usage.PromptTokens)
			require.Equal(t, tc.completion, usage.CompletionTokens)
			require.Equal(t, tc.prompt+tc.completion, usage.TotalTokens)
			require.False(t, c.GetBool(string(constant.ContextKeyLocalCountTokens)))
			require.NotContains(t, recorder.Body.String(), "[DONE]")
		})
	}
}

func TestChatStream1234BeforeOutputPreservesProviderError(t *testing.T) {
	c, recorder := newResponsesTestContext()
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-5.3-flash"}}
	info.SetEstimatePromptTokens(191664)
	body := "data: {\"error\":{\"code\":\"1234\",\"message\":\"synthetic network error\"}}\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	require.Nil(t, usage)
	require.NotNil(t, err)
	require.Equal(t, types.ErrorCode("1234"), err.GetErrorCode())
	require.True(t, types.IsSkipRetryError(err))
	require.Empty(t, recorder.Body.String())
}

func TestChatFailedStreamWithGeneratedTextRetainsUsageEstimate(t *testing.T) {
	c, _ := newResponsesTestContext()
	info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions,
		RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-5.3-flash"}}
	info.SetEstimatePromptTokens(31)
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"actual generated text\"}}]}\n\n" +
		"data: {\"error\":{\"code\":\"1234\",\"message\":\"synthetic failure\"}}\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	require.NotNil(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 31, usage.PromptTokens)
	require.Positive(t, usage.CompletionTokens)
	require.True(t, types.IsSkipRetryError(err))
}
