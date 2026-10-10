package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

type gptRetryBudgetFixture struct {
	protocol, model, first, explicit string
	stream, cached                   bool
	maxAttempts                      int
}

// Backdating both request clocks by 179s avoids a three-minute test. The first
// real local HTTP call crosses the original 180s retry deadline before failing.
// Actual Relay selection, transport and protocol handlers then decide recovery.
func runGPTRetryBudgetRelay(t *testing.T, f gptRetryBudgetFixture) streamResilienceResult {
	t.Helper()
	setupCPAContractChannels(t, true)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	oldGroup := ratio_setting.GroupRatio2JSONString()
	oldFirst, oldNonStream := common.RelayFirstEventTotalTimeout, common.RelayNonStreamTimeout
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		common.RelayFirstEventTotalTimeout, common.RelayNonStreamTimeout = oldFirst, oldNonStream
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroup))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
	common.RelayFirstEventTotalTimeout, common.RelayNonStreamTimeout = 1200, 1200
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	if f.model == "" {
		f.model = "gpt-6.1-sol"
	}
	if f.maxAttempts == 0 {
		f.maxAttempts = 3
	}
	t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", f.explicit)
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "1")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", fmt.Sprint(f.maxAttempts))
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
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
	for id := 9; id <= 10; id++ {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, id)
			mu.Unlock()
			if id == 9 {
				time.Sleep(1100 * time.Millisecond)
				switch f.first {
				case "cancel":
					cancel()
				case "unknown":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("synthetic hijack: %v", err)
						return
					}
					_ = conn.Close()
					return
				case "output":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_synthetic\",\"call_id\":\"call_synthetic\",\"name\":\"synthetic_tool\",\"arguments\":\"{}\"}}\n\n")
					return
				}
				status, code := http.StatusServiceUnavailable, "server_error"
				if f.first == "state" {
					status, code = http.StatusConflict, "state_owner_unavailable"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"server_error","code":%q,"message":"synthetic slow first attempt"}}`, code)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if f.protocol == "chat" {
				if f.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat_synthetic\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_synthetic\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n", f.model, f.model)
				} else {
					_, _ = fmt.Fprintf(w, `{"id":"chat_synthetic","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, f.model)
				}
			} else {
				response := fmt.Sprintf(`{"id":"resp_recovered","object":"response","status":"completed","model":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, f.model)
				if f.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				} else {
					_, _ = fmt.Fprint(w, response)
				}
			}
		}))
		t.Cleanup(server.Close)
		priority := int64(200 - id*10)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Group: "default", Models: f.model, Priority: &priority, BaseURL: &server.URL,
			Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: f.model, ChannelId: id, Enabled: true, Priority: &priority}).Error)
		if id == 9 {
			first = channel
		}
	}
	common.MemoryCacheEnabled = f.cached
	if f.cached {
		model.InitChannelCache()
	}
	path, format := "/v1/responses", types.RelayFormat(types.RelayFormatOpenAIResponses)
	payload := fmt.Sprintf(`{"model":%q,"input":"synthetic","stream":%t}`, f.model, f.stream)
	if f.protocol == "chat" {
		path, format = "/v1/chat/completions", types.RelayFormatOpenAI
		payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"synthetic"}],"stream":%t}`, f.model, f.stream)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, f.model))
	started := time.Now().Add(-179 * time.Second)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, started)
	c.Set("relay_retry_started_at", started)
	Relay(c, format)
	mu.Lock()
	defer mu.Unlock()
	return streamResilienceResult{c, recorder, append([]int(nil), calls...)}
}

func TestGPTRetryBudgetSlowAttemptRecoversWithinRequestDeadline(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/cache=%t", protocol, stream, cached), func(t *testing.T) {
					got := runGPTRetryBudgetRelay(t, gptRetryBudgetFixture{protocol: protocol, stream: stream, cached: cached})
					t.Logf("calls=%v attempts=%d elapsed_ms=%d max_elapsed_ms=%d stop=%s", got.calls, got.context.GetInt("retry_attempt_no"), got.context.GetInt64("retry_elapsed_ms"), got.context.GetInt64("retry_max_elapsed_ms"), got.context.GetString("retry_stop_reason"))
					require.Equal(t, []int{9, 10}, got.calls)
					require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
					require.Contains(t, got.response.Body.String(), "recovered")
					require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
					require.Greater(t, got.context.GetInt64("retry_elapsed_ms"), int64(180000))
					require.InDelta(t, 1200000, got.context.GetInt64("retry_max_elapsed_ms"), 2)
				})
			}
		}
	}
}

func TestGPTRetryBudgetPreservesStopBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture gptRetryBudgetFixture
		stop    string
	}{
		{"non GPT", gptRetryBudgetFixture{protocol: "chat", model: "MiniMax-M3"}, service.RetryStopReasonDeadlineExceeded},
		{"explicit operator budget", gptRetryBudgetFixture{explicit: "180"}, service.RetryStopReasonDeadlineExceeded},
		{"attempt limit", gptRetryBudgetFixture{maxAttempts: 1}, service.RetryStopReasonAttemptsExhausted},
		{"caller cancel", gptRetryBudgetFixture{first: "cancel"}, service.RetryStopReasonClientGone},
		{"unknown accepted request", gptRetryBudgetFixture{first: "unknown"}, service.RetryStopReasonUpstreamUnknown},
		{"tool output started", gptRetryBudgetFixture{first: "output", stream: true}, service.RetryStopReasonOutputStarted},
		{"state owner", gptRetryBudgetFixture{first: "state"}, service.RetryStopReasonNotRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGPTRetryBudgetRelay(t, tc.fixture)
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, tc.stop, got.context.GetString("retry_stop_reason"))
			require.NotContains(t, got.response.Body.String(), "recovered")
		})
	}
}
