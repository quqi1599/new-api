package controller

import (
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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Drive the real Relay entrypoint rather than constructing RetryParam in the
// test. This catches missing production wiring for exclusion-aware selection.
func TestRelayPriorityFallbackUsesNextHealthyTier(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "database"
		if cached {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) {
			setupCPAContractChannels(t, true)
			require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
			oldRetry, oldConsumeLog, oldErrorLog := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
			oldFreePreConsume := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
			oldGroupRatio := ratio_setting.GroupRatio2JSONString()
			t.Cleanup(func() {
				common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsumeLog, oldErrorLog
				operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFreePreConsume
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroupRatio))
			})
			common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
			operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
			t.Setenv("RELAY_RETRY_MAX_ROUNDS", "1")
			t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "3")
			service.InitHttpClient()

			var mu sync.Mutex
			var calls []int
			var first model.Channel
			for i, priority := range []int64{100, 90, 80} {
				id := 9 + i
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					calls = append(calls, int(priority))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if priority == 100 {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":{"message":"synthetic exhausted route","code":"auth_unavailable","type":"server_error"}}`))
						return
					}
					if priority == 80 {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":{"message":"unexpected lower priority","code":"request_feature_unsupported","type":"invalid_request_error"}}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
				}))
				t.Cleanup(server.Close)
				channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
					Group: "default", Models: "gpt-5.6-sol", Priority: common.GetPointer(priority), BaseURL: &server.URL, Key: "synthetic-test-key"}
				require.NoError(t, model.DB.Save(&channel).Error)
				ability := model.Ability{Group: "default", Model: "gpt-5.6-sol", ChannelId: id, Enabled: true, Priority: common.GetPointer(priority)}
				require.NoError(t, model.DB.Save(&ability).Error)
				if i == 0 {
					first = channel
				}
			}
			common.MemoryCacheEnabled = cached
			if cached {
				model.InitChannelCache()
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"synthetic"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, "gpt-5.6-sol"))

			Relay(c, types.RelayFormatOpenAIResponses)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), `"completed"`)
			mu.Lock()
			gotCalls := append([]int(nil), calls...)
			mu.Unlock()
			require.Equal(t, []int{100, 90}, gotCalls)
			require.Equal(t, 2, c.GetInt("retry_attempt_no"))
			require.Equal(t, service.RetryStopReasonSuccess, c.GetString("retry_stop_reason"))
		})
	}
}
