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
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			output := &responsesResponse.Output[i]
			if !relaycommon.IsBillableResponsesOutput(output) {
				continue
			}
			switch output.Type {
			case dto.BuildInCallWebSearchCall:
				info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
			case dto.BuildInCallFileSearchCall:
				info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
			case dto.BuildInCallFunctionCall:
				info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
			}
		}
	}
	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			index := i
			imageCounter.Observe(&responsesResponse.Output[i], &index)
		}
	}
	imageCounter.Commit(info)
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
	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	imageCommitted := false

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
		case "error", "response.error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
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
			if !imageCommitted {
				imageCounter.Reset()
				imageCounter.Commit(info)
				imageCommitted = true
			}
			sr.Stop(streamFailure)
			return
		}
		switch streamResponse.Type {
		case "response.completed", "response.done":
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
				if !imageCommitted {
					if relaycommon.IsNonBillableResponsesStatus(streamResponse.Response.Status) {
						imageCounter.Reset()
					} else {
						for i := range streamResponse.Response.Output {
							index := i
							imageCounter.Observe(&streamResponse.Response.Output[i], &index)
						}
					}
					imageCounter.Commit(info)
					imageCommitted = true
				}
			} else if !imageCommitted {
				imageCounter.Commit(info)
				imageCommitted = true
			}
			sr.Done()
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			if streamResponse.Item != nil {
				if !relaycommon.IsBillableResponsesOutput(streamResponse.Item) {
					break
				}
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
				case dto.BuildInCallFileSearchCall:
					info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
				case dto.BuildInCallFunctionCall:
					info.CountBillableToolCall(dto.BuildInCallFunctionCall, streamResponse.Item.Name)
				case dto.ResponsesOutputTypeImageGenerationCall:
					if !imageCommitted {
						imageCounter.Observe(streamResponse.Item, streamResponse.OutputIndex)
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
