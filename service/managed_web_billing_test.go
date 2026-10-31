package service

import (
	"math"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestManagedWebPriceIgnoresEveryModelAndGroupMultiplier(t *testing.T) {
	for _, group := range []float64{0, 1, 2, 3, 10} {
		for _, model := range []string{"gpt-4o", "gpt-6.1-sol", "claude-opus-5.5", "deepseek-v4.1-flash"} {
			c, info := serverToolBillingFixture()
			info.OriginModelName = model
			info.PriceData.GroupRatioInfo.GroupRatio = group
			info.PriceData.ModelRatio = 100
			var err error
			info.ManagedWeb, err = NewManagedWebBilling(7.3)
			require.NoError(t, err)
			info.ManagedWeb.SearchRequests = 2
			info.ManagedWeb.FetchRequests = 1
			info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{dto.BuildInToolWebSearchPreview: {CallCount: 999}}}
			c.Set("claude_web_search_requests", 999)
			summary := calculateTextQuotaSummary(c, info, &dto.Usage{ServerToolUse: &dto.ServerToolUsage{WebSearchRequests: common.GetPointer(999)}})
			require.Equal(t, 5137, common.QuotaFromDecimal(summary.ToolCallSurchargeQuota))
			require.Len(t, summary.ToolSurchargeItems, 2)
		}
	}
}
func TestManagedWebPriceCurrencyAndRounding(t *testing.T) {
	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		_, err := NewManagedWebBilling(rate)
		require.Error(t, err)
	}
	m, err := NewManagedWebBilling(7.3)
	require.NoError(t, err)
	for _, tc := range []struct{ calls, quota int }{{0, 0}, {1, 1712}, {2, 3425}, {50, 85616}, {613, 1049658}, {1000, 1712329}} {
		m.SearchRequests = tc.calls
		require.Equal(t, tc.quota, common.QuotaFromDecimal(ManagedWebQuota(m)))
	}
}
