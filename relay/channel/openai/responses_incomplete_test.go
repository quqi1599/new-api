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
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesIncompleteContract(t *testing.T) {
	for _, endpoint := range []struct {
		name              string
		stream, converted bool
		handler           func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"http_native", false, false, OaiResponsesHandler},
		{"sse_native", true, false, OaiResponsesStreamHandler},
		{"http_chat", false, true, OaiResponsesToChatHandler},
		{"sse_chat", true, true, OaiResponsesToChatStreamHandler},
	} {
		for _, reason := range []struct{ raw, safe, finish string }{
			{`"max_output_tokens"`, "max_output_tokens", "length"},
			{`"content_filter"`, "content_filter", "content_filter"},
			{`"provider supplied sensitive text"`, "unknown", "incomplete"},
			{`null`, "unknown", "incomplete"},
		} {
			for _, counts := range []struct {
				raw, inputSource, outputSource string
				input, output                  int
			}{
				{`{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":8},"output_tokens_details":{"reasoning_tokens":1}}`, "reported", "reported", 10, 2},
				{`{"input_tokens":0,"output_tokens":0}`, "reported", "reported", 0, 0},
				{`{"input_tokens":10}`, "reported", "unreported", 10, 0},
				{`null`, "unreported", "unreported", 0, 0},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s/%d", endpoint.name, reason.raw, counts.outputSource, counts.input), func(t *testing.T) {
					c, recorder := newResponsesTestContext()
					info := &relaycommon.RelayInfo{IsStream: endpoint.stream, RelayFormat: types.RelayFormatOpenAI,
						ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.6-sol"}, ShouldIncludeUsage: true}
					info.SetEstimatePromptTokens(9999)
					body := fmt.Sprintf(`{"id":"fixture","model":"gpt-5.6-sol","status":"incomplete","incomplete_details":{"reason":%s},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial answer"}]}],"usage":%s}`, reason.raw, counts.raw)
					if endpoint.stream {
						// Snapshot-only output and an extra frame after the terminal
						// verify completion does not consume or deliver another event.
						body = "data: {\"type\":\"response.incomplete\",\"response\":" + body + "}\n\n" +
							"data: {\"type\":\"response.output_text.delta\",\"delta\":\"MUST_NOT_DELIVER\"}\n\n"
					}
					usage, apiErr := endpoint.handler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
					require.Nil(t, apiErr, "incomplete is not a failed upstream operation")
					require.Equal(t, counts.input, usage.PromptTokens)
					require.Equal(t, counts.output, usage.CompletionTokens)
					require.Equal(t, counts.input+counts.output, usage.TotalTokens)
					if counts.output == 2 {
						require.Equal(t, 1, usage.CompletionTokenDetails.ReasoningTokens)
						require.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
					}
					require.Equal(t, "incomplete", info.ResponsesOutcome.State)
					require.Equal(t, reason.safe, info.ResponsesOutcome.Reason)
					require.Equal(t, counts.inputSource, info.ResponsesOutcome.InputTokensSource)
					require.Equal(t, counts.outputSource, info.ResponsesOutcome.OutputTokensSource)
					require.False(t, info.ShouldRecordChannelSuccess(), "must not restore circuit health")
					require.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
					require.Contains(t, recorder.Body.String(), "partial answer")
					require.NotContains(t, recorder.Body.String(), "MUST_NOT_DELIVER")
					if endpoint.converted {
						require.Contains(t, recorder.Body.String(), `"finish_reason":"`+reason.finish+`"`)
					}
					if endpoint.stream {
						require.True(t, info.StreamStatus.IsNormalEnd(), "transport reached a valid terminal")
						require.False(t, info.StreamStatus.HasErrors())
						require.Equal(t, 1, info.ReceivedResponseCount)
					}
				})
			}
		}
	}
}

func TestResponsesConvertedIncompleteDoesNotRepeatSnapshotOutput(t *testing.T) {
	c, recorder := newResponsesTestContext()
	info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{}}
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial \"}\n\n" +
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial answer\"}]}]}}\n\n"
	_, apiErr := OaiResponsesToChatStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(body))})
	require.Nil(t, apiErr)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "partial "))
	require.Contains(t, recorder.Body.String(), `"content":"answer"`)
}

func TestBufferedResponsesIncompletePresenceAndJSON(t *testing.T) {
	for _, tc := range []struct {
		name, early, terminal, inputSource, outputSource string
		input, output, cache                             int
	}{
		{name: "reported_zero", terminal: `,"usage":{"input_tokens":0,"output_tokens":0}`, inputSource: "reported", outputSource: "reported"},
		{name: "absent", inputSource: "unreported", outputSource: "unreported"},
		{name: "input_only", terminal: `,"usage":{"input_tokens":0}`, inputSource: "reported", outputSource: "unreported"},
		{name: "earlier_partial_usage", early: `data: {"type":"response.in_progress","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":8}}}}` + "\n\n", terminal: `,"usage":{"input_tokens":10}`, inputSource: "reported", outputSource: "reported", input: 10, output: 2, cache: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newResponsesTestContext()
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "fixture"}}
			info.SetEstimatePromptTokens(9000)
			body := tc.early + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
				`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"unknown"}` + tc.terminal + "}}\n\n"
			usage, err := OaiResponsesToChatBufferedStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))})
			require.Nil(t, err)
			require.Equal(t, tc.input, usage.PromptTokens)
			require.Equal(t, tc.output, usage.CompletionTokens)
			require.Equal(t, tc.cache, usage.PromptTokensDetails.CachedTokens)
			require.Equal(t, tc.inputSource, info.ResponsesOutcome.InputTokensSource)
			require.Equal(t, tc.outputSource, info.ResponsesOutcome.OutputTokensSource)
			require.Equal(t, "unknown", info.ResponsesOutcome.Reason)
			require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
			require.NotContains(t, recorder.Body.String(), "data:")
			require.NotContains(t, recorder.Body.String(), "[DONE]")
			require.Contains(t, recorder.Body.String(), `"content":"partial"`)
			require.Contains(t, recorder.Body.String(), `"finish_reason":"incomplete"`)
			require.False(t, info.ShouldRecordChannelSuccess())
		})
	}
}
