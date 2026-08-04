package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newResponsesTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, recorder
}

func TestOaiResponsesHandlerBillsActualOutputsNotToolDeclarations(t *testing.T) {
	c, _ := newResponsesTestContext()
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-4.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
			dto.BuildInToolWebSearch:  {ToolName: dto.BuildInToolWebSearch},
			dto.BuildInToolFileSearch: {ToolName: dto.BuildInToolFileSearch},
		}},
	}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{
		"status":"completed",
		"tools":[{"type":"web_search"},{"type":"file_search"}],
		"output":[
			{"type":"web_search_call","status":"completed"},
			{"type":"web_search_call","status":"completed"},
			{"type":"file_search_call","status":"completed"},
			{"type":"file_search_call","status":"failed"},
			{"type":"image_generation_call","id":"image-1","status":"completed","result":"image-data","quality":"medium","size":"1024x1536"},
			{"type":"image_generation_call","id":"image-empty","status":"completed","quality":"high","size":"1024x1024"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`))}

	usage, handlerErr := OaiResponsesHandler(c, info, resp)

	require.Nil(t, handlerErr)
	require.Equal(t, 15, usage.TotalTokens)
	require.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount)
	require.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
	require.Equal(t, []relaycommon.ImageGenerationCallInfo{{Quality: "medium", Size: "1024x1536"}}, info.ResponsesUsageInfo.ImageGenerationCalls)
}

func TestOaiResponsesHandlerDoesNotBillIncompleteResponse(t *testing.T) {
	c, _ := newResponsesTestContext()
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-4.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
			dto.BuildInToolWebSearch: {ToolName: dto.BuildInToolWebSearch},
		}},
	}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{
		"status":"incomplete",
		"output":[
			{"type":"web_search_call","status":"completed"},
			{"type":"image_generation_call","id":"image-1","status":"completed","result":"image-data","quality":"high","size":"1024x1024"}
		]
	}`))}

	_, handlerErr := OaiResponsesHandler(c, info, resp)

	require.Nil(t, handlerErr)
	require.Zero(t, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount)
	require.Empty(t, info.ResponsesUsageInfo.ImageGenerationCalls)
}

func TestOaiResponsesStreamHandlerDeduplicatesImageAcrossEvents(t *testing.T) {
	c, _ := newResponsesTestContext()
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-4.1",
		ChannelMeta:     &relaycommon.ChannelMeta{},
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
			dto.BuildInToolWebSearch: {ToolName: dto.BuildInToolWebSearch},
		}},
	}
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","status":"completed"}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"image_generation_call","id":"image-1","status":"completed","result":"image-data","quality":"low","size":"1024x1024"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"image_generation_call","id":"image-1","status":"completed","result":"image-data","quality":"low","size":"1024x1024"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	}, "\n") + "\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}

	usage, handlerErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, handlerErr)
	require.Equal(t, 5, usage.TotalTokens)
	require.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount)
	require.Equal(t, []relaycommon.ImageGenerationCallInfo{{Quality: "low", Size: "1024x1024"}}, info.ResponsesUsageInfo.ImageGenerationCalls)
}
