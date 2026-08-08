package coze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

const cozeRequestBudgetKey = "coze_request_budget"

type cozeRequestBudget struct {
	ctx            context.Context
	parent         context.Context
	cancel         context.CancelFunc
	timeoutSeconds int
	state          atomic.Uint32 // 0=armed, 1=finished, 2=expired
	timer          *time.Timer
	finishOnce     sync.Once
}

func newCozeRequestBudget(c *gin.Context, info *relaycommon.RelayInfo) (*cozeRequestBudget, error) {
	timeoutSeconds := common.RelayNonStreamTimeout
	if timeoutSeconds <= 0 {
		return newCozeRequestBudgetWithTimeout(c, 0, 0)
	}
	totalTimeout := time.Duration(timeoutSeconds) * time.Second
	timeout := totalTimeout
	if info != nil {
		info.EnsureNonStreamDeadline(info.StartTime, totalTimeout)
		if remaining, limited := info.RemainingNonStreamBudget(); limited {
			if remaining <= 0 {
				return nil, cozeTotalTimeoutError(c, &cozeRequestBudget{timeoutSeconds: timeoutSeconds}, info.UpstreamRequestMayHaveBeenAccepted())
			}
			timeout = remaining
		}
	}
	return newCozeRequestBudgetWithTimeout(c, timeout, timeoutSeconds)
}

func newCozeRequestBudgetWithTimeout(c *gin.Context, timeout time.Duration, timeoutSeconds int) (*cozeRequestBudget, error) {
	if c == nil || c.Request == nil {
		return nil, errors.New("Coze request context is missing")
	}
	parent := c.Request.Context()
	ctx, cancel := context.WithCancel(parent)
	budget := &cozeRequestBudget{
		ctx:            ctx,
		parent:         parent,
		cancel:         cancel,
		timeoutSeconds: timeoutSeconds,
	}
	if timeout > 0 {
		budget.timer = time.AfterFunc(timeout, func() {
			if budget.state.CompareAndSwap(0, 2) {
				cancel()
			}
		})
	}
	return budget, nil
}

func (b *cozeRequestBudget) finish() {
	if b == nil {
		return
	}
	b.finishOnce.Do(func() {
		b.state.CompareAndSwap(0, 1)
		if b.timer != nil {
			b.timer.Stop()
		}
		b.cancel()
	})
}

func (b *cozeRequestBudget) expired() bool {
	return b != nil && b.state.Load() == 2
}

func getCozeRequestBudget(c *gin.Context) *cozeRequestBudget {
	if c == nil {
		return nil
	}
	value, exists := c.Get(cozeRequestBudgetKey)
	if !exists {
		return nil
	}
	budget, _ := value.(*cozeRequestBudget)
	return budget
}

func setCozeTimeoutMetadata(c *gin.Context, origin, phase string, timeoutSeconds int) {
	if c == nil {
		return
	}
	common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, origin)
	common.SetContextKey(c, constant.ContextKeyRelayTimeoutPhase, phase)
	common.SetContextKey(c, constant.ContextKeyRelayTimeoutSeconds, timeoutSeconds)
}

func cozeTotalTimeoutError(c *gin.Context, budget *cozeRequestBudget, allowChannelPenalty bool) *types.NewAPIError {
	timeoutSeconds := common.RelayNonStreamTimeout
	if budget != nil {
		timeoutSeconds = budget.timeoutSeconds
	}
	setCozeTimeoutMetadata(c, constant.RelayCancelOriginGatewayDeadline, "non_stream_total", timeoutSeconds)
	options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
	if allowChannelPenalty {
		options = append(options, types.ErrOptionWithChannelPenalty())
	}
	return types.NewErrorWithStatusCode(
		errors.New("Coze multi-step response exceeded the total timeout"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		options...,
	)
}

func classifyCozeStageError(c *gin.Context, budget *cozeRequestBudget, err error, phase string) *types.NewAPIError {
	if budget != nil && budget.expired() {
		return cozeTotalTimeoutError(c, budget, true)
	}
	if budget != nil && budget.parent.Err() != nil {
		return channel.ClassifyDoRequestError(c, err)
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		errorCode := types.ErrorCodeUpstreamResponseHeaderTimeout
		timeoutSeconds := common.RelayResponseHeaderTimeout
		message := "Coze response headers timed out"
		if phase == "response_body" {
			errorCode = types.ErrorCodeUpstreamNonStreamTimeout
			timeoutSeconds = common.RelayNonStreamTimeout
			message = "Coze response body timed out"
		}
		setCozeTimeoutMetadata(c, constant.RelayCancelOriginUpstreamTimeout, phase, timeoutSeconds)
		return types.NewErrorWithStatusCode(
			errors.New(message),
			errorCode,
			http.StatusGatewayTimeout,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithChannelPenalty(),
		)
	}
	return types.NewError(
		err,
		types.ErrorCodeDoRequestFailed,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
}

type cozeReadResult struct {
	body []byte
	err  error
}

type cozeBudgetBody struct {
	io.ReadCloser
	budget *cozeRequestBudget
}

func (b *cozeBudgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.budget.finish()
	return err
}

func readCozeResponseBody(c *gin.Context, resp *http.Response, budget *cozeRequestBudget) ([]byte, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewError(errors.New("Coze response body is missing"), types.ErrorCodeBadResponseBody)
	}
	if budget == nil {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		return body, nil
	}

	resultCh := make(chan cozeReadResult, 1)
	go func() {
		body, err := io.ReadAll(resp.Body)
		resultCh <- cozeReadResult{body: body, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, classifyCozeStageError(c, budget, result.err, "response_body")
		}
		return result.body, nil
	case <-budget.ctx.Done():
		_ = resp.Body.Close()
		select {
		case result := <-resultCh:
			if result.err == nil && !budget.expired() && budget.parent.Err() == nil {
				return result.body, nil
			}
		default:
		}
		return nil, classifyCozeStageError(c, budget, budget.ctx.Err(), "response_body")
	}
}

func convertCozeChatRequest(c *gin.Context, request dto.GeneralOpenAIRequest) *CozeChatRequest {
	var messages []CozeEnterMessage
	// 将 request的messages的role为user的content转换为CozeMessage
	for _, message := range request.Messages {
		if message.Role == "user" {
			messages = append(messages, CozeEnterMessage{
				Role:    "user",
				Content: message.Content,
				// TODO: support more content type
				ContentType: "text",
			})
		}
	}
	user := request.User
	if len(user) == 0 {
		user = json.RawMessage(helper.GetResponseID(c))
	}
	cozeRequest := &CozeChatRequest{
		BotId:              c.GetString("bot_id"),
		UserId:             user,
		AdditionalMessages: messages,
		Stream:             lo.FromPtrOr(request.Stream, false),
	}
	return cozeRequest
}

func cozeChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	responseBody, readErr := readCozeResponseBody(c, resp, getCozeRequestBudget(c))
	if readErr != nil {
		return nil, readErr
	}
	// convert coze response to openai response
	var response dto.TextResponse
	var cozeResponse CozeChatDetailResponse
	response.Model = info.UpstreamModelName
	err := common.Unmarshal(responseBody, &cozeResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if cozeResponse.Code != 0 {
		return nil, types.NewError(errors.New(cozeResponse.Msg), types.ErrorCodeBadResponseBody)
	}
	// 从上下文获取 usage
	var usage dto.Usage
	usage.PromptTokens = c.GetInt("coze_input_count")
	usage.CompletionTokens = c.GetInt("coze_output_count")
	usage.TotalTokens = c.GetInt("coze_token_count")
	response.Usage = usage
	response.Id = helper.GetResponseID(c)

	var responseContent json.RawMessage
	for _, data := range cozeResponse.Data {
		if data.Type == "answer" {
			responseContent = data.Content
			response.Created = data.CreatedAt
		}
	}
	// 添加 response.Choices
	response.Choices = []dto.OpenAITextResponseChoice{
		{
			Index:        0,
			Message:      dto.Message{Role: "assistant", Content: responseContent},
			FinishReason: "stop",
		},
	}
	jsonResponse, err := common.Marshal(response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)

	return &usage, nil
}

func cozeChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	id := helper.GetResponseID(c)
	var responseText string
	var usage = &dto.Usage{}

	helper.StreamScannerHandlerWithDecoder(c, resp, info, &cozeEventStreamDecoder{}, func(frame helper.StreamFrame, sr *helper.StreamResult) {
		handleCozeStreamFrame(c, frame, sr, &responseText, usage, id, info)
	})
	service.CloseResponseBodyGracefully(resp)

	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return nil, streamErr
	}

	if usage.TotalTokens == 0 {
		usage = service.ResponseText2Usage(c, responseText, info.UpstreamModelName, c.GetInt("coze_input_count"))
	}
	if helper.ShouldFinalizeStream(info) {
		helper.Done(c)
	}

	return usage, nil
}

// cozeEventStreamDecoder implements Coze's event-aware SSE framing. A Coze
// event can contain multiple data lines and the final event is still valid
// when the upstream closes without a trailing blank line.
type cozeEventStreamDecoder struct {
	event     string
	dataLines []string
}

func (d *cozeEventStreamDecoder) Feed(line string) ([]helper.StreamFrame, error) {
	if line == "" {
		return d.dispatch(), nil
	}
	if strings.HasPrefix(line, ":") {
		return nil, nil
	}

	field, value, hasColon := strings.Cut(line, ":")
	if !hasColon {
		value = ""
	} else if strings.HasPrefix(value, " ") {
		value = value[1:]
	}

	switch field {
	case "event":
		d.event = value
	case "data":
		d.dataLines = append(d.dataLines, value)
	}
	return nil, nil
}

func (d *cozeEventStreamDecoder) Flush() ([]helper.StreamFrame, error) {
	return d.dispatch(), nil
}

func (d *cozeEventStreamDecoder) dispatch() []helper.StreamFrame {
	event := d.event
	dataLines := d.dataLines
	d.event = ""
	d.dataLines = nil

	if len(dataLines) == 0 {
		return nil
	}
	return []helper.StreamFrame{{
		Kind:  "sse",
		Event: event,
		Data:  strings.Join(dataLines, "\n"),
	}}
}

func handleCozeStreamFrame(c *gin.Context, frame helper.StreamFrame, sr *helper.StreamResult, responseText *string, usage *dto.Usage, id string, info *relaycommon.RelayInfo) {
	event := strings.TrimSpace(frame.Event)
	if strings.TrimSpace(frame.Data) == "[DONE]" {
		// Coze's protocol is complete only after conversation.chat.completed.
		// A generic SSE marker is not proof that the chat completed.
		sr.Error(errors.New("coze stream received [DONE] without conversation.chat.completed"))
		return
	}

	switch event {
	case "conversation.chat.completed":
		var chatData CozeChatResponseData
		if err := common.Unmarshal([]byte(frame.Data), &chatData); err != nil {
			sr.Stop(fmt.Errorf("invalid Coze completed event: %w", err))
			return
		}
		if chatData.Status != "" && !strings.EqualFold(chatData.Status, "completed") {
			sr.Stop(fmt.Errorf("invalid Coze completed event status: %s", chatData.Status))
			return
		}
		if chatData.LastError.Code != 0 || chatData.LastError.Message != "" {
			sr.Stop(errors.New("Coze completed event contains an upstream error"))
			return
		}
		if !sr.Accept() {
			return
		}

		usage.PromptTokens = chatData.Usage.InputCount
		usage.CompletionTokens = chatData.Usage.OutputCount
		usage.TotalTokens = chatData.Usage.TokenCount

		finishReason := "stop"
		stopResponse := helper.GenerateStopResponse(id, common.GetTimestamp(), info.UpstreamModelName, finishReason)
		if err := helper.ObjectData(c, stopResponse); err != nil {
			sr.Stop(fmt.Errorf("write Coze stop response: %w", err))
			return
		}
		sr.Done()

	case "conversation.message.delta":
		var messageData CozeChatV3MessageDetail
		if err := common.Unmarshal([]byte(frame.Data), &messageData); err != nil {
			sr.Stop(fmt.Errorf("invalid Coze delta event: %w", err))
			return
		}

		var content string
		if err := common.Unmarshal(messageData.Content, &content); err != nil {
			sr.Stop(fmt.Errorf("invalid Coze delta content: %w", err))
			return
		}
		if !sr.Accept() {
			return
		}

		*responseText += content

		openaiResponse := dto.ChatCompletionsStreamResponse{
			Id:      id,
			Object:  "chat.completion.chunk",
			Created: common.GetTimestamp(),
			Model:   info.UpstreamModelName,
		}

		choice := dto.ChatCompletionsStreamResponseChoice{
			Index: 0,
		}
		choice.Delta.SetContentString(content)
		openaiResponse.Choices = append(openaiResponse.Choices, choice)

		if err := helper.ObjectData(c, openaiResponse); err != nil {
			sr.Stop(fmt.Errorf("write Coze delta response: %w", err))
		}

	case "conversation.chat.created", "conversation.chat.in_progress", "conversation.message.created", "conversation.message.completed":
		var eventData map[string]any
		if err := common.Unmarshal([]byte(frame.Data), &eventData); err != nil || len(eventData) == 0 {
			if err == nil {
				err = errors.New("empty event payload")
			}
			sr.Stop(fmt.Errorf("invalid Coze %s event: %w", event, err))
			return
		}
		sr.Accept()

	case "conversation.chat.failed", "conversation.chat.requires_action", "conversation.chat.canceled", "conversation.chat.cancelled", "error":
		sr.Stop(fmt.Errorf("Coze stream failure event: %s", event))

	case "", "done":
		sr.Error(fmt.Errorf("Coze stream event is missing a supported event type"))

	default:
		// Ignore forward-compatible event types, but do not let an unknown frame
		// satisfy the first-valid-event deadline or complete the stream.
		sr.Error(fmt.Errorf("unsupported Coze stream event: %s", event))
	}
}

func checkIfChatComplete(a *Adaptor, c *gin.Context, info *relaycommon.RelayInfo, budget *cozeRequestBudget) (error, bool) {
	requestURL := fmt.Sprintf("%s/v3/chat/retrieve", info.ChannelBaseUrl)

	requestURL = requestURL + "?conversation_id=" + c.GetString("coze_conversation_id") + "&chat_id=" + c.GetString("coze_chat_id")
	// 将 conversationId和chatId作为参数发送get请求
	req, err := http.NewRequestWithContext(budget.ctx, "GET", requestURL, nil)
	if err != nil {
		return err, false
	}
	err = a.SetupRequestHeader(c, &req.Header, info)
	if err != nil {
		return err, false
	}

	resp, err := doRequest(c, req, info, budget) // 调用 doRequest
	if err != nil {
		return err, false
	}
	if resp == nil { // 确保在 doRequest 失败时 resp 不为 nil 导致 panic
		return fmt.Errorf("resp is nil"), false
	}
	defer resp.Body.Close() // 确保响应体被关闭

	// 解析 resp 到 CozeChatResponse
	var cozeResponse CozeChatResponse
	responseBody, readErr := readCozeResponseBody(c, resp, budget)
	if readErr != nil {
		return readErr, false
	}
	err = common.Unmarshal(responseBody, &cozeResponse)
	if err != nil {
		return fmt.Errorf("unmarshal response body failed: %w", err), false
	}
	if cozeResponse.Data.Status == "completed" {
		// 在上下文设置 usage
		c.Set("coze_token_count", cozeResponse.Data.Usage.TokenCount)
		c.Set("coze_output_count", cozeResponse.Data.Usage.OutputCount)
		c.Set("coze_input_count", cozeResponse.Data.Usage.InputCount)
		return nil, true
	} else if cozeResponse.Data.Status == "failed" || cozeResponse.Data.Status == "canceled" || cozeResponse.Data.Status == "requires_action" {
		return fmt.Errorf("chat status: %s", cozeResponse.Data.Status), false
	} else {
		return nil, false
	}
}

func getChatDetail(a *Adaptor, c *gin.Context, info *relaycommon.RelayInfo, budget *cozeRequestBudget) (*http.Response, error) {
	requestURL := fmt.Sprintf("%s/v3/chat/message/list", info.ChannelBaseUrl)

	requestURL = requestURL + "?conversation_id=" + c.GetString("coze_conversation_id") + "&chat_id=" + c.GetString("coze_chat_id")
	req, err := http.NewRequestWithContext(budget.ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	err = a.SetupRequestHeader(c, &req.Header, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	resp, err := doRequest(c, req, info, budget)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

func doRequest(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo, budget *cozeRequestBudget) (*http.Response, error) {
	var client *http.Client
	var err error // 声明 err 变量
	if info.ChannelSetting.Proxy != "" {
		client, err = service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
		if err != nil {
			return nil, fmt.Errorf("new proxy http client failed: %w", err)
		}
	} else {
		client = service.GetHttpClient()
	}
	info.MarkUpstreamRequestMayHaveBeenAccepted()
	resp, err := client.Do(req)
	if err != nil { // 增加对 client.Do(req) 返回错误的检查
		return nil, classifyCozeStageError(c, budget, err, "response_headers")
	}
	common.SetContextKey(c, constant.ContextKeyRelayResponseHeaders, true)
	// _ = resp.Body.Close()
	return resp, nil
}
