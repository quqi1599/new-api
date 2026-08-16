package codex

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
)

func TestGetRequestURLUsesCodexAlphaSearchEndpoint(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeAlphaSearch,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://chatgpt.com",
			ChannelType:    constant.ChannelTypeCodex,
		},
	}
	got, err := (&Adaptor{}).GetRequestURL(info)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://chatgpt.com/backend-api/codex/alpha/search"
	if got != want {
		t.Fatalf("request URL = %q, want %q", got, want)
	}
}
