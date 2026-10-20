package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func serverToolBillingFixture() (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{
		OriginModelName: "deepseek-v4.1-flash", StartTime: time.Now(),
		PriceData: types.PriceData{ModelRatio: 1, CompletionRatio: 1, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
	}
	return c, info
}

func TestReportedSearchUsageOverridesOtherViews(t *testing.T) {
	c, info := serverToolBillingFixture()
	info.OriginModelName = "gpt-4.1"
	c.Set("claude_web_search_requests", 4)
	info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
		dto.BuildInToolWebSearch:        {CallCount: 7},
		dto.BuildInToolWebSearchPreview: {CallCount: 5},
		dto.BuildInToolFileSearch:       {CallCount: 2},
	}}
	usage := &dto.Usage{PromptTokens: 100, CompletionTokens: 50, ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: common.GetPointer(2)}}
	summary := calculateTextQuotaSummary(c, info, usage)
	require.Equal(t, 150, summary.TotalTokens)
	require.Equal(t, 2, summary.WebSearchCallCount)
	require.Zero(t, summary.ClaudeWebSearchCallCount)
	require.Len(t, summary.ToolSurchargeItems, 2)
	want := common.QuotaRound((2*operation_setting.GetToolPriceForModel("web_search_preview", info.OriginModelName) + 2*operation_setting.GetToolPriceForModel("file_search", info.OriginModelName)) / 1000 * common.QuotaPerUnit)
	require.Equal(t, want, common.QuotaFromDecimal(summary.ToolCallSurchargeQuota))
	info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", GroupRatio: 1}
	require.Equal(t, 1000+want, composeTieredTextQuota(info, summary, 1000, &billingexpr.TieredResult{ActualQuotaBeforeGroup: 1000, ActualQuotaAfterGroup: 1000}))
	require.Equal(t, 7, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount, "settlement must not mutate recorded events")
}

func TestReportedZeroSearchSuppressesInferredChargesAndIsLogged(t *testing.T) {
	c, info := serverToolBillingFixture()
	c.Set("claude_web_search_requests", 3)
	info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{dto.BuildInToolWebSearch: {CallCount: 8}}}
	usage := &dto.Usage{ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: common.GetPointer(0)}}
	summary := calculateTextQuotaSummary(c, info, usage)
	require.True(t, summary.ToolCallSurchargeQuota.IsZero())
	require.Zero(t, summary.ClaudeWebSearchCallCount)
	require.Empty(t, summary.ToolSurchargeItems)
	other := map[string]interface{}{}
	injectReportedServerToolUsage(other, summary)
	wire, err := common.Marshal(other)
	require.NoError(t, err)
	require.JSONEq(t, `{"server_tool_use":{"web_search_requests":0},"tool_usage_source":"upstream_reported"}`, string(wire))
}

func TestFreeReportedSearchRetainsObservationWithoutCharge(t *testing.T) {
	setting := config.GlobalConfig.Get("tool_price_setting").(*operation_setting.ToolPriceSetting)
	previous := setting.Prices
	prices := make(map[string]float64, len(previous)+1)
	for key, price := range previous {
		prices[key] = price
	}
	prices["web_search:free-search-fixture*"] = 0
	setting.Prices = prices
	operation_setting.RebuildToolPriceIndex()
	t.Cleanup(func() { setting.Prices = previous; operation_setting.RebuildToolPriceIndex() })
	c, info := serverToolBillingFixture()
	info.OriginModelName = "free-search-fixture"
	usage := &dto.Usage{PromptTokens: 10, CompletionTokens: 2, ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: common.GetPointer(3)}}
	summary := calculateTextQuotaSummary(c, info, usage)
	require.Equal(t, 12, summary.TotalTokens)
	require.Equal(t, 3, summary.ClaudeWebSearchCallCount)
	require.Zero(t, summary.ClaudeWebSearchPrice)
	require.True(t, summary.ToolCallSurchargeQuota.IsZero())
	require.Empty(t, summary.ToolSurchargeItems)
	other := map[string]interface{}{}
	injectReportedServerToolUsage(other, summary)
	require.Equal(t, 3, *other["server_tool_use"].(*dto.ServerToolUsage).WebSearchRequests)
}

func TestOpenAIToClaudePreservesReportedServerToolUsage(t *testing.T) {
	for _, count := range []*int{nil, common.GetPointer(0), common.GetPointer(3)} {
		src := &dto.Usage{PromptTokens: 10, CompletionTokens: 5, ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: count}}
		converted := buildClaudeUsageFromOpenAIUsage(src)
		require.Equal(t, 10, converted.InputTokens)
		require.Equal(t, 5, converted.OutputTokens)
		if count == nil {
			require.Nil(t, converted.ServerToolUse)
		} else {
			require.Equal(t, *count, *converted.ServerToolUse.WebSearchRequests)
			*count = 99
			require.NotEqual(t, 99, *converted.ServerToolUse.WebSearchRequests)
		}
	}
}
