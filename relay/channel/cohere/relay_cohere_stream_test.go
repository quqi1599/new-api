package cohere

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

type cohereFailingReadCloser struct {
	data []byte
	sent bool
}

func (r *cohereFailingReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("injected Cohere stream read failure")
}

func (*cohereFailingReadCloser) Close() error { return nil }

func newCohereStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
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
			UpstreamModelName: "command-r",
		},
	}

	return c, resp, info, recorder
}

func TestCohereStreamHandlerZeroFramesReturnsRelayError(t *testing.T) {
	c, resp, info, recorder := newCohereStreamTest(t, "")

	usage, streamErr := cohereStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Zero(t, info.ReceivedResponseCount)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

func TestCohereStreamHandlerCleanEOFAfterPartialDoesNotFinalize(t *testing.T) {
	body := "{\"event_type\":\"text-generation\",\"text\":\"partial\"}\n"
	c, resp, info, recorder := newCohereStreamTest(t, body)

	usage, streamErr := cohereStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestCohereStreamHandlerScannerErrorAfterPartialDoesNotFinalize(t *testing.T) {
	c, resp, info, recorder := newCohereStreamTest(t, "")
	resp.Body = &cohereFailingReadCloser{data: []byte("{\"event_type\":\"text-generation\",\"text\":\"partial\"}\n")}

	usage, streamErr := cohereStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonScannerErr, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
}

func TestCohereStreamHandlerExplicitTerminalFinalizes(t *testing.T) {
	body := "{\"event_type\":\"text-generation\",\"text\":\"hello\"}\n" +
		"{\"event_type\":\"stream-end\",\"is_finished\":true,\"finish_reason\":\"COMPLETE\",\"response\":{\"finish_reason\":\"COMPLETE\",\"meta\":{\"billed_units\":{\"input_tokens\":3,\"output_tokens\":2}}}}\n"
	c, resp, info, recorder := newCohereStreamTest(t, body)

	usage, streamErr := cohereStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, 3, usage.PromptTokens)
	require.Equal(t, 2, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Contains(t, recorder.Body.String(), `"finish_reason":"stop"`)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
}

func TestCohereStreamHandlerRejectsInvalidPreOutputFrame(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "bad JSON", body: "{not-json}\n"},
		{name: "empty JSON object", body: "{}\n"},
		{name: "error event", body: "{\"event_type\":\"stream-error\",\"text\":\"rejected\"}\n"},
		{name: "error envelope", body: "{\"event_type\":\"text-generation\",\"error\":{\"message\":\"rejected\"}}\n"},
		{name: "non-success terminal", body: "{\"event_type\":\"stream-end\",\"is_finished\":true,\"finish_reason\":\"ERROR_LIMIT\"}\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newCohereStreamTest(t, tt.body)

			usage, streamErr := cohereStreamHandler(c, info, resp)

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
