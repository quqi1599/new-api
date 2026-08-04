package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configureRelayTransportForTest(t *testing.T) {
	t.Helper()

	previousRelayTimeout := common.RelayTimeout
	previousDial := common.RelayDialTimeout
	previousTLS := common.RelayTLSHandshakeTimeout
	previousHeader := common.RelayResponseHeaderTimeout
	previousExpect := common.RelayExpectContinueTimeout
	previousIdle := common.RelayIdleConnTimeout
	previousMaxIdle := common.RelayMaxIdleConns
	previousMaxIdlePerHost := common.RelayMaxIdleConnsPerHost
	t.Cleanup(func() {
		common.RelayTimeout = previousRelayTimeout
		common.RelayDialTimeout = previousDial
		common.RelayTLSHandshakeTimeout = previousTLS
		common.RelayResponseHeaderTimeout = previousHeader
		common.RelayExpectContinueTimeout = previousExpect
		common.RelayIdleConnTimeout = previousIdle
		common.RelayMaxIdleConns = previousMaxIdle
		common.RelayMaxIdleConnsPerHost = previousMaxIdlePerHost
		ResetProxyClientCache()
	})

	common.RelayTimeout = 600
	common.RelayDialTimeout = 7
	common.RelayTLSHandshakeTimeout = 8
	common.RelayResponseHeaderTimeout = 9
	common.RelayExpectContinueTimeout = 2
	common.RelayIdleConnTimeout = 90
	common.RelayMaxIdleConns = 200
	common.RelayMaxIdleConnsPerHost = 50
	ResetProxyClientCache()
}

func assertRelayTransportPolicy(t *testing.T, transport *http.Transport) {
	t.Helper()
	require.NotNil(t, transport)
	assert.Equal(t, 8*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(t, 9*time.Second, transport.ResponseHeaderTimeout)
	assert.Equal(t, 2*time.Second, transport.ExpectContinueTimeout)
	assert.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	assert.Equal(t, 200, transport.MaxIdleConns)
	assert.Equal(t, 50, transport.MaxIdleConnsPerHost)
	assert.True(t, transport.ForceAttemptHTTP2)
	assert.NotNil(t, transport.DialContext)
}

func TestRelayTransportUsesPhaseTimeoutsWithoutClientLifetime(t *testing.T) {
	configureRelayTransportForTest(t)

	transport := newRelayHTTPTransport(http.ProxyFromEnvironment, nil)
	assertRelayTransportPolicy(t, transport)
	client := newRelayHTTPClient(transport)
	assert.Zero(t, client.Timeout)
}

func TestExplicitProxyClientsShareRelayTransportPolicy(t *testing.T) {
	configureRelayTransportForTest(t)

	httpProxyClient, err := NewProxyHttpClient("http://127.0.0.1:3128")
	require.NoError(t, err)
	httpProxyTransport, ok := httpProxyClient.Transport.(*http.Transport)
	require.True(t, ok)
	assertRelayTransportPolicy(t, httpProxyTransport)
	assert.Zero(t, httpProxyClient.Timeout)
	proxyURL, err := httpProxyTransport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
	require.NoError(t, err)
	require.NotNil(t, proxyURL)
	assert.Equal(t, "http://127.0.0.1:3128", proxyURL.String())

	socksProxyClient, err := NewProxyHttpClient("socks5://127.0.0.1:1080")
	require.NoError(t, err)
	socksProxyTransport, ok := socksProxyClient.Transport.(*http.Transport)
	require.True(t, ok)
	assertRelayTransportPolicy(t, socksProxyTransport)
	assert.Zero(t, socksProxyClient.Timeout)
	assert.Nil(t, socksProxyTransport.Proxy)
}

func TestProxyClientCacheCanonicalizesAndInvalidates(t *testing.T) {
	configureRelayTransportForTest(t)

	first, err := GetHttpClientWithProxy("http://127.0.0.1:3128/")
	require.NoError(t, err)
	alias, err := GetHttpClientWithProxy("http://127.0.0.1:3128/legacy?ignored=1")
	require.NoError(t, err)
	require.Same(t, first, alias)

	InvalidateProxyClient("http://127.0.0.1:3128")
	after, err := GetHttpClientWithProxy("http://127.0.0.1:3128")
	require.NoError(t, err)
	require.NotSame(t, first, after)
}

func TestProxyClientCacheCreatesOneClientConcurrently(t *testing.T) {
	configureRelayTransportForTest(t)

	const workers = 32
	clients := make(chan *http.Client, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			client, err := GetHttpClientWithProxy("socks5://127.0.0.1:1080")
			clients <- client
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(clients)
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}
	var first *http.Client
	for client := range clients {
		if first == nil {
			first = client
			continue
		}
		require.Same(t, first, client)
	}
}

func TestRelayResponseHeaderTimeoutDoesNotBecomeBodyTimeout(t *testing.T) {
	configureRelayTransportForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(1100 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	transport := newRelayHTTPTransport(nil, nil)
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	client := newRelayHTTPClient(transport)
	started := time.Now()
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
	assert.GreaterOrEqual(t, time.Since(started), time.Second)
}

func TestRelayResponseHeaderTimeoutStopsHeaderStall(t *testing.T) {
	configureRelayTransportForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	transport := newRelayHTTPTransport(nil, nil)
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	client := newRelayHTTPClient(transport)
	started := time.Now()
	_, err := client.Get(server.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout awaiting response headers")
	assert.Less(t, time.Since(started), time.Second)
}

func TestRelayClientDoesNotFollowPostRedirect(t *testing.T) {
	var redirectedRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	client := newRelayHTTPClient(http.DefaultTransport)
	response, err := client.Post(entry.URL, "application/json", strings.NewReader(`{"model":"test"}`))
	require.NoError(t, err)
	defer response.Body.Close()

	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.Zero(t, redirectedRequests.Load(), "model POST redirects must not be replayed")
}

func TestSOCKSProxyDialHonorsRequestCancellation(t *testing.T) {
	configureRelayTransportForTest(t)
	common.RelayDialTimeout = 10

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		accepted <- connection
		_, _ = io.Copy(io.Discard, connection)
	}()

	client, err := NewProxyHttpClient("socks5://" + listener.Addr().String())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)

	started := time.Now()
	_, err = client.Do(request)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "unexpected cancellation error: %v", err)
	assert.Less(t, time.Since(started), time.Second)

	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("SOCKS test proxy did not accept a connection")
	}
}
