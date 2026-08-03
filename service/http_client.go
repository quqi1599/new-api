package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"golang.org/x/net/proxy"
)

var (
	httpClient              *http.Client
	ssrfProtectedHTTPClient *http.Client
	proxyClientLock         sync.Mutex
	proxyClients            = make(map[string]*http.Client)
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func relayTimeoutDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func withRelayDialTimeout(dialContext dialContextFunc) dialContextFunc {
	timeout := relayTimeoutDuration(common.RelayDialTimeout)
	if timeout == 0 {
		return dialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return dialContext(dialCtx, network, address)
	}
}

// newRelayHTTPTransport applies connection-phase timeouts without imposing an
// absolute lifetime on response-body reads. This keeps long-lived SSE streams
// alive after response headers arrive while bounding connection and
// response-header stalls consistently for direct, HTTP proxy, and SOCKS proxy
// clients. The first valid SSE event has a separate scanner-level timeout.
func newRelayHTTPTransport(proxyFunc func(*http.Request) (*url.URL, error), dialContext dialContextFunc) *http.Transport {
	if dialContext == nil {
		dialer := &net.Dialer{KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}

	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           withRelayDialTimeout(dialContext),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          common.RelayMaxIdleConns,
		MaxIdleConnsPerHost:   common.RelayMaxIdleConnsPerHost,
		IdleConnTimeout:       relayTimeoutDuration(common.RelayIdleConnTimeout),
		TLSHandshakeTimeout:   relayTimeoutDuration(common.RelayTLSHandshakeTimeout),
		ResponseHeaderTimeout: relayTimeoutDuration(common.RelayResponseHeaderTimeout),
		ExpectContinueTimeout: relayTimeoutDuration(common.RelayExpectContinueTimeout),
	}
	if common.TLSInsecureSkipVerify {
		transport.TLSClientConfig = common.InsecureTLSConfig.Clone()
	}
	return transport
}

func newRelayHTTPClient(transport http.RoundTripper) *http.Client {
	// Do not apply common.RelayTimeout as http.Client.Timeout here. The latter
	// includes response-body reads and would terminate a healthy stream after a
	// fixed wall-clock duration. A positive legacy RELAY_TIMEOUT is instead used
	// as the response-header timeout fallback during common.InitEnv.
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	// A redirect response proves only that an upstream received the request; it
	// does not prove the model operation was not started. Never transparently
	// replay a generation/task POST (including POST -> GET rewrites on 301/302/303).
	for _, previous := range via {
		if previous.Method != http.MethodGet && previous.Method != http.MethodHead {
			return http.ErrUseLastResponse
		}
	}
	urlStr := req.URL.String()
	if err := validateURLWithCurrentFetchSetting(urlStr, true); err != nil {
		return fmt.Errorf("redirect to %s blocked: %v", urlStr, err)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

func checkProtectedFetchRedirect(req *http.Request, via []*http.Request) error {
	urlStr := req.URL.String()
	if err := ValidateSSRFProtectedFetchURL(urlStr); err != nil {
		return fmt.Errorf("redirect to %s blocked: %v", urlStr, err)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

func validateURLWithCurrentFetchSetting(urlStr string, applyDomainIPFilter bool) error {
	fetchSetting := system_setting.GetFetchSetting()
	return common.ValidateURLWithFetchSetting(urlStr, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, applyDomainIPFilter && fetchSetting.ApplyIPFilterForDomain)
}

func ValidateSSRFProtectedFetchURL(urlStr string) error {
	return validateURLWithCurrentFetchSetting(urlStr, true)
}

func InitHttpClient() {
	transport := newRelayHTTPTransport(http.ProxyFromEnvironment, nil)
	httpClient = newRelayHTTPClient(transport)
	ssrfProtectedHTTPClient = newProtectedFetchHTTPClient()
}

// GetHttpClient returns the general outbound client used by relay/provider
// integrations. Do not attach the SSRF-protected dialer here: provider base URLs
// are root/operator-managed deployment targets, not arbitrary user-controlled
// input, and may legitimately point at private networks, private-link endpoints,
// self-hosted services, or local proxies. Code paths that fetch arbitrary
// user-controlled URLs must use GetSSRFProtectedHTTPClient or
// ValidateSSRFProtectedFetchURL instead.
func GetHttpClient() *http.Client {
	return httpClient
}

// GetSSRFProtectedHTTPClient 返回带拨号时 SSRF 校验的客户端。
// ssrfProtectedHTTPClient 由 InitHttpClient 在启动时初始化，运行期只读。
func GetSSRFProtectedHTTPClient() *http.Client {
	if fetchSetting := system_setting.GetFetchSetting(); fetchSetting != nil && !fetchSetting.EnableSSRFProtection {
		return GetHttpClient()
	}
	return ssrfProtectedHTTPClient
}

// GetHttpClientWithProxy returns the default client or a proxy-enabled one when proxyURL is provided.
func GetHttpClientWithProxy(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return GetHttpClient(), nil
	}
	return NewProxyHttpClient(proxyURL)
}

// ResetProxyClientCache 清空代理客户端缓存，确保下次使用时重新初始化
func ResetProxyClientCache() {
	proxyClientLock.Lock()
	defer proxyClientLock.Unlock()
	for _, client := range proxyClients {
		if transport, ok := client.Transport.(*http.Transport); ok && transport != nil {
			transport.CloseIdleConnections()
		}
	}
	proxyClients = make(map[string]*http.Client)
}

// NewProxyHttpClient 创建支持代理的 HTTP 客户端
func NewProxyHttpClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		if client := GetHttpClient(); client != nil {
			return client, nil
		}
		return http.DefaultClient, nil
	}

	proxyClientLock.Lock()
	if client, ok := proxyClients[proxyURL]; ok {
		proxyClientLock.Unlock()
		return client, nil
	}
	proxyClientLock.Unlock()

	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	switch parsedURL.Scheme {
	case "http", "https":
		transport := newRelayHTTPTransport(http.ProxyURL(parsedURL), nil)
		client := newRelayHTTPClient(transport)
		proxyClientLock.Lock()
		proxyClients[proxyURL] = client
		proxyClientLock.Unlock()
		return client, nil

	case "socks5", "socks5h":
		// 获取认证信息
		var auth *proxy.Auth
		if parsedURL.User != nil {
			auth = &proxy.Auth{
				User:     parsedURL.User.Username(),
				Password: "",
			}
			if password, ok := parsedURL.User.Password(); ok {
				auth.Password = password
			}
		}

		// 创建 SOCKS5 代理拨号器。基础 net.Dialer 和 SOCKS dialer 都必须
		// 保留 Context，以便客户端取消能中止 TCP 连接和 SOCKS 握手。
		forwardDialer := &net.Dialer{KeepAlive: 30 * time.Second}
		// proxy.SOCKS5 使用 tcp 参数，所有 TCP 连接包括 DNS 查询都将通过代理进行。行为与 socks5h 相同
		dialer, err := proxy.SOCKS5("tcp", parsedURL.Host, auth, forwardDialer)
		if err != nil {
			return nil, err
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS5 dialer for %s does not support context cancellation", parsedURL.Host)
		}

		transport := newRelayHTTPTransport(nil, contextDialer.DialContext)
		client := newRelayHTTPClient(transport)
		proxyClientLock.Lock()
		proxyClients[proxyURL] = client
		proxyClientLock.Unlock()
		return client, nil

	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s, must be http, https, socks5 or socks5h", parsedURL.Scheme)
	}
}
