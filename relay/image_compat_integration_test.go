package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type imageFailingHTTPWriter struct {
	*httptest.ResponseRecorder
	allow, writes int
}

func (w *imageFailingHTTPWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.allow >= 0 && w.writes > w.allow {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func TestImagesHTTPAndBillingIntegration(t *testing.T) {
	service.InitHttpClient()
	settings := model_setting.GetGlobalSettings()
	oldPass := settings.PassThroughRequestEnabled
	settings.PassThroughRequestEnabled = false
	t.Cleanup(func() { settings.PassThroughRequestEnabled = oldPass })
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldBatch, oldLog, oldExport := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, false, true, false
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "image-billing.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = oldRedis, oldBatch, oldLog, oldExport
		if conn, err := db.DB(); err == nil {
			_ = conn.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
	const initialQuota = 10000000
	const usage = `"usage":{"input_tokens":10,"output_tokens":2}`
	const partial = "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"preview\"," + usage + "}\n\n"
	const completed = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"complete\"," + usage + "}\n\n"
	const broken = "data: broken\n\n"
	unit := int(common.QuotaPerUnit)
	for _, tc := range []struct {
		name, body          string
		fixed, stream, sse  bool
		writes, status, fee int
	}{
		{"partial_token", partial + broken, false, true, true, -1, 502, 12},
		{"partial_fixed", partial + broken, true, true, true, -1, 502, 0},
		{"complete_fixed_failed", completed + broken, true, true, true, -1, 502, unit},
		{"duplicate_fixed_failed", completed + completed + broken, true, true, true, -1, 502, unit},
		{"disconnect_fixed", partial + completed, true, true, true, 1, 499, unit},
		{"disconnect_token", partial + completed, false, true, true, 1, 499, 12},
		{"complete_success_keeps_n", completed, true, true, true, -1, 0, 2 * unit},
		{"zero_token", `{"data":[{"b64_json":"complete"}],"usage":{"input_tokens":0,"output_tokens":0}}`, false, false, false, -1, 0, 0},
		{"zero_fixed", `{"data":[{"b64_json":"complete"}],"usage":{"input_tokens":0,"output_tokens":0}}`, true, false, false, -1, 0, 2 * unit},
		{"json_disconnect", `{"data":[{"b64_json":"complete"}],` + usage + `}`, true, false, false, 0, 499, unit},
		{"json_as_sse", `{"data":[{"b64_json":"complete"}],` + usage + `}`, false, true, false, -1, 0, 12},
		{"sse_as_json", completed, false, false, true, -1, 0, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			outbound := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				outbound <- body
				if tc.sse {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			writer := &imageFailingHTTPWriter{ResponseRecorder: httptest.NewRecorder(), allow: tc.writes}
			c, _ := gin.CreateTestContext(writer)
			requestBody := `{"model":"gpt-image-2","prompt":"synthetic image","n":2,"stream":` + map[bool]string{true: "true", false: "false"}[tc.stream] + `,"partial_images":0,"output_compression":0}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(requestBody))
			c.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			request, err := helper.GetAndValidOpenAIImageRequest(c, relayconstant.RelayModeImagesGenerations)
			require.NoError(t, err)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-image-2")
			user := &model.User{Username: tc.name, AffCode: tc.name, Status: common.UserStatusEnabled, Quota: initialQuota}
			require.NoError(t, db.Create(user).Error)
			token := &model.Token{UserId: user.Id, Name: "image-test", Key: tc.name, Status: common.TokenStatusEnabled, RemainQuota: initialQuota, ExpiredTime: -1}
			require.NoError(t, db.Create(token).Error)
			channel := &model.Channel{Name: "image-test", Type: constant.ChannelTypeOpenAI}
			require.NoError(t, db.Create(channel).Error)
			common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
			info := &relaycommon.RelayInfo{IsStream: tc.stream, OriginModelName: "gpt-image-2", Request: request,
				RelayMode: relayconstant.RelayModeImagesGenerations, RelayFormat: types.RelayFormatOpenAI,
				RequestURLPath: "/v1/images/generations", StartTime: time.Now(), UserId: user.Id, TokenId: token.Id, TokenKey: token.Key,
				UserSetting: dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1},
				PriceData:   types.PriceData{UsePrice: tc.fixed, ModelPrice: 1, ModelRatio: 1, CompletionRatio: 1, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
			}
			if tc.fixed {
				info.PriceData.AddOtherRatio("n", 2)
			}
			require.Nil(t, service.PreConsumeBilling(c, 2*unit, info))
			apiErr := ImageHelper(c, info)
			require.EqualValues(t, 1, calls.Load(), "accepted image requests must not be replayed")
			if tc.status == 0 {
				require.Nil(t, apiErr)
			} else {
				require.NotNil(t, apiErr)
				require.Equal(t, tc.status, apiErr.StatusCode)
				require.True(t, types.IsSkipRetryError(apiErr))
			}
			wire := <-outbound
			require.Equal(t, "gpt-image-2", gjson.GetBytes(wire, "model").String())
			require.True(t, gjson.GetBytes(wire, "stream").Exists())
			require.Equal(t, tc.stream, gjson.GetBytes(wire, "stream").Bool())
			require.Equal(t, "0", gjson.GetBytes(wire, "partial_images").Raw)
			require.Equal(t, "0", gjson.GetBytes(wire, "output_compression").Raw)
			require.False(t, info.Billing.NeedsRefund(), "settlement must finish exactly once even for partial failure")
			info.Billing.Refund(c)
			info.Billing.Refund(c)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			require.Equal(t, tc.fee, logs[0].Quota)
			var savedUser model.User
			require.NoError(t, db.First(&savedUser, user.Id).Error)
			require.Equal(t, initialQuota-tc.fee, savedUser.Quota, "extra refund must not undo settled image usage")
			var savedToken model.Token
			require.NoError(t, db.First(&savedToken, token.Id).Error)
			require.Equal(t, initialQuota-tc.fee, savedToken.RemainQuota)
			if strings.HasPrefix(tc.name, "zero_") {
				require.Zero(t, logs[0].PromptTokens)
				require.Zero(t, logs[0].CompletionTokens)
			}
			require.Equal(t, 1.0, info.PriceData.ModelPrice)
			if tc.fixed {
				require.Equal(t, 2.0, info.PriceData.OtherRatios()["n"])
			}
		})
	}
}
