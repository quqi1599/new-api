package controller

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

// Run the production-baseline NewAPI Relay and selector against an actual legacy
// CPA process. All credentials, requests, and upstreams are synthetic and local.
func TestLegacyCPARetryWithRealGateway(t *testing.T) {
	binary := os.Getenv("LEGACY_CPA_RETRY_TEST_BINARY")
	if binary == "" {
		t.Skip("set LEGACY_CPA_RETRY_TEST_BINARY to a locally built legacy CPA binary")
	}
	require.True(t, filepath.IsAbs(binary))
	for _, claude := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []struct {
				name      string
				status    int
				keys      int
				barrier   bool
				strip     bool
				noConnect bool
				noHeaders bool
				truncated bool
			}{
				{name: "explicit_429", status: 429, keys: 2},
				{name: "explicit_503", status: 503, keys: 2},
				{name: "connection_refused", keys: 2, noConnect: true},
				{name: "written_request_without_headers", keys: 1, noHeaders: true, barrier: true},
				{name: "accepted_headers_truncated_body", keys: 1, truncated: true, barrier: true},
				{name: "negative_control_headers_stripped", status: 503, keys: 2, strip: true},
			} {
				t.Run(fmt.Sprintf("claude=%t/stream=%t/%s", claude, stream, scenario.name), func(t *testing.T) {
					var upstreamCalls atomic.Int32
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						upstreamCalls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						if scenario.noHeaders || scenario.truncated {
							connection, buffer, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Errorf("hijack synthetic upstream: %v", err)
								return
							}
							if scenario.truncated {
								_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n")
								_ = buffer.Flush()
							}
							_ = connection.Close()
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(scenario.status)
						_, _ = io.WriteString(w, `{"error":{"type":"server_error","code":"synthetic_rejection","message":"synthetic upstream rejection"}}`)
					}))
					t.Cleanup(origin.Close)
					upstreamURL := origin.URL
					if scenario.noConnect {
						origin.Close()
					}
					base := startLegacyRetryCPA(t, binary, upstreamURL, scenario.keys)
					target, err := url.Parse(base)
					require.NoError(t, err)
					proxy := httputil.NewSingleHostReverseProxy(target)
					var headersMu sync.Mutex
					var scopes, attempts []string
					proxy.ModifyResponse = func(response *http.Response) error {
						headersMu.Lock()
						scopes = append(scopes, response.Header.Get("X-CPA-Retry-Scope"))
						attempts = append(attempts, response.Header.Get("X-CPA-Retry-Attempts"))
						headersMu.Unlock()
						if scenario.strip {
							response.Header.Del("X-CPA-Retry-Scope")
							response.Header.Del("X-CPA-Retry-Attempts")
							response.Header.Del("X-CPA-Retry-Reason")
						}
						return nil
					}
					got := runCPAStateRelay(t, "synthetic_rejection", scenario.status, true, stream, cpaStateAliasOptions{
						claude: claude, ownerHandler: proxy, maxAttempts: 6,
						cached: stream, ownerHeaderOverride: `{"*":true}`, trustedCPA: true,
					})
					headersMu.Lock()
					defer headersMu.Unlock()
					t.Logf("owner=%d independent=%d upstream=%d scopes=%v attempts=%v status=%d", got.ownerCalls, got.alternateCalls, upstreamCalls.Load(), scopes, attempts, got.response.Code)
					if scenario.barrier {
						require.EqualValues(t, 1, got.ownerCalls)
						require.Zero(t, got.alternateCalls, "an accepted or possibly accepted request must not reach another provider")
						// The legacy GPT-specific policy permits two internal rounds
						// for transport failures even when request-retry is zero.
						// This patch reports both calls and stops outer replay only.
						require.EqualValues(t, 2, upstreamCalls.Load())
						require.Equal(t, []string{"2"}, attempts)
						require.Equal(t, []string{"request"}, scopes)
						require.NotEmpty(t, common.GetContextKeyString(got.context, constant.ContextKeyRelayGatewayNoReplayReason))
						require.NotEqual(t, http.StatusOK, got.response.Code, got.response.Body.String())
						require.NotContains(t, got.response.Body.String(), "[DONE]")
						return
					}
					require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
					require.EqualValues(t, 1, got.alternateCalls, "a known rejection or pre-connect failure can use an independent provider")
					require.Empty(t, common.GetContextKeyString(got.context, constant.ContextKeyRelayGatewayNoReplayReason))
					if scenario.strip {
						require.Greater(t, got.ownerCalls, int32(1), "negative control must expose repeated entrance traversal")
						require.Greater(t, upstreamCalls.Load(), int32(scenario.keys))
					} else {
						require.EqualValues(t, 1, got.ownerCalls)
						require.Equal(t, []string{"gateway"}, scopes)
						require.Equal(t, []string{fmt.Sprint(scenario.keys)}, attempts)
						if scenario.noConnect {
							require.Zero(t, upstreamCalls.Load())
						} else {
							require.EqualValues(t, scenario.keys, upstreamCalls.Load())
						}
					}
				})
			}
		}
	}
}

func startLegacyRetryCPA(t *testing.T, binary, upstream string, keys int) string {
	t.Helper()
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().(*net.TCPAddr)
	port := address.Port
	require.NoError(t, listener.Close())
	credentials := make([]map[string]any, 0, keys)
	for i := range keys {
		credentials = append(credentials, map[string]any{
			"api-key": fmt.Sprintf("synthetic-upstream-%d", i), "base-url": upstream,
			"native-responses": true, "proxy-url": "direct", "disable-cooling": true,
			"models": []map[string]string{{"name": "gpt-6.1-sol", "alias": "gpt-6.1-sol"}},
		})
	}
	config := map[string]any{
		"host": "127.0.0.1", "port": port, "auth-dir": filepath.Join(dir, "auth"),
		"api-keys": []string{"synthetic-key"}, "codex-api-key": credentials,
		"request-retry": 0, "max-retry-credentials": keys, "disable-cooling": true,
		"content-audit":     map[string]any{"enabled": false},
		"remote-management": map[string]any{"disable-control-panel": true, "disable-auto-update-panel": true},
	}
	data, err := common.Marshal(config)
	require.NoError(t, err)
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, data, 0600))
	command := exec.Command(binary, "--config", configPath, "--local-model")
	command.Dir = dir
	// Inherit only runtime essentials, never storage, proxy, or service secrets.
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "PATH=") || strings.HasPrefix(item, "TMPDIR=") || strings.HasPrefix(item, "LANG=") {
			command.Env = append(command.Env, item)
		}
	}
	logPath := filepath.Join(dir, "process.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	command.Stdout, command.Stderr = logFile, logFile
	require.NoError(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
		_ = logFile.Close()
	})
	client := &http.Client{Timeout: time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer synthetic-key")
		response, callErr := client.Do(request)
		if callErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	processLog, _ := os.ReadFile(logPath)
	t.Fatalf("synthetic legacy CPA did not become ready: %s", processLog)
	return ""
}
