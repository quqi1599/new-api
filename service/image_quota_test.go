package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func imageQuotaFixture() *relaycommon.RelayInfo {
	price := types.PriceData{UsePrice: true, ModelPrice: 0.12, ModelRatio: 1, CompletionRatio: 2,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
	price.AddOtherRatio("n", 2)
	return &relaycommon.RelayInfo{OriginModelName: "gpt-image-2", RelayFormat: types.RelayFormatOpenAIImage,
		RelayMode: relayconstant.RelayModeImagesGenerations, PriceData: price, StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1}}
}

func TestImageFixedQuotaUsesCompleteImagesWithoutRewritingUsageOrPrices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		completed int
		failed    bool
		input     int
		output    int
		want      int
	}{
		{"preview only with reported tokens", 0, true, 10, 20, 0},
		{"preview only without tokens", 0, true, 0, 0, 0},
		{"one completed then failed without tokens", 1, true, 0, 0, 60000},
		{"one completed then failed with tokens", 1, true, 10, 20, 60000},
		{"success keeps requested count with reported zero", 1, false, 0, 0, 120000},
		{"success applies count once", 2, false, 10, 20, 120000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			info := imageQuotaFixture()
			usage := &dto.Usage{PromptTokens: tc.input, CompletionTokens: tc.output, TotalTokens: tc.input + tc.output}
			beforeUsage, beforePrice, beforeRatios := *usage, info.PriceData, info.PriceData.OtherRatios()
			relaycommon.SetImageBillingOutcome(ctx, tc.completed, tc.failed)
			summary := calculateTextQuotaSummary(ctx, info, usage)
			require.Equal(t, tc.want, summary.Quota)
			require.Equal(t, 0.12, summary.ModelPrice)
			require.Equal(t, 1.0, summary.ModelRatio)
			require.Equal(t, tc.input, summary.PromptTokens)
			require.Equal(t, tc.output, summary.CompletionTokens)
			require.Equal(t, beforeUsage, *usage)
			require.Equal(t, beforePrice, info.PriceData)
			require.Equal(t, beforeRatios, info.PriceData.OtherRatios())
		})
	}
}

func TestImageFixedCountPreservesOtherMultipliersAndEndpointIsolation(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := imageQuotaFixture()
	info.PriceData.AddOtherRatio("synthetic_factor", 1.5)
	relaycommon.SetImageBillingOutcome(ctx, 1, true)
	summary := calculateTextQuotaSummary(ctx, info, &dto.Usage{})
	require.Equal(t, 90000, summary.Quota)
	info.PriceData.AddOtherRatio("n", 1)
	relaycommon.SetImageBillingOutcome(ctx, 1, false)
	summary = calculateTextQuotaSummary(ctx, info, &dto.Usage{})
	require.Equal(t, 90000, summary.Quota, "successful adaptor-adjusted n must be applied once")
	info.RelayMode = relayconstant.RelayModeChatCompletions
	summary = calculateTextQuotaSummary(ctx, info, &dto.Usage{})
	require.False(t, summary.FixedImageBilling)
	require.Zero(t, summary.Quota, "image observations cannot make zero-token text billable")
}

func TestImageSettlementKeepsActualTokensFrozenPricingAndSingleRefund(t *testing.T) {
	for _, tc := range []struct {
		name      string
		completed int
		failed    bool
		input     int
		output    int
		mode      string
		want      int
	}{
		{"fixed preview failure", 0, true, 10, 20, "fixed", 0},
		{"fixed completed partial failure", 1, true, 0, 0, "fixed", 60000},
		{"fixed success reported zero", 1, false, 0, 0, "fixed", 120000},
		{"token preview retains reported usage", 0, true, 10, 20, "token", 50},
		{"token complete reported zero stays zero", 1, false, 0, 0, "token", 0},
		{"tiered preview retains frozen expression", 0, true, 10, 20, "tiered", 40},
		{"tiered snapshot outranks a fixed-price flag", 0, true, 10, 20, "tiered_fixed_flag", 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, account := setupWalletReservationBoundary(t, 1_000_000, 1_000_000)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Log{}))
			require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "synthetic-image"}).Error)
			oldLogs, oldBatch, oldExport, oldRedis := common.LogConsumeEnabled, common.BatchUpdateEnabled, common.DataExportEnabled, common.RedisEnabled
			common.LogConsumeEnabled, common.BatchUpdateEnabled, common.DataExportEnabled, common.RedisEnabled = true, false, false, false
			t.Cleanup(func() {
				common.LogConsumeEnabled, common.BatchUpdateEnabled, common.DataExportEnabled, common.RedisEnabled = oldLogs, oldBatch, oldExport, oldRedis
			})
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			info := imageQuotaFixture()
			info.UserId, info.TokenId, info.TokenKey = account.UserId, account.TokenId, account.TokenKey
			info.UserQuota, info.ForcePreConsume = 1_000_000, true
			info.UsingGroup = "default"
			if tc.mode != "fixed" && tc.mode != "tiered_fixed_flag" {
				info.PriceData.UsePrice = false
				info.PriceData.ReplaceOtherRatios(nil)
			}
			if tc.mode == "tiered" || tc.mode == "tiered_fixed_flag" {
				expr := `tier("base", p * 2 + c * 3)`
				info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: expr,
					ExprHash: billingexpr.ExprHashString(expr), ExprVersion: billingexpr.ExprVersion(expr), QuotaPerUnit: common.QuotaPerUnit, GroupRatio: 1}
			}
			beforeSnapshot, err := common.Marshal(info.TieredBillingSnapshot)
			require.NoError(t, err)
			beforePrice, beforeRatios := info.PriceData, info.PriceData.OtherRatios()
			usage := &dto.Usage{PromptTokens: tc.input, CompletionTokens: tc.output, TotalTokens: tc.input + tc.output}
			beforeUsage := *usage
			relaycommon.SetImageBillingOutcome(ctx, tc.completed, tc.failed)
			session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: info.UserId}}
			require.Nil(t, session.preConsume(ctx, 120000))
			info.Billing = session

			PostTextConsumeQuota(ctx, info, usage, nil)

			userQuota, tokenRemain, tokenUsed := persistedWalletAndTokenQuota(t, info)
			require.Equal(t, 1_000_000-tc.want, userQuota)
			require.Equal(t, 1_000_000-tc.want, tokenRemain)
			require.Equal(t, tc.want, tokenUsed)
			require.True(t, session.settled)
			require.False(t, session.NeedsRefund())
			// The controller returns the stream error after settlement and invokes
			// Refund. A repeated settlement/refund cannot charge or refund again.
			session.Refund(ctx)
			require.NoError(t, session.Settle(tc.want))
			afterUser, afterRemain, afterUsed := persistedWalletAndTokenQuota(t, info)
			require.Equal(t, []int{userQuota, tokenRemain, tokenUsed}, []int{afterUser, afterRemain, afterUsed})
			require.Equal(t, beforeUsage, *usage)
			require.Equal(t, beforePrice, info.PriceData)
			require.Equal(t, beforeRatios, info.PriceData.OtherRatios())
			afterSnapshot, err := common.Marshal(info.TieredBillingSnapshot)
			require.NoError(t, err)
			require.JSONEq(t, string(beforeSnapshot), string(afterSnapshot))

			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			require.Equal(t, tc.want, logs[0].Quota)
			require.Equal(t, tc.input, logs[0].PromptTokens)
			require.Equal(t, tc.output, logs[0].CompletionTokens)
			require.Equal(t, 0.12, gjson.Get(logs[0].Other, "model_price").Float())
			require.Equal(t, 1.0, gjson.Get(logs[0].Other, "model_ratio").Float())
			if tc.mode == "fixed" {
				require.Equal(t, "image", gjson.Get(logs[0].Other, "image_billing_unit").String())
				require.Equal(t, int64(tc.completed), gjson.Get(logs[0].Other, "image_completed_count").Int())
				billedCount := tc.completed
				if !tc.failed {
					billedCount = 2
				}
				require.Equal(t, int64(billedCount), gjson.Get(logs[0].Other, "image_billing_count").Int())
			} else {
				require.False(t, gjson.Get(logs[0].Other, "image_billing_count").Exists())
			}
		})
	}
}
