package dto

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestKimiDynamicToolLoadingSurvivesRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"kimi-k3","tools":[{"type":"function","function":{"name":"top_tool"}}],"messages":[{"role":"system","tools":[{"type":"function","function":{"name":"dynamic_tool","parameters":{"const":9007199254740993}}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"dynamic_tool","arguments":"{}"}}]}]}`)
	var request GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal(raw, &request))
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	var wire struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, common.Unmarshal(encoded, &wire))
	require.NotContains(t, wire.Messages[0], "content")
	require.Contains(t, string(wire.Messages[0]["tools"]), "9007199254740993")
	require.Equal(t, "null", string(wire.Messages[1]["content"]))
	require.Contains(t, wire.Messages[1], "tool_calls")
	meta := request.GetTokenCountMeta()
	require.Equal(t, 2, meta.ToolsCount)
	require.Contains(t, meta.CombineText, "dynamic_tool")
	require.Contains(t, meta.CombineText, "top_tool")
}

func TestKimiEmptyToolDeclarationsKeepOrdinaryContentSemantics(t *testing.T) {
	for _, raw := range []string{"null", "[]", "[ ]"} {
		request := GeneralOpenAIRequest{Model: "kimi-k3", Messages: []Message{{Role: "assistant", Tools: json.RawMessage(raw)}}}
		require.False(t, request.Messages[0].HasToolDeclarations())
		encoded, err := common.Marshal(request)
		require.NoError(t, err)
		var wire struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		require.NoError(t, common.Unmarshal(encoded, &wire))
		require.Equal(t, "null", string(wire.Messages[0]["content"]))
	}
}
