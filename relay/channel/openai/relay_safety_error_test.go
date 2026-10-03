package openai

import (
	"io"
	"net/http"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestSafetyStreamErrorPreservesRequestClassification(t *testing.T) {
	for _, protocol := range []string{"chat", "responses_top_level", "responses_nested", "responses_failed"} {
		for _, started := range []bool{false, true} {
			t.Run(protocol+"/started="+map[bool]string{true: "true", false: "false"}[started], func(t *testing.T) {
				c, recorder := newResponsesTestContext()
				info := &relaycommon.RelayInfo{IsStream: true, RelayFormat: types.RelayFormatOpenAIResponses, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-5.3-flash"}}
				if started {
					_, _ = c.Writer.Write([]byte(": ping\n\n"))
				}
				data := `{"error":{"code":"content_policy_violation","message":"fixture"}}`
				switch protocol {
				case "responses_top_level":
					data = `{"type":"error","code":"content_policy_violation","message":"fixture"}`
				case "responses_nested":
					data = `{"type":"error","error":{"code":1301,"message":"fixture"}}`
				case "responses_failed":
					data = `{"type":"response.failed","response":{"error":{"code":"content_policy_violation","message":"fixture"}}}`
				}
				resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: " + data + "\n\ndata: [DONE]\n\n"))}
				var err *types.NewAPIError
				if protocol == "chat" {
					info.RelayFormat = types.RelayFormatOpenAI
					_, err = OaiStreamHandler(c, info, resp)
				} else {
					_, err = OaiResponsesStreamHandler(c, info, resp)
				}
				require.NotNil(t, err)
				require.Equal(t, 400, err.StatusCode)
				require.Equal(t, types.ErrorCode("content_policy_violation"), err.GetErrorCode())
				require.True(t, types.IsSkipRetryError(err))
				require.False(t, types.IsChannelPenaltyAllowed(err))
				if started {
					require.Contains(t, recorder.Body.String(), "content_policy_violation")
				} else {
					require.False(t, c.Writer.Written())
					require.Empty(t, recorder.Body.String())
				}
				require.NotContains(t, recorder.Body.String(), "[DONE]")
				require.NotContains(t, recorder.Body.String(), "response.completed")
			})
		}
	}
}
