package deepseek

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestResponsesRequestURL(t *testing.T) {
	url, err := (&Adaptor{}).GetRequestURL(&relaycommon.RelayInfo{
		RelayMode: constant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://api.deepseek.com",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://api.deepseek.com/responses", url)
}

func TestConvertOpenAIResponsesRequestAppliesThinkingSuffix(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAIResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "deepseek-v4-chat-max",
		},
	}
	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, info, dto.OpenAIResponsesRequest{
		Model: "customer-model",
	})
	require.NoError(t, err)
	request, ok := converted.(dto.OpenAIResponsesRequest)
	require.True(t, ok)
	require.Equal(t, "deepseek-v4-chat", request.Model)
	require.NotNil(t, request.Reasoning)
	require.Equal(t, "max", request.Reasoning.Effort)
	require.Equal(t, "deepseek-v4-chat", info.UpstreamModelName)
	require.Equal(t, "max", info.ReasoningEffort)
}

func TestConvertOpenAIResponsesRequestDisablesThinking(t *testing.T) {
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "deepseek-v4-chat-none"}}
	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, info, dto.OpenAIResponsesRequest{Model: "customer-model"})
	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	require.Equal(t, "deepseek-v4-chat", request.Model)
	require.NotNil(t, request.Reasoning)
	require.Equal(t, "none", request.Reasoning.Effort)
}
