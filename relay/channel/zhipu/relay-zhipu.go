package zhipu

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// https://open.bigmodel.cn/doc/api#chatglm_std
// chatglm_std, chatglm_lite
// https://open.bigmodel.cn/api/paas/v3/model-api/chatglm_std/invoke
// https://open.bigmodel.cn/api/paas/v3/model-api/chatglm_std/sse-invoke

var zhipuTokens sync.Map
var expSeconds int64 = 24 * 3600

func getZhipuToken(apikey string) string {
	data, ok := zhipuTokens.Load(apikey)
	if ok {
		tokenData := data.(zhipuTokenData)
		if time.Now().Before(tokenData.ExpiryTime) {
			return tokenData.Token
		}
	}

	split := strings.Split(apikey, ".")
	if len(split) != 2 {
		common.SysLog("invalid zhipu key: " + apikey)
		return ""
	}

	id := split[0]
	secret := split[1]

	expMillis := time.Now().Add(time.Duration(expSeconds)*time.Second).UnixNano() / 1e6
	expiryTime := time.Now().Add(time.Duration(expSeconds) * time.Second)

	timestamp := time.Now().UnixNano() / 1e6

	payload := jwt.MapClaims{
		"api_key":   id,
		"exp":       expMillis,
		"timestamp": timestamp,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, payload)

	token.Header["alg"] = "HS256"
	token.Header["sign_type"] = "SIGN"

	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		return ""
	}

	zhipuTokens.Store(apikey, zhipuTokenData{
		Token:      tokenString,
		ExpiryTime: expiryTime,
	})

	return tokenString
}

func requestOpenAI2Zhipu(request dto.GeneralOpenAIRequest) *ZhipuRequest {
	messages := make([]ZhipuMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" {
			messages = append(messages, ZhipuMessage{
				Role:    "system",
				Content: message.StringContent(),
			})
			messages = append(messages, ZhipuMessage{
				Role:    "user",
				Content: "Okay",
			})
		} else {
			messages = append(messages, ZhipuMessage{
				Role:    message.Role,
				Content: message.StringContent(),
			})
		}
	}
	return &ZhipuRequest{
		Prompt:      messages,
		Temperature: request.Temperature,
		TopP:        lo.FromPtrOr(request.TopP, 0),
		Incremental: false,
	}
}

func responseZhipu2OpenAI(response *ZhipuResponse) *dto.OpenAITextResponse {
	fullTextResponse := dto.OpenAITextResponse{
		Id:      response.Data.TaskId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Choices: make([]dto.OpenAITextResponseChoice, 0, len(response.Data.Choices)),
		Usage:   response.Data.Usage,
	}
	for i, choice := range response.Data.Choices {
		openaiChoice := dto.OpenAITextResponseChoice{
			Index: i,
			Message: dto.Message{
				Role:    choice.Role,
				Content: strings.Trim(choice.Content, "\""),
			},
			FinishReason: "",
		}
		if i == len(response.Data.Choices)-1 {
			openaiChoice.FinishReason = "stop"
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, openaiChoice)
	}
	return &fullTextResponse
}

func streamResponseZhipu2OpenAI(zhipuResponse string) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString(zhipuResponse)
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "chatglm",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func streamMetaResponseZhipu2OpenAI(zhipuResponse *ZhipuStreamMetaResponse) (*dto.ChatCompletionsStreamResponse, *dto.Usage) {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString("")
	choice.FinishReason = &constant.FinishReasonStop
	response := dto.ChatCompletionsStreamResponse{
		Id:      zhipuResponse.RequestId,
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "chatglm",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response, &zhipuResponse.Usage
}

func zhipuStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var usage *dto.Usage
	var responseText strings.Builder

	helper.StreamScannerHandlerWithDecoder(c, resp, info, zhipuV3LineDecoder{}, func(frame helper.StreamFrame, sr *helper.StreamResult) {
		switch frame.Kind {
		case "data":
			data := frame.Data
			if strings.TrimSpace(data) == "" {
				sr.Error(fmt.Errorf("empty Zhipu data frame"))
				return
			}
			if strings.TrimSpace(data) == "[DONE]" {
				// The v3 protocol is complete only when its meta frame reports
				// task_status=SUCCESS.
				sr.Error(fmt.Errorf("Zhipu stream received [DONE] without SUCCESS meta"))
				return
			}
			if !sr.Accept() {
				return
			}
			response := streamResponseZhipu2OpenAI(data)
			response.Model = info.UpstreamModelName
			responseText.WriteString(data)
			if err := helper.ObjectData(c, response); err != nil {
				sr.Stop(fmt.Errorf("write Zhipu data response: %w", err))
			}

		case "meta":
			var zhipuResponse ZhipuStreamMetaResponse
			if err := common.Unmarshal([]byte(frame.Data), &zhipuResponse); err != nil {
				sr.Stop(fmt.Errorf("invalid Zhipu meta frame: %w", err))
				return
			}

			status := strings.ToUpper(strings.TrimSpace(zhipuResponse.TaskStatus))
			switch status {
			case "SUCCESS":
				if !sr.Accept() {
					return
				}
				response, zhipuUsage := streamMetaResponseZhipu2OpenAI(&zhipuResponse)
				response.Model = info.UpstreamModelName
				if err := helper.ObjectData(c, response); err != nil {
					sr.Stop(fmt.Errorf("write Zhipu terminal response: %w", err))
					return
				}
				usage = zhipuUsage
				sr.Done()

			case "FAIL", "FAILED", "ERROR", "CANCELED", "CANCELLED", "REJECTED", "TIMEOUT":
				sr.Stop(fmt.Errorf("Zhipu stream failed with task status %s", status))

			case "PROCESSING", "RUNNING", "PENDING":
				// A valid progress meta frame extends the idle budget, but it does
				// not make an EOF successful without a later SUCCESS frame.
				sr.Accept()

			case "":
				sr.Error(fmt.Errorf("Zhipu meta frame is missing task_status"))

			default:
				sr.Error(fmt.Errorf("unsupported Zhipu task status %s", status))
			}
		}
	})
	service.CloseResponseBodyGracefully(resp)

	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return nil, streamErr
	}
	if usage == nil {
		usage = service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	if helper.ShouldFinalizeStream(info) {
		helper.Done(c)
	}
	return usage, nil
}

// zhipuV3LineDecoder preserves the legacy v3 wire types while delegating all
// cancellation, timeout, and goroutine lifecycle handling to the shared stream
// state machine.
type zhipuV3LineDecoder struct{}

func (zhipuV3LineDecoder) Feed(line string) ([]helper.StreamFrame, error) {
	switch {
	case strings.HasPrefix(line, "data:"):
		return []helper.StreamFrame{{Kind: "data", Data: line[len("data:"):]}}, nil
	case strings.HasPrefix(line, "meta:"):
		return []helper.StreamFrame{{Kind: "meta", Data: line[len("meta:"):]}}, nil
	default:
		return nil, nil
	}
}

func (zhipuV3LineDecoder) Flush() ([]helper.StreamFrame, error) { return nil, nil }

func zhipuHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var zhipuResponse ZhipuResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)
	err = json.Unmarshal(responseBody, &zhipuResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if !zhipuResponse.Success {
		return nil, types.WithOpenAIError(types.OpenAIError{
			Message: zhipuResponse.Msg,
			Code:    zhipuResponse.Code,
		}, resp.StatusCode)
	}
	fullTextResponse := responseZhipu2OpenAI(&zhipuResponse)
	jsonResponse, err := json.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return &fullTextResponse.Usage, nil
}
