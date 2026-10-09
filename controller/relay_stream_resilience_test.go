package controller

import (
	"bytes"
	"context"
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

type streamResilienceFixture struct {
	cached             bool
	first              string
	middle             string
	maxAttempts        int
	emptyCandidates    int
	allCandidatesEmpty bool
}

type streamResilienceResult struct {
	context  *gin.Context
	response *httptest.ResponseRecorder
	calls    []int
}

// The controller, selector, transport and SSE parser are real. Every upstream,
// database and circuit store is local and synthetic; no production key is used.
func runStreamResilienceRelay(t *testing.T, fixture streamResilienceFixture) streamResilienceResult {
	t.Helper()
	setupCPAContractChannels(t, true)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	oldGroup := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroup))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 3, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	if fixture.maxAttempts == 0 {
		fixture.maxAttempts = 4
	}
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "1")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", fmt.Sprint(fixture.maxAttempts))
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })

	requestCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	var calls []int
	var first model.Channel
	if fixture.emptyCandidates == 0 {
		fixture.emptyCandidates = 1
	}
	healthyID := 10 + fixture.emptyCandidates
	for id := 9; id <= healthyID; id++ {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, id)
			mu.Unlock()
			if id == 9 {
				switch fixture.first {
				case "created_then_close":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_synthetic_started\",\"status\":\"in_progress\"}}\n\n")
					return
				case "tool_then_close":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_synthetic\",\"call_id\":\"call_synthetic\",\"name\":\"synthetic_tool\",\"arguments\":\"{}\"}}\n\n")
					return
				case "usage_then_error":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_synthetic_failed\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"synthetic failure\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
					return
				case "written_request_unknown":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack local synthetic connection: %v", err)
						return
					}
					_ = conn.Close()
					return
				case "empty_stream":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					return
				case "cancel":
					cancel()
				}
				status, code := http.StatusServiceUnavailable, "server_error"
				switch fixture.first {
				case "request_error":
					status, code = http.StatusBadRequest, "request_feature_unsupported"
				case "owner_error":
					status, code = http.StatusConflict, "state_owner_unavailable"
				case "sampling_error":
					status, code = http.StatusUnprocessableEntity, "gpt_sampling_unsupported"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"server_error","code":%q,"message":"synthetic pre-output rejection"}}`, code)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_synthetic_healthy\",\"status\":\"in_progress\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic_healthy\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}))
		t.Cleanup(server.Close)
		priority := int64(200 - id*10)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Group: "default", Models: "gpt-5.6-sol", Priority: &priority, BaseURL: &server.URL,
			Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		if id > 9 && (id < healthyID || fixture.allCandidatesEmpty) {
			switch fixture.middle {
			case "no_enabled_key":
				channel.ChannelInfo.IsMultiKey = true
				channel.ChannelInfo.MultiKeyStatusList = map[int]int{0: common.ChannelStatusManuallyDisabled}
			case "connection_refused":
				server.Close()
			case "manually_disabled":
				channel.Status = common.ChannelStatusManuallyDisabled
			}
		}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: "gpt-5.6-sol", ChannelId: id,
			Enabled: channel.Status == common.ChannelStatusEnabled, Priority: &priority}).Error)
		if id == 9 {
			first = channel
		}
	}
	common.MemoryCacheEnabled = fixture.cached
	if fixture.cached {
		model.InitChannelCache()
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"synthetic","stream":true}`)).WithContext(requestCtx)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-5.6-sol"))
	Relay(c, types.RelayFormatOpenAIResponses)
	mu.Lock()
	defer mu.Unlock()
	return streamResilienceResult{c, recorder, append([]int(nil), calls...)}
}

func TestStreamResilienceSkipsUnusableCandidateBeforeHealthyFallback(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, middle := range []string{"no_enabled_key", "connection_refused", "manually_disabled"} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, middle), func(t *testing.T) {
				got := runStreamResilienceRelay(t, streamResilienceFixture{cached: cached, middle: middle})
				t.Logf("http_calls=%v selected_attempts=%v stop_reason=%s", got.calls, got.context.GetStringSlice("use_channel"), got.context.GetString("retry_stop_reason"))
				require.Equal(t, []int{9, 11}, got.calls, "an unusable middle candidate must not hide the healthy third entrance")
				require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
				require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
				require.NotContains(t, got.response.Body.String(), `"error"`)
				require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
				if middle == "no_enabled_key" {
					require.Equal(t, 2, got.context.GetInt("retry_attempt_no"), "a local rejection does not spend an upstream attempt")
					require.Equal(t, 1, got.context.GetInt("retry_candidate_skip_count"))
					require.Equal(t, "no_enabled_keys", got.context.GetString("retry_candidate_skip_reason"))
				}
			})
		}
	}
}

func TestStreamResilienceDoesNotReplayUnsafeOrTerminalFailure(t *testing.T) {
	for _, first := range []string{"created_then_close", "tool_then_close", "usage_then_error", "written_request_unknown", "empty_stream", "request_error", "owner_error", "sampling_error", "cancel"} {
		t.Run(first, func(t *testing.T) {
			got := runStreamResilienceRelay(t, streamResilienceFixture{first: first})
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
			require.NotContains(t, got.response.Body.String(), "resp_synthetic_healthy")
			require.NotContains(t, got.response.Body.String(), `"type":"response.completed"`)
			if first == "created_then_close" || first == "tool_then_close" {
				require.Equal(t, service.RetryStopReasonOutputStarted, got.context.GetString("retry_stop_reason"))
			}
			if first == "empty_stream" || first == "written_request_unknown" {
				require.Equal(t, service.RetryStopReasonUpstreamUnknown, got.context.GetString("retry_stop_reason"))
			}
			if first == "cancel" {
				require.Equal(t, service.RetryStopReasonClientGone, got.context.GetString("retry_stop_reason"))
			}
		})
	}
	t.Run("attempt budget", func(t *testing.T) {
		got := runStreamResilienceRelay(t, streamResilienceFixture{maxAttempts: 1})
		require.Equal(t, []int{9}, got.calls)
		require.Equal(t, service.RetryStopReasonAttemptsExhausted, got.context.GetString("retry_stop_reason"))
	})
	for _, middle := range []string{"connection_refused"} {
		t.Run("candidate budget/"+middle, func(t *testing.T) {
			got := runStreamResilienceRelay(t, streamResilienceFixture{middle: middle, maxAttempts: 2})
			require.Equal(t, []int{9}, got.calls, "an untried healthy candidate does not waive the hard budget")
			require.Equal(t, 2, got.context.GetInt("retry_attempt_no"))
			require.Equal(t, service.RetryStopReasonAttemptsExhausted, got.context.GetString("retry_stop_reason"))
			if middle == "connection_refused" {
				require.False(t, common.GetContextKeyBool(got.context, constant.ContextKeyRelayRequestWritten))
				require.False(t, common.GetContextKeyBool(got.context, constant.ContextKeyRelayResponseHeaders))
			}
		})
	}
}

func TestStreamResilienceFinalDiagnosticsIncludeSingleAttemptWithoutPayload(t *testing.T) {
	var output bytes.Buffer
	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultWriter
	gin.DefaultWriter = &output
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = oldWriter
		common.LogWriterMu.Unlock()
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("private synthetic body"))
	c.Request.Header.Set("Authorization", "Bearer synthetic-secret")
	c.Set("channel_name", "private provider name")
	c.Set("use_channel", []string{"9"})
	state := service.NewRelayRetryState(types.RelayFormatOpenAIResponses, 1)
	state.RecordAttempt(9)
	state.StopReason = service.RetryStopReasonOutputStarted
	setRelayRetryDiagnostics(c, state)
	logRelayRetryRoute(c)
	require.Contains(t, output.String(), "relay_retry_final channels=9 attempts=1")
	require.Contains(t, output.String(), "stop_reason=output_started")
	require.Contains(t, output.String(), "max_attempts=")
	require.Contains(t, output.String(), "candidate_skip_limit=32")
	for _, private := range []string{"private synthetic body", "synthetic-secret", "private provider name", "已尝试全部"} {
		require.NotContains(t, output.String(), private)
	}
}

func TestStreamResilienceBoundsCandidateSkipsSeparatelyFromUpstreamAttempts(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t/two empty candidates", cached), func(t *testing.T) {
			got := runStreamResilienceRelay(t, streamResilienceFixture{cached: cached, middle: "no_enabled_key", emptyCandidates: 2, maxAttempts: 2})
			require.Equal(t, []int{9, 12}, got.calls)
			require.Equal(t, 2, got.context.GetInt("retry_attempt_no"))
			require.Equal(t, 2, got.context.GetInt("retry_distinct_channel_count"))
			require.Equal(t, 2, got.context.GetInt("retry_candidate_skip_count"))
			require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
			require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
		})
		t.Run(fmt.Sprintf("cache=%t/all empty candidates", cached), func(t *testing.T) {
			got := runStreamResilienceRelay(t, streamResilienceFixture{cached: cached, middle: "no_enabled_key", emptyCandidates: maxRelayCandidateSkips + 1, allCandidatesEmpty: true, maxAttempts: 2})
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
			require.Equal(t, maxRelayCandidateSkips, got.context.GetInt("retry_candidate_skip_count"))
			require.Equal(t, service.RetryStopReasonCandidateLimit, got.context.GetString("retry_stop_reason"))
		})
	}
}
