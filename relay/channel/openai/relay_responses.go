package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := dto.Usage{}
	if responsesResponse.Usage != nil {
		usage.PromptTokens = responsesResponse.Usage.InputTokens
		usage.CompletionTokens = responsesResponse.Usage.OutputTokens
		usage.TotalTokens = responsesResponse.Usage.TotalTokens
		if responsesResponse.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = responsesResponse.Usage.InputTokensDetails.CachedTokens
		}
	}
	if info == nil || info.ResponsesUsageInfo == nil || info.ResponsesUsageInfo.BuiltInTools == nil {
		return &usage, nil
	}
	// 解析 Tools 用量
	for _, tool := range responsesResponse.Tools {
		buildToolinfo, ok := info.ResponsesUsageInfo.BuiltInTools[common.Interface2String(tool["type"])]
		if !ok || buildToolinfo == nil {
			logger.LogError(c, fmt.Sprintf("BuiltInTools not found for tool type: %v", tool["type"]))
			continue
		}
		buildToolinfo.CallCount++
	}
	return &usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	var terminalFailure *types.NewAPIError

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if data == "[DONE]" {
			sr.Stop(fmt.Errorf("unexpected [DONE] marker in Responses stream"))
			return
		}

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		if streamResponse.Type == "" {
			sr.Error(fmt.Errorf("responses stream event is missing type"))
			return
		}
		if !sr.Accept() {
			return
		}
		sendResponsesStreamData(c, streamResponse, data)

		// Adapted from official PR #6549: Responses error events are valid SSE
		// payloads, so transport-level success must not erase their business error.
		switch streamResponse.Type {
		case "error", "response.error", "response.failed", "response.incomplete":
			streamError := streamResponse.Error
			if (len(streamError) == 0 || string(streamError) == "null") && streamResponse.Response != nil && streamResponse.Response.Error != nil {
				if errorBytes, err := common.Marshal(streamResponse.Response.Error); err == nil {
					streamError = errorBytes
				}
			}
			streamFailure := fmt.Errorf("responses stream ended with %s", streamResponse.Type)
			if len(streamError) > 0 && string(streamError) != "null" {
				streamFailure = fmt.Errorf("%s: %s", streamResponse.Type, streamError)
			}
			terminalFailure = types.NewErrorWithStatusCode(
				streamFailure,
				types.ErrorCodeBadResponse,
				http.StatusBadGateway,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithChannelPenalty(),
			)
			sr.Stop(streamFailure)
			return
		}
		switch streamResponse.Type {
		case "response.completed":
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					if streamResponse.Response.Usage.InputTokens != 0 {
						usage.PromptTokens = streamResponse.Response.Usage.InputTokens
					}
					if streamResponse.Response.Usage.OutputTokens != 0 {
						usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
					}
					if streamResponse.Response.Usage.TotalTokens != 0 {
						usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
					}
					if streamResponse.Response.Usage.InputTokensDetails != nil {
						usage.PromptTokensDetails.CachedTokens = streamResponse.Response.Usage.InputTokensDetails.CachedTokens
					}
				}
				if streamResponse.Response.HasImageGenerationCall() {
					c.Set("image_generation_call", true)
					c.Set("image_generation_call_quality", streamResponse.Response.GetQuality())
					c.Set("image_generation_call_size", streamResponse.Response.GetSize())
				}
			}
			sr.Done()
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			// 函数调用处理
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					if info != nil && info.ResponsesUsageInfo != nil && info.ResponsesUsageInfo.BuiltInTools != nil {
						if webSearchTool, exists := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool != nil {
							webSearchTool.CallCount++
						}
					}
				}
			}
		}
	})
	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return nil, streamErr
	}
	if terminalFailure != nil {
		// Protocol-native error frames may already have committed an SSE 200. The
		// controller suppresses a trailing JSON error in that case, but it still
		// needs a non-nil result to avoid success settlement and success logging.
		return usage, terminalFailure
	}

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	return usage, nil
}
