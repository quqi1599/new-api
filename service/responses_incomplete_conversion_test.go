package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestIncompleteChatToClaudeRetainsPartialToolOutput(t *testing.T) {
	for _, args := range []string{`{"value":`, `null`, `{"value":1}`} {
		message := dto.Message{Role: "assistant", Content: "partial text"}
		message.SetToolCalls([]dto.ToolCallResponse{{ID: "call_fixture", Type: "function", Function: dto.FunctionResponse{Name: "fixture", Arguments: args}}})
		response := &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: message, FinishReason: "length"}}}
		info := &relaycommon.RelayInfo{ResponsesOutcome: &relaycommon.ResponsesOutcome{State: "incomplete", Reason: "max_output_tokens"}}
		converted := ResponseOpenAI2Claude(response, info)
		require.Equal(t, "max_tokens", converted.StopReason)
		require.Len(t, converted.Content, 2)
		require.Equal(t, "partial text", *converted.Content[0].Text)
		if strings.HasSuffix(args, "}") {
			require.Equal(t, "tool_use", converted.Content[1].Type)
		} else {
			require.Equal(t, "text", converted.Content[1].Type)
			require.Contains(t, *converted.Content[1].Text, args)
			require.Nil(t, converted.Content[1].Input)
		}
	}
}

func TestClaudeToolImagesFollowParallelResults(t *testing.T) {
	var req dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"model":"fixture","messages":[
        {"role":"assistant","content":[{"type":"tool_use","id":"one","name":"a","input":{}},{"type":"tool_use","id":"two","name":"b","input":{}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"one","content":[{"type":"text","text":"first"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQ=="}}]},
        {"type":"tool_result","tool_use_id":"two","content":[{"type":"image","source":{"type":"url","url":"https://fixture.invalid/image"}}]}]}]}`, &req))
	got, err := ClaudeToOpenAIRequest(req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
	require.NoError(t, err)
	require.Len(t, got.Messages, 4)
	require.Equal(t, "first", got.Messages[1].StringContent())
	require.Equal(t, "one", got.Messages[1].ToolCallId)
	require.Equal(t, "two", got.Messages[2].ToolCallId)
	require.Equal(t, "user", got.Messages[3].Role)
	parts := got.Messages[3].ParseContent()
	require.Len(t, parts, 2)
	require.Equal(t, "data:image/png;base64,AQ==", parts[0].GetImageMedia().Url)
	require.Equal(t, "https://fixture.invalid/image", parts[1].GetImageMedia().Url)
}
