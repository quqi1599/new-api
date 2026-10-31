package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/websearch"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type failingWebStore struct{ webTestStore }

func (s failingWebStore) Put(context.Context, string, websearch.Record) error {
	return errors.New("storage unavailable")
}

func TestManagedWebFailurePreservesMeasuredUsageWithoutReplay(t *testing.T) {
	oldLog, oldExport := common.LogConsumeEnabled, common.DataExportEnabled
	common.LogConsumeEnabled = true
	common.DataExportEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = oldLog; common.DataExportEnabled = oldExport })
	for _, scenario := range []string{"model_failure", "duplicate_call", "store_failure", "client_disconnect", "empty_search"} {
		t.Run(scenario, func(t *testing.T) {
			user, token := setupMidjourneyBillingTest(t, 1000000, 1000000)
			writer := &imageFailingHTTPWriter{ResponseRecorder: httptest.NewRecorder(), allow: -1}
			if scenario == "client_disconnect" {
				writer.allow = 0
			}
			c, _ := gin.CreateTestContext(writer)
			body := `{"model":"gpt-6.1-sol","input":"search","tools":[{"type":"web_search"}],"stream":true}`
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(common.RequestIdKey, "managed-failure")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, OriginModelName: "gpt-6.1-sol", UsingGroup: "default", TokenGroup: "default", UserGroup: "default", StartTime: time.Now(), IsStream: true, RelayFormat: types.RelayFormatOpenAIResponses, RelayMode: relayconstant.RelayModeResponses, ChannelMeta: &relaycommon.ChannelMeta{}, UserSetting: dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1}, PriceData: types.PriceData{ModelRatio: 1, CompletionRatio: 1, CacheRatio: 1, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, service.PreConsumeBilling(c, 100, info))
			plan, err := websearch.NewPlan([]byte(body), "responses")
			require.NoError(t, err)
			backend := &webTestBackend{empty: scenario == "empty_search"}
			var store websearch.Store = webTestStore{}
			if scenario == "store_failure" {
				store = failingWebStore{webTestStore{}}
			}
			session := &websearch.Session{Backend: backend, Store: store}
			rounds := 0
			apiErr := runManagedWeb(c, info, func() *types.NewAPIError {
				rounds++
				if rounds == 2 && scenario == "model_failure" {
					return webError(errors.New("upstream failed"), 502)
				}
				payload := `{"id":"r1","status":"completed","output":[{"type":"function_call","id":"f1","call_id":"c1","name":"newapi_web_search","arguments":"{\"query\":\"fixture\"}"}],"usage":{"input_tokens":10,"output_tokens":2}}`
				if rounds == 2 && scenario != "duplicate_call" {
					payload = `{"id":"r2","status":"completed","output":[{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
				}
				_, err := c.Writer.Write([]byte(payload))
				require.NoError(t, err)
				service.PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}, nil)
				return nil
			}, []byte(body), plan, session)
			if scenario == "empty_search" {
				require.Nil(t, apiErr)
			} else {
				require.NotNil(t, apiErr)
				require.True(t, types.IsSkipRetryError(apiErr))
			}
			require.Equal(t, 1, backend.calls, "never repeat a dispatched search")
			modelRounds := 2
			if scenario == "model_failure" || scenario == "store_failure" {
				modelRounds = 1
			}
			fee := 1712
			if scenario == "empty_search" {
				fee = 0
			}
			want := 12*modelRounds + fee
			wallet, remaining := persistedMidjourneyQuota(t, user.Id, token.Id)
			require.Equal(t, 1000000-want, wallet)
			require.Equal(t, wallet, remaining)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("user_id=? AND type=?", user.Id, model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			require.Equal(t, want, logs[0].Quota)
			if scenario != "empty_search" {
				require.NotEqual(t, "completed", gjson.Get(logs[0].Other, "generation_outcome.state").String(), fmt.Sprint(logs[0].Other))
			}
			info.Billing.Refund(c)
			wallet, _ = persistedMidjourneyQuota(t, user.Id, token.Id)
			require.Equal(t, 1000000-want, wallet)
		})
	}
}
