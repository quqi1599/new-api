package dto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIChatCapabilitiesCompatibility(t *testing.T) {
	for _, tt := range []struct {
		model, effort                                      string
		completion, developer, temperature, topP, logprobs bool
	}{
		{"gpt-6-sol", "", true, true, true, true, true},
		{"gpt-6-luna", "none", true, true, true, true, true},
		{"gpt-6-sol", "high", true, true, false, false, false},
		{"gpt-6-astra", "none", true, true, false, false, false},
		{"gpt-6-luna-2026-09-03", "none", true, true, true, true, true},
		{"gpt-6-sol-2026-99-99", "none", false, false, true, true, true},
		{"gpt-6-sol-custom", "high", false, false, true, true, true},
		{"gpt-7", "high", false, false, true, true, true},
		{"gpt-5.2", "none", true, true, true, true, true},
		{"gpt-5.2", "high", true, true, false, false, false},
		{"gpt-5.2-pro", "none", true, true, false, false, false},
		{"gpt-5.6-sol", "high", true, true, false, false, false},
		{"o3", "high", true, true, false, true, true},
		{"o1-mini", "", true, false, false, true, true},
		{"o1-preview", "", true, false, false, true, true},
		{"ollama-model", "high", false, false, true, true, true},
		{"qwen3.8-max", "high", false, false, true, true, true},
	} {
		t.Run(tt.model+"/"+tt.effort, func(t *testing.T) {
			got := GetOpenAIChatCapabilities(tt.model, tt.effort)
			require.Equal(t, OpenAIChatCapabilities{UseMaxCompletionTokens: tt.completion, UseDeveloperRole: tt.developer, SupportsTemperature: tt.temperature, SupportsTopP: tt.topP, SupportsLogProbs: tt.logprobs}, got)
		})
	}
	for _, name := range []string{"gpt-6-sol-high", "gpt-6-luna-none", "gpt-6-astra-2026-09-03-high"} {
		require.Equal(t, "developer", (&GeneralOpenAIRequest{Model: name}).GetSystemRoleName())
	}
}
