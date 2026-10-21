package controller

import (
	"encoding/json"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This optional cross-repository check runs the actual CPA binary and the real
// NewAPI Relay/selector against synthetic HTTP 429s. It never uses live keys.
// Build cpa-new/cmd/cpa and set CPA_RETRY_TEST_BINARY to its absolute path.
func TestCPANestedRetryWithRealGateway(t *testing.T) {
	binary := os.Getenv("CPA_RETRY_TEST_BINARY")
	if binary == "" {
		t.Skip("set CPA_RETRY_TEST_BINARY to a locally built CPA binary")
	}
	require.True(t, filepath.IsAbs(binary))
	for _, claude := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []struct {
				name      string
				strip     bool
				alternate bool
				keys      int
				calls     int32
				status    int
			}{
				{"legacy_without_contract", true, false, 3, 18, 429},
				{"gateway_budget_once", false, false, 3, 3, 429},
				{"independent_fallback", false, true, 3, 3, 200},
				{"large_pool_once", false, false, 25, 25, 429},
				{"large_pool_independent_fallback", false, true, 25, 25, 200},
			} {
				t.Run(fmt.Sprintf("claude=%t/stream=%t/%s", claude, stream, scenario.name), func(t *testing.T) {
					var upstreamCalls atomic.Int32
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						upstreamCalls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"synthetic concurrent request limit"}}`)
					}))
					t.Cleanup(origin.Close)
					base := startNestedRetryCPA(t, binary, origin.URL, scenario.keys)
					target, err := url.Parse(base)
					require.NoError(t, err)
					proxy := httputil.NewSingleHostReverseProxy(target)
					proxy.ModifyResponse = func(response *http.Response) error {
						// A negative control may re-enter after all keys have acquired
						// a local pause. That zero-dispatch response has no marker.
						marked := response.Header.Get("X-CPA-Retry-Scope") != ""
						if response.StatusCode != 429 || (!scenario.strip || marked) && (response.Header.Get("X-CPA-Retry-Scope") != "gateway" || response.Header.Get("X-CPA-Retry-Attempts") != fmt.Sprint(scenario.keys)) {
							t.Errorf("unexpected CPA contract: status=%d scope=%q attempts=%q", response.StatusCode, response.Header.Get("X-CPA-Retry-Scope"), response.Header.Get("X-CPA-Retry-Attempts"))
						}
						if scenario.strip {
							response.Header.Del("X-CPA-Retry-Scope")
							response.Header.Del("X-CPA-Retry-Attempts")
						}
						return nil
					}
					got := runCPAStateRelay(t, "rate_limit_exceeded", 429, scenario.alternate, stream, cpaStateAliasOptions{
						claude: claude, ownerHandler: proxy, maxAttempts: 6,
						cached: stream, ownerHeaderOverride: `{"*":true}`,
					})
					require.Equal(t, scenario.status, got.response.Code, got.response.Body.String())
					if scenario.strip {
						// The regression is repeated entrance traversal, even if the
						// key-health guard later prevents some actual dispatches.
						require.Greater(t, upstreamCalls.Load(), int32(scenario.keys))
						require.LessOrEqual(t, upstreamCalls.Load(), scenario.calls)
						require.Greater(t, got.ownerCalls, int32(1))
					} else {
						require.Equal(t, scenario.calls, upstreamCalls.Load())
						require.Equal(t, scenario.calls/int32(scenario.keys), got.ownerCalls)
					}
					if scenario.alternate {
						require.EqualValues(t, 1, got.alternateCalls)
					} else {
						require.Zero(t, got.alternateCalls)
					}
					t.Logf("CPA calls=%d upstream attempts=%d independent fallback=%d HTTP=%d", got.ownerCalls, upstreamCalls.Load(), got.alternateCalls, got.response.Code)
				})
			}
		}
	}
}

func startNestedRetryCPA(t *testing.T, binary, origin string, count int) string {
	t.Helper()
	dir := t.TempDir()
	keys := make([]map[string]any, count)
	for i := range keys {
		keys[i] = map[string]any{"id": fmt.Sprint(i), "enabled": true, "api_key_env": fmt.Sprintf("CPA_SYNTHETIC_RETRY_KEY_%d", i)}
	}
	config := map[string]any{"version": 0, "gateways": []map[string]any{{
		"id": "synthetic", "name": "synthetic", "enabled": true,
		"kind": "newapi", "protocol": "openai", "base_url": origin,
		"responses_mode": "chat", "session_affinity": false,
		"model_aliases": map[string]string{"gpt-6.1-sol": "MiniMax-M2.7"},
		"capabilities":  map[string]bool{"chat": true, "responses": true, "claude_messages": true, "stream": true, "tools": true},
		"keys":          keys,
	}}}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateways.json"), data, 0600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	command := exec.Command(binary)
	command.Dir = dir
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "CPA_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env,
		"CPA_LISTEN_ADDR="+address, "CPA_API_KEY=synthetic-key", "CPA_MANAGEMENT_KEY=synthetic-admin",
		"CPA_GATEWAYS_FILE="+filepath.Join(dir, "gateways.json"), "CPA_USAGE_PATH="+filepath.Join(dir, "usage.sqlite"),
		"CPA_LOG_PATH="+filepath.Join(dir, "logs.sqlite"), "CPA_ROUTING_STATE_PATH="+filepath.Join(dir, "routing.json"),
		"CPA_PROBE_PATH="+filepath.Join(dir, "probes.json"), "CPA_CONTENT_AUDIT_CONFIG="+filepath.Join(dir, "audit.json"),
		"CPA_MAX_ATTEMPTS=3", "CPA_KEY_AUTO_RECOVERY=false", "CPA_SESSION_AFFINITY=false",
		"CPA_SHUTDOWN_GRACE_SECONDS=1", "CPA_SHUTDOWN_FINALIZE_SECONDS=1")
	for i := range keys {
		command.Env = append(command.Env, fmt.Sprintf("CPA_SYNTHETIC_RETRY_KEY_%d=synthetic-key-%d", i, i))
	}
	logFile, err := os.Create(filepath.Join(dir, "process.log"))
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
	base := "http://" + address
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, callErr := client.Get(base + "/readyz")
		if callErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	processLog, _ := os.ReadFile(filepath.Join(dir, "process.log"))
	t.Fatalf("synthetic CPA did not become ready: %s", processLog)
	return ""
}
