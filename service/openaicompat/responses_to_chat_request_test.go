package openaicompat

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestResponsesRequestToChatCompletionsRequestPreservesAgentTurns(t *testing.T) {
	input, err := common.Marshal([]map[string]any{
		{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "checking"}},
		},
		{
			"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`,
		},
		{
			"type": "function_call_output", "call_id": "call_1", "output": map[string]any{"ok": true},
		},
	})
	require.NoError(t, err)
	tools, err := common.Marshal([]map[string]any{{
		"type": "function", "name": "lookup", "description": "Lookup without declared parameters",
	}})
	require.NoError(t, err)
	toolChoice, err := common.Marshal(map[string]any{"type": "function", "name": "lookup"})
	require.NoError(t, err)
	parallel, err := common.Marshal(false)
	require.NoError(t, err)
	instructions, err := common.Marshal("be concise")
	require.NoError(t, err)

	got, err := ResponsesRequestToChatCompletionsRequest(&dto.OpenAIResponsesRequest{
		Model:             "claude-test",
		Input:             input,
		Instructions:      instructions,
		Tools:             tools,
		ToolChoice:        toolChoice,
		ParallelToolCalls: parallel,
	})
	require.NoError(t, err)
	require.Len(t, got.Messages, 3)
	require.Equal(t, "system", got.Messages[0].Role)
	require.Equal(t, "be concise", got.Messages[0].StringContent())
	require.Equal(t, "assistant", got.Messages[1].Role)
	require.Equal(t, "checking", got.Messages[1].StringContent())
	require.Len(t, got.Messages[1].ParseToolCalls(), 1)
	require.Equal(t, "lookup", got.Messages[1].ParseToolCalls()[0].Function.Name)
	require.Equal(t, "tool", got.Messages[2].Role)
	require.Equal(t, "call_1", got.Messages[2].ToolCallId)
	require.Equal(t, `{"ok":true}`, got.Messages[2].StringContent())
	require.Len(t, got.Tools, 1)
	require.Nil(t, got.Tools[0].Function.Parameters)
	require.NotNil(t, got.ParallelTooCalls)
	require.False(t, *got.ParallelTooCalls)
	require.Equal(t, map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "lookup"},
	}, got.ToolChoice)
}

func TestChatCompletionsRequestToResponsesRequestPreservesPromptCacheKey(t *testing.T) {
	key := "session-\"quoted\"\\path\n世界"
	got, err := ChatCompletionsRequestToResponsesRequest(&dto.GeneralOpenAIRequest{
		Model:          "gpt-test",
		Messages:       []dto.Message{{Role: "user", Content: "hello"}},
		PromptCacheKey: key,
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(got)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	require.Equal(t, key, payload["prompt_cache_key"])
}

func TestResponsesRequestToChatCompletionsRequestRejectsStatefulFields(t *testing.T) {
	_, err := ResponsesRequestToChatCompletionsRequest(&dto.OpenAIResponsesRequest{
		Model:              "claude-test",
		PreviousResponseID: "resp_previous",
	})
	require.ErrorContains(t, err, "previous_response_id")
}

func TestChatCompletionsResponseToResponsesResponseKeepsTextAndToolCalls(t *testing.T) {
	message := dto.Message{Role: "assistant", Content: "I need a tool"}
	message.SetToolCalls([]dto.ToolCallResponse{{
		ID: "call_1", Type: "function", Function: dto.FunctionResponse{Name: "lookup", Arguments: `{"q":"x"}`},
	}})
	got, _, err := ChatCompletionsResponseToResponsesResponse(&dto.OpenAITextResponse{
		Id: "chat_1", Model: "claude-test", Created: int64(123),
		Choices: []dto.OpenAITextResponseChoice{{Message: message, FinishReason: "tool_calls"}},
	}, "resp_1")
	require.NoError(t, err)
	require.Len(t, got.Output, 2)
	require.Equal(t, "message", got.Output[0].Type)
	require.Equal(t, "I need a tool", got.Output[0].Content[0].Text)
	require.Equal(t, "function_call", got.Output[1].Type)
	require.Equal(t, "lookup", got.Output[1].Name)
	require.Equal(t, `{"q":"x"}`, got.Output[1].ArgumentsString())
}
