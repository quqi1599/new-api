package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestGPTChannelFallbackStopsAfterOutputOrOneFallback(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.5"}
	err := types.NewErrorWithStatusCode(errors.New("rate limited"), types.ErrorCodeChannelNoAvailableKey, http.StatusTooManyRequests)
	if !isGPTChannelFallbackError(info, err) || !canRetryGPTChannelFallback(info, 1) {
		t.Fatal("expected one pre-output GPT 429 fallback")
	}
	if canRetryGPTChannelFallback(info, 2) {
		t.Fatal("must allow at most one fallback channel")
	}

	info.SendResponseCount = 1
	if canRetryGPTChannelFallback(info, 1) {
		t.Fatal("must not retry after output starts")
	}
}

func TestGPT524GetsOneForcedFallback(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
	err := types.NewErrorWithStatusCode(errors.New("proxy read timeout"), types.ErrorCodeBadResponse, statusCodeCloudflareTimeout)
	if !isGPTChannelFallbackError(info, err) || !canRetryGPTChannelFallback(info, 1) {
		t.Fatal("expected one pre-output GPT 524 fallback")
	}

	info.OriginModelName = "claude-sonnet-5"
	if isGPTChannelFallbackError(info, err) {
		t.Fatal("524 fallback must remain GPT-only")
	}
}

func TestGPTChannelFallbackCoversUpstreamFailuresButSkipsRequestErrors(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
	tests := []struct {
		name       string
		statusCode int
		options    []types.NewAPIErrorOptions
		want       bool
	}{
		{name: "upstream 401", statusCode: http.StatusUnauthorized, want: true},
		{name: "upstream 403", statusCode: http.StatusForbidden, want: true},
		{name: "upstream 404", statusCode: http.StatusNotFound, want: true},
		{name: "upstream 429", statusCode: http.StatusTooManyRequests, want: true},
		{name: "upstream 500", statusCode: http.StatusInternalServerError, want: true},
		{name: "gateway 504", statusCode: http.StatusGatewayTimeout, want: true},
		{name: "gateway 524", statusCode: statusCodeCloudflareTimeout, want: true},
		{name: "network error without status", statusCode: 0, want: true},
		{name: "invalid request", statusCode: http.StatusBadRequest, want: false},
		{
			name:       "explicit skip retry",
			statusCode: http.StatusServiceUnavailable,
			options:    []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(errors.New(tt.name), types.ErrorCodeBadResponse, tt.statusCode, tt.options...)
			if got := isGPTChannelFallbackError(info, err); got != tt.want {
				t.Fatalf("isGPTChannelFallbackError() = %v, want %v", got, tt.want)
			}
		})
	}

	info.OriginModelName = "claude-sonnet-4-6"
	err := types.NewErrorWithStatusCode(errors.New("upstream 500"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	if isGPTChannelFallbackError(info, err) {
		t.Fatal("generic upstream fallback must remain GPT-only")
	}
}

func TestExplicitSkipRetryOverridesChannelError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	err := types.NewError(
		errors.New("invalid channel parameter override"),
		types.ErrorCodeChannelParamOverrideInvalid,
		types.ErrOptionWithSkipRetry(),
	)
	if shouldRetry(c, err, 10) {
		t.Fatal("explicit skip retry must override the channel error classification")
	}
}

func TestGatewaySessionBlockedError(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		errors.New("This session has been blocked by the gateway content policy. Contact the administrator."),
		types.ErrorCodeBadResponse,
		http.StatusForbidden,
	)
	if !isGatewaySessionBlockedError(err) {
		t.Fatal("expected gateway session policy rejection")
	}

	err.StatusCode = http.StatusServiceUnavailable
	if !isGatewaySessionBlockedError(err) {
		t.Fatal("status-code mapping must not hide the exact gateway policy rejection")
	}
}

func TestProtectedChannelControlsGlobalTokenBan(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "o4-mini"}
	err := types.NewErrorWithStatusCode(
		errors.New("This session has been blocked by the gateway content policy."),
		types.ErrorCodeBadResponse,
		http.StatusForbidden,
	)
	info.ChannelMeta = &relaycommon.ChannelMeta{
		ChannelOtherSettings: dto.ChannelOtherSettings{APIKeyPolicyProtectionEnabled: true},
	}
	if !shouldBanTokenFromProtectedChannels(info, err) {
		t.Fatal("protected channel must globally ban the API key regardless of model alias")
	}

	info.ChannelOtherSettings.APIKeyPolicyProtectionEnabled = false
	if shouldBanTokenFromProtectedChannels(info, err) {
		t.Fatal("unprotected channel must not globally ban the API key")
	}
}

func TestForcedGPTFallbackExtendsZeroRetryBudgetOnce(t *testing.T) {
	retryLimit := extendRetryLimitForForcedGPTFallback(0, 0)
	if retryLimit != 1 {
		t.Fatalf("retry limit = %d, want 1", retryLimit)
	}
	if retryLimit = extendRetryLimitForForcedGPTFallback(retryLimit, 0); retryLimit != 1 {
		t.Fatalf("existing retry budget changed to %d, want 1", retryLimit)
	}
	if canContinueRelayRetry(true, false, false, true) {
		t.Fatal("must not make a third attempt after the policy fallback channel fails")
	}
}
