package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

// Verify the producer and consumer together: a real CPA executor must preserve
// the request-scoped rejection through both wire protocols before Relay retries.
func TestLegacyCPAInvalidRequestWithRealGateway(t *testing.T) {
	binary := os.Getenv("LEGACY_CPA_RETRY_TEST_BINARY")
	if binary == "" {
		t.Skip("set LEGACY_CPA_RETRY_TEST_BINARY to a locally built legacy CPA binary")
	}
	require.True(t, filepath.IsAbs(binary))
	for _, claude := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("claude=%t/stream=%t", claude, stream), func(t *testing.T) {
				var upstreamCalls atomic.Int32
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnprocessableEntity)
					if claude {
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"private-synthetic-validation-detail"}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"code":"invalid_request_error","type":"invalid_request_error","message":"private-synthetic-validation-detail"}}`)
					}
				}))
				t.Cleanup(origin.Close)
				models := []map[string]string{{"name": "gpt-6.1-sol", "alias": "gpt-6.1-sol"}}
				config := map[string]any{"codex-api-key": nil}
				if claude {
					config["claude-api-key"] = []map[string]any{{
						"api-key": "synthetic-upstream", "base-url": origin.URL, "proxy-url": "direct", "models": models,
					}}
				} else {
					config["openai-compatibility"] = []map[string]any{{
						"name": "synthetic-chat", "base-url": origin.URL, "models": models,
						"api-key-entries": []map[string]string{{"api-key": "synthetic-upstream", "proxy-url": "direct"}},
					}}
				}
				base := startLegacyRetryCPA(t, binary, origin.URL, 1, config)
				target, err := url.Parse(base)
				require.NoError(t, err)
				got := runCPAStateRelay(t, "invalid_request_error", 422, false, stream, cpaStateAliasOptions{
					claude: claude, chat: true, cached: stream, maxAttempts: 6,
					ownerHandler: httputil.NewSingleHostReverseProxy(target),
				})
				t.Logf("entrance_calls=%d upstream_calls=%d status=%d attempts=%d", got.ownerCalls, upstreamCalls.Load(), got.response.Code, got.context.GetInt("retry_attempt_no"))
				require.EqualValues(t, 1, got.ownerCalls, "request rejection must not replay an entrance in later rounds")
				require.EqualValues(t, 1, upstreamCalls.Load())
				require.Equal(t, http.StatusUnprocessableEntity, got.response.Code)
				require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
				require.Equal(t, service.RetryStopReasonNotRetryable, got.context.GetString("retry_stop_reason"))
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, common.Unmarshal(got.response.Body.Bytes(), &response))
				require.Equal(t, "upstream_invalid_request", response.Error.Code)
				require.NotContains(t, got.response.Body.String(), "private-synthetic-validation-detail")
			})
		}
	}
}
