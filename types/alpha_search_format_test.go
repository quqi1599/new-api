package types

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
)

func TestAlphaSearchFormatCarriesStrictEndpointCapability(t *testing.T) {
	if got := RelayFormatToRequiredEndpointType(RelayFormatOpenAIAlphaSearch); got != constant.EndpointTypeOpenAIAlphaSearch {
		t.Fatalf("required endpoint = %q", got)
	}
	if got := PathToRequiredEndpointType("/v1/alpha/search"); got != constant.EndpointTypeOpenAIAlphaSearch {
		t.Fatalf("path required endpoint = %q", got)
	}
	preferred := RelayFormatToPreferredChannelTypes(RelayFormatOpenAIAlphaSearch)
	if len(preferred) != 2 || preferred[0] != constant.ChannelTypeOpenAI || preferred[1] != constant.ChannelTypeCodex {
		t.Fatalf("preferred channel types = %#v", preferred)
	}
	if got := RelayFormatToRequiredEndpointType(RelayFormatOpenAIResponses); got != "" {
		t.Fatalf("ordinary responses unexpectedly requires %q", got)
	}
}
