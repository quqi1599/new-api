package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

type capabilityRetryFixture struct {
	codes           []string
	status          int
	cached, healthy bool
	barrier         string
	protocol        string
	nonStream       bool
	message         string
}

func runCapabilityRetryRelay(t *testing.T, fixture capabilityRetryFixture) streamResilienceResult {
	t.Helper()
	setupCPAContractChannels(t, false)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree, oldRatios := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume, ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatios))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "3")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "6")
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
	t.Setenv("RELAY_RETRY_MAX_ROUND_BACKOFF_MS", "0")
	if fixture.barrier == "budget" {
		t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "1")
	}
	if fixture.status == 0 {
		fixture.status = http.StatusUnprocessableEntity
	}
	if fixture.message == "" {
		fixture.message = "synthetic reasoning_budget_unsupported mention"
	}
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	var calls []int
	var first model.Channel
	count := len(fixture.codes)
	if fixture.healthy {
		count++
	}
	for index := 0; index < count; index++ {
		id := 9 + index
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Model           string         `json:"model"`
				Reasoning       *dto.Reasoning `json:"reasoning"`
				ReasoningEffort string         `json:"reasoning_effort"`
			}
			require.NoError(t, common.DecodeJson(r.Body, &request))
			require.Equal(t, "gpt-5.6-sol", request.Model)
			if fixture.protocol == "chat" {
				require.Equal(t, "high", request.ReasoningEffort)
			} else {
				require.NotNil(t, request.Reasoning)
				require.Equal(t, "high", request.Reasoning.Effort, "fallback must preserve the requested control")
			}
			mu.Lock()
			calls = append(calls, id)
			mu.Unlock()
			if index < len(fixture.codes) {
				if fixture.barrier == "unknown" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					return
				}
				if fixture.barrier == "output" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_started\",\"status\":\"in_progress\"}}\n\n")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_started\",\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":\"synthetic capability failure\"}}}\n\n", fixture.codes[index])
					return
				}
				if fixture.barrier == "cancel" {
					cancel()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fixture.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"type":"invalid_request_error","message":%q}}`, fixture.codes[index], fixture.message)
				return
			}
			if fixture.nonStream {
				w.Header().Set("Content-Type", "application/json")
				if fixture.protocol == "chat" {
					_, _ = fmt.Fprint(w, `{"id":"chatcmpl_healthy","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				} else {
					_, _ = fmt.Fprint(w, `{"id":"resp_capability_healthy","object":"response","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_capability_healthy\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}))
		t.Cleanup(server.Close)
		priority := int64(100 - index)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-5.6-sol", Priority: &priority, BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: "gpt-5.6-sol", ChannelId: id, Enabled: true, Priority: &priority}).Error)
		if index == 0 {
			first = channel
		}
	}
	common.MemoryCacheEnabled = fixture.cached
	if fixture.cached {
		model.InitChannelCache()
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	path, format := "/v1/responses", types.RelayFormat(types.RelayFormatOpenAIResponses)
	body := fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"synthetic","reasoning":{"effort":"high"},"text":{"format":{"type":"json_object"}},"stream":%t}`, !fixture.nonStream)
	if fixture.protocol == "chat" {
		path, format = "/v1/chat/completions", types.RelayFormatOpenAI
		body = fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"high","response_format":{"type":"json_object"},"stream":%t}`, !fixture.nonStream)
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	if fixture.barrier == "specific" {
		c.Set("specific_channel_id", "9")
	}
	if fixture.barrier == "affinity" {
		c.Set("channel_affinity_skip_retry_on_failure", true)
	}
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-5.6-sol"))
	Relay(c, format)
	mu.Lock()
	defer mu.Unlock()
	return streamResilienceResult{c, recorder, append([]int(nil), calls...)}
}

func TestCapabilityRouteRetryDoesNotReviveRejectedEntrances(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, codes := range [][]string{{"reasoning_budget_unsupported", "reasoning_effort_unsupported"}, {"reasoning_extension_unsupported"}} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, codes[0]), func(t *testing.T) {
				got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: codes, cached: cached})
				want := []int{9}
				if len(codes) > 1 {
					want = append(want, 10)
				}
				t.Logf("calls=%v attempts=%d stop=%s", got.calls, got.context.GetInt("retry_attempt_no"), got.context.GetString("retry_stop_reason"))
				require.Equal(t, want, got.calls)
				require.Equal(t, http.StatusUnprocessableEntity, got.response.Code)
				require.Equal(t, service.RetryStopReasonNoChannel, got.context.GetString("retry_stop_reason"))
			})
		}
		t.Run(fmt.Sprintf("cache=%t/healthy alternate", cached), func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{"reasoning_budget_unsupported", "reasoning_effort_unsupported"}, cached: cached, healthy: true})
			require.Equal(t, []int{9, 10, 11}, got.calls)
			require.Equal(t, http.StatusOK, got.response.Code)
			require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
		})
		t.Run(fmt.Sprintf("cache=%t/extension healthy alternate", cached), func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{"reasoning_extension_unsupported"}, cached: cached, healthy: true})
			require.Equal(t, []int{9, 10}, got.calls)
			require.Equal(t, http.StatusOK, got.response.Code)
			require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
		})
	}
}

func TestCapabilityRouteExclusionMatchesOnlyVerified422UpstreamCodes(t *testing.T) {
	for _, code := range []string{"reasoning_budget_unsupported", "reasoning_effort_unsupported", "reasoning_extension_unsupported"} {
		for _, status := range []int{400, 422, 502, 503} {
			for _, upstream := range []bool{false, true} {
				var options []types.NewAPIErrorOptions
				if upstream {
					options = append(options, types.ErrOptionWithUpstreamResponse())
				}
				err := types.NewErrorWithStatusCode(errors.New("synthetic"), types.ErrorCode(code), status, options...)
				require.Equal(t, upstream && status == 422, isKnownUnavailableRouteError(err))
			}
		}
	}
	for _, code := range []string{"other_unsupported", "reasoning_off_unsupported", "server_error"} {
		err := types.WithOpenAIError(types.OpenAIError{Code: code, Message: "reasoning_budget_unsupported reasoning_effort_unsupported reasoning_extension_unsupported"}, 422, types.ErrOptionWithUpstreamResponse())
		require.False(t, isKnownUnavailableRouteError(err))
	}
}

func TestCapabilityRouteRetryKeepsOtherRetryAndSafetyContracts(t *testing.T) {
	for _, tc := range []struct {
		code              string
		status, wantCalls int
	}{{"server_error", 422, 3}, {"reasoning_budget_unsupported", 503, 3}} {
		t.Run(fmt.Sprintf("ordinary retry/%s/%d", tc.code, tc.status), func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{tc.code}, status: tc.status})
			require.Len(t, got.calls, tc.wantCalls)
		})
	}
	for _, barrier := range []string{"output", "unknown", "cancel", "specific", "affinity", "budget"} {
		t.Run(barrier, func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{"reasoning_budget_unsupported"}, healthy: true, barrier: barrier})
			require.Equal(t, []int{9}, got.calls)
			require.NotContains(t, got.response.Body.String(), "resp_capability_healthy")
		})
	}
	for _, tc := range []struct {
		code   string
		status int
	}{{"state_owner_unavailable", 409}, {"request_feature_unsupported", 400}} {
		t.Run(tc.code, func(t *testing.T) {
			got := runCapabilityRetryRelay(t, capabilityRetryFixture{codes: []string{tc.code}, status: tc.status, healthy: true})
			require.Equal(t, []int{9}, got.calls)
		})
	}
}
