package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAcceptedGenerationDecodeFailureUsesEligibleAlternate(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%t", protocol, cached), func(t *testing.T) {
				const message = "synthetic upstream response could not be decoded"
				got := runCapabilityRetryRelay(t, capabilityRetryFixture{
					codes: []string{"upstream_response_decode_failed"}, status: 502,
					cached: cached, healthy: true, protocol: protocol, nonStream: true, message: message,
				})
				t.Logf("upstream_calls=%v status=%d stop=%s", got.calls, got.response.Code, got.context.GetString("retry_stop_reason"))
				require.Equal(t, []int{9, 10}, got.calls)
				require.Equal(t, 200, got.response.Code)
				require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
			})
		}
	}
}

func TestAcceptedGenerationBarriersPreserveOrdinaryFailover(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"server_error", 500}, {"server_error", 502}, {"temporary_overload", 503},
		{"upstream_response_decode_failed", 500}, {"upstream_response_decode_failed", 503},
		{"output_invalid_json", 500}, {"output_invalid_json_suffix", 502},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.code, tc.status), func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{
				codes: []string{tc.code}, status: tc.status, healthy: true,
				protocol: "chat", nonStream: true, message: "earlier upstream_response_decode_failed output_invalid_json",
			})
			require.Equal(t, []int{9, 10}, got.calls)
			require.Equal(t, http.StatusOK, got.response.Code)
		})
	}
}

func TestAcceptedGenerationDecodeRemapRetainsCircuitPolicy(t *testing.T) {
	err := service.RelayErrorHandler(t.Context(), &http.Response{
		StatusCode: 502, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","code":"upstream_response_decode_failed","message":"synthetic decode error"}}`)),
	}, false)
	require.NotNil(t, err)
	require.False(t, types.IsSkipRetryError(err))
	require.True(t, types.IsChannelPenaltyAllowed(err))
	require.True(t, service.ShouldCountChannelCircuitFailure(err))
	service.ResetStatusCode(err, `{"502":"503"}`)
	require.Equal(t, 503, err.StatusCode)
	require.False(t, types.IsSkipRetryError(err))
	require.True(t, service.ShouldCountChannelCircuitFailure(err))
}

func TestAcceptedGenerationClaudeOutputSSEDoesNotPenalize(t *testing.T) {
	oldEnabled, oldRanges := common.AutomaticDisableChannelEnabled, operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, started := range []bool{false, true} {
			for _, code := range []string{"output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool"} {
				t.Run(fmt.Sprintf("%s/started=%t/%s", format, started, code), func(t *testing.T) {
					const message = "synthetic generated output failed validation"
					sse := ""
					if started {
						sse = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
					}
					sse += fmt.Sprintf("data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"code\":%q,\"message\":%q}}\n\n", code, message)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
					info := &relaycommon.RelayInfo{RelayFormat: format, IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"}}
					resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
					var err *types.NewAPIError
					if format == types.RelayFormatOpenAIResponses {
						_, err = claude.ClaudeResponsesStreamHandler(c, resp, info)
					} else {
						_, err = claude.ClaudeStreamHandler(c, resp, info)
					}
					require.NotNil(t, err)
					t.Logf("status=%d code=%s skip=%t penalty=%t circuit=%t", err.StatusCode, err.GetErrorCode(), types.IsSkipRetryError(err), types.IsChannelPenaltyAllowed(err), service.ShouldCountChannelCircuitFailure(err))
					require.Equal(t, 500, err.StatusCode)
					require.Equal(t, types.ErrorCode(code), err.GetErrorCode())
					require.Equal(t, message, err.Error())
					require.True(t, types.IsSkipRetryError(err))
					require.False(t, types.IsChannelPenaltyAllowed(err))
					require.False(t, service.ShouldCountChannelCircuitFailure(err))
					require.False(t, service.ShouldDisableChannel(err))
					require.False(t, shouldRetry(c, err, 3, format))
					if started {
						require.Contains(t, recorder.Body.String(), code)
					} else {
						require.False(t, c.Writer.Written())
					}
					require.NotContains(t, recorder.Body.String(), "response.completed")
					require.NotContains(t, recorder.Body.String(), "[DONE]")
					service.ResetStatusCode(err, `{"500":"503"}`)
					require.True(t, types.IsSkipRetryError(err))
					require.False(t, service.ShouldCountChannelCircuitFailure(err))
				})
			}
		}
	}
}
