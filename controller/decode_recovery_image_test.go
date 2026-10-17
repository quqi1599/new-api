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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type imageDecodeRecoveryCase struct {
	name, model, firstModel, nextModel, extension string
	gemini, nextGemini, recover                   bool
}

func TestDecodeRecoveryNeverRegeneratesImplicitImageRequests(t *testing.T) {
	settings := model_setting.GetGeminiSettings()
	original := settings.SupportedImagineModels
	settings.SupportedImagineModels = append(append([]string(nil), original...), "synthetic-canvas-v7")
	t.Cleanup(func() { settings.SupportedImagineModels = original })

	for _, tc := range []imageDecodeRecoveryCase{
		{name: "gpt image 2", model: "gpt-image-2"},
		{name: "namespaced image model", model: "provider/gpt-image-2"},
		{name: "banana native chat conversion", model: "gemini-3-pro-image-preview", gemini: true},
		{name: "dynamic imagine model", model: "synthetic-canvas-v7", gemini: true},
		{name: "customer alias chain", model: "customer-model", firstModel: "gemini-3.1-flash-image-preview", nextModel: "gemini-2.5-flash-image"},
		{name: "dynamic alias chain", model: "customer-model", firstModel: "synthetic-canvas-v7", nextModel: "synthetic-text", gemini: true},
		{name: "raw image name survives mapping", model: "nano-banana-2", firstModel: "synthetic-text"},
		{name: "next channel maps text alias to image", model: "customer-model", firstModel: "synthetic-text", nextModel: "gpt-image-2"},
		{name: "next gemini channel maps alias to dynamic image", model: "customer-model", firstModel: "synthetic-text", nextModel: "synthetic-canvas-v7", nextGemini: true},
		{name: "explicit image modality", model: "synthetic-text", extension: `,"modalities":["image"]`},
		{name: "explicit image tool", model: "synthetic-text", extension: `,"tools":[{"type":"image_generation"}]`},
		{name: "ordinary text", model: "synthetic-text", recover: true},
		{name: "dynamic name on ordinary openai channel", model: "synthetic-canvas-v7", recover: true},
		{name: "ordinary text alias chain", model: "customer-model", firstModel: "synthetic-text", nextModel: "synthetic-alternate-text", recover: true},
	} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, cached), func(t *testing.T) {
				runImageDecodeRecoveryCase(t, tc, cached)
			})
		}
	}
}

func TestDecodeRecoveryImageModelsAndHostedToolsBlockResponses(t *testing.T) {
	for _, modelName := range []string{"gpt-image-2", "gemini-3.1-flash-image-preview", "nano-banana-2"} {
		t.Run(modelName, func(t *testing.T) {
			got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, nonStream: true, modelName: modelName})
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, http.StatusBadGateway, got.response.Code)
			require.Equal(t, service.RetryStopReasonUpstreamUnknown, got.context.GetString("retry_stop_reason"))
		})
	}
	t.Run("hosted image generation tool", func(t *testing.T) {
		got := runDecodeRecoveryRelay(t, decodeRecoveryFixture{codes: []string{"upstream_response_decode_failed"}, status: 502, healthy: true, nonStream: true,
			payload: `{"model":"gpt-5.6-sol","input":"synthetic prompt","tools":[{"type":"image_generation"}]}`})
		require.Equal(t, []int{9}, got.calls)
		require.Equal(t, http.StatusBadGateway, got.response.Code)
		require.Equal(t, service.RetryStopReasonUpstreamUnknown, got.context.GetString("retry_stop_reason"))
	})
}

func TestDecodeRecoveryImageEligibilityDoesNotMutateAttemptState(t *testing.T) {
	for _, target := range []string{"gpt-image-2", "synthetic-text"} {
		t.Run(target, func(t *testing.T) {
			body := `{"model":"customer-model","messages":[{"role":"user","content":"synthetic prompt"}]}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("model_mapping", fmt.Sprintf(`{"customer-model":"synthetic-middle","synthetic-middle":%q}`, target))
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			storage, err := common.GetBodyStorage(c)
			require.NoError(t, err)
			stored, err := storage.Bytes()
			require.NoError(t, err)
			var request dto.GeneralOpenAIRequest
			require.NoError(t, common.Unmarshal(stored, &request))
			info := &relaycommon.RelayInfo{OriginModelName: "customer-model", Request: &request,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "previous-effective", IsModelMapped: true},
				PriceData:   types.PriceData{QuotaToPreConsume: 123, ModelPrice: 0.75}}
			info.MarkUpstreamRequestMayHaveBeenAccepted()
			requestBefore, err := common.Marshal(request)
			require.NoError(t, err)
			contextBefore, err := common.Marshal(c.Keys)
			require.NoError(t, err)
			metaBefore, priceBefore := *info.ChannelMeta, info.PriceData

			require.Equal(t, target == "synthetic-text", decodeRecoveryRequestSafe(c, info, types.RelayFormatOpenAI))

			requestAfter, err := common.Marshal(request)
			require.NoError(t, err)
			contextAfter, err := common.Marshal(c.Keys)
			require.NoError(t, err)
			require.JSONEq(t, string(requestBefore), string(requestAfter))
			require.JSONEq(t, string(contextBefore), string(contextAfter))
			require.Same(t, &request, info.Request)
			require.Equal(t, "customer-model", info.OriginModelName)
			require.Equal(t, metaBefore, *info.ChannelMeta)
			require.Equal(t, priceBefore, info.PriceData)
			require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
			storedAfter, err := storage.Bytes()
			require.NoError(t, err)
			require.Equal(t, body, string(storedAfter))
		})
	}
}

func runImageDecodeRecoveryCase(t *testing.T, tc imageDecodeRecoveryCase, cached bool) {
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
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })

	var mu sync.Mutex
	var calls []int
	var first model.Channel
	for index := 0; index < 2; index++ {
		id := 9 + index
		gemini := tc.gemini || index == 1 && tc.nextGemini
		upstreamModel := tc.firstModel
		if index == 1 {
			upstreamModel = tc.nextModel
		}
		if upstreamModel == "" {
			upstreamModel = tc.model
		}
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read synthetic upstream request: %v", err)
				w.WriteHeader(500)
				return
			}
			mu.Lock()
			calls = append(calls, id)
			mu.Unlock()
			if gemini {
				if r.URL.Path != "/v1beta/models/"+upstreamModel+":generateContent" || !strings.Contains(gjson.GetBytes(body, "generationConfig.responseModalities").Raw, `"IMAGE"`) {
					t.Errorf("implicit image conversion was not applied: path=%s modalities=%s", r.URL.Path, gjson.GetBytes(body, "generationConfig.responseModalities").Raw)
				}
			} else if gjson.GetBytes(body, "model").String() != upstreamModel {
				t.Errorf("expected mapped model %q, got %q", upstreamModel, gjson.GetBytes(body, "model").String())
			}
			w.Header().Set("Content-Type", "application/json")
			if index == 0 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"code":"upstream_response_decode_failed","type":"invalid_request_error","message":"synthetic accepted generation decode failure"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chatcmpl_synthetic","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"recovered text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}))
		t.Cleanup(remote.Close)
		priority := int64(100 - index)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: tc.model, Priority: &priority, BaseURL: &remote.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		if gemini {
			channel.Type = constant.ChannelTypeGemini
		}
		if upstreamModel != tc.model {
			mapping, err := common.Marshal(map[string]string{tc.model: "synthetic-intermediate", "synthetic-intermediate": upstreamModel})
			require.NoError(t, err)
			channel.ModelMapping = common.GetPointer(string(mapping))
		}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: tc.model, ChannelId: id, Enabled: true, Priority: &priority}).Error)
		if index == 0 {
			first = channel
		}
	}
	common.MemoryCacheEnabled = cached
	if cached {
		model.InitChannelCache()
	}

	type result struct {
		attempts int
		stop     string
	}
	completed := make(chan result, 1)
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
		if err := middleware.SetupContextForSelectedChannel(c, &first, tc.model); err != nil {
			t.Errorf("set synthetic channel: %v", err)
			c.Status(500)
		} else {
			Relay(c, types.RelayFormatOpenAI)
		}
		completed <- result{attempts: c.GetInt("retry_attempt_no"), stop: c.GetString("retry_stop_reason")}
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"synthetic prompt"}],"stream":false%s}`, tc.model, tc.extension)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	require.NoError(t, err)
	diagnostics := <-completed
	mu.Lock()
	gotCalls := append([]int(nil), calls...)
	mu.Unlock()
	if tc.recover {
		require.Equal(t, []int{9, 10}, gotCalls)
		require.Equal(t, http.StatusOK, response.StatusCode, string(responseBody))
		require.Equal(t, 2, diagnostics.attempts)
		require.Equal(t, service.RetryStopReasonSuccess, diagnostics.stop)
	} else {
		require.Equal(t, []int{9}, gotCalls, "accepted image work must not be generated through another alias or channel")
		require.Equal(t, http.StatusBadGateway, response.StatusCode, string(responseBody))
		require.Equal(t, 1, diagnostics.attempts)
		require.Equal(t, service.RetryStopReasonUpstreamUnknown, diagnostics.stop)
		require.Contains(t, string(responseBody), "upstream_response_decode_failed")
	}
}
