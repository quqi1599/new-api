package hailuo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func hailuoSuccessfulTaskBody(t *testing.T) []byte {
	t.Helper()
	body, err := common.Marshal(QueryTaskResponse{
		TaskID: "task-1",
		Status: TaskStatusSuccess,
		FileID: "file-1",
		BaseResp: BaseResp{
			StatusCode: StatusSuccess,
		},
	})
	require.NoError(t, err)
	return body
}

func TestParseTaskResultContextCancelsSecondaryFileRetrieval(t *testing.T) {
	service.InitHttpClient()
	requestStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	adaptor := &TaskAdaptor{apiKey: "key", baseURL: upstream.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()

	result, err := adaptor.ParseTaskResultContext(ctx, hailuoSuccessfulTaskBody(t))

	require.Nil(t, result)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "secondary request must preserve the polling deadline: %v", err)
	require.Less(t, time.Since(startedAt), time.Second)
	select {
	case <-requestStarted:
	default:
		t.Fatal("secondary file retrieval did not start")
	}
}

func TestParseTaskResultContextReturnsResolvedVideoURL(t *testing.T) {
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file":{"download_url":"https://cdn.example/video.mp4"},"base_resp":{"status_code":0}}`))
	}))
	t.Cleanup(upstream.Close)
	adaptor := &TaskAdaptor{apiKey: "key", baseURL: upstream.URL}

	result, err := adaptor.ParseTaskResultContext(context.Background(), hailuoSuccessfulTaskBody(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "https://cdn.example/video.mp4", result.Url)
}
