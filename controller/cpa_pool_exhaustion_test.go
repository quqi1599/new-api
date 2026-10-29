package controller

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCPAPoolExhaustionStopsOnlySameService(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, alternate := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("trusted=%t/alternate=%t/stream=%t", trusted, alternate, stream), func(t *testing.T) {
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("X-CPA-Pool-Exhausted") != "" {
							t.Error("customer contract leaked upstream")
						}
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-CPA-Pool-Exhausted", "capacity-v1")
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"code":"model_cooldown","type":"cpa_error","message":"synthetic capacity exhaustion"}}`)
					})
					got := runCPAStateRelay(t, "model_cooldown", 503, alternate, stream, cpaStateAliasOptions{trustedCPA: trusted, ownerHandler: handler, maxAttempts: 6, claude: true, ownerHeaderOverride: `{"*":true}`, requestHeaders: http.Header{"X-Cpa-Pool-Exhausted": {"capacity-v1"}}})
					if trusted {
						require.EqualValues(t, 1, got.ownerCalls)
					} else {
						require.Greater(t, got.ownerCalls, int32(1))
					}
					if alternate {
						require.Equal(t, 200, got.response.Code)
						require.EqualValues(t, 1, got.alternateCalls)
					} else {
						require.Equal(t, 503, got.response.Code)
					}
				})
			}
		}
	}
}

func TestCPAPoolExhaustionRealProcess(t *testing.T) {
	binary := os.Getenv("CPA_RETRY_TEST_BINARY")
	if binary == "" {
		t.Skip("set CPA_RETRY_TEST_BINARY for real CPA process")
	}
	for _, stream := range []bool{false, true} {
		for _, alternate := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/alternate=%t", stream, alternate), func(t *testing.T) {
				var calls atomic.Int32
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"synthetic"}}`)
				}))
				defer origin.Close()
				base := startNestedRetryCPA(t, binary, origin.URL, 3, "generic")
				req, err := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"seed cooldown"}]}`))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer synthetic-key")
				req.Header.Set("Content-Type", "application/json")
				response, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				require.Equal(t, 429, response.StatusCode)
				require.EqualValues(t, 3, calls.Load())
				target, err := url.Parse(base)
				require.NoError(t, err)
				proxy := httputil.NewSingleHostReverseProxy(target)
				proxy.ModifyResponse = func(r *http.Response) error {
					require.Equal(t, 503, r.StatusCode)
					require.Equal(t, "capacity-v1", r.Header.Get("X-CPA-Pool-Exhausted"))
					require.Empty(t, r.Header.Get("X-CPA-Retry-Attempts"))
					return nil
				}
				got := runCPAStateRelay(t, "model_cooldown", 503, alternate, stream, cpaStateAliasOptions{trustedCPA: true, ownerHandler: proxy, maxAttempts: 6, claude: true, ownerHeaderOverride: `{"*":true}`})
				require.EqualValues(t, 1, got.ownerCalls)
				require.EqualValues(t, 3, calls.Load(), "zero-dispatch requests must not call cold upstream")
				if alternate {
					require.Equal(t, 200, got.response.Code)
					require.EqualValues(t, 1, got.alternateCalls)
				} else {
					require.Equal(t, 503, got.response.Code)
				}
			})
		}
	}
}
