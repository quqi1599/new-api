package relay

import (
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
)

// Dynamic tool declarations belong to their original message and turn. The
// existing non-Chat converters cannot express that location; promoting them to
// top-level tools would change which historical calls had access to each tool.
func checkMessageToolsRoute(info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest, chatViaResponses bool) *types.NewAPIError {
	if request == nil {
		return nil
	}
	hasMessageTools := false
	for _, message := range request.Messages {
		if message.HasToolDeclarations() {
			hasMessageTools = true
			break
		}
	}
	if !hasMessageTools {
		return nil
	}
	if info == nil || info.ChannelMeta == nil || info.RelayMode != relayconstant.RelayModeChatCompletions || chatViaResponses {
		return types.NewMessageToolsRouteError()
	}
	// These API types use the same OpenAI adaptor and preserve Message.Tools.
	// Raw-body passthrough is not an exception for a Messages/Gemini endpoint.
	switch info.ApiType {
	case constant.APITypeOpenAI, constant.APITypeOpenRouter, constant.APITypeXinference:
		return nil
	default:
		return types.NewMessageToolsRouteError()
	}
}
