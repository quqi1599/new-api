package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func gatewayExhaustionHandler(status int, marked bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		if marked {
			w.Header().Set("X-CPA-Retry-Scope", "gateway")
			w.Header().Set("X-CPA-Retry-Attempts", "3")
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"error":{"type":"cpa_error","code":"upstream_unavailable","message":"synthetic rate limited"}}`)
	})
}

func TestGatewayRetryExhaustionStopsSamePoolAcrossRoundsAndProtocols(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, claude := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, marked := range []bool{false, true} {
					t.Run(fmt.Sprintf("cached=%t/claude=%t/stream=%t/marked=%t", cached, claude, stream, marked), func(t *testing.T) {
						got := runCPAStateRelay(t, "upstream_unavailable", http.StatusTooManyRequests, false, stream, cpaStateAliasOptions{
							cached: cached, claude: claude, maxAttempts: 6,
							ownerHandler: gatewayExhaustionHandler(http.StatusTooManyRequests, marked),
						})
						wantCalls := int32(6)
						if marked {
							wantCalls = 1
							require.Equal(t, "17", got.response.Header().Get("Retry-After"))
						}
						require.Equal(t, wantCalls, got.ownerCalls)
						require.EqualValues(t, wantCalls, got.context.GetInt("retry_attempt_no"))
						require.Equal(t, http.StatusTooManyRequests, got.response.Code)
						require.Contains(t, got.response.Body.String(), "upstream_unavailable")
						require.NotContains(t, got.response.Body.String(), "data:")
						require.Zero(t, got.alternateCalls)
					})
				}
			}
		}
	}
}

func TestGatewayRetryExhaustionKeepsIndependentFallback(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, claude := range []bool{false, true} {
			for _, status := range []int{429, 503} {
				t.Run(fmt.Sprintf("cached=%t/claude=%t/status=%d", cached, claude, status), func(t *testing.T) {
					got := runCPAStateRelay(t, "upstream_unavailable", status, true, false, cpaStateAliasOptions{
						cached: cached, claude: claude, maxAttempts: 6,
						ownerHandler: gatewayExhaustionHandler(status, true),
					})
					require.EqualValues(t, 1, got.ownerCalls)
					require.EqualValues(t, 1, got.alternateCalls)
					require.Equal(t, 200, got.response.Code, got.response.Body.String())
					require.Equal(t, []string{"9", "10"}, got.context.GetStringSlice("use_channel"))
				})
			}
		}
	}
}

func TestGatewayRetryScopeMatchesStandardPassthroughAliases(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, claude := range []bool{false, true} {
			t.Run(fmt.Sprintf("cached=%t/claude=%t", cached, claude), func(t *testing.T) {
				got := runCPAStateRelay(t, "upstream_unavailable", 429, true, true, cpaStateAliasOptions{
					cached: cached, claude: claude, maxAttempts: 6, ownerHeaderOverride: `{"*":true}`,
					ownerHandler: gatewayExhaustionHandler(429, true),
				})
				require.EqualValues(t, 1, got.ownerCalls)
				require.EqualValues(t, 1, got.alternateCalls)
				require.Equal(t, 200, got.response.Code, got.response.Body.String())
				require.Equal(t, []string{"9", "10"}, got.context.GetStringSlice("use_channel"))
			})
		}
	}
}

func TestGatewayRetryExhaustionSurvivesTruncatedHTTPBody(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		t.Run(fmt.Sprintf("alternate=%t", alternate), func(t *testing.T) {
			got := runCPAStateRelay(t, "upstream_unavailable", 429, alternate, false, cpaStateAliasOptions{
				claude: true, maxAttempts: 6, ownerHeaderOverride: `{"*":true}`,
				ownerHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", "1024")
					w.Header().Set("Retry-After", "17")
					w.Header().Set("X-CPA-Retry-Scope", "gateway")
					w.Header().Set("X-CPA-Retry-Attempts", "3")
					w.WriteHeader(429)
					_, _ = fmt.Fprint(w, `{"error":`)
				}),
			})
			require.EqualValues(t, 1, got.ownerCalls)
			if alternate {
				require.EqualValues(t, 1, got.alternateCalls)
				require.Equal(t, 200, got.response.Code, got.response.Body.String())
			} else {
				require.Equal(t, 429, got.response.Code)
				require.Equal(t, "17", got.response.Header().Get("Retry-After"))
				require.Contains(t, got.response.Body.String(), "unexpected EOF")
			}
		})
	}
}

func TestGatewayRetryScopeRejectsDifferentOrDynamicHeaderRules(t *testing.T) {
	base := model.Channel{Id: 9, Type: constant.ChannelTypeOpenAI, BaseURL: common.GetPointer("https://example.test/pool"), Key: "synthetic-a", HeaderOverride: common.GetPointer(`{"*":true}`)}
	alias := base
	alias.Id, alias.Type = 19, constant.ChannelTypeAnthropic
	for _, tc := range []struct {
		name       string
		headers    map[string]interface{}
		parameters map[string]interface{}
		want       bool
	}{
		{"same passthrough", map[string]interface{}{"*": true}, nil, true},
		{"missing effective rule", nil, nil, false},
		{"extra dynamic header", map[string]interface{}{"*": true, "X-Tenant": "other"}, nil, false},
		{"different rule", map[string]interface{}{"re:^x-": true}, nil, false},
		{"dynamic parameters", map[string]interface{}{"*": true}, map[string]interface{}{"model": "other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			param := &service.RetryParam{Ctx: c, ModelName: "model-a"}
			common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, tc.headers)
			common.SetContextKey(c, constant.ContextKeyChannelParamOverride, tc.parameters)
			err := types.WithOpenAIError(types.OpenAIError{Code: "upstream_unavailable", Message: "synthetic"}, 429, types.ErrOptionWithUpstreamResponse())
			err.SetGatewayRetryAttempts(3)
			recordExhaustedGateway(param, &base, err)
			require.Equal(t, []int{9}, param.PersistentExcludedIds)
			require.Equal(t, tc.want, isExhaustedGatewayCandidate(param, &alias))
			withoutPassthrough := alias
			withoutPassthrough.HeaderOverride = nil
			require.False(t, isExhaustedGatewayCandidate(param, &withoutPassthrough))
		})
	}
}

func TestGatewayRetryScopePreservesPoolBoundaries(t *testing.T) {
	base := model.Channel{Id: 9, Type: constant.ChannelTypeOpenAI, BaseURL: common.GetPointer("https://example.test/pool"), Key: "synthetic-a"}
	want := gatewayServiceIdentityForScope(&base, true)
	alias := base
	alias.Id, alias.Type = 19, constant.ChannelTypeAnthropic
	require.Equal(t, want, gatewayServiceIdentityForScope(&alias, true))
	for _, change := range []func(*model.Channel){
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/other") },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/pool/") },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/pool?tenant=other") },
		func(c *model.Channel) { c.Key = "synthetic-b" },
		func(c *model.Channel) { c.OpenAIOrganization = common.GetPointer("other-org") },
		func(c *model.Channel) { c.ModelMapping = common.GetPointer(`{"a":"b"}`) },
		func(c *model.Channel) { c.HeaderOverride = common.GetPointer(`{"X-Tenant":"other"}`) },
		func(c *model.Channel) { c.ParamOverride = common.GetPointer(`{"model":"other"}`) },
		func(c *model.Channel) { c.ChannelInfo.IsMultiKey = true },
	} {
		other := base
		change(&other)
		require.NotEqual(t, want, gatewayServiceIdentityForScope(&other, true))
	}

	for _, overridden := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		param := &service.RetryParam{Ctx: c, ModelName: "model-a"}
		if overridden {
			common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, map[string]interface{}{"X-Tenant": "other"})
		}
		err := types.WithOpenAIError(types.OpenAIError{Code: "upstream_unavailable", Message: "synthetic"}, 429, types.ErrOptionWithUpstreamResponse())
		err.SetGatewayRetryAttempts(3)
		recordExhaustedGateway(param, &base, err)
		require.Equal(t, []int{9}, param.PersistentExcludedIds, "selected entrance always remains excluded")
		require.Equal(t, !overridden, isExhaustedGatewayCandidate(param, &alias))
		param.ModelName = "model-b"
		require.False(t, isExhaustedGatewayCandidate(param, &alias))
	}
}

func TestGatewayRetryExhaustionKeepsIndependentTransientRecovery(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	param := &service.RetryParam{Ctx: c}
	param.AddPersistentExcludedChannel(9)
	state := service.NewRelayRetryState(types.RelayFormatOpenAI, 5)
	state.RecordAttempt(9)
	state.RecordAttempt(10)
	err := types.WithOpenAIError(types.OpenAIError{Code: "temporary_overload", Message: "synthetic"}, 502, types.ErrOptionWithUpstreamResponse())
	require.True(t, canStartNextRelayRetryRound(context.Background(), state, true, false, err, param))
	param.AddPersistentExcludedChannel(10)
	require.False(t, canStartNextRelayRetryRound(context.Background(), state, true, false, err, param))
}
