package claude

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"testing"
)

func TestClaudeSchemaConversionPreservesExplicitZeroControls(t *testing.T) {
	var request dto.GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"model":"claude-test","stream":false,"temperature":0,"top_p":0,"max_tokens":0,"messages":[{"role":"user","content":"synthetic"}],"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}}}`, &request))
	got, err := RequestOpenAI2ClaudeMessage(nil, request)
	require.NoError(t, err)
	body, err := common.Marshal(got)
	require.NoError(t, err)
	for _, path := range []string{"temperature", "top_p", "max_tokens"} {
		require.Equal(t, "0", gjson.GetBytes(body, path).Raw)
	}
	require.Equal(t, "false", gjson.GetBytes(body, "stream").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(body, "output_config.format.schema.properties.n.const").Raw)
}
