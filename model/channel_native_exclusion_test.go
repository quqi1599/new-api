package model

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNativePreferenceFallsBackAfterExcluded(t *testing.T) {
	oldEnabled, oldGroups, oldChannels := common.MemoryCacheEnabled, group2model2channels, channelsIDM
	t.Cleanup(func() {
		common.MemoryCacheEnabled, group2model2channels, channelsIDM = oldEnabled, oldGroups, oldChannels
	})
	common.MemoryCacheEnabled = true
	group2model2channels = map[string]map[string][]int{"default": {"gpt-5.6-sol": {9, 10}}}
	priority, weight := int64(0), uint(1)
	channelsIDM = map[int]*Channel{
		9:  {Id: 9, Type: constant.ChannelTypeOpenAI, Priority: &priority, Weight: &weight},
		10: {Id: 10, Type: constant.ChannelTypeAzure, Priority: &priority, Weight: &weight},
	}
	selected, err := GetRandomSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", 0, []int{constant.ChannelTypeOpenAI}, "", nil)
	if err != nil || selected == nil || selected.Id != 9 {
		t.Fatalf("first selection = %v, %v", selected, err)
	}
	selected, err = GetRandomSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", 1, []int{constant.ChannelTypeOpenAI}, "", []int{9})
	if err != nil || selected == nil || selected.Id != 10 {
		t.Fatalf("healthy compatible fallback must remain reachable after native 9 excluded: selected=%v err=%v", selected, err)
	}
}

// Cache and DB selection must apply the same hard gates before the soft native
// preference, including when remaining candidates span several priority tiers.
func TestNativePreferenceEligibilityAndPriority(t *testing.T) {
	for _, cache := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			excluded []int
			retry    int
			endpoint constant.EndpointType
			prepare  func([]Channel)
			want     int
		}{
			{name: "native preference beats compatible priority", want: 9},
			{name: "all native excluded fallback", excluded: []int{9, 11}, want: 10},
			{name: "fallback selects next eligible priority", excluded: []int{9, 11}, retry: 1, want: 12},
			{name: "fallback clamps exhausted priorities", excluded: []int{9, 11}, retry: 99, want: 12},
			{name: "remaining native outranks high priority compatible", excluded: []int{9}, want: 11},
			{name: "remaining compatible after other high priority excluded", excluded: []int{9, 10, 11}, want: 12},
			{name: "all excluded fail closed", excluded: []int{9, 10, 11, 12}},
			{name: "endpoint unsupported fail closed", endpoint: constant.EndpointTypeOpenAIAlphaSearch},
			{
				name: "endpoint qualified compatible survives unsupported native", endpoint: constant.EndpointTypeOpenAIAlphaSearch, want: 10,
				prepare: func(channels []Channel) { setting := `{"alpha_search_enabled":true}`; channels[1].Setting = &setting },
			},
			{
				name: "excluded only endpoint capable route fails closed", excluded: []int{10}, endpoint: constant.EndpointTypeOpenAIAlphaSearch,
				prepare: func(channels []Channel) { setting := `{"alpha_search_enabled":true}`; channels[1].Setting = &setting },
			},
			{
				name: "disabled native cannot shadow compatible", want: 10,
				prepare: func(channels []Channel) {
					channels[0].Status = common.ChannelStatusAutoDisabled
					channels[2].Status = common.ChannelStatusManuallyDisabled
				},
			},
		} {
			t.Run(fmt.Sprintf("cache=%v/%s", cache, tc.name), func(t *testing.T) {
				channels := []Channel{
					nativePreferenceChannel(9, constant.ChannelTypeOpenAI, 10),
					nativePreferenceChannel(10, constant.ChannelTypeAzure, 100),
					nativePreferenceChannel(11, constant.ChannelTypeOpenAI, 1),
					nativePreferenceChannel(12, constant.ChannelTypeAzure, 0),
				}
				if tc.prepare != nil {
					tc.prepare(channels)
				}
				setupNativePreferenceSelection(t, cache, channels)
				got, err := GetRandomSatisfiedChannelForEndpoint("default", "gpt-5.6-sol", tc.retry, []int{constant.ChannelTypeOpenAI}, tc.endpoint, tc.excluded)
				if err != nil {
					t.Fatal(err)
				}
				if tc.want == 0 {
					if got != nil {
						t.Fatalf("ineligible route selected: %#v", got)
					}
				} else if got == nil || got.Id != tc.want {
					t.Fatalf("selected=%v want id=%d", got, tc.want)
				}
				for _, groupModel := range [][2]string{{"other", "gpt-5.6-sol"}, {"default", "unregistered-model"}} {
					got, err := GetRandomSatisfiedChannelForEndpoint(groupModel[0], groupModel[1], tc.retry, []int{constant.ChannelTypeOpenAI}, "", nil)
					if err != nil || got != nil {
						t.Fatalf("group/model eligibility escaped: %v, %v", got, err)
					}
				}
			})
		}
	}
}

func nativePreferenceChannel(id, channelType int, priority int64) Channel {
	weight := uint(1)
	return Channel{Id: id, Type: channelType, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-5.6-sol", Priority: &priority, Weight: &weight}
}

func setupNativePreferenceSelection(t *testing.T, cache bool, channels []Channel) {
	t.Helper()
	oldDB, oldGroupCol := DB, commonGroupCol
	oldCache, oldGroups, oldChannels := common.MemoryCacheEnabled, group2model2channels, channelsIDM
	oldProtected, oldModelFirst := apiKeyPolicyProtectedChannelIds, modelRoutingFirstChannelIds
	t.Cleanup(func() {
		DB, commonGroupCol = oldDB, oldGroupCol
		common.MemoryCacheEnabled, group2model2channels, channelsIDM = oldCache, oldGroups, oldChannels
		apiKeyPolicyProtectedChannelIds, modelRoutingFirstChannelIds = oldProtected, oldModelFirst
	})
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "selection.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	DB, commonGroupCol = db, "`group`"
	if err := DB.AutoMigrate(&Channel{}, &Ability{}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range channels {
		if err := DB.Create(&channel).Error; err != nil {
			t.Fatal(err)
		}
		// Keep the ability enabled for disabled-channel cases to exercise the
		// channel-status gate even if cached ability data is stale.
		ability := Ability{Group: channel.Group, Model: channel.Models, ChannelId: channel.Id, Enabled: true, Priority: channel.Priority, Weight: uint(channel.GetWeight())}
		if err := DB.Create(&ability).Error; err != nil {
			t.Fatal(err)
		}
	}
	common.MemoryCacheEnabled = cache
	InitChannelCache()
}
