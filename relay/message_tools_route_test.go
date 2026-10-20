package relay

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestMessageToolsRoutePreservesNativeChatOnly(t *testing.T) {
	for _, apiType := range []int{constant.APITypeOpenAI, constant.APITypeOpenRouter, constant.APITypeXinference, constant.APITypeAnthropic, constant.APITypeGemini, constant.APITypeCodex, constant.APITypeMoonshot} {
		for _, passthrough := range []bool{false, true} {
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, ChannelMeta: &relaycommon.ChannelMeta{ApiType: apiType, ChannelSetting: dto.ChannelSettings{PassThroughBodyEnabled: passthrough}}}
			request := &dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "system", Tools: json.RawMessage(`[{"type":"function","function":{"name":"lookup"}}]`)}}}
			err := checkMessageToolsRoute(info, request, false)
			native := apiType == constant.APITypeOpenAI || apiType == constant.APITypeOpenRouter || apiType == constant.APITypeXinference
			require.Equal(t, !native, types.IsLocalMessageToolsRouteError(err), "api=%d passthrough=%t", apiType, passthrough)
			require.True(t, types.IsLocalMessageToolsRouteError(checkMessageToolsRoute(info, request, true)), "Chat-to-Responses must not drop dynamic tools")
		}
	}
}

func TestMessageToolsRouteLeavesOrdinaryRequestsUnchanged(t *testing.T) {
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, ChannelMeta: &relaycommon.ChannelMeta{ApiType: constant.APITypeAnthropic}}
	for _, raw := range []string{"", "null", " null ", "[]", "[ ]"} {
		require.Nil(t, checkMessageToolsRoute(info, &dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "system", Tools: json.RawMessage(raw)}}}, false))
	}
	// A malformed declaration must not be silently discarded on conversion.
	require.True(t, types.IsLocalMessageToolsRouteError(checkMessageToolsRoute(info, &dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "system", Tools: json.RawMessage(`{}`)}}}, false)))
}
