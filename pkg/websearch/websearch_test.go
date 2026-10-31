package websearch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type memoryStore map[string]Record

func (m memoryStore) Put(_ context.Context, id string, r Record) error { m[id] = r; return nil }
func (m memoryStore) Get(_ context.Context, id string) (Record, error) {
	r, ok := m[id]
	if !ok {
		return r, errors.New("missing reference")
	}
	return r, nil
}

type fakeBackend struct {
	searches, fetches int
	empty             bool
	fail              bool
}

func (f *fakeBackend) Search(_ context.Context, r SearchRequest) ([]Hit, error) {
	f.searches++
	if f.fail {
		return nil, errors.New("unavailable")
	}
	if f.empty {
		return []Hit{}, nil
	}
	return []Hit{{Title: r.Query, URL: "https://example.com/news", Summary: "source text"}}, nil
}
func (f *fakeBackend) Fetch(_ context.Context, u string) (Page, error) {
	f.fetches++
	if f.fail {
		return Page{}, errors.New("unavailable")
	}
	if f.empty {
		return Page{}, nil
	}
	return Page{Title: u, Content: "first\nsecond [link](https://example.com/next)\nneedle\n"}, nil
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeepSeekHTTPBillingConditions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		search     bool
		want       int
		failure    bool
	}{
		{"search", `{"results":[{"title":"x","url":"https://example.com"}]}`, 200, true, 1, false},
		{"empty", `{"results":[]}`, 200, true, 0, false},
		{"missing", `{}`, 200, true, 0, true},
		{"null", `{"results":null}`, 200, true, 0, true},
		{"badurl", `{"results":[{"url":"http://127.0.0.1"}]}`, 200, true, 0, true},
		{"fetch", `{"title":"x","content":"body"}`, 200, false, 1, false},
		{"blank", `{"content":" \n"}`, 200, false, 0, false},
		{"failure", `{"error":"secret detail"}`, 429, true, 0, true},
		{"redirect", `{}`, 302, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{Key: "synthetic", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "api.deepseek.com", r.URL.Host)
				require.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			s := &Session{Backend: client}
			var err error
			if tc.search {
				_, err = s.Search(context.Background(), SearchRequest{Query: "test"})
			} else {
				_, err = s.Fetch(context.Background(), "https://example.com")
			}
			require.Equal(t, tc.failure, err != nil)
			require.Equal(t, tc.want, s.Meter.Search+s.Meter.Fetch)
		})
	}
}
func TestManagedPlanPreservesModelControlsAndNumbers(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","input":"query","instructions":"keep","temperature":0,"metadata":{"n":9007199254740993},"tools":[{"type":"web_search"},{"type":"function","name":"other","parameters":{"type":"object"}}],"tool_choice":{"type":"web_search"}}`)
	p, err := NewPlan(body, "responses")
	require.NoError(t, err)
	require.NoError(t, p.Tools())
	encoded := Raw(p.Body)
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(encoded, "model").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(encoded, "metadata.n").Raw)
	require.Equal(t, "0", gjson.GetBytes(encoded, "temperature").Raw)
	require.Equal(t, SearchFunction, gjson.GetBytes(encoded, "tool_choice.name").String())
	require.Equal(t, 3, len(Arr(p.Body["tools"])))
	require.NotContains(t, string(encoded), `"type":"web_search"`)
}
func TestManagedPlanRejectsLostSemanticsAndDoesNotStealClientFunction(t *testing.T) {
	for _, tool := range []string{`{"type":"web_search","external_web_access":false}`, `{"type":"web_search","user_location":{"city":"Paris"}}`, `{"type":"web_search","max_uses":0}`, `{"type":"web_search","max_uses":1.5}`, `{"type":"web_search","filters":{"allowed_domains":["x.com OR evil"]}}`} {
		_, err := NewPlan([]byte(`{"tools":[`+tool+`]}`), "responses")
		require.Error(t, err)
	}
	p, err := NewPlan([]byte(`{"tools":[{"type":"function","name":"web_search"}]}`), "responses")
	require.NoError(t, err)
	require.Nil(t, p)
}
func TestManagedSearchHistoryRoundTrip(t *testing.T) {
	for _, protocol := range []string{"responses", "claude"} {
		t.Run(protocol, func(t *testing.T) {
			store := memoryStore{}
			backend := &fakeBackend{}
			s := &Session{Backend: backend, Store: store}
			p := &Plan{Protocol: protocol, Search: true, Fetch: true, Limit: 2}
			original := Raw(map[string]any{"type": "function_call", "id": "fc1", "call_id": "call1", "name": SearchFunction, "arguments": `{"query":"test"}`})
			if protocol == "claude" {
				original = Raw(map[string]any{"type": "tool_use", "id": "call1", "name": SearchFunction, "input": map[string]string{"query": "test"}})
			}
			e, err := p.Execute(context.Background(), s, Call{ID: "call1", Name: SearchFunction, Arguments: []byte(`{"query":"test"}`), Original: original})
			require.NoError(t, err)
			require.Equal(t, 1, s.Meter.Search)
			pub := e.Public(protocol)
			if protocol == "responses" {
				p.Body = Object{"input": Raw(pub)}
			} else {
				p.Body = Object{"messages": Raw([]any{map[string]any{"role": "assistant", "content": pub}})}
			}
			require.NoError(t, p.Restore(context.Background(), store))
			wire := string(Raw(p.Body))
			require.Contains(t, wire, SearchFunction)
			require.Contains(t, wire, "source text")
			require.NotContains(t, wire, e.PublicID)
			other := memoryStore{}
			if protocol == "responses" {
				p.Body = Object{"input": Raw(pub)}
			} else {
				p.Body = Object{"messages": Raw([]any{map[string]any{"role": "assistant", "content": pub}})}
			}
			require.Error(t, p.Restore(context.Background(), other))
			require.Equal(t, 1, backend.searches)
		})
	}
}
func TestAlphaSearchOpenFindClickAndEmptyCharges(t *testing.T) {
	backend := &fakeBackend{}
	s := &Session{Backend: backend, Store: memoryStore{}}
	res, err := Alpha(context.Background(), s, []byte(`{"commands":{"search_query":[{"q":"one"},{"q":"two"}]}}`))
	require.NoError(t, err)
	require.Equal(t, 2, s.Meter.Search)
	ref := gjson.GetBytes(Raw(res), "results.0.ref_id").String()
	require.NotEmpty(t, ref)
	res, err = Alpha(context.Background(), s, []byte(`{"commands":{"open":[{"ref_id":"`+ref+`"}]}}`))
	require.NoError(t, err)
	require.Equal(t, 1, s.Meter.Fetch)
	pageRef := gjson.GetBytes(Raw(res), "results.0.ref_id").String()
	_, err = Alpha(context.Background(), s, []byte(`{"commands":{"find":[{"ref_id":"`+pageRef+`","pattern":"needle"}],"click":[{"ref_id":"`+pageRef+`","id":0}]}}`))
	require.NoError(t, err)
	require.Equal(t, 2, s.Meter.Fetch)
	before := backend.searches
	_, err = Alpha(context.Background(), s, []byte(`{"commands":{"search_query":[{"q":"test"}],"screenshot":[{"ref_id":"x"}]}}`))
	require.Error(t, err)
	require.Equal(t, before, backend.searches)
	backend.empty = true
	_, err = Alpha(context.Background(), s, []byte(`{"commands":{"search_query":[{"q":"empty"}]}}`))
	require.NoError(t, err)
	require.Equal(t, 2, s.Meter.Search)
}
func TestEmitSnapshotPreservesToolsAndOpaqueReasoning(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{"responses", `{"id":"resp1","model":"original-model","status":"completed","output":[{"id":"reason1","type":"reasoning","encrypted_content":"opaque"},{"id":"fc1","call_id":"c1","type":"function_call","name":"other","arguments":"{\"n\":9007199254740993}"},{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2}}`},
		{"claude", `{"id":"msg1","type":"message","role":"assistant","model":"original-model","content":[{"type":"thinking","thinking":"reason","signature":"opaque"},{"type":"text","text":"answer"},{"type":"tool_use","id":"t1","name":"other","input":{"n":9007199254740993}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":2}}`},
		{"chat", `{"id":"chat1","object":"chat.completion","model":"original-model","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"reason","tool_calls":[{"id":"c1","type":"function","function":{"name":"other","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			o, err := Obj([]byte(tc.body))
			require.NoError(t, err)
			var b bytes.Buffer
			require.NoError(t, Emit(&b, tc.protocol, o))
			got, err := Snapshot(tc.protocol, b.Bytes())
			require.NoError(t, err)
			require.JSONEq(t, tc.body, string(Raw(got)))
		})
	}
}
func TestPartialOrMalformedStreamIsNotSuccess(t *testing.T) {
	for _, protocol := range []string{"responses", "claude", "chat"} {
		_, err := Snapshot(protocol, []byte("data: {\"error\":{\"message\":\"failed\"}}\n\n"))
		require.Error(t, err)
		_, err = Snapshot(protocol, []byte("data: {}\n\n"))
		require.Error(t, err)
	}
}

func TestDeepSeekHTTPAuditContextAndGuard(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		client := &Client{Key: "synthetic", BaseURL: "http://audited-proxy", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, "original request", gjson.GetBytes(body, "input.0.content").String())
			h := make(http.Header)
			status := 200
			if blocked {
				status = 403
				h.Set("X-CPA-Local-Guard", "content-audit")
			} else {
				h.Set("X-CPA-Web-Provider", "deepseek")
			}
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(`{"results":[{"url":"https://example.com"}]}`))}, nil
		})}}
		session := &Session{Backend: client}
		ctx := WithAuditInput(context.Background(), Raw([]any{map[string]string{"role": "user", "content": "original request"}}))
		_, err := session.Search(ctx, SearchRequest{Query: "test"})
		if blocked {
			var guard *GuardError
			require.ErrorAs(t, err, &guard)
			require.Equal(t, 0, session.Meter.Search)
		} else {
			require.NoError(t, err)
			require.Equal(t, 1, session.Meter.Search)
		}
	}
}

func TestManagedSeparateSearchFetchLimitsAndErrorTypes(t *testing.T) {
	for _, limits := range [][2]int{{1, 3}, {3, 1}} {
		body := Raw(map[string]any{"tools": []any{map[string]any{"type": "web_search_20250305", "max_uses": limits[0]}, map[string]any{"type": "web_fetch_20250910", "max_uses": limits[1]}}})
		plan, err := NewPlan(body, "claude")
		require.NoError(t, err)
		backend := &fakeBackend{}
		session := &Session{Backend: backend, Store: memoryStore{}}
		for j, name := range []string{SearchFunction, FetchFunction} {
			for i := 0; i <= limits[j]; i++ {
				call := Call{ID: "c", Name: name, Arguments: Raw(map[string]string{"query": "test"}), Original: Raw(map[string]string{"id": "c"})}
				if name == FetchFunction {
					call.Arguments = Raw(map[string]string{"url": "https://example.com"})
				}
				e, err := plan.Execute(context.Background(), session, call)
				require.NoError(t, err)
				require.Equal(t, i == limits[j], e.Failed)
				if e.Failed {
					expected := "web_search_tool_result_error"
					if name == FetchFunction {
						expected = "web_fetch_tool_result_error"
					}
					require.Equal(t, expected, gjson.GetBytes(e.Public("claude")[1], "content.type").String())
				}
			}
		}
		require.Equal(t, limits[0], backend.searches)
		require.Equal(t, limits[1], backend.fetches)
	}
}

func TestManagedFetchDomainsRemainIndependentAndNoChargeForFilteredResults(t *testing.T) {
	p, err := NewPlan([]byte(`{"tools":[{"type":"web_search_20250305","allowed_domains":["search.example"]},{"type":"web_fetch_20250910","allowed_domains":["fetch.example"],"max_uses":3}]}`), "claude")
	require.NoError(t, err)
	b := &fakeBackend{}
	s := &Session{Backend: b, Store: memoryStore{}}
	_, err = p.Execute(context.Background(), s, Call{ID: "s", Name: SearchFunction, Arguments: Raw(map[string]string{"query": "test"})})
	require.NoError(t, err)
	require.Zero(t, s.Meter.Search)
	for _, tc := range []struct {
		target string
		failed bool
	}{{"https://sub.fetch.example/page", false}, {"https://search.example/page", true}, {"https://fetch.example.attacker.test", true}} {
		e, err := p.Execute(context.Background(), s, Call{ID: "f", Name: FetchFunction, Arguments: Raw(map[string]string{"url": tc.target})})
		require.NoError(t, err)
		require.Equal(t, tc.failed, e.Failed)
		if tc.failed {
			require.Equal(t, "url_not_allowed", gjson.GetBytes(e.Public("claude")[1], "content.error_code").String())
		}
	}
	require.Equal(t, 1, b.fetches)
	require.Equal(t, 1, s.Meter.Fetch)
	require.False(t, allowedToolURL("https://private.example.com", nil, []string{"example.com"}))
}

func TestAlphaHonorsDomainFiltersBeforeFetchAndBilling(t *testing.T) {
	b := &fakeBackend{}
	s := &Session{Backend: b, Store: memoryStore{}}
	_, err := Alpha(context.Background(), s, []byte(`{"commands":{"search_query":[{"q":"test","domains":["allowed.example"]}]}}`))
	require.NoError(t, err)
	require.Zero(t, s.Meter.Search)
	_, err = Alpha(context.Background(), s, []byte(`{"settings":{"allowed_domains":["allowed.example"]},"commands":{"open":[{"ref_id":"https://example.com"}]}}`))
	require.Error(t, err)
	require.Zero(t, b.fetches)
	require.Zero(t, s.Meter.Fetch)
}
