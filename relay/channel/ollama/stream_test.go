package ollama

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ollamaFailingReadCloser struct {
	data []byte
	sent bool
}

func (r *ollamaFailingReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("injected Ollama stream read failure")
}

func (*ollamaFailingReadCloser) Close() error { return nil }

func newOllamaStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{
		StartTime:   time.Unix(1_700_000_000, 0),
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "llama3.2",
		},
	}

	return c, resp, info, recorder
}

func TestOllamaStreamHandlerZeroFramesReturnsRelayError(t *testing.T) {
	c, resp, info, recorder := newOllamaStreamTest(t, "")

	usage, streamErr := ollamaStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Zero(t, info.ReceivedResponseCount)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

func TestOllamaStreamHandlerCleanEOFAfterPartialDoesNotFinalize(t *testing.T) {
	body := "{\"model\":\"llama3.2\",\"created_at\":\"2026-08-03T10:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":\"partial\"},\"done\":false}\n"
	c, resp, info, recorder := newOllamaStreamTest(t, body)

	usage, streamErr := ollamaStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.Equal(t, 2, strings.Count(recorder.Body.String(), "data: "), "only the accepted start and partial delta may be emitted")
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestOllamaStreamHandlerScannerErrorAfterPartialDoesNotFinalize(t *testing.T) {
	c, resp, info, recorder := newOllamaStreamTest(t, "")
	resp.Body = &ollamaFailingReadCloser{data: []byte("{\"model\":\"llama3.2\",\"message\":{\"role\":\"assistant\",\"content\":\"partial\"},\"done\":false}\n")}

	usage, streamErr := ollamaStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonScannerErr, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
}

func TestOllamaStreamHandlerExplicitDoneFinalizes(t *testing.T) {
	body := "{\"model\":\"llama3.2\",\"created_at\":\"2026-08-03T10:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":\"hello\"},\"done\":false}\n" +
		"{\"model\":\"llama3.2\",\"created_at\":\"2026-08-03T10:00:01Z\",\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":4,\"eval_count\":2}\n"
	c, resp, info, recorder := newOllamaStreamTest(t, body)

	usage, streamErr := ollamaStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, 4, usage.PromptTokens)
	require.Equal(t, 2, usage.CompletionTokens)
	require.Equal(t, 6, usage.TotalTokens)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Contains(t, recorder.Body.String(), `"finish_reason":"stop"`)
	require.Contains(t, recorder.Body.String(), `"prompt_tokens":4`)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
}

func TestOllamaStreamHandlerRejectsInvalidPreOutputFrame(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "bad JSON", body: "{not-json}\n"},
		{name: "empty JSON object", body: "{}\n"},
		{name: "error envelope", body: "{\"model\":\"llama3.2\",\"error\":\"model failed\"}\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newOllamaStreamTest(t, tt.body)

			usage, streamErr := ollamaStreamHandler(c, info, resp)

			require.Nil(t, usage)
			require.NotNil(t, streamErr)
			require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
			require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
			require.Zero(t, info.ReceivedResponseCount)
			require.Empty(t, recorder.Body.String())
			require.Empty(t, recorder.Header().Get("Content-Type"))
		})
	}
}
