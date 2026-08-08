package channel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	constant2 "github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayhelper "github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type contextTestAdaptor struct {
	Adaptor
	requestURL string
}

func (a *contextTestAdaptor) GetRequestURL(*relaycommon.RelayInfo) (string, error) {
	return a.requestURL, nil
}

func (a *contextTestAdaptor) SetupRequestHeader(*gin.Context, *http.Header, *relaycommon.RelayInfo) error {
	return nil
}

type contextTestTaskAdaptor struct {
	TaskAdaptor
	requestURL string
}

func (a *contextTestTaskAdaptor) BuildRequestURL(*relaycommon.RelayInfo) (string, error) {
	return a.requestURL, nil
}

func (a *contextTestTaskAdaptor) BuildRequestHeader(*gin.Context, *http.Request, *relaycommon.RelayInfo) error {
	return nil
}

type contextTestRequestResult struct {
	resp *http.Response
	err  error
}

type closeAwareHandshakeBody struct {
	closed bool
}

func (b *closeAwareHandshakeBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *closeAwareHandshakeBody) Close() error {
	b.closed = true
	return nil
}

func newContextTestGinContext(t *testing.T) (*gin.Context, context.CancelFunc, *httptest.ResponseRecorder) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "http://client.example/v1/chat/completions", strings.NewReader("{}"))
	c.Request = c.Request.WithContext(ctx)
	t.Cleanup(cancel)
	return c, cancel, recorder
}

func requireClientCanceledRequestError(t *testing.T, err error) *types.NewAPIError {
	t.Helper()

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, statusClientClosedRequest, apiErr.StatusCode)
	require.True(t, types.IsSkipRetryError(apiErr))

	previousAutoDisable := common2.AutomaticDisableChannelEnabled
	common2.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() {
		common2.AutomaticDisableChannelEnabled = previousAutoDisable
	})
	require.False(t, service.ShouldDisableChannel(apiErr))
	return apiErr
}

func TestDoApiRequestClientCancelBeforeResponseHeadersDoesNotCommitResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	requestStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, cancel, recorder := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	resultCh := make(chan contextTestRequestResult, 1)
	go func() {
		resp, err := DoApiRequest(&contextTestAdaptor{requestURL: upstream.URL}, c, info, strings.NewReader("{}"))
		resultCh <- contextTestRequestResult{resp: resp, err: err}
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}

	time.Sleep(60 * time.Millisecond)
	cancelStarted := time.Now()
	cancel()

	var result contextTestRequestResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("request did not return promptly after client cancellation")
	}
	if result.resp != nil {
		_ = result.resp.Body.Close()
	}
	require.Less(t, time.Since(cancelStarted), time.Second)
	requireClientCanceledRequestError(t, result.err)
	require.Equal(t, constant2.RelayCancelOriginDownstreamDisconnected, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.Empty(t, recorder.Body.String(), "waiting for upstream headers must not write gateway heartbeat bytes")
	require.False(t, recorder.Flushed, "waiting for upstream headers must not commit downstream HTTP 200")
}

func TestFormAndTaskRequestsCancelBeforeResponseHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	tests := []struct {
		name   string
		invoke func(*gin.Context, *relaycommon.RelayInfo, string) (*http.Response, error)
	}{
		{
			name: "form",
			invoke: func(c *gin.Context, info *relaycommon.RelayInfo, requestURL string) (*http.Response, error) {
				return DoFormRequest(&contextTestAdaptor{requestURL: requestURL}, c, info, strings.NewReader("field=value"))
			},
		},
		{
			name: "task",
			invoke: func(c *gin.Context, info *relaycommon.RelayInfo, requestURL string) (*http.Response, error) {
				return DoTaskApiRequest(&contextTestTaskAdaptor{requestURL: requestURL}, c, info, strings.NewReader("{}"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestStarted := make(chan struct{})
			releaseUpstream := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				select {
				case <-r.Context().Done():
				case <-releaseUpstream:
				}
			}))
			t.Cleanup(func() {
				close(releaseUpstream)
				upstream.Close()
			})

			c, cancel, _ := newContextTestGinContext(t)
			resultCh := make(chan contextTestRequestResult, 1)
			go func() {
				resp, err := test.invoke(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}, upstream.URL)
				resultCh <- contextTestRequestResult{resp: resp, err: err}
			}()

			select {
			case <-requestStarted:
			case <-time.After(time.Second):
				t.Fatal("upstream request did not start")
			}
			cancel()

			select {
			case result := <-resultCh:
				if result.resp != nil {
					_ = result.resp.Body.Close()
				}
				requireClientCanceledRequestError(t, result.err)
			case <-time.After(time.Second):
				t.Fatal("request did not return promptly after client cancellation")
			}
		})
	}
}

func TestDoWssRequestClientCancelDuringHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)

	requestStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, cancel, _ := newContextTestGinContext(t)
	requestURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	resultCh := make(chan error, 1)
	go func() {
		conn, err := DoWssRequest(
			&contextTestAdaptor{requestURL: requestURL},
			c,
			&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}},
			nil,
		)
		if conn != nil {
			_ = conn.Close()
		}
		resultCh <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("websocket handshake did not start")
	}
	cancel()

	select {
	case err := <-resultCh:
		requireClientCanceledRequestError(t, err)
	case <-time.After(time.Second):
		t.Fatal("websocket handshake did not stop after client cancellation")
	}
}

func TestHandleWebSocketHandshakeFailureClosesResponseBody(t *testing.T) {
	body := &closeAwareHandshakeBody{}
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: body}

	err := handleWebSocketHandshakeFailure(nil, "ws://upstream.example", resp, websocket.ErrBadHandshake)

	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.True(t, body.closed)
}

func TestDoWssRequestUsesRelayHandshakeTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	common2.RelayResponseHeaderTimeout = 1
	t.Cleanup(func() {
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
	})

	requestStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, _, _ := newContextTestGinContext(t)
	requestURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	startedAt := time.Now()
	conn, err := DoWssRequest(
		&contextTestAdaptor{requestURL: requestURL},
		c,
		&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}},
		nil,
	)
	if conn != nil {
		_ = conn.Close()
	}

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamResponseHeaderTimeout, apiErr.GetErrorCode())
	require.False(t, types.IsSkipRetryError(apiErr), "the model payload was not sent before the upgrade completed")
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Equal(t, constant2.RelayCancelOriginUpstreamTimeout, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.Equal(t, "response_headers", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	require.GreaterOrEqual(t, time.Since(startedAt), 900*time.Millisecond)
	require.Less(t, time.Since(startedAt), 3*time.Second)
}

func TestDialWebSocketContextZeroDisablesDefaultHandshakeTimeout(t *testing.T) {
	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	common2.RelayResponseHeaderTimeout = 0
	t.Cleanup(func() {
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
	})

	require.Zero(t, newRelayWebSocketDialer().HandshakeTimeout)
}

func TestRelayWebSocketDialerKeepsDedicatedDialTimeout(t *testing.T) {
	baseDial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	dial := withRelayWebSocketDialTimeout(baseDial, 100*time.Millisecond)
	dialStarted := time.Now()
	_, err := dial(context.Background(), "tcp", "upstream.invalid:443")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	var phaseErr *relayWebSocketPhaseTimeoutError
	require.ErrorAs(t, err, &phaseErr)
	require.Equal(t, "connect", phaseErr.phase)
	require.Equal(t, 1, phaseErr.timeoutSeconds)
	require.GreaterOrEqual(t, time.Since(dialStarted), 80*time.Millisecond)
	require.Less(t, time.Since(dialStarted), 500*time.Millisecond)
}

func TestRelayWebSocketTLSHandshakeKeepsDedicatedTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	serverRead := make(chan struct{})
	go func() {
		buffer := make([]byte, 4096)
		_, _ = serverConn.Read(buffer)
		close(serverRead)
	}()
	baseDial := func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}
	tlsDial := withRelayWebSocketTLSHandshakeTimeout(baseDial, nil, 100*time.Millisecond)
	startedAt := time.Now()
	_, err := tlsDial(context.Background(), "tcp", "upstream.example:443")

	require.Error(t, err)
	var phaseErr *relayWebSocketPhaseTimeoutError
	require.ErrorAs(t, err, &phaseErr)
	require.Equal(t, "tls_handshake", phaseErr.phase)
	require.Equal(t, 1, phaseErr.timeoutSeconds)
	require.GreaterOrEqual(t, time.Since(startedAt), 80*time.Millisecond)
	require.Less(t, time.Since(startedAt), 500*time.Millisecond)
	select {
	case <-serverRead:
	case <-time.After(time.Second):
		t.Fatal("TLS client did not start the handshake")
	}
}

func TestConfigureDirectRelayWebSocketTLS(t *testing.T) {
	dialer := newRelayWebSocketDialer()
	dialer.Proxy = nil

	configureDirectRelayWebSocketTLS(context.Background(), "wss://upstream.example/v1/realtime", &dialer)

	require.NotNil(t, dialer.NetDialTLSContext)
}

func TestDialWebSocketContextWSSBindsEstablishedConnectionToCaller(t *testing.T) {
	previousInsecure := common2.TLSInsecureSkipVerify
	common2.TLSInsecureSkipVerify = true
	t.Cleanup(func() { common2.TLSInsecureSkipVerify = previousInsecure })

	peerClosed := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			peerClosed <- err
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		peerClosed <- err
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithCancel(context.Background())
	requestURL := "wss" + strings.TrimPrefix(upstream.URL, "https")
	conn, _, err := DialWebSocketContext(ctx, requestURL, nil)
	require.NoError(t, err)
	require.NotNil(t, conn)
	defer conn.Close()

	cancel()
	select {
	case peerErr := <-peerClosed:
		require.Error(t, peerErr)
	case <-time.After(time.Second):
		t.Fatal("canceling the caller context did not close the established WSS connection")
	}
}

func TestClassifyWebSocketPhaseTimeouts(t *testing.T) {
	tests := []struct {
		name      string
		phase     string
		wantCode  types.ErrorCode
		wantPhase string
	}{
		{name: "connect", phase: "connect", wantCode: types.ErrorCodeUpstreamConnectionTimeout, wantPhase: "connect"},
		{name: "tls", phase: "tls_handshake", wantCode: types.ErrorCodeUpstreamTLSHandshakeTimeout, wantPhase: "tls_handshake"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _, _ := newContextTestGinContext(t)
			timeoutErr := &relayWebSocketPhaseTimeoutError{
				phase:          test.phase,
				timeoutSeconds: 1,
				err:            context.DeadlineExceeded,
			}

			apiErr := ClassifyWebSocketHandshakeError(c, timeoutErr)

			require.NotNil(t, apiErr)
			require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
			require.Equal(t, test.wantCode, apiErr.GetErrorCode())
			require.False(t, types.IsSkipRetryError(apiErr), "connect/TLS failure occurs before the model payload is sent")
			require.True(t, types.IsChannelPenaltyAllowed(apiErr), "retryable upstream transport failures still inform channel health")
			require.Equal(t, test.wantPhase, common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
			require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
		})
	}
}

func TestClassifyPreWriteTransportTimeoutsAsRetryable504(t *testing.T) {
	tests := []struct {
		phase    string
		wantCode types.ErrorCode
	}{
		{phase: "connect", wantCode: types.ErrorCodeUpstreamConnectionTimeout},
		{phase: "tls_handshake", wantCode: types.ErrorCodeUpstreamTLSHandshakeTimeout},
	}
	for _, test := range tests {
		t.Run(test.phase, func(t *testing.T) {
			c, _, _ := newContextTestGinContext(t)
			common2.SetContextKey(c, constant2.ContextKeyRelayTimeoutPhase, test.phase)

			apiErr := classifyDoRequestError(c, &relayWebSocketPhaseTimeoutError{
				phase:          test.phase,
				timeoutSeconds: 1,
				err:            context.DeadlineExceeded,
			}, false)

			require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
			require.Equal(t, test.wantCode, apiErr.GetErrorCode())
			require.False(t, types.IsSkipRetryError(apiErr), "nothing was written, so another channel remains safe")
			require.Equal(t, constant2.RelayCancelOriginUpstreamTimeout, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
		})
	}
}

func TestDoRequestClassifiesTLSHandshakeTimeoutBeforeRequestWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-serverDone
	})

	oldDialTimeout := common2.RelayDialTimeout
	oldTLSTimeout := common2.RelayTLSHandshakeTimeout
	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	oldNonStreamTimeout := common2.RelayNonStreamTimeout
	common2.RelayDialTimeout = 5
	common2.RelayTLSHandshakeTimeout = 1
	common2.RelayResponseHeaderTimeout = 5
	common2.RelayNonStreamTimeout = 0
	service.InitHttpClient()
	t.Cleanup(func() {
		common2.RelayDialTimeout = oldDialTimeout
		common2.RelayTLSHandshakeTimeout = oldTLSTimeout
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
		common2.RelayNonStreamTimeout = oldNonStreamTimeout
		service.InitHttpClient()
	})

	c, _, _ := newContextTestGinContext(t)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://"+listener.Addr().String(), strings.NewReader("{}"))
	require.NoError(t, err)
	startedAt := time.Now()

	resp, requestErr := DoRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})

	require.Nil(t, resp)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, requestErr, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamTLSHandshakeTimeout, apiErr.GetErrorCode())
	require.False(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, "tls_handshake", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	require.Less(t, time.Since(startedAt), 3*time.Second)
}

func TestDoWssRequestCallerDeadlineWinsWithoutChannelPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	common2.RelayResponseHeaderTimeout = 1
	t.Cleanup(func() {
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
	})

	requestStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "http://client.example/v1/realtime", strings.NewReader("{}"))
	c.Request = c.Request.WithContext(ctx)

	requestURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	conn, err := DoWssRequest(
		&contextTestAdaptor{requestURL: requestURL},
		c,
		&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}},
		nil,
	)
	if conn != nil {
		_ = conn.Close()
	}

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Equal(t, constant2.RelayCancelOriginGatewayDeadline, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
}

func TestClassifyDoRequestErrorKeepsInternalTimeoutRetryable(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	apiErr := ClassifyDoRequestError(c, context.DeadlineExceeded)

	require.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	require.False(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, constant2.RelayCancelOriginUpstreamTimeout, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
}

func TestClassifyDoRequestErrorStopsRetryAfterRequestMayHaveBeenSent(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	apiErr := classifyDoRequestError(c, context.DeadlineExceeded, true)

	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamResponseHeaderTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, constant2.RelayCancelOriginUpstreamTimeout, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.Equal(t, "response_headers", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, common2.RelayResponseHeaderTimeout, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
}

func TestClassifyDoRequestErrorDistinguishesRequestWriteTimeout(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	common2.SetContextKey(c, constant2.ContextKeyRelayTimeoutPhase, "request_write")
	apiErr := classifyDoRequestError(c, context.DeadlineExceeded, true)

	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamRequestWriteTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, "request_write", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
}

func TestDoApiRequestResponseHeaderTimeoutDoesNotReplayWrittenRequest(t *testing.T) {
	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	common2.RelayResponseHeaderTimeout = 1
	service.InitHttpClient()
	t.Cleanup(func() {
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
		service.InitHttpClient()
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	t.Cleanup(upstream.Close)

	c, _, _ := newContextTestGinContext(t)
	// Retry attempts share this Gin context. A prior channel may have left a
	// different transport phase behind; the new attempt must not reuse it.
	common2.SetContextKey(c, constant2.ContextKeyRelayTimeoutPhase, "tls_handshake")
	common2.SetContextKey(c, constant2.ContextKeyRelayTimeoutSeconds, 10)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	_, err := DoApiRequest(
		&contextTestAdaptor{requestURL: upstream.URL},
		c,
		info,
		strings.NewReader("{}"),
	)

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.True(t, types.IsSkipRetryError(apiErr), "a written request must not be replayed after a header timeout")
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamResponseHeaderTimeout, apiErr.GetErrorCode())
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, "response_headers", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
}

func TestDoApiRequestNonStreamBodyUsesTotalTimeout(t *testing.T) {
	service.InitHttpClient()
	oldTimeout := common2.RelayNonStreamTimeout
	common2.RelayNonStreamTimeout = 1
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldTimeout
	})

	bodyStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(bodyStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	started := time.Now()
	resp, err := DoApiRequest(
		&contextTestAdaptor{requestURL: upstream.URL},
		c,
		info,
		strings.NewReader("{}"),
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()

	select {
	case <-bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream did not flush response headers")
	}
	_, readErr := io.ReadAll(resp.Body)
	require.Error(t, readErr)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, readErr, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	wrappedErr := types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	require.Same(t, apiErr, wrappedErr, "provider wrappers must preserve the typed 504")
	require.Equal(t, http.StatusGatewayTimeout, wrappedErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, wrappedErr.GetErrorCode())
	require.Equal(t, constant2.RelayCancelOriginGatewayDeadline, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.Equal(t, "non_stream_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	require.Less(t, time.Since(started), 2*time.Second, "non-stream response body must not wait indefinitely after headers")
}

func TestDoRequestNonStreamTotalTimeoutBeforeHeadersKeepsItsPhase(t *testing.T) {
	oldHeaderTimeout := common2.RelayResponseHeaderTimeout
	oldNonStreamTimeout := common2.RelayNonStreamTimeout
	common2.RelayResponseHeaderTimeout = 0
	common2.RelayNonStreamTimeout = 1
	service.InitHttpClient()
	t.Cleanup(func() {
		common2.RelayResponseHeaderTimeout = oldHeaderTimeout
		common2.RelayNonStreamTimeout = oldNonStreamTimeout
		service.InitHttpClient()
	})

	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, err := DoRequest(c, req, info)
	require.Nil(t, resp)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, "non_stream_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	require.False(t, common2.GetContextKeyBool(c, constant2.ContextKeyRelayResponseHeaders))
}

func TestDoRequestDisarmsNonStreamTimerForDynamicSSE(t *testing.T) {
	oldTimeout := common2.RelayNonStreamTimeout
	common2.RelayNonStreamTimeout = 1
	service.InitHttpClient()
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldTimeout
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{IsStream: false, ChannelMeta: &relaycommon.ChannelMeta{}}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, err := DoRequest(c, req, info)
	require.NoError(t, err)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr, "an SSE response discovered from headers must not retain the non-stream total timer")
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "[DONE]")
	require.Empty(t, common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
}

func TestDoRequestDynamicSSEPreservesRequestWideFirstEventBudget(t *testing.T) {
	oldNonStreamTimeout := common2.RelayNonStreamTimeout
	oldFirstEventTotalTimeout := common2.RelayFirstEventTotalTimeout
	oldStreamingTimeout := constant2.StreamingTimeout
	common2.RelayNonStreamTimeout = 5
	common2.RelayFirstEventTotalTimeout = 1
	constant2.StreamingTimeout = 2
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldNonStreamTimeout
		common2.RelayFirstEventTotalTimeout = oldFirstEventTotalTimeout
		constant2.StreamingTimeout = oldStreamingTimeout
	})

	t.Run("header wait consumes shared budget", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(400 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		t.Cleanup(upstream.Close)

		c, _, _ := newContextTestGinContext(t)
		started := time.Now()
		info := &relaycommon.RelayInfo{StartTime: started, ChannelMeta: &relaycommon.ChannelMeta{}}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
		require.NoError(t, err)

		resp, err := DoRequest(c, req, info)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.True(t, info.IsStream)
		require.True(t, common2.GetContextKeyBool(c, constant2.ContextKeyIsStream))
		remaining, limited := info.RemainingFirstValidEventBudget()
		require.True(t, limited)
		require.Positive(t, remaining)
		require.Less(t, remaining, 750*time.Millisecond,
			"dynamic SSE must not restart the full first-event budget after response headers")
		require.NoError(t, resp.Body.Close())
	})

	t.Run("already exhausted budget returns typed 504", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		t.Cleanup(upstream.Close)

		c, _, _ := newContextTestGinContext(t)
		info := &relaycommon.RelayInfo{
			StartTime:   time.Now().Add(-2 * time.Second),
			ChannelMeta: &relaycommon.ChannelMeta{},
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
		require.NoError(t, err)

		resp, err := DoRequest(c, req, info)
		require.Nil(t, resp)
		var apiErr *types.NewAPIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
		require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
		require.True(t, types.IsSkipRetryError(apiErr))
		require.Equal(t, "first_valid_event_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
		require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	})

	t.Run("first event releases absolute deadline for healthy long stream", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(700 * time.Millisecond)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			w.(http.Flusher).Flush()
		}))
		t.Cleanup(upstream.Close)

		c, _, _ := newContextTestGinContext(t)
		info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{}}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
		require.NoError(t, err)

		resp, err := DoRequest(c, req, info)
		require.NoError(t, err)
		relayhelper.StreamScannerHandler(c, resp, info, func(data string, result *relayhelper.StreamResult) {
			if data == "[DONE]" {
				result.Done()
				return
			}
			result.Accept()
		})

		require.Greater(t, time.Since(info.StartTime), time.Second,
			"fixture must cross the absolute first-event deadline after accepting its first event")
		require.NotNil(t, info.StreamStatus)
		require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
		require.Empty(t, common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	})
}

func TestStreamingErrorBodyUsesRequestWideFirstEventBudget(t *testing.T) {
	service.InitHttpClient()
	oldFirstEventTotalTimeout := common2.RelayFirstEventTotalTimeout
	common2.RelayFirstEventTotalTimeout = 1
	t.Cleanup(func() {
		common2.RelayFirstEventTotalTimeout = oldFirstEventTotalTimeout
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	c, _, recorder := newContextTestGinContext(t)
	startedAt := time.Now().Add(-850 * time.Millisecond)
	info := &relaycommon.RelayInfo{
		StartTime:   startedAt,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	info.EnsureFirstValidEventDeadline(startedAt, time.Second)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, err := DoRequest(c, req, info)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	readStartedAt := time.Now()
	apiErr := service.RelayErrorHandler(c.Request.Context(), resp, false)
	require.NotNil(t, apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.Less(t, time.Since(readStartedAt), 750*time.Millisecond)
	require.Equal(t, "first_valid_event_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, constant2.RelayCancelOriginGatewayDeadline, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.False(t, recorder.Flushed)
	require.False(t, c.Writer.Written(), "the downstream status must remain uncommitted for the controller's 504")
	require.Empty(t, recorder.Body.String())
}

func TestContextCancelReadCloserDoesNotRewriteNaturalEOFAsTimeout(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	phaseTimer := &cancelPhaseTimer{}
	phaseTimer.state.Store(phaseTimerExpired)
	observerStopped := false
	reader := &contextCancelReadCloser{
		ReadCloser:     io.NopCloser(strings.NewReader("")),
		ginCtx:         c,
		nonStreamTimer: phaseTimer,
		stopNonStreamObserver: func() bool {
			observerStopped = true
			return true
		},
		nonStreamTimeout: 1,
	}

	_, err := reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
	require.True(t, observerStopped, "natural completion must unregister the non-stream deadline observer")
	require.Empty(t, common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
}

func TestDoRequestUsesLocalNonStreamBudgetBeforeLaterCallerDeadline(t *testing.T) {
	service.InitHttpClient()
	oldTimeout := common2.RelayNonStreamTimeout
	common2.RelayNonStreamTimeout = 1
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldTimeout
	})

	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, _, _ := newContextTestGinContext(t)
	callerCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(callerCtx, http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, err := DoRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
	require.NoError(t, err)
	defer resp.Body.Close()
	deadline, ok := resp.Request.Context().Deadline()
	require.True(t, ok)
	require.Greater(t, time.Until(deadline), 4*time.Second,
		"the local phase timer must not replace the caller's envelope deadline")

	_, readErr := io.ReadAll(resp.Body)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, readErr, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.Equal(t, "non_stream_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, 1, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
}

func TestDoRequestNonStreamBudgetDoesNotResetAfterEarlierWork(t *testing.T) {
	service.InitHttpClient()
	oldTimeout := common2.RelayNonStreamTimeout
	common2.RelayNonStreamTimeout = 1
	t.Cleanup(func() {
		common2.RelayNonStreamTimeout = oldTimeout
	})

	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{
		StartTime:   time.Now().Add(-750 * time.Millisecond),
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	startedAt := time.Now()

	resp, err := DoRequest(c, req, info)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, readErr := io.ReadAll(resp.Body)

	var apiErr *types.NewAPIError
	require.ErrorAs(t, readErr, &apiErr)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.Less(t, time.Since(startedAt), 750*time.Millisecond,
		"the final body read must inherit the request-wide budget instead of receiving a fresh second")
}

func TestDoRequestClassifiesCallerDeadlineAfterHeadersAsBodyTimeout(t *testing.T) {
	service.InitHttpClient()
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, _, _ := newContextTestGinContext(t)
	callerCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(callerCtx, http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := DoRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
	require.NoError(t, err)
	defer resp.Body.Close()

	_, readErr := io.ReadAll(resp.Body)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, readErr, &apiErr)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.True(t, common2.GetContextKeyBool(c, constant2.ContextKeyRelayResponseHeaders))
	require.Equal(t, "response_body_context", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
}

func TestDoRequestMarksUnsafeHTTPResponseAsPossiblyAccepted(t *testing.T) {
	service.InitHttpClient()
	for _, statusCode := range []int{http.StatusTemporaryRedirect, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			t.Cleanup(upstream.Close)

			c, _, _ := newContextTestGinContext(t)
			postInfo := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
			postReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
			require.NoError(t, err)
			resp, err := DoRequest(c, postReq, postInfo)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.True(t, postInfo.UpstreamRequestMayHaveBeenAccepted())

			getContext, _, _ := newContextTestGinContext(t)
			getInfo := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
			getReq, err := http.NewRequestWithContext(getContext.Request.Context(), http.MethodGet, upstream.URL, nil)
			require.NoError(t, err)
			resp, err = DoRequest(getContext, getReq, getInfo)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.False(t, getInfo.UpstreamRequestMayHaveBeenAccepted())
		})
	}
}

func TestDoRequestPropagatesValidatedRequestIDWithoutOverwritingAdaptorValue(t *testing.T) {
	service.InitHttpClient()
	type requestIDs struct {
		canonical string
		standard  string
	}
	seenIDs := make(chan requestIDs, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenIDs <- requestIDs{canonical: r.Header.Get(common2.RequestIdKey), standard: r.Header.Get("X-Request-Id")}
		w.Header().Set("X-Request-Id", "cpa-request-456")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	for _, test := range []struct {
		name       string
		preset     string
		wantHeader string
	}{
		{name: "canonical context id", wantHeader: "edge-trace-123"},
		{name: "adaptor value wins", preset: "provider-specific-id", wantHeader: "provider-specific-id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _, _ := newContextTestGinContext(t)
			c.Set(common2.RequestIdKey, "edge-trace-123")
			req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, upstream.URL, nil)
			require.NoError(t, err)
			if test.preset != "" {
				req.Header.Set(common2.RequestIdKey, test.preset)
			}

			resp, err := DoRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			seen := <-seenIDs
			require.Equal(t, test.wantHeader, seen.canonical)
			require.Equal(t, "edge-trace-123", seen.standard)
			require.Equal(t, "cpa-request-456", c.GetString(common2.UpstreamRequestIdKey))
		})
	}
}

func TestDoRequestSharedFirstEventBudgetBoundsResponseHeaders(t *testing.T) {
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	info.SetFirstValidEventDeadline(time.Now().Add(75 * time.Millisecond))
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	started := time.Now()
	resp, err := DoRequest(c, req, info)
	require.Nil(t, resp)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
	require.Equal(t, constant2.RelayCancelOriginGatewayDeadline, common2.GetContextKeyString(c, constant2.ContextKeyRelayCancelOrigin))
	require.Equal(t, "first_valid_event_total", common2.GetContextKeyString(c, constant2.ContextKeyRelayTimeoutPhase))
	require.Equal(t, common2.RelayFirstEventTotalTimeout, common2.GetContextKeyInt(c, constant2.ContextKeyRelayTimeoutSeconds))
	require.Less(t, time.Since(started), time.Second)
}

func TestDoRequestDisarmsSharedBudgetAfterResponseHeaders(t *testing.T) {
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(250 * time.Millisecond)
		_, _ = w.Write([]byte("event after the header-phase deadline"))
	}))
	t.Cleanup(upstream.Close)

	c, _, _ := newContextTestGinContext(t)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	info.SetFirstValidEventDeadline(time.Now().Add(100 * time.Millisecond))
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, err := DoRequest(c, req, info)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "the response-header timer must not become a whole-stream timeout")
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "event after the header-phase deadline", string(body))
}

func TestDoRequestCombinesAdaptorDeadlineWithInboundCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	requestStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	c, cancelInbound, _ := newContextTestGinContext(t)
	adaptorCtx, cancelAdaptor := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancelAdaptor)
	req, err := http.NewRequestWithContext(adaptorCtx, http.MethodPost, upstream.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resultCh := make(chan contextTestRequestResult, 1)
	go func() {
		resp, requestErr := DoRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
		resultCh <- contextTestRequestResult{resp: resp, err: requestErr}
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	cancelInbound()

	select {
	case result := <-resultCh:
		if result.resp != nil {
			_ = result.resp.Body.Close()
		}
		requireClientCanceledRequestError(t, result.err)
	case <-time.After(time.Second):
		t.Fatal("adaptor-owned context ignored inbound cancellation")
	}
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}
