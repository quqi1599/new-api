package controller

import (
	"errors"
	"net/http"
	"testing"

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

func TestGPTSessionBlockedError(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-luna"}
	err := types.NewErrorWithStatusCode(
		errors.New("This session has been blocked by the gateway content policy. Contact the administrator."),
		types.ErrorCodeBadResponse,
		http.StatusForbidden,
	)
	if !isGPTSessionBlockedError(info, err) {
		t.Fatal("expected GPT gateway session policy rejection")
	}

	info.OriginModelName = "claude-sonnet-4"
	if isGPTSessionBlockedError(info, err) {
		t.Fatal("must not block channels for non-GPT models")
	}
}
