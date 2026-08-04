package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"golang.org/x/net/proxy"
)

var (
	httpClient              *http.Client
	ssrfProtectedHTTPClient *http.Client
	proxyClients            = proxyHTTPClientCache{
		clients: make(map[string]*http.Client),
		aliases: make(map[string]string),
	}
	legacyProxyURLWarnings sync.Map
)

type proxyHTTPClientCache struct {
	mutex   sync.RWMutex
	clients map[string]*http.Client
	aliases map[string]string
}

type proxyURLConfig struct {
	parsedURL *url.URL
	cacheKey  string
}

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

func newProxyURLConfig(parsedURL *url.URL) *proxyURLConfig {
	return &proxyURLConfig{parsedURL: parsedURL, cacheKey: parsedURL.String()}
}

func warnLegacyProxyURLOnce(config *proxyURLConfig) {
	if _, loaded := legacyProxyURLWarnings.LoadOrStore(config.cacheKey, struct{}{}); loaded {
		return
	}
	logger.LogWarn(context.Background(), fmt.Sprintf(
		"legacy proxy URL suffix ignored at runtime: scheme=%s host=%s; update the channel proxy setting",
		config.parsedURL.Scheme,
		config.parsedURL.Host,
	))
}

// NormalizeProxyURL validates a proxy URL and returns its canonical cache key.
func NormalizeProxyURL(rawProxyURL string) (string, error) {
	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(rawProxyURL)
	if err != nil {
		return "", err
	}
	if parsedURL == nil {
		return "", nil
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	return config.cacheKey, nil
}

func ValidateProxyURL(rawProxyURL string) error {
	_, err := common.ParseProxyURLStrict(rawProxyURL)
	return err
}

func (cache *proxyHTTPClientCache) get(rawCacheKey string) (*http.Client, bool) {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()
	cacheKey := rawCacheKey
	if canonicalKey, ok := cache.aliases[rawCacheKey]; ok {
		cacheKey = canonicalKey
	}
	client, ok := cache.clients[cacheKey]
	return client, ok
}

func (cache *proxyHTTPClientCache) getOrCreate(rawCacheKey string, config *proxyURLConfig) (*http.Client, error) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if client, ok := cache.clients[config.cacheKey]; ok {
		cache.aliases[rawCacheKey] = config.cacheKey
		return client, nil
	}
	client, err := newProxyHTTPClient(config.parsedURL)
	if err != nil {
		return nil, err
	}
	cache.clients[config.cacheKey] = client
	cache.aliases[rawCacheKey] = config.cacheKey
	return client, nil
}

func (cache *proxyHTTPClientCache) remove(cacheKey string) *http.Client {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	client := cache.clients[cacheKey]
	delete(cache.clients, cacheKey)
	for alias, canonicalKey := range cache.aliases {
		if canonicalKey == cacheKey {
			delete(cache.aliases, alias)
		}
	}
	return client
}

func (cache *proxyHTTPClientCache) reset() map[string]*http.Client {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	oldClients := cache.clients
	cache.clients = make(map[string]*http.Client)
	cache.aliases = make(map[string]string)
	return oldClients
}

func newProxyHTTPClient(proxyURL *url.URL) (*http.Client, error) {
	switch proxyURL.Scheme {
	case "http", "https":
		return newRelayHTTPClient(newRelayHTTPTransport(http.ProxyURL(proxyURL), nil)), nil
	case "socks5", "socks5h":
		forwardDialer := &net.Dialer{KeepAlive: 30 * time.Second}
		dialer, err := proxy.FromURL(proxyURL, forwardDialer)
		if err != nil {
			return nil, err
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS proxy dialer does not support context cancellation")
		}
		return newRelayHTTPClient(newRelayHTTPTransport(nil, contextDialer.DialContext)), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme")
	}
}

// GetHttpClientWithProxy returns the default client or one shared by canonical proxy URL.
func GetHttpClientWithProxy(rawProxyURL string) (*http.Client, error) {
	trimmedProxyURL := strings.TrimSpace(rawProxyURL)
	if trimmedProxyURL == "" {
		if client := GetHttpClient(); client != nil {
			return client, nil
		}
		return http.DefaultClient, nil
	}
	if client, ok := proxyClients.get(trimmedProxyURL); ok {
		return client, nil
	}
	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(trimmedProxyURL)
	if err != nil {
		return nil, err
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	return proxyClients.getOrCreate(trimmedProxyURL, config)
}

func InvalidateProxyClient(rawProxyURL string) {
	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(rawProxyURL)
	if err != nil || parsedURL == nil {
		return
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	if client := proxyClients.remove(config.cacheKey); client != nil {
		client.CloseIdleConnections()
	}
}

func ResetProxyClientCache() {
	for _, client := range proxyClients.reset() {
		client.CloseIdleConnections()
	}
}

// NewProxyHttpClient is retained for compatibility.
// Deprecated: use GetHttpClientWithProxy.
func NewProxyHttpClient(proxyURL string) (*http.Client, error) {
	return GetHttpClientWithProxy(proxyURL)
}
