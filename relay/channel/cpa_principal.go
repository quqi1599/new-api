package channel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

const cpaPrincipalHeader = "X-CPA-Principal"

// The final outbound boundary owns this reserved header. Neither client
// passthrough nor channel/runtime header overrides may impersonate another
// authenticated customer on the shared CPA credential.
func applyCPAPrincipalHeader(header http.Header, info *relaycommon.RelayInfo, authority string) {
	for name := range header {
		if strings.EqualFold(strings.TrimSpace(name), cpaPrincipalHeader) {
			delete(header, name)
		}
	}
	if info == nil || info.ChannelMeta == nil || info.UserId <= 0 || info.TokenId <= 0 || info.IsChannelTest || info.ApiKey == "" {
		return
	}
	authority = strings.TrimRight(strings.TrimSpace(authority), "/")
	parsed, err := url.Parse(authority)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		// An unconfigured/default authority is shared across installations.
		// Omitting the claim disables CPA replay safely until configured.
		return
	}
	mac := hmac.New(sha256.New, []byte(info.ApiKey))
	// NUL-delimited fields and a versioned domain prevent concatenation and
	// cross-purpose collisions. The transmitted claim contains no plaintext ID.
	_, _ = mac.Write([]byte("newapi/cpa-principal/v1\x00" + authority + "\x00" + strconv.Itoa(info.UserId) + "\x00" + strconv.Itoa(info.TokenId)))
	header.Set(cpaPrincipalHeader, hex.EncodeToString(mac.Sum(nil)))
}

func applyCPAPrincipal(header http.Header, info *relaycommon.RelayInfo) {
	applyCPAPrincipalHeader(header, info, system_setting.ServerAddress)
}
