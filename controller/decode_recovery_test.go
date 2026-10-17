package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type decodeRecoveryFixture struct {
	codes                 []string
	status                int
	cached, healthy       bool
	barrier               string
	protocol              string
	nonStream             bool
	message               string
	payload               string
	attemptLimit          int
	verifyFunctionHistory bool
	statusMapping         string
	modelName             string
}

func runDecodeRecoveryRelay(t *testing.T, fixture decodeRecoveryFixture) streamResilienceResult {
	t.Helper()
	modelName := fixture.modelName
	if modelName == "" {
		modelName = "gpt-5.6-sol"
	}
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
	if fixture.attemptLimit > 0 {
		t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", fmt.Sprint(fixture.attemptLimit))
	}
	if fixture.barrier == "budget" {
		t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "1")
	}
	if fixture.status == 0 {
		fixture.status = http.StatusUnprocessableEntity
	}
	if fixture.message == "" {
		fixture.message = "synthetic reasoning_budget_unsupported mention"
	}
	if fixture.barrier == "elapsed" {
		t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "1")
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
			var request map[string]json.RawMessage
			require.NoError(t, common.DecodeJson(r.Body, &request))
			var upstreamModel string
			require.NoError(t, common.Unmarshal(request["model"], &upstreamModel))
			require.Equal(t, modelName, upstreamModel)
			if fixture.verifyFunctionHistory {
				var tools []json.RawMessage
				require.NoError(t, common.Unmarshal(request["tools"], &tools))
				require.Len(t, tools, 9)
				field := "messages"
				if fixture.protocol != "chat" {
					field = "input"
				}
				require.Equal(t, 4, strings.Count(string(request[field]), "synthetic_result_"), "complete history must reach every attempted entrance")
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
				if fixture.barrier == "elapsed" {
					time.Sleep(1100 * time.Millisecond)
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
			if fixture.protocol == "chat" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_synthetic\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_capability_healthy\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}))
		t.Cleanup(server.Close)
		priority := int64(100 - index)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, Priority: &priority, BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		if fixture.statusMapping != "" {
			channel.StatusCodeMapping = &fixture.statusMapping
		}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: modelName, ChannelId: id, Enabled: true, Priority: &priority}).Error)
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
	if fixture.payload != "" {
		body = fixture.payload
	}
	if fixture.protocol == "images" {
		path, format = "/v1/images/generations", types.RelayFormatOpenAIImage
	}
	body = strings.ReplaceAll(body, "gpt-5.6-sol", modelName)
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
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, modelName))
	Relay(c, format)
	mu.Lock()
	defer mu.Unlock()
	return streamResilienceResult{c, recorder, append([]int(nil), calls...)}
}

func recoveryFunctionPayload(t *testing.T, protocol string) string {
	t.Helper()
	tools := []any{}
	for i := 0; i < 9; i++ {
		definition := map[string]any{"name": fmt.Sprintf("synthetic_fn_%d", i), "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
		if protocol == "chat" {
			tools = append(tools, map[string]any{"type": "function", "function": definition})
		} else {
			definition["type"] = "function"
			tools = append(tools, definition)
		}
	}
	history := []any{map[string]any{"role": "user", "content": "synthetic text"}}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("call_synthetic_%d", i)
		if protocol == "chat" {
			history = append(history, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": fmt.Sprintf("synthetic_fn_%d", i), "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": id, "content": fmt.Sprintf("synthetic_result_%d", i)})
		} else {
			history = append(history, map[string]any{"type": "function_call", "call_id": id, "name": fmt.Sprintf("synthetic_fn_%d", i), "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": id, "output": fmt.Sprintf("synthetic_result_%d", i)})
		}
	}
	body := map[string]any{"model": "gpt-5.6-sol", "tools": tools, "stream": false}
	if protocol == "chat" {
		body["messages"] = history
	} else {
		body["input"] = history
	}
	data, err := common.Marshal(body)
	require.NoError(t, err)
	return string(data)
}

func TestDecodeRecoveryCompletesWithoutDroppingFunctionHistory(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%t", protocol, cached), func(t *testing.T) {
				got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, nonStream: true, cached: cached, protocol: protocol, payload: recoveryFunctionPayload(t, protocol), verifyFunctionHistory: true, attemptLimit: 2, modelName: "mimo-v2.6-pro"})
				t.Logf("calls=%v stop=%s", got.calls, got.context.GetString("retry_stop_reason"))
				require.Equal(t, []int{9, 10}, got.calls)
				require.Equal(t, 200, got.response.Code)
				require.Equal(t, 2, got.context.GetInt("retry_attempt_no"))
				require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
			})
		}
	}
}

func TestDecodeRecoveryNeverRevivesFailedEntrancesOrAddsBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codes []string
		limit int
		want  []int
	}{
		{"single entrance", []string{"upstream_response_decode_failed"}, 6, []int{9}},
		{"all decode failures", []string{"upstream_response_decode_failed", "upstream_response_decode_failed"}, 6, []int{9, 10}},
		{"mixed failures", []string{"upstream_response_decode_failed", "server_error"}, 6, []int{9, 10}},
		{"shared attempt limit", []string{"upstream_response_decode_failed", "server_error", "upstream_response_decode_failed"}, 2, []int{9, 10}},
	} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, cached), func(t *testing.T) {
				got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: tc.codes, status: 502, nonStream: true, cached: cached, protocol: "chat", attemptLimit: tc.limit})
				t.Logf("calls=%v stop=%s", got.calls, got.context.GetString("retry_stop_reason"))
				require.Equal(t, tc.want, got.calls)
				require.Equal(t, 502, got.response.Code)
				require.Equal(t, len(tc.want), got.context.GetInt("retry_attempt_no"))
			})
		}
	}
}

func TestDecodeRecoveryKeepsDynamicSafetyBarriers(t *testing.T) {
	for _, barrier := range []string{"output", "cancel", "budget", "elapsed", "specific", "affinity", "unknown"} {
		t.Run(barrier, func(t *testing.T) {
			got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, barrier: barrier, protocol: "responses"})
			require.Equal(t, []int{9}, got.calls)
			require.NotContains(t, got.response.Body.String(), "resp_capability_healthy")
		})
	}
}

func TestDecodeRecoveryKeepsStateAndHostedToolBarriers(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"previous response", `{"model":"gpt-5.6-sol","input":"synthetic","previous_response_id":"resp_synthetic_previous"}`},
		{"conversation", `{"model":"gpt-5.6-sol","input":"synthetic","conversation":"conv_synthetic"}`},
		{"background", `{"model":"gpt-5.6-sol","input":"synthetic","background":true}`},
		{"item reference", `{"model":"gpt-5.6-sol","input":[{"type":"item_reference","id":"item_synthetic"}]}`},
		{"encrypted reasoning", `{"model":"gpt-5.6-sol","input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`},
		{"hosted tool", `{"model":"gpt-5.6-sol","input":"synthetic","tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`},
		{"remote MCP", `{"model":"gpt-5.6-sol","input":"synthetic","tools":[{"type":"mcp","server_label":"synthetic","server_url":"https://example.invalid/mcp"}]}`},
		{"custom tool", `{"model":"gpt-5.6-sol","input":"synthetic","tools":[{"type":"custom","name":"synthetic"}]}`},
		{"unpaired call", `{"model":"gpt-5.6-sol","input":[{"type":"function_call","call_id":"call_synthetic","name":"f","arguments":"{}"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, nonStream: true, protocol: "responses", payload: tc.payload})
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, 502, got.response.Code)
		})
	}
	for _, code := range []string{"state_owner_unavailable", "state_owner_conflict", "local_state_unavailable"} {
		t.Run(code, func(t *testing.T) {
			got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{code}, status: 409, healthy: true, nonStream: true})
			require.Equal(t, []int{9}, got.calls)
		})
	}
}

func TestDecodeRecoveryMediaAndUnknownShapesAreExcluded(t *testing.T) {
	for _, tc := range []struct {
		path   string
		format types.RelayFormat
	}{
		{"/v1/images/generations", types.RelayFormatOpenAIImage}, {"/v1/audio/speech", types.RelayFormatOpenAI}, {"/v1/responses/compact", types.RelayFormatOpenAIResponses}, {"/v1/messages", types.RelayFormatClaude},
	} {
		t.Run(tc.path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"synthetic","input":"synthetic"}`))
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
			require.False(t, decodeRecoveryRequestSafe(c, info, tc.format))
		})
	}
	for _, body := range []string{
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"synthetic"}}]}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"missing","content":"synthetic"}]}`,
		`{"messages":[{"role":"user","content":"synthetic"}],"modalities":["audio"]}`,
		`{"messages":[{"role":"user","content":"synthetic"}],"extra_body":{"tools":[{"type":"shell"}]}}`,
	} {
		require.False(t, decodeRecoveryPayloadSafe([]byte(body), types.RelayFormatOpenAI))
	}
}

func TestDecodeRecoveryRejectsAmbiguousJSONAndUnverifiedErrors(t *testing.T) {
	for _, body := range []string{
		`{"input":"safe","input":[{"type":"item_reference","id":"opaque"}]}`,
		`{"messages":[{"role":"user","content":"safe"}],"messages":[{"role":"tool","tool_call_id":"unknown","content":"opaque"}]}`,
		`{"input":"safe","tools":[],"tools":[{"type":"mcp"}]}`,
		`{"input":"safe","tools":[{"type":"function","type":"mcp","name":"f","parameters":{}}]}`,
		`{"input":[{"role":"user","role":"tool","content":"safe"}]}`,
		`{"input":[{"type":"function_call","type":"item_reference","name":"f","arguments":"{}","call_id":"x"},{"type":"function_call_output","call_id":"x","output":"safe"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","type":"image","text":"safe"}]}]}`,
	} {
		require.False(t, decodeRecoveryPayloadSafe([]byte(body), types.RelayFormatOpenAIResponses))
		require.False(t, decodeRecoveryPayloadSafe([]byte(body), types.RelayFormatOpenAI))
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"safe"}`))
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	err := types.WithOpenAIError(types.OpenAIError{Code: "upstream_response_decode_failed", Message: "synthetic"}, 502)
	require.True(t, relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIResponses, err), "local code without an upstream response must not establish recovery")
}

func TestDecodeRecoveryPlainStreamingCompletes(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, protocol: protocol})
			require.Equal(t, []int{9, 10}, got.calls)
			require.Equal(t, 200, got.response.Code)
			require.Equal(t, service.RetryStopReasonSuccess, got.context.GetString("retry_stop_reason"))
		})
	}
}

func TestDecodeRecoveryStatusMappingKeepsRiskAndBudgetGates(t *testing.T) {
	for _, mapped := range []int{400, 500, 503} {
		for _, stateful := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/stateful=%t", mapped, stateful), func(t *testing.T) {
				payload := recoveryFunctionPayload(t, "responses")
				if stateful {
					payload = `{"model":"gpt-5.6-sol","input":"synthetic","previous_response_id":"resp_synthetic"}`
				}
				got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, statusMapping: fmt.Sprintf(`{"502":"%d"}`, mapped), healthy: true, nonStream: true, protocol: "responses", payload: payload, attemptLimit: 2})
				if stateful || mapped == 400 {
					require.Equal(t, []int{9}, got.calls)
					require.Equal(t, mapped, got.response.Code)
				} else {
					require.Equal(t, []int{9, 10}, got.calls)
					require.Equal(t, 200, got.response.Code)
				}
			})
		}
	}
}

func TestDecodeRecoveryMapped400CannotBypassSafetyWithWideRetryRange(t *testing.T) {
	old := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = old })
	got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, statusMapping: `{"502":"400"}`, healthy: true, nonStream: true, protocol: "responses"})
	require.Equal(t, []int{9}, got.calls)
	require.Equal(t, 400, got.response.Code)
}
