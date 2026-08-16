package openai

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
)

func TestGetRequestURLPreservesAlphaSearchPathForCompatibleGateway(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode:      relayconstant.RelayModeAlphaSearch,
		RelayFormat:    types.RelayFormatOpenAIAlphaSearch,
		RequestURLPath: "/v1/alpha/search",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://cpa.example",
			ChannelType:    constant.ChannelTypeOpenAI,
		},
	}
	got, err := (&Adaptor{}).GetRequestURL(info)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://cpa.example/v1/alpha/search"
	if got != want {
		t.Fatalf("request URL = %q, want %q", got, want)
	}
}
