package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHandleClaudeResponseDataConvertsToResponsesFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAIResponses,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}
	claudeInfo := &ClaudeResponseInfo{Usage: &dto.Usage{}}
	data := []byte(`{
		"id":"msg_123","type":"message","role":"assistant","model":"claude-test",
		"content":[
			{"type":"text","text":"check"},
			{"type":"text","text":"ing"},
			{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}
		],
		"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}
	}`)
	httpResponse := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	err := HandleClaudeResponseData(c, info, claudeInfo, httpResponse, data)
	require.Nil(t, err)
	var response dto.OpenAIResponsesResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "response", response.Object)
	require.Len(t, response.Output, 2)
	require.Equal(t, "checking", response.Output[0].Content[0].Text)
	require.Equal(t, "function_call", response.Output[1].Type)
	require.Equal(t, "lookup", response.Output[1].Name)
	require.Equal(t, `{"q":"x"}`, response.Output[1].ArgumentsString())
	require.NotNil(t, response.Usage)
	require.Equal(t, 10, response.Usage.InputTokens)
	require.Equal(t, 5, response.Usage.OutputTokens)
}

func TestClaudeResponsesStreamHandlerEmitsTerminalResponsesContract(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	recorder, c, info, httpResponse := newClaudeResponsesStreamTest(sse)

	usage, err := ClaudeResponsesStreamHandler(c, httpResponse, info)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.Contains(t, recorder.Body.String(), "event: response.created")
	require.Contains(t, recorder.Body.String(), "event: response.output_text.delta")
	require.Contains(t, recorder.Body.String(), `"delta":"ok"`)
	require.Contains(t, recorder.Body.String(), "event: response.completed")
	require.Contains(t, recorder.Body.String(), `"input_tokens":10`)
	require.Contains(t, recorder.Body.String(), `"output_tokens":5`)
	require.Less(t, strings.Index(recorder.Body.String(), "event: response.created"), strings.Index(recorder.Body.String(), "event: response.output_text.delta"))
	require.Less(t, strings.Index(recorder.Body.String(), "event: response.output_text.delta"), strings.Index(recorder.Body.String(), "event: response.completed"))
}

func TestClaudeResponsesStreamHandlerDoesNotCompleteInterruptedStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		``,
	}, "\n")
	recorder, c, info, httpResponse := newClaudeResponsesStreamTest(sse)

	_, err := ClaudeResponsesStreamHandler(c, httpResponse, info)
	require.Nil(t, err)
	require.Contains(t, recorder.Body.String(), "event: response.output_text.delta")
	require.NotContains(t, recorder.Body.String(), "event: response.completed")
}

func newClaudeResponsesStreamTest(sse string) (*httptest.ResponseRecorder, *gin.Context, *relaycommon.RelayInfo, *http.Response) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}
	return recorder, c, info, response
}
