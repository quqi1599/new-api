package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

type contextTaskPollingAdaptor struct{}

func (contextTaskPollingAdaptor) Init(*relaycommon.RelayInfo) {}
func (contextTaskPollingAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, errors.New("legacy fetch must not be used")
}
func (contextTaskPollingAdaptor) FetchTaskContext(ctx context.Context, _ string, _ string, _ map[string]any, _ string) (*http.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (contextTaskPollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}
func (contextTaskPollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

type contextTaskResultTimeoutAdaptor struct {
	contextTaskPollingAdaptor
}

func (contextTaskResultTimeoutAdaptor) ParseTaskResultContext(ctx context.Context, _ []byte) (*relaycommon.TaskInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type accumulatingTaskPollingAdaptor struct {
	mu         sync.Mutex
	initCount  int
	fetchCount int
}

func (a *accumulatingTaskPollingAdaptor) Init(*relaycommon.RelayInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initCount++
}

func (a *accumulatingTaskPollingAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, errors.New("legacy fetch must not be used")
}

func (a *accumulatingTaskPollingAdaptor) FetchTaskContext(_ context.Context, _ string, key string, body map[string]any, _ string) (*http.Response, error) {
	a.mu.Lock()
	a.fetchCount++
	a.mu.Unlock()
	taskID, _ := body["task_id"].(string)
	if taskID == "" {
		taskID = key
	}
	return nil, types.NewErrorWithStatusCode(
		fmt.Errorf("timeout for %s: %w", taskID, context.DeadlineExceeded),
		types.ErrorCodeUpstreamResponseHeaderTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
	)
}

func (a *accumulatingTaskPollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}

func (a *accumulatingTaskPollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

func (a *accumulatingTaskPollingAdaptor) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.initCount, a.fetchCount
}

type blockingTaskPollingBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingTaskPollingBody() *blockingTaskPollingBody {
	return &blockingTaskPollingBody{closed: make(chan struct{})}
}

func (b *blockingTaskPollingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingTaskPollingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestFetchTaskWithContextPropagatesDeadlineAsTyped504(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := fetchTaskWithContext(ctx, contextTaskPollingAdaptor{}, "https://example.invalid", "key", nil, "")

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamResponseHeaderTimeout, apiErr.GetErrorCode())
}

func TestReadTaskPollingBodyStopsAtRequestDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	body := newBlockingTaskPollingBody()
	resp := &http.Response{StatusCode: http.StatusOK, Body: body}

	started := time.Now()
	_, err := readTaskPollingBody(ctx, resp)

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
}

func TestParseTaskPollingResultClassifiesSecondaryRequestDeadlineAsTyped504(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := ParseTaskPollingResultContext(ctx, contextTaskResultTimeoutAdaptor{}, []byte(`{}`))

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
}

func TestDispatchSunoAccumulatesChannelErrors(t *testing.T) {
	channelIDs := []int{910001, 910002}
	seedTaskPollingChannels(t, channelIDs)
	adaptor := &accumulatingTaskPollingAdaptor{}
	installTaskPollingAdaptor(t, adaptor)

	err := DispatchPlatformUpdateContext(context.Background(), constant.TaskPlatformSuno, map[int][]string{
		channelIDs[0]: {"suno-a"},
		channelIDs[1]: {"suno-b"},
	}, nil)

	_, fetchCount := adaptor.counts()
	require.Error(t, err)
	require.Equal(t, 2, fetchCount)
	require.Contains(t, err.Error(), "channel #910001")
	require.Contains(t, err.Error(), "channel #910002")
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
}

func TestUpdateVideoTasksAccumulatesTaskAndChannelErrors(t *testing.T) {
	channelIDs := []int{920001, 920002}
	seedTaskPollingChannels(t, channelIDs)
	adaptor := &accumulatingTaskPollingAdaptor{}
	installTaskPollingAdaptor(t, adaptor)

	taskM := map[string]*model.Task{
		"video-a": {TaskID: "video-a", ChannelId: channelIDs[0]},
		"video-b": {TaskID: "video-b", ChannelId: channelIDs[0]},
		"video-c": {TaskID: "video-c", ChannelId: channelIDs[1]},
	}
	err := UpdateVideoTasks(context.Background(), constant.TaskPlatform("timeout-test"), map[int][]string{
		channelIDs[0]: {"video-a", "video-b"},
		channelIDs[1]: {"video-c"},
	}, taskM)

	initCount, fetchCount := adaptor.counts()
	require.Error(t, err)
	require.Equal(t, 2, initCount)
	require.Equal(t, 3, fetchCount)
	for _, want := range []string{"video-a", "video-b", "video-c", "channel #920001", "channel #920002"} {
		require.Contains(t, err.Error(), want)
	}
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
}

func seedTaskPollingChannels(t *testing.T, channelIDs []int) {
	t.Helper()
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		model.DB.Delete(&model.Channel{}, channelIDs)
	})
	for _, channelID := range channelIDs {
		baseURL := fmt.Sprintf("https://channel-%d.example.invalid", channelID)
		require.NoError(t, model.DB.Create(&model.Channel{
			Id:      channelID,
			Name:    fmt.Sprintf("polling-%d", channelID),
			Key:     fmt.Sprintf("key-%d", channelID),
			BaseURL: &baseURL,
			Status:  common.ChannelStatusEnabled,
		}).Error)
	}
}

func installTaskPollingAdaptor(t *testing.T, adaptor TaskPollingAdaptor) {
	t.Helper()
	previous := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previous })
}

func TestTaskPollingBudgetsUseTwentyMinuteDefault(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1_200
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })
	t.Setenv("TASK_POLLING_REQUEST_TIMEOUT_SECONDS", "")

	require.Equal(t, 20*time.Minute, taskPollingRequestTimeout())
	require.Equal(t, 20*time.Minute, taskPollingCycleTimeout())
	ctx, cancel := newTaskPollingCycleContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), 20*time.Minute)
	require.Greater(t, time.Until(deadline), 19*time.Minute)
}

func TestTaskPollingBudgetUsesSmallerRelayOrExplicitLimit(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })

	t.Run("relay budget wins", func(t *testing.T) {
		t.Setenv("TASK_POLLING_REQUEST_TIMEOUT_SECONDS", "1200")
		common.RelayNonStreamTimeout = 900
		require.Equal(t, 15*time.Minute, taskPollingRequestTimeout())
	})

	t.Run("explicit polling budget wins", func(t *testing.T) {
		t.Setenv("TASK_POLLING_REQUEST_TIMEOUT_SECONDS", "900")
		common.RelayNonStreamTimeout = 1200
		require.Equal(t, 15*time.Minute, taskPollingRequestTimeout())
	})
}
