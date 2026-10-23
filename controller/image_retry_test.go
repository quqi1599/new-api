package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestImageFailureRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name, code, errorType, param string
		status                       int
		local, skip, want            bool
	}{
		{name: "observed generic image rejection", code: "request_failed", status: 400, want: true},
		{name: "misreported upstream failure", code: "internal_server_error", status: 400, want: true},
		{name: "provider model unavailable", code: "model_not_supported", status: 422, want: true},
		{name: "unknown 400", status: 400},
		{name: "unknown 422", status: 422},
		{name: "invalid image", code: "invalid_image_input", status: 400},
		{name: "invalid size", code: "invalid_value", param: "size", status: 400},
		{name: "generic error with field rejection", code: "request_failed", param: "size", status: 400},
		{name: "content policy", code: "content_policy_violation", status: 400},
		{name: "policy type overrides generic code", code: "request_failed", errorType: "content_safety_blocked", status: 400},
		{name: "policy with misleading 503", code: "safety_violation", status: 503},
		{name: "invalid parameter with misleading 500", code: "invalid_parameter", status: 500},
		{name: "upstream key rejected", status: 401, want: true},
		{name: "upstream forbidden", status: 403, want: true},
		{name: "missing route", status: 404, want: true},
		{name: "explicit upstream timeout", status: 408, want: true},
		{name: "overload", status: 429, want: true},
		{name: "bad gateway", status: 502, want: true},
		{name: "explicit gateway failure", status: 504, want: true},
		{name: "too large", status: 413},
		{name: "local generic failure", code: "request_failed", status: 400, local: true},
		{name: "accepted decode failure", status: 502, skip: true},
		{name: "not a failure", status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []types.NewAPIErrorOptions{}
			if !tc.local {
				opts = append(opts, types.ErrOptionWithUpstreamResponse())
			}
			if tc.skip {
				opts = append(opts, types.ErrOptionWithSkipRetry())
			}
			err := types.WithOpenAIError(types.OpenAIError{Code: tc.code, Type: tc.errorType, Param: tc.param, Message: "synthetic rejection"}, tc.status, opts...)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			require.Equal(t, tc.want, imageFailureRetryable(err))
			require.Equal(t, tc.want, shouldRetry(c, err, 3, types.RelayFormatOpenAIImage))
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-image-2.5", RelayFormat: types.RelayFormatOpenAIImage}
			require.Equal(t, tc.want, isGPTChannelFallbackError(info, err))
			if tc.code == "request_failed" {
				require.False(t, shouldRetry(c, err, 3, types.RelayFormatOpenAI), "image recovery must not broaden text retries")
			}
		})
	}
}

type imageRetryReply struct {
	status int
	body   string
	mode   string
}

type imageRetryFixture struct {
	replies                 []imageRetryReply
	edits, cached, specific bool
	maxAttempts             int
}

func runImageRetryRelay(t *testing.T, f imageRetryFixture) (*gin.Context, *httptest.ResponseRecorder, []int) {
	t.Helper()
	setupCPAContractChannels(t, false)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}))
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree, oldGroup := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume, ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroup))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 5, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0}`))
	if f.maxAttempts == 0 {
		f.maxAttempts = 6
	}
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "3")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", fmt.Sprint(f.maxAttempts))
	t.Setenv("RELAY_RETRY_ROUND_BACKOFF_MS", "0")
	t.Setenv("RELAY_RETRY_MAX_ROUND_BACKOFF_MS", "0")
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var mu sync.Mutex
	var calls []int
	var first model.Channel
	const name = "gpt-image-2.5-sunburst"
	for index, reply := range f.replies {
		id := 9 + index
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, id)
			attempts++
			attempt := attempts
			mu.Unlock()
			if f.edits {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse replayed image form: %v", err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				for key, value := range map[string]string{"model": name, "prompt": "synthetic prompt", "size": "1024x1024", "quality": "high", "n": "1"} {
					if r.FormValue(key) != value {
						t.Errorf("field %s changed: %q", key, r.FormValue(key))
					}
				}
				file, _, err := r.FormFile("image")
				if err != nil {
					t.Errorf("missing replayed image: %v", err)
					return
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || string(data) != "synthetic image bytes" {
					t.Errorf("image bytes changed: %q (%v)", data, err)
				}
			} else {
				var request dto.ImageRequest
				if err := common.DecodeJson(r.Body, &request); err != nil {
					t.Errorf("decode image request: %v", err)
					return
				}
				if request.Model != name || request.Prompt != "synthetic prompt" || request.Size != "1024x1024" || request.Quality != "high" || request.N == nil || *request.N != 1 {
					t.Errorf("image generation parameters changed: %+v", request)
				}
			}
			if reply.mode == "disconnect" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("synthetic disconnect: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			if reply.mode == "cancel" {
				cancel()
			}
			w.Header().Set("Content-Type", "application/json")
			if reply.mode == "stream" {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			if reply.mode == "recover" && attempt > 1 {
				_, _ = io.WriteString(w, `{"data":[{"b64_json":"c3ludGhldGlj"}]}`)
				return
			}
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		}))
		t.Cleanup(server.Close)
		priority := int64(100 - index)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Group: "default", Models: name, Priority: &priority, BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: "default", Model: name, ChannelId: id, Enabled: true, Priority: &priority}).Error)
		if index == 0 {
			first = channel
		}
	}
	common.MemoryCacheEnabled = f.cached
	if f.cached {
		model.InitChannelCache()
	}
	path := "/v1/images/generations"
	body := []byte(`{"model":"gpt-image-2.5-sunburst","prompt":"synthetic prompt","size":"1024x1024","quality":"high","n":1}`)
	contentType := "application/json"
	if f.edits {
		path = "/v1/images/edits"
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		for key, value := range map[string]string{"model": name, "prompt": "synthetic prompt", "size": "1024x1024", "quality": "high", "n": "1"} {
			require.NoError(t, writer.WriteField(key, value))
		}
		part, err := writer.CreateFormFile("image", "image.png")
		require.NoError(t, err)
		_, err = io.WriteString(part, "synthetic image bytes")
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		body, contentType = buf.Bytes(), writer.FormDataContentType()
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", contentType)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	if f.specific {
		c.Set("specific_channel_id", first.Id)
	}
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &first, name))
	Relay(c, types.RelayFormatOpenAIImage)
	mu.Lock()
	defer mu.Unlock()
	return c, recorder, append([]int(nil), calls...)
}

func TestImageRetryTraversesDistinctChannelsWithoutChangingRequest(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, edits := range []bool{false, true} {
			t.Run(fmt.Sprintf("cache=%t/edits=%t", cached, edits), func(t *testing.T) {
				c, response, calls := runImageRetryRelay(t, imageRetryFixture{cached: cached, edits: edits, replies: []imageRetryReply{
					{401, `{"error":{"message":"Invalid token","code":"invalid_api_key"}}`, ""},
					{400, `{"error":{"type":"invalid_request_error","code":"request_failed","message":"This may be due to a content policy violation, invalid parameters, or an unknown issue."}}`, ""},
					{200, `{"created":1,"data":[{"b64_json":"c3ludGhldGlj"}]}`, ""},
				}})
				require.Equal(t, []int{9, 10, 11}, calls)
				require.Equal(t, 200, response.Code, response.Body.String())
				require.Equal(t, service.RetryStopReasonSuccess, c.GetString("retry_stop_reason"))
			})
		}
	}
}

func TestImageRetryPreservesSafetyAndBudgetBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, stop  string
		first       imageRetryReply
		specific    bool
		maxAttempts int
	}{
		{"content policy", service.RetryStopReasonNotRetryable, imageRetryReply{400, `{"error":{"code":"content_policy_violation","message":"blocked"}}`, ""}, false, 0},
		{"invalid parameter", service.RetryStopReasonNotRetryable, imageRetryReply{400, `{"error":{"code":"invalid_value","param":"size","message":"bad size"}}`, ""}, false, 0},
		{"unknown 400", service.RetryStopReasonNotRetryable, imageRetryReply{400, `{"error":{"message":"unknown rejection"}}`, ""}, false, 0},
		{"accepted connection lost", service.RetryStopReasonUpstreamUnknown, imageRetryReply{0, "", "disconnect"}, false, 0},
		{"accepted malformed success", service.RetryStopReasonUpstreamUnknown, imageRetryReply{200, `{"data":`, ""}, false, 0},
		{"explicit route", service.RetryStopReasonNotRetryable, imageRetryReply{400, `{"error":{"code":"request_failed","message":"synthetic rejected request"}}`, ""}, true, 0},
		{"attempt limit", service.RetryStopReasonAttemptsExhausted, imageRetryReply{400, `{"error":{"code":"request_failed","message":"synthetic rejected request"}}`, ""}, false, 1},
		{"client cancellation", service.RetryStopReasonClientGone, imageRetryReply{503, `{"error":{"code":"server_error","message":"synthetic upstream failure"}}`, "cancel"}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, calls := runImageRetryRelay(t, imageRetryFixture{specific: tc.specific, maxAttempts: tc.maxAttempts, replies: []imageRetryReply{
				tc.first, {200, `{"data":[{"b64_json":"c3ludGhldGlj"}]}`, ""},
			}})
			require.Equal(t, []int{9}, calls)
			require.Equal(t, tc.stop, c.GetString("retry_stop_reason"))
		})
	}
}

func TestImageHardRejectionsRemainExcludedAcrossRounds(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			err := types.WithOpenAIError(types.OpenAIError{Code: "request_failed"}, status, types.ErrOptionWithUpstreamResponse())
			param := &service.RetryParam{}
			excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAIImage, LastError: err}, 9)
			if status == 429 || status == 503 {
				require.Empty(t, param.PersistentExcludedIds)
			} else {
				require.Equal(t, []int{9}, param.PersistentExcludedIds)
			}
		})
	}
	_, response, calls := runImageRetryRelay(t, imageRetryFixture{replies: []imageRetryReply{
		{401, `{"error":{"code":"invalid_api_key","message":"synthetic invalid key"}}`, ""},
		{400, `{"error":{"code":"request_failed","message":"synthetic rejected request"}}`, ""},
	}})
	require.Equal(t, []int{9, 10}, calls, "round reset must not retry rejected image entrances")
	require.Equal(t, 400, response.Code)
	require.True(t, strings.Contains(response.Body.String(), "request_failed"))
}

func TestImageTransientFailureRecoversWithoutRepeatingInvalidKey(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			c, response, calls := runImageRetryRelay(t, imageRetryFixture{cached: cached, replies: []imageRetryReply{
				{503, `{"error":{"code":"server_error","message":"synthetic overload"}}`, "recover"},
				{401, `{"error":{"code":"invalid_api_key","message":"synthetic invalid key"}}`, ""},
			}})
			require.Equal(t, []int{9, 10, 9}, calls)
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Equal(t, 2, c.GetInt("retry_round_no"))
		})
	}
}

func TestImagePartialOutputNeverRegenerates(t *testing.T) {
	c, response, calls := runImageRetryRelay(t, imageRetryFixture{replies: []imageRetryReply{
		{200, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"synthetic preview\"}\n\nevent: error\ndata: {\"code\":\"request_failed\",\"message\":\"synthetic interrupted generation\"}\n\n", "stream"},
		{200, `{"data":[{"b64_json":"c3ludGhldGlj"}]}`, ""},
	}})
	require.Equal(t, []int{9}, calls)
	require.NotEqual(t, service.RetryStopReasonSuccess, c.GetString("retry_stop_reason"))
	require.NotContains(t, response.Body.String(), "c3ludGhldGlj")
}
