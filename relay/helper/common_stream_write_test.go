package helper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type failingStreamWriter struct {
	gin.ResponseWriter
	err error
}

func (w *failingStreamWriter) Write([]byte) (int, error) { return 0, w.err }
func (w *failingStreamWriter) WriteString(string) (int, error) {
	return 0, w.err
}

func newFailingStreamContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Writer = &failingStreamWriter{ResponseWriter: c.Writer, err: errors.New("downstream write failed")}
	return c
}

func TestStreamWritersPropagateDownstreamWriteErrors(t *testing.T) {
	tests := []struct {
		name  string
		write func(*gin.Context) error
	}{
		{name: "plain SSE", write: func(c *gin.Context) error { return StringData(c, "hello") }},
		{name: "Claude SSE", write: func(c *gin.Context) error {
			return ClaudeChunkData(c, dto.ClaudeResponse{Type: "content_block_delta"}, `{}`)
		}},
		{name: "Responses SSE", write: func(c *gin.Context) error {
			return ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.output_text.delta"}, `{}`)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.write(newFailingStreamContext())
			require.Error(t, err)
			require.Contains(t, err.Error(), "downstream write failed")
		})
	}
}

func TestStreamStartedAndSendInBandStreamError(t *testing.T) {
	tests := []struct {
		name        string
		relayFormat types.RelayFormat
		start       string
		want        string
	}{
		{
			name:        "claude error event",
			relayFormat: types.RelayFormatClaude,
			start:       "event: message_start\ndata: {}\n\n",
			want:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream overloaded\"}}\n\n",
		},
		{
			name:        "openai error object",
			relayFormat: types.RelayFormatOpenAI,
			start:       "data: {}\n\n",
			want:        "data: {\"error\":{\"message\":\"upstream overloaded\",\"type\":\"overloaded_error\",\"param\":\"\",\"code\":\"overloaded_error\"}}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			require.False(t, StreamStarted(c))

			_, err := c.Writer.Write([]byte(tt.start))
			require.NoError(t, err)
			require.True(t, StreamStarted(c))

			relayErr := types.WithClaudeError(types.ClaudeError{
				Type:    "overloaded_error",
				Message: "upstream overloaded",
			}, http.StatusInternalServerError)
			require.NoError(t, SendInBandStreamError(c, tt.relayFormat, relayErr))
			require.Equal(t, tt.start+tt.want, recorder.Body.String())
		})
	}
}

func TestSendInBandStreamErrorRejectsUnstartedStream(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	relayErr := types.WithClaudeError(types.ClaudeError{
		Type:    "overloaded_error",
		Message: "upstream overloaded",
	}, http.StatusInternalServerError)

	require.ErrorContains(t, SendInBandStreamError(c, types.RelayFormatClaude, relayErr), "stream has not started")
	require.Empty(t, recorder.Body.String())
	require.False(t, recorder.Flushed)
}
