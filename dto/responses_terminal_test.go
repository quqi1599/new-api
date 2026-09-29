package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestResponsesTerminalReasonAndState(t *testing.T) {
	for _, tc := range []struct{ event, body, state, reason, finish string }{
		{"response.completed", `{}`, "completed", "", "stop"},
		{"response.done", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "incomplete", "max_output_tokens", "length"},
		{"response.incomplete", `{"status":"completed","incomplete_details":{"reason":"content_filter"}}`, "incomplete", "content_filter", "content_filter"},
		{"response.incomplete", `{"incomplete_details":{"reasoning":"max_output_tokens"}}`, "incomplete", "unknown", "incomplete"},
		{"", `{"status":"incomplete","incomplete_details":{"reason":"customer secret"}}`, "incomplete", "unknown", "incomplete"},
	} {
		var response OpenAIResponsesResponse
		require.NoError(t, common.UnmarshalJsonStr(tc.body, &response))
		state, reason := ResponsesTerminal(tc.event, &response)
		require.Equal(t, tc.state, state)
		require.Equal(t, tc.reason, reason)
		require.Equal(t, tc.finish, ResponsesFinishReason(tc.event, &response, false))
	}
}

func TestClaudeSafeguardsSurvivesRelayDTO(t *testing.T) {
	var req ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"model":"fixture","messages":[{"role":"user","content":"hello","output_config":{"effort":"high"}}],"safeguards":{"guardrails":["test"],"enabled":false}}`, &req))
	data, err := common.Marshal(req)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, common.Unmarshal(data, &decoded))
	require.Equal(t, false, decoded["safeguards"].(map[string]any)["enabled"])
	require.JSONEq(t, `{"effort":"high"}`, string(req.Messages[0].OutputConfig))
}
