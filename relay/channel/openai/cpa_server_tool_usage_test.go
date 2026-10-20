package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// CPA reports cumulative server-tool counts separately from token totals.
// The text deliberately resembles usage but is not provider accounting.
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

func assertCPASearchWireCount(t *testing.T, body string, stream, responses bool, want *int) {
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
			if responses {
				prefix = "response."
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

func cpaSearchUsageInfo(stream, responses bool) *relaycommon.RelayInfo {
	format, mode := types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions
	if responses {
		format, mode = types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses
	}
	return &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4.1-flash", RelayFormat: format, RelayMode: mode,
		IsStream: stream, ShouldIncludeUsage: true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "deepseek-flash"},
	}
}

func TestCPAServerToolUsageJSONHandlers(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, tc := range []struct {
			name, value string
			want        *int
		}{
			{"absent", "", nil}, {"null", "null", nil},
			{"zero", "0", cpaSearchUsageCount(0)}, {"positive", "3", cpaSearchUsageCount(3)},
		} {
			for _, forceFormat := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/force_format=%t", protocol, tc.name, forceFormat), func(t *testing.T) {
					c, recorder := newResponsesTestContext()
					responses := protocol == "responses"
					info := cpaSearchUsageInfo(false, responses)
					info.ChannelSetting.ForceFormat = forceFormat
					body := fmt.Sprintf(`{"id":"chat_cpa","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15%s}}`, cpaSearchUsageQuotedText(t), cpaSearchUsageFragment(tc.value))
					if responses {
						body = fmt.Sprintf(`{"id":"resp_cpa","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15%s}}`, cpaSearchUsageQuotedText(t), cpaSearchUsageFragment(tc.value))
					}
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
					var usage *dto.Usage
					var handlerErr *types.NewAPIError
					if responses {
						usage, handlerErr = OaiResponsesHandler(c, info, resp)
					} else {
						usage, handlerErr = OpenaiHandler(c, info, resp)
					}
					require.Nil(t, handlerErr)
					assertCPASearchUsageCount(t, usage, tc.want)
					require.Equal(t, 10, usage.PromptTokens)
					require.Equal(t, 5, usage.CompletionTokens)
					require.Equal(t, 15, usage.TotalTokens, "search counts must not be added to tokens")
					assertCPASearchWireCount(t, recorder.Body.String(), false, responses, tc.want)
				})
			}
		}
	}
}

func TestCPAServerToolUsageSSECumulativeSnapshots(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
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
			{"initial_positive_only", nil, cpaSearchUsageCount(3), "3"},
			{"initial_zero_only", nil, cpaSearchUsageCount(0), "0"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				c, recorder := newResponsesTestContext()
				responses := protocol == "responses"
				info := cpaSearchUsageInfo(true, responses)
				info.ChannelSetting.ForceFormat = true
				frames := []string{fmt.Sprintf(`{"id":"chat_cpa","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":%s},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10%s}}`, cpaSearchUsageQuotedText(t), cpaSearchUsageFragment(tc.initial))}
				if responses {
					frames = []string{
						`{"type":"response.created","response":{"id":"resp_cpa","status":"in_progress","usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10` + cpaSearchUsageFragment(tc.initial) + `}}}`,
						fmt.Sprintf(`{"type":"response.output_text.delta","delta":%s}`, cpaSearchUsageQuotedText(t)),
					}
				}
				for _, value := range tc.values {
					frame := `{"id":"chat_cpa","object":"chat.completion.chunk","choices":[],"usage":{"server_tool_use":{"web_search_requests":` + value + `}}}`
					if responses {
						frame = `{"type":"response.in_progress","response":{"id":"resp_cpa","status":"in_progress","usage":{"server_tool_use":{"web_search_requests":` + value + `}}}}`
					}
					frames = append(frames, frame)
				}
				if responses {
					frames = append(frames, `{"type":"response.completed","response":{"id":"resp_cpa","status":"completed","usage":{"output_tokens":5}}}`)
				} else {
					frames = append(frames, `{"id":"chat_cpa","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":5}}`, "[DONE]")
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"))}
				var usage *dto.Usage
				var handlerErr *types.NewAPIError
				if responses {
					usage, handlerErr = OaiResponsesStreamHandler(c, info, resp)
				} else {
					usage, handlerErr = OaiStreamHandler(c, info, resp)
				}
				require.Nil(t, handlerErr)
				assertCPASearchUsageCount(t, usage, tc.want)
				require.Equal(t, 10, usage.PromptTokens)
				require.Equal(t, 5, usage.CompletionTokens)
				require.Equal(t, 15, usage.TotalTokens)
				assertCPASearchWireCount(t, recorder.Body.String(), true, responses, tc.want)
				if responses {
					require.Contains(t, recorder.Body.String(), "response.completed")
				} else {
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "[DONE]"))
				}
			})
		}
	}
}

func TestCPAServerToolUsageRejectsInvalidJSONCounts(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, value := range []string{"-1", "1.5", "1.0", `"1"`, "true", "9223372036854775808", "1e999"} {
			t.Run(protocol+"/"+value, func(t *testing.T) {
				c, recorder := newResponsesTestContext()
				responses := protocol == "responses"
				info := cpaSearchUsageInfo(false, responses)
				body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15` + cpaSearchUsageFragment(value) + `}}`
				if responses {
					body = `{"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15` + cpaSearchUsageFragment(value) + `}}`
				}
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
				var handlerErr *types.NewAPIError
				if responses {
					_, handlerErr = OaiResponsesHandler(c, info, resp)
				} else {
					_, handlerErr = OpenaiHandler(c, info, resp)
				}
				require.NotNil(t, handlerErr)
				require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
				require.True(t, types.IsSkipRetryError(handlerErr), "malformed accounting must not replay an accepted generation")
				require.Empty(t, recorder.Body.String(), "invalid accounting must not be returned as successful JSON")
			})
		}
	}
}

func TestCPAServerToolUsageRejectsInvalidSSECounts(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stage := range []string{"first", "after_output", "final"} {
			for _, value := range []string{"-1", "1.5", "9223372036854775808", "1e999", `"1"`, "true", "{}", "[]"} {
				t.Run(protocol+"/"+stage+"/"+value, func(t *testing.T) {
					c, recorder := newResponsesTestContext()
					responses := protocol == "responses"
					info := cpaSearchUsageInfo(true, responses)
					var frames []string
					if stage != "first" {
						frames = []string{
							`{"choices":[{"index":0,"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"server_tool_use":{"web_search_requests":2}}}`,
							`{"choices":[{"index":0,"delta":{"content":" more"}}]}`,
						}
						if responses {
							frames = []string{
								`{"type":"response.created","response":{"id":"resp_cpa","status":"in_progress","usage":{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":2}}}}`,
								`{"type":"response.output_text.delta","delta":"ok"}`,
							}
						}
					}
					badFrame := `{"vendor_marker":"INVALID_CPA_USAGE","choices":[{"index":0,"delta":{"content":"invalid usage"},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":7` + cpaSearchUsageFragment(value) + `}}`
					if stage == "final" {
						badFrame = `{"vendor_marker":"INVALID_CPA_USAGE","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7` + cpaSearchUsageFragment(value) + `}}`
					}
					if responses {
						event, status := "response.in_progress", "in_progress"
						if stage == "first" {
							event = "response.created"
						} else if stage == "final" {
							event, status = "response.completed", "completed"
						}
						badFrame = fmt.Sprintf(`{"type":%q,"vendor_marker":"INVALID_CPA_USAGE","response":{"id":"resp_cpa","status":%q,"usage":{"input_tokens":10,"output_tokens":7%s}}}`, event, status, cpaSearchUsageFragment(value))
					}
					frames = append(frames, badFrame)
					if responses {
						frames = append(frames, `{"type":"response.completed","response":{"id":"resp_cpa","status":"completed","usage":{"input_tokens":10,"output_tokens":7}}}`)
					} else {
						frames = append(frames, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`, "[DONE]")
					}
					resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"))}
					var usage *dto.Usage
					var handlerErr *types.NewAPIError
					if responses {
						usage, handlerErr = OaiResponsesStreamHandler(c, info, resp)
					} else {
						usage, handlerErr = OaiStreamHandler(c, info, resp)
					}
					require.NotNil(t, handlerErr, "invalid accounting was accepted as a successful stream")
					require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
					require.True(t, types.IsSkipRetryError(handlerErr), "malformed accounting must not replay an accepted generation")
					require.False(t, info.ShouldRecordChannelSuccess())
					require.NotContains(t, recorder.Body.String(), "INVALID_CPA_USAGE", "invalid frame leaked to the client")
					require.NotContains(t, recorder.Body.String(), "[DONE]")
					require.NotContains(t, recorder.Body.String(), "response.completed")
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

func TestCPAServerToolUsageDoesNotTurnFailedStreamIntoSuccess(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			c, recorder := newResponsesTestContext()
			responses := protocol == "responses"
			info := cpaSearchUsageInfo(true, responses)
			frames := []string{
				`{"choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"server_tool_use":{"web_search_requests":2}}}`,
				`{"choices":[{"index":0,"delta":{"content":" output"}}]}`,
				`{"error":{"code":"upstream_error","message":"synthetic failure"}}`,
			}
			if responses {
				frames = []string{
					`{"type":"response.created","response":{"id":"resp_cpa","status":"in_progress","usage":{"input_tokens":10,"output_tokens":2,"server_tool_use":{"web_search_requests":2}}}}`,
					`{"type":"response.output_text.delta","delta":"partial"}`,
					`{"type":"response.failed","response":{"id":"resp_cpa","status":"failed","error":{"code":"upstream_error","message":"synthetic failure"}}}`,
				}
			}
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"))}
			var usage *dto.Usage
			var handlerErr *types.NewAPIError
			if responses {
				usage, handlerErr = OaiResponsesStreamHandler(c, info, resp)
			} else {
				usage, handlerErr = OaiStreamHandler(c, info, resp)
			}
			require.NotNil(t, handlerErr)
			require.True(t, types.IsSkipRetryError(handlerErr))
			assertCPASearchUsageCount(t, usage, cpaSearchUsageCount(2))
			require.Equal(t, 12, usage.TotalTokens)
			require.NotContains(t, recorder.Body.String(), "[DONE]")
			require.NotContains(t, recorder.Body.String(), "response.completed")
		})
	}
}
