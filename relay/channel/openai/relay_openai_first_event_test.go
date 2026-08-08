package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenaiHandlerPreservesNonStreamBodyTimeoutAs504(t *testing.T) {
	oldTimeout := common2.RelayNonStreamTimeout
	common2.RelayNonStreamTimeout = 1
	service.InitHttpClient()
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldTimeout
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := channel.DoRequest(c, req, info)
	require.NoError(t, err)

	started := time.Now()
	usage, handlerErr := OpenaiHandler(c, info, resp)
	require.Nil(t, usage)
	require.NotNil(t, handlerErr)
	require.Equal(t, http.StatusGatewayTimeout, handlerErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, handlerErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(handlerErr))
	require.True(t, types.IsChannelPenaltyAllowed(handlerErr))
	require.Empty(t, recorder.Body.String(), "the provider handler must leave the typed 504 for the controller")
	require.Less(t, time.Since(started), 2*time.Second)
}

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
	require.False(t, recorder.Flushed, "pre-output timeout must not commit HTTP 200 before the controller writes the error")

	c.JSON(handlerErr.StatusCode, gin.H{"error": handlerErr.ToOpenAIError()})
	require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
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
