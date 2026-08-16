package claude

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestRequestOpenAI2ClaudeMessagePreservesParameterlessTools(t *testing.T) {
	request := dto.GeneralOpenAIRequest{
		Model:    "claude-test",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
		Tools: []dto.ToolCallRequest{{
			Type:     "function",
			Function: dto.FunctionRequest{Name: "ping", Description: "No arguments"},
		}},
	}
	got, err := RequestOpenAI2ClaudeMessage(nil, request)
	require.NoError(t, err)
	tools := got.GetTools()
	require.Len(t, tools, 1)
	tool, ok := tools[0].(*dto.Tool)
	require.True(t, ok)
	require.Equal(t, "object", tool.InputSchema["type"])
	require.Equal(t, map[string]interface{}{}, tool.InputSchema["properties"])
}

func TestRequestOpenAI2ClaudeMessageOmitsEmptyTools(t *testing.T) {
	got, err := RequestOpenAI2ClaudeMessage(nil, dto.GeneralOpenAIRequest{
		Model: "claude-test", Messages: []dto.Message{{Role: "user", Content: "hello"}},
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(got)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	_, exists := payload["tools"]
	require.False(t, exists)
}
