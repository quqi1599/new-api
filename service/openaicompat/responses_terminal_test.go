package openaicompat

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestResponsesIncompleteSurvivesChatRoundTrip(t *testing.T) {
	for _, tc := range []struct{ finish, reason string }{
		{"length", "max_output_tokens"}, {"content_filter", "content_filter"}, {"incomplete", "unknown"},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			chat := &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{
				Message: dto.Message{Role: "assistant", Content: "partial"}, FinishReason: tc.finish,
			}}, Usage: dto.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}}
			response, _, err := ChatCompletionsResponseToResponsesResponse(chat, "fixture")
			require.NoError(t, err)
			require.Equal(t, tc.reason, response.IncompleteDetails.Reason)
			back, usage, err := ResponsesResponseToChatCompletionsResponse(response, "fixture")
			require.NoError(t, err)
			require.Equal(t, tc.finish, back.Choices[0].FinishReason)
			require.Equal(t, "partial", back.Choices[0].Message.StringContent())
			require.Equal(t, 6, usage.TotalTokens)

			state := NewChatToResponsesStreamState("fixture", "test")
			_, err = ChatCompletionsStreamChunkToResponsesEvents(&dto.ChatCompletionsStreamResponse{
				Choices: []dto.ChatCompletionsStreamResponseChoice{{FinishReason: common.GetPointer(tc.finish)}},
			}, state)
			require.NoError(t, err)
			events := FinalizeChatCompletionsStreamToResponses(state)
			require.NotEmpty(t, events)
			terminal := events[len(events)-1]
			require.Equal(t, "response.incomplete", terminal.Type)
			require.Equal(t, tc.reason, terminal.Payload.Response.IncompleteDetails.Reason)
			require.Empty(t, FinalizeChatCompletionsStreamToResponses(state), "only one terminal is allowed")
		})
	}
}

func TestChatToResponsesReopensClosedSegments(t *testing.T) {
	state := NewChatToResponsesStreamState("fixture", "fixture")
	var events []ChatToResponsesStreamEvent
	for _, text := range []string{"first", "second"} {
		var chunk dto.ChatCompletionsStreamResponse
		require.NoError(t, common.UnmarshalJsonStr(`{"choices":[{"delta":{"content":"`+text+`","reasoning_content":"think `+text+`"},"finish_reason":"stop"}]}`, &chunk))
		got, err := ChatCompletionsStreamChunkToResponsesEvents(&chunk, state)
		require.NoError(t, err)
		events = append(events, got...)
	}
	final := FinalizeChatCompletionsStreamToResponses(state)
	require.Len(t, final, 1)
	output := final[0].Payload.Response.Output
	require.Len(t, output, 4)
	require.Equal(t, "think first", output[0].Summary[0].Text)
	require.Equal(t, "first", output[1].Content[0].Text)
	require.Equal(t, "think second", output[2].Summary[0].Text)
	require.Equal(t, "second", output[3].Content[0].Text)
	require.NotEqual(t, output[0].ID, output[2].ID)
	require.NotEqual(t, output[1].ID, output[3].ID)
	added, done := 0, 0
	for _, event := range events {
		if event.Type == "response.reasoning_summary_part.added" {
			added++
		}
		if event.Type == "response.reasoning_summary_part.done" {
			done++
		}
	}
	require.Equal(t, 2, added)
	require.Equal(t, 2, done)
	require.Empty(t, FinalizeChatCompletionsStreamToResponses(state))
}
