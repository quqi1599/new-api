package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestBatch1ClaudeToolNumbers(t *testing.T) {
	for _, args := range []string{`{"id":9007199254740993}`, `{"ids":[18446744073709551615]}`, `{"value":0.123456789012345678901}`, `{"q":"ok"}`} {
		t.Run(args, func(t *testing.T) {
			message := dto.Message{Role: "assistant"}
			message.SetToolCalls([]dto.ToolCallRequest{{ID: "call_1", Type: "function", Function: dto.FunctionRequest{Name: "lookup", Arguments: args}}})
			got := ResponseOpenAI2Claude(&dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: message, FinishReason: "tool_calls"}}}, &relaycommon.RelayInfo{})
			require.Len(t, got.Content, 1)
			wire, err := common.Marshal(got.Content[0].Input)
			require.NoError(t, err)
			require.Equal(t, args, string(wire))
		})
	}
}

func TestBatch1GeminiTextAndTools(t *testing.T) {
	call := dto.GeminiPart{FunctionCall: &dto.FunctionCall{FunctionName: "lookup", Arguments: map[string]any{"q": "ok"}}}
	for _, tc := range []struct {
		name  string
		parts []dto.GeminiPart
		texts []string
		calls int
	}{
		{"both", []dto.GeminiPart{{Text: "checking"}, call}, []string{"checking"}, 1},
		{"around", []dto.GeminiPart{{Text: "before"}, call, {Text: "after"}}, []string{"before", "after"}, 1},
		{"text", []dto.GeminiPart{{Text: "checking"}}, []string{"checking"}, 0},
		{"tool", []dto.GeminiPart{call}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GeminiToOpenAIRequest(&dto.GeminiChatRequest{Contents: []dto.GeminiChatContent{{Role: "model", Parts: tc.parts}}}, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "fixture"}})
			require.NoError(t, err)
			wire, err := common.Marshal(got)
			require.NoError(t, err)
			var decoded dto.GeneralOpenAIRequest
			require.NoError(t, common.Unmarshal(wire, &decoded))
			require.Len(t, decoded.Messages, 1)
			parts := decoded.Messages[0].ParseContent()
			require.Len(t, parts, len(tc.texts))
			for i, want := range tc.texts {
				require.Equal(t, want, parts[i].Text)
			}
			require.Len(t, decoded.Messages[0].ParseToolCalls(), tc.calls)
		})
	}
}
