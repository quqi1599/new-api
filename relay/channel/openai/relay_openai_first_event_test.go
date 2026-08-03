package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOaiStreamHandlerReturns504WithoutSynthesizingDoneOnFirstEventTimeout(t *testing.T) {
	oldFirstEventTimeout := constant.RelayFirstEventTimeout
	constant.RelayFirstEventTimeout = 1
	t.Cleanup(func() {
		constant.RelayFirstEventTimeout = oldFirstEventTimeout
	})

	reader, writer := io.Pipe()
	defer writer.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "test-model",
		},
	}

	usage, handlerErr := OaiStreamHandler(c, info, &http.Response{Body: reader})

	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusGatewayTimeout, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.Empty(t, recorder.Body.String(), "pre-output timeout must not become an empty [DONE] success")

	c.JSON(handlerErr.StatusCode, gin.H{"error": handlerErr.ToOpenAIError()})
	require.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.NotContains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
}

func TestOaiStreamHandlerRejectsCleanEOFWithoutAnyValidEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "test-model",
		},
	}

	usage, handlerErr := OaiStreamHandler(c, info, &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	})

	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamStreamIncomplete, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.Empty(t, recorder.Body.String(), "zero-event EOF must not become an empty [DONE] success")
}

func TestOaiStreamHandlerRejectsErrorEnvelopeBeforeDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "test-model",
		},
	}

	usage, handlerErr := OaiStreamHandler(c, info, &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n" +
				"data: [DONE]\n\n",
		)),
	})

	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamStreamIncomplete, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.Empty(t, recorder.Body.String(), "an upstream error frame must not become a successful SSE stream")
	require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
}

func TestOaiStreamHandlerRejectsNonChunkJSONBeforeDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "test-model",
		},
	}

	usage, handlerErr := OaiStreamHandler(c, info, &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: {\"status\":\"failed\"}\n\ndata: [DONE]\n\n")),
	})

	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
	require.Empty(t, recorder.Body.String())
	require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
}

func TestOpenaiTTSHandlerRejectsCleanEOFBeforeCommittingSuccess(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	info := &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "test-tts-model",
		},
	}

	usage, handlerErr := OpenaiTTSHandler(c, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}, info)

	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusBadGateway, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamStreamIncomplete, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.Empty(t, recorder.Body.String(), "TTS zero-event EOF must remain controller-visible")
	require.Empty(t, recorder.Header().Get("Content-Type"), "upstream SSE headers must not contaminate the JSON error response")
}
