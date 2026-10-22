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
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Runs the real gateway on a private loopback instance, never live credentials.
func TestCPANoReplayWithRealGateway(t *testing.T) {
	binary := os.Getenv("CPA_RETRY_TEST_BINARY")
	if binary == "" {
		t.Skip("set CPA_RETRY_TEST_BINARY to a locally built CPA binary")
	}
	require.True(t, filepath.IsAbs(binary))
	for _, stream := range []bool{false, true} {
		for _, scenario := range []struct {
			name, reason             string
			strip, rejected, invalid bool
		}{
			{name: "generated_failure", reason: "upstream_output_observed"},
			{name: "generated_failure_without_contract", strip: true},
			{name: "accepted_invalid_stream", reason: "accepted_outcome_unknown", invalid: true},
			{name: "independent_rejection_fallback", rejected: true},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, scenario.name), func(t *testing.T) {
				var upstreamCalls atomic.Int32
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if scenario.rejected {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(429)
						_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"synthetic rejection before generation"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario.invalid {
						_, _ = io.WriteString(w, "data: {invalid\n\n")
						return
					}
					_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"resp_generated","status":"failed","output":[],"error":{"type":"server_error","code":"server_error","message":"synthetic failure after generation"},"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}}`+"\n\n")
				}))
				t.Cleanup(origin.Close)
				base := startNoReplayCPA(t, binary, origin.URL)
				target, err := url.Parse(base)
				require.NoError(t, err)
				proxy := httputil.NewSingleHostReverseProxy(target)
				var contracts atomic.Int32
				proxy.ModifyResponse = func(resp *http.Response) error {
					if scenario.rejected {
						assert.Equal(t, "gateway", resp.Header.Get("X-CPA-Retry-Scope"))
					} else {
						assert.Equal(t, "request", resp.Header.Get("X-CPA-Retry-Scope"))
						if scenario.reason != "" {
							assert.Equal(t, scenario.reason, resp.Header.Get("X-CPA-Retry-Reason"))
						}
					}
					contracts.Add(1)
					if scenario.strip {
						for _, h := range []string{"X-CPA-Retry-Scope", "X-CPA-Retry-Reason", "X-CPA-Retry-Attempts"} {
							resp.Header.Del(h)
						}
					}
					return nil
				}
				got := runCPAStateRelay(t, "upstream_stream_failed", 502, true, stream, cpaStateAliasOptions{ownerHandler: proxy, maxAttempts: 6, trustedCPA: true, cached: stream})
				wantOwnerCalls := int32(1)
				if scenario.strip {
					wantOwnerCalls = 2
				}
				require.Equal(t, wantOwnerCalls, got.ownerCalls)
				require.Equal(t, wantOwnerCalls, contracts.Load())
				if scenario.strip || scenario.rejected {
					require.Equal(t, 200, got.response.Code, got.response.Body.String())
					require.EqualValues(t, 1, got.alternateCalls)
				} else {
					require.GreaterOrEqual(t, got.response.Code, 400, got.response.Body.String())
					require.Zero(t, got.alternateCalls, "accepted generation was replayed by NewAPI")
					require.Equal(t, scenario.reason, got.context.GetString("retry_stop_reason"))
				}
				wantCalls := wantOwnerCalls
				if scenario.rejected {
					wantCalls = 2
				}
				require.Equal(t, wantCalls, upstreamCalls.Load())
				t.Logf("upstream=%d CPA=%d alternate=%d stop=%s HTTP=%d", upstreamCalls.Load(), got.ownerCalls, got.alternateCalls, got.context.GetString("retry_stop_reason"), got.response.Code)
			})
		}
	}
}

func startNoReplayCPA(t *testing.T, binary, origin string) string {
	t.Helper()
	dir := t.TempDir()
	config := map[string]any{"version": 0, "gateways": []map[string]any{{"id": "synthetic-native", "name": "synthetic-native", "enabled": true, "kind": "newapi", "protocol": "openai", "compatibility": "generic", "base_url": origin, "responses_mode": "native_sse", "responses_bridge": true, "session_affinity": false, "model_aliases": map[string]string{"gpt-6.1-sol": "synthetic-model"}, "capabilities": map[string]bool{"chat": true, "responses": true, "claude_messages": true, "stream": true, "tools": true}, "keys": []map[string]any{{"id": "a", "enabled": true, "api_key_env": "CPA_FIXTURE_KEY_A"}, {"id": "b", "enabled": true, "api_key_env": "CPA_FIXTURE_KEY_B"}}}}}
	data, err := common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateways.json"), data, 0600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	cmd := exec.Command(binary)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CPA_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "CPA_LISTEN_ADDR="+address, "CPA_API_KEY=synthetic-key", "CPA_MANAGEMENT_KEY=synthetic-admin", "CPA_GATEWAYS_FILE="+filepath.Join(dir, "gateways.json"), "CPA_USAGE_PATH="+filepath.Join(dir, "usage.sqlite"), "CPA_LOG_PATH="+filepath.Join(dir, "logs.sqlite"), "CPA_ROUTING_STATE_PATH="+filepath.Join(dir, "routing.json"), "CPA_PROBE_PATH="+filepath.Join(dir, "probes.json"), "CPA_CONTENT_AUDIT_CONFIG="+filepath.Join(dir, "audit.json"), "CPA_MAX_ATTEMPTS=3", "CPA_KEY_AUTO_RECOVERY=false", "CPA_SESSION_AFFINITY=false", "CPA_SHUTDOWN_GRACE_SECONDS=1", "CPA_SHUTDOWN_FINALIZE_SECONDS=1", "CPA_FIXTURE_KEY_A=synthetic-a", "CPA_FIXTURE_KEY_B=synthetic-b")
	log, err := os.Create(filepath.Join(dir, "process.log"))
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = log.Close()
	})
	base := "http://" + address
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	output, _ := os.ReadFile(filepath.Join(dir, "process.log"))
	t.Fatalf("CPA fixture not ready: %s", output)
	return ""
}
