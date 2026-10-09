package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

type cpaStateRelayResult struct {
	context        *gin.Context
	response       *httptest.ResponseRecorder
	ownerCalls     int32
	alternateCalls int32
}

// Exercise Relay, real HTTP decoding and the selector across three rounds.
// Both endpoints are local synthetic servers; the alternate represents an
// unrelated state owner that must never receive rejected opaque history.
func runCPAStateRelay(t *testing.T, code string, status int, alternate, stream bool) cpaStateRelayResult {
	t.Helper()
	setupCPAContractChannels(t, alternate)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	oldGroupRatio := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroupRatio))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 0, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "3")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "3")
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
	t.Setenv("RELAY_RETRY_MAX_ROUND_BACKOFF_MS", "0")
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })

	var ownerCalls, alternateCalls atomic.Int32
	var first model.Channel
	previousID := ""
	if strings.HasPrefix(code, "state_owner_") || code == "local_state_unavailable" {
		previousID = "resp_synthetic_owner_a"
	}
	count := 1
	if alternate {
		count = 2
	}
	for i := 0; i < count; i++ {
		isOwner := i == 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request dto.OpenAIResponsesRequest
			if err := common.DecodeJson(r.Body, &request); err != nil {
				t.Errorf("decode synthetic request: %v", err)
			}
			if request.Model != "gpt-6.1-sol" || request.PreviousResponseID != previousID {
				t.Errorf("mapping or opaque history changed: model=%q previous_response_id=%q", request.Model, request.PreviousResponseID)
			}
			w.Header().Set("Content-Type", "application/json")
			if isOwner {
				ownerCalls.Add(1)
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":%q,"message":"synthetic state_owner_unavailable mention"}}`, code)
				return
			}
			alternateCalls.Add(1)
			response := `{"id":"resp_synthetic","object":"response","status":"completed","model":"gpt-6.1-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
			} else {
				_, _ = w.Write([]byte(response))
			}
		}))
		t.Cleanup(server.Close)
		id := 9 + i
		priority := int64(100 - i*10)
		mapping := `{"gpt-5.5":"gpt-6.1-sol"}`
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Group: "default", Models: "gpt-5.5", ModelMapping: &mapping, Priority: &priority,
			BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: "gpt-5.5", ChannelId: id, Enabled: true, Priority: &priority}).Error)
		if isOwner {
			first = channel
		}
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.5","previous_response_id":%q,"input":"synthetic","stream":%t}`, previousID, stream)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-5.5"))
	Relay(c, types.RelayFormatOpenAIResponses)
	return cpaStateRelayResult{c, recorder, ownerCalls.Load(), alternateCalls.Load()}
}

func TestCPAStateOwnershipStopsSameChannelReplay(t *testing.T) {
	got := runCPAStateRelay(t, "state_owner_unavailable", http.StatusConflict, false, false)
	t.Logf("owner_calls=%d alternate_calls=%d retry_attempts=%d", got.ownerCalls, got.alternateCalls, got.context.GetInt("retry_attempt_no"))
	require.EqualValues(t, 1, got.ownerCalls, "the production baseline repeats this deterministic 409 three times")
	require.Zero(t, got.alternateCalls)
	require.Equal(t, http.StatusConflict, got.response.Code)
	require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
	require.Equal(t, service.RetryStopReasonNotRetryable, got.context.GetString("retry_stop_reason"))
}

func TestCPAStateOwnershipNeverFallsBackToAnotherOwner(t *testing.T) {
	for _, code := range []string{"state_owner_unavailable", "state_owner_conflict", "state_owner_expired", "local_state_unavailable"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", code, stream), func(t *testing.T) {
				got := runCPAStateRelay(t, code, http.StatusConflict, true, stream)
				require.EqualValues(t, 1, got.ownerCalls)
				require.Zero(t, got.alternateCalls, "do not leak opaque history to another state owner")
				require.Equal(t, http.StatusConflict, got.response.Code)
				require.Contains(t, got.response.Body.String(), code)
				require.NotContains(t, got.response.Body.String(), "data:")
				require.NotContains(t, got.response.Body.String(), "[DONE]")
				require.False(t, common.GetContextKeyBool(got.context, constant.ContextKeyRelayFirstValidEvent))
				var stored model.Channel
				require.NoError(t, model.DB.First(&stored, 9).Error)
				require.Equal(t, common.ChannelStatusEnabled, stored.Status)
				require.JSONEq(t, `{"gpt-5.5":"gpt-6.1-sol"}`, stored.GetModelMapping())
			})
		}
	}
}

func TestCPAStateOwnershipDoesNotChangeOtherRetryContracts(t *testing.T) {
	for _, tc := range []struct {
		code      string
		status    int
		wantCalls int32
	}{
		{"resource_conflict", http.StatusConflict, 2},
		{"temporary_overload", http.StatusServiceUnavailable, 2},
		{"auth_unavailable", http.StatusServiceUnavailable, 2},
		{"auth_not_found", http.StatusServiceUnavailable, 2},
		{"model_cooldown", http.StatusServiceUnavailable, 2},
		{"upstream_connection_refused", http.StatusBadGateway, 2},
		{"gpt_sampling_unsupported", http.StatusUnprocessableEntity, 1},
		{"request_feature_unsupported", http.StatusBadRequest, 1},
		{"compaction_route_unavailable", http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.code, func(t *testing.T) {
			got := runCPAStateRelay(t, tc.code, tc.status, true, true)
			require.EqualValues(t, 1, got.ownerCalls)
			require.Equal(t, tc.wantCalls-1, got.alternateCalls)
			wantStatus := tc.status
			if tc.wantCalls > 1 {
				wantStatus = http.StatusOK
			}
			require.Equal(t, wantStatus, got.response.Code, got.response.Body.String())
		})
	}
}
