package openai

import (
	"bytes"
	"fmt"
	"github.com/QuantumNous/new-api/service/openaicompat"
	"github.com/tidwall/sjson"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func OaiResponsesToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var responsesResp dto.OpenAIResponsesResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	if err := common.Unmarshal(body, &responsesResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := responsesResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	chatId := helper.GetResponseID(c)
	chatResp, usage, err := service.ResponsesResponseToChatCompletionsResponse(&responsesResp, chatId)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	info.ObserveResponsesTerminal("", &responsesResp)
	usageEstimate := responsesUsageEstimate{useModelEstimate: true}
	usageEstimate.observeResponse(&responsesResp, string(body), "", usage)
	usageEstimate.finish(c, info, usage)
	chatResp.Usage = *usage

	var responseBody []byte
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		claudeResp := service.ResponseOpenAI2Claude(chatResp, info)
		responseBody, err = common.Marshal(claudeResp)
	case types.RelayFormatGemini:
		geminiResp := service.ResponseOpenAI2Gemini(chatResp, info)
		responseBody, err = common.Marshal(geminiResp)
	default:
		responseBody, err = common.Marshal(chatResp)
	}
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

// The transport and accounting guards stay in the host; PR #5772 supplies
// the pure output-index/tool/reasoning conversion state machine.
func OaiResponsesToChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)
	state := openaicompat.NewResponsesToChatStreamState(info.UpstreamModelName, false)
	state.ID = helper.GetResponseID(c)
	state.Created = time.Now().Unix()
	usage := &dto.Usage{}
	estimate := responsesUsageEstimate{useModelEstimate: true}
	var streamErr *types.NewAPIError
	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo == nil {
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{LastMessagesType: relaycommon.LastMessageTypeNone}
	}
	sendChunk := func(chunk dto.ChatCompletionsStreamResponse) bool {
		var err error
		if info.RelayFormat == types.RelayFormatOpenAI {
			err = helper.ObjectData(c, &chunk)
		} else {
			data, marshalErr := common.Marshal(&chunk)
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = HandleStreamFormat(c, info, string(data), false, false)
			}
		}
		if err != nil {
			streamErr = helper.DownstreamStreamError(c, info, err)
			if streamErr == nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
			}
			return false
		}
		return true
	}
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if data == "[DONE]" {
			sr.Stop(fmt.Errorf("unexpected [DONE] marker in Responses stream"))
			return
		}
		var event dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &event); err != nil {
			sr.Error(err)
			return
		}
		if event.Type == "" {
			sr.Error(fmt.Errorf("responses stream event is missing type"))
			return
		}
		if !sr.Accept() {
			return
		}
		switch event.Type {
		case "error", "response.error", "response.failed", "response.cancelled", "response.canceled":
			streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", event.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
			sr.Stop(streamErr)
			return
		}
		info.ObserveResponsesTerminal(event.Type, event.Response)
		estimate.observe(&event, data, usage)
		if info.ClaudeConvertInfo != nil {
			info.ClaudeConvertInfo.Usage = usage
		}
		chunks, err := openaicompat.ResponsesStreamEventToChatChunks(&event, state)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
			sr.Stop(streamErr)
			return
		}
		for _, chunk := range chunks {
			if !sendChunk(chunk) {
				sr.Stop(streamErr)
				return
			}
		}
		switch event.Type {
		case "response.completed", "response.done", "response.incomplete":
			sr.Done()
		}
	})
	if err := helper.PreOutputStreamError(c, info); err != nil {
		return nil, err
	}
	if streamErr != nil && info.PartialStreamError != streamErr {
		if helper.StreamStarted(c) {
			_ = helper.SendInBandStreamError(c, info.RelayFormat, streamErr)
		}
		return nil, streamErr
	}
	estimate.finish(c, info, usage)
	if streamErr != nil {
		return usage, streamErr
	}
	if err := helper.PostOutputStreamError(c, info); err != nil {
		_ = helper.SendInBandStreamError(c, info.RelayFormat, err)
		return usage, err
	}
	if helper.ShouldFinalizeStream(info) {
		if info.RelayFormat == types.RelayFormatOpenAI {
			if info.ShouldIncludeUsage {
				if !sendChunk(*helper.GenerateFinalUsageResponse(state.ID, state.Created, state.Model, *usage)) {
					return usage, streamErr
				}
			}
			if err := helper.StringData(c, "[DONE]"); err != nil {
				return usage, helper.DownstreamStreamError(c, info, err)
			}
		}
	}
	return usage, nil
}

// Buffer an upstream Responses SSE stream for a non-stream Chat client. This
// uses the shared timeout/cancellation scanner, and never invents completed on EOF.
func OaiResponsesToChatBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)
	oldPing := info.DisablePing
	info.DisablePing = true
	defer func() { info.DisablePing = oldPing }()
	accumulator := openaicompat.NewResponsesBufferedAccumulator()
	var terminal *dto.OpenAIResponsesResponse
	observedUsage := &dto.Usage{}
	observed := responsesUsageEstimate{}
	reportedUsage := func() *dto.Usage {
		if observed.promptReported || observed.completionReported {
			return observedUsage
		}
		return nil
	}
	var apiErr *types.NewAPIError
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if data == "[DONE]" {
			sr.Stop(fmt.Errorf("Responses stream ended without a terminal event"))
			return
		}
		var event dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &event); err != nil {
			sr.Error(err)
			return
		}
		if event.Type == "" {
			sr.Error(fmt.Errorf("responses stream event is missing type"))
			return
		}
		if !sr.Accept() {
			return
		}
		switch event.Type {
		case "error", "response.error", "response.failed", "response.cancelled", "response.canceled":
			apiErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", event.Type), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
			sr.Stop(apiErr)
			return
		}
		observed.observe(&event, data, observedUsage)
		accumulator.ProcessEvent(&event)
		switch event.Type {
		case "response.completed", "response.done", "response.incomplete":
			terminal = event.Response
			if terminal == nil {
				terminal = &dto.OpenAIResponsesResponse{}
			}
			if event.Type == "response.incomplete" {
				terminal.Status = []byte(`"incomplete"`)
			} else if len(terminal.Status) == 0 {
				terminal.Status = []byte(`"completed"`)
			}
			info.ObserveResponsesTerminal(event.Type, terminal)
			observed.recordSources(info, false, false)
			sr.Done()
		}
	})
	if apiErr != nil {
		return reportedUsage(), apiErr
	}
	if err := helper.PreOutputStreamError(c, info); err != nil {
		return reportedUsage(), err
	}
	if terminal == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("Responses stream has no terminal"), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
	}
	accumulator.SupplementResponseOutput(terminal)
	body, err := common.Marshal(terminal)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	// Retain field presence and earlier usage trailers while buffering. On a
	// failed buffer no output reached the client, so the existing refund path
	// remains in force even when reported usage is returned for diagnostics.
	sparseUsage := map[string]any{}
	if observed.promptReported {
		sparseUsage["input_tokens"] = observedUsage.PromptTokens
	}
	if observed.completionReported {
		sparseUsage["output_tokens"] = observedUsage.CompletionTokens
	}
	sparseUsage["input_tokens_details"] = observedUsage.PromptTokensDetails
	sparseUsage["output_tokens_details"] = observedUsage.CompletionTokenDetails
	if len(sparseUsage) > 0 {
		raw, marshalErr := common.Marshal(sparseUsage)
		if marshalErr != nil {
			return nil, types.NewOpenAIError(marshalErr, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		}
		body, err = sjson.SetRawBytes(body, "usage", raw)
	} else {
		body, err = sjson.DeleteBytes(body, "usage")
	}
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	headers := resp.Header.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	return OaiResponsesToChatHandler(c, info, &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(bytes.NewReader(body))})
}
