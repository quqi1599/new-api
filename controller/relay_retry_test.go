package controller

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
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
