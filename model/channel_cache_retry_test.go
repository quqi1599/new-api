package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
)

func TestGetRandomSatisfiedChannelDoesNotReuseExcludedChannels(t *testing.T) {
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldGroups := group2model2channels
	oldChannels := channelsIDM
	oldProtectedChannelIds := apiKeyPolicyProtectedChannelIds
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		group2model2channels = oldGroups
		channelsIDM = oldChannels
		apiKeyPolicyProtectedChannelIds = oldProtectedChannelIds
	})

	common.MemoryCacheEnabled = true
	group2model2channels = map[string]map[string][]int{
		"default": {"gpt-5.5": {9, 131}},
	}
	priority := int64(0)
	weight := uint(1)
	channelsIDM = map[int]*Channel{
		9:   {Id: 9, Priority: &priority, Weight: &weight},
		131: {Id: 131, Priority: &priority, Weight: &weight},
	}
	apiKeyPolicyProtectedChannelIds = []int{131}

	protectedChannelIds, err := GetAPIKeyPolicyProtectedChannelIds()
	if err != nil || len(protectedChannelIds) != 1 || protectedChannelIds[0] != 131 {
		t.Fatalf("protected channel ids = %#v, err = %v", protectedChannelIds, err)
	}

	channel, err := GetRandomSatisfiedChannel("default", "gpt-5.5", 0, nil, []int{9})
	if err != nil || channel == nil || channel.Id != 131 {
		t.Fatalf("fallback channel = %#v, err = %v", channel, err)
	}

	channel, err = GetRandomSatisfiedChannel("default", "gpt-5.5", 0, nil, []int{9, 131})
	if err != nil || channel != nil {
		t.Fatalf("fully excluded channel = %#v, err = %v", channel, err)
	}
}
