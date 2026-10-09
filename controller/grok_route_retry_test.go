package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestGrokUnavailableRoutesAreNotRevivedAcrossRounds(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(map[bool]string{false: "database", true: "cache"}[cache], func(t *testing.T) {
			c := setupPrioritySequenceChannels(t, cache, []int64{100, 90, 80})
			param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true}
			state := newCPAContractRetryState()
			failures := []*types.NewAPIError{
				parseCPAContractError(t, 502, `{"error":{"code":"upstream_connection_refused","message":"upstream connection was refused"}}`),
				parseCPAContractError(t, 404, `{"error":{"code":"model_not_supported","message":"model unavailable on this channel"}}`),
				parseCPAContractError(t, 404, `{"error":{"code":"model_not_found","message":"model unavailable on this channel"}}`),
			}
			var selected []int
			for i, relayErr := range failures {
				channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel == nil || channel.Id != 901+i {
					t.Fatalf("distinct fallback disappeared: channel=%v err=%v", channel, err)
				}
				if !shouldRetry(c, relayErr, 10, types.RelayFormatOpenAI) {
					t.Fatal("explicit pre-output failure must allow another channel")
				}
				state.RecordAttempt(channel.Id)
				selected = append(selected, channel.Id)
				excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{OriginModelName: "grok-4.7", LastError: relayErr}, channel.Id)
				param.IncreaseRetry()
			}
			if !slices.Equal(param.PersistentExcludedIds, selected) {
				t.Fatalf("known unavailable routes can be retried: %v", param.PersistentExcludedIds)
			}
			if canStartNextRelayRetryRound(context.Background(), state, true, false, failures[2], param) {
				t.Fatal("all known-unavailable routes were revived for another round")
			}
			if state.Attempts != 3 || state.StopReason != service.RetryStopReasonNoChannel {
				t.Fatalf("wrong retry outcome: %+v", state)
			}
			next := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true}
			channel, _, err := service.CacheGetRandomSatisfiedChannel(next)
			if err != nil || channel == nil || channel.Id != 901 {
				t.Fatal("request-local exclusion disabled a channel for later requests", channel, err)
			}
		})
	}
}

func TestLegacyGrokAuthNotFoundCannotReplaySameEntrance(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		want   bool
	}{
		{`{"error":{"code":"internal_server_error","message":"auth_not_found: no available authentication"}}`, 503, true},
		{`{"error":{"code":"auth_not_found","message":"no available authentication"}}`, 503, true},
		{`{"error":{"code":"internal_server_error","message":"  AUTH_NOT_FOUND: no available authentication"}}`, 503, true},
		{`{"error":{"code":"internal_server_error","message":"earlier auth_not_found: request unavailable"}}`, 503, false},
		{`{"error":{"code":"internal_server_error","message":"auth_not_found: no available authentication"}}`, 429, false},
	} {
		err := parseCPAContractError(t, tc.status, tc.body)
		if isAuthUnavailableError(err) != tc.want {
			t.Fatalf("legacy auth contract misclassified: status=%d error=%v", tc.status, err)
		}
		param := &service.RetryParam{}
		state := newCPAContractRetryState()
		state.RecordAttempt(3)
		recordRelayChannelFailure(param, 3, err)
		if tc.want && (canStartNextRelayRetryRound(context.Background(), state, true, false, err, param) || state.StopReason != service.RetryStopReasonAuthUnavailable) {
			t.Fatal("legacy auth failure revived the same aggregate entrance")
		}
	}
	local := types.NewErrorWithStatusCode(errors.New("auth_not_found: local failure"), types.ErrorCode("auth_not_found"), 503)
	if isAuthUnavailableError(local) {
		t.Fatal("local text cannot prove upstream pool exhaustion")
	}
}

func TestKnownRouteRejectionRequiresStructuredUpstreamEvidence(t *testing.T) {
	for _, tc := range []struct {
		code       string
		status     int
		upstream   bool
		persistent bool
	}{
		{"model_not_supported", 404, true, true}, {"model_not_found", 404, true, true},
		{"model_cooldown", 503, true, true}, {"model_cooldown", 429, true, true},
		{"upstream_connection_refused", 502, true, true},
		{"model_not_supported", 500, true, false}, {"model_cooldown", 500, true, false},
		{"model_not_found", 404, false, false}, {"upstream_connection_refused", 502, false, false},
		{"internal_server_error", 404, true, false}, {"server_error", 503, true, false},
		{"rate_limit_exceeded", 429, true, false}, {"private_code", 503, true, false},
	} {
		var options []types.NewAPIErrorOptions
		if tc.upstream {
			options = append(options, types.ErrOptionWithUpstreamResponse())
		}
		err := types.NewErrorWithStatusCode(errors.New("model_not_supported: upstream_connection_refused model_cooldown"), types.ErrorCode(tc.code), tc.status, options...)
		param := &service.RetryParam{}
		recordRelayChannelFailure(param, 143, err)
		if (len(param.PersistentExcludedIds) == 1) != tc.persistent {
			t.Fatalf("code=%s status=%d upstream=%v exclusions=%v", tc.code, tc.status, tc.upstream, param.PersistentExcludedIds)
		}
	}
}

func TestKnownRouteRejectionKeepsTransientRecoveryAndOutputBarrier(t *testing.T) {
	state := newCPAContractRetryState()
	param := &service.RetryParam{}
	missing := parseCPAContractError(t, 404, `{"error":{"code":"model_not_supported","message":"model unavailable on this channel"}}`)
	transient := parseCPAContractError(t, 503, `{"error":{"code":"server_error","message":"temporary upstream failure"}}`)
	state.RecordAttempt(1)
	state.RecordAttempt(143)
	recordRelayChannelFailure(param, 1, missing)
	recordRelayChannelFailure(param, 143, transient)
	if !canStartNextRelayRetryRound(context.Background(), state, true, false, missing, param) {
		t.Fatal("unrelated transient route lost bounded recovery")
	}
	param.ExcludedChannelIds = nil
	if !slices.Equal(param.PersistentExcludedIds, []int{1}) {
		t.Fatal("request lifetime exclusion was lost")
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer.WriteHeaderNow()
	if shouldRetry(c, missing, 10, types.RelayFormatOpenAIResponses) {
		t.Fatal("output was replayed")
	}
}
