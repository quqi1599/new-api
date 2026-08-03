package palm

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// https://developers.generativeai.google/api/rest/generativelanguage/models/generateMessage#request-body
// https://developers.generativeai.google/api/rest/generativelanguage/models/generateMessage#response-body

func responsePaLM2OpenAI(response *PaLMChatResponse) *dto.OpenAITextResponse {
	fullTextResponse := dto.OpenAITextResponse{
		Choices: make([]dto.OpenAITextResponseChoice, 0, len(response.Candidates)),
	}
	for i, candidate := range response.Candidates {
		choice := dto.OpenAITextResponseChoice{
			Index: i,
			Message: dto.Message{
				Role:    "assistant",
				Content: candidate.Content,
			},
			FinishReason: "stop",
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	}
	return &fullTextResponse
}

func streamResponsePaLM2OpenAI(palmResponse *PaLMChatResponse) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	if len(palmResponse.Candidates) > 0 {
		choice.Delta.SetContentString(palmResponse.Candidates[0].Content)
	}
	choice.FinishReason = &constant.FinishReasonStop
	var response dto.ChatCompletionsStreamResponse
	response.Object = "chat.completion.chunk"
	response.Model = "palm2"
	response.Choices = []dto.ChatCompletionsStreamResponseChoice{choice}
	return &response
}

func palmStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, string) {
	defer service.CloseResponseBodyGracefully(resp)
	// PaLM's legacy "stream" endpoint returns one complete JSON document rather
	// than line-oriented SSE. Keep that read inside the same request-wide
	// first-valid-event budget used by the shared stream scanner. Closing the body
	// is required here because the response-header timer has already been
	// disarmed by doRequest.
	var budgetState atomic.Uint32 // 0=armed, 1=disarmed, 2=expired
	var budgetTimer *time.Timer
	if remaining, limited := info.RemainingFirstValidEventBudget(); limited {
		if remaining <= 0 {
			return palmFirstEventBudgetError(), ""
		}
		budgetTimer = time.AfterFunc(remaining, func() {
			if budgetState.CompareAndSwap(0, 2) {
				_ = resp.Body.Close()
			}
		})
	}
	responseBody, err := io.ReadAll(resp.Body)
	if budgetTimer != nil {
		budgetState.CompareAndSwap(0, 1)
		budgetTimer.Stop()
	}
	if budgetState.Load() == 2 && (c == nil || c.Request == nil || c.Request.Context().Err() == nil) {
		return palmFirstEventBudgetError(), ""
	}
	if err != nil {
		return types.NewError(err, types.ErrorCodeReadResponseBodyFailed), ""
	}
	var palmResponse PaLMChatResponse
	if err := common.Unmarshal(responseBody, &palmResponse); err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), ""
	}
	if palmResponse.Error.Code != 0 {
		return types.WithOpenAIError(types.OpenAIError{
			Message: palmResponse.Error.Message,
			Type:    palmResponse.Error.Status,
			Code:    palmResponse.Error.Code,
		}, resp.StatusCode), ""
	}
	if len(palmResponse.Candidates) == 0 {
		return types.NewError(fmt.Errorf("PaLM stream response contained no candidates"), types.ErrorCodeBadResponseBody), ""
	}

	responseId := helper.GetResponseID(c)
	createdTime := common.GetTimestamp()
	fullTextResponse := streamResponsePaLM2OpenAI(&palmResponse)
	fullTextResponse.Id = responseId
	fullTextResponse.Created = createdTime
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), ""
	}

	info.SetFirstResponseTime()
	info.ReceivedResponseCount++
	if err := helper.StringData(c, string(jsonResponse)); err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry()), ""
	}
	helper.Done(c)
	return nil, palmResponse.Candidates[0].Content
}

func palmFirstEventBudgetError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New("request-wide first valid event budget exhausted"),
		types.ErrorCodeUpstreamFirstEventTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
}

func palmHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)
	var palmResponse PaLMChatResponse
	err = common.Unmarshal(responseBody, &palmResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if palmResponse.Error.Code != 0 || len(palmResponse.Candidates) == 0 {
		return nil, types.WithOpenAIError(types.OpenAIError{
			Message: palmResponse.Error.Message,
			Type:    palmResponse.Error.Status,
			Param:   "",
			Code:    palmResponse.Error.Code,
		}, resp.StatusCode)
	}
	fullTextResponse := responsePaLM2OpenAI(&palmResponse)
	usage := service.ResponseText2Usage(c, palmResponse.Candidates[0].Content, info.UpstreamModelName, info.GetEstimatePromptTokens())
	fullTextResponse.Usage = *usage
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	service.IOCopyBytesGracefully(c, resp, jsonResponse)
	return usage, nil
}
