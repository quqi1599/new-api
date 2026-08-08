package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setRelayPreflightTimeoutsForTest(t *testing.T, nonStream, firstEvent int) {
	t.Helper()
	previousNonStream := common.RelayNonStreamTimeout
	previousFirstEvent := common.RelayFirstEventTotalTimeout
	common.RelayNonStreamTimeout = nonStream
	common.RelayFirstEventTotalTimeout = firstEvent
	t.Cleanup(func() {
		common.RelayNonStreamTimeout = previousNonStream
		common.RelayFirstEventTotalTimeout = previousFirstEvent
	})
}

func newPreflightGinContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	return c
}

func TestRelayPreflightUnsafeUploadUsesAbsoluteNonStreamBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setRelayPreflightTimeoutsForTest(t, 1, 1)

	requestReceived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-r.Context().Done()
	}))
	defer server.Close()

	c := newPreflightGinContext()
	info := &relaycommon.RelayInfo{
		StartTime:   time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	info.EnsureNonStreamDeadline(info.StartTime, 80*time.Millisecond)
	preflight := NewRelayPreflight(c, c.Request.Context(), info, true)
	defer preflight.Close()
	req, err := http.NewRequestWithContext(preflight.Context(), http.MethodPost, server.URL, nil)
	require.NoError(t, err)

	started := time.Now()
	_, err = preflight.Do(server.Client(), req)
	require.Error(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	select {
	case <-requestReceived:
	default:
		t.Fatal("upstream did not receive the upload request")
	}

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, "non_stream_total", common.GetContextKeyString(c, constant.ContextKeyRelayTimeoutPhase))
}

func TestRelayPreflightStreamUsesSharedFirstEventDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setRelayPreflightTimeoutsForTest(t, 1, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	c := newPreflightGinContext()
	info := &relaycommon.RelayInfo{
		StartTime:   time.Now(),
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	info.SetFirstValidEventDeadline(time.Now().Add(80 * time.Millisecond))
	preflight := NewRelayPreflight(c, c.Request.Context(), info, false)
	defer preflight.Close()
	req, err := http.NewRequestWithContext(preflight.Context(), http.MethodPost, server.URL, nil)
	require.NoError(t, err)

	_, err = preflight.Do(server.Client(), req)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, "first_valid_event_total", common.GetContextKeyString(c, constant.ContextKeyRelayTimeoutPhase))
	remaining, limited := info.RemainingFirstValidEventBudget()
	require.True(t, limited)
	require.LessOrEqual(t, remaining, time.Duration(0))
}

func TestRelayPreflightCallerCancellationWinsWithoutChannelPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setRelayPreflightTimeoutsForTest(t, 5, 5)

	requestReceived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-r.Context().Done()
	}))
	defer server.Close()

	caller, cancel := context.WithCancel(context.Background())
	c := newPreflightGinContext()
	c.Request = c.Request.WithContext(caller)
	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{}}
	preflight := NewRelayPreflight(c, caller, info, true)
	defer preflight.Close()
	req, err := http.NewRequestWithContext(preflight.Context(), http.MethodPost, server.URL, nil)
	require.NoError(t, err)

	go func() {
		<-requestReceived
		cancel()
	}()
	_, err = preflight.Do(server.Client(), req)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 499, apiErr.StatusCode)
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Equal(t, constant.RelayCancelOriginDownstreamDisconnected, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
}

type preflightTimeoutError struct{}

func (preflightTimeoutError) Error() string   { return "timeout" }
func (preflightTimeoutError) Timeout() bool   { return true }
func (preflightTimeoutError) Temporary() bool { return true }

func TestRelayPreflightSafeTokenTransportTimeoutRemainsRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setRelayPreflightTimeoutsForTest(t, 5, 5)
	c := newPreflightGinContext()
	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{}}
	preflight := NewRelayPreflight(c, c.Request.Context(), info, false)
	defer preflight.Close()

	apiErr := preflight.ClassifyError(preflightTimeoutError{})
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamConnectionTimeout, apiErr.GetErrorCode())
	require.False(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, info.UpstreamRequestMayHaveBeenAccepted())
}
