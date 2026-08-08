package coze

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *common.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

// ConvertAudioRequest implements channel.Adaptor.
func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *common.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("not implemented")
}

// ConvertClaudeRequest implements channel.Adaptor.
func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *common.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertEmbeddingRequest implements channel.Adaptor.
func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *common.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertImageRequest implements channel.Adaptor.
func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *common.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertOpenAIRequest implements channel.Adaptor.
func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *common.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	return convertCozeChatRequest(c, *request), nil
}

// ConvertOpenAIResponsesRequest implements channel.Adaptor.
func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *common.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertRerankRequest implements channel.Adaptor.
func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// DoRequest implements channel.Adaptor.
func (a *Adaptor) DoRequest(c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (any, error) {
	if info.IsStream {
		return channel.DoApiRequest(a, c, info, requestBody)
	}
	budget, err := newCozeRequestBudget(c, info)
	if err != nil {
		return nil, err
	}
	c.Set(cozeRequestBudgetKey, budget)
	handOffBudget := false
	defer func() {
		if !handOffBudget {
			budget.finish()
			c.Set(cozeRequestBudgetKey, nil)
		}
	}()

	// 首先发送创建消息请求，成功后再发送获取消息请求
	// 发送创建消息请求
	originalRequest := c.Request
	c.Request = originalRequest.WithContext(budget.ctx)
	resp, err := channel.DoApiRequest(a, c, info, requestBody)
	c.Request = originalRequest
	if err != nil {
		if budget.expired() {
			return nil, cozeTotalTimeoutError(c, budget, true)
		}
		return nil, err
	}
	// 解析 resp
	var cozeResponse CozeChatResponse
	respBody, readErr := readCozeResponseBody(c, resp, budget)
	service.CloseResponseBodyGracefully(resp)
	if readErr != nil {
		return nil, readErr
	}
	err = common2.Unmarshal(respBody, &cozeResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
	}
	if cozeResponse.Code != 0 {
		return nil, types.NewError(errors.New(cozeResponse.Msg), types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty())
	}
	c.Set("coze_conversation_id", cozeResponse.Data.ConversationId)
	c.Set("coze_chat_id", cozeResponse.Data.Id)
	// 轮询检查消息是否完成
	for {
		err, isComplete := checkIfChatComplete(a, c, info, budget)
		if err != nil {
			return nil, err
		} else {
			if isComplete {
				break
			}
		}
		select {
		case <-budget.ctx.Done():
			if budget.expired() {
				return nil, cozeTotalTimeoutError(c, budget, true)
			}
			return nil, channel.ClassifyDoRequestError(c, budget.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	// 发送获取消息请求
	resp, err = getChatDetail(a, c, info, budget)
	if err != nil {
		return nil, err
	}
	resp.Body = &cozeBudgetBody{ReadCloser: resp.Body, budget: budget}
	handOffBudget = true
	return resp, nil
}

// DoResponse implements channel.Adaptor.
func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *common.RelayInfo) (usage any, err *types.NewAPIError) {
	if budget := getCozeRequestBudget(c); budget != nil {
		defer func() {
			budget.finish()
			c.Set(cozeRequestBudgetKey, nil)
		}()
	}
	if info.IsStream {
		usage, err = cozeChatStreamHandler(c, info, resp)
	} else {
		usage, err = cozeChatHandler(c, info, resp)
	}
	return
}

// GetChannelName implements channel.Adaptor.
func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

// GetModelList implements channel.Adaptor.
func (a *Adaptor) GetModelList() []string {
	return ModelList
}

// GetRequestURL implements channel.Adaptor.
func (a *Adaptor) GetRequestURL(info *common.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/v3/chat", info.ChannelBaseUrl), nil
}

// Init implements channel.Adaptor.
func (a *Adaptor) Init(info *common.RelayInfo) {

}

// SetupRequestHeader implements channel.Adaptor.
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *common.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}
