package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestClaudeToOpenAIRequestSanitizesToolSchema(t *testing.T) {
	t.Parallel()

	claudeRequest := dto.ClaudeRequest{
		Model: "deepseek-v4-pro",
		Tools: []dto.Tool{
			{
				Name: "mcp__pencil__replace_all_matching_properties",
				InputSchema: map[string]any{
					"type":                 "object",
					"required":             nil,
					"additionalProperties": "object",
					"properties": map[string]any{
						"metadata": map[string]any{
							"type":                 "object",
							"required":             nil,
							"additionalProperties": "object",
						},
						"enabled":         true,
						"replacement_map": "object",
					},
				},
			},
		},
	}

	openAIRequest, err := ClaudeToOpenAIRequest(claudeRequest, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
	})
	require.NoError(t, err)
	require.Len(t, openAIRequest.Tools, 1)

	params, ok := openAIRequest.Tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok)
	require.NotContains(t, params, "required")
	require.Equal(t, map[string]any{"type": "object"}, params["additionalProperties"])

	properties, ok := params["properties"].(map[string]any)
	require.True(t, ok)

	metadata, ok := properties["metadata"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, metadata, "required")
	require.Equal(t, map[string]any{"type": "object"}, metadata["additionalProperties"])

	enabled, ok := properties["enabled"].(map[string]any)
	require.True(t, ok)
	require.Empty(t, enabled)

	replacementMap, ok := properties["replacement_map"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", replacementMap["type"])
}

func TestSanitizeOpenAIFunctionParametersFallsBackToObjectSchema(t *testing.T) {
	t.Parallel()

	params, ok := sanitizeOpenAIFunctionParameters(nil).(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", params["type"])
	require.Equal(t, map[string]any{}, params["properties"])
}

func TestSanitizeOpenAIFunctionToolParametersSanitizesDirectOpenAITools(t *testing.T) {
	t.Parallel()

	request := &dto.GeneralOpenAIRequest{
		Model: "kimi-k2.6",
		Tools: []dto.ToolCallRequest{
			{
				Type: "function",
				Function: dto.FunctionRequest{
					Name: "moonshot_schema_case",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"type": "object",
						},
					},
				},
			},
		},
	}

	SanitizeOpenAIFunctionToolParameters(request)

	params, ok := request.Tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok)
	properties, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	typeProperty, ok := properties["type"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", typeProperty["type"])
}
