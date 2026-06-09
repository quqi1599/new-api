package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupOpenAITestContext() (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, recorder
}

func TestHandleLastResponse_DoneFrameDoesNotUnmarshal(t *testing.T) {
	t.Parallel()

	var responseId string
	var createAt int64
	var systemFingerprint string
	var model string
	usage := &dto.Usage{}
	containStreamUsage := false
	shouldSendLastResp := true

	err := handleLastResponse("data: [DONE]", &responseId, &createAt, &systemFingerprint, &model, &usage,
		&containStreamUsage, &relaycommon.RelayInfo{}, &shouldSendLastResp)

	require.NoError(t, err)
	assert.False(t, shouldSendLastResp)
	assert.Empty(t, responseId)
	assert.Empty(t, model)
	assert.False(t, containStreamUsage)
}

func TestHandleStreamFormat_NormalizesBeforeOpenAIOutput(t *testing.T) {
	t.Parallel()

	c, recorder := setupOpenAITestContext()
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI}

	err := HandleStreamFormat(c, info, `data: data: {"id":1}`, false, false)

	require.NoError(t, err)
	assert.Contains(t, recorder.Body.String(), `data: {"id":1}`)
	assert.NotContains(t, recorder.Body.String(), "data: data:")
	assert.Equal(t, 1, info.SendResponseCount)
}

func TestHandleStreamFormat_DoneFrameSkipped(t *testing.T) {
	t.Parallel()

	c, recorder := setupOpenAITestContext()
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI}

	err := HandleStreamFormat(c, info, "data: data: [DONE]", false, false)

	require.NoError(t, err)
	assert.Empty(t, recorder.Body.String())
	assert.Equal(t, 0, info.SendResponseCount)
}

func TestHandleLastResponse_NormalizesNestedDataPrefix(t *testing.T) {
	t.Parallel()

	var responseId string
	var createAt int64
	var systemFingerprint string
	var model string
	var usage *dto.Usage
	containStreamUsage := false
	shouldSendLastResp := true

	lastStreamData := `data: data: {"id":"chatcmpl-test","created":1710000000,"model":"MiniMax-M3","system_fingerprint":"fp-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

	err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model, &usage,
		&containStreamUsage, &relaycommon.RelayInfo{}, &shouldSendLastResp)

	require.NoError(t, err)
	assert.Equal(t, "chatcmpl-test", responseId)
	assert.Equal(t, int64(1710000000), createAt)
	assert.Equal(t, "fp-test", systemFingerprint)
	assert.Equal(t, "MiniMax-M3", model)
	require.NotNil(t, usage)
	assert.Equal(t, 1, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
	assert.True(t, containStreamUsage)
	assert.False(t, shouldSendLastResp)
}

func TestHandleFinalResponse_DoneFramesDoNotUnmarshalForConvertedFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		relayFormat types.RelayFormat
	}{
		{name: "claude", relayFormat: types.RelayFormatClaude},
		{name: "gemini", relayFormat: types.RelayFormatGemini},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, recorder := setupOpenAITestContext()
			info := &relaycommon.RelayInfo{RelayFormat: tt.relayFormat}

			require.NotPanics(t, func() {
				HandleFinalResponse(c, info, "data: data: [DONE]", "", 0, "", "", &dto.Usage{}, false)
			})
			assert.Empty(t, recorder.Body.String())
		})
	}
}

func TestProcessTokens_NormalizesPrefixedStreamItems(t *testing.T) {
	t.Parallel()

	streamItems := []string{
		`data: data: {"choices":[{"delta":{"content":"hel"}}]}`,
		`data:{"choices":[{"delta":{"reasoning_content":"lo"}}]}`,
		`data: data: [DONE]`,
	}
	var responseTextBuilder strings.Builder
	toolCount := 0

	err := processTokens(relayconstant.RelayModeChatCompletions, streamItems, &responseTextBuilder, &toolCount)

	require.NoError(t, err)
	assert.Equal(t, "hello", responseTextBuilder.String())
	assert.Equal(t, 0, toolCount)
}
