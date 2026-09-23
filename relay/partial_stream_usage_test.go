package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type partialStreamSettlement struct{ calls int }

func (s *partialStreamSettlement) Settle(int) error              { s.calls++; return nil }
func (*partialStreamSettlement) Refund(*gin.Context)             {}
func (*partialStreamSettlement) NeedsRefund() bool               { return false }
func (*partialStreamSettlement) GetPreConsumedQuota() int        { return 0 }
func (*partialStreamSettlement) GetInitialPreConsumedQuota() int { return 0 }
func (*partialStreamSettlement) Reserve(int) error               { return nil }

func TestPartialStreamUsageRequiresExactMarkedFailure(t *testing.T) {
	for _, invalid := range []string{"", "other_error", "nil_usage", "typed_nil_usage", "before_output", "non_stream", "no_event", "retryable"} {
		t.Run(invalid, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if invalid != "before_output" {
				c.Writer.WriteHeaderNow()
			}
			err := types.NewError(errors.New("partial failure"), types.ErrorCodeUpstreamStreamIncomplete, types.ErrOptionWithSkipRetry())
			info := &relaycommon.RelayInfo{IsStream: true, ReceivedResponseCount: 1, PartialStreamError: err}
			var usage any = &dto.Usage{PromptTokens: 10, CompletionTokens: 2}
			switch invalid {
			case "other_error":
				err = types.NewError(errors.New("other"), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			case "nil_usage":
				usage = nil
			case "typed_nil_usage":
				usage = (*dto.Usage)(nil)
			case "non_stream":
				info.IsStream = false
			case "no_event":
				info.ReceivedResponseCount = 0
			case "retryable":
				err = types.NewError(errors.New("retry"), types.ErrorCodeBadResponse)
				info.PartialStreamError = err
			}
			require.Equal(t, invalid == "", canSettlePartialStreamUsage(c, info, usage, err))
		})
	}
}

// Exercise the real helper -> adaptor -> scanner -> settlement -> error return,
// not just the marker predicate. Empty partial output avoids any account writes.
func TestPartialStreamHelpersSettleOnceAndReturnFailure(t *testing.T) {
	oldLog := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = oldLog })
	service.InitHttpClient()
	for _, responses := range []bool{false, true} {
		name := "chat"
		if responses {
			name = "responses"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				if responses {
					_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
				} else {
					_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[]}\n\ndata: {\"error\":{\"code\":\"upstream_stream_break\",\"message\":\"broken\"}}\n\n"))
				}
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			path := "/v1/chat/completions"
			if responses {
				path = "/v1/responses"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-5.6-sol")
			billing := &partialStreamSettlement{}
			info := &relaycommon.RelayInfo{IsStream: true, OriginModelName: "gpt-5.6-sol", Billing: billing,
				RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
				RequestURLPath: path, StartTime: time.Now(),
				Request: &dto.GeneralOpenAIRequest{Model: "gpt-5.6-sol", Stream: common.GetPointer(true)},
			}
			var err *types.NewAPIError
			if responses {
				info.RelayFormat = types.RelayFormatOpenAIResponses
				info.RelayMode = relayconstant.RelayModeResponses
				info.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.6-sol", Stream: common.GetPointer(true), Input: []byte(`[]`)}
				err = ResponsesHelper(c, info)
			} else {
				err = TextHelper(c, info)
			}
			require.NotNil(t, err)
			require.Same(t, info.PartialStreamError, err)
			require.True(t, types.IsSkipRetryError(err))
			require.True(t, types.IsChannelPenaltyAllowed(err))
			require.Equal(t, 1, calls)
			require.Equal(t, 1, billing.calls, "partial settlement must not erase the failure or disappear")
		})
	}
}
