package kling

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type closeAwareTaskBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeAwareTaskBody) Close() error {
	b.closed.Store(true)
	return nil
}

type failingTaskBodyReader struct{}

func (failingTaskBodyReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func TestDoResponseAlwaysClosesResponseBody(t *testing.T) {
	t.Run("successful response", func(t *testing.T) {
		body := &closeAwareTaskBody{Reader: strings.NewReader(`{"code":0,"data":{"task_id":"upstream-task"}}`)}
		resp := &http.Response{StatusCode: http.StatusOK, Body: body}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{
			OriginModelName: "model",
			TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "public-task"},
		}

		taskID, _, taskErr := (&TaskAdaptor{}).DoResponse(ctx, resp, info)

		require.Nil(t, taskErr)
		require.Equal(t, "upstream-task", taskID)
		require.True(t, body.closed.Load())
	})

	t.Run("read error", func(t *testing.T) {
		body := &closeAwareTaskBody{Reader: failingTaskBodyReader{}}
		resp := &http.Response{StatusCode: http.StatusOK, Body: body}

		_, _, taskErr := (&TaskAdaptor{}).DoResponse(nil, resp, nil)

		require.NotNil(t, taskErr)
		require.True(t, body.closed.Load())
	})
}
