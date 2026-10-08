package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func runResponsesTerminalFixture(t *testing.T, wire string) (*httptest.ResponseRecorder, *types.NewAPIError, int, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	usage, err := OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(wire))})
	if usage == nil {
		return recorder, err, 0, 0
	}
	return recorder, err, usage.PromptTokens, usage.CompletionTokens
}

func TestResponsesNativeFailurePreservesTerminalAndReportedUsage(t *testing.T) {
	created := `{"type":"response.created","sequence_number":4,"response":{"id":"resp_owned","status":"in_progress"}}`
	failed := `{"type":"response.failed","sequence_number":9,"response":{"id":"resp_owned","status":"failed","error":{"type":"server_error","code":"upstream_failure","message":"upstream HTTP 502","provider_extension":9007199254740993},"usage":{"input_tokens":13,"output_tokens":7},"output":[]}}`
	recorder, err, input, output := runResponsesTerminalFixture(t, "data: "+created+"\n\ndata: "+failed+"\n\n")
	require.NotNil(t, err)
	require.True(t, types.IsSkipRetryError(err))
	require.Equal(t, 13, input)
	require.Equal(t, 7, output)
	require.Contains(t, recorder.Body.String(), "event: response.failed\ndata: "+failed+"\n\n")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed\n"))
	require.NotContains(t, recorder.Body.String(), "event: error\n")
	require.NotContains(t, recorder.Body.String(), "response.completed")
	require.NotContains(t, recorder.Body.String(), "[DONE]")
}

func TestResponsesGenericFailureAndEOFProduceLifecycleTerminal(t *testing.T) {
	for name, tail := range map[string]string{
		"error":          `data: {"type":"error","code":"provider_busy","message":"busy"}` + "\n\n",
		"response.error": `data: {"type":"response.error","error":{"code":"provider_busy","message":"busy"}}` + "\n\n",
		"cancelled":      `data: {"type":"response.cancelled","response":{"status":"cancelled"}}` + "\n\n",
		"EOF":            "",
	} {
		t.Run(name, func(t *testing.T) {
			created := `{"type":"response.created","sequence_number":6,"response":{"id":"resp_known","status":"in_progress"}}`
			recorder, err, _, _ := runResponsesTerminalFixture(t, "data: "+created+"\n\n"+tail)
			require.NotNil(t, err)
			require.True(t, types.IsSkipRetryError(err))
			require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed\n"))
			require.NotContains(t, recorder.Body.String(), "event: error\n")
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				root := gjson.Parse(strings.TrimPrefix(line, "data: "))
				if root.Get("type").String() != "response.failed" {
					continue
				}
				require.Equal(t, "resp_known", root.Get("response.id").String())
				require.Equal(t, "failed", root.Get("response.status").String())
				require.EqualValues(t, 7, root.Get("sequence_number").Int())
				require.NotEmpty(t, root.Get("response.error.code").String())
				if name == "error" || name == "response.error" {
					require.Equal(t, "provider_busy", root.Get("response.error.code").String())
					require.Equal(t, "busy", root.Get("response.error.message").String())
				}
			}
		})
	}
}

func TestResponsesFailureBeforeFirstWriteKeepsHTTPErrorPath(t *testing.T) {
	recorder, err, _, _ := runResponsesTerminalFixture(t, `data: {"type":"response.failed","response":{"error":{"code":"provider_busy","message":"busy"}}}`+"\n\n")
	require.NotNil(t, err)
	require.True(t, types.IsSkipRetryError(err))
	require.Empty(t, recorder.Body.String())
	require.False(t, recorder.Flushed)
}
