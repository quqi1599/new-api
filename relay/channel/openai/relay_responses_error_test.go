package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOaiResponsesStreamHandlerRecordsResponseFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.failed\",\"error\":{\"message\":\"upstream failed\"}}\n" +
			"data: [DONE]\n",
	))}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	_, handlerErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeBadResponse, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.NotNil(t, info.StreamStatus)
	require.Equal(t, 1, info.StreamStatus.TotalErrorCount())
	require.Contains(t, info.StreamStatus.Errors[0].Message, "response.failed")
	require.NotContains(t, recorder.Body.String(), "[DONE]")
}
