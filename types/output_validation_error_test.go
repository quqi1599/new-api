package types

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeneratedOutputValidation502PreservesEnvelopeWithoutReplayOrPenalty(t *testing.T) {
	for _, code := range []string{"output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool"} {
		const message = "上游输出校验失败，请保留诊断信息。"
		for _, err := range []*NewAPIError{
			WithOpenAIError(OpenAIError{Code: code, Type: "server_error", Message: message}, http.StatusBadGateway, ErrOptionWithUpstreamResponse(), ErrOptionWithChannelPenalty()),
			WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: message}, http.StatusBadGateway, ErrOptionWithChannelPenalty()),
		} {
			require.Equal(t, http.StatusBadGateway, err.StatusCode)
			require.Equal(t, ErrorCode(code), err.GetErrorCode())
			require.Equal(t, message, err.Error())
			require.True(t, IsSkipRetryError(err))
			require.False(t, IsChannelPenaltyAllowed(err))
		}
		for _, status := range []int{400, 409, 422, 503} {
			err := WithOpenAIError(OpenAIError{Code: code, Message: message}, status)
			require.False(t, IsSkipRetryError(err), "an unverified status must not inherit the 502 contract")
			require.True(t, IsChannelPenaltyAllowed(err))
		}
	}
	for _, code := range []string{"server_error", "native_response_conversion_failed", "output_validation_future", "output_invalid_json_suffix"} {
		err := WithOpenAIError(OpenAIError{Code: code, Message: "output_invalid_json output_schema_mismatch output_not_json_object output_stream_mismatch output_unknown_tool"}, 502)
		require.False(t, IsSkipRetryError(err))
		require.True(t, IsChannelPenaltyAllowed(err))
	}
}
