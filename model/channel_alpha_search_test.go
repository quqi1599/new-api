package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
)

func TestChannelSupportsAlphaSearchOnlyWhenDeclared(t *testing.T) {
	enabled := `{"alpha_search_enabled":true}`
	disabled := `{"alpha_search_enabled":false}`
	invalid := `{`

	tests := []struct {
		name    string
		channel *Channel
		want    bool
	}{
		{name: "ordinary OpenAI defaults off", channel: &Channel{Type: constant.ChannelTypeOpenAI}},
		{name: "ordinary OpenAI explicit off", channel: &Channel{Type: constant.ChannelTypeOpenAI, Setting: &disabled}},
		{name: "ordinary OpenAI explicit on", channel: &Channel{Type: constant.ChannelTypeOpenAI, Setting: &enabled}, want: true},
		{name: "Codex native support", channel: &Channel{Type: constant.ChannelTypeCodex}, want: true},
		{name: "invalid setting fails closed", channel: &Channel{Type: constant.ChannelTypeOpenAI, Setting: &invalid}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.channel.SupportsEndpointType(constant.EndpointTypeOpenAIAlphaSearch); got != tt.want {
				t.Fatalf("SupportsEndpointType() = %v, want %v", got, tt.want)
			}
		})
	}

	if !(&Channel{}).SupportsEndpointType("") {
		t.Fatal("an unspecified capability must preserve existing selection behavior")
	}
	if (*Channel)(nil).SupportsEndpointType("") {
		t.Fatal("nil channels must never be eligible")
	}
	if !(&Channel{Setting: common.GetPointer(enabled)}).SupportsEndpointType(constant.EndpointTypeOpenAIResponse) {
		t.Fatal("unrelated endpoint types must preserve existing behavior")
	}
}
