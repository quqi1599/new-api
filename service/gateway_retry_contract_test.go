package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestRelayErrorHandlerGatewayRetryContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		scope    []string
		attempts []string
		want     int
	}{
		{"429", 429, []string{"gateway"}, []string{"3"}, 3},
		{"500", 500, []string{"gateway"}, []string{"1"}, 1},
		{"503", 503, []string{"gateway"}, []string{"8"}, 8},
		{"599", 599, []string{"gateway"}, []string{"2"}, 2},
		{"missing scope", 429, nil, []string{"3"}, 0},
		{"missing attempts", 429, []string{"gateway"}, nil, 0},
		{"unknown scope", 429, []string{"request"}, []string{"3"}, 0},
		{"duplicate scope", 429, []string{"gateway", "gateway"}, []string{"3"}, 0},
		{"duplicate attempts", 429, []string{"gateway"}, []string{"3", "3"}, 0},
		{"zero attempts", 429, []string{"gateway"}, []string{"0"}, 0},
		{"nine attempts", 429, []string{"gateway"}, []string{"9"}, 9},
		{"large pool", 503, []string{"gateway"}, []string{"25"}, 25},
		{"wire bound", 503, []string{"gateway"}, []string{"1048576"}, 1048576},
		{"unbounded attempts", 429, []string{"gateway"}, []string{"1048577"}, 0},
		{"overflow attempts", 503, []string{"gateway"}, []string{"99999999999999999999999999999"}, 0},
		{"signed attempts", 503, []string{"gateway"}, []string{"+25"}, 0},
		{"invalid attempts", 429, []string{"gateway"}, []string{"3, 3"}, 0},
		{"noncanonical attempts", 429, []string{"gateway"}, []string{"03"}, 0},
		{"success status", 200, []string{"gateway"}, []string{"3"}, 0},
		{"request rejection", 400, []string{"gateway"}, []string{"3"}, 0},
		{"authentication rejection", 401, []string{"gateway"}, []string{"3"}, 0},
		{"capability rejection", 422, []string{"gateway"}, []string{"3"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Header: http.Header{
					"X-Cpa-Retry-Scope":    tc.scope,
					"X-Cpa-Retry-Attempts": tc.attempts,
					"Retry-After":          []string{"17"},
				},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"upstream rate limited","type":"cpa_error","code":"upstream_unavailable"}}`)),
			}
			got := RelayErrorHandler(context.Background(), resp, false)
			require.Equal(t, tc.want, got.GatewayRetryAttempts())
			require.Equal(t, tc.status, got.StatusCode)
			require.Equal(t, types.ErrorCode("upstream_unavailable"), got.GetErrorCode())
			require.Equal(t, "17", got.GetRetryAfter())
			require.True(t, got.HasUpstreamResponse())
			require.False(t, types.IsSkipRetryError(got), "another independent channel may still work")
			ResetStatusCode(got, `{"429":503,"503":429}`)
			require.Equal(t, tc.want, got.GatewayRetryAttempts(), "status mapping must not erase the original contract")
		})
	}
}

func TestGatewayRetryContractCannotMarkLocalFailure(t *testing.T) {
	got := types.NewErrorWithStatusCode(io.ErrUnexpectedEOF, types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway)
	got.SetGatewayRetryAttempts(3)
	got.SetRetryAfter("17\r\nX-Other: true")
	require.Zero(t, got.GatewayRetryAttempts())
	require.Empty(t, got.GetRetryAfter())
}

func TestGatewayRetryConcurrencyRequiresExplicitReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
		want    int
	}{
		{"explicit", []string{types.GatewayRetryReasonKimiConcurrency}, 3},
		{"missing", nil, 0},
		{"generic permission", []string{"permission_denied"}, 0},
		{"duplicate", []string{types.GatewayRetryReasonKimiConcurrency, types.GatewayRetryReasonKimiConcurrency}, 0},
		{"combined", []string{types.GatewayRetryReasonKimiConcurrency + ",other"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, truncated := range []bool{false, true} {
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"error":{"code":"concurrent_request_limit","type":"cpa_error","message":"safe"}}`))
				if truncated {
					body = &relayErrorReadCloser{err: io.ErrUnexpectedEOF}
				}
				resp := &http.Response{StatusCode: 403, Header: http.Header{
					"X-Cpa-Retry-Scope": {"gateway"}, "X-Cpa-Retry-Attempts": {"3"}, "X-Cpa-Retry-Reason": tc.reasons,
				}, Body: body}
				got := RelayErrorHandler(context.Background(), resp, false)
				require.Equal(t, tc.want, got.GatewayRetryAttempts())
				if tc.want > 0 {
					require.Equal(t, 403, got.StatusCode)
					require.True(t, got.HasUpstreamResponse())
				}
			}
		})
	}
}

func TestGatewayRetryContractSurvivesTruncatedErrorBody(t *testing.T) {
	for _, marked := range []bool{false, true} {
		for _, status := range []int{429, 503} {
			for _, typed := range []bool{false, true} {
				header := make(http.Header)
				if marked {
					header.Set("X-CPA-Retry-Scope", "gateway")
					header.Set("X-CPA-Retry-Attempts", "3")
				}
				header.Set("Retry-After", "17")
				var bodyErr error = io.ErrUnexpectedEOF
				if typed {
					bodyErr = types.NewErrorWithStatusCode(io.ErrUnexpectedEOF, types.ErrorCodeReadResponseBodyFailed, 408)
				}
				resp := &http.Response{StatusCode: status, Header: header, Body: &relayErrorReadCloser{err: bodyErr}}
				got := RelayErrorHandler(context.Background(), resp, false)
				require.Contains(t, got.Error(), io.ErrUnexpectedEOF.Error())
				require.Equal(t, marked, got.HasUpstreamResponse())
				require.Equal(t, "17", got.GetRetryAfter())
				if marked {
					require.Equal(t, 3, got.GatewayRetryAttempts())
					require.Equal(t, status, got.StatusCode)
				} else {
					require.Zero(t, got.GatewayRetryAttempts())
					if typed {
						require.Equal(t, 408, got.StatusCode)
					} else {
						require.Equal(t, 502, got.StatusCode)
					}
				}
			}
		}
	}
}
