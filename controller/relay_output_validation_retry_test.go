package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var generatedOutputFailureCodes = []string{"output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool"}

func TestGeneratedOutputValidationFailureNeverRegeneratesOnAnotherChannel(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		for _, code := range generatedOutputFailureCodes {
			t.Run(protocol+"/"+code, func(t *testing.T) {
				const message = "上游输出未满足所请求的格式，请保留本次失败信息。"
				got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{code}, status: 502, healthy: true, protocol: protocol, nonStream: true, message: message})
				t.Logf("generated_entries=%v final_status=%d stop=%s", got.calls, got.response.Code, got.context.GetString("retry_stop_reason"))
				require.Equal(t, []int{9}, got.calls, "CPA has already generated the rejected output; another channel would generate again")
				require.Equal(t, http.StatusBadGateway, got.response.Code)
				require.Contains(t, got.response.Body.String(), code)
				require.Contains(t, got.response.Body.String(), message)
				require.Equal(t, service.RetryStopReasonNotRetryable, got.context.GetString("retry_stop_reason"))
			})
		}
	}
}

func TestGeneratedOutputValidationPreservesUnrelatedPreOutputFallback(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{{"server_error", 502}, {"temporary_overload", 503}, {"native_response_conversion_failed", 502}, {"output_invalid_json", 503}} {
		t.Run(fmt.Sprintf("%s/%d", tc.code, tc.status), func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{tc.code}, status: tc.status, healthy: true, nonStream: true, message: "earlier output_invalid_json"})
			require.Equal(t, []int{9, 10}, got.calls)
			require.Equal(t, http.StatusOK, got.response.Code)
		})
	}
}

func TestGeneratedOutputValidationSSEKeepsCodeAndDoesNotPenalizeChannel(t *testing.T) {
	for _, code := range generatedOutputFailureCodes {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/started=%t", code, started), func(t *testing.T) {
				prefix := ""
				if started {
					prefix = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_output\",\"status\":\"in_progress\"}}\n\n"
				}
				data := prefix + fmt.Sprintf("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_output\",\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":\"上游输出校验失败\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n", code)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				info := &relaycommon.RelayInfo{IsStream: true, OriginModelName: "gpt-5.6-sol", ChannelMeta: &relaycommon.ChannelMeta{}}
				usage, err := openai.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))})
				require.NotNil(t, err)
				require.Equal(t, types.ErrorCode(code), err.GetErrorCode())
				require.Equal(t, 502, err.StatusCode)
				require.True(t, types.IsSkipRetryError(err))
				require.False(t, types.IsChannelPenaltyAllowed(err))
				require.False(t, service.ShouldCountChannelCircuitFailure(err))
				require.Equal(t, 3, usage.PromptTokens)
				require.Equal(t, 2, usage.CompletionTokens)
				require.False(t, shouldRetry(c, err, 3, types.RelayFormatOpenAIResponses))
			})
		}
	}
}
