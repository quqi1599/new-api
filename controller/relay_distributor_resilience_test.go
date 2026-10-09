package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

var distributorResilienceIdentity atomic.Int64

type distributorResilienceFixture struct {
	cached        bool
	candidates    []string
	groups        []string
	forced        bool
	modelDenied   bool
	autoGroup     bool
	canceled      bool
	expired       bool
	affinityCheck bool
}

func runDistributorResilience(t *testing.T, fixture distributorResilienceFixture) streamResilienceResult {
	t.Helper()
	require.NoError(t, i18n.Init())
	setupCPAContractChannels(t, true)
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.Token{}, &model.TokenProtectedChannelBan{}))
	require.NoError(t, model.DB.Where("1 = 1").Delete(&model.Ability{}).Error)
	require.NoError(t, model.DB.Where("1 = 1").Delete(&model.Channel{}).Error)
	oldRetry, oldConsume, oldError := common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled
	oldFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	oldRatios, oldGroups, oldAuto := ratio_setting.GroupRatio2JSONString(), setting.UserUsableGroups2JSONString(), setting.AutoGroups2JsonString()
	t.Cleanup(func() {
		common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = oldRetry, oldConsume, oldError
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFree
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatios))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(oldGroups))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(oldAuto))
	})
	common.RetryTimes, common.LogConsumeEnabled, constant.ErrorLogEnabled = 2, false, false
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0,"backup":0}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"default","backup":"backup","auto":"auto"}`))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["default","backup"]`))
	t.Setenv("RELAY_RETRY_MAX_ROUNDS", "1")
	t.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "2")
	service.InitHttpClient()
	circuit := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: circuit.Addr()})
	oldRedis, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldRedis, oldClient; _ = client.Close() })

	userID := 980000 + int(distributorResilienceIdentity.Add(1))
	user := model.User{Id: userID, Username: fmt.Sprintf("route%d", userID), Status: common.UserStatusEnabled, Group: "default", Role: common.RoleAdminUser, Setting: `{"accept_unset_model_ratio_model":true}`}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{Id: userID, UserId: userID, Key: fmt.Sprintf("syntheticInitialRoute%d", userID), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	if fixture.modelDenied {
		token.ModelLimitsEnabled, token.ModelLimits = true, "other-model"
	}
	if fixture.autoGroup {
		token.Group, token.CrossGroupRetry = "auto", true
	}
	require.NoError(t, model.DB.Create(&token).Error)

	var mu sync.Mutex
	var calls []int
	var affinityPhase atomic.Int32
	if fixture.affinityCheck {
		settings := operation_setting.GetChannelAffinitySetting()
		old := *settings
		*settings = operation_setting.ChannelAffinitySetting{Enabled: true, SwitchOnSuccess: true, MaxEntries: 100, DefaultTTLSeconds: 60,
			Rules: []operation_setting.ChannelAffinityRule{{Name: "synthetic-affinity", ModelRegex: []string{"^gpt-"}, PathRegex: []string{"/v1/responses"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "header", Key: "X-Synthetic-Affinity"}}, TTLSeconds: 60, IncludeRuleName: true}}}
		t.Cleanup(func() { *settings = old })
	}
	for index, kind := range fixture.candidates {
		id := 9 + index
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, id)
			mu.Unlock()
			if fixture.affinityCheck && affinityPhase.Load() >= 4 && id == 9 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"error":{"code":"server_error","message":"synthetic pre-output affinity failure"}}`)
				return
			}
			if fixture.affinityCheck && affinityPhase.Load() == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed_affinity\",\"status\":\"in_progress\"}}\n\n")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed_affinity\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"synthetic terminal failure\"}}}\n\n")
				return
			}
			if fixture.affinityCheck && affinityPhase.Load() == 3 {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete_affinity\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				return
			}
			if kind == "error" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"error":{"code":"server_error","message":"synthetic explicit rejection"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_initial_healthy\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}))
		t.Cleanup(server.Close)
		group := "default"
		if index < len(fixture.groups) {
			group = fixture.groups[index]
		}
		priority := int64(200 - index)
		channel := model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: group,
			Models: "gpt-5.6-sol", Priority: &priority, BaseURL: &server.URL, Key: "synthetic-key", AutoBan: common.GetPointer(0)}
		if kind == "empty" {
			channel.ChannelInfo.IsMultiKey = true
			channel.ChannelInfo.MultiKeyStatusList = map[int]int{0: common.ChannelStatusManuallyDisabled}
		}
		require.NoError(t, model.DB.Save(&channel).Error)
		require.NoError(t, model.DB.Save(&model.Ability{Group: group, Model: "gpt-5.6-sol", ChannelId: id, Enabled: true, Priority: &priority}).Error)
	}
	common.MemoryCacheEnabled = fixture.cached
	if fixture.cached {
		model.InitChannelCache()
	}
	var captured *gin.Context
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.TokenAuth(), func(c *gin.Context) {
		captured = c
		if fixture.expired {
			c.Set("relay_retry_started_at", time.Now().Add(-time.Hour))
		}
		c.Next()
	}, middleware.Distribute())
	router.POST("/v1/responses", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"synthetic","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	key := "sk-" + token.Key
	if fixture.forced {
		key += "-9"
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if fixture.affinityCheck {
		req.Header.Set("X-Synthetic-Affinity", fmt.Sprintf("fixture-%d", userID))
	}
	if fixture.canceled {
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		req = req.WithContext(ctx)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	require.NotNil(t, captured, "the real auth middleware must accept the fixture token")
	if fixture.affinityCheck {
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		cacheKey := captured.GetString("channel_affinity_cache_key")
		require.NotEmpty(t, cacheKey)
		require.Equal(t, time.Minute, circuit.TTL(cacheKey))
		binding, err := circuit.Get(cacheKey)
		require.NoError(t, err)
		circuit.FastForward(20 * time.Second)
		for phase := int32(1); phase <= 5; phase++ {
			operation_setting.GetChannelAffinitySetting().Rules[0].SkipRetryOnFailure = phase == 4
			affinityPhase.Store(phase)
			next := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"synthetic","stream":true}`))
			next.Header = req.Header.Clone()
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, next)
			if phase == 4 {
				require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "explicit affinity stop must override GPT forced fallback")
				require.Equal(t, service.RetryStopReasonNotRetryable, captured.GetString("retry_stop_reason"))
			} else {
				require.Equal(t, http.StatusOK, recorder.Code)
			}
			affinity, found := captured.Get("channel_affinity_log_info")
			require.True(t, found, "the second request must hit the established affinity")
			require.Equal(t, 9, affinity.(map[string]interface{})["channel_id"])
			if phase == 1 {
				require.Contains(t, recorder.Body.String(), `"type":"response.failed"`)
				require.Equal(t, "failed", captured.GetString("relay_final_outcome"))
				require.Equal(t, 40*time.Second, circuit.TTL(cacheKey), "HTTP 200 with an explicit failed terminal must not refresh affinity")
			} else if phase == 4 {
				require.Equal(t, 50*time.Second, circuit.TTL(cacheKey))
			} else {
				require.Equal(t, time.Minute, circuit.TTL(cacheKey), "completed and existing legitimate incomplete behavior must keep affinity")
				if phase == 2 {
					require.Equal(t, "completed", captured.GetString("relay_final_outcome"))
				}
				if phase == 3 {
					require.Equal(t, "incomplete", captured.GetString("relay_final_outcome"))
				}
			}
			currentBinding, err := circuit.Get(cacheKey)
			require.NoError(t, err)
			if phase == 5 {
				require.NotEqual(t, binding, currentBinding, "ordinary affinity still permits an explicitly allowed healthy fallback")
			} else {
				require.Equal(t, binding, currentBinding, "failure must neither remove nor migrate the existing binding")
			}
			if phase == 2 || phase == 3 {
				circuit.FastForward(10 * time.Second)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return streamResilienceResult{captured, recorder, append([]int(nil), calls...)}
}

func TestDistributorResilienceDoesNotRenewAffinityAfterCommittedStreamFailure(t *testing.T) {
	got := runDistributorResilience(t, distributorResilienceFixture{candidates: []string{"healthy", "healthy"}, affinityCheck: true})
	require.Equal(t, []int{9, 9, 9, 9, 9, 9, 10}, got.calls)
}

func TestDistributorResilienceForcedChannelNeverUsesAnotherEntrance(t *testing.T) {
	got := runDistributorResilience(t, distributorResilienceFixture{candidates: []string{"error", "healthy"}, forced: true})
	require.Equal(t, []int{9}, got.calls)
	require.Equal(t, http.StatusServiceUnavailable, got.response.Code)
}

func TestDistributorResilienceSharesCandidateCapAcrossInitialAndRelaySelection(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			candidates := make([]string, 31)
			for i := range candidates {
				candidates[i] = "empty"
			}
			candidates = append(candidates, "error", "empty", "empty", "healthy")
			got := runDistributorResilience(t, distributorResilienceFixture{cached: cached, candidates: candidates})
			require.Equal(t, []int{40}, got.calls)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
			require.Equal(t, 32, got.context.GetInt("retry_candidate_skip_count"))
			require.Equal(t, service.RetryStopReasonCandidateLimit, got.context.GetString("retry_stop_reason"))
		})
	}
}

func TestDistributorResilienceInitialEmptyCandidateUsesHealthyEntrance(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, candidates := range [][]string{{"empty", "healthy"}, {"empty", "empty", "healthy"}, {"empty", "error", "empty", "healthy"}} {
			t.Run(fmt.Sprintf("cache=%t/%v", cached, candidates), func(t *testing.T) {
				got := runDistributorResilience(t, distributorResilienceFixture{cached: cached, candidates: candidates})
				want := []int{8 + len(candidates)}
				if len(candidates) == 4 {
					want = []int{10, 12}
				}
				require.Equal(t, want, got.calls, got.response.Body.String())
				require.Equal(t, http.StatusOK, got.response.Code)
				require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
				require.Equal(t, len(want), got.context.GetInt("retry_attempt_no"))
				require.Equal(t, len(candidates)-len(want), got.context.GetInt("retry_candidate_skip_count"))
			})
		}
	}
}

func TestDistributorResilienceKeepsBoundsAndAuthorization(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			fixture distributorResilienceFixture
			status  int
		}{
			{"forced empty channel", distributorResilienceFixture{forced: true}, http.StatusInternalServerError},
			{"token model denied", distributorResilienceFixture{modelDenied: true}, http.StatusForbidden},
			{"different group denied", distributorResilienceFixture{groups: []string{"default", "forbidden"}}, http.StatusServiceUnavailable},
			{"canceled", distributorResilienceFixture{canceled: true}, 499},
			{"elapsed budget", distributorResilienceFixture{expired: true}, http.StatusGatewayTimeout},
		} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, tc.name), func(t *testing.T) {
				tc.fixture.cached, tc.fixture.candidates = cached, []string{"empty", "healthy"}
				got := runDistributorResilience(t, tc.fixture)
				require.Empty(t, got.calls)
				require.Equal(t, tc.status, got.response.Code, got.response.Body.String())
			})
		}
		t.Run(fmt.Sprintf("cache=%t/auto group", cached), func(t *testing.T) {
			got := runDistributorResilience(t, distributorResilienceFixture{cached: cached, candidates: []string{"empty", "healthy"}, groups: []string{"default", "backup"}, autoGroup: true})
			require.Equal(t, []int{10}, got.calls, got.response.Body.String())
			require.Equal(t, "backup", common.GetContextKeyString(got.context, constant.ContextKeyAutoGroup))
		})
		t.Run(fmt.Sprintf("cache=%t/all empty bounded", cached), func(t *testing.T) {
			candidates := make([]string, 34)
			for i := range candidates {
				candidates[i] = "empty"
			}
			got := runDistributorResilience(t, distributorResilienceFixture{cached: cached, candidates: candidates})
			require.Empty(t, got.calls)
			require.Equal(t, 32, got.context.GetInt("retry_candidate_skip_count"))
			require.Equal(t, service.RetryStopReasonCandidateLimit, got.context.GetString("retry_stop_reason"))
		})
	}
}
