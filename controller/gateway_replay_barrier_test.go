package controller

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func replayBarrierFixture(reason string, status int, stream, truncated bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-CPA-Retry-Scope", "request")
		w.Header().Set("X-CPA-Retry-Reason", reason)
		w.Header().Set("X-CPA-Retry-Attempts", "1")
		w.Header().Set("Content-Type", "application/json")
		if truncated {
			w.Header().Set("Content-Length", "1024")
		}
		if stream && status == 200 {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		if truncated {
			_, _ = io.WriteString(w, `{"error":`)
			return
		}
		if stream && status == 200 {
			_, _ = io.WriteString(w, "data: {\"error\":{\"type\":\"server_error\",\"code\":\"upstream_unavailable\",\"message\":\"synthetic\"}}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"error":{"type":"cpa_error","code":"upstream_unavailable","message":"synthetic"}}`)
	})
}

func TestGatewayReplayBarrierStopsEveryChannel(t *testing.T) {
	for _, reason := range []string{service.RetryStopReasonUpstreamOutputObserved, service.RetryStopReasonAcceptedOutcomeUnknown} {
		for _, cached := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, status := range []int{429, 502, 200} {
					if status == 200 && !stream {
						continue
					}
					t.Run(fmt.Sprintf("%s/cached=%t/stream=%t/status=%d", reason, cached, stream, status), func(t *testing.T) {
						got := runCPAStateRelay(t, "upstream_unavailable", status, true, stream, cpaStateAliasOptions{
							trustedCPA: true, cached: cached, maxAttempts: 6,
							ownerHandler: replayBarrierFixture(reason, status, stream, false),
						})
						require.EqualValues(t, 1, got.ownerCalls, got.response.Body.String())
						require.Zero(t, got.alternateCalls)
						require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
						require.Equal(t, reason, got.context.GetString("retry_stop_reason"))
						require.NotEqual(t, 200, got.response.Code)
					})
				}
			}
		}
	}
}

func TestGatewayReplayBarrierSurvivesTruncatedBody(t *testing.T) {
	got := runCPAStateRelay(t, "upstream_unavailable", 502, true, false, cpaStateAliasOptions{
		trustedCPA: true, maxAttempts: 6,
		ownerHandler: replayBarrierFixture(service.RetryStopReasonUpstreamOutputObserved, 502, false, true),
	})
	require.EqualValues(t, 1, got.ownerCalls)
	require.Zero(t, got.alternateCalls)
	require.Equal(t, service.RetryStopReasonUpstreamOutputObserved, got.context.GetString("retry_stop_reason"))
}

func TestGatewayReplayBarrierDoesNotTrustUnmarkedChannelsOrClientHeaders(t *testing.T) {
	for _, name := range []string{"untrusted_channel", "unknown_reason", "client_reflection", "stale_attempt", "safe_gateway_rejection"} {
		t.Run(name, func(t *testing.T) {
			options := cpaStateAliasOptions{trustedCPA: true, maxAttempts: 6, ownerHeaderOverride: `{"*":true}`}
			options.ownerHandler = replayBarrierFixture(service.RetryStopReasonUpstreamOutputObserved, 429, false, false)
			switch name {
			case "untrusted_channel":
				options.trustedCPA = false
			case "unknown_reason":
				options.ownerHandler = replayBarrierFixture("unknown", 429, false, false)
			case "client_reflection":
				options.requestHeaders = http.Header{"X-Cpa-Retry-Scope": {"request"}, "X-Cpa-Retry-Reason": {service.RetryStopReasonUpstreamOutputObserved}, "X-Cpa-Retry-Attempts": {"1"}}
				options.ownerHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for _, key := range []string{"X-CPA-Retry-Scope", "X-CPA-Retry-Reason", "X-CPA-Retry-Attempts"} {
						require.Empty(t, r.Header.Get(key), "reserved contract request header leaked")
						if value := r.Header.Get(key); value != "" {
							w.Header().Set(key, value)
						}
					}
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"upstream_unavailable","message":"synthetic"}}`)
				})
			case "stale_attempt":
				options.staleReplayBarrier = service.RetryStopReasonUpstreamOutputObserved
				options.ownerHandler = gatewayExhaustionHandler(429, true)
			case "safe_gateway_rejection":
				options.ownerHandler = gatewayExhaustionHandler(429, true)
			}
			got := runCPAStateRelay(t, "upstream_unavailable", 429, true, false, options)
			require.EqualValues(t, 1, got.alternateCalls, got.response.Body.String())
			require.Equal(t, 200, got.response.Code)
			require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
			require.Empty(t, service.GatewayReplayStopReason(got.context))
		})
	}
}
