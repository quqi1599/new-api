package claude

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestClaudeTerminalOutcomeAcrossFormats(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, stop := range []string{"max_tokens", "end_turn", "tool_use", "refusal"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", format, stop, stream), func(t *testing.T) {
					sse := fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n", stop)
					recorder, c, info, resp := newClaudeStreamErrorTestContext(format, sse)
					var usage *dto.Usage
					var apiErr *types.NewAPIError
					if stream {
						if format == types.RelayFormatOpenAIResponses {
							usage, apiErr = ClaudeResponsesStreamHandler(c, resp, info)
						} else {
							usage, apiErr = ClaudeStreamHandler(c, resp, info)
						}
					} else {
						info.IsStream = false
						data := []byte(fmt.Sprintf(`{"type":"message","id":"msg_test","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":%q,"usage":{"input_tokens":10,"output_tokens":5}}`, stop))
						ci := &ClaudeResponseInfo{}
						apiErr = HandleClaudeResponseData(c, info, ci, &http.Response{StatusCode: 200, Header: http.Header{}}, data)
						usage = ci.Usage
					}
					require.Nil(t, apiErr)
					require.NotNil(t, info.ResponsesOutcome)
					want, reason := "completed", ""
					if stop == "max_tokens" {
						want, reason = "incomplete", "max_output_tokens"
					}
					if stop == "refusal" {
						want, reason = "incomplete", "content_filter"
					}
					require.Equal(t, want, info.ResponsesOutcome.State)
					require.Equal(t, reason, info.ResponsesOutcome.Reason)
					require.Equal(t, "reported", info.ResponsesOutcome.InputTokensSource)
					require.Equal(t, "reported", info.ResponsesOutcome.OutputTokensSource)
					require.Equal(t, 10, usage.PromptTokens)
					require.Equal(t, 5, usage.CompletionTokens)
					require.Equal(t, want == "completed", info.ShouldRecordChannelSuccess())
					other := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 1, 1)
					require.Equal(t, want, other["responses_outcome"].(*relaycommon.ResponsesOutcome).State)
					if stream {
						status := "ok"
						if want == "incomplete" {
							status = "incomplete"
						}
						require.Equal(t, status, other["stream_status"].(map[string]interface{})["status"])
					}
					if format == types.RelayFormatClaude {
						require.Contains(t, strings.ReplaceAll(recorder.Body.String(), " ", ""), `"stop_reason":"`+stop+`"`)
					}
					if format == types.RelayFormatOpenAI && stop == "max_tokens" {
						require.Contains(t, recorder.Body.String(), `"finish_reason":"length"`)
					}
					if format == types.RelayFormatOpenAIResponses && stop == "max_tokens" {
						require.Contains(t, recorder.Body.String(), `"status":"incomplete"`)
					}
				})
			}
		}
	}
}

func TestClaudeReportedZeroIsNotReplacedByEstimate(t *testing.T) {
	for _, usageJSON := range []string{`{"input_tokens":0,"output_tokens":0}`, `{}`} {
		sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"synthetic text should not overwrite usage\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":" + usageJSON + "}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		_, c, info, resp := newClaudeStreamErrorTestContext(types.RelayFormatClaude, sse)
		usage, err := ClaudeStreamHandler(c, resp, info)
		require.Nil(t, err)
		require.Zero(t, usage.PromptTokens)
		require.Zero(t, usage.CompletionTokens)
		wantSource := "reported"
		if usageJSON == `{}` {
			wantSource = "unreported"
		}
		require.Equal(t, wantSource, info.ResponsesOutcome.InputTokensSource)
		require.Equal(t, wantSource, info.ResponsesOutcome.OutputTokensSource)
	}
}

func TestClaudeInterruptedTerminalDoesNotHealOrComplete(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, ending := range []string{"", "data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"synthetic\"}}\n\n"} {
			sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":5}}\n\n" + ending
			_, c, info, resp := newClaudeStreamErrorTestContext(format, sse)
			if format == types.RelayFormatOpenAIResponses {
				_, _ = ClaudeResponsesStreamHandler(c, resp, info)
			} else {
				_, _ = ClaudeStreamHandler(c, resp, info)
			}
			require.NotNil(t, info.ResponsesOutcome)
			require.Equal(t, "incomplete", info.ResponsesOutcome.State)
			require.Equal(t, "max_output_tokens", info.ResponsesOutcome.Reason)
			require.Equal(t, "failed", info.GenerationOutcome.State)
			require.False(t, info.ShouldRecordChannelSuccess())
			other := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 1, 1)
			require.Equal(t, "error", other["stream_status"].(map[string]interface{})["status"])
			require.Equal(t, "reported", info.ResponsesOutcome.InputTokensSource)
			require.Equal(t, "reported", info.ResponsesOutcome.OutputTokensSource)
		}
	}
}

// Fixtures use CPA's native Responses -> Messages envelope: the start has
// placeholder zeros; only terminal cpa_usage establishes reported fields.
func TestCPAMessagesBridgeIncompletePreservesUnknownUsage(t *testing.T) {
	for _, reported := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("reported=%t/stream=%t", reported, stream), func(t *testing.T) {
				usageJSON := `{}`
				if reported {
					usageJSON = `{"input_tokens":0,"output_tokens":0}`
				}
				terminal := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"incomplete"},"usage":%s,"cpa_usage":{"input_reported":%t,"output_reported":%t},"cpa_terminal":{"status":"incomplete","reason":"unknown"}}`, usageJSON, reported, reported)
				sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\ndata: " + terminal + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
				_, c, info, resp := newClaudeStreamErrorTestContext(types.RelayFormatClaude, sse)
				if stream {
					_, err := ClaudeStreamHandler(c, resp, info)
					require.Nil(t, err)
				} else {
					info.IsStream = false
					body := fmt.Sprintf(`{"type":"message","id":"msg_test","model":"claude-test","content":[],"stop_reason":"incomplete","usage":%s,"cpa_usage":{"input_reported":%t,"output_reported":%t},"cpa_terminal":{"status":"incomplete","reason":"unknown"}}`, usageJSON, reported, reported)
					require.Nil(t, HandleClaudeResponseData(c, info, &ClaudeResponseInfo{}, resp, []byte(body)))
				}
				require.NotNil(t, info.ResponsesOutcome)
				require.Equal(t, "incomplete", info.ResponsesOutcome.State)
				require.Equal(t, "unknown", info.ResponsesOutcome.Reason)
				source := "unreported"
				if reported {
					source = "reported"
				}
				require.Equal(t, source, info.ResponsesOutcome.InputTokensSource)
				require.Equal(t, source, info.ResponsesOutcome.OutputTokensSource)
			})
		}
	}
}
