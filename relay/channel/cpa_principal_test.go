package channel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestCPAPrincipalCannotBeOverriddenOrSharedAcrossCustomers(t *testing.T) {
	info := &relaycommon.RelayInfo{UserId: 12345, TokenId: 67890, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "fixture-channel-credential"}}
	claim := func(authority string) string {
		header := http.Header{"x-cpa-principal": {"client-forged"}, "X-CPA-PRINCIPAL": {"override-forged"}}
		applyCPAPrincipalHeader(header, info, authority)
		require.Len(t, header, 1)
		value := header.Get(cpaPrincipalHeader)
		require.Regexp(t, `^[a-f0-9]{64}$`, value)
		require.NotContains(t, value, "fixture-channel-credential")
		return value
	}
	first := claim("https://fixture.newapi.test/")
	require.Equal(t, first, claim("https://fixture.newapi.test"))
	info.UserId++
	require.NotEqual(t, first, claim("https://fixture.newapi.test"))
	info.UserId--
	info.TokenId++
	require.NotEqual(t, first, claim("https://fixture.newapi.test"))
	info.TokenId--
	require.NotEqual(t, first, claim("https://other.newapi.test"))
	info.ApiKey = "different-fixture-credential"
	require.NotEqual(t, first, claim("https://fixture.newapi.test"))
}

func TestCPAPrincipalOverridesClientAndChannelOnActualTransport(t *testing.T) {
	service.InitHttpClient()
	previousAuthority := system_setting.ServerAddress
	system_setting.ServerAddress = "https://synthetic-newapi.test"
	t.Cleanup(func() { system_setting.ServerAddress = previousAuthority })
	for _, socket := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "websocket"}[socket], func(t *testing.T) {
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				if socket {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err == nil {
						_ = conn.Close()
					}
				} else {
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer upstream.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set(cpaPrincipalHeader, "client-forged")
			info := &relaycommon.RelayInfo{UserId: 12, TokenId: 34, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "fixture-channel-credential", HeadersOverride: map[string]interface{}{cpaPrincipalHeader: "channel-forged"}}}
			info.UseRuntimeHeadersOverride = true
			info.RuntimeHeadersOverride = map[string]interface{}{cpaPrincipalHeader: "runtime-forged"}
			address := upstream.URL
			if socket {
				address = strings.Replace(address, "http:", "ws:", 1)
			}
			adapter := &contextTestAdaptor{requestURL: address}
			if socket {
				conn, err := DoWssRequest(adapter, c, info, nil)
				require.NoError(t, err)
				_ = conn.Close()
			} else {
				response, err := DoApiRequest(adapter, c, info, strings.NewReader(`{}`))
				require.NoError(t, err)
				_ = response.Body.Close()
			}
			expected := http.Header{}
			applyCPAPrincipal(expected, info)
			received := <-seen
			require.Equal(t, expected.Get(cpaPrincipalHeader), received.Get(cpaPrincipalHeader))
			require.Regexp(t, `^[a-f0-9]{64}$`, received.Get(cpaPrincipalHeader))
		})
	}
}

func TestCPAPrincipalRemovesSpoofWithoutAuthenticatedIdentity(t *testing.T) {
	for _, info := range []*relaycommon.RelayInfo{nil, {}, {UserId: 1, TokenId: 2, IsChannelTest: true, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "fixture"}}} {
		header := http.Header{"x-cpa-principal": {"client-forged"}, "X-CPA-PRINCIPAL": {"override-forged"}}
		applyCPAPrincipalHeader(header, info, "https://fixture.test")
		require.Empty(t, header)
	}
	for _, authority := range []string{"", "http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000", "http://0.0.0.0:3000", "not-an-authority"} {
		header := http.Header{cpaPrincipalHeader: {"forged"}}
		applyCPAPrincipalHeader(header, &relaycommon.RelayInfo{UserId: 1, TokenId: 2, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "fixture"}}, authority)
		require.Empty(t, header, "unconfigured instances must not share a replay principal")
	}
}
