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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const cpaMediaCustomerMessage = "本次请求内容过大（要求：不超过 64 MB），图片和完整对话历史计入总量。请压缩图片；如果历史包含图片或工具截图，请新建对话，仅上传必要图片后重新发送。"

func TestCPAMediaErrorReachesCustomerWithoutReplayOrChannelPenalty(t *testing.T) {
	for _, source := range []string{"chat", "responses", "claude"} {
		for _, stream := range []bool{false, true} {
			for _, afterOutput := range []bool{false, true} {
				if afterOutput && !stream {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%v/afterOutput=%v", source, stream, afterOutput), func(t *testing.T) {
					setupCPAContractChannels(t, true)
					require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
					oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
					oldFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
					oldGroup := ratio_setting.GroupRatio2JSONString()
					oldRanges := operation_setting.AutomaticRetryStatusCodeRanges
					t.Cleanup(func() {
						common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
						operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
						operation_setting.AutomaticRetryStatusCodeRanges = oldRanges
						require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroup))
					})
					common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 3, false, false
					operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
					// Even an operator retry range containing 413/5xx cannot reopen it.
					operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
					require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
					service.InitHttpClient()
					var calls atomic.Int32
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if !afterOutput {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(413)
							fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error","code":"request_too_large"}}`, cpaMediaCustomerMessage)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if strings.HasSuffix(r.URL.Path, "/responses") {
							fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
							fmt.Fprintf(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"failed\",\"error\":{\"code\":\"request_too_large\",\"message\":%q}}}\n\n", cpaMediaCustomerMessage)
						} else {
							fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"}}]}\n\n")
							// The relay buffers one accepted Chat frame. A second
							// frame commits the first before the error is observed.
							fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" continued\"}}]}\n\n")
							fmt.Fprintf(w, "data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"request_too_large\",\"message\":%q}}\n\n", cpaMediaCustomerMessage)
						}
					}))
					defer remote.Close()
					var first model.Channel
					for _, id := range []int{9, 10} {
						priority := int64(110 - id)
						channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-6.1-sol", Priority: &priority, BaseURL: &remote.URL, Key: "synthetic-key"}
						require.NoError(t, model.DB.Save(&channel).Error)
						require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: "gpt-6.1-sol", ChannelId: id, Enabled: true, Priority: &priority}).Error)
						if id == 9 {
							first = channel
						}
					}
					path, format, input := "/v1/chat/completions", types.RelayFormatOpenAI, `"messages":[{"role":"user","content":"synthetic"}]`
					if source == "responses" {
						path, format, input = "/v1/responses", types.RelayFormatOpenAIResponses, `"input":"synthetic"`
					}
					if source == "claude" {
						path, format, input = "/v1/messages", types.RelayFormatClaude, `"max_tokens":16,"messages":[{"role":"user","content":"synthetic"}]`
					}
					body := fmt.Sprintf(`{"model":"gpt-6.1-sol",%s,"stream":%v}`, input, stream)
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
					common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
					common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
					common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
					require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-6.1-sol"))
					Relay(c, format)
					status := 413
					if afterOutput {
						status = 200
					}
					require.Equal(t, status, w.Code, w.Body.String())
					require.Equal(t, int32(1), calls.Load(), "media rejection was replayed")
					require.Contains(t, w.Body.String(), "request_too_large")
					require.Contains(t, w.Body.String(), "压缩图片")
					require.Contains(t, w.Body.String(), "新建对话")
					if afterOutput {
						require.NotContains(t, w.Body.String(), "[DONE]")
						require.NotContains(t, w.Body.String(), `"type":"message_stop"`)
					}
					var stored model.Channel
					require.NoError(t, model.DB.First(&stored, "id = ?", 9).Error)
					require.Equal(t, common.ChannelStatusEnabled, stored.Status)
				})
			}
		}
	}
}
