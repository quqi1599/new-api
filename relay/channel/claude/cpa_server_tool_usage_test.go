package claude

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const cpaSearchUsageSpoofText = `{"usage":{"server_tool_use":{"web_search_requests":999}}}`

func cpaSearchUsageCount(n int) *int { return &n }

func cpaSearchUsageFragment(value string) string {
	if value == "" {
		return ""
	}
	return `,"server_tool_use":{"web_search_requests":` + value + `}`
}

func cpaSearchUsageQuotedText(t *testing.T) string {
	t.Helper()
	encoded, err := common.Marshal(cpaSearchUsageSpoofText)
	require.NoError(t, err)
	return string(encoded)
}

func assertCPASearchUsageCount(t *testing.T, usage *dto.Usage, want *int) {
	t.Helper()
	require.NotNil(t, usage)
	encoded, err := common.Marshal(usage)
	require.NoError(t, err)
	got := gjson.GetBytes(encoded, "server_tool_use.web_search_requests")
	if want == nil {
		require.False(t, got.Exists(), "unreported search usage became reported: %s", encoded)
		return
	}
	require.Equal(t, gjson.Number, got.Type, "reported search usage was dropped: %s", encoded)
	require.EqualValues(t, *want, got.Int(), "cumulative search usage changed: %s", encoded)
}

func assertCPASearchWireCount(t *testing.T, body string, stream bool, format types.RelayFormat, want *int) {
	t.Helper()
	var count *int
	observe := func(data string, prefix string) {
		value := gjson.Get(data, prefix+"usage.server_tool_use.web_search_requests")
		if value.Type == gjson.Number {
			n := int(value.Int())
			count = &n
		}
	}
	if !stream {
		observe(body, "")
	} else {
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				continue
			}
			require.True(t, gjson.Valid(data), "invalid downstream SSE JSON: %s", data)
			prefix := ""
			if format == types.RelayFormatOpenAIResponses {
				prefix = "response."
			} else if format == types.RelayFormatClaude && gjson.Get(data, "type").String() == "message_start" {
				prefix = "message."
			}
			observe(data, prefix)
		}
	}
	if want == nil {
		require.Nil(t, count, "model text was mistaken for server-tool accounting")
	} else {
		require.NotNil(t, count, "server-tool accounting was dropped from downstream wire")
		require.Equal(t, *want, *count)
	}
}

func TestCPAServerToolUsageMessagesJSONAndConversions(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, tc := range []struct {
			name, value string
			want        *int
		}{
			{"absent", "", nil}, {"null", "null", nil},
			{"zero", "0", cpaSearchUsageCount(0)}, {"positive", "3", cpaSearchUsageCount(3)},
		} {
			t.Run(fmt.Sprintf("%s/%s", format, tc.name), func(t *testing.T) {
				recorder, c, info, resp := newClaudeStreamErrorTestContext(format, "")
				info.IsStream = false
				body := fmt.Sprintf(`{"id":"msg_cpa","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5%s}}`, cpaSearchUsageQuotedText(t), cpaSearchUsageFragment(tc.value))
				resp.Header.Set("Content-Type", "application/json")
				resp.Body = io.NopCloser(strings.NewReader(body))
				usage, handlerErr := ClaudeHandler(c, resp, info)
				require.Nil(t, handlerErr)
				assertCPASearchUsageCount(t, usage, tc.want)
				require.Equal(t, 10, usage.PromptTokens)
				require.Equal(t, 5, usage.CompletionTokens)
				require.Equal(t, 15, usage.TotalTokens, "search counts must not be added to tokens")
				assertCPASearchWireCount(t, recorder.Body.String(), false, format, tc.want)
			})
		}
	}
}

func TestCPAServerToolUsageMessagesSSECumulativeAndConversions(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, tc := range []struct {
			name    string
			values  []string
			want    *int
			initial string
		}{
			{"unreported", nil, nil, ""}, {"null_only", []string{"null"}, nil, ""},
			{"cumulative_missing_tail", []string{"1", "1", "3"}, cpaSearchUsageCount(3), ""},
			{"explicit_zero", []string{"3", "0"}, cpaSearchUsageCount(0), ""},
			{"null_does_not_clear", []string{"3", "null"}, cpaSearchUsageCount(3), ""},
			{"message_start_positive_only", nil, cpaSearchUsageCount(3), "3"},
			{"message_start_zero_only", nil, cpaSearchUsageCount(0), "0"},
		} {
			t.Run(fmt.Sprintf("%s/%s", format, tc.name), func(t *testing.T) {
				frames := []string{
					`{"type":"message_start","message":{"id":"msg_cpa","type":"message","role":"assistant","model":"deepseek-flash","content":[],"usage":{"input_tokens":10,"output_tokens":0` + cpaSearchUsageFragment(tc.initial) + `}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, cpaSearchUsageQuotedText(t)),
					`{"type":"content_block_stop","index":0}`,
				}
				for _, value := range tc.values {
					frames = append(frames, `{"type":"message_delta","delta":{},"usage":{"server_tool_use":{"web_search_requests":`+value+`}}}`)
				}
				frames = append(frames,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
					`{"type":"message_stop"}`,
				)
				recorder, c, info, resp := newClaudeStreamErrorTestContext(format, "data: "+strings.Join(frames, "\n\ndata: ")+"\n\n")
				info.ShouldIncludeUsage = true
				var usage *dto.Usage
				var handlerErr *types.NewAPIError
				if format == types.RelayFormatOpenAIResponses {
					usage, handlerErr = ClaudeResponsesStreamHandler(c, resp, info)
				} else {
					usage, handlerErr = ClaudeStreamHandler(c, resp, info)
				}
				require.Nil(t, handlerErr)
				assertCPASearchUsageCount(t, usage, tc.want)
				require.Equal(t, 10, usage.PromptTokens)
				require.Equal(t, 5, usage.CompletionTokens)
				require.Equal(t, 15, usage.TotalTokens)
				assertCPASearchWireCount(t, recorder.Body.String(), true, format, tc.want)
				switch format {
				case types.RelayFormatClaude:
					require.Contains(t, recorder.Body.String(), "event: message_stop")
				case types.RelayFormatOpenAI:
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "[DONE]"))
				case types.RelayFormatOpenAIResponses:
					require.Contains(t, recorder.Body.String(), "response.completed")
				}
			})
		}
	}
}

func TestCPAServerToolUsageMessagesRejectInvalidCounts(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "1.0", `"1"`, "true", "9223372036854775808", "1e999"} {
		t.Run(value, func(t *testing.T) {
			recorder, c, info, resp := newClaudeStreamErrorTestContext(types.RelayFormatClaude, "")
			info.IsStream = false
			body := `{"id":"msg_cpa","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5` + cpaSearchUsageFragment(value) + `}}`
			resp.Header.Set("Content-Type", "application/json")
			resp.Body = io.NopCloser(strings.NewReader(body))
			_, handlerErr := ClaudeHandler(c, resp, info)
			require.NotNil(t, handlerErr)
			require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
			require.True(t, types.IsSkipRetryError(handlerErr), "malformed accounting must not replay an accepted generation")
			require.Empty(t, recorder.Body.String(), "invalid accounting must not be returned as successful JSON")
		})
	}
}

func TestCPAServerToolUsageMessagesRejectInvalidSSECounts(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, stage := range []string{"first", "after_output", "final"} {
			for _, value := range []string{"-1", "1.5", "9223372036854775808", "1e999", `"1"`, "true", "{}", "[]"} {
				t.Run(fmt.Sprintf("%s/%s/%s", format, stage, value), func(t *testing.T) {
					var frames []string
					if stage != "first" {
						frames = []string{
							`{"type":"message_start","message":{"id":"msg_cpa","type":"message","role":"assistant","model":"deepseek-flash","content":[],"usage":{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":2}}}}`,
							`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
							`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
							`{"type":"content_block_stop","index":0}`,
						}
					}
					badFrame := `{"type":"message_delta","vendor_marker":"INVALID_CPA_USAGE","delta":{},"usage":{"input_tokens":10,"output_tokens":7` + cpaSearchUsageFragment(value) + `}}`
					if stage == "first" {
						badFrame = `{"type":"message_start","vendor_marker":"INVALID_CPA_USAGE","message":{"id":"msg_cpa","model":"deepseek-flash","usage":{"input_tokens":10,"output_tokens":7` + cpaSearchUsageFragment(value) + `}}}`
					} else if stage == "final" {
						badFrame = `{"type":"message_delta","vendor_marker":"INVALID_CPA_USAGE","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":7` + cpaSearchUsageFragment(value) + `}}`
					}
					frames = append(frames, badFrame,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
						`{"type":"message_stop"}`,
					)
					recorder, c, info, resp := newClaudeStreamErrorTestContext(format, "data: "+strings.Join(frames, "\n\ndata: ")+"\n\n")
					var usage *dto.Usage
					var handlerErr *types.NewAPIError
					if format == types.RelayFormatOpenAIResponses {
						usage, handlerErr = ClaudeResponsesStreamHandler(c, resp, info)
					} else {
						usage, handlerErr = ClaudeStreamHandler(c, resp, info)
					}
					require.NotNil(t, handlerErr)
					require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
					require.True(t, types.IsSkipRetryError(handlerErr), "malformed accounting must not replay an accepted generation")
					require.False(t, info.ShouldRecordChannelSuccess())
					require.NotContains(t, recorder.Body.String(), "INVALID_CPA_USAGE", "invalid frame leaked to the client")
					require.NotContains(t, recorder.Body.String(), "[DONE]")
					require.NotContains(t, recorder.Body.String(), "response.completed")
					require.NotContains(t, recorder.Body.String(), "event: message_stop")
					if stage == "first" {
						require.Empty(t, recorder.Body.String(), "first malformed frame must stay uncommitted")
					} else {
						assertCPASearchUsageCount(t, usage, cpaSearchUsageCount(2))
						require.Equal(t, 10, usage.PromptTokens)
						require.Equal(t, 5, usage.CompletionTokens)
						require.Equal(t, 15, usage.TotalTokens, "invalid snapshot must not replace prior known token usage")
					}
				})
			}
		}
	}
}

func TestCPAServerToolUsageMessagesDoesNotHealFailedStream(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		t.Run(string(format), func(t *testing.T) {
			frames := []string{
				`{"type":"message_start","message":{"id":"msg_cpa","type":"message","role":"assistant","model":"deepseek-flash","content":[],"usage":{"input_tokens":10,"output_tokens":0,"server_tool_use":{"web_search_requests":2}}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial output"}}`,
				`{"type":"error","error":{"type":"overloaded_error","message":"synthetic failure"}}`,
			}
			recorder, c, info, resp := newClaudeStreamErrorTestContext(format, "data: "+strings.Join(frames, "\n\ndata: ")+"\n\n")
			var handlerErr *types.NewAPIError
			if format == types.RelayFormatOpenAIResponses {
				_, handlerErr = ClaudeResponsesStreamHandler(c, resp, info)
			} else {
				_, handlerErr = ClaudeStreamHandler(c, resp, info)
			}
			require.NotNil(t, handlerErr)
			require.True(t, types.IsSkipRetryError(handlerErr))
			require.NotContains(t, recorder.Body.String(), "[DONE]")
			require.NotContains(t, recorder.Body.String(), "response.completed")
			require.NotContains(t, recorder.Body.String(), "event: message_stop")
			require.Equal(t, http.StatusOK, recorder.Code, "post-output errors must stay in-band")
		})
	}
}
