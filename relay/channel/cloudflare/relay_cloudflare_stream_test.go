package cloudflare

import (
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

func newCloudflareStreamTest(t *testing.T, body string, includeUsage bool) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
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
		StartTime:          time.Unix(1_700_000_000, 0),
		ShouldIncludeUsage: includeUsage,
		DisablePing:        true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o-mini",
		},
	}

	return c, resp, info, recorder
}

func TestCloudflareStreamHandlerExplicitDoneFinalizes(t *testing.T) {
	body := "data: {\"id\":\"upstream\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n"
	c, resp, info, recorder := newCloudflareStreamTest(t, body, true)

	streamErr, usage := cfStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
	require.Equal(t, 3, strings.Count(recorder.Body.String(), "data: "))
	require.Contains(t, recorder.Body.String(), "\"role\":\"assistant\"")
	require.Contains(t, recorder.Body.String(), "\"content\":\"hello\"")
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestCloudflareStreamHandlerCleanEOFAfterOutputDoesNotFinalize(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	c, resp, info, recorder := newCloudflareStreamTest(t, body, true)

	streamErr, usage := cfStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: "), "truncated streams must not receive a synthetic usage event")
	require.Contains(t, recorder.Body.String(), "\"content\":\"partial\"")
}

func TestCloudflareStreamHandlerInvalidFirstFrameReturnsRelayError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: "data: not-json\n\ndata: [DONE]\n\n"},
		{name: "semantically empty event", body: "data: {}\n\ndata: [DONE]\n\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newCloudflareStreamTest(t, tt.body, false)

			streamErr, usage := cfStreamHandler(c, info, resp)

			require.NotNil(t, streamErr)
			require.Nil(t, usage)
			require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
			require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
			require.Zero(t, info.ReceivedResponseCount)
			require.Empty(t, recorder.Body.String())
			require.Empty(t, recorder.Header().Get("Content-Type"))
		})
	}
}
