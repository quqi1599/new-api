package service

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayReplayBarrierContract(t *testing.T) {
	for _, reason := range []string{RetryStopReasonUpstreamOutputObserved, RetryStopReasonAcceptedOutcomeUnknown} {
		for _, status := range []int{200, 299, 400, 403, 429, 500, 502, 599} {
			header := http.Header{"X-Cpa-Retry-Scope": {"request"}, "X-Cpa-Retry-Reason": {reason}, "X-Cpa-Retry-Attempts": {"1"}}
			require.Equal(t, reason, ParseGatewayReplayBarrier(header, status))
		}
	}
	for _, tc := range []struct {
		name   string
		key    string
		values []string
		status int
	}{
		{"scope absent", "X-CPA-Retry-Scope", nil, 502},
		{"scope gateway", "X-CPA-Retry-Scope", []string{"gateway"}, 502},
		{"scope duplicate", "X-CPA-Retry-Scope", []string{"request", "request"}, 502},
		{"scope combined", "X-CPA-Retry-Scope", []string{"request,gateway"}, 502},
		{"reason absent", "X-CPA-Retry-Reason", nil, 502},
		{"reason unknown", "X-CPA-Retry-Reason", []string{"some_provider_text"}, 502},
		{"reason duplicate", "X-CPA-Retry-Reason", []string{RetryStopReasonUpstreamOutputObserved, RetryStopReasonUpstreamOutputObserved}, 502},
		{"reason combined", "X-CPA-Retry-Reason", []string{RetryStopReasonUpstreamOutputObserved + ",other"}, 502},
		{"reason newline", "X-CPA-Retry-Reason", []string{RetryStopReasonUpstreamOutputObserved + "\nInjected: true"}, 502},
		{"attempt absent", "X-CPA-Retry-Attempts", nil, 502},
		{"attempt zero", "X-CPA-Retry-Attempts", []string{"0"}, 502},
		{"attempt negative", "X-CPA-Retry-Attempts", []string{"-1"}, 502},
		{"attempt signed", "X-CPA-Retry-Attempts", []string{"+1"}, 502},
		{"attempt padded", "X-CPA-Retry-Attempts", []string{"01"}, 502},
		{"attempt duplicate", "X-CPA-Retry-Attempts", []string{"1", "1"}, 502},
		{"attempt overflow", "X-CPA-Retry-Attempts", []string{"999999999999999999999999999999999999999"}, 502},
		{"attempt over bound", "X-CPA-Retry-Attempts", []string{"1048577"}, 502},
		{"redirect", "", nil, 302},
		{"invalid status", "", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"X-Cpa-Retry-Scope": {"request"}, "X-Cpa-Retry-Reason": {RetryStopReasonUpstreamOutputObserved}, "X-Cpa-Retry-Attempts": {"1"}}
			if tc.key != "" {
				h[http.CanonicalHeaderKey(tc.key)] = tc.values
			}
			require.Empty(t, ParseGatewayReplayBarrier(h, tc.status))
		})
	}
	require.Empty(t, ParseGatewayReplayBarrier(nil, 502))
}

func TestGatewayReplayBarrierReadsOnlyOwnedMetadata(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Request, _ = http.NewRequest(http.MethodPost, "https://client.invalid/v1/responses", nil)
	c.Request.Header.Set("X-CPA-Retry-Scope", "request")
	c.Request.Header.Set("X-CPA-Retry-Reason", RetryStopReasonUpstreamOutputObserved)
	c.Request.Header.Set("X-CPA-Retry-Attempts", "1")
	require.Empty(t, GatewayReplayStopReason(c))
	common.SetContextKey(c, constant.ContextKeyRelayGatewayNoReplayReason, "arbitrary")
	require.Empty(t, GatewayReplayStopReason(c))
	common.SetContextKey(c, constant.ContextKeyRelayGatewayNoReplayReason, RetryStopReasonAcceptedOutcomeUnknown)
	require.Equal(t, RetryStopReasonAcceptedOutcomeUnknown, GatewayReplayStopReason(c))
	require.Empty(t, GatewayReplayStopReason(nil))
}
