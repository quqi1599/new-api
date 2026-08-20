package claude

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

func TestClaudeStreamHandlerDeliversMidStreamErrorInBand(t *testing.T) {
	tests := []struct {
		name        string
		relayFormat types.RelayFormat
		wantError   string
	}{
		{
			name:        "native claude",
			relayFormat: types.RelayFormatClaude,
			wantError:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream overloaded\"}}\n\n",
		},
		{
			name:        "openai translation",
			relayFormat: types.RelayFormatOpenAI,
			wantError:   "data: {\"error\":{\"message\":\"upstream overloaded\",\"type\":\"overloaded_error\",\"param\":\"\",\"code\":\"overloaded_error\"}}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse := strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":0}}}`,
				``,
				`event: error`,
				`data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}`,
				``,
			}, "\n")
			recorder, c, info, resp := newClaudeStreamErrorTestContext(tt.relayFormat, sse)

			usage, apiErr := ClaudeStreamHandler(c, resp, info)

			require.Nil(t, usage)
			require.NotNil(t, apiErr)
			require.Equal(t, types.ErrorCode("overloaded_error"), apiErr.GetErrorCode())
			require.True(t, c.Writer.Written())
			require.Contains(t, recorder.Body.String(), tt.wantError)
			require.NotContains(t, recorder.Body.String(), "[DONE]")
			require.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
		})
	}
}

func TestClaudeStreamHandlerLeavesFirstErrorForHTTPController(t *testing.T) {
	sse := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}`,
		``,
	}, "\n")
	recorder, c, info, resp := newClaudeStreamErrorTestContext(types.RelayFormatClaude, sse)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	require.Empty(t, recorder.Body.String())
	require.False(t, c.Writer.Written())
	require.False(t, recorder.Flushed)
}

func newClaudeStreamErrorTestContext(relayFormat types.RelayFormat, sse string) (*httptest.ResponseRecorder, *gin.Context, *relaycommon.RelayInfo, *http.Response) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: relayFormat,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	return recorder, c, info, resp
}
