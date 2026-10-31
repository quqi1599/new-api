package service

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"

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
	// NewAPI dollars are prepaid platform credits: RMB 40 buys 200 credits.
	// The display FX rate is not the cash selling price of those credits.
	creditCNY := 0.2
	if raw := strings.TrimSpace(os.Getenv("DEEPSEEK_SEARCH_CNY_PER_CREDIT")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, errors.New("invalid cash value per platform credit")
		}
		creditCNY = parsed
	}
	return &relaycommon.ManagedWebBilling{Provider: "deepseek", CNYPerThousand: 25, USDToCNY: usdToCNY, CNYPerCredit: creditCNY}, nil
}
func ManagedWebQuota(m *relaycommon.ManagedWebBilling) decimal.Decimal {
	if m == nil || managedWebCreditCNY(m) <= 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(m.SearchRequests + m.FetchRequests)).Mul(decimal.NewFromFloat(m.CNYPerThousand)).Div(decimal.NewFromInt(1000)).Div(decimal.NewFromFloat(managedWebCreditCNY(m))).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
}
func managedWebSurcharges(m *relaycommon.ManagedWebBilling) []ToolSurchargeItem {
	var items []ToolSurchargeItem
	if m == nil || managedWebCreditCNY(m) <= 0 {
		return items
	}
	for _, item := range []struct {
		name  string
		count int
	}{{"deepseek_search", m.SearchRequests}, {"deepseek_fetch", m.FetchRequests}} {
		if item.count == 0 {
			continue
		}
		price := decimal.NewFromFloat(m.CNYPerThousand).Div(decimal.NewFromFloat(managedWebCreditCNY(m)))
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

// Old records without a credit selling price retain their recorded FX basis.
func managedWebCreditCNY(m *relaycommon.ManagedWebBilling) float64 {
	if m.CNYPerCredit > 0 {
		return m.CNYPerCredit
	}
	return m.USDToCNY
}
