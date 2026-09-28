package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func TestChannelProbeGPT6Compatibility(t *testing.T) {
	for _, modelName := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-6-luna-high", "gpt-6-sol-2026-09-03", "gpt-5.2"} {
		for _, endpoint := range []string{"", string(constant.EndpointTypeOpenAI)} {
			for _, stream := range []bool{false, true} {
				t.Run(modelName+"/"+endpoint+"/"+map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
					request := buildTestRequest(modelName, endpoint, &model.Channel{Type: constant.ChannelTypeOpenAI}, stream).(*dto.GeneralOpenAIRequest)
					require.Nil(t, request.MaxTokens)
					require.Equal(t, common.GetPointer(uint(16)), request.MaxCompletionTokens)
					if stream {
						require.True(t, request.StreamOptions.IncludeUsage)
					}
				})
			}
		}
	}
}

func TestChannelProbePreservesOtherProviderLimits(t *testing.T) {
	for _, modelName := range []string{"qwen3.8-max", "gpt-6-sol-custom", "ollama-model"} {
		request := buildTestRequest(modelName, "", nil, false).(*dto.GeneralOpenAIRequest)
		require.NotNil(t, request.MaxTokens)
		require.Nil(t, request.MaxCompletionTokens)
	}
}
