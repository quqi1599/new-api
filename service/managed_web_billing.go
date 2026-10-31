package service

import (
	"errors"
	"math"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/shopspring/decimal"
)

// The RMB retail price is five times DeepSeek's verified RMB 5 / 1000 calls.
// Group and model multipliers deliberately do not enter this calculation.
func NewManagedWebBilling(usdToCNY float64) (*relaycommon.ManagedWebBilling, error) {
	if usdToCNY <= 0 || math.IsNaN(usdToCNY) || math.IsInf(usdToCNY, 0) {
		return nil, errors.New("invalid currency conversion for managed web search")
	}
	return &relaycommon.ManagedWebBilling{Provider: "deepseek", CNYPerThousand: 25, USDToCNY: usdToCNY}, nil
}
func ManagedWebQuota(m *relaycommon.ManagedWebBilling) decimal.Decimal {
	if m == nil || m.USDToCNY <= 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(m.SearchRequests + m.FetchRequests)).Mul(decimal.NewFromFloat(m.CNYPerThousand)).Div(decimal.NewFromInt(1000)).Div(decimal.NewFromFloat(m.USDToCNY)).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
}
func managedWebSurcharges(m *relaycommon.ManagedWebBilling) []ToolSurchargeItem {
	var items []ToolSurchargeItem
	if m == nil || m.USDToCNY <= 0 {
		return items
	}
	for _, item := range []struct {
		name  string
		count int
	}{{"deepseek_search", m.SearchRequests}, {"deepseek_fetch", m.FetchRequests}} {
		if item.count == 0 {
			continue
		}
		price := decimal.NewFromFloat(m.CNYPerThousand).Div(decimal.NewFromFloat(m.USDToCNY))
		quota := price.Mul(decimal.NewFromInt(int64(item.count))).Div(decimal.NewFromInt(1000)).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
		items = append(items, ToolSurchargeItem{Name: item.name, Count: item.count, Price: price.InexactFloat64(), Unit: "per_1k_calls", Quota: common.QuotaFromDecimal(quota)})
	}
	if len(items) > 0 {
		allocated := 0
		for i := 0; i < len(items)-1; i++ {
			allocated += items[i].Quota
		}
		items[len(items)-1].Quota = common.QuotaFromDecimal(ManagedWebQuota(m)) - allocated
	}
	return items
}
