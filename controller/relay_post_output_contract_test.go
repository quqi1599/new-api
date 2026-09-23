package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPostOutputFailureReachesControllerWithoutReplayOrSuccess(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{IsStream: true, OriginModelName: "gpt-5.6-sol", ChannelMeta: &relaycommon.ChannelMeta{}}
	_, err := openai.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200,
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))})
	require.NotNil(t, err, "nil would enter RecordChannelCircuitSuccess in Relay")
	require.True(t, service.ShouldCountChannelCircuitFailure(err))
	require.False(t, canRetryGPTChannelFallback(info, types.RelayFormatOpenAIResponses, err))
	require.True(t, relayRetryIsUnsafe(c, info, types.RelayFormatOpenAIResponses, err))
	before := c.Writer.Size()
	writeRelayError(c, nil, types.RelayFormatOpenAIResponses, err)
	require.Equal(t, before, c.Writer.Size(), "controller must not append a second/plain JSON error")
}
