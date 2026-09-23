package service

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

func TestPersistentChannelExclusionsSurviveRoundReset(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, []int{3})
	param := &RetryParam{Ctx: c, ExcludedChannelIds: []int{2}}
	param.AddPersistentExcludedChannel(1)
	param.AddPersistentExcludedChannel(1)
	param.AddPersistentExcludedChannel(0)
	if !slices.Equal(param.PersistentExcludedIds, []int{1}) {
		t.Fatalf("persistent exclusions = %v, want unique positive channel IDs", param.PersistentExcludedIds)
	}
	if got := excludedChannelIdsForRequest(param); !slices.Equal(got, []int{2, 1, 3}) {
		t.Fatalf("merged exclusions = %v", got)
	}
	param.ExcludedChannelIds = nil
	if got := excludedChannelIdsForRequest(param); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("reset lost persistent/token exclusions: %v", got)
	}
	if got := excludedChannelIdsForRequest(&RetryParam{Ctx: c}); !slices.Equal(got, []int{3}) {
		t.Fatalf("request-specific exclusion leaked to another request: %v", got)
	}
}
