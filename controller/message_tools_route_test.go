package controller

import (
	"errors"
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
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMessageToolsLocalRouteFallbackPreservesRetryBarriers(t *testing.T) {
	for _, barrier := range []string{"", "written", "specific", "affinity", "budget"} {
		t.Run(barrier, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			budget := 2
			switch barrier {
			case "written":
				_, _ = c.Writer.WriteString("started")
			case "specific":
				c.Set("specific_channel_id", "9")
			case "affinity":
				c.Set("channel_affinity_skip_retry_on_failure", true)
			case "budget":
				budget = 0
			}
			err := types.NewMessageToolsRouteError()
			require.Equal(t, barrier == "", shouldRetry(c, err, budget, types.RelayFormatOpenAI))
			require.True(t, isKnownUnavailableRouteError(err))
			retry := &service.RetryParam{}
			recordRelayChannelFailure(retry, 9, err)
			require.Contains(t, retry.PersistentExcludedIds, 9)
		})
	}
	// Neither untyped local text nor a matching upstream code gets the local
	// exception. Retain SkipRetry here so this tests the added authorization,
	// independently of the operator's ordinary HTTP 422 retry configuration.
	for _, upstream := range []bool{false, true} {
		options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
		if upstream {
			options = append(options, types.ErrOptionWithUpstreamResponse())
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		err := types.NewErrorWithStatusCode(errors.New("message-level tools"), "message_tools_route_unsupported", 422, options...)
		require.False(t, shouldRetry(c, err, 2, types.RelayFormatOpenAI))
		require.False(t, isKnownUnavailableRouteError(err))
	}
}

func TestMessageToolsRouteFallsBackBeforeAnyLossyUpstreamRequest(t *testing.T) {
	for _, mode := range []string{"anthropic", "anthropic passthrough", "global passthrough", "responses conversion", "native passthrough", "no compatible route"} {
		t.Run(mode, func(t *testing.T) {
			setupCPAContractChannels(t, false)
			require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
			oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
			oldFree, oldRatios := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume, ratio_setting.GroupRatio2JSONString()
			oldGlobal := *model_setting.GetGlobalSettings()
			t.Cleanup(func() {
				common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
				operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
				*model_setting.GetGlobalSettings() = oldGlobal
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatios))
			})
			common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
			operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
			t.Setenv("RELAY_RETRY_MAX_ROUNDS", "3")
			t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "6")
			t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
			service.InitHttpClient()
			global := model_setting.GetGlobalSettings()
			global.PassThroughRequestEnabled = mode == "global passthrough"
			global.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: mode == "responses conversion" || mode == "native passthrough", ChannelIDs: []int{9}, ModelPatterns: []string{"^kimi-k3$"}}
			var calls [2]atomic.Int32
			var first model.Channel
			for index := 0; index < 2; index++ {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls[index].Add(1)
					require.Equal(t, "/v1/chat/completions", r.URL.Path)
					var wire dto.GeneralOpenAIRequest
					require.NoError(t, common.DecodeJson(r.Body, &wire))
					require.Len(t, wire.Messages, 2)
					require.JSONEq(t, `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"const":9007199254740993}}}}}]`, string(wire.Messages[0].Tools))
					require.Equal(t, "system", wire.Messages[0].Role)
					require.Nil(t, wire.Messages[0].Content)
					require.Empty(t, wire.Tools, "dynamic tools must not be promoted to top-level")
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"chatcmpl_fixture","object":"chat.completion","model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
				}))
				t.Cleanup(server.Close)
				kind := constant.ChannelTypeOpenAI
				if mode == "no compatible route" || index == 0 && (mode == "anthropic" || mode == "anthropic passthrough" || mode == "global passthrough") {
					kind = constant.ChannelTypeAnthropic
				}
				priority := int64(100 - index)
				channel := model.Channel{Id: 9 + index, Type: kind, Status: common.ChannelStatusEnabled, Group: "default", Models: "kimi-k3", Priority: &priority, BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
				if index == 0 && (mode == "anthropic passthrough" || mode == "native passthrough") {
					setting, err := common.Marshal(dto.ChannelSettings{PassThroughBodyEnabled: true})
					require.NoError(t, err)
					channel.Setting = common.GetPointer(string(setting))
				}
				require.NoError(t, model.DB.Save(&channel).Error)
				require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: "kimi-k3", ChannelId: channel.Id, Enabled: true, Priority: &priority}).Error)
				if index == 0 {
					first = channel
				}
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := `{"model":"kimi-k3","messages":[{"role":"system","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"const":9007199254740993}}}}}]},{"role":"user","content":"synthetic"}],"stream":false}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "kimi-k3"))
			Relay(c, types.RelayFormatOpenAI)
			if mode == "no compatible route" {
				require.Equal(t, http.StatusUnprocessableEntity, recorder.Code, recorder.Body.String())
				require.Contains(t, recorder.Body.String(), "message_tools_route_unsupported")
				require.Zero(t, calls[0].Load())
				require.Zero(t, calls[1].Load())
				require.Equal(t, service.RetryStopReasonNoChannel, c.GetString("retry_stop_reason"))
				return
			}
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), `"content":"ok"`)
			if mode == "native passthrough" {
				require.EqualValues(t, 1, calls[0].Load())
				require.Zero(t, calls[1].Load())
			} else {
				require.Zero(t, calls[0].Load(), "incompatible candidate must fail locally")
				require.EqualValues(t, 1, calls[1].Load(), "native Chat must remain available after local rejection")
			}
		})
	}
}
