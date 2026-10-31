// Package websearch executes platform-owned web tools against DeepSeek. Model
// generation and wallet settlement remain owned by the existing relay.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
)

const MaxCalls = 32
const MaxBody = 4 << 20
const SearchFunction = "newapi_web_search"
const FetchFunction = "newapi_web_fetch"

type requestHookKey struct{}
type auditInputKey struct{}

func WithAuditInput(ctx context.Context, input json.RawMessage) context.Context {
	return context.WithValue(ctx, auditInputKey{}, input)
}

type GuardError struct{ Status int }

func (e *GuardError) Error() string {
	return "web tool request was stopped by the platform content audit"
}

func WithRequestHook(ctx context.Context, hook func(*http.Request) error) context.Context {
	return context.WithValue(ctx, requestHookKey{}, hook)
}

func Enabled() bool { return os.Getenv("UNIFIED_WEB_SEARCH_ENABLED") == "true" }

type SearchRequest struct {
	Query       string `json:"query"`
	Count       int    `json:"count,omitempty"`
	NeedContent bool   `json:"need_content"`
}
type Hit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Summary string `json:"summary"`
	Content string `json:"content"`
}
type Page struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}
type Backend interface {
	Search(context.Context, SearchRequest) ([]Hit, error)
	Fetch(context.Context, string) (Page, error)
}
type Client struct {
	Key                   string
	BaseURL               string
	HTTP                  *http.Client
	mu                    sync.Mutex
	nextSearch, nextFetch time.Time
}

var productionClient struct {
	sync.Mutex
	client *Client
}

func ProductionClient() (*Client, error) {
	productionClient.Lock()
	defer productionClient.Unlock()
	if productionClient.client != nil {
		return productionClient.client, nil
	}
	base := strings.TrimRight(os.Getenv("DEEPSEEK_SEARCH_PROXY_URL"), "/")
	u, parseErr := url.Parse(base)
	if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("audited DeepSeek search proxy URL is not configured")
	}
	key := ""
	if path := os.Getenv("DEEPSEEK_SEARCH_PROXY_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("DeepSeek search credential file is unavailable")
		}
		key = strings.TrimSpace(string(data))
	}
	if key == "" {
		return nil, errors.New("DeepSeek search credential is not configured")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 16
	productionClient.client = &Client{Key: key, BaseURL: base, HTTP: &http.Client{Timeout: 45 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return productionClient.client, nil
}

// Limits are per designated account, shared across search sessions. A cancelled
// reservation is deliberately not reused; it must not create a rate burst.
func (c *Client) wait(ctx context.Context, fetch bool) error {
	c.mu.Lock()
	next, interval := &c.nextSearch, 10*time.Millisecond
	if fetch {
		next, interval = &c.nextFetch, 40*time.Millisecond
	}
	at := time.Now()
	if next.After(at) {
		at = *next
	}
	*next = at.Add(interval)
	c.mu.Unlock()
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) call(ctx context.Context, endpoint string, request, result any) error {
	if err := c.wait(ctx, endpoint == "fetch"); err != nil {
		return err
	}
	body, err := common.Marshal(request)
	if err != nil {
		return err
	}
	if input, ok := ctx.Value(auditInputKey{}).(json.RawMessage); ok && len(input) > 0 {
		if c.BaseURL == "" {
			return errors.New("audit context must not be sent directly to a search provider")
		}
		fields, _ := Obj(body)
		fields["input"] = input
		body = Raw(fields)
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.deepseek.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/tools/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	if hook, ok := ctx.Value(requestHookKey{}).(func(*http.Request) error); ok {
		if err := hook(req); err != nil {
			return err
		}
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("DeepSeek %s transport failed", endpoint)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.Header.Get("X-CPA-Local-Guard") == "content-audit" {
			return &GuardError{Status: response.StatusCode}
		}
		return fmt.Errorf("DeepSeek %s returned HTTP %d", endpoint, response.StatusCode)
	}
	if c.BaseURL != "" && response.Header.Get("X-CPA-Web-Provider") != "deepseek" {
		return errors.New("search proxy did not confirm the DeepSeek provider")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBody+1))
	if err != nil {
		return errors.New("DeepSeek web result could not be read")
	}
	if len(data) > MaxBody {
		return errors.New("DeepSeek web result exceeds the response limit")
	}
	if err := common.Unmarshal(data, result); err != nil {
		return errors.New("DeepSeek returned an invalid web result")
	}
	return nil
}

func (c *Client) Search(ctx context.Context, request SearchRequest) ([]Hit, error) {
	if strings.TrimSpace(request.Query) == "" || utf8.RuneCountInString(request.Query) > 128 {
		return nil, errors.New("search query must contain 1 to 128 Unicode characters")
	}
	if request.Count == 0 {
		request.Count = 10
	}
	if request.Count < 1 || request.Count > 50 {
		return nil, errors.New("search count must be between 1 and 50")
	}
	var result struct {
		Results *[]Hit `json:"results"`
	}
	if err := c.call(ctx, "search", request, &result); err != nil {
		return nil, err
	}
	if result.Results == nil {
		return nil, errors.New("DeepSeek search omitted its results array")
	}
	for _, hit := range *result.Results {
		if !PublicURL(hit.URL) {
			return nil, errors.New("DeepSeek search returned an invalid result URL")
		}
	}
	return *result.Results, nil
}
func (c *Client) Fetch(ctx context.Context, target string) (Page, error) {
	if !PublicURL(target) {
		return Page{}, errors.New("fetch requires a public HTTP(S) URL without credentials")
	}
	var result Page
	err := c.call(ctx, "fetch", map[string]string{"url": target}, &result)
	return result, err
}

func PublicURL(value string) bool {
	if len(value) > 8192 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast()) {
		return false
	}
	return true
}
