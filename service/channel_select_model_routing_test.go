package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

func TestModelRoutingFirstPersistentExclusionSurvivesRetryRoundReset(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, []int{77})
	param := &RetryParam{Ctx: c, ExcludedChannelIds: []int{9}}
	param.AddPersistentExcludedChannel(131)
	param.AddPersistentExcludedChannel(131)

	excluded := excludedChannelIdsForRequest(param)
	if !containsAllChannelIDs(excluded, 9, 77, 131) || len(param.PersistentExcludedIds) != 1 {
		t.Fatalf("initial exclusions = %#v, persistent = %#v", excluded, param.PersistentExcludedIds)
	}

	param.ExcludedChannelIds = nil
	excluded = excludedChannelIdsForRequest(param)
	if !containsAllChannelIDs(excluded, 77, 131) {
		t.Fatalf("round-reset exclusions = %#v", excluded)
	}
}

func TestAppendModelRoutingFirstAdminInfo(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	adminInfo := map[string]interface{}{}
	AppendModelRoutingFirstAdminInfo(c, adminInfo)
	if len(adminInfo) != 0 {
		t.Fatalf("unexpected admin info without selected channel: %#v", adminInfo)
	}

	common.SetContextKey(c, constant.ContextKeyModelRoutingFirstChannel, 131)
	AppendModelRoutingFirstAdminInfo(c, adminInfo)
	if adminInfo["model_routing_first_channel_id"] != 131 {
		t.Fatalf("admin info = %#v", adminInfo)
	}
}

func containsAllChannelIDs(channelIDs []int, expected ...int) bool {
	for _, expectedID := range expected {
		found := false
		for _, channelID := range channelIDs {
			if channelID == expectedID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
