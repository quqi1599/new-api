package xunfei

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type xunfeiReadResult struct {
	data []byte
	err  error
}

type xunfeiScriptedWebSocket struct {
	mu             sync.Mutex
	reads          []xunfeiReadResult
	readIndex      int
	readDeadlines  []time.Time
	writeDeadlines []time.Time
	closed         chan struct{}
	closeOnce      sync.Once
	wrote          chan struct{}
	wroteOnce      sync.Once
}

func newXunfeiScriptedWebSocket(reads ...xunfeiReadResult) *xunfeiScriptedWebSocket {
	return &xunfeiScriptedWebSocket{
		reads:  reads,
		closed: make(chan struct{}),
		wrote:  make(chan struct{}),
	}
}

func (c *xunfeiScriptedWebSocket) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	if c.readIndex < len(c.reads) {
		result := c.reads[c.readIndex]
		c.readIndex++
		c.mu.Unlock()
		return 1, result.data, result.err
	}
	c.mu.Unlock()
	<-c.closed
	return 0, nil, errors.New("fake websocket closed")
}

func (c *xunfeiScriptedWebSocket) WriteJSON(any) error {
	c.wroteOnce.Do(func() { close(c.wrote) })
	return nil
}

func (c *xunfeiScriptedWebSocket) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *xunfeiScriptedWebSocket) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *xunfeiScriptedWebSocket) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *xunfeiScriptedWebSocket) deadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.readDeadlines...)
}

func xunfeiTestDial(conn xunfeiWebSocket) xunfeiDialFunc {
	return func(context.Context, string) (xunfeiWebSocket, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols}, nil
	}
}

func newXunfeiHandlerTest(t *testing.T, ctx context.Context) (*gin.Context, *relaycommon.RelayInfo, *httptest.ResponseRecorder, dto.GeneralOpenAIRequest) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	info := &relaycommon.RelayInfo{StartTime: time.Now(), DisablePing: true}
	info.SetFirstValidEventDeadline(time.Now().Add(500 * time.Millisecond))
	request := dto.GeneralOpenAIRequest{Model: "SparkDesk-v1.1"}
	return c, info, recorder, request
}

func xunfeiFrame(status int, content string, promptTokens, completionTokens, totalTokens int) []byte {
	response := XunfeiChatResponse{}
	response.Header.Code = 0
	response.Payload.Choices.Status = status
	response.Payload.Choices.Text = []XunfeiChatResponseTextItem{{Content: content, Role: "assistant"}}
	response.Payload.Usage.Text = dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}
	data, _ := common.Marshal(response)
	return data
}

func TestXunfeiStreamExplicitTerminalFinalizes(t *testing.T) {
	conn := newXunfeiScriptedWebSocket(
		xunfeiReadResult{data: xunfeiFrame(0, "hello", 0, 0, 0)},
		xunfeiReadResult{data: xunfeiFrame(2, "", 3, 2, 5)},
	)
	c, info, recorder, request := newXunfeiHandlerTest(t, context.Background())

	usage, streamErr := xunfeiStreamHandlerWithDial(c, info, request, "app", "secret", "key", xunfeiTestDial(conn))

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, 5, usage.TotalTokens)
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Equal(t, 2, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))

	deadlines := conn.deadlines()
	require.Len(t, deadlines, 2)
	require.True(t, deadlines[1].After(deadlines[0].Add(10*time.Second)), "the second read must use the longer idle timeout")
}

func TestXunfeiStreamTruncatedAfterPartialDoesNotFinalize(t *testing.T) {
	conn := newXunfeiScriptedWebSocket(
		xunfeiReadResult{data: xunfeiFrame(0, "partial", 0, 0, 0)},
		xunfeiReadResult{err: io.EOF},
	)
	c, info, recorder, request := newXunfeiHandlerTest(t, context.Background())

	usage, streamErr := xunfeiStreamHandlerWithDial(c, info, request, "app", "secret", "key", xunfeiTestDial(conn))

	require.Nil(t, streamErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "data: [DONE]")
}

func TestXunfeiStreamClientCancelClosesWebSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := newXunfeiScriptedWebSocket()
	c, info, recorder, request := newXunfeiHandlerTest(t, ctx)
	done := make(chan struct{})
	var usage *dto.Usage
	var streamErr *types.NewAPIError
	go func() {
		defer close(done)
		usage, streamErr = xunfeiStreamHandlerWithDial(c, info, request, "app", "secret", "key", xunfeiTestDial(conn))
	}()

	select {
	case <-conn.wrote:
	case <-time.After(time.Second):
		t.Fatal("request was not written")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after client cancellation")
	}

	require.Nil(t, usage)
	require.NotNil(t, streamErr)
	require.Equal(t, 499, streamErr.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

func TestXunfeiStreamRejectsInvalidPreOutputResponses(t *testing.T) {
	tests := []struct {
		name string
		read xunfeiReadResult
		end  relaycommon.StreamEndReason
	}{
		{name: "zero frames", read: xunfeiReadResult{err: io.EOF}, end: relaycommon.StreamEndReasonEOF},
		{name: "bad JSON", read: xunfeiReadResult{data: []byte("{not-json")}, end: relaycommon.StreamEndReasonHandlerStop},
		{name: "business error", read: xunfeiReadResult{data: []byte(`{"header":{"code":1001,"message":"rejected"}}`)}, end: relaycommon.StreamEndReasonHandlerStop},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := newXunfeiScriptedWebSocket(tt.read)
			c, info, recorder, request := newXunfeiHandlerTest(t, context.Background())

			usage, streamErr := xunfeiStreamHandlerWithDial(c, info, request, "app", "secret", "key", xunfeiTestDial(conn))

			require.Nil(t, usage)
			require.NotNil(t, streamErr)
			require.Equal(t, http.StatusBadGateway, streamErr.StatusCode)
			require.Equal(t, tt.end, info.StreamStatus.EndReason)
			require.Zero(t, info.ReceivedResponseCount)
			require.Empty(t, recorder.Body.String())
			require.Empty(t, recorder.Header().Get("Content-Type"))
		})
	}
}

func TestXunfeiNonStreamTruncationReturnsErrorWithoutOutput(t *testing.T) {
	conn := newXunfeiScriptedWebSocket(
		xunfeiReadResult{data: xunfeiFrame(0, "partial", 0, 0, 0)},
		xunfeiReadResult{err: io.EOF},
	)
	c, info, recorder, request := newXunfeiHandlerTest(t, context.Background())

	usage, apiErr := xunfeiHandlerWithDial(c, info, request, "app", "secret", "key", xunfeiTestDial(conn))

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}
