package coze

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type blockingCozeBody struct {
	closed chan struct{}
	once   sync.Once
}

type errorCozeBody struct {
	err    error
	closed bool
}

func (b *errorCozeBody) Read([]byte) (int, error) { return 0, b.err }
func (b *errorCozeBody) Close() error {
	b.closed = true
	return nil
}

func newBlockingCozeBody() *blockingCozeBody {
	return &blockingCozeBody{closed: make(chan struct{})}
}

func (b *blockingCozeBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingCozeBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestReadCozeResponseBodyStopsAtMultiStepBudget(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	budget, err := newCozeRequestBudgetWithTimeout(c, 35*time.Millisecond, 1)
	require.NoError(t, err)
	defer budget.finish()
	body := newBlockingCozeBody()
	resp := &http.Response{StatusCode: http.StatusOK, Body: body}

	started := time.Now()
	_, readErr := readCozeResponseBody(c, resp, budget)

	require.NotNil(t, readErr)
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, http.StatusGatewayTimeout, readErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, readErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(readErr))
	require.True(t, types.IsChannelPenaltyAllowed(readErr))
	require.Equal(t, constant.RelayCancelOriginGatewayDeadline, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
	require.Equal(t, "non_stream_total", common.GetContextKeyString(c, constant.ContextKeyRelayTimeoutPhase))
}

func TestCozeChatHandlerClosesBodyWhenReadFails(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := &errorCozeBody{err: errors.New("read failed")}
	resp := &http.Response{StatusCode: http.StatusOK, Body: body}

	usage, apiErr := cozeChatHandler(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	require.True(t, body.closed)
}

func TestCozeMultiStepBudgetUsesOriginalRequestStart(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{StartTime: time.Now().Add(-750 * time.Millisecond)}
	budget, err := newCozeRequestBudget(c, info)
	require.NoError(t, err)
	defer budget.finish()
	body := newBlockingCozeBody()
	resp := &http.Response{StatusCode: http.StatusOK, Body: body}

	started := time.Now()
	_, readErr := readCozeResponseBody(c, resp, budget)

	require.NotNil(t, readErr)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, readErr.GetErrorCode())
}

func TestCozeMultiStepBudgetRejectsAlreadyExhaustedRequest(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{StartTime: time.Now().Add(-2 * time.Second)}

	budget, err := newCozeRequestBudget(c, info)

	require.Nil(t, budget)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
}

func TestCozeMultiStepBudgetZeroDisablesLocalBudget(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 0
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{StartTime: time.Now().Add(-time.Hour)}

	budget, err := newCozeRequestBudget(c, info)
	require.NoError(t, err)
	defer budget.finish()

	require.Zero(t, budget.timeoutSeconds)
	require.Nil(t, budget.timer)
	_, limited := info.RemainingNonStreamBudget()
	require.False(t, limited)
	select {
	case <-budget.ctx.Done():
		t.Fatal("disabled Coze non-stream budget canceled the multi-step context")
	default:
	}
}

func TestClassifyCozeStageErrorMapsHeaderTimeoutToTyped504(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	budget, err := newCozeRequestBudgetWithTimeout(c, time.Second, 1)
	require.NoError(t, err)
	defer budget.finish()

	stageErr := classifyCozeStageError(c, budget, context.DeadlineExceeded, "response_headers")

	require.Equal(t, http.StatusGatewayTimeout, stageErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamResponseHeaderTimeout, stageErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(stageErr))
	require.True(t, types.IsChannelPenaltyAllowed(stageErr))
	require.Equal(t, constant.RelayCancelOriginUpstreamTimeout, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
	require.Equal(t, "response_headers", common.GetContextKeyString(c, constant.ContextKeyRelayTimeoutPhase))
}

func TestCozeCallerCancellationRemains499WithoutPenalty(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	parent, cancelParent := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(parent)
	budget, err := newCozeRequestBudgetWithTimeout(c, time.Second, 1)
	require.NoError(t, err)
	defer budget.finish()
	cancelParent()
	<-budget.ctx.Done()

	stageErr := classifyCozeStageError(c, budget, budget.ctx.Err(), "response_headers")

	require.Equal(t, 499, stageErr.StatusCode)
	require.True(t, types.IsSkipRetryError(stageErr))
	require.False(t, types.IsChannelPenaltyAllowed(stageErr))
	require.Equal(t, constant.RelayCancelOriginDownstreamDisconnected, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
}
