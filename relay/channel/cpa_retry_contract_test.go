package channel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cpaRetryBarrierFixture struct {
	context  *gin.Context
	request  *http.Request
	response *http.Response
	info     *relaycommon.RelayInfo
}

func newCPARetryBarrierFixture() cpaRetryBarrierFixture {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req := httptest.NewRequest(http.MethodPost, "https://cpa.example/v1/responses", nil)
	responseRequest := httptest.NewRequest(http.MethodPost, "https://cpa.example/v1/responses", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	info.ChannelOtherSettings.CPAAuditIdentityEnabled = true
	header := http.Header{}
	header.Set("X-CPA-Retry-Scope", "request")
	header.Set("X-CPA-Retry-Reason", "upstream_output_observed")
	header.Set("X-CPA-Retry-Attempts", "1")
	return cpaRetryBarrierFixture{
		context: c, request: req, info: info,
		response: &http.Response{StatusCode: http.StatusBadGateway, Header: header, Request: responseRequest},
	}
}

func (f cpaRetryBarrierFixture) capture() string {
	captureCPARetryBarrier(f.context, f.request, f.response, f.info)
	return service.GatewayReplayStopReason(f.context)
}

func TestCPARetryBarrierCapturesFixedReasonsAcrossHTTPStatuses(t *testing.T) {
	for _, reason := range []string{"upstream_output_observed", "accepted_outcome_unknown"} {
		for _, status := range []int{200, 204, 299, 400, 429, 500, 599} {
			for _, attempts := range []string{"1", "25", "1048576"} {
				t.Run(fmt.Sprintf("%s/status=%d/attempts=%s", reason, status, attempts), func(t *testing.T) {
					f := newCPARetryBarrierFixture()
					f.response.StatusCode = status
					f.response.Header.Set("X-CPA-Retry-Reason", reason)
					f.response.Header.Set("X-CPA-Retry-Attempts", attempts)
					require.Equal(t, reason, f.capture())
				})
			}
		}
	}
}

func TestCPARetryBarrierRequiresTrustedChannelAndActualResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*cpaRetryBarrierFixture)
	}{
		{"nil info", func(f *cpaRetryBarrierFixture) { f.info = nil }},
		{"nil channel metadata", func(f *cpaRetryBarrierFixture) { f.info.ChannelMeta = nil }},
		{"untrusted channel", func(f *cpaRetryBarrierFixture) { f.info.ChannelOtherSettings.CPAAuditIdentityEnabled = false }},
		{"nil outbound request", func(f *cpaRetryBarrierFixture) { f.request = nil }},
		{"nil outbound URL", func(f *cpaRetryBarrierFixture) { f.request.URL = nil }},
		{"nil response", func(f *cpaRetryBarrierFixture) { f.response = nil }},
		{"nil response request", func(f *cpaRetryBarrierFixture) { f.response.Request = nil }},
		{"nil response request URL", func(f *cpaRetryBarrierFixture) { f.response.Request.URL = nil }},
		{"nil response headers", func(f *cpaRetryBarrierFixture) { f.response.Header = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPARetryBarrierFixture()
			common2.SetContextKey(f.context, constant.ContextKeyRelayGatewayNoReplayReason, "upstream_output_observed")
			tc.change(&f)
			require.Empty(t, f.capture(), "a rejected capture must also remove prior attempt metadata")
		})
	}
	f := newCPARetryBarrierFixture()
	require.NotPanics(t, func() { captureCPARetryBarrier(nil, f.request, f.response, f.info) })
}

func TestCPARetryBarrierRequiresSameHTTPOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, requestURL, responseURL string
		want                          bool
	}{
		{"same origin", "https://cpa.example/v1/responses", "https://cpa.example/v1/responses", true},
		{"same origin redirect", "https://cpa.example/v1/responses", "https://cpa.example/other?route=2", true},
		{"host case", "https://CPA.EXAMPLE/v1/responses", "https://cpa.example/v1/responses", true},
		{"default HTTPS port", "https://cpa.example/v1/responses", "https://cpa.example:443/v1/responses", true},
		{"default HTTP port", "http://cpa.example:80/v1/responses", "http://cpa.example/v1/responses", true},
		{"IPv6 default port", "https://[::1]/v1/responses", "https://[::1]:443/v1/responses", true},
		{"different host", "https://cpa.example/v1/responses", "https://other.example/v1/responses", false},
		{"subdomain redirect", "https://cpa.example/v1/responses", "https://sub.cpa.example/v1/responses", false},
		{"HTTPS downgrade", "https://cpa.example/v1/responses", "http://cpa.example/v1/responses", false},
		{"different port", "https://cpa.example/v1/responses", "https://cpa.example:8443/v1/responses", false},
		{"outbound userinfo", "https://user@cpa.example/v1/responses", "https://cpa.example/v1/responses", false},
		{"redirect userinfo", "https://cpa.example/v1/responses", "https://user@cpa.example/v1/responses", false},
		{"unsupported scheme", "ftp://cpa.example/v1/responses", "ftp://cpa.example/v1/responses", false},
		{"missing origin", "/v1/responses", "/v1/responses", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPARetryBarrierFixture()
			var err error
			f.request.URL, err = url.Parse(tc.requestURL)
			require.NoError(t, err)
			f.response.Request.URL, err = url.Parse(tc.responseURL)
			require.NoError(t, err)
			if tc.want {
				require.Equal(t, "upstream_output_observed", f.capture())
			} else {
				require.Empty(t, f.capture())
			}
		})
	}
}

func TestCPARetryBarrierChecksEveryRedirectHop(t *testing.T) {
	previous := func(current *http.Response, address string) *http.Response {
		prior := &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Request:    httptest.NewRequest(http.MethodPost, address, nil),
		}
		current.Request.Response = prior
		return prior
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Response)
		want   string
	}{
		{"same origin multiple hops", func(final *http.Response) {
			middle := previous(final, "https://cpa.example:443/middle")
			previous(middle, "https://cpa.example/start")
		}, "upstream_output_observed"},
		{"leaves origin then returns", func(final *http.Response) {
			middle := previous(final, "https://untrusted.example/middle")
			previous(middle, "https://cpa.example/start")
		}, ""},
		{"prior response lacks request", func(final *http.Response) {
			final.Request.Response = &http.Response{StatusCode: http.StatusTemporaryRedirect}
		}, ""},
		{"cyclic redirect history", func(final *http.Response) {
			middle := previous(final, "https://cpa.example/middle")
			middle.Request.Response = final
		}, ""},
		{"excessive redirect history", func(final *http.Response) {
			current := final
			for hop := 0; hop < 16; hop++ {
				current = previous(current, fmt.Sprintf("https://cpa.example/hop/%d", hop))
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPARetryBarrierFixture()
			common2.SetContextKey(f.context, constant.ContextKeyRelayGatewayNoReplayReason, "accepted_outcome_unknown")
			tc.change(f.response)
			require.Equal(t, tc.want, f.capture())
		})
	}
}

func TestCPARetryBarrierRejectsMalformedHeadersAndStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		values       []string
	}{
		{"missing scope", "X-CPA-Retry-Scope", nil},
		{"unknown scope", "X-CPA-Retry-Scope", []string{"gateway"}},
		{"duplicate scope", "X-CPA-Retry-Scope", []string{"request", "request"}},
		{"combined scope", "X-CPA-Retry-Scope", []string{"request,request"}},
		{"missing reason", "X-CPA-Retry-Reason", nil},
		{"unknown reason", "X-CPA-Retry-Reason", []string{"retry_later"}},
		{"duplicate reason", "X-CPA-Retry-Reason", []string{"upstream_output_observed", "upstream_output_observed"}},
		{"combined reason", "X-CPA-Retry-Reason", []string{"upstream_output_observed,accepted_outcome_unknown"}},
		{"missing attempts", "X-CPA-Retry-Attempts", nil},
		{"zero attempts", "X-CPA-Retry-Attempts", []string{"0"}},
		{"negative attempts", "X-CPA-Retry-Attempts", []string{"-1"}},
		{"signed attempts", "X-CPA-Retry-Attempts", []string{"+1"}},
		{"noncanonical attempts", "X-CPA-Retry-Attempts", []string{"01"}},
		{"fractional attempts", "X-CPA-Retry-Attempts", []string{"1.0"}},
		{"duplicate attempts", "X-CPA-Retry-Attempts", []string{"1", "1"}},
		{"combined attempts", "X-CPA-Retry-Attempts", []string{"1,1"}},
		{"out of bounds attempts", "X-CPA-Retry-Attempts", []string{"1048577"}},
		{"overflow attempts", "X-CPA-Retry-Attempts", []string{"999999999999999999999999"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPARetryBarrierFixture()
			f.response.Header[http.CanonicalHeaderKey(tc.header)] = tc.values
			require.Empty(t, f.capture())
		})
	}
	for _, status := range []int{0, 100, 199, 300, 302, 399, 600} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			f := newCPARetryBarrierFixture()
			f.response.StatusCode = status
			require.Empty(t, f.capture())
		})
	}
}

func TestCPARetryBarrierIgnoresClientClaimsAndClearsEachCapture(t *testing.T) {
	f := newCPARetryBarrierFixture()
	f.context.Request = httptest.NewRequest(http.MethodPost, "/v1/responses?retry_scope=request", strings.NewReader(`{"retry_scope":"request","retry_reason":"upstream_output_observed"}`))
	f.context.Request.Header = f.response.Header.Clone()
	f.request.Header = f.response.Header.Clone()
	f.response.Header = http.Header{}
	require.Empty(t, f.capture(), "client and outbound headers do not establish a response contract")

	f.response.Header = f.request.Header.Clone()
	require.Equal(t, "upstream_output_observed", f.capture())
	f.response.Header = http.Header{}
	require.Empty(t, f.capture(), "a later unmarked response cannot inherit the previous barrier")
	f.response.Header = f.request.Header.Clone()
	f.response.Header.Set("X-CPA-Retry-Reason", "accepted_outcome_unknown")
	require.Equal(t, "accepted_outcome_unknown", f.capture())
	f.info.ChannelOtherSettings.CPAAuditIdentityEnabled = false
	require.Empty(t, f.capture(), "a later untrusted channel cannot inherit the previous barrier")
}

func TestStripCPARetryRequestHeadersIsCaseInsensitive(t *testing.T) {
	header := http.Header{
		"x-cpa-retry-scope":    {"request"},
		"X-Cpa-Retry-Reason":   {"upstream_output_observed"},
		"X-CPA-RETRY-ATTEMPTS": {"1"},
		"X-CPA-Principal":      {"preserved-principal"},
		"X-Unrelated":          {"preserved"},
		"Retry-After":          {"17"},
	}
	stripCPARetryRequestHeaders(header)
	require.Equal(t, http.Header{
		"X-CPA-Principal": {"preserved-principal"},
		"X-Unrelated":     {"preserved"},
		"Retry-After":     {"17"},
	}, header)
	require.NotPanics(t, func() { stripCPARetryRequestHeaders(nil) })
}
