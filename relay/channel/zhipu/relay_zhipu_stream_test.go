package zhipu

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

func newZhipuStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
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
			UpstreamModelName: "chatglm_std",
		},
	}

	return c, resp, info, recorder
}

func TestZhipuV3StreamHandlerRequiresSuccessMeta(t *testing.T) {
	body := "data:hello\n" +
		"data: world\n" +
		"meta:{\"request_id\":\"req-1\",\"task_id\":\"task-1\",\"task_status\":\"SUCCESS\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n"
	c, resp, info, recorder := newZhipuStreamTest(t, body)

	usage, streamErr := zhipuStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, 3, usage.PromptTokens)
	require.Equal(t, 2, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 3, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "\"content\":\"hello\"")
	require.Contains(t, recorder.Body.String(), "\"content\":\" world\"")
	require.Contains(t, recorder.Body.String(), "\"finish_reason\":\"stop\"")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestZhipuV3StreamHandlerCleanEOFAfterDataDoesNotFinalize(t *testing.T) {
	c, resp, info, recorder := newZhipuStreamTest(t, "data:partial\n")

	usage, streamErr := zhipuStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "\"content\":\"partial\"")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.NotContains(t, recorder.Body.String(), "\"finish_reason\":\"stop\"")
}

func TestZhipuV3StreamHandlerRejectsInvalidPreOutputTermination(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		endReason relaycommon.StreamEndReason
		received  int
	}{
		{
			name:      "explicit failure meta",
			body:      "meta:{\"task_status\":\"FAILED\"}\n",
			endReason: relaycommon.StreamEndReasonHandlerStop,
		},
		{
			name:      "malformed meta",
			body:      "meta:not-json\n",
			endReason: relaycommon.StreamEndReasonHandlerStop,
		},
		{
			name:      "missing task status",
			body:      "meta:{\"request_id\":\"req-1\"}\n",
			endReason: relaycommon.StreamEndReasonEOF,
		},
		{
			name:      "generic done without success meta",
			body:      "data:[DONE]\n",
			endReason: relaycommon.StreamEndReasonEOF,
		},
		{
			name:      "progress meta without success",
			body:      "meta:{\"request_id\":\"req-1\",\"task_status\":\"PROCESSING\"}\n",
			endReason: relaycommon.StreamEndReasonEOF,
			received:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newZhipuStreamTest(t, tt.body)

			usage, streamErr := zhipuStreamHandler(c, info, resp)

			require.Nil(t, usage)
			require.NotNil(t, streamErr)
			require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
			require.Equal(t, tt.endReason, info.StreamStatus.EndReason)
			require.Equal(t, tt.received, info.ReceivedResponseCount)
			require.Empty(t, recorder.Body.String())
			require.Empty(t, recorder.Header().Get("Content-Type"))
		})
	}
}
