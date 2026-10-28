package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingSelectionSkipsAnthropicBeforePriority(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			channels := []Channel{
				nativePreferenceChannel(141, constant.ChannelTypeAnthropic, 100),
				nativePreferenceChannel(142, constant.ChannelTypeOpenAI, 1),
			}
			setupNativePreferenceSelection(t, cached, channels)
			for _, excluded := range [][]int{nil, {141}, {142}, {141, 142}} {
				got, err := GetRandomSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", 0, nil, constant.EndpointTypeEmbeddings, excluded)
				require.NoError(t, err)
				if len(excluded) > 0 && excluded[len(excluded)-1] == 142 {
					require.Nil(t, got, "must not fall back to an incompatible Messages channel")
				} else {
					require.NotNil(t, got)
					require.Equal(t, 142, got.Id)
				}
			}
			got, err := GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", nil, constant.EndpointTypeEmbeddings, nil)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, 142, got.Id, "exhaustive retry keeps the same capability gate")
			got, err = GetRandomSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", 0, nil, "", nil)
			require.NoError(t, err)
			require.Equal(t, 141, got.Id, "chat priority remains unchanged")
		})
	}
}
