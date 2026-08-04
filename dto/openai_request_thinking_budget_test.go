package dto

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestQwenThinkingBudgetMarshal(t *testing.T) {
	tests := []struct {
		name       string
		request    any
		wantBudget bool
		wantValue  int64
	}{
		{
			name:       "chat preserves explicit zero",
			request:    GeneralOpenAIRequest{Model: "qwen-plus", ThinkingBudget: json.RawMessage(`0`)},
			wantBudget: true,
		},
		{
			name:       "responses preserves qwq budget",
			request:    OpenAIResponsesRequest{Model: "provider/QwQ-32B", ThinkingBudget: json.RawMessage(`128`)},
			wantBudget: true,
			wantValue:  128,
		},
		{
			name:    "chat drops unsupported model budget",
			request: GeneralOpenAIRequest{Model: "gpt-4.1", ThinkingBudget: json.RawMessage(`128`)},
		},
		{
			name:    "responses drops unsupported model budget",
			request: OpenAIResponsesRequest{Model: "deepseek-r1", ThinkingBudget: json.RawMessage(`128`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := common.Marshal(tt.request)
			require.NoError(t, err)
			value := gjson.GetBytes(encoded, "thinking_budget")
			require.Equal(t, tt.wantBudget, value.Exists())
			if tt.wantBudget {
				require.Equal(t, tt.wantValue, value.Int())
			}
		})
	}
}

func TestIsQwenThinkingBudgetModel(t *testing.T) {
	tests := map[string]bool{
		"qwen-plus":                     true,
		"Qwen/Qwen3-235B-A22B-Thinking": true,
		"qwq-32b":                       true,
		"provider/qwq-32b":              true,
		"gpt-4.1":                       false,
		"deepseek-r1":                   false,
	}
	for model, want := range tests {
		require.Equal(t, want, IsQwenThinkingBudgetModel(model), model)
	}
}
