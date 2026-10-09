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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const samplingRejectionFixture = `{"error":{"message":"unsupported sampling; fields=temperature,top_p; reasoning=model_default; reasoning_source=thinking.type","type":"invalid_request_error","code":"gpt_sampling_unsupported"}}`

func TestGPTSamplingRejectionStopsRelayReplay(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/cache=%t", protocol, stream, cached), func(t *testing.T) {
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
					common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
					operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
					require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
					t.Setenv("RELAY_RETRY_MAX_ROUNDS", "3")
					t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "3")
					service.InitHttpClient()
					var calls atomic.Int32
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = w.Write([]byte(samplingRejectionFixture))
					}))
					defer remote.Close()
					var first model.Channel
					for _, id := range []int{9, 10} {
						priority := int64(110 - id)
						channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-6.1-sol", Priority: &priority, BaseURL: &remote.URL, Key: "synthetic-key"}
						require.NoError(t, model.DB.Save(&channel).Error)
						ability := model.Ability{Group: "default", Model: "gpt-6.1-sol", ChannelId: id, Enabled: true, Priority: &priority}
						require.NoError(t, model.DB.Save(&ability).Error)
						if id == 9 {
							first = channel
						}
					}
					common.MemoryCacheEnabled = cached
					if cached {
						model.InitChannelCache()
					}
					path, format, input := "/v1/chat/completions", types.RelayFormatOpenAI, `"messages":[{"role":"user","content":"synthetic"}]`
					if protocol == "responses" {
						path, format, input = "/v1/responses", types.RelayFormatOpenAIResponses, `"input":"synthetic"`
					}
					body := fmt.Sprintf(`{"model":"gpt-6.1-sol",%s,"stream":%t,"temperature":0.7,"top_p":1,"thinking":{"type":"enabled"}}`, input, stream)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
					common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
					common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
					common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
					require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-6.1-sol"))
					Relay(c, format)
					require.Equal(t, http.StatusUnprocessableEntity, recorder.Code, recorder.Body.String())
					require.Equal(t, int32(1), calls.Load(), "deterministic rejection was sent again")
					require.Equal(t, 1, c.GetInt("retry_attempt_no"))
					require.Equal(t, service.RetryStopReasonNotRetryable, c.GetString("retry_stop_reason"))
					require.Contains(t, recorder.Body.String(), "gpt_sampling_unsupported")
					require.Contains(t, recorder.Body.String(), "temperature,top_p")
					var stored model.Channel
					require.NoError(t, model.DB.First(&stored, "id = ?", 9).Error)
					require.Equal(t, common.ChannelStatusEnabled, stored.Status)
				})
			}
		}
	}
}

func TestGPTSamplingErrorCodeOverridesRetryStatusMapping(t *testing.T) {
	oldRanges := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })
	for _, status := range []int{400, 422, 429, 503} {
		err := parseCPAContractError(t, status, samplingRejectionFixture)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol"}
		require.False(t, shouldRetry(c, err, 16, types.RelayFormatOpenAI), "mapped status=%d", status)
		require.False(t, isGPTChannelFallbackError(info, err), "GPT fallback reopened retry status=%d", status)
	}
	err := parseCPAContractError(t, 422, `{"error":{"message":"previous request had gpt_sampling_unsupported","type":"server_error","code":"server_error"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, shouldRetry(c, err, 16, types.RelayFormatOpenAI), "message text must not control semantic retry policy")
}
