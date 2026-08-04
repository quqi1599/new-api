package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseProxyURLStrict(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", raw: " HTTP://proxy.example:8080/ ", want: "http://proxy.example:8080"},
		{name: "socks default port", raw: "socks5://proxy.example", want: "socks5://proxy.example:1080"},
		{name: "unsupported", raw: "ftp://proxy.example", wantErr: true},
		{name: "missing host", raw: "http:///path", wantErr: true},
		{name: "bad port", raw: "http://proxy.example:70000", wantErr: true},
		{name: "path", raw: "http://proxy.example/path", wantErr: true},
		{name: "query", raw: "http://proxy.example?x=1", wantErr: true},
		{name: "fragment", raw: "http://proxy.example#x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseProxyURLStrict(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.want == "" {
				require.Nil(t, parsed)
				return
			}
			require.Equal(t, tt.want, parsed.String())
		})
	}
}

func TestParseProxyURLRuntimeStripsLegacySuffix(t *testing.T) {
	parsed, stripped, err := ParseProxyURLRuntime("https://user:pass@proxy.example:8443/legacy?x=1#fragment")
	require.NoError(t, err)
	require.True(t, stripped)
	require.Equal(t, "https://user:pass@proxy.example:8443", parsed.String())
}
