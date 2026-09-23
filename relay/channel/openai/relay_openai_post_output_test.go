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

type postOutputBrokenReader struct{ io.Reader }

func (r postOutputBrokenReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestPostOutputChatPreservesErrorAfterOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		IsStream:    true,
		RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test-model"},
	}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: {\"error\":{\"type\":\"server_error\",\"code\":\"upstream_stream_break\",\"message\":\"synthetic stream failure\"}}\n\n"
	usage, err := OaiStreamHandler(c, info, &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))})
	require.NotNil(t, usage)
	require.Equal(t, 1, usage.PromptTokens)
	require.Equal(t, 1, usage.CompletionTokens)
	require.Same(t, info.PartialStreamError, err)
	require.NotContains(t, recorder.Body.String(), "[DONE]")
	if err == nil {
		t.Errorf("upstream error swallowed after output: end=%s received=%d written=%v", info.StreamStatus.EndReason, info.ReceivedResponseCount, c.Writer.Written())
	}
	if !strings.Contains(recorder.Body.String(), "upstream_stream_break") {
		t.Errorf("upstream error also absent from downstream SSE: %s", recorder.Body.String())
	}
}

func TestPostOutputResponsesRequiresTerminalSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{"created_then_clean_eof", strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")},
		{"created_then_transport_error", postOutputBrokenReader{strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")}},
		{"created_then_unexpected_done", strings.NewReader("data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
			_, err := OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(tc.body)})
			if err == nil {
				t.Fatalf("incomplete Responses stream reported success: end=%s received=%d written=%v", info.StreamStatus.EndReason, info.ReceivedResponseCount, c.Writer.Written())
			}
			require.True(t, types.IsSkipRetryError(err))
			require.True(t, types.IsChannelPenaltyAllowed(err))
			require.Same(t, info.PartialStreamError, err)
		})
	}
}
