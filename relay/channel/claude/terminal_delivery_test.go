package claude

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type claudeDisconnectWriter struct {
	*httptest.ResponseRecorder
	writes, allow int
}

func (w *claudeDisconnectWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.allow {
		return 0, errors.New("synthetic disconnected client")
	}
	return w.ResponseRecorder.Write(p)
}

func TestClaudeDeliveryFailureKeepsUsageAndCancellation(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		w := &claudeDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), allow: 1}
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		info := &relaycommon.RelayInfo{RelayFormat: format, IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"}}
		sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}
		var usage *dto.Usage
		var err *types.NewAPIError
		if format == types.RelayFormatOpenAIResponses {
			usage, err = ClaudeResponsesStreamHandler(c, resp, info)
		} else {
			usage, err = ClaudeStreamHandler(c, resp, info)
		}
		require.NotNil(t, err)
		require.Equal(t, 499, err.StatusCode)
		require.True(t, types.IsSkipRetryError(err))
		require.False(t, types.IsChannelPenaltyAllowed(err))
		require.Same(t, err, info.PartialStreamError)
		require.NotNil(t, usage)
		require.Equal(t, 10, usage.PromptTokens)
		require.Equal(t, "cancelled", info.GenerationOutcome.State)
		require.Equal(t, constant.RelayCancelOriginDownstreamDisconnected, common.GetContextKeyString(c, constant.ContextKeyRelayCancelOrigin))
		require.False(t, info.ShouldRecordChannelSuccess())
		require.NotContains(t, w.Body.String(), "[DONE]")
	}
}
