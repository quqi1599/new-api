package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestEstimateClaudeInputTokensIncludesToolSchemas(t *testing.T) {
	var withoutTools dto.ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(`{
		"model":"claude-haiku-4-5","messages":[{"role":"user","content":"count"}]
	}`), &withoutTools))
	var withTools dto.ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(`{
		"model":"claude-haiku-4-5","messages":[{"role":"user","content":"count"}],
		"tools":[{"name":"Bash","description":"Execute a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]
	}`), &withTools))

	base := EstimateClaudeInputTokens(&withoutTools)
	withSchema := EstimateClaudeInputTokens(&withTools)
	require.Greater(t, base, 0)
	require.Greater(t, withSchema, base)
}

func TestEstimateClaudeInputTokensNilSafety(t *testing.T) {
	require.Zero(t, EstimateClaudeInputTokens(nil))
	require.Zero(t, EstimateClaudeInputTokens(&dto.ClaudeRequest{}))
}
