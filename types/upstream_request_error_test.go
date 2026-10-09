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

func TestCPAStateOwnershipPreservesEnvelopeAndOverridesPenalty(t *testing.T) {
	for _, code := range []string{"state_owner_unavailable", "state_owner_conflict", "state_owner_expired", "local_state_unavailable"} {
		// HTTP uses 409. A Responses SSE error adapter may already have assigned
		// 502; classification must still avoid a channel penalty or replay.
		for _, status := range []int{http.StatusConflict, http.StatusBadGateway} {
			for _, err := range []*NewAPIError{
				WithOpenAIError(OpenAIError{Code: code, Type: "invalid_request_error", Message: "retain original context"}, status, ErrOptionWithUpstreamResponse(), ErrOptionWithChannelPenalty()),
				WithClaudeError(ClaudeError{Code: code, Type: "invalid_request_error", Message: "retain original context"}, status, ErrOptionWithUpstreamResponse(), ErrOptionWithChannelPenalty()),
			} {
				require.Equal(t, status, err.StatusCode)
				require.Equal(t, ErrorCode(code), err.GetErrorCode())
				require.Equal(t, "retain original context", err.Error())
				require.True(t, err.HasUpstreamResponse())
				require.True(t, IsSkipRetryError(err))
				require.False(t, IsChannelPenaltyAllowed(err))
			}
		}
	}
	for _, code := range []string{"resource_conflict", "state_owner_future_transient_error", "server_error"} {
		err := WithOpenAIError(OpenAIError{Code: code, Message: "state_owner_unavailable state_owner_conflict state_owner_expired local_state_unavailable"}, http.StatusConflict)
		require.False(t, IsSkipRetryError(err), "neither status, prefix nor message text establishes this contract")
		require.True(t, IsChannelPenaltyAllowed(err))
	}
}
