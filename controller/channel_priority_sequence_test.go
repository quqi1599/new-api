package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
)

func TestRelayPrioritySequenceUsesFailedExclusionsNotAttemptIndex(t *testing.T) {
	for _, cache := range []bool{false, true} {
		for _, priorities := range [][]int64{{100, 90, 80}, {100, 100, 90}} {
			t.Run(fmt.Sprintf("cache=%v/tiers=%v", cache, priorities), func(t *testing.T) {
				c := setupPrioritySequenceChannels(t, cache, priorities)
				param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true}
				state := newCPAContractRetryState()
				upstreamErr := parseCPAContractError(t, http.StatusBadGateway, `{"error":{"message":"temporary failure","code":"server_error"}}`)
				var selected []int
				for index, priority := range priorities {
					if !state.CanAttempt(c.Request.Context()) {
						t.Fatal("fixture exhausted its attempt budget early")
					}
					channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
					if err != nil || channel == nil || channel.GetPriority() != priority || slices.Contains(selected, channel.Id) {
						t.Fatalf("attempt=%d retry=%d selection=%v err=%v want priority=%d selected=%v", index+1, param.GetRetry(), channel, err, priority, selected)
					}
					state.RecordAttempt(channel.Id)
					selected = append(selected, channel.Id)
					// Use the same exclusion and counter mutations as Relay after an
					// explicit retryable failure, not hand-picked retry arguments.
					excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: upstreamErr}, channel.Id)
					param.IncreaseRetry()
				}
				channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel != nil || param.GetRetry() != len(priorities) || state.Attempts != len(priorities) {
					t.Fatalf("exhaustion lost counters: channel=%v err=%v retry=%d attempts=%d", channel, err, param.GetRetry(), state.Attempts)
				}
				if !canStartNextRelayRetryRound(c.Request.Context(), state, true, false, upstreamErr, param) {
					t.Fatal("temporary exclusions must allow the existing bounded next round")
				}
				state.StartNextRound()
				param.ExcludedChannelIds = nil
				param.SetRetry(0)
				channel, _, err = service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel == nil || channel.GetPriority() != priorities[0] || state.Attempts != len(priorities) {
					t.Fatalf("round reset selection=%v err=%v attempts=%d", channel, err, state.Attempts)
				}
			})
		}
	}
}

func TestRelayPrioritySequenceKeepsPersistentAndTokenExclusions(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%v", cache), func(t *testing.T) {
			c := setupPrioritySequenceChannels(t, cache, []int64{100, 90, 80, 70})
			common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, []int{904})
			param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true}
			authErr := parseCPAContractError(t, http.StatusServiceUnavailable, `{"error":{"message":"no eligible credentials","code":"auth_unavailable"}}`)
			transientErr := parseCPAContractError(t, http.StatusBadGateway, `{"error":{"message":"temporary failure","code":"server_error"}}`)
			for _, wantID := range []int{901, 902, 903} {
				channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel == nil || channel.Id != wantID {
					t.Fatalf("selection=%v err=%v want=%d", channel, err, wantID)
				}
				lastErr := transientErr
				if wantID == 901 {
					lastErr = authErr
				}
				excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: lastErr}, channel.Id)
				param.IncreaseRetry()
			}
			param.ExcludedChannelIds = nil
			param.SetRetry(0)
			for _, wantID := range []int{902, 903} {
				channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel == nil || channel.Id != wantID {
					t.Fatalf("cross-round permanent/token exclusion selection=%v err=%v want=%d", channel, err, wantID)
				}
				excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: transientErr}, channel.Id)
				param.IncreaseRetry()
			}
			channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
			if err != nil || channel != nil || !slices.Equal(param.PersistentExcludedIds, []int{901}) {
				t.Fatalf("permanent exclusions revived: selection=%v err=%v persistent=%v", channel, err, param.PersistentExcludedIds)
			}
		})
	}
}

func TestRelayPrioritySequenceNativeFallbackAndAutoGroups(t *testing.T) {
	for _, cache := range []bool{false, true} {
		for _, scenario := range []string{"native fallback", "auto groups"} {
			t.Run(fmt.Sprintf("cache=%v/%s", cache, scenario), func(t *testing.T) {
				c := setupPrioritySequenceChannels(t, cache, []int64{10, 1, 100, 90})
				param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true,
					PreferredChannelTypes: []int{constant.ChannelTypeOpenAI}}
				if err := model.DB.Model(&model.Channel{}).Where("id IN ?", []int{903, 904}).Update("type", constant.ChannelTypeAzure).Error; err != nil {
					t.Fatal(err)
				}
				if scenario == "auto groups" {
					oldAuto, oldGroups, oldRetries := setting.AutoGroups2JsonString(), setting.UserUsableGroups2JSONString(), common.RetryTimes
					t.Cleanup(func() {
						_ = setting.UpdateAutoGroupsByJsonString(oldAuto)
						_ = setting.UpdateUserUsableGroupsByJSONString(oldGroups)
						common.RetryTimes = oldRetries
					})
					if err := setting.UpdateAutoGroupsByJsonString(`["default","vip"]`); err != nil {
						t.Fatal(err)
					}
					if err := setting.UpdateUserUsableGroupsByJSONString(`{"default":"default","vip":"vip"}`); err != nil {
						t.Fatal(err)
					}
					if err := model.DB.Model(&model.Channel{}).Where("id = ?", 904).Update("group", "vip").Error; err != nil {
						t.Fatal(err)
					}
					if err := model.DB.Model(&model.Ability{}).Where("channel_id = ?", 904).Update("group", "vip").Error; err != nil {
						t.Fatal(err)
					}
					common.RetryTimes = 2
					common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, true)
					param.TokenGroup = "auto"
				}
				model.InitChannelCache()
				upstreamErr := parseCPAContractError(t, http.StatusBadGateway, `{"error":{"message":"temporary failure","code":"server_error"}}`)
				for _, wantID := range []int{901, 902, 903, 904} {
					channel, group, err := service.CacheGetRandomSatisfiedChannel(param)
					if err != nil || channel == nil || channel.Id != wantID {
						t.Fatalf("retry=%d selection=%v err=%v want=%d", param.GetRetry(), channel, err, wantID)
					}
					wantGroup := "default"
					if scenario == "auto groups" && wantID == 904 {
						wantGroup = "vip"
					}
					if group != wantGroup {
						t.Fatalf("selected group=%s want=%s", group, wantGroup)
					}
					excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: upstreamErr}, channel.Id)
					param.IncreaseRetry()
				}
			})
		}
	}
}

func TestRelayPrioritySequenceBoundsAttemptsAndPreservesTaskTierMode(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%v", cache), func(t *testing.T) {
			c := setupPrioritySequenceChannels(t, cache, []int64{100, 100, 90, 80})
			param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", ExhaustCandidates: true}
			state := newCPAContractRetryState()
			state.Policy.MaxAttempts = 2
			upstreamErr := parseCPAContractError(t, http.StatusBadGateway, `{"error":{"message":"temporary failure","code":"server_error"}}`)
			for state.CanAttempt(c.Request.Context()) {
				channel, _, err := service.CacheGetRandomSatisfiedChannel(param)
				if err != nil || channel == nil {
					t.Fatalf("selection=%v err=%v", channel, err)
				}
				state.RecordAttempt(channel.Id)
				excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: upstreamErr}, channel.Id)
				param.IncreaseRetry()
			}
			if state.Attempts != 2 || param.GetRetry() != 2 || state.StopReason != service.RetryStopReasonAttemptsExhausted || state.CanStartNextRound(c.Request.Context()) {
				t.Fatalf("candidate exhaustion bypassed request budget: state=%#v retry=%d", state, param.GetRetry())
			}
			// Task retries do not record failed-channel exclusions. Even with a
			// hard token exclusion, they must retain the legacy tier index instead
			// of repeatedly selecting the highest remaining tier.
			common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, []int{901})
			taskParam := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol"}
			for _, priority := range []int64{100, 90, 80} {
				channel, _, err := service.CacheGetRandomSatisfiedChannel(taskParam)
				if err != nil || channel == nil || channel.GetPriority() != priority {
					t.Fatalf("Task priority mode changed: selection=%v err=%v want=%d", channel, err, priority)
				}
				taskParam.IncreaseRetry()
			}
		})
	}
}

func setupPrioritySequenceChannels(t *testing.T, cache bool, priorities []int64) *gin.Context {
	t.Helper()
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldCache, oldRedis := common.MemoryCacheEnabled, common.RedisEnabled
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.MemoryCacheEnabled, common.RedisEnabled = oldCache, oldRedis
		common.SetMainDatabaseType(oldMainType)
		common.SetLogDatabaseType(oldLogType)
		if oldCache && oldDB != nil {
			model.InitChannelCache()
		}
	})
	db := setupModelListControllerTestDB(t)
	for i, priority := range priorities {
		channel := model.Channel{Id: 901 + i, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Group: "default", Models: "gpt-5.6-sol", Priority: common.GetPointer(priority), Weight: common.GetPointer(uint(1))}
		if err := db.Create(&channel).Error; err != nil {
			t.Fatal(err)
		}
		if err := channel.AddAbilities(nil); err != nil {
			t.Fatal(err)
		}
	}
	common.MemoryCacheEnabled = cache
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}
