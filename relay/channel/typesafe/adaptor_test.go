package typesafe

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/stretchr/testify/require"
)

func TestDecisionsUpstreamPath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "https://api.typesafe.ai/v1/systemone"},
		{"/alpha/decisions", "https://api.typesafe.ai/alpha/decisions"},
		{"v1/systemone", "https://api.typesafe.ai/v1/systemone"},
		{"//other.example/api", ""},
		{"/../other", ""},
		{"/%2e%2e/other", ""},
		{"/v1/systemone?secret=value", ""},
		{"/v1/systemone#fragment", ""},
	} {
		info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeDecisions, ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://api.typesafe.ai/", ChannelSetting: dto.ChannelSettings{DecisionsUpstreamPath: tc.path}}}
		got, err := (&Adaptor{}).GetRequestURL(info)
		if tc.want == "" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		}
	}
}
