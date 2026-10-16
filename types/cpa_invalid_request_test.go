package types

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCPAInvalidRequestExactHTTPContract(t *testing.T) {
	for _, kind := range []string{"cpa_error", "api_error", "invalid_request_error"} {
		for _, status := range []int{400, 422, 500, 502} {
			for _, upstream := range []bool{false, true} {
				options := []NewAPIErrorOptions{ErrOptionWithChannelPenalty()}
				if upstream {
					options = append(options, ErrOptionWithUpstreamResponse())
				}
				for _, err := range []*NewAPIError{
					WithOpenAIError(OpenAIError{Code: "upstream_invalid_request", Type: kind, Message: "retain original"}, status, options...),
					WithClaudeError(ClaudeError{Code: "upstream_invalid_request", Type: kind, Message: "retain original"}, status, options...),
				} {
					want := status == http.StatusUnprocessableEntity && upstream
					require.Equal(t, want, IsSkipRetryError(err))
					require.Equal(t, !want, IsChannelPenaltyAllowed(err))
					require.Equal(t, status, err.StatusCode)
					require.Equal(t, "retain original", err.Error())
				}
			}
		}
	}
	err := WithOpenAIError(OpenAIError{Code: "server_error", Message: "upstream_invalid_request"}, 422, ErrOptionWithUpstreamResponse())
	require.False(t, IsSkipRetryError(err))
}
