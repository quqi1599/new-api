package claude

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

// Upstream #7561: parsing and re-marshalling native Messages must not drop the
// per-message configuration, including unknown future fields and explicit zeros.
func TestNativeMessageOutputConfigCompatibility(t *testing.T) {
	body := `{"model":"claude-opus-5.5","max_tokens":64,"output_config":{"effort":"medium"},"messages":[` +
		`{"role":"user","content":"summary"},` +
		`{"role":"system","content":[],"output_config":{"effort":"high","future_flag":false,"future_limit":0}},` +
		`{"role":"assistant","content":"done"}]}`
	var request dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &request))
	converted, err := (&Adaptor{}).ConvertClaudeRequest(nil, nil, &request)
	require.NoError(t, err)
	encoded, err := common.Marshal(converted)
	require.NoError(t, err)
	var got struct {
		OutputConfig map[string]any   `json:"output_config"`
		Messages     []map[string]any `json:"messages"`
	}
	require.NoError(t, common.Unmarshal(encoded, &got))
	require.Equal(t, map[string]any{"effort": "medium"}, got.OutputConfig)
	require.Len(t, got.Messages, 3)
	require.Equal(t, map[string]any{"effort": "high", "future_flag": false, "future_limit": float64(0)}, got.Messages[1]["output_config"])
	require.Equal(t, []any{}, got.Messages[1]["content"])
	require.NotContains(t, got.Messages[0], "output_config")
	require.NotContains(t, got.Messages[2], "output_config")
}
