package ali

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpdateTaskBoundsResponseBodyRead(t *testing.T) {
	t.Setenv("ALI_TASK_POLL_REQUEST_TIMEOUT_SECONDS", "1")
	service.InitHttpClient()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	started := time.Now()
	_, err, _ := updateTask(context.Background(), &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL},
	}, "task-1")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second)
}

func TestAsyncTaskWaitStopsAfterConsecutivePollErrorsWithoutFakingTotalTimeout(t *testing.T) {
	service.InitHttpClient()
	oldInitialDelay := aliTaskInitialPollDelay
	oldPollInterval := aliTaskPollInterval
	oldMaxErrors := aliTaskMaxConsecutivePollErrors
	oldTotalTimeout := common.RelayNonStreamTimeout
	aliTaskInitialPollDelay = 0
	aliTaskPollInterval = time.Millisecond
	aliTaskMaxConsecutivePollErrors = 3
	common.RelayNonStreamTimeout = 5
	t.Cleanup(func() {
		aliTaskInitialPollDelay = oldInitialDelay
		aliTaskPollInterval = oldPollInterval
		aliTaskMaxConsecutivePollErrors = oldMaxErrors
		common.RelayNonStreamTimeout = oldTotalTimeout
	})

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("not-json"))
	}))
	t.Cleanup(upstream.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	info := &relaycommon.RelayInfo{
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: upstream.URL,
		},
	}

	_, _, err := asyncTaskWait(c, info, "task-1")

	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeBadResponse, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Equal(t, int32(3), requests.Load())
}

func TestAsyncTaskWaitProcessingStateRunsUntilTerminalOrAbsoluteBudget(t *testing.T) {
	service.InitHttpClient()
	oldInitialDelay := aliTaskInitialPollDelay
	oldPollInterval := aliTaskPollInterval
	oldMaxErrors := aliTaskMaxConsecutivePollErrors
	oldTotalTimeout := common.RelayNonStreamTimeout
	aliTaskInitialPollDelay = 0
	aliTaskPollInterval = time.Millisecond
	aliTaskMaxConsecutivePollErrors = 2
	common.RelayNonStreamTimeout = 5
	t.Cleanup(func() {
		aliTaskInitialPollDelay = oldInitialDelay
		aliTaskPollInterval = oldPollInterval
		aliTaskMaxConsecutivePollErrors = oldMaxErrors
		common.RelayNonStreamTimeout = oldTotalTimeout
	})

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		status := "PROCESSING"
		if attempt == 4 {
			status = "SUCCEEDED"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"task_status":"` + status + `"}}`))
	}))
	t.Cleanup(upstream.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	info := &relaycommon.RelayInfo{
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: upstream.URL,
		},
	}

	rsp, _, err := asyncTaskWait(c, info, "task-1")

	require.NoError(t, err)
	require.NotNil(t, rsp)
	require.Equal(t, "SUCCEEDED", rsp.Output.TaskStatus)
	require.Equal(t, int32(4), requests.Load())
}

func TestAsyncTaskWaitInheritsAlreadySpentNonStreamBudget(t *testing.T) {
	oldInitialDelay := aliTaskInitialPollDelay
	oldTotalTimeout := common.RelayNonStreamTimeout
	aliTaskInitialPollDelay = 0
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() {
		aliTaskInitialPollDelay = oldInitialDelay
		common.RelayNonStreamTimeout = oldTotalTimeout
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	info := &relaycommon.RelayInfo{
		StartTime:   time.Now().Add(-2 * time.Second),
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	startedAt := time.Now()
	_, _, err := asyncTaskWait(c, info, "task-1")

	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.Less(t, time.Since(startedAt), 250*time.Millisecond)
}
