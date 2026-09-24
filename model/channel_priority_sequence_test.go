package model

import (
	"fmt"
	"slices"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
)

func TestRemainingChannelPriorityOrdering(t *testing.T) {
	for _, cache := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			channels   []Channel
			preferred  []int
			priorities []int64
		}{
			{
				name: "three priority tiers",
				channels: []Channel{nativePreferenceChannel(9, constant.ChannelTypeOpenAI, 100),
					nativePreferenceChannel(10, constant.ChannelTypeOpenAI, 90), nativePreferenceChannel(11, constant.ChannelTypeOpenAI, 80)},
				priorities: []int64{100, 90, 80},
			},
			{
				name: "same tier exhausted before lower tier",
				channels: []Channel{nativePreferenceChannel(9, constant.ChannelTypeOpenAI, 100),
					nativePreferenceChannel(10, constant.ChannelTypeOpenAI, 100), nativePreferenceChannel(11, constant.ChannelTypeOpenAI, 90)},
				priorities: []int64{100, 100, 90},
			},
			{
				name: "native exhaustion starts at highest compatible tier",
				channels: []Channel{nativePreferenceChannel(9, constant.ChannelTypeOpenAI, 10),
					nativePreferenceChannel(10, constant.ChannelTypeOpenAI, 1), nativePreferenceChannel(11, constant.ChannelTypeAzure, 100),
					nativePreferenceChannel(12, constant.ChannelTypeAzure, 90)},
				preferred: []int{constant.ChannelTypeOpenAI}, priorities: []int64{10, 1, 100, 90},
			},
		} {
			t.Run(fmt.Sprintf("cache=%v/%s", cache, tc.name), func(t *testing.T) {
				setupNativePreferenceSelection(t, cache, tc.channels)
				var excluded []int
				for _, priority := range tc.priorities {
					channel, err := GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", tc.preferred, "", excluded)
					if err != nil || channel == nil || channel.GetPriority() != priority || slices.Contains(excluded, channel.Id) {
						t.Fatalf("selected=%v err=%v want priority=%d excluded=%v", channel, err, priority, excluded)
					}
					excluded = append(excluded, channel.Id)
				}
				channel, err := GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", tc.preferred, "", excluded)
				if err != nil || channel != nil {
					t.Fatalf("exhausted selection=%v err=%v", channel, err)
				}
			})
		}
	}
}

func TestRemainingChannelPriorityKeepsHardGatesAndZeroWeight(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%v", cache), func(t *testing.T) {
			channels := []Channel{nativePreferenceChannel(9, constant.ChannelTypeOpenAI, 100),
				nativePreferenceChannel(10, constant.ChannelTypeAzure, 90), nativePreferenceChannel(11, constant.ChannelTypeAzure, 80)}
			zero := uint(0)
			setting := `{"alpha_search_enabled":true}`
			for i := range channels {
				channels[i].Weight = &zero
			}
			channels[1].Setting, channels[2].Setting = &setting, &setting
			channels[0].Status = common.ChannelStatusAutoDisabled
			setupNativePreferenceSelection(t, cache, channels)
			preferred := []int{constant.ChannelTypeOpenAI}
			channel, err := GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", preferred, "", nil)
			if err != nil || channel == nil || channel.Id != 10 {
				t.Fatalf("disabled native route escaped: selection=%v err=%v", channel, err)
			}
			channel, err = GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", preferred, constant.EndpointTypeOpenAIAlphaSearch, nil)
			if err != nil || channel == nil || channel.Id != 10 {
				t.Fatalf("endpoint gate / zero weight selection=%v err=%v", channel, err)
			}
			channel, err = GetNextSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", preferred, constant.EndpointTypeOpenAIAlphaSearch, []int{10})
			if err != nil || channel == nil || channel.Id != 11 {
				t.Fatalf("excluded route reappeared: selection=%v err=%v", channel, err)
			}
			for _, groupModel := range [][2]string{{"other", "gpt-5.6-sol"}, {"default", "other"}} {
				channel, err = GetNextSatisfiedChannelForEndpoint(groupModel[0], groupModel[1], preferred, "", nil)
				if err != nil || channel != nil {
					t.Fatalf("group/model gate escaped: selection=%v err=%v", channel, err)
				}
			}
		})
	}
}
