package controller

import (
	"fmt"
	"io"
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
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type formatWireCall struct {
	path string
	body []byte
}

func runFormatWireRelay(t *testing.T, path, body string, format types.RelayFormat, firstType int, fallback bool, viaResponses ...bool) (*httptest.ResponseRecorder, *gin.Context, []formatWireCall) {
	t.Helper()
	setupCPAContractChannels(t, false)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree, oldRatio := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume, ratio_setting.GroupRatio2JSONString()
	settings := model_setting.GetGlobalSettings()
	oldPass, oldPolicy := settings.PassThroughRequestEnabled, settings.ChatCompletionsToResponsesPolicy
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		_ = ratio_setting.UpdateGroupRatioByJSONString(oldRatio)
		settings.PassThroughRequestEnabled, settings.ChatCompletionsToResponsesPolicy = oldPass, oldPolicy
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy.Enabled = false
	if len(viaResponses) > 0 && viaResponses[0] {
		settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
	}
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "3")
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "2")
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
	service.InitHttpClient()
	var mu sync.Mutex
	var calls []formatWireCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, formatWireCall{r.URL.Path, append([]byte(nil), data...)})
		mu.Unlock()
		stream := gjson.GetBytes(data, "stream").Bool()
		w.Header().Set("Content-Type", "application/json")
		var response string
		switch r.URL.Path {
		case "/v1/messages":
			response = `{"id":"msg_synthetic","type":"message","model":"gpt-5.5","role":"assistant","content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
			if stream {
				response = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic\",\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{}\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
			}
		case "/v1/responses":
			response = `{"id":"resp_synthetic","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":10,"output_tokens":2}}`
			if stream {
				response = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
			}
		default:
			response = `{"id":"chatcmpl_synthetic","object":"chat.completion","model":"gpt-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
			if stream {
				response = "data: {\"id\":\"chatcmpl_synthetic\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
			}
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	modelName := gjson.Get(body, "model").String()
	var first model.Channel
	count := 1
	if fallback {
		count = 2
	}
	for i := 0; i < count; i++ {
		kind := firstType
		if i > 0 {
			kind = constant.ChannelTypeOpenAI
		}
		priority := int64(100 - i*10)
		channel := model.Channel{Id: 9 + i, Type: kind, Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, Priority: &priority, BaseURL: &server.URL, Key: "synthetic", AutoBan: common.GetPointer(0)}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: modelName, ChannelId: channel.Id, Enabled: true, Priority: &priority}).Error)
		if i == 0 {
			first = channel
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, modelName))
	Relay(c, format)
	mu.Lock()
	defer mu.Unlock()
	return w, c, append([]formatWireCall(nil), calls...)
}

const wireExactSchema = `{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993,"minimum":0},"flag":{"type":"boolean","default":false}},"required":["n"],"additionalProperties":false}`

func TestStructuredOutputActualWireAcrossProtocols(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, path, fields, wantPath, schemaPath string
			format                                   types.RelayFormat
			kind                                     int
		}{
			{"chat to messages", "/v1/chat/completions", `"messages":[{"role":"user","content":"synthetic"}],"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":` + wireExactSchema + `}}`, "/v1/messages", "output_config.format.schema", types.RelayFormatOpenAI, constant.ChannelTypeAnthropic},
			{"responses to messages", "/v1/responses", `"input":"synthetic","text":{"format":{"type":"json_schema","name":"result","strict":true,"schema":` + wireExactSchema + `}}`, "/v1/messages", "output_config.format.schema", types.RelayFormatOpenAIResponses, constant.ChannelTypeAnthropic},
			{"messages to chat", "/v1/messages", `"messages":[{"role":"user","content":"synthetic"}],"max_tokens":32,"output_config":{"format":{"type":"json_schema","schema":` + wireExactSchema + `}}`, "/v1/chat/completions", "response_format.json_schema.schema", types.RelayFormatClaude, constant.ChannelTypeOpenAI},
			{"legacy messages to chat", "/v1/messages", `"messages":[{"role":"user","content":"synthetic"}],"max_tokens":32,"output_format":{"type":"json_schema","schema":` + wireExactSchema + `}`, "/v1/chat/completions", "response_format.json_schema.schema", types.RelayFormatClaude, constant.ChannelTypeOpenAI},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,%s}`, stream, tc.fields)
				w, c, calls := runFormatWireRelay(t, tc.path, body, tc.format, tc.kind, false)
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Len(t, calls, 1)
				require.Equal(t, tc.wantPath, calls[0].path)
				require.Equal(t, "9007199254740993", gjson.GetBytes(calls[0].body, tc.schemaPath+".properties.n.const").Raw)
				require.Equal(t, "0", gjson.GetBytes(calls[0].body, tc.schemaPath+".properties.n.minimum").Raw)
				require.Equal(t, "false", gjson.GetBytes(calls[0].body, tc.schemaPath+".additionalProperties").Raw)
				require.Equal(t, fmt.Sprint(stream), gjson.GetBytes(calls[0].body, "stream").Raw)
				require.Equal(t, "completed", c.GetString("relay_final_outcome"))
			})
		}
	}
}

func TestUnsupportedMessagesFormatFallsBackBeforeAnyGeneration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, formatJSON := range []string{`{"type":"json_object"}`, `{"type":"json_schema","json_schema":{"name":"result","strict":false,"schema":` + wireExactSchema + `}}`} {
			t.Run(fmt.Sprintf("%s/stream=%t", formatJSON, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"messages":[{"role":"user","content":"synthetic"}],"response_format":%s}`, stream, formatJSON)
				w, c, calls := runFormatWireRelay(t, "/v1/chat/completions", body, types.RelayFormatOpenAI, constant.ChannelTypeAnthropic, true)
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Len(t, calls, 1, "incompatible Messages route must not receive a generation request")
				require.Equal(t, "/v1/chat/completions", calls[0].path)
				require.JSONEq(t, formatJSON, gjson.GetBytes(calls[0].body, "response_format").Raw)
				require.Equal(t, []string{"9", "10"}, c.GetStringSlice("use_channel"))
			})
		}
	}
}

func TestInvalidOrUnknownOutputFormatNeverGenerates(t *testing.T) {
	for _, tc := range []struct {
		formatJSON string
		status     int
	}{
		{`{"type":"json_object","future_flag":false}`, 422},
		{`{"type":"json_schema","json_schema":{"name":"result","schema":{},"future_limit":0}}`, 422},
		{`{"type":"json_schema","json_schema":{"name":"result","schema":{},"strict":0}}`, 400},
	} {
		t.Run(tc.formatJSON, func(t *testing.T) {
			body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic"}],"response_format":` + tc.formatJSON + `}`
			w, _, calls := runFormatWireRelay(t, "/v1/chat/completions", body, types.RelayFormatOpenAI, constant.ChannelTypeAnthropic, false)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Empty(t, calls)
		})
	}
}

func TestStructuredOutputNativeResponsesWireAndEffortMerge(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, messages := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses/stream=%t/messages=%t", stream, messages), func(t *testing.T) {
				path, format := "/v1/chat/completions", types.RelayFormat(types.RelayFormatOpenAI)
				fields := `"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":false,"schema":` + wireExactSchema + `}}`
				if messages {
					path, format = "/v1/messages", types.RelayFormatClaude
					fields = `"max_tokens":32,"output_config":{"format":{"type":"json_schema","schema":` + wireExactSchema + `}}`
				}
				body := fmt.Sprintf(`{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic"}],"stream":%t,%s}`, stream, fields)
				w, _, calls := runFormatWireRelay(t, path, body, format, constant.ChannelTypeOpenAI, false, true)
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Len(t, calls, 1)
				require.Equal(t, "/v1/responses", calls[0].path)
				require.Equal(t, "9007199254740993", gjson.GetBytes(calls[0].body, "text.format.schema.properties.n.const").Raw)
				require.Equal(t, fmt.Sprint(messages), gjson.GetBytes(calls[0].body, "text.format.strict").Raw)
			})
		}
	}
	for _, modelName := range []string{"claude-opus-4-7-high", "claude-opus-4-7-thinking"} {
		t.Run(modelName, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"synthetic"}],"max_tokens":32,"output_config":{"format":{"type":"json_schema","schema":%s}}}`, modelName, wireExactSchema)
			w, _, calls := runFormatWireRelay(t, "/v1/messages", body, types.RelayFormatClaude, constant.ChannelTypeAnthropic, false)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Len(t, calls, 1)
			require.Equal(t, "9007199254740993", gjson.GetBytes(calls[0].body, "output_config.format.schema.properties.n.const").Raw)
			require.Equal(t, "high", gjson.GetBytes(calls[0].body, "output_config.effort").String())
		})
	}
}
