package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func TestDBControllerNativePreferenceDoesNotLoseAlternative(t *testing.T) {
	setupCPAContractChannels(t, true)
	if err := model.DB.Model(&model.Channel{}).Where("id = ?", 10).Update("type", constant.ChannelTypeAzure).Error; err != nil {
		t.Fatal(err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	param := &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "gpt-5.6-sol", PreferredChannelTypes: types.RelayFormatToPreferredChannelTypes(types.RelayFormatOpenAIResponses)}
	first, _, err := service.CacheGetRandomSatisfiedChannel(param)
	if err != nil || first == nil || first.Id != 9 {
		t.Fatalf("first selection=%v error=%v", first, err)
	}
	authErr := parseCPAContractError(t, http.StatusServiceUnavailable, `{"error":{"message":"no eligible credentials","code":"auth_unavailable"}}`)
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.6-sol", LastError: authErr}
	excludeFailedChannelForRetry(param, info, first.Id)
	param.IncreaseRetry()
	second, _, err := service.CacheGetRandomSatisfiedChannel(param)
	if err != nil || second == nil || second.Id != 10 {
		state := newCPAContractRetryState()
		state.RecordAttempt(first.Id)
		next := canStartNextRelayRetryRound(context.Background(), state, true, false, authErr, param)
		t.Fatalf("healthy Azure alternate must remain reachable: selected=%v err=%v nextRound=%v stop=%s", second, err, next, state.StopReason)
	}
	// The distinct compatible entrance may recover in a later round, while the
	// exhausted native CPA entrance remains excluded for the entire request.
	transientErr := parseCPAContractError(t, http.StatusBadGateway, `{"error":{"message":"temporary upstream failure","code":"server_error"}}`)
	excludeFailedChannelForRetry(param, &relaycommon.RelayInfo{LastError: transientErr}, second.Id)
	state := newCPAContractRetryState()
	state.RecordAttempt(first.Id)
	state.RecordAttempt(second.Id)
	for _, lastErr := range []*types.NewAPIError{authErr, transientErr} {
		if !canStartNextRelayRetryRound(context.Background(), state, true, false, lastErr, param) {
			t.Fatal("a distinct compatible entrance must keep bounded next-round recovery available")
		}
	}
	state.StartNextRound()
	param.ExcludedChannelIds = nil // normal round reset must not erase permanent exclusions
	param.SetRetry(0)
	third, _, err := service.CacheGetRandomSatisfiedChannel(param)
	if err != nil || third == nil || third.Id != 10 || !slices.Equal(param.PersistentExcludedIds, []int{9}) {
		t.Fatalf("cross-round fallback=%v err=%v persistent=%v", third, err, param.PersistentExcludedIds)
	}
}
