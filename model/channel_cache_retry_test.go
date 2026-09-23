package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
)

func TestGetRandomSatisfiedChannelDoesNotReuseExcludedChannels(t *testing.T) {
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldGroups := group2model2channels
	oldChannels := channelsIDM
	oldProtectedChannelIds := apiKeyPolicyProtectedChannelIds
	oldModelRoutingFirstChannelIds := modelRoutingFirstChannelIds
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		group2model2channels = oldGroups
		channelsIDM = oldChannels
		apiKeyPolicyProtectedChannelIds = oldProtectedChannelIds
		modelRoutingFirstChannelIds = oldModelRoutingFirstChannelIds
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

func TestGetRandomSatisfiedChannelForAlphaSearchFiltersUnsupportedChannels(t *testing.T) {
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldGroups := group2model2channels
	oldChannels := channelsIDM
	oldProtectedChannelIds := apiKeyPolicyProtectedChannelIds
	oldModelRoutingFirstChannelIds := modelRoutingFirstChannelIds
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		group2model2channels = oldGroups
		channelsIDM = oldChannels
		apiKeyPolicyProtectedChannelIds = oldProtectedChannelIds
		modelRoutingFirstChannelIds = oldModelRoutingFirstChannelIds
	})

	common.MemoryCacheEnabled = true
	group2model2channels = map[string]map[string][]int{
		"default": {"gpt-5.5": {1, 9}},
	}
	highPriority := int64(100)
	lowPriority := int64(0)
	weight := uint(1)
	alphaEnabled := `{"alpha_search_enabled":true}`
	channelsIDM = map[int]*Channel{
		1: {Id: 1, Type: constant.ChannelTypeOpenAI, Priority: &highPriority, Weight: &weight},
		9: {Id: 9, Type: constant.ChannelTypeOpenAI, Setting: &alphaEnabled, Priority: &lowPriority, Weight: &weight},
	}

	channel, err := GetRandomSatisfiedChannelForEndpoint(
		"default",
		"gpt-5.5",
		0,
		[]int{constant.ChannelTypeOpenAI},
		constant.EndpointTypeOpenAIAlphaSearch,
		nil,
	)
	if err != nil || channel == nil || channel.Id != 9 {
		t.Fatalf("selected channel = %#v, err = %v", channel, err)
	}

	channel.Setting = nil
	channel, err = GetRandomSatisfiedChannelForEndpoint(
		"default",
		"gpt-5.5",
		0,
		[]int{constant.ChannelTypeOpenAI},
		constant.EndpointTypeOpenAIAlphaSearch,
		nil,
	)
	if err != nil || channel != nil {
		t.Fatalf("unsupported channels must fail closed: channel = %#v, err = %v", channel, err)
	}
}

func TestModelRoutingFirstChannelOverridesPriorityAndFallsBackAfterExclusion(t *testing.T) {
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldGroups := group2model2channels
	oldChannels := channelsIDM
	oldProtectedChannelIds := apiKeyPolicyProtectedChannelIds
	oldModelRoutingFirstChannelIds := modelRoutingFirstChannelIds
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		group2model2channels = oldGroups
		channelsIDM = oldChannels
		apiKeyPolicyProtectedChannelIds = oldProtectedChannelIds
		modelRoutingFirstChannelIds = oldModelRoutingFirstChannelIds
	})

	common.MemoryCacheEnabled = true
	group2model2channels = map[string]map[string][]int{
		"default": {"gpt-5.5": {9, 131}},
	}
	highPriority := int64(9)
	lowPriority := int64(0)
	highWeight := uint(100)
	lowWeight := uint(1)
	channelsIDM = map[int]*Channel{
		9:   {Id: 9, Type: constant.ChannelTypeOpenAI, Priority: &highPriority, Weight: &highWeight},
		131: {Id: 131, Type: constant.ChannelTypeOpenAI, Priority: &lowPriority, Weight: &lowWeight},
	}
	modelRoutingFirstChannelIds = map[int]struct{}{131: {}}

	channel, err := GetModelRoutingFirstSatisfiedChannelForEndpoint("default", "gpt-5.5", "", nil)
	if err != nil || channel == nil || channel.Id != 131 {
		t.Fatalf("model-routing-first channel = %#v, err = %v", channel, err)
	}

	channel, err = GetModelRoutingFirstSatisfiedChannelForEndpoint("default", "gpt-5.5", "", []int{131})
	if err != nil || channel != nil {
		t.Fatalf("excluded model-routing-first channel = %#v, err = %v", channel, err)
	}

	channel, err = GetRandomSatisfiedChannel("default", "gpt-5.5", 0, nil, []int{131})
	if err != nil || channel == nil || channel.Id != 9 {
		t.Fatalf("fallback channel = %#v, err = %v", channel, err)
	}
}

func TestModelRoutingFirstChannelStillHonorsEndpointCapability(t *testing.T) {
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldGroups := group2model2channels
	oldChannels := channelsIDM
	oldModelRoutingFirstChannelIds := modelRoutingFirstChannelIds
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		group2model2channels = oldGroups
		channelsIDM = oldChannels
		modelRoutingFirstChannelIds = oldModelRoutingFirstChannelIds
	})

	common.MemoryCacheEnabled = true
	group2model2channels = map[string]map[string][]int{
		"default": {"gpt-5.5": {131}},
	}
	priority := int64(0)
	weight := uint(1)
	channelsIDM = map[int]*Channel{
		131: {Id: 131, Type: constant.ChannelTypeOpenAI, Priority: &priority, Weight: &weight},
	}
	modelRoutingFirstChannelIds = map[int]struct{}{131: {}}

	channel, err := GetModelRoutingFirstSatisfiedChannelForEndpoint(
		"default",
		"gpt-5.5",
		constant.EndpointTypeOpenAIAlphaSearch,
		nil,
	)
	if err != nil || channel != nil {
		t.Fatalf("unsupported model-routing-first channel = %#v, err = %v", channel, err)
	}
}
