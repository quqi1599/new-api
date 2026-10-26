package controller

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONModeSkipsMessagesBeforeUpstreamAttempt(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, chat := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("cache=%t/chat=%t/stream=%t", cached, chat, stream), func(t *testing.T) {
					got := runDistributorResilience(t, distributorResilienceFixture{
						cached: cached, candidates: []string{"messages", "healthy"}, chat: chat, nonStream: !stream, jsonObject: true,
					})
					require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
					require.Equal(t, []int{10}, got.calls)
					require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
					require.Equal(t, 1, got.context.GetInt("retry_candidate_skip_count"))
					require.Equal(t, "output_format_route_unsupported", got.context.GetString("retry_candidate_skip_reason"))
				})
			}
		}
	}
}

func TestJSONModeForcedMessagesChannelDoesNotSwitch(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			got := runDistributorResilience(t, distributorResilienceFixture{
				cached: cached, candidates: []string{"messages", "healthy"}, chat: true, jsonObject: true, forced: true,
			})
			require.Equal(t, http.StatusUnprocessableEntity, got.response.Code, got.response.Body.String())
			require.Empty(t, got.calls)
			require.Zero(t, got.context.GetInt("retry_attempt_no"))
		})
	}
}

func TestJSONModeRetrySkipsIncompatibleRouteWithoutAnotherAttempt(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			got := runDistributorResilience(t, distributorResilienceFixture{
				cached: cached, candidates: []string{"error", "messages-retry"}, chat: true, jsonObject: true,
			})
			require.Equal(t, http.StatusServiceUnavailable, got.response.Code, got.response.Body.String())
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
			require.Equal(t, 1, got.context.GetInt("retry_candidate_skip_count"))
		})
	}
}
