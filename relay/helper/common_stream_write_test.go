package helper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/dto"
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
