package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestAuthUnavailableDoesNotReplayAggregateChannelAcrossRounds(t *testing.T) {
	state := service.NewRelayRetryState(types.RelayFormatOpenAI, 16)
	state.RecordAttempt(9)
	authErr := types.WithOpenAIError(
		types.OpenAIError{
			Message: "requested route is temporarily unavailable",
			Type:    "upstream_error",
			Code:    string(types.ErrorCodeAuthUnavailable),
		},
		http.StatusServiceUnavailable,
		types.ErrOptionWithUpstreamResponse(),
	)

	if canStartNextRelayRetryRound(context.Background(), state, true, false, authErr) {
		t.Fatal("auth_unavailable must not reset exclusions and replay the same aggregate channel")
	}
	if state.StopReason != service.RetryStopReasonAuthUnavailable {
		t.Fatalf("stop reason = %q", state.StopReason)
	}
}

func TestCPAAuthUnavailableCompatibilityEnvelopeDoesNotReplayAcrossRounds(t *testing.T) {
	body := `{"error":{"message":"auth_unavailable: requested route is temporarily unavailable","type":"server_error","code":"internal_server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	authErr := service.RelayErrorHandler(context.Background(), resp, false)
	if authErr == nil {
		t.Fatal("expected CPA upstream error")
	}
	if authErr.GetErrorCode() == types.ErrorCodeAuthUnavailable {
		t.Fatal("fixture must cover the legacy CPA envelope that loses the structured code")
	}
	if !isAuthUnavailableError(authErr) {
		t.Fatal("legacy CPA auth_unavailable envelope must be classified")
	}

	state := service.NewRelayRetryState(types.RelayFormatOpenAI, 16)
	state.RecordAttempt(9)
	if canStartNextRelayRetryRound(context.Background(), state, true, false, authErr) {
		t.Fatal("legacy CPA auth_unavailable must not replay the aggregate channel")
	}
	if state.StopReason != service.RetryStopReasonAuthUnavailable {
		t.Fatalf("stop reason = %q", state.StopReason)
	}
}

func TestAuthUnavailableMessageFallbackRequiresExplicitUpstream503(t *testing.T) {
	localErr := types.NewErrorWithStatusCode(
		errors.New("auth_unavailable: local diagnostic"),
		types.ErrorCodeBadResponse,
		http.StatusServiceUnavailable,
	)
	if isAuthUnavailableError(localErr) {
		t.Fatal("local errors must not be classified by message text alone")
	}
}

func TestOtherRetryableFailureMayStartAnotherRound(t *testing.T) {
	state := service.NewRelayRetryState(types.RelayFormatOpenAI, 1)
	upstreamErr := types.NewErrorWithStatusCode(
		errors.New("upstream 502"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
		types.ErrOptionWithUpstreamResponse(),
	)

	if !canStartNextRelayRetryRound(context.Background(), state, true, false, upstreamErr) {
		t.Fatal("ordinary retryable upstream failures must preserve bounded cross-round retry")
	}
}

func TestGPTChannelFallbackStopsAfterOutputButAllowsDistinctChannels(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.5"}
	err := types.NewErrorWithStatusCode(errors.New("rate limited"), types.ErrorCodeChannelNoAvailableKey, http.StatusTooManyRequests)
	if !isGPTChannelFallbackError(info, err) || !canRetryGPTChannelFallback(info, types.RelayFormatOpenAI, err) {
		t.Fatal("expected one pre-output GPT 429 fallback")
	}

	info.SendResponseCount = 1
	if canRetryGPTChannelFallback(info, types.RelayFormatOpenAI, err) {
		t.Fatal("must not retry after output starts")
	}

	info.SendResponseCount = 0
	info.MarkUpstreamRequestMayHaveBeenAccepted()
	if canRetryGPTChannelFallback(info, types.RelayFormatOpenAI, err) {
		t.Fatal("must not retry after a non-idempotent upstream request may have been accepted")
	}

	err = types.NewErrorWithStatusCode(
		errors.New("explicit upstream failure"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
		types.ErrOptionWithUpstreamResponse(),
	)
	if !canRetryGPTChannelFallback(info, types.RelayFormatOpenAI, err) {
		t.Fatal("an explicit upstream failure before output must allow a different GPT channel")
	}
}

func TestModelRoutingFirstFailureRemainsExcludedAcrossRetryRounds(t *testing.T) {
	retryParam := &service.RetryParam{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelOtherSettings: dto.ChannelOtherSettings{ModelRoutingFirstEnabled: true},
		},
	}

	excludeFailedChannelForRetry(retryParam, info, 131)
	if len(retryParam.ExcludedChannelIds) != 1 || retryParam.ExcludedChannelIds[0] != 131 {
		t.Fatalf("round exclusions = %#v", retryParam.ExcludedChannelIds)
	}
	if len(retryParam.PersistentExcludedIds) != 1 || retryParam.PersistentExcludedIds[0] != 131 {
		t.Fatalf("persistent exclusions = %#v", retryParam.PersistentExcludedIds)
	}

	retryParam.ExcludedChannelIds = nil
	if len(retryParam.PersistentExcludedIds) != 1 || retryParam.PersistentExcludedIds[0] != 131 {
		t.Fatalf("persistent exclusions after round reset = %#v", retryParam.PersistentExcludedIds)
	}
}

func TestGPT524AllowsFallback(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
	err := types.NewErrorWithStatusCode(errors.New("proxy read timeout"), types.ErrorCodeBadResponse, statusCodeCloudflareTimeout)
	if !isGPTChannelFallbackError(info, err) || !canRetryGPTChannelFallback(info, types.RelayFormatOpenAI, err) {
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

func TestRelayRetryStopsAfterAnyStreamProgress(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		errors.New("upstream stream failed"),
		types.ErrorCodeBadResponse,
		http.StatusInternalServerError,
	)

	t.Run("downstream already written", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		_, writeErr := c.Writer.Write([]byte("data: partial\n\n"))
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if shouldRetry(c, err, 1) {
			t.Fatal("must not retry after downstream bytes were committed")
		}
	})

	t.Run("upstream event already received", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{ReceivedResponseCount: 1}
		if !relayProgressStarted(c, info) {
			t.Fatal("a valid upstream event must close the retry gate")
		}
	})

	t.Run("ambiguous upstream request already sent", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{}
		info.MarkUpstreamRequestMayHaveBeenAccepted()
		if !relayRetryIsUnsafe(c, info, types.RelayFormatClaude, err) {
			t.Fatal("a possibly accepted POST must close the retry gate")
		}
	})

	t.Run("explicit upstream failure permits cross-channel retry", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{}
		info.MarkUpstreamRequestMayHaveBeenAccepted()
		upstreamErr := types.NewErrorWithStatusCode(
			errors.New("upstream 502"),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusBadGateway,
			types.ErrOptionWithUpstreamResponse(),
		)
		if !shouldRetry(c, upstreamErr, 1) {
			t.Fatal("an explicit upstream 502 must be retryable")
		}
		for _, relayFormat := range []types.RelayFormat{
			types.RelayFormatClaude,
			types.RelayFormatOpenAI,
			types.RelayFormatOpenAIImage,
		} {
			if relayRetryIsUnsafe(c, info, relayFormat, upstreamErr) {
				t.Fatalf("explicit upstream failure must allow %s to try another channel", relayFormat)
			}
		}
	})

	t.Run("explicit upstream failure does not replay asynchronous task", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{}
		info.MarkUpstreamRequestMayHaveBeenAccepted()
		upstreamErr := types.NewErrorWithStatusCode(
			errors.New("upstream 500"),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusInternalServerError,
			types.ErrOptionWithUpstreamResponse(),
		)
		if !relayRetryIsUnsafe(c, info, types.RelayFormatTask, upstreamErr) {
			t.Fatal("a possibly accepted asynchronous task must not be replayed")
		}
	})

	t.Run("explicit upstream failure does not replay alpha search", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{}
		info.MarkUpstreamRequestMayHaveBeenAccepted()
		upstreamErr := types.NewErrorWithStatusCode(
			errors.New("upstream 500"),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusInternalServerError,
			types.ErrOptionWithUpstreamResponse(),
		)
		if !relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIAlphaSearch, upstreamErr) {
			t.Fatal("a possibly accepted standalone search must not be replayed")
		}
	})
}

func TestExplicitUpstreamStatusesRetryAcrossSynchronousFormats(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	for _, statusCode := range []int{
		http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusGatewayTimeout,
		statusCodeCloudflareTimeout,
	} {
		relayErr := types.NewErrorWithStatusCode(
			errors.New("explicit upstream gateway timeout"),
			types.ErrorCodeBadResponseStatusCode,
			statusCode,
			types.ErrOptionWithUpstreamResponse(),
		)
		for _, relayFormat := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIImage} {
			if !shouldRetry(c, relayErr, 1, relayFormat) {
				t.Fatalf("status %d must retry for %s after an explicit upstream response", statusCode, relayFormat)
			}
		}
		if shouldRetry(c, relayErr, 1, types.RelayFormatTask) {
			t.Fatalf("status %d must not replay an asynchronous task", statusCode)
		}
	}
}

func TestTaskRelayDoesNotRetryLocalCancellationOrDeadline(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	for _, statusCode := range []int{499, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		taskErr := &dto.TaskError{
			StatusCode: statusCode,
			LocalError: true,
		}
		if shouldRetryTaskRelay(c, nil, 1, taskErr, 1) {
			t.Fatalf("local task error with status %d must not retry", statusCode)
		}
	}

	upstreamErr := &dto.TaskError{StatusCode: http.StatusInternalServerError}
	if !shouldRetryTaskRelay(c, nil, 1, upstreamErr, 1) {
		t.Fatal("ordinary upstream 500 should preserve existing retry behavior")
	}

	for _, statusCode := range []int{http.StatusTemporaryRedirect, http.StatusTooManyRequests, http.StatusInternalServerError} {
		info := &relaycommon.RelayInfo{}
		info.MarkUpstreamRequestMayHaveBeenAccepted()
		if shouldRetryTaskRelay(c, info, 1, &dto.TaskError{StatusCode: statusCode}, 1) {
			t.Fatalf("possibly accepted task submission with status %d must not retry", statusCode)
		}
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

func TestExtractModerationReviewId(t *testing.T) {
	const moderationId = "e2bb21e3d0a60657d6895957df3585300633fc1b1390cf727a2e37892f5bcbf8"
	err := types.NewErrorWithStatusCode(
		errors.New("This session has been blocked by the gateway content policy. Contact the administrator if you believe this is a mistake. Moderation review id: "+moderationId+"."),
		types.ErrorCodeBadResponse,
		http.StatusForbidden,
	)
	if got := extractModerationReviewId(err); got != moderationId {
		t.Fatalf("moderation review id = %q, want %q", got, moderationId)
	}

	err.SetMessage("Moderation_id=" + moderationId)
	if got := extractModerationReviewId(err); got != moderationId {
		t.Fatalf("moderation id alternate format = %q, want %q", got, moderationId)
	}

	err.SetMessage("This session has been blocked by the gateway content policy.")
	if got := extractModerationReviewId(err); got != "" {
		t.Fatalf("unexpected moderation review id %q", got)
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

func TestGPTFallbackContinuesWithinUnifiedRetryBudget(t *testing.T) {
	if !canContinueRelayRetry(true, false, false) {
		t.Fatal("ordinary retryable failures must continue")
	}
	if !canContinueRelayRetry(true, true, true) {
		t.Fatal("safe GPT fallback must continue across eligible channels")
	}
	if canContinueRelayRetry(true, true, false) {
		t.Fatal("unsafe GPT fallback must stop")
	}
}
