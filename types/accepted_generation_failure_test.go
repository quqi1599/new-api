package types

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptedGenerationDecode502PreservesEnvelopeAndHealthPolicy(t *testing.T) {
	const code = "upstream_response_decode_failed"
	const message = "synthetic upstream response decoding failed"
	for _, claude := range []bool{false, true} {
		for _, status := range []int{400, 422, 500, 502, 503, 504} {
			t.Run(fmt.Sprintf("claude=%t/status=%d", claude, status), func(t *testing.T) {
				var err *NewAPIError
				if claude {
					err = WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: message}, status, ErrOptionWithUpstreamResponse())
					require.Equal(t, ClaudeError{Code: code, Type: "api_error", Message: message}, err.ToClaudeError())
				} else {
					err = WithOpenAIError(OpenAIError{Code: code, Type: "server_error", Message: message}, status, ErrOptionWithUpstreamResponse())
					require.Equal(t, OpenAIError{Code: code, Type: "server_error", Message: message}, err.ToOpenAIError())
				}
				require.Equal(t, status, err.StatusCode)
				require.Equal(t, ErrorCode(code), err.GetErrorCode())
				require.Equal(t, message, err.Error())
				require.False(t, IsSkipRetryError(err), "decode recovery is decided using request context, not a global error flag")
				require.True(t, IsChannelPenaltyAllowed(err), "disabling replay must retain protocol failure health policy")
			})
		}
	}
	for _, code := range []string{"server_error", "upstream_response_decode_failed_suffix", "upstream_response_read_failed", "invalid_claude_sse_event"} {
		err := WithOpenAIError(OpenAIError{Code: code, Message: message + " upstream_response_decode_failed"}, 502)
		require.False(t, IsSkipRetryError(err), "only the verified exact code is included in this change")
		require.True(t, IsChannelPenaltyAllowed(err))
	}
}

func TestAcceptedGenerationDecodePreservesCallerPenaltyOptions(t *testing.T) {
	for _, claude := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			options []NewAPIErrorOptions
			penalty bool
		}{
			{"default", nil, true},
			{"skip without penalty", []NewAPIErrorOptions{ErrOptionWithSkipRetry()}, false},
			{"skip with penalty", []NewAPIErrorOptions{ErrOptionWithSkipRetry(), ErrOptionWithChannelPenalty()}, true},
		} {
			t.Run(fmt.Sprintf("claude=%t/%s", claude, tc.name), func(t *testing.T) {
				var err *NewAPIError
				if claude {
					err = WithClaudeError(ClaudeError{Code: "upstream_response_decode_failed", Type: "api_error", Message: "synthetic"}, 502, tc.options...)
				} else {
					err = WithOpenAIError(OpenAIError{Code: "upstream_response_decode_failed", Type: "server_error", Message: "synthetic"}, 502, tc.options...)
				}
				require.Equal(t, tc.name != "default", IsSkipRetryError(err))
				require.Equal(t, tc.penalty, IsChannelPenaltyAllowed(err), "retain the caller's existing health policy")
			})
		}
	}
}

func TestAcceptedGenerationClaudeOutput500PreservesEnvelopeWithoutPenalty(t *testing.T) {
	const message = "synthetic generated output failed validation"
	for _, code := range []string{"output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool"} {
		t.Run(code, func(t *testing.T) {
			err := WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: message}, 500, ErrOptionWithSkipRetry(), ErrOptionWithChannelPenalty())
			require.Equal(t, 500, err.StatusCode)
			require.Equal(t, ClaudeError{Code: code, Type: "api_error", Message: message}, err.ToClaudeError())
			require.True(t, IsSkipRetryError(err))
			require.False(t, IsChannelPenaltyAllowed(err))
			for _, status := range []int{400, 422, 503, 504} {
				unverified := WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: message}, status)
				require.False(t, IsSkipRetryError(unverified))
				require.True(t, IsChannelPenaltyAllowed(unverified))
			}
			openAI := WithOpenAIError(OpenAIError{Code: code, Type: "server_error", Message: message}, 500)
			require.False(t, IsSkipRetryError(openAI), "Claude's synthetic 500 mapping does not establish an OpenAI 500 contract")
			require.True(t, IsChannelPenaltyAllowed(openAI))
		})
	}
	for _, code := range []string{"overloaded_error", "output_invalid_json_suffix", "output_validation_future"} {
		err := WithClaudeError(ClaudeError{Code: code, Type: "api_error", Message: "earlier output_invalid_json"}, 500, ErrOptionWithSkipRetry(), ErrOptionWithChannelPenalty())
		require.True(t, IsChannelPenaltyAllowed(err))
	}
}
