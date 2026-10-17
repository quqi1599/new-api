package controller

import (
	"crypto/sha256"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
)

// Gateway exclusions live only for this outer request. The credential digest
// separates tenants/pools at one URL and is never logged or sent downstream.
// Keep the base path, query and organization: equal hosts alone prove nothing.
func gatewayServiceIdentity(channel *model.Channel) string {
	return gatewayServiceIdentityForScope(channel, false)
}

func gatewayServiceIdentityForScope(channel *model.Channel, gatewayScope bool) string {
	if channel == nil || channel.ChannelInfo.IsMultiKey || channel.Key == "" {
		return ""
	}
	switch channel.Type {
	case constant.ChannelTypeOpenAI, constant.ChannelTypeAnthropic:
	default:
		return ""
	}
	u, err := url.Parse(channel.GetBaseURL())
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	// The adapters append their path verbatim. A trailing slash therefore can
	// change the actual endpoint; do not collapse paths or encoded separators.
	// Only the exact standard passthrough rule is safe across protocols: both
	// adapters apply the same incoming request headers and skip credentials.
	// Other overrides can change routing/authentication and remain unclassified.
	headerIdentity, safeHeaders := gatewayHeaderIdentity(channel.GetHeaderOverride(), gatewayScope)
	if len(channel.GetParamOverride()) > 0 || !safeHeaders {
		return ""
	}
	organization := ""
	if channel.OpenAIOrganization != nil {
		organization = *channel.OpenAIOrganization
	}
	protocol := strconv.Itoa(channel.Type)
	if gatewayScope {
		// CPA explicitly finished its gateway-wide retry policy; switching
		// OpenAI/Claude wire formats does not create an independent model pool.
		protocol = "gateway"
	}
	identity := sha256.Sum256([]byte(u.String() + "\x00" + channel.Key + "\x00" + organization + "\x00" + channel.GetModelMapping() + "\x00" + protocol + "\x00" + headerIdentity))
	return string(identity[:])
}

func gatewayHeaderIdentity(headers map[string]interface{}, gatewayScope bool) (string, bool) {
	if len(headers) == 0 {
		return "", true
	}
	if gatewayScope && len(headers) == 1 && headers["*"] == true {
		return "passthrough-all", true
	}
	return "", false
}

func isExhaustedGatewayError(err *types.NewAPIError) bool {
	if err == nil || !err.HasUpstreamResponse() {
		return false
	}
	if err.GatewayRetryAttempts() > 0 {
		return true
	}
	switch err.GetErrorCode() {
	case "auth_unavailable", "auth_not_found", "codex_channels_unavailable":
		return err.StatusCode == http.StatusServiceUnavailable
	default:
		return false
	}
}

const exhaustedGatewayServicesKey = "relay_exhausted_gateway_services"

func recordExhaustedGateway(retryParam *service.RetryParam, channel *model.Channel, err *types.NewAPIError) {
	if retryParam == nil || retryParam.Ctx == nil || !isExhaustedGatewayError(err) {
		return
	}
	if channel == nil {
		return
	}
	if err.GatewayRetryAttempts() > 0 {
		// The selected entrance is exhausted even when overrides or a multi-key
		// configuration prevent proving that other records use the same pool.
		retryParam.AddPersistentExcludedChannel(channel.Id)
	}
	// Affinity templates may add overrides even when the channel record has
	// none. The selected request's effective context is authoritative.
	c := retryParam.Ctx
	gatewayScope := err.GatewayRetryAttempts() > 0
	effectiveHeaders := common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride)
	effectiveHeaderIdentity, safeHeaders := gatewayHeaderIdentity(effectiveHeaders, gatewayScope)
	if len(common.GetContextKeyStringMap(c, constant.ContextKeyChannelParamOverride)) > 0 || !safeHeaders {
		return
	}
	if channel != nil && channel.Key == "" {
		// The initially distributed channel is represented by a minimal record.
		// Read the selected attempt's existing context, not a newer DB snapshot.
		selected := *channel
		selected.Key = common.GetContextKeyString(c, constant.ContextKeyChannelKey)
		selected.BaseURL = common.GetPointer(common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl))
		selected.ModelMapping = common.GetPointer(common.GetContextKeyString(c, constant.ContextKeyChannelModelMapping))
		selected.OpenAIOrganization = common.GetPointer(c.GetString("channel_organization"))
		selected.ChannelInfo.IsMultiKey = common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if effectiveHeaderIdentity == "passthrough-all" {
			selected.HeaderOverride = common.GetPointer(`{"*":true}`)
		}
		channel = &selected
	}
	configuredHeaderIdentity, safeHeaders := gatewayHeaderIdentity(channel.GetHeaderOverride(), gatewayScope)
	if !safeHeaders || configuredHeaderIdentity != effectiveHeaderIdentity {
		return
	}
	identity := gatewayServiceIdentityForScope(channel, gatewayScope)
	if identity == "" {
		return
	}
	value, _ := retryParam.Ctx.Get(exhaustedGatewayServicesKey)
	excluded, _ := value.(map[string]struct{})
	if excluded == nil {
		excluded = make(map[string]struct{})
	}
	excluded[identity+"\x00"+retryParam.ModelName] = struct{}{}
	retryParam.Ctx.Set(exhaustedGatewayServicesKey, excluded)
	retryParam.AddPersistentExcludedChannel(channel.Id)
}

func isExhaustedGatewayCandidate(retryParam *service.RetryParam, channel *model.Channel) bool {
	if retryParam == nil || retryParam.Ctx == nil {
		return false
	}
	value, _ := retryParam.Ctx.Get(exhaustedGatewayServicesKey)
	excluded, _ := value.(map[string]struct{})
	for _, gatewayScope := range []bool{false, true} {
		identity := gatewayServiceIdentityForScope(channel, gatewayScope)
		if identity != "" {
			if _, exists := excluded[identity+"\x00"+retryParam.ModelName]; exists {
				return true
			}
		}
	}
	return false
}
