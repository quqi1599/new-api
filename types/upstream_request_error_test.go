package types

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStructuredRequestErrorsNeverRetryOrPenalizeChannel(t *testing.T) {
	for _, code := range []any{"1301", float64(1301), "content_policy_violation", "request_feature_unsupported"} {
		for _, err := range []*NewAPIError{
			WithOpenAIError(OpenAIError{Code: code, Type: "invalid_request_error", Message: "fixture"}, 502, ErrOptionWithChannelPenalty()),
			WithClaudeError(ClaudeError{Code: code, Type: "invalid_request_error", Message: "fixture"}, 500, ErrOptionWithChannelPenalty()),
		} {
			require.Equal(t, http.StatusBadRequest, err.StatusCode)
			require.True(t, IsSkipRetryError(err))
			require.False(t, IsChannelPenaltyAllowed(err))
			require.Contains(t, []ErrorCode{"content_policy_violation", "request_feature_unsupported"}, err.GetErrorCode())
		}
	}
	unknown := WithOpenAIError(OpenAIError{Code: "invalid_request_error", Message: "1301 content_policy_violation"}, 502)
	require.Equal(t, 502, unknown.StatusCode, "message text alone must not classify safety")
}
