package tencent

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

func newTencentStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
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
			UpstreamModelName: "hunyuan-pro",
		},
	}

	return c, resp, info, recorder
}

func TestTencentStreamHandlerExplicitFinishReasonFinalizes(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		openAIReason string
	}{
		{name: "stop", finishReason: "stop", openAIReason: "stop"},
		{name: "tool calls", finishReason: "tool_calls", openAIReason: "tool_calls"},
		{name: "sensitive", finishReason: "sensitive", openAIReason: "content_filter"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "data: {\"Choices\":[{\"Delta\":{\"Role\":\"assistant\",\"Content\":\"hello\"}}]}\n\n" +
				"data: {\"Choices\":[{\"Delta\":{\"Role\":\"assistant\",\"Content\":\"\"},\"FinishReason\":\"" + tt.finishReason + "\"}],\"Usage\":{\"PromptTokens\":3,\"CompletionTokens\":2,\"TotalTokens\":5}}\n\n"
			c, resp, info, recorder := newTencentStreamTest(t, body)

			usage, streamErr := tencentStreamHandler(c, info, resp)

			require.Nil(t, streamErr)
			require.NotNil(t, usage)
			require.Equal(t, 3, usage.PromptTokens)
			require.Equal(t, 2, usage.CompletionTokens)
			require.Equal(t, 5, usage.TotalTokens)
			require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
			require.Equal(t, 2, info.ReceivedResponseCount)
			require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
			require.Contains(t, recorder.Body.String(), "\"finish_reason\":\""+tt.openAIReason+"\"")
			require.Contains(t, recorder.Body.String(), "\"content\":\"hello\"")
		})
	}
}

func TestTencentStreamHandlerCleanEOFAfterOutputDoesNotFinalize(t *testing.T) {
	body := "data: {\"Choices\":[{\"Delta\":{\"Role\":\"assistant\",\"Content\":\"partial\"}}]}\n\n"
	c, resp, info, recorder := newTencentStreamTest(t, body)

	usage, streamErr := tencentStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
	require.Contains(t, recorder.Body.String(), "\"content\":\"partial\"")
}

func TestTencentStreamHandlerRejectsInvalidPreOutputFrames(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: "data: not-json\n\n"},
		{name: "OpenAI done is not a native terminal", body: "data: [DONE]\n\n"},
		{name: "semantically empty event", body: "data: {}\n\n"},
		{name: "upstream business error", body: "data: {\"Error\":{\"Code\":1001,\"Message\":\"rejected\"}}\n\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info, recorder := newTencentStreamTest(t, tt.body)

			usage, streamErr := tencentStreamHandler(c, info, resp)

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
