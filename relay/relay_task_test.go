package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type taskTimeoutReadCloser struct {
	err    error
	closed bool
}

func (b *taskTimeoutReadCloser) Read([]byte) (int, error) { return 0, b.err }
func (b *taskTimeoutReadCloser) Close() error {
	b.closed = true
	return nil
}

type realtimeTaskTestAdaptor struct {
	stage   string
	timeout error
}

func (a *realtimeTaskTestAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *realtimeTaskTestAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, errors.New("legacy realtime fetch must not be used")
}
func (a *realtimeTaskTestAdaptor) FetchTaskContext(context.Context, string, string, map[string]any, string) (*http.Response, error) {
	if a.stage == "fetch" {
		return nil, a.timeout
	}
	if a.stage == "read" {
		return &http.Response{StatusCode: http.StatusOK, Body: &taskTimeoutReadCloser{err: a.timeout}}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}
func (a *realtimeTaskTestAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}
func (a *realtimeTaskTestAdaptor) ParseTaskResultContext(context.Context, []byte) (*relaycommon.TaskInfo, error) {
	if a.stage == "parse" {
		return nil, a.timeout
	}
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}
func (a *realtimeTaskTestAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

func TestTaskErrorFromDoRequestPreservesClientCancellation(t *testing.T) {
	apiErr := types.NewErrorWithStatusCode(
		context.Canceled,
		types.ErrorCodeDoRequestFailed,
		499,
		types.ErrOptionWithSkipRetry(),
	)

	taskErr := taskErrorFromDoRequest(fmt.Errorf("provider request failed: %w", apiErr))

	require.Equal(t, 499, taskErr.StatusCode)
	require.Equal(t, string(types.ErrorCodeDoRequestFailed), taskErr.Code)
	require.True(t, taskErr.LocalError)
	require.True(t, taskErr.SkipRetry)
	require.ErrorIs(t, taskErr.Error, context.Canceled)
}

func TestTaskErrorFromDoRequestSeparatesNoReplayFromChannelHealth(t *testing.T) {
	apiErr := types.NewErrorWithStatusCode(
		context.DeadlineExceeded,
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)

	taskErr := taskErrorFromDoRequest(apiErr)

	require.True(t, taskErr.SkipRetry)
	require.False(t, taskErr.LocalError)
}

func TestTaskErrorFromDoRequestKeepsUpstreamFailureRetryable(t *testing.T) {
	taskErr := taskErrorFromDoRequest(fmt.Errorf("upstream unavailable"))

	require.Equal(t, http.StatusInternalServerError, taskErr.StatusCode)
	require.Equal(t, "do_request_failed", taskErr.Code)
	require.False(t, taskErr.LocalError)
}

func TestTaskErrorFromUpstreamResponsePreservesTypedReadTimeout(t *testing.T) {
	typedTimeout := types.NewErrorWithStatusCode(
		context.DeadlineExceeded,
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
	)
	body := &taskTimeoutReadCloser{err: typedTimeout}

	taskErr := taskErrorFromUpstreamResponse(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       body,
	})

	require.True(t, body.closed)
	require.Equal(t, http.StatusGatewayTimeout, taskErr.StatusCode)
	require.Equal(t, string(types.ErrorCodeUpstreamNonStreamTimeout), taskErr.Code)
	require.ErrorIs(t, taskErr.Error, context.DeadlineExceeded)
}

func TestFetchRealtimeTaskResultNeverDowngradesTypedTimeout(t *testing.T) {
	stages := []struct {
		name string
		code types.ErrorCode
	}{
		{name: "fetch", code: types.ErrorCodeUpstreamResponseHeaderTimeout},
		{name: "read", code: types.ErrorCodeUpstreamNonStreamTimeout},
		{name: "parse", code: types.ErrorCodeUpstreamNonStreamTimeout},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			typedTimeout := types.NewErrorWithStatusCode(
				context.DeadlineExceeded,
				stage.code,
				http.StatusGatewayTimeout,
				types.ErrOptionWithSkipRetry(),
			)
			adaptor := &realtimeTaskTestAdaptor{stage: stage.name, timeout: typedTimeout}

			_, _, err := fetchRealtimeTaskResult(context.Background(), adaptor, "https://example.invalid", "key", nil, "")
			taskErr := service.TaskErrorWrapper(err, "fetch_realtime_task_failed", http.StatusBadGateway)

			require.Error(t, err)
			require.Equal(t, http.StatusGatewayTimeout, taskErr.StatusCode)
			require.Equal(t, string(stage.code), taskErr.Code)
			require.ErrorIs(t, taskErr.Error, context.DeadlineExceeded)
		})
	}
}

func TestVideoFetchByIDRealtimeTimeoutDoesNotFallbackToCachedSuccess(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "realtime-task.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Task{}))
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldGetAdaptor := getRealtimeTaskPollingAdaptor
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		getRealtimeTaskPollingAdaptor = oldGetAdaptor
		model.DB, model.LOG_DB = oldDB, oldLogDB
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	testCases := []struct {
		name        string
		channelType int
		stage       string
		code        types.ErrorCode
	}{
		{name: "gemini_fetch", channelType: constant.ChannelTypeGemini, stage: "fetch", code: types.ErrorCodeUpstreamResponseHeaderTimeout},
		{name: "gemini_read", channelType: constant.ChannelTypeGemini, stage: "read", code: types.ErrorCodeUpstreamNonStreamTimeout},
		{name: "vertex_fetch", channelType: constant.ChannelTypeVertexAi, stage: "fetch", code: types.ErrorCodeUpstreamResponseHeaderTimeout},
		{name: "vertex_read", channelType: constant.ChannelTypeVertexAi, stage: "read", code: types.ErrorCodeUpstreamNonStreamTimeout},
	}

	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			channelID := 930000 + index
			userID := 730000 + index
			publicTaskID := "public-" + testCase.name
			baseURL := "https://realtime.example.invalid"
			require.NoError(t, db.Create(&model.Channel{
				Id:      channelID,
				Type:    testCase.channelType,
				Key:     "test-key",
				Name:    testCase.name,
				Status:  common.ChannelStatusEnabled,
				BaseURL: &baseURL,
			}).Error)
			require.NoError(t, db.Create(&model.Task{
				TaskID:    publicTaskID,
				Platform:  constant.TaskPlatform(strconv.Itoa(testCase.channelType)),
				UserId:    userID,
				ChannelId: channelID,
				Status:    model.TaskStatusInProgress,
				PrivateData: model.TaskPrivateData{
					UpstreamTaskID: "upstream-" + testCase.name,
				},
			}).Error)

			typedTimeout := types.NewErrorWithStatusCode(
				context.DeadlineExceeded,
				testCase.code,
				http.StatusGatewayTimeout,
				types.ErrOptionWithSkipRetry(),
			)
			getRealtimeTaskPollingAdaptor = func(constant.TaskPlatform) service.TaskPollingAdaptor {
				return &realtimeTaskTestAdaptor{stage: testCase.stage, timeout: typedTimeout}
			}

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+publicTaskID, nil)
			c.Params = gin.Params{{Key: "task_id", Value: publicTaskID}}
			c.Set("id", userID)

			respBody, taskErr := videoFetchByIDRespBodyBuilder(c)

			require.Empty(t, respBody)
			require.NotNil(t, taskErr)
			require.Equal(t, http.StatusGatewayTimeout, taskErr.StatusCode)
			require.Equal(t, string(testCase.code), taskErr.Code)
			require.ErrorIs(t, taskErr.Error, context.DeadlineExceeded)
		})
	}
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}
