package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const exactFormatSchema = `{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993,"minimum":0},"flag":{"type":"boolean","default":false}},"additionalProperties":false,"required":["n"]}`

func TestOutputFormatSchemaRoundTripPreservesNumbersAndStrictness(t *testing.T) {
	for _, strict := range []string{"true", "false"} {
		format := &dto.ResponseFormat{Type: "json_schema", JsonSchema: json.RawMessage(`{"name":"result","strict":` + strict + `,"schema":` + exactFormatSchema + `}`)}
		text, err := chatFormatToResponsesText(format, json.RawMessage(`"low"`))
		require.NoError(t, err)
		require.Equal(t, "9007199254740993", gjson.GetBytes(text, "format.schema.properties.n.const").Raw)
		require.Equal(t, strict, gjson.GetBytes(text, "format.strict").Raw)
		back, verbosity, err := responsesTextToChatFormat(text)
		require.NoError(t, err)
		require.Equal(t, `"low"`, string(verbosity))
		require.Equal(t, exactFormatSchema, gjson.GetBytes(back.JsonSchema, "schema").Raw)
		claude := &dto.ClaudeRequest{OutputConfig: json.RawMessage(`{"effort":"low"}`)}
		err = ApplyChatOutputFormatToClaude(back, claude)
		if strict == "false" {
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			require.True(t, types.IsLocalOutputFormatRouteError(apiErr))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, "low", gjson.GetBytes(claude.OutputConfig, "effort").String())
		require.Equal(t, "9007199254740993", gjson.GetBytes(claude.OutputConfig, "format.schema.properties.n.const").Raw)
		require.Equal(t, "0", gjson.GetBytes(claude.OutputConfig, "format.schema.properties.n.minimum").Raw)
		require.Equal(t, "false", gjson.GetBytes(claude.OutputConfig, "format.schema.additionalProperties").Raw)
		chat, err := ClaudeOutputFormatToChat(claude)
		require.NoError(t, err)
		require.Equal(t, "true", gjson.GetBytes(chat.JsonSchema, "strict").Raw)
		require.Equal(t, gjson.GetBytes(claude.OutputConfig, "format.schema").Raw, gjson.GetBytes(chat.JsonSchema, "schema").Raw)
	}
}

func TestOutputFormatRejectsUnrepresentableExtensionsBeforeGeneration(t *testing.T) {
	for _, body := range []string{
		`{"type":"json_object"}`,
		`{"type":"json_object","future_limit":0}`,
		`{"type":"json_schema","json_schema":{"name":"result","schema":{},"strict":false}}`,
		`{"type":"json_schema","json_schema":{"name":"result","schema":{},"future_flag":false}}`,
	} {
		var format dto.ResponseFormat
		require.NoError(t, common.UnmarshalJsonStr(body, &format))
		err := ApplyChatOutputFormatToClaude(&format, &dto.ClaudeRequest{})
		var apiErr *types.NewAPIError
		require.ErrorAs(t, err, &apiErr)
		require.True(t, types.IsLocalOutputFormatRouteError(apiErr))
		require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	}
	for _, body := range []string{
		`{"format":{"type":"json_schema","name":"result","schema":{},"future_flag":false}}`,
		`{"format":{"type":"json_object","future_limit":0}}`,
	} {
		_, _, err := responsesTextToChatFormat([]byte(body))
		require.Error(t, err)
	}
	for _, body := range []string{
		`{"type":"json_schema","json_schema":{"name":"result","schema":{},"strict":0}}`,
		`{"type":"json_schema","json_schema":{"schema":{}}}`,
		`{"type":"json_schema","json_schema":{"name":"result","schema":[]}}`,
	} {
		var format dto.ResponseFormat
		require.NoError(t, common.UnmarshalJsonStr(body, &format))
		var apiErr *types.NewAPIError
		require.ErrorAs(t, ApplyChatOutputFormatToClaude(&format, &dto.ClaudeRequest{}), &apiErr)
		require.Equal(t, 400, apiErr.StatusCode)
		require.False(t, types.IsLocalOutputFormatRouteError(apiErr))
	}
}

func TestClaudeLegacyFormatAndEffortPreserveRawSchema(t *testing.T) {
	request := &dto.ClaudeRequest{OutputFormat: json.RawMessage(`{"type":"json_schema","schema":` + exactFormatSchema + `}`)}
	format, err := ClaudeOutputFormatToChat(request)
	require.NoError(t, err)
	require.Equal(t, exactFormatSchema, gjson.GetBytes(format.JsonSchema, "schema").Raw)
	request.OutputConfig = json.RawMessage(`{"format":{"type":"json_schema","schema":` + exactFormatSchema + `}}`)
	_, err = ReadClaudeOutputFormat(request)
	require.Error(t, err, "conflicting legacy/new fields must not choose silently")
	request.OutputFormat = nil
	require.NoError(t, SetClaudeOutputEffort(request, "high"))
	require.Equal(t, exactFormatSchema, gjson.GetBytes(request.OutputConfig, "format.schema").Raw)
}
