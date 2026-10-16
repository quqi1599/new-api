package controller

import (
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

func TestCPAInvalidRequestDoesNotReplayAnyEntrance(t *testing.T) {
	for _, claude := range []bool{false, true} {
		t.Run(fmt.Sprint(claude), func(t *testing.T) {
			got := runCPAStateRelay(t, "upstream_invalid_request", http.StatusUnprocessableEntity, true, false, cpaStateAliasOptions{claude: claude})
			require.EqualValues(t, 1, got.ownerCalls)
			require.Zero(t, got.alternateCalls)
			require.Equal(t, 422, got.response.Code)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
		})
	}
}

func TestCPAProtocolCapabilityRejectionCanUseSameServiceChat(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			got := runCPAStateRelay(t, "auth_unavailable", 503, true, false, cpaStateAliasOptions{cached: cached, claude: true, recoverChat: true})
			require.EqualValues(t, 1, got.ownerCalls, "Messages rejects json_object locally before calling CPA")
			require.Zero(t, got.alternateCalls)
			require.Equal(t, 200, got.response.Code, got.response.Body.String())
			require.Equal(t, []string{"9", "19"}, got.context.GetStringSlice("use_channel"))
		})
	}
	got := runCPAStateRelay(t, "auth_unavailable", 503, false, false, cpaStateAliasOptions{})
	require.EqualValues(t, 1, got.ownerCalls, "same-protocol aliases must not revisit exhausted pool")
}

func TestCPAUnclassifiedOrTransientFailureDoesNotExhaustService(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			got := runCPAStateRelay(t, "upstream_unavailable", status, true, false, cpaStateAliasOptions{})
			require.EqualValues(t, 2, got.ownerCalls, "generic CPA fallback code does not prove pool exhaustion")
			require.EqualValues(t, 1, got.alternateCalls)
			require.Equal(t, 200, got.response.Code)
		})
	}
}

func TestGatewayExclusionRespectsRequestModelAndEffectiveOverrides(t *testing.T) {
	for _, overridden := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		channel := &model.Channel{Id: 9, Type: constant.ChannelTypeOpenAI, BaseURL: common.GetPointer("https://example.test/pool"), Key: "synthetic-a"}
		param := &service.RetryParam{Ctx: c, ModelName: "model-a"}
		if overridden {
			common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]interface{}{"model": "different-model"})
		}
		err := types.WithOpenAIError(types.OpenAIError{Code: "auth_unavailable", Type: "cpa_error", Message: "synthetic"}, 503, types.ErrOptionWithUpstreamResponse())
		recordExhaustedGateway(param, channel, err)
		require.Equal(t, !overridden, isExhaustedGatewayCandidate(param, channel))
		param.ModelName = "model-b"
		require.False(t, isExhaustedGatewayCandidate(param, channel))
	}
}

func TestExhaustedGatewaySkipsAliasesButKeepsIndependentAlternate(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, claude := range []bool{false, true} {
			for _, code := range []string{"auth_unavailable", "codex_channels_unavailable"} {
				t.Run(fmt.Sprintf("%s/cached=%t/claude=%t", code, cached, claude), func(t *testing.T) {
					got := runCPAStateRelay(t, code, http.StatusServiceUnavailable, true, false, cpaStateAliasOptions{cached: cached, claude: claude})
					wantOwner, wantAttempts := 1, 2
					if claude {
						wantOwner, wantAttempts = 2, 3
					}
					require.EqualValues(t, wantOwner, got.ownerCalls)
					require.EqualValues(t, 1, got.alternateCalls)
					require.Equal(t, 200, got.response.Code)
					require.Equal(t, wantAttempts, got.context.GetInt("retry_attempt_no"))
					if !claude {
						require.Equal(t, "gateway_service_exhausted", got.context.GetString("retry_candidate_skip_reason"))
					}
				})
			}
		}
	}
	got := runCPAStateRelay(t, "temporary_overload", http.StatusServiceUnavailable, true, false, cpaStateAliasOptions{})
	require.EqualValues(t, 2, got.ownerCalls, "generic failures do not establish aggregate exhaustion")
	require.EqualValues(t, 1, got.alternateCalls)
}

func TestGatewayServiceIdentityPreservesPoolBoundaries(t *testing.T) {
	base := model.Channel{Type: constant.ChannelTypeOpenAI, BaseURL: common.GetPointer("https://EXAMPLE.test:443/pool"), Key: "synthetic-a"}
	want := gatewayServiceIdentity(&base)
	require.NotEmpty(t, want)
	alias := base
	alias.BaseURL = common.GetPointer("https://example.test/pool")
	require.Equal(t, want, gatewayServiceIdentity(&alias))
	for _, change := range []func(*model.Channel){
		func(c *model.Channel) { c.Type = constant.ChannelTypeAnthropic },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/pool/") },
		func(c *model.Channel) { c.Key = "synthetic-b" },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/other") },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("http://example.test/pool") },
		func(c *model.Channel) { c.BaseURL = common.GetPointer("https://example.test/pool?tenant=other") },
		func(c *model.Channel) { c.OpenAIOrganization = common.GetPointer("other-org") },
		func(c *model.Channel) { c.ModelMapping = common.GetPointer(`{"a":"b"}`) },
		func(c *model.Channel) { c.ChannelInfo.IsMultiKey = true },
		func(c *model.Channel) { c.HeaderOverride = common.GetPointer(`{"X-Tenant":"other"}`) },
	} {
		other := base
		change(&other)
		require.NotEqual(t, want, gatewayServiceIdentity(&other))
	}
}
