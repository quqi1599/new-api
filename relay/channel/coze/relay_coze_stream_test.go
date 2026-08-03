package coze

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

func newCozeStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
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
			UpstreamModelName: "coze-bot",
		},
	}

	return c, resp, info, recorder
}

func TestCozeStreamHandlerEventAwareFramesAndEOFCompleted(t *testing.T) {
	body := "event: conversation.message.delta\n" +
		"data: {\"content\":\n" +
		"data: \"hello\",\"type\":\"answer\"}\n\n" +
		"event: conversation.chat.completed\n" +
		"data: {\"status\":\"completed\",\"usage\":{\"token_count\":5,\"output_count\":2,\"input_count\":3}}"
	c, resp, info, recorder := newCozeStreamTest(t, body)

	usage, streamErr := cozeChatStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, 3, usage.PromptTokens)
	require.Equal(t, 2, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "\"content\":\"hello\"")
	require.Contains(t, recorder.Body.String(), "\"finish_reason\":\"stop\"")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestCozeStreamHandlerCleanEOFAfterDeltaDoesNotFinalize(t *testing.T) {
	body := "event: conversation.message.delta\n" +
		"data: {\"content\":\"partial\",\"type\":\"answer\"}\n\n"
	c, resp, info, recorder := newCozeStreamTest(t, body)

	usage, streamErr := cozeChatStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "\"content\":\"partial\"")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.NotContains(t, recorder.Body.String(), "\"finish_reason\":\"stop\"")
}

func TestCozeStreamHandlerRejectsFailureAndGenericDoneBeforeOutput(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		endReason relaycommon.StreamEndReason
	}{
		{
			name:      "failure event",
			body:      "event: conversation.chat.failed\ndata: {\"code\":400}\n\n",
			endReason: relaycommon.StreamEndReasonHandlerStop,
		},
		{
			name:      "generic done without completed event",
			body:      "data: [DONE]\n\n",
			endReason: relaycommon.StreamEndReasonEOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newCozeStreamTest(t, tt.body)

			usage, streamErr := cozeChatStreamHandler(c, info, resp)

			require.Nil(t, usage)
			require.NotNil(t, streamErr)
			require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
			require.Equal(t, tt.endReason, info.StreamStatus.EndReason)
			require.Zero(t, info.ReceivedResponseCount)
			require.Empty(t, recorder.Body.String())
			require.Empty(t, recorder.Header().Get("Content-Type"))
		})
	}
}

func TestCozeStreamHandlerMalformedCompletedEventIsNotSuccess(t *testing.T) {
	c, resp, info, recorder := newCozeStreamTest(t,
		"event: conversation.chat.completed\ndata: not-json\n\n")

	usage, streamErr := cozeChatStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	require.Zero(t, info.ReceivedResponseCount)
	require.Empty(t, recorder.Body.String())
}
