package openaicompat

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestThinkingControlsSurviveResponsesRoundTrip(t *testing.T) {
	for _, fields := range []string{`"enable_thinking":false`, `"thinking":{"type":"disabled"}`, `"reasoning_effort":"none"`, `"enable_thinking":true,"thinking_budget":4096`} {
		t.Run(fields, func(t *testing.T) {
			var original dto.GeneralOpenAIRequest
			require.NoError(t, common.UnmarshalJsonStr(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],`+fields+`}`, &original))
			responses, err := ChatCompletionsRequestToResponsesRequest(&original)
			require.NoError(t, err)
			wire, err := common.Marshal(responses)
			require.NoError(t, err)
			var parsed dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal(wire, &parsed))
			got, err := ResponsesRequestToChatCompletionsRequest(&parsed)
			require.NoError(t, err)
			require.Equal(t, original.EnableThinking, got.EnableThinking)
			require.Equal(t, original.THINKING, got.THINKING)
			require.Equal(t, original.ThinkingBudget, got.ThinkingBudget)
			require.Equal(t, original.ReasoningEffort, got.ReasoningEffort)
		})
	}
}
