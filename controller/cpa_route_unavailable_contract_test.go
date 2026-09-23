package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// Exercise the actual upstream envelope decoder before the NewAPI retry gates.
// A CPA credential-pool outage must not become a customer quota error or suppress
// a distinct NewAPI entrance that can still serve the same request.
func TestCPARouteUnavailableHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name, body                         string
		status                             int
		wantAuthUnavailable, wantSameRound bool
		wantNextRound                      bool
	}{
		{
			name: "structured credential pool outage", status: http.StatusServiceUnavailable,
			body:                `{"error":{"message":"requested route is temporarily unavailable","type":"server_error","code":"auth_unavailable"}}`,
			wantAuthUnavailable: true, wantSameRound: true,
		},
		{
			name: "legacy CPA compatibility envelope", status: http.StatusServiceUnavailable,
			body:                `{"error":{"message":"auth_unavailable: requested route is temporarily unavailable","type":"server_error","code":"internal_server_error"}}`,
			wantAuthUnavailable: true, wantSameRound: true,
		},
		{
			name: "legacy prefix normalization", status: http.StatusServiceUnavailable,
			body:                `{"error":{"message":"  AUTH_UNAVAILABLE: requested route is temporarily unavailable","type":"server_error","code":"internal_server_error"}}`,
			wantAuthUnavailable: true, wantSameRound: true,
		},
		{
			name: "genuine rate limit", status: http.StatusTooManyRequests,
			body:          `{"error":{"message":"upstream rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			wantSameRound: true, wantNextRound: true,
		},
		{
			name: "temporary upstream overload", status: http.StatusServiceUnavailable,
			body:          `{"error":{"message":"upstream overloaded","type":"server_error","code":"server_error"}}`,
			wantSameRound: true, wantNextRound: true,
		},
		{
			name: "message mentions another request", status: http.StatusServiceUnavailable,
			body:          `{"error":{"message":"temporary overload; earlier request reported auth_unavailable: exhausted","type":"server_error","code":"internal_server_error"}}`,
			wantSameRound: true, wantNextRound: true,
		},
		{
			name: "legacy prefix on 429 does not suppress ordinary retry", status: http.StatusTooManyRequests,
			body:          `{"error":{"message":"auth_unavailable: upstream cooling down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			wantSameRound: true, wantNextRound: true,
		},
		{
			name: "deterministic tool history incompatibility", status: http.StatusBadRequest,
			body: `{"error":{"message":"complex tool history requires a compatible Responses route","type":"invalid_request_error","code":"request_feature_unsupported"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseCPAContractError(t, tc.status, tc.body)
			if got := isAuthUnavailableError(err); got != tc.wantAuthUnavailable {
				t.Fatalf("isAuthUnavailableError = %v, want %v", got, tc.wantAuthUnavailable)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
			info.MarkUpstreamRequestMayHaveBeenAccepted()
			retryAllowed := shouldRetry(c, err, 10, types.RelayFormatOpenAIResponses)
			if got := retryAllowed && !relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIResponses, err); got != tc.wantSameRound {
				t.Fatalf("pre-output retry to a different entrance = %v, want %v", got, tc.wantSameRound)
			}
			if got := isGPTChannelFallbackError(info, err); got != tc.wantSameRound {
				t.Fatalf("GPT fallback classification = %v, want %v", got, tc.wantSameRound)
			}
			state := newCPAContractRetryState()
			state.RecordAttempt(9)
			param := &service.RetryParam{Ctx: c}
			recordRelayChannelFailure(param, 9, err)
			if got := canStartNextRelayRetryRound(context.Background(), state, retryAllowed, false, err, param); got != tc.wantNextRound {
				t.Fatalf("cross-round replay = %v, want %v", got, tc.wantNextRound)
			}
			if tc.wantAuthUnavailable && state.StopReason != service.RetryStopReasonAuthUnavailable {
				t.Fatalf("stop reason = %q", state.StopReason)
			}
			recorder := httptest.NewRecorder()
			output, _ := gin.CreateTestContext(recorder)
			output.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			writeRelayError(output, nil, types.RelayFormatOpenAIResponses, err)
			if recorder.Code != tc.status {
				t.Fatalf("client status = %d, want %d", recorder.Code, tc.status)
			}
			if got := recorder.Header().Get("Retry-After") != ""; got != tc.wantAuthUnavailable {
				t.Fatalf("auth outage Retry-After present = %v, want %v", got, tc.wantAuthUnavailable)
			}
			var clientEnvelope struct {
				Error types.OpenAIError `json:"error"`
			}
			if decodeErr := common.Unmarshal(recorder.Body.Bytes(), &clientEnvelope); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if clientEnvelope.Error.Code != string(err.GetErrorCode()) {
				t.Fatalf("client error code lost: %v, want %q", clientEnvelope.Error.Code, err.GetErrorCode())
			}
		})
	}
}

func TestCPARouteUnavailableNeverReplaysAfterStreamProgress(t *testing.T) {
	for _, fixture := range []struct {
		name, body string
		status     int
	}{
		{"pool exhausted", `{"error":{"message":"route pool exhausted","type":"server_error","code":"auth_unavailable"}}`, http.StatusServiceUnavailable},
		{"rate limited", `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`, http.StatusTooManyRequests},
	} {
		for _, progress := range []string{"downstream bytes", "received event", "sent event"} {
			t.Run(progress+" "+fixture.name, func(t *testing.T) {
				err := parseCPAContractError(t, fixture.status, fixture.body)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
				switch progress {
				case "downstream bytes":
					if _, writeErr := c.Writer.WriteString("data: partial\n\n"); writeErr != nil {
						t.Fatal(writeErr)
					}
				case "received event":
					info.ReceivedResponseCount = 1
				case "sent event":
					info.SendResponseCount = 1
				}
				retryAllowed := shouldRetry(c, err, 16, types.RelayFormatOpenAIResponses)
				if retryAllowed && !relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIResponses, err) {
					t.Fatal("stream progress must close the replay gate even for a retryable CPA error")
				}
				if progress == "downstream bytes" {
					before := recorder.Body.String()
					writeRelayError(c, nil, types.RelayFormatOpenAIResponses, err)
					if recorder.Body.String() != before {
						t.Fatal("error reporting appended JSON after SSE output")
					}
				}
			})
		}
	}
}

func TestCPARouteUnavailableExcludesOnlyExhaustedEntranceAcrossRounds(t *testing.T) {
	for _, transientStatus := range []int{http.StatusBadGateway, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, authUnavailableLast := range []bool{false, true} {
			name := http.StatusText(transientStatus)
			if authUnavailableLast {
				name += " auth unavailable last"
			}
			t.Run(name, func(t *testing.T) {
				setupCPAContractChannels(t, true)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol"}
				state := newCPAContractRetryState()
				authErr := parseCPAContractError(t, http.StatusServiceUnavailable,
					`{"error":{"message":"no eligible credentials","code":"auth_unavailable"}}`)
				transientErr := parseCPAContractError(t, transientStatus,
					`{"error":{"message":"temporary upstream failure","code":"server_error"}}`)
				if authUnavailableLast {
					state.RecordAttempt(10)
					recordRelayChannelFailure(param, 10, transientErr)
				}
				state.RecordAttempt(9)
				recordRelayChannelFailure(param, 9, authErr)
				if !authUnavailableLast {
					channel, _, selectErr := service.CacheGetRandomSatisfiedChannel(param)
					if selectErr != nil || channel == nil || channel.Id != 10 {
						t.Fatalf("same-round alternate = %v error=%v, want channel 10", channel, selectErr)
					}
					state.RecordAttempt(10)
					recordRelayChannelFailure(param, 10, transientErr)
				}
				lastErr := transientErr
				if authUnavailableLast {
					lastErr = authErr
				}
				if !canStartNextRelayRetryRound(context.Background(), state, true, false, lastErr, param) {
					t.Fatal("a transiently failing alternate must retain bounded next-round recovery")
				}
				state.StartNextRound()
				param.ExcludedChannelIds = nil
				param.SetRetry(0)
				channel, _, selectErr := service.CacheGetRandomSatisfiedChannel(param)
				if selectErr != nil || channel == nil || channel.Id != 10 {
					t.Fatalf("next-round channel = %v error=%v, must not replay exhausted CPA 9", channel, selectErr)
				}
				if !slices.Equal(param.PersistentExcludedIds, []int{9}) {
					t.Fatalf("request-lifetime exclusions = %v, want only 9", param.PersistentExcludedIds)
				}
				var stored model.Channel
				if err := model.DB.First(&stored, "id = ?", 9).Error; err != nil || stored.Status != common.ChannelStatusEnabled {
					t.Fatal("request exclusion must not disable the channel globally")
				}
			})
		}
	}
}

func TestCPARouteUnavailableOnlyEntranceStopsWithoutReplay(t *testing.T) {
	setupCPAContractChannels(t, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol"}
	state := newCPAContractRetryState()
	err := parseCPAContractError(t, http.StatusServiceUnavailable,
		`{"error":{"message":"auth_unavailable: exhausted","code":"internal_server_error"}}`)
	state.RecordAttempt(9)
	recordRelayChannelFailure(param, 9, err)
	if canStartNextRelayRetryRound(context.Background(), state, true, false, err, param) {
		t.Fatal("a request with only an exhausted CPA entrance must stop")
	}
	param.ExcludedChannelIds = nil
	channel, _, selectErr := service.CacheGetRandomSatisfiedChannel(param)
	if selectErr != nil || channel != nil {
		t.Fatalf("exhausted entrance reselected after round reset: %v error=%v", channel, selectErr)
	}
}

func TestCPARequestFeatureUnsupportedOverridesConfiguredRetryStatuses(t *testing.T) {
	oldRanges := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := parseCPAContractError(t, status,
			`{"error":{"message":"unsupported request feature","type":"invalid_request_error","code":"request_feature_unsupported"}}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol"}
		if shouldRetry(c, err, 16, types.RelayFormatOpenAIResponses) || isGPTChannelFallbackError(info, err) {
			t.Fatalf("deterministic feature error must not be replayed even with retryable/mapped status %d", status)
		}
		gptFallback := isGPTChannelFallbackError(info, err)
		canGPTFallback := gptFallback && canRetryGPTChannelFallback(info, types.RelayFormatOpenAIResponses, err)
		retryAllowed := shouldRetry(c, err, 16, types.RelayFormatOpenAIResponses)
		if gptFallback {
			retryAllowed = canGPTFallback
		}
		if relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIResponses, err) {
			retryAllowed = false
		}
		if canContinueRelayRetry(retryAllowed, gptFallback, canGPTFallback) {
			t.Fatalf("GPT forced-fallback branch reopened replay for deterministic feature error with status %d", status)
		}
	}
}

func TestCPARouteUnavailableDoesNotSuppressAlternateCircuitRecovery(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	param := &service.RetryParam{Ctx: c, CircuitSkippedIds: []int{10}}
	state := newCPAContractRetryState()
	state.RecordAttempt(9)
	err := parseCPAContractError(t, http.StatusServiceUnavailable,
		`{"error":{"message":"pool exhausted","code":"auth_unavailable"}}`)
	recordRelayChannelFailure(param, 9, err)
	if !canStartNextRelayRetryRound(context.Background(), state, true, true, err, param) {
		t.Fatal("a distinct circuit-skipped entrance may recover without replaying exhausted CPA")
	}
	param.AddPersistentExcludedChannel(10)
	if canStartNextRelayRetryRound(context.Background(), state, true, true, err, param) {
		t.Fatal("circuit-skipped entrances that also exhausted their pool must not reopen retry rounds")
	}
}

func setupCPAContractChannels(t *testing.T, includeAlternate bool) {
	t.Helper()
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldCache, oldRedis := common.MemoryCacheEnabled, common.RedisEnabled
	oldSQLite, oldMySQL, oldPostgres := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.MemoryCacheEnabled, common.RedisEnabled = oldCache, oldRedis
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgres
	})
	db := setupModelListControllerTestDB(t)
	common.MemoryCacheEnabled = false
	channelIDs := []int{9}
	if includeAlternate {
		channelIDs = append(channelIDs, 10)
	}
	for _, id := range channelIDs {
		channel := model.Channel{Id: id, Type: 1, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-5.6-sol"}
		if err := db.Create(&channel).Error; err != nil {
			t.Fatal(err)
		}
		ability := model.Ability{Group: "default", Model: "gpt-5.6-sol", ChannelId: id, Enabled: true}
		if err := db.Create(&ability).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func parseCPAContractError(t *testing.T, status int, body string) *types.NewAPIError {
	t.Helper()
	err := service.RelayErrorHandler(context.Background(), &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, false)
	if err == nil || !err.HasUpstreamResponse() {
		t.Fatal("fixture must retain explicit upstream HTTP response provenance")
	}
	return err
}

func newCPAContractRetryState() *service.RelayRetryState {
	return &service.RelayRetryState{
		Policy:    service.RelayRetryPolicy{MaxRounds: 3, MaxAttempts: 30, MaxElapsed: time.Minute},
		StartedAt: time.Now(), Round: 1, DistinctChannel: make(map[int]struct{}),
	}
}
