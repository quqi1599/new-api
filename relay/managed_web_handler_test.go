package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/websearch"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type webTestStore map[string]websearch.Record

func (m webTestStore) Put(_ context.Context, id string, r websearch.Record) error {
	m[id] = r
	return nil
}
func (m webTestStore) Get(_ context.Context, id string) (websearch.Record, error) {
	r, ok := m[id]
	if !ok {
		return r, errors.New("missing")
	}
	return r, nil
}

type webTestBackend struct {
	calls int
	empty bool
}

func (f *webTestBackend) Search(_ context.Context, _ websearch.SearchRequest) ([]websearch.Hit, error) {
	f.calls++
	if f.empty {
		return []websearch.Hit{}, nil
	}
	return []websearch.Hit{{Title: "source", URL: "https://example.com", Summary: "verified fixture"}}, nil
}
func (f *webTestBackend) Fetch(_ context.Context, _ string) (websearch.Page, error) {
	f.calls++
	return websearch.Page{Content: "page"}, nil
}

func TestManagedWebRelaySettlesModelRoundsAndSearchExactlyOnce(t *testing.T) {
	service.InitTokenEncoders()
	settings := model_setting.GetGlobalSettings()
	oldPass, oldPolicy := settings.PassThroughRequestEnabled, settings.ChatCompletionsToResponsesPolicy
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy.Enabled = false
	t.Cleanup(func() {
		settings.PassThroughRequestEnabled = oldPass
		settings.ChatCompletionsToResponsesPolicy = oldPolicy
	})
	oldRate, oldLog, oldExport := operation_setting.USDExchangeRate, common.LogConsumeEnabled, common.DataExportEnabled
	operation_setting.USDExchangeRate = 7.3
	common.LogConsumeEnabled = true
	common.DataExportEnabled = false
	t.Cleanup(func() {
		operation_setting.USDExchangeRate = oldRate
		common.LogConsumeEnabled = oldLog
		common.DataExportEnabled = oldExport
	})
	for _, protocol := range []string{"responses", "claude", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, group := range []float64{1, 3} {
				t.Run(fmt.Sprintf("%s/stream=%t/group=%v", protocol, stream, group), func(t *testing.T) {
					user, token := setupMidjourneyBillingTest(t, 1000000, 1000000)
					path, format, mode := "/v1/responses", types.RelayFormat(types.RelayFormatOpenAIResponses), relayconstant.RelayModeResponses
					body := fmt.Sprintf(`{"model":"gpt-6.1-sol","input":"search fixture","tools":[{"type":"web_search"}],"stream":%t}`, stream)
					first := `{"id":"r1","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"function_call","id":"fc1","call_id":"c1","name":"newapi_web_search","arguments":"{\"query\":\"fixture\"}"}],"usage":{"input_tokens":10,"output_tokens":2}}`
					last := `{"id":"r2","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"answer https://example.com","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
					if protocol == "claude" {
						path, format, mode = "/v1/messages", types.RelayFormatClaude, relayconstant.RelayModeChatCompletions
						body = fmt.Sprintf(`{"model":"gpt-6.1-sol","max_tokens":1024,"messages":[{"role":"user","content":"search fixture"}],"tools":[{"type":"web_search_20250305","name":"web_search"}],"stream":%t}`, stream)
						first = `{"id":"msg1","type":"message","model":"gpt-6.1-sol","role":"assistant","content":[{"type":"tool_use","id":"c1","name":"newapi_web_search","input":{"query":"fixture"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":2}}`
						last = `{"id":"msg2","type":"message","model":"gpt-6.1-sol","role":"assistant","content":[{"type":"text","text":"answer https://example.com"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":2}}`
					}
					if protocol == "chat" {
						path, format, mode = "/v1/chat/completions", types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions
						body = fmt.Sprintf(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"search fixture"}],"web_search_options":{},"stream":%t}`, stream)
						first = `{"id":"chat1","object":"chat.completion","model":"gpt-6.1-sol","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"newapi_web_search","arguments":"{\"query\":\"fixture\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
						last = `{"id":"chat2","object":"chat.completion","model":"gpt-6.1-sol","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"answer https://example.com"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set(common.RequestIdKey, "managed-fixture")
					t.Cleanup(func() { common.CleanupBodyStorage(c) })
					info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, OriginModelName: "gpt-6.1-sol", UsingGroup: "default", TokenGroup: "default", UserGroup: "default", StartTime: time.Now(), IsStream: stream, RelayFormat: format, RelayMode: mode, ChannelMeta: &relaycommon.ChannelMeta{}, UserSetting: dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1}, PriceData: types.PriceData{ModelRatio: 1, CompletionRatio: 1, CacheRatio: 1, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: group}}}
					require.Nil(t, service.PreConsumeBilling(c, 100, info))
					plan, err := websearch.NewPlan([]byte(body), protocol)
					require.NoError(t, err)
					backend := &webTestBackend{}
					session := &websearch.Session{Backend: backend, Store: webTestStore{}}
					rounds := 0
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						rounds++
						raw, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(raw, "model").String())
						require.NotContains(t, string(raw), `"type":"web_search"`)
						if rounds == 2 {
							require.Contains(t, string(raw), "verified fixture")
						}
						payload := first
						if rounds == 2 {
							payload = last
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							o, e := websearch.Obj([]byte(payload))
							require.NoError(t, e)
							require.NoError(t, websearch.Emit(w, protocol, o))
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, err = w.Write([]byte(payload))
							require.NoError(t, err)
						}
					}))
					defer remote.Close()
					channelType := constant.ChannelTypeOpenAI
					if protocol == "claude" {
						channelType = constant.ChannelTypeAnthropic
					}
					common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
					common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, remote.URL)
					common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture")
					common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-6.1-sol")
					info.ShouldIncludeUsage = true
					apiErr := runManagedWeb(c, info, func() *types.NewAPIError {
						switch protocol {
						case "responses":
							return ResponsesHelper(c, info)
						case "claude":
							return ClaudeHelper(c, info)
						default:
							return TextHelper(c, info)
						}
					}, []byte(body), plan, session)
					require.Nil(t, apiErr)
					require.Equal(t, 2, rounds)
					require.Equal(t, 1, backend.calls)
					want := 1712 + int(24*group)
					wallet, remaining := persistedMidjourneyQuota(t, user.Id, token.Id)
					require.Equal(t, 1000000-want, wallet)
					require.Equal(t, wallet, remaining)
					var logs []model.Log
					require.NoError(t, model.LOG_DB.Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Find(&logs).Error)
					require.Len(t, logs, 1)
					require.Equal(t, want, logs[0].Quota)
					require.EqualValues(t, 2, gjson.Get(logs[0].Other, "managed_web.model_rounds").Int())
					require.Equal(t, "deepseek", gjson.Get(logs[0].Other, "managed_web.provider").String())
					out, err := websearch.Snapshot(protocol, recorder.Body.Bytes())
					require.NoError(t, err)
					wire := websearch.Raw(out)
					require.EqualValues(t, 1, gjson.GetBytes(wire, "usage.server_tool_use.web_search_requests").Int())
					require.Contains(t, string(wire), "answer https://example.com")
					require.NotContains(t, string(wire), "newapi_web_search")
					info.Billing.Refund(c)
					wallet, _ = persistedMidjourneyQuota(t, user.Id, token.Id)
					require.Equal(t, 1000000-want, wallet)
				})
			}
		}
	}
}

func TestManagedAlphaInitializesSelectedChannelAndLogsExactlyOnce(t *testing.T) {
	oldLog, oldExport := common.LogConsumeEnabled, common.DataExportEnabled
	common.LogConsumeEnabled = true
	common.DataExportEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = oldLog; common.DataExportEnabled = oldExport })
	for _, scenario := range []string{"search", "fetch", "cached_find"} {
		t.Run(scenario, func(t *testing.T) {
			user, token := setupMidjourneyBillingTest(t, 1000000, 1000000)
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			body := `{"model":"gpt-6.1-sol","commands":{"search_query":[{"q":"fixture"}]}}`
			store := webTestStore{}
			if scenario == "fetch" {
				body = `{"model":"gpt-6.1-sol","commands":{"open":[{"ref_id":"https://example.com"}]}}`
			}
			if scenario == "cached_find" {
				body = `{"model":"gpt-6.1-sol","commands":{"find":[{"ref_id":"cached_ref","pattern":"fixture"}]}}`
				store["cached_ref"] = websearch.Record{URL: "https://example.com", Content: "fixture page"}
			}
			c.Request = httptest.NewRequest("POST", "/v1/alpha/search", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyChannelId, 143)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-6.1-sol")
			c.Set(common.RequestIdKey, "owned-alpha-fixture")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, OriginModelName: "gpt-6.1-sol", UsingGroup: "default", TokenGroup: "default", UserGroup: "default", StartTime: time.Now(), RelayFormat: types.RelayFormatOpenAIAlphaSearch, RelayMode: relayconstant.RelayModeAlphaSearch, UserSetting: dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: 1}, PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, info.ChannelMeta)
			require.Nil(t, service.PreConsumeBilling(c, 1712, info))
			session := &websearch.Session{Backend: &webTestBackend{}, Store: store}
			require.NotPanics(t, func() {
				require.Nil(t, runManagedWeb(c, info, func() *types.NewAPIError { t.Fatal("standalone search must not invoke model"); return nil }, []byte(body), nil, session))
			})
			require.Equal(t, 143, info.ChannelId)
			require.True(t, gjson.Valid(writer.Body.String()), writer.Body.String())
			expected := 1712
			if scenario == "cached_find" {
				expected = 0
			}
			wallet, remaining := persistedMidjourneyQuota(t, user.Id, token.Id)
			require.Equal(t, 1000000-expected, wallet)
			require.Equal(t, wallet, remaining)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("user_id=? AND type=?", user.Id, model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			require.Equal(t, expected, logs[0].Quota)
			require.Equal(t, 143, logs[0].ChannelId)
			require.Zero(t, gjson.Get(logs[0].Other, "managed_web.model_quota").Int())
			require.Equal(t, "completed", gjson.Get(logs[0].Other, "managed_web.outcome").String())
		})
	}
}
