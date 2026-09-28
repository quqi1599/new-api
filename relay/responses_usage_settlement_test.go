package relay

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Exercise the real raw Responses route and BillingSession, not just the usage
// estimator. All accounts and logs live in a temporary SQLite database.
func TestResponsesMissingUsageSettlement(t *testing.T) {
	service.InitHttpClient()
	service.InitTokenEncoders()
	settings := model_setting.GetGlobalSettings()
	oldPassThrough := settings.PassThroughRequestEnabled
	settings.PassThroughRequestEnabled = false
	t.Cleanup(func() { settings.PassThroughRequestEnabled = oldPassThrough })
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldBatch, oldLog, oldExport := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, false, true, false
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "responses-usage.db")), &gorm.Config{})
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
	const toolDelta = "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"hello world\"}\n\n"
	for _, tc := range []struct {
		name, body                     string
		writes, status                 int
		wantLog, wantCharge, estimated bool
	}{
		{"tool_eof", toolDelta, -1, http.StatusBadGateway, true, true, true},
		{"tool_completed", toolDelta + "data: {\"type\":\"response.completed\"}\n\n", -1, 0, true, true, true},
		{"downstream_disconnect", "data: {\"type\":\"response.created\"}\n\n" + toolDelta, 1, 499, true, true, true},
		{"failed_refund", toolDelta + convertedResponsesFailed, -1, http.StatusBadGateway, false, false, false},
		{"metadata_only", "data: {\"type\":\"response.created\"}\n\n", -1, http.StatusBadGateway, true, false, false},
		{"upstream_usage", toolDelta + convertedResponsesCompleted, -1, 0, true, true, false},
		{"explicit_zero", toolDelta + "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", -1, 0, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info, calls := newConvertedResponsesFixture(t, convertedResponsesCase{name: tc.name, body: tc.body, allowWrites: tc.writes})
			c.Request.URL.Path = "/v1/responses"
			info.RequestURLPath = "/v1/responses"
			info.RelayFormat = types.RelayFormatOpenAIResponses
			info.RelayMode = relayconstant.RelayModeResponses
			info.Request = &dto.OpenAIResponsesRequest{Model: info.OriginModelName, Stream: common.GetPointer(true), Input: []byte(`"synthetic test"`)}
			user := &model.User{Username: "raw-" + tc.name, AffCode: tc.name, Status: common.UserStatusEnabled, Quota: initialQuota}
			require.NoError(t, db.Create(user).Error)
			token := &model.Token{UserId: user.Id, Name: "synthetic", Key: "raw-" + tc.name, Status: common.TokenStatusEnabled, RemainQuota: initialQuota, ExpiredTime: -1}
			require.NoError(t, db.Create(token).Error)
			channel := &model.Channel{Name: "synthetic", Type: constant.ChannelTypeOpenAI}
			require.NoError(t, db.Create(channel).Error)
			common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
			info.UserId, info.TokenId, info.TokenKey = user.Id, token.Id, token.Key
			info.UserSetting = dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1}
			info.PriceData = types.PriceData{ModelRatio: 1, CompletionRatio: 1, CacheRatio: 0.25, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
			require.Nil(t, service.PreConsumeBilling(c, 100, info))
			apiErr := ResponsesHelper(c, info)
			require.EqualValues(t, 1, calls.Load(), "never replay a stream after output")
			if tc.status == 0 {
				require.Nil(t, apiErr)
			} else {
				require.NotNil(t, apiErr)
				require.Equal(t, tc.status, apiErr.StatusCode)
				require.True(t, types.IsSkipRetryError(apiErr))
			}
			require.Equal(t, !tc.wantLog, info.Billing.NeedsRefund())
			info.Billing.Refund(c)
			info.Billing.Refund(c)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Find(&logs).Error)
			quota := 0
			if tc.wantLog {
				require.Len(t, logs, 1)
				quota = logs[0].Quota
				if tc.wantCharge {
					require.Positive(t, quota)
				} else {
					require.Zero(t, quota)
				}
				var other struct {
					AdminInfo struct {
						LocalCount bool `json:"local_count_tokens"`
					} `json:"admin_info"`
					StreamStatus struct {
						Status string `json:"status"`
					} `json:"stream_status"`
				}
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				require.Equal(t, tc.estimated, other.AdminInfo.LocalCount)
				status := "ok"
				if tc.status != 0 {
					status = "error"
				}
				require.Equal(t, status, other.StreamStatus.Status)
				require.NoError(t, info.Billing.Settle(quota), "settlement is idempotent")
				if tc.name == "upstream_usage" {
					require.Equal(t, 6, quota, "preserve upstream cache discount: 2 + 8*0.25 + 2")
				}
			} else {
				require.Empty(t, logs)
			}
			require.Eventually(t, func() bool {
				var u model.User
				var tok model.Token
				if db.First(&u, user.Id).Error != nil || db.First(&tok, token.Id).Error != nil {
					return false
				}
				return u.Quota == initialQuota-quota && u.UsedQuota == quota && tok.RemainQuota == initialQuota-quota && tok.UsedQuota == quota
			}, time.Second, time.Millisecond)
		})
	}
}
