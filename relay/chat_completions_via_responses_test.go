package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const convertedResponsesText = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
const convertedResponsesCompleted = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12,\"input_tokens_details\":{\"cached_tokens\":8}}}}\n\n"
const convertedResponsesFailed = "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"code\":\"fixture_failed\",\"message\":\"synthetic failure\"}}}\n\n"

type convertedResponsesCase struct {
	name        string
	body        string
	allowWrites int // -1 permits all writes
	wantStatus  int
	wantPartial bool
	wantUsage   bool
	wantCache   int
}

func convertedResponsesCases() []convertedResponsesCase {
	return []convertedResponsesCase{
		{name: "completed", body: convertedResponsesText + convertedResponsesCompleted, allowWrites: -1, wantUsage: true, wantCache: 8},
		{name: "pre_output_eof", allowWrites: -1, wantStatus: http.StatusBadGateway},
		{name: "explicit_failure", body: convertedResponsesText + convertedResponsesFailed, allowWrites: -1, wantStatus: http.StatusInternalServerError},
		{name: "partial_eof", body: convertedResponsesText, allowWrites: -1, wantStatus: http.StatusGatewayTimeout, wantPartial: true, wantUsage: true},
		// The completed event's usage arrives before its stop chunk is written.
		{name: "stop_delivery_failure", body: convertedResponsesText + convertedResponsesCompleted, allowWrites: 2, wantStatus: 499, wantPartial: true, wantUsage: true, wantCache: 8},
	}
}

type convertedResponsesWriter struct {
	*httptest.ResponseRecorder
	writes      int
	allowWrites int
}

func (w *convertedResponsesWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.allowWrites >= 0 && w.writes > w.allowWrites {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func newConvertedResponsesFixture(t *testing.T, tc convertedResponsesCase) (*gin.Context, *relaycommon.RelayInfo, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request did not use the Responses conversion: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(tc.body))
	}))
	t.Cleanup(server.Close)
	w := &convertedResponsesWriter{ResponseRecorder: httptest.NewRecorder(), allowWrites: tc.allowWrites}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-5.6-sol")
	// Verify status remapping mutates, rather than replaces, the marked error.
	if tc.name == "partial_eof" {
		c.Set("status_code_mapping", `{"502":"504"}`)
	}
	info := &relaycommon.RelayInfo{
		IsStream: true, OriginModelName: "gpt-5.6-sol", RelayFormat: types.RelayFormatOpenAI,
		RelayMode: relayconstant.RelayModeChatCompletions, RequestURLPath: "/v1/chat/completions", StartTime: time.Now(),
		Request: &dto.GeneralOpenAIRequest{Model: "gpt-5.6-sol", Stream: common.GetPointer(true), Messages: []dto.Message{{Role: "user", Content: "synthetic test"}}},
	}
	info.InitChannelMeta(c)
	info.SetEstimatePromptTokens(10)
	return c, info, calls
}

func assertConvertedResponsesError(t *testing.T, tc convertedResponsesCase, c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, apiErr *types.NewAPIError) {
	t.Helper()
	if tc.wantStatus == 0 {
		require.Nil(t, apiErr)
	} else {
		require.NotNil(t, apiErr)
		require.Equal(t, tc.wantStatus, apiErr.StatusCode)
		require.True(t, types.IsSkipRetryError(apiErr))
	}
	if tc.wantPartial {
		require.Same(t, info.PartialStreamError, apiErr)
		require.True(t, canSettlePartialStreamUsage(c, info, usage, apiErr))
	} else {
		require.Nil(t, info.PartialStreamError)
		require.False(t, canSettlePartialStreamUsage(c, info, usage, apiErr))
	}
	if tc.wantUsage {
		require.NotNil(t, usage)
		require.Equal(t, 10, usage.PromptTokens)
		require.Positive(t, usage.CompletionTokens)
		require.Equal(t, tc.wantCache, usage.PromptTokensDetails.CachedTokens)
	} else {
		require.Nil(t, usage)
	}
}

func TestChatCompletionsViaResponsesPreservesUsageAndFailure(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range convertedResponsesCases() {
		t.Run(tc.name, func(t *testing.T) {
			c, info, calls := newConvertedResponsesFixture(t, tc)
			adaptor := GetAdaptor(constant.APITypeOpenAI)
			adaptor.Init(info)
			usage, apiErr := chatCompletionsViaResponses(c, info, adaptor, info.Request.(*dto.GeneralOpenAIRequest))
			assertConvertedResponsesError(t, tc, c, info, usage, apiErr)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, relayconstant.RelayModeChatCompletions, info.RelayMode)
			require.Equal(t, "/v1/chat/completions", info.RequestURLPath)
		})
	}
}

// Exercise the public TextHelper route, real BillingSession, wallet/token rows,
// and consumption log. The controller's error defer is replayed via Refund:
// partial settlement must survive it, while unmarked failures still refund.
func TestConvertedResponsesBillingSettlesOnceOrRefunds(t *testing.T) {
	service.InitHttpClient()
	settings := model_setting.GetGlobalSettings()
	oldSettings := *settings
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
	t.Cleanup(func() { *settings = oldSettings })
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldBatch, oldLog, oldExport := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, false, true, false
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "converted-responses.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = oldRedis, oldBatch, oldLog, oldExport
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
	const initialQuota = 1000000
	for _, tc := range convertedResponsesCases() {
		t.Run(tc.name, func(t *testing.T) {
			c, info, calls := newConvertedResponsesFixture(t, tc)
			user := &model.User{Username: "converted-" + tc.name, AffCode: tc.name, Status: common.UserStatusEnabled, Quota: initialQuota}
			require.NoError(t, db.Create(user).Error)
			token := &model.Token{UserId: user.Id, Name: "synthetic", Key: "converted-" + tc.name, Status: common.TokenStatusEnabled, RemainQuota: initialQuota, ExpiredTime: -1}
			require.NoError(t, db.Create(token).Error)
			channel := &model.Channel{Name: "synthetic", Type: constant.ChannelTypeOpenAI}
			require.NoError(t, db.Create(channel).Error)
			common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
			info.UserId, info.TokenId, info.TokenKey = user.Id, token.Id, token.Key
			info.UserSetting = dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1}
			info.PriceData = types.PriceData{ModelRatio: 1, CompletionRatio: 1, CacheRatio: 0.25, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
			require.Nil(t, service.PreConsumeBilling(c, 100, info))
			apiErr := TextHelper(c, info)
			require.EqualValues(t, 1, calls.Load())
			if tc.wantStatus == 0 {
				require.Nil(t, apiErr)
			} else {
				require.NotNil(t, apiErr)
				require.Equal(t, tc.wantStatus, apiErr.StatusCode)
				require.True(t, types.IsSkipRetryError(apiErr))
			}
			if tc.wantPartial {
				require.Same(t, info.PartialStreamError, apiErr)
			}
			if tc.wantUsage {
				require.False(t, info.Billing.NeedsRefund())
			} else {
				require.True(t, info.Billing.NeedsRefund())
			}
			info.Billing.Refund(c)
			info.Billing.Refund(c)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Find(&logs).Error)
			wantQuota := 0
			if tc.wantUsage {
				require.Len(t, logs, 1)
				wantQuota = logs[0].Quota
				require.Positive(t, wantQuota)
				var logInfo struct {
					StreamStatus struct {
						Status string `json:"status"`
					} `json:"stream_status"`
				}
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &logInfo))
				wantStreamStatus := "ok"
				if tc.wantPartial {
					wantStreamStatus = "error"
				}
				require.Equal(t, wantStreamStatus, logInfo.StreamStatus.Status)
				if tc.wantCache > 0 {
					require.Equal(t, 6, wantQuota, "10 input - 8 cached + 8*0.25 + 2 output")
				}
				require.NoError(t, info.Billing.Settle(wantQuota), "duplicate settlement must be idempotent")
			} else {
				require.Empty(t, logs)
			}
			require.Eventually(t, func() bool {
				var persistedUser model.User
				var persistedToken model.Token
				if db.First(&persistedUser, user.Id).Error != nil || db.First(&persistedToken, token.Id).Error != nil {
					return false
				}
				return persistedUser.Quota == initialQuota-wantQuota && persistedUser.UsedQuota == wantQuota &&
					persistedToken.RemainQuota == initialQuota-wantQuota && persistedToken.UsedQuota == wantQuota
			}, time.Second, time.Millisecond)
		})
	}
}
