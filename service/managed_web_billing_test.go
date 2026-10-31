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
			require.Equal(t, 187500, common.QuotaFromDecimal(summary.ToolCallSurchargeQuota))
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
	for _, tc := range []struct{ calls, quota int }{{0, 0}, {1, 62500}, {2, 125000}, {50, 3125000}, {613, 38312500}, {1000, 62500000}} {
		m.SearchRequests = tc.calls
		require.Equal(t, tc.quota, common.QuotaFromDecimal(ManagedWebQuota(m)))
	}
}

func TestManagedWebCashConversionIgnoresDisplayFX(t *testing.T) {
	t.Setenv("DEEPSEEK_SEARCH_CNY_PER_CREDIT", "0.2")
	for _, fx := range []float64{1, 7.3, 10} {
		m, err := NewManagedWebBilling(fx)
		require.NoError(t, err)
		m.SearchRequests = 1
		require.Equal(t, 62500, common.QuotaFromDecimal(ManagedWebQuota(m)))
		require.Equal(t, 125.0, managedWebSurcharges(m)[0].Price)
	}
	for _, bad := range []string{"0", "-1", "NaN", "Inf", "invalid"} {
		t.Setenv("DEEPSEEK_SEARCH_CNY_PER_CREDIT", bad)
		_, err := NewManagedWebBilling(7.3)
		require.Error(t, err)
	}
	t.Setenv("DEEPSEEK_SEARCH_CNY_PER_CREDIT", "0.3")
	m, err := NewManagedWebBilling(7.3)
	require.NoError(t, err)
	m.SearchRequests = 3
	require.Equal(t, 125000, common.QuotaFromDecimal(ManagedWebQuota(m)), "round the combined charge, not each call")
}
