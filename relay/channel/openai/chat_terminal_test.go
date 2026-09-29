package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestChatIncompleteUsesSameTerminalContract(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ finish, extension, state, reason string }{
			{"length", "", "incomplete", "max_output_tokens"},
			{"content_filter", "", "incomplete", "content_filter"},
			{"incomplete", `,"cpa_terminal":{"status":"incomplete","reason":"unknown"}`, "incomplete", "unknown"},
			{"stop", `,"cpa_terminal":{"status":"incomplete","reason":"unexpected private data"}`, "incomplete", "unknown"},
			{"stop", "", "completed", ""},
			{"tool_calls", "", "completed", ""},
		} {
			for _, report := range []struct {
				json, source  string
				input, output int
			}{
				{`{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}`, "reported", 4, 2},
				{`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, "reported", 0, 0},
				{`{}`, "unreported", 0, 0},
			} {
				if tc.state == "completed" && report.source == "unreported" {
					continue
				} // unchanged successful fallback policy
				t.Run(fmt.Sprintf("stream=%v/%s/%s/%d", stream, tc.finish, report.source, report.input), func(t *testing.T) {
					c, recorder := newResponsesTestContext()
					c.Request.URL.Path = "/v1/chat/completions"
					info := &relaycommon.RelayInfo{IsStream: stream, RelayFormat: types.RelayFormatOpenAI,
						ShouldIncludeUsage: true, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "test-model"}}
					info.SetEstimatePromptTokens(9000)
					info.ChannelSetting.ForceFormat = tc.extension != ""
					// A string inside the response body cannot become a root terminal.
					text := `partial response with cpa_terminal=incomplete`
					body := fmt.Sprintf(`{"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"%s"}],"usage":%s%s}`, text, tc.finish, report.json, tc.extension)
					if stream {
						// The usage trailer is separate from the finish frame.
						body = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"%s\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"%s\"}]%s}\n\ndata: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", text, tc.finish, tc.extension, report.json)
					}
					response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
					handler := OpenaiHandler
					if stream {
						handler = OaiStreamHandler
					}
					usage, apiErr := handler(c, info, response)
					require.Nil(t, apiErr)
					require.Equal(t, report.input, usage.PromptTokens)
					require.Equal(t, report.output, usage.CompletionTokens)
					require.Equal(t, tc.state, info.ResponsesOutcome.State)
					require.Equal(t, tc.reason, info.ResponsesOutcome.Reason)
					require.Equal(t, report.source, info.ResponsesOutcome.InputTokensSource)
					require.Equal(t, report.source, info.ResponsesOutcome.OutputTokensSource)
					require.Equal(t, tc.state == "completed", info.ShouldRecordChannelSuccess())
					require.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
					require.Contains(t, recorder.Body.String(), text)
					if tc.extension != "" {
						require.Contains(t, recorder.Body.String(), `"cpa_terminal"`)
					}
				})
			}
		}
	}
}

func TestChatUsageRegressionChatPartialZero(t *testing.T) {
	service.InitTokenEncoders()
	c, _ := newResponsesTestContext()
	info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, ShouldIncludeUsage: true, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4"}}
	info.SetEstimatePromptTokens(9000)
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":0}}\n\ndata: [DONE]\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokens != 0 {
		t.Errorf("reported input zero overwritten: input=%d output=%d input_source=%s output_source=%s", usage.PromptTokens, usage.CompletionTokens, info.ResponsesOutcome.InputTokensSource, info.ResponsesOutcome.OutputTokensSource)
	}
}
func TestChatUsageRegressionCPAUsagePresence(t *testing.T) {
	info := &relaycommon.RelayInfo{}
	var usage dto.Usage
	var observer responsesUsageEstimate
	observer.observeChatTerminal(info, `{"choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":0,"completion_tokens":0},"cpa_usage":{"input_reported":false,"output_reported":false}}`, &usage)
	if info.ResponsesOutcome.InputTokensSource != "unreported" || info.ResponsesOutcome.OutputTokensSource != "unreported" {
		t.Fatalf("synthesized zeros classified as actual reports: %+v", info.ResponsesOutcome)
	}
}
func TestChatUsageRegressionPartialUsageKeepsCache(t *testing.T) {
	info := &relaycommon.RelayInfo{}
	var usage dto.Usage
	var observer responsesUsageEstimate
	observer.observeChatTerminal(info, `{"choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}`, &usage)
	observer.observeChatTerminal(info, `{"choices":[],"usage":{"prompt_tokens":10}}`, &usage)
	if usage.PromptTokensDetails.CachedTokens != 8 {
		t.Fatal(fmt.Sprintf("cache discount lost: prompt=%d completion=%d cached=%d", usage.PromptTokens, usage.CompletionTokens, usage.PromptTokensDetails.CachedTokens))
	}
}

func TestChatCompletedPartialTrailersKeepCache(t *testing.T) {
	for _, finish := range []string{"stop", "length"} {
		c, _ := newResponsesTestContext()
		info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4"}}
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"prompt_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\n" +
			fmt.Sprintf("data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":10}}\n\ndata: [DONE]\n\n", finish)
		usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
		require.Nil(t, err)
		require.Equal(t, 10, usage.PromptTokens)
		require.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
	}
}

func TestChatCPAUsageFalseSurvivesForceFormatting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		c, recorder := newResponsesTestContext()
		info := &relaycommon.RelayInfo{IsStream: stream, RelayFormat: types.RelayFormatOpenAI, ShouldIncludeUsage: true, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "fixture"}}
		info.ChannelSetting.ForceFormat = true
		info.SetEstimatePromptTokens(9000)
		body := `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":0,"completion_tokens":0},"cpa_usage":{"input_reported":false,"output_reported":false}}`
		handler := OpenaiHandler
		if stream {
			handler = OaiStreamHandler
			body = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0},\"cpa_usage\":{\"input_reported\":false,\"output_reported\":false}}\n\ndata: [DONE]\n\n"
		}
		usage, err := handler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
		require.Nil(t, err)
		require.Zero(t, usage.PromptTokens)
		require.Zero(t, usage.CompletionTokens)
		require.Equal(t, "unreported", info.ResponsesOutcome.InputTokensSource)
		require.Equal(t, "unreported", info.ResponsesOutcome.OutputTokensSource)
		require.Contains(t, recorder.Body.String(), `"input_reported":false`)
		require.Contains(t, recorder.Body.String(), `"output_reported":false`)
	}
}
