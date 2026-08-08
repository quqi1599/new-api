package cohere

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func requestOpenAI2Cohere(textRequest dto.GeneralOpenAIRequest) *CohereRequest {
	cohereReq := CohereRequest{
		Model:       textRequest.Model,
		ChatHistory: []ChatHistory{},
		Message:     "",
		Stream:      lo.FromPtrOr(textRequest.Stream, false),
		MaxTokens:   textRequest.GetMaxTokens(),
	}
	if common.CohereSafetySetting != "NONE" {
		cohereReq.SafetyMode = common.CohereSafetySetting
	}
	if cohereReq.MaxTokens == 0 {
		cohereReq.MaxTokens = 4000
	}
	for _, msg := range textRequest.Messages {
		if msg.Role == "user" {
			cohereReq.Message = msg.StringContent()
		} else {
			var role string
			if msg.Role == "assistant" {
				role = "CHATBOT"
			} else if msg.Role == "system" {
				role = "SYSTEM"
			} else {
				role = "USER"
			}
			cohereReq.ChatHistory = append(cohereReq.ChatHistory, ChatHistory{
				Role:    role,
				Message: msg.StringContent(),
			})
		}
	}

	return &cohereReq
}

func requestConvertRerank2Cohere(rerankRequest dto.RerankRequest) *CohereRerankRequest {
	topN := lo.FromPtrOr(rerankRequest.TopN, 1)
	if topN <= 0 {
		topN = 1
	}
	cohereReq := CohereRerankRequest{
		Query:           rerankRequest.Query,
		Documents:       rerankRequest.Documents,
		Model:           rerankRequest.Model,
		TopN:            topN,
		ReturnDocuments: true,
	}
	return &cohereReq
}

func stopReasonCohere2OpenAI(reason string) string {
	switch reason {
	case "COMPLETE":
		return "stop"
	case "MAX_TOKENS":
		return "max_tokens"
	default:
		return reason
	}
}

// cohereNDJSONDecoder preserves one Cohere JSON object per physical line while
// delegating cancellation, timeout, and scanner cleanup to the shared stream
// state machine. JSON validation intentionally remains in the adapter so a
// malformed line can never count as the first valid upstream event.
type cohereNDJSONDecoder struct{}

func (cohereNDJSONDecoder) Feed(line string) ([]helper.StreamFrame, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	return []helper.StreamFrame{{Kind: "ndjson", Data: line}}, nil
}

func (cohereNDJSONDecoder) Flush() ([]helper.StreamFrame, error) { return nil, nil }

func cohereFinishReason(response CohereResponse) string {
	reason := strings.TrimSpace(response.FinishReason)
	if reason == "" && response.Response != nil {
		reason = strings.TrimSpace(response.Response.FinishReason)
	}
	return strings.ToUpper(reason)
}

func cohereStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("empty response"), types.ErrorCodeBadResponse, http.StatusBadRequest)
	}
	defer service.CloseResponseBodyGracefully(resp)

	responseId := helper.GetResponseID(c)
	createdTime := common.GetTimestamp()
	usage := &dto.Usage{}
	var responseText strings.Builder

	helper.StreamScannerHandlerWithDecoder(c, resp, info, cohereNDJSONDecoder{}, func(frame helper.StreamFrame, sr *helper.StreamResult) {
		var cohereResp CohereResponse
		if err := common.Unmarshal([]byte(frame.Data), &cohereResp); err != nil {
			common.SysLog("error unmarshalling Cohere stream response: " + err.Error())
			sr.Stop(err)
			return
		}

		eventType := strings.ToLower(strings.TrimSpace(cohereResp.EventType))
		finishReason := cohereFinishReason(cohereResp)
		if cohereResp.Error != nil || strings.Contains(eventType, "error") || strings.Contains(finishReason, "ERROR") {
			sr.Stop(fmt.Errorf("Cohere stream returned an error event"))
			return
		}

		terminal := cohereResp.IsFinished || eventType == "stream-end"
		if terminal && finishReason != "COMPLETE" && finishReason != "MAX_TOKENS" {
			sr.Stop(fmt.Errorf("invalid Cohere terminal event"))
			return
		}
		if !terminal && eventType == "" && cohereResp.Text == "" {
			sr.Stop(fmt.Errorf("invalid Cohere stream event"))
			return
		}
		if !terminal && finishReason != "" {
			sr.Stop(fmt.Errorf("invalid Cohere non-terminal finish reason"))
			return
		}
		if !sr.Accept() {
			return
		}

		openaiResp := dto.ChatCompletionsStreamResponse{
			Id:      responseId,
			Created: createdTime,
			Object:  "chat.completion.chunk",
			Model:   info.UpstreamModelName,
		}
		if terminal {
			openAIReason := stopReasonCohere2OpenAI(finishReason)
			openaiResp.Choices = []dto.ChatCompletionsStreamResponseChoice{{
				Delta:        dto.ChatCompletionsStreamResponseChoiceDelta{},
				Index:        0,
				FinishReason: &openAIReason,
			}}
			if cohereResp.Response != nil {
				usage.PromptTokens = cohereResp.Response.Meta.BilledUnits.InputTokens
				usage.CompletionTokens = cohereResp.Response.Meta.BilledUnits.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
		} else {
			openaiResp.Choices = []dto.ChatCompletionsStreamResponseChoice{{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					Role:    "assistant",
					Content: &cohereResp.Text,
				},
				Index: 0,
			}}
			responseText.WriteString(cohereResp.Text)
		}

		if err := helper.ObjectData(c, openaiResp); err != nil {
			sr.Stop(err)
			return
		}
		if terminal {
			sr.Done()
		}
	})

	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return nil, streamErr
	}
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		usage = service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	if helper.ShouldFinalizeStream(info) {
		helper.Done(c)
	}
	return usage, nil
}

func cohereHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	createdTime := common.GetTimestamp()
	defer service.CloseResponseBodyGracefully(resp)
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	var cohereResp CohereResponseResult
	err = json.Unmarshal(responseBody, &cohereResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	usage := dto.Usage{}
	usage.PromptTokens = cohereResp.Meta.BilledUnits.InputTokens
	usage.CompletionTokens = cohereResp.Meta.BilledUnits.OutputTokens
	usage.TotalTokens = cohereResp.Meta.BilledUnits.InputTokens + cohereResp.Meta.BilledUnits.OutputTokens

	var openaiResp dto.TextResponse
	openaiResp.Id = cohereResp.ResponseId
	openaiResp.Created = createdTime
	openaiResp.Object = "chat.completion"
	openaiResp.Model = info.UpstreamModelName
	openaiResp.Usage = usage

	openaiResp.Choices = []dto.OpenAITextResponseChoice{
		{
			Index:        0,
			Message:      dto.Message{Content: cohereResp.Text, Role: "assistant"},
			FinishReason: stopReasonCohere2OpenAI(cohereResp.FinishReason),
		},
	}

	jsonResponse, err := json.Marshal(openaiResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)
	return &usage, nil
}

func cohereRerankHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	var cohereResp CohereRerankResponseResult
	err = json.Unmarshal(responseBody, &cohereResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	usage := dto.Usage{}
	if cohereResp.Meta.BilledUnits.InputTokens == 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
		usage.CompletionTokens = 0
		usage.TotalTokens = info.GetEstimatePromptTokens()
	} else {
		usage.PromptTokens = cohereResp.Meta.BilledUnits.InputTokens
		usage.CompletionTokens = cohereResp.Meta.BilledUnits.OutputTokens
		usage.TotalTokens = cohereResp.Meta.BilledUnits.InputTokens + cohereResp.Meta.BilledUnits.OutputTokens
	}

	var rerankResp dto.RerankResponse
	rerankResp.Results = cohereResp.Results
	rerankResp.Usage = usage

	jsonResponse, err := json.Marshal(rerankResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return &usage, nil
}
