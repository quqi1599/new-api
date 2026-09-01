package ali

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

func TestRequestOpenAI2AliFiltersThinkingBudgetByUpstreamModel(t *testing.T) {
	tests := []struct {
		name          string
		requestModel  string
		upstreamModel string
		budget        string
		wantBudget    bool
	}{
		{name: "qwen", requestModel: "qwen-plus", upstreamModel: "qwen-plus", budget: "128", wantBudget: true},
		{name: "qwq explicit zero", requestModel: "qwq-32b", upstreamModel: "qwq-32b", budget: "0", wantBudget: true},
		{name: "mapped non-qwen", requestModel: "qwen-plus", upstreamModel: "deepseek-r1", budget: "128"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted := requestOpenAI2Ali(dto.GeneralOpenAIRequest{
				Model:          tt.requestModel,
				ThinkingBudget: json.RawMessage(tt.budget),
			}, tt.upstreamModel)
			require.Equal(t, tt.wantBudget, converted.ThinkingBudget != nil)
			if tt.wantBudget {
				require.Equal(t, tt.budget, string(converted.ThinkingBudget))
			}
		})
	}
}

func TestRequestOpenAI2AliPreservesOmittedTopPAndClampsExplicitBounds(t *testing.T) {
	tests := []struct {
		name string
		topP *float64
		want *float64
	}{
		{name: "omitted"},
		{name: "zero", topP: lo.ToPtr(0.0), want: lo.ToPtr(0.01)},
		{name: "one", topP: lo.ToPtr(1.0), want: lo.ToPtr(0.99)},
		{name: "inside", topP: lo.ToPtr(0.5), want: lo.ToPtr(0.5)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted := requestOpenAI2Ali(dto.GeneralOpenAIRequest{Model: "qwen-plus", TopP: tt.topP}, "qwen-plus")
			require.Equal(t, tt.want, converted.TopP)
		})
	}
}
