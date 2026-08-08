package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type downloadRoundTripFunc func(*http.Request) (*http.Response, error)

func (f downloadRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type downloadTimeoutError struct{}

func (downloadTimeoutError) Error() string   { return "download timeout" }
func (downloadTimeoutError) Timeout() bool   { return true }
func (downloadTimeoutError) Temporary() bool { return true }

type downloadBlockingBody struct {
	ctx context.Context
}

func (b *downloadBlockingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *downloadBlockingBody) Close() error {
	return nil
}

func configureDownloadTestClient(t *testing.T, roundTripper http.RoundTripper) {
	t.Helper()
	fetchSetting := system_setting.GetFetchSetting()
	originalFetchSetting := *fetchSetting
	originalHTTPClient := httpClient
	originalProtectedClient := ssrfProtectedHTTPClient
	originalWorkerURL := system_setting.WorkerUrl
	originalWorkerKey := system_setting.WorkerValidKey
	originalWorkerAllowHTTP := system_setting.WorkerAllowHttpImageRequestEnabled
	originalMaxFileDownloadMB := constant.MaxFileDownloadMB
	t.Cleanup(func() {
		*fetchSetting = originalFetchSetting
		httpClient = originalHTTPClient
		ssrfProtectedHTTPClient = originalProtectedClient
		system_setting.WorkerUrl = originalWorkerURL
		system_setting.WorkerValidKey = originalWorkerKey
		system_setting.WorkerAllowHttpImageRequestEnabled = originalWorkerAllowHTTP
		constant.MaxFileDownloadMB = originalMaxFileDownloadMB
	})

	fetchSetting.EnableSSRFProtection = false
	system_setting.WorkerUrl = ""
	constant.MaxFileDownloadMB = 1
	httpClient = &http.Client{Transport: roundTripper}
	ssrfProtectedHTTPClient = httpClient
}

func setDownloadTimeoutsForTest(t *testing.T, nonStream, firstEvent int) {
	t.Helper()
	originalNonStream := common.RelayNonStreamTimeout
	originalFirstEvent := common.RelayFirstEventTotalTimeout
	t.Cleanup(func() {
		common.RelayNonStreamTimeout = originalNonStream
		common.RelayFirstEventTotalTimeout = originalFirstEvent
	})
	common.RelayNonStreamTimeout = nonStream
	common.RelayFirstEventTotalTimeout = firstEvent
}

func newDownloadGinContext(ctx context.Context, startTime time.Time, isStream bool) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request = request.WithContext(ctx)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, startTime)
	common.SetContextKey(c, constant.ContextKeyIsStream, isStream)
	return c
}

func imageResponse(req *http.Request, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"image/png"},
		},
		Body:    body,
		Request: req,
	}
}

func TestRelayImageDownloadCallerCancellationStopsBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 5, 5)
	responseReady := make(chan struct{})
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(responseReady)
		return imageResponse(req, &downloadBlockingBody{ctx: req.Context()}), nil
	}))

	caller, cancel := context.WithCancel(context.Background())
	c := newDownloadGinContext(caller, time.Now(), false)
	info := &relaycommon.RelayInfo{StartTime: time.Now()}
	go func() {
		<-responseReady
		cancel()
	}()

	_, _, err := GetImageFromURLWithRelayInfo(c, info, "https://asset.example/image.png")
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 499, apiErr.StatusCode)
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Equal(t, constant.RelayCancelOriginDownstreamDisconnected, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
}

func TestRelayImageDownloadUsesElapsedAbsoluteBudgetForBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 1, 1)
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return imageResponse(req, &downloadBlockingBody{ctx: req.Context()}), nil
	}))

	startTime := time.Now().Add(-700 * time.Millisecond)
	c := newDownloadGinContext(context.Background(), startTime, false)
	info := &relaycommon.RelayInfo{StartTime: startTime}
	started := time.Now()
	_, _, err := GetImageFromURLWithRelayInfo(c, info, "https://asset.example/image.png")

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, ShouldCountChannelCircuitFailure(apiErr))
	require.Less(t, time.Since(started), 650*time.Millisecond, "download received a fresh timeout instead of the remaining absolute budget")
	require.Equal(t, "non_stream_total", common.GetContextKeyString(c, constant.ContextKeyRelayTimeoutPhase))
}

func TestRelayWorkerDownloadBodyUsesElapsedAbsoluteBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 1, 1)
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		return imageResponse(req, &downloadBlockingBody{ctx: req.Context()}), nil
	}))
	system_setting.WorkerUrl = "https://worker.example"
	system_setting.WorkerValidKey = "test-key"

	startTime := time.Now().Add(-700 * time.Millisecond)
	c := newDownloadGinContext(context.Background(), startTime, false)
	info := &relaycommon.RelayInfo{StartTime: startTime}
	started := time.Now()
	_, _, err := GetImageFromURLWithRelayInfo(c, info, "https://asset.example/image.png")

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, ShouldCountChannelCircuitFailure(apiErr))
	require.Less(t, time.Since(started), 650*time.Millisecond)
}

func TestRelayDownloadTransportTimeoutDoesNotPenalizeModelChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 5, 5)
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, downloadTimeoutError{}
	}))

	c := newDownloadGinContext(context.Background(), time.Now(), false)
	info := &relaycommon.RelayInfo{StartTime: time.Now()}
	_, err := DoDownloadRequestWithRelayInfo(c, info, "https://asset.example/image.png")

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamConnectionTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, ShouldCountChannelCircuitFailure(apiErr))
}

func TestGinRelayDownloadsShareAbsoluteDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 2, 2)

	for _, testCase := range []struct {
		name     string
		isStream bool
	}{
		{name: "non_stream", isStream: false},
		{name: "stream_first_event", isStream: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var deadlines []time.Time
			configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				require.True(t, ok)
				deadlines = append(deadlines, deadline)
				return imageResponse(req, io.NopCloser(strings.NewReader("image-bytes"))), nil
			}))

			startTime := time.Now().Add(-time.Second)
			c := newDownloadGinContext(context.Background(), startTime, testCase.isStream)
			_, _, err := GetImageFromURLWithRelayInfo(c, nil, "https://asset.example/first.png")
			require.NoError(t, err)
			time.Sleep(30 * time.Millisecond)
			_, _, err = GetImageFromURLWithRelayInfo(c, nil, "https://asset.example/second.png")
			require.NoError(t, err)

			require.Len(t, deadlines, 2)
			expectedDeadline := startTime.Add(2 * time.Second)
			require.WithinDuration(t, expectedDeadline, deadlines[0], 10*time.Millisecond)
			require.WithinDuration(t, deadlines[0], deadlines[1], 10*time.Millisecond, "a later download must not receive a reset budget")
			require.Less(t, time.Until(deadlines[1]), 1100*time.Millisecond)
		})
	}
}

func TestDoWorkerRequestContextHonorsCallerCancellation(t *testing.T) {
	requestStarted := make(chan struct{})
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		close(requestStarted)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	system_setting.WorkerUrl = "https://worker.example"
	system_setting.WorkerValidKey = "test-key"

	caller, cancel := context.WithCancel(context.Background())
	go func() {
		<-requestStarted
		cancel()
	}()
	_, err := DoWorkerRequestContext(caller, &WorkerRequest{URL: "https://asset.example/image.png"})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestEstimateRequestTokenPreservesRelayDownloadTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setDownloadTimeoutsForTest(t, 1, 1)
	configureDownloadTestClient(t, downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return imageResponse(req, &downloadBlockingBody{ctx: req.Context()}), nil
	}))
	originalCountToken := constant.CountToken
	originalGetMediaToken := constant.GetMediaToken
	originalGetMediaTokenNotStream := constant.GetMediaTokenNotStream
	t.Cleanup(func() {
		constant.CountToken = originalCountToken
		constant.GetMediaToken = originalGetMediaToken
		constant.GetMediaTokenNotStream = originalGetMediaTokenNotStream
	})
	constant.CountToken = true
	constant.GetMediaToken = true
	constant.GetMediaTokenNotStream = true

	startTime := time.Now().Add(-700 * time.Millisecond)
	c := newDownloadGinContext(context.Background(), startTime, false)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
	info := &relaycommon.RelayInfo{
		StartTime:   startTime,
		RelayFormat: types.RelayFormatOpenAI,
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTextNumber,
		Files: []*types.FileMeta{
			{Source: types.NewURLFileSource("https://asset.example/image.png")},
		},
	}

	_, err := EstimateRequestToken(c, meta, info)
	require.Error(t, err)
	controllerWrapped := types.NewError(err, types.ErrorCodeCountTokenFailed)
	require.Equal(t, http.StatusGatewayTimeout, controllerWrapped.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, controllerWrapped.GetErrorCode())
	require.True(t, types.IsSkipRetryError(controllerWrapped))
	require.False(t, types.IsChannelPenaltyAllowed(controllerWrapped))
	require.False(t, ShouldCountChannelCircuitFailure(controllerWrapped))
}
