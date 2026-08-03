package channel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
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

func TestDoApiRequestClientCancelBeforeResponseHeadersDoesNotCommitHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	generalSetting := operation_setting.GetGeneralSetting()
	previousPingEnabled := generalSetting.PingIntervalEnabled
	previousPingSeconds := generalSetting.PingIntervalSeconds
	generalSetting.PingIntervalEnabled = true
	generalSetting.PingIntervalSeconds = 1
	t.Cleanup(func() {
		generalSetting.PingIntervalEnabled = previousPingEnabled
		generalSetting.PingIntervalSeconds = previousPingSeconds
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

	// The former pre-header pinger fired after one second and committed a 200.
	time.Sleep(1100 * time.Millisecond)
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
	require.Empty(t, recorder.Body.String())
	require.False(t, recorder.Flushed)
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

func TestClassifyDoRequestErrorKeepsInternalTimeoutRetryable(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	apiErr := ClassifyDoRequestError(c, context.DeadlineExceeded)

	require.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	require.False(t, types.IsSkipRetryError(apiErr))
}

func TestClassifyDoRequestErrorStopsRetryAfterRequestMayHaveBeenSent(t *testing.T) {
	c, _, _ := newContextTestGinContext(t)
	apiErr := classifyDoRequestError(c, context.DeadlineExceeded, true)

	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
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
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, apiErr.GetErrorCode())
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
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
	seenIDs := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenIDs <- r.Header.Get(common2.RequestIdKey)
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
			require.Equal(t, test.wantHeader, <-seenIDs)
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
