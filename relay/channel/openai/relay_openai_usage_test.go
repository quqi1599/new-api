package openai

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSSEPayloadForUsageResponse_ExtractsLastImagePayload(t *testing.T) {
	t.Parallel()

	body := []byte("event: image_generation.partial_image\ndata: {\"created\":1710000000,\"data\":[{\"b64_json\":\"partial\"}]}\n\nevent: image_generation.completed\ndata: data: {\"created\":1710000001,\"data\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	got, ok := normalizeSSEPayloadForUsageResponse(body)

	require.True(t, ok)
	assert.JSONEq(t, `{"created":1710000001,"data":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, string(got))

	var usageResp dto.SimpleResponse
	require.NoError(t, common.Unmarshal(got, &usageResp))
	assert.Equal(t, 2, usageResp.PromptTokens)
	assert.Equal(t, 3, usageResp.CompletionTokens)
	assert.Equal(t, 5, usageResp.TotalTokens)
}

func TestNormalizeSSEPayloadForUsageResponse_IgnoresPlainJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	got, ok := normalizeSSEPayloadForUsageResponse(body)

	assert.False(t, ok)
	assert.Equal(t, body, got)
}
