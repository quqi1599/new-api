package channel

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Reserved response-contract headers must never originate in client
// passthrough or request overrides, including an upstream that reflects them.
func stripCPARetryRequestHeaders(header http.Header) {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "x-cpa-retry-") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "x-cpa-pool-") {
			delete(header, name)
		}
	}
}

func captureCPARetryBarrier(c *gin.Context, req *http.Request, resp *http.Response, info *relaycommon.RelayInfo) {
	if c == nil {
		return
	}
	common.SetContextKey(c, constant.ContextKeyRelayGatewayNoReplayReason, "")
	service.SetCPAPoolResponseTrusted(resp, false)
	// This administrator-owned setting already declares a CPA identity channel.
	// It is not inferred from a channel name, client header or generic gateway
	// fingerprint. Missing source metadata and cross-origin redirects fail closed.
	if info == nil || info.ChannelMeta == nil || !info.ChannelOtherSettings.CPAAuditIdentityEnabled ||
		req == nil || !sameCPAResponseChain(req.URL, resp) {
		return
	}
	if values := resp.Header.Values("X-CPA-Pool-Exhausted"); resp.StatusCode == http.StatusServiceUnavailable && len(values) == 1 && values[0] == "capacity-v1" {
		service.SetCPAPoolResponseTrusted(resp, true)
	}
	if reason := service.ParseGatewayReplayBarrier(resp.Header, resp.StatusCode); reason != "" {
		common.SetContextKey(c, constant.ContextKeyRelayGatewayNoReplayReason, reason)
	}
}

func sameCPAResponseChain(origin *url.URL, response *http.Response) bool {
	if response == nil {
		return false
	}
	// net/http retains preceding redirect responses on Request.Response. Check
	// every hop, so leaving the trusted origin and returning does not regain trust.
	for hops := 0; response != nil; hops++ {
		if hops >= 10 || response.Request == nil || !sameCPAResponseOrigin(origin, response.Request.URL) {
			return false
		}
		response = response.Request.Response
	}
	return true
}

func sameCPAResponseOrigin(request, response *url.URL) bool {
	valid := func(u *url.URL) bool {
		return u != nil && u.User == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https")
	}
	if !valid(request) || !valid(response) || request.Scheme != response.Scheme || !strings.EqualFold(request.Hostname(), response.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if value := u.Port(); value != "" {
			return value
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return port(request) == port(response)
}
