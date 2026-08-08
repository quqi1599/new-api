package xunfei

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// https://console.xfyun.cn/services/cbm
// https://www.xfyun.cn/doc/spark/Web.html

func requestOpenAI2Xunfei(request dto.GeneralOpenAIRequest, xunfeiAppId string, domain string) *XunfeiChatRequest {
	messages := make([]XunfeiMessage, 0, len(request.Messages))
	shouldCovertSystemMessage := !strings.HasSuffix(request.Model, "3.5")
	for _, message := range request.Messages {
		if message.Role == "system" && shouldCovertSystemMessage {
			messages = append(messages, XunfeiMessage{
				Role:    "user",
				Content: message.StringContent(),
			})
			messages = append(messages, XunfeiMessage{
				Role:    "assistant",
				Content: "Okay",
			})
		} else {
			messages = append(messages, XunfeiMessage{
				Role:    message.Role,
				Content: message.StringContent(),
			})
		}
	}
	xunfeiRequest := XunfeiChatRequest{}
	xunfeiRequest.Header.AppId = xunfeiAppId
	xunfeiRequest.Parameter.Chat.Domain = domain
	xunfeiRequest.Parameter.Chat.Temperature = request.Temperature
	xunfeiRequest.Parameter.Chat.TopK = lo.FromPtrOr(request.N, 0)
	xunfeiRequest.Parameter.Chat.MaxTokens = request.GetMaxTokens()
	xunfeiRequest.Payload.Message.Text = messages
	return &xunfeiRequest
}

func responseXunfei2OpenAI(response *XunfeiChatResponse) *dto.OpenAITextResponse {
	if len(response.Payload.Choices.Text) == 0 {
		response.Payload.Choices.Text = []XunfeiChatResponseTextItem{
			{
				Content: "",
			},
		}
	}
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: response.Payload.Choices.Text[0].Content,
		},
		FinishReason: constant.FinishReasonStop,
	}
	fullTextResponse := dto.OpenAITextResponse{
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Choices: []dto.OpenAITextResponseChoice{choice},
		Usage:   response.Payload.Usage.Text,
	}
	return &fullTextResponse
}

func streamResponseXunfei2OpenAI(xunfeiResponse *XunfeiChatResponse) *dto.ChatCompletionsStreamResponse {
	if len(xunfeiResponse.Payload.Choices.Text) == 0 {
		xunfeiResponse.Payload.Choices.Text = []XunfeiChatResponseTextItem{
			{
				Content: "",
			},
		}
	}
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString(xunfeiResponse.Payload.Choices.Text[0].Content)
	if xunfeiResponse.Payload.Choices.Status == 2 {
		choice.FinishReason = &constant.FinishReasonStop
	}
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "SparkDesk",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func buildXunfeiAuthUrl(hostUrl string, apiKey, apiSecret string) string {
	HmacWithShaToBase64 := func(algorithm, data, key string) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(data))
		encodeData := mac.Sum(nil)
		return base64.StdEncoding.EncodeToString(encodeData)
	}
	ul, err := url.Parse(hostUrl)
	if err != nil {
		fmt.Println(err)
	}
	date := time.Now().UTC().Format(time.RFC1123)
	signString := []string{"host: " + ul.Host, "date: " + date, "GET " + ul.Path + " HTTP/1.1"}
	sign := strings.Join(signString, "\n")
	sha := HmacWithShaToBase64("hmac-sha256", sign, apiSecret)
	authUrl := fmt.Sprintf("hmac username=\"%s\", algorithm=\"%s\", headers=\"%s\", signature=\"%s\"", apiKey,
		"hmac-sha256", "host date request-line", sha)
	authorization := base64.StdEncoding.EncodeToString([]byte(authUrl))
	v := url.Values{}
	v.Add("host", ul.Host)
	v.Add("date", date)
	v.Add("authorization", authorization)
	callUrl := hostUrl + "?" + v.Encode()
	return callUrl
}

type xunfeiWebSocket interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteJSON(v any) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	Close() error
}

type xunfeiDialFunc func(requestCtx context.Context, handshakeCtx context.Context, authURL string) (xunfeiWebSocket, *http.Response, error)

func dialXunfeiWebSocket(requestCtx context.Context, handshakeCtx context.Context, authURL string) (xunfeiWebSocket, *http.Response, error) {
	return channel.DialWebSocketContextWithHandshakeContext(requestCtx, handshakeCtx, authURL, nil)
}

type xunfeiHandshakeError struct {
	err error
}

func (e *xunfeiHandshakeError) Error() string { return e.err.Error() }
func (e *xunfeiHandshakeError) Unwrap() error { return e.err }

type xunfeiResponseEnvelope struct {
	Header *struct {
		Code *int `json:"code"`
	} `json:"header"`
	Payload *struct {
		Choices *struct {
			Status *int `json:"status"`
		} `json:"choices"`
	} `json:"payload"`
}

type xunfeiProtocolError struct {
	err error
}

func (e *xunfeiProtocolError) Error() string { return e.err.Error() }
func (e *xunfeiProtocolError) Unwrap() error { return e.err }

func newXunfeiProtocolError(format string, args ...any) error {
	return &xunfeiProtocolError{err: fmt.Errorf(format, args...)}
}

func xunfeiOpenSession(requestCtx context.Context, handshakeCtx context.Context, info *relaycommon.RelayInfo, textRequest dto.GeneralOpenAIRequest, domain, authURL, appID string, dial xunfeiDialFunc) (xunfeiWebSocket, func(), error) {
	conn, resp, err := dial(requestCtx, handshakeCtx, authURL)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, &xunfeiHandshakeError{err: err}
	}
	if conn == nil || resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
		return nil, nil, fmt.Errorf("Xunfei websocket handshake failed")
	}
	if err := requestCtx.Err(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := handshakeCtx.Err(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if info.IsStream {
		if remaining, limited := info.RemainingFirstValidEventBudget(); limited && remaining <= 5*time.Millisecond {
			_ = conn.Close()
			return nil, nil, context.DeadlineExceeded
		}
	}
	stopCancellationClose := context.AfterFunc(requestCtx, func() {
		_ = conn.Close()
	})
	cleanup := func() {
		stopCancellationClose()
		_ = conn.Close()
	}

	writeWait := info.BoundFirstValidEventWait(helper.StreamFirstEventTimeout())
	if writeWait <= 0 {
		writeWait = time.Nanosecond
	}
	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		cleanup()
		return nil, nil, err
	}

	// This JSON frame starts a non-idempotent generation. Mark the replay gate
	// before attempting the write because a connection failure cannot prove the
	// provider did not receive a complete frame.
	info.MarkUpstreamRequestMayHaveBeenAccepted()
	if err := conn.WriteJSON(requestOpenAI2Xunfei(textRequest, appID, domain)); err != nil {
		cleanup()
		return nil, nil, err
	}
	return conn, cleanup, nil
}

func classifyXunfeiOpenSessionError(c *gin.Context, err error) *types.NewAPIError {
	if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
		return channel.ClassifyDoRequestError(c, err)
	}
	var handshakeErr *xunfeiHandshakeError
	if errors.As(err, &handshakeErr) {
		if classifiedErr := channel.ClassifyWebSocketHandshakeError(c, handshakeErr.err); classifiedErr != nil {
			return classifiedErr
		}
	}
	return types.NewError(err, types.ErrorCodeDoRequestFailed)
}

func xunfeiReadResponse(conn xunfeiWebSocket, wait time.Duration) (XunfeiChatResponse, error) {
	if wait <= 0 {
		wait = time.Nanosecond
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return XunfeiChatResponse{}, err
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return XunfeiChatResponse{}, err
	}

	var envelope xunfeiResponseEnvelope
	if err := common.Unmarshal(msg, &envelope); err != nil {
		return XunfeiChatResponse{}, newXunfeiProtocolError("invalid Xunfei websocket JSON: %w", err)
	}
	if envelope.Header == nil || envelope.Header.Code == nil {
		return XunfeiChatResponse{}, newXunfeiProtocolError("invalid Xunfei response header")
	}
	if *envelope.Header.Code != 0 {
		return XunfeiChatResponse{}, newXunfeiProtocolError("Xunfei upstream returned error code %d", *envelope.Header.Code)
	}
	if envelope.Payload == nil || envelope.Payload.Choices == nil || envelope.Payload.Choices.Status == nil {
		return XunfeiChatResponse{}, newXunfeiProtocolError("invalid Xunfei response choices")
	}
	if status := *envelope.Payload.Choices.Status; status < 0 || status > 2 {
		return XunfeiChatResponse{}, newXunfeiProtocolError("invalid Xunfei response status %d", status)
	}

	var response XunfeiChatResponse
	if err := common.Unmarshal(msg, &response); err != nil {
		return XunfeiChatResponse{}, newXunfeiProtocolError("invalid Xunfei response: %w", err)
	}
	return response, nil
}

func xunfeiSetRequestContextEnd(c *gin.Context, info *relaycommon.RelayInfo, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginGatewayDeadline)
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonRequestDeadline, err)
		return
	}
	common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginDownstreamDisconnected)
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
}

func xunfeiSetReadEnd(c *gin.Context, info *relaycommon.RelayInfo, firstEvent bool, err error) {
	if ctxErr := c.Request.Context().Err(); ctxErr != nil {
		xunfeiSetRequestContextEnd(c, info, ctxErr)
		return
	}
	var protocolErr *xunfeiProtocolError
	if errors.As(err, &protocolErr) {
		xunfeiSetHandlerEnd(info, err)
		return
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if firstEvent {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonFirstEventTimeout, err)
		} else {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, err)
		}
		return
	}
	if errors.Is(err, io.EOF) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, err)
		return
	}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
}

func xunfeiSetHandlerEnd(info *relaycommon.RelayInfo, err error) {
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonHandlerStop, err)
	if err != nil {
		info.StreamStatus.RecordError(err.Error())
	}
}

func xunfeiInitStreamStatus(info *relaycommon.RelayInfo) {
	previous := info.StreamStatus
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.CopyErrorsFrom(previous)
}

func xunfeiAcceptResponse(info *relaycommon.RelayInfo, firstEvent *bool) {
	if *firstEvent {
		*firstEvent = false
		info.SetFirstResponseTime()
	}
	info.ReceivedResponseCount++
}

func xunfeiWriteStreamObject(c *gin.Context, info *relaycommon.RelayInfo, object any) error {
	helper.ExtendWriteDeadline(c)
	if err := helper.ObjectData(c, object); err != nil {
		if ctxErr := c.Request.Context().Err(); ctxErr != nil {
			xunfeiSetRequestContextEnd(c, info, ctxErr)
		} else {
			xunfeiSetHandlerEnd(info, err)
		}
		return err
	}
	return nil
}

func xunfeiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, textRequest dto.GeneralOpenAIRequest, appID string, apiSecret string, apiKey string) (*dto.Usage, *types.NewAPIError) {
	return xunfeiStreamHandlerWithDial(c, info, textRequest, appID, apiSecret, apiKey, dialXunfeiWebSocket)
}

func xunfeiStreamHandlerWithDial(c *gin.Context, info *relaycommon.RelayInfo, textRequest dto.GeneralOpenAIRequest, appID string, apiSecret string, apiKey string, dial xunfeiDialFunc) (*dto.Usage, *types.NewAPIError) {
	xunfeiInitStreamStatus(info)
	info.IsStream = true
	requestCtx := c.Request.Context()
	handshakeCtx := requestCtx
	cancelHandshake := func() {}
	if common.RelayFirstEventTotalTimeout > 0 {
		info.EnsureFirstValidEventDeadline(info.StartTime, time.Duration(common.RelayFirstEventTotalTimeout)*time.Second)
		remaining, _ := info.RemainingFirstValidEventBudget()
		if remaining <= 0 {
			return nil, channel.NewFirstValidEventTotalTimeoutError(c, false)
		}
		handshakeCtx, cancelHandshake = context.WithTimeout(requestCtx, remaining)
	}
	domain, authURL := getXunfeiAuthUrl(c, apiKey, apiSecret, textRequest.Model)
	conn, cleanup, err := xunfeiOpenSession(requestCtx, handshakeCtx, info, textRequest, domain, authURL, appID, dial)
	firstEventBudgetExpired := false
	if requestCtx.Err() == nil {
		if errors.Is(handshakeCtx.Err(), context.DeadlineExceeded) {
			firstEventBudgetExpired = true
		} else if remaining, limited := info.RemainingFirstValidEventBudget(); limited && remaining <= 5*time.Millisecond {
			firstEventBudgetExpired = true
		}
	}
	cancelHandshake()
	if firstEventBudgetExpired {
		if cleanup != nil {
			cleanup()
		}
		return nil, channel.NewFirstValidEventTotalTimeoutError(c, info.UpstreamRequestMayHaveBeenAccepted())
	}
	if err != nil {
		return nil, classifyXunfeiOpenSessionError(c, err)
	}
	defer cleanup()

	usage := &dto.Usage{}
	firstEvent := true
	for {
		wait := helper.StreamIdleTimeout()
		if firstEvent {
			wait = info.BoundFirstValidEventWait(helper.StreamFirstEventTimeout())
		}
		xunfeiResponse, readErr := xunfeiReadResponse(conn, wait)
		if readErr != nil {
			xunfeiSetReadEnd(c, info, firstEvent, readErr)
			if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
				return nil, streamErr
			}
			return usage, nil
		}

		xunfeiAcceptResponse(info, &firstEvent)
		usage.PromptTokens += xunfeiResponse.Payload.Usage.Text.PromptTokens
		usage.CompletionTokens += xunfeiResponse.Payload.Usage.Text.CompletionTokens
		usage.TotalTokens += xunfeiResponse.Payload.Usage.Text.TotalTokens
		if err := xunfeiWriteStreamObject(c, info, streamResponseXunfei2OpenAI(&xunfeiResponse)); err != nil {
			return usage, nil
		}

		if xunfeiResponse.Payload.Choices.Status == 2 {
			helper.ExtendWriteDeadline(c)
			if err := helper.StringData(c, "[DONE]"); err != nil {
				if ctxErr := c.Request.Context().Err(); ctxErr != nil {
					xunfeiSetRequestContextEnd(c, info, ctxErr)
				} else {
					xunfeiSetHandlerEnd(info, err)
				}
				return usage, nil
			}
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			return usage, nil
		}
	}
}

func xunfeiHandler(c *gin.Context, info *relaycommon.RelayInfo, textRequest dto.GeneralOpenAIRequest, appID string, apiSecret string, apiKey string) (*dto.Usage, *types.NewAPIError) {
	return xunfeiHandlerWithDial(c, info, textRequest, appID, apiSecret, apiKey, dialXunfeiWebSocket)
}

func xunfeiHandlerWithDial(c *gin.Context, info *relaycommon.RelayInfo, textRequest dto.GeneralOpenAIRequest, appID string, apiSecret string, apiKey string, dial xunfeiDialFunc) (*dto.Usage, *types.NewAPIError) {
	xunfeiInitStreamStatus(info)
	requestCtx := c.Request.Context()
	budgetCtx := requestCtx
	cancelBudget := func() {}
	if common.RelayNonStreamTimeout > 0 {
		info.EnsureNonStreamDeadline(time.Time{}, time.Duration(common.RelayNonStreamTimeout)*time.Second)
		remaining, _ := info.RemainingNonStreamBudget()
		if remaining <= 0 {
			return nil, channel.NewNonStreamTotalTimeoutError(c, common.RelayNonStreamTimeout, false)
		}
		budgetCtx, cancelBudget = context.WithTimeout(requestCtx, remaining)
	}
	defer cancelBudget()
	classifyBudgetError := func(err error) *types.NewAPIError {
		if errors.Is(budgetCtx.Err(), context.DeadlineExceeded) && requestCtx.Err() == nil {
			return channel.NewNonStreamTotalTimeoutError(c, common.RelayNonStreamTimeout, info.UpstreamRequestMayHaveBeenAccepted())
		}
		if requestCtx.Err() != nil {
			return channel.ClassifyDoRequestError(c, err)
		}
		return nil
	}
	domain, authURL := getXunfeiAuthUrl(c, apiKey, apiSecret, textRequest.Model)
	conn, cleanup, err := xunfeiOpenSession(budgetCtx, budgetCtx, info, textRequest, domain, authURL, appID, dial)
	if err != nil {
		if budgetErr := classifyBudgetError(err); budgetErr != nil {
			return nil, budgetErr
		}
		return nil, classifyXunfeiOpenSessionError(c, err)
	}
	defer cleanup()

	usage := &dto.Usage{}
	var content strings.Builder
	var terminalResponse XunfeiChatResponse
	firstEvent := true
	for {
		wait := helper.StreamIdleTimeout()
		if firstEvent {
			wait = info.BoundFirstValidEventWait(helper.StreamFirstEventTimeout())
		}
		if common.RelayNonStreamTimeout > 0 {
			remaining, _ := info.RemainingNonStreamBudget()
			if remaining <= 0 {
				return nil, channel.NewNonStreamTotalTimeoutError(c, common.RelayNonStreamTimeout, true)
			}
			if remaining < wait {
				wait = remaining
			}
		}
		xunfeiResponse, readErr := xunfeiReadResponse(conn, wait)
		if readErr != nil {
			if budgetErr := classifyBudgetError(readErr); budgetErr != nil {
				return nil, budgetErr
			}
			xunfeiSetReadEnd(c, info, firstEvent, readErr)
			if apiErr := helper.PreOutputStreamError(c, info); apiErr != nil {
				return nil, apiErr
			}
			return nil, types.NewError(readErr, types.ErrorCodeBadResponseBody)
		}

		xunfeiAcceptResponse(info, &firstEvent)
		if len(xunfeiResponse.Payload.Choices.Text) != 0 {
			content.WriteString(xunfeiResponse.Payload.Choices.Text[0].Content)
		}
		usage.PromptTokens += xunfeiResponse.Payload.Usage.Text.PromptTokens
		usage.CompletionTokens += xunfeiResponse.Payload.Usage.Text.CompletionTokens
		usage.TotalTokens += xunfeiResponse.Payload.Usage.Text.TotalTokens
		if xunfeiResponse.Payload.Choices.Status == 2 {
			terminalResponse = xunfeiResponse
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			break
		}
	}

	if len(terminalResponse.Payload.Choices.Text) == 0 {
		terminalResponse.Payload.Choices.Text = []XunfeiChatResponseTextItem{{Content: ""}}
	}
	terminalResponse.Payload.Choices.Text[0].Content = content.String()
	terminalResponse.Payload.Usage.Text = *usage
	response := responseXunfei2OpenAI(&terminalResponse)
	jsonResponse, err := common.Marshal(response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	if _, err := c.Writer.Write(jsonResponse); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	return usage, nil
}

func apiVersion2domain(apiVersion string) string {
	switch apiVersion {
	case "v1.1":
		return "lite"
	case "v2.1":
		return "generalv2"
	case "v3.1":
		return "generalv3"
	case "v3.5":
		return "generalv3.5"
	case "v4.0":
		return "4.0Ultra"
	}
	return "general" + apiVersion
}

func getXunfeiAuthUrl(c *gin.Context, apiKey string, apiSecret string, modelName string) (string, string) {
	apiVersion := getAPIVersion(c, modelName)
	domain := apiVersion2domain(apiVersion)
	authUrl := buildXunfeiAuthUrl(fmt.Sprintf("wss://spark-api.xf-yun.com/%s/chat", apiVersion), apiKey, apiSecret)
	return domain, authUrl
}

func getAPIVersion(c *gin.Context, modelName string) string {
	query := c.Request.URL.Query()
	apiVersion := query.Get("api-version")
	if apiVersion != "" {
		return apiVersion
	}
	parts := strings.Split(modelName, "-")
	if len(parts) == 2 {
		apiVersion = parts[1]
		return apiVersion

	}
	apiVersion = c.GetString("api_version")
	if apiVersion != "" {
		return apiVersion
	}
	apiVersion = "v1.1"
	common.SysLog("api_version not found, using default: " + apiVersion)
	return apiVersion
}
