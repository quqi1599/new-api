package aws

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrockruntimeTypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/auth/bearer"
)

// getAwsErrorStatusCode extracts HTTP status code from AWS SDK error
func getAwsErrorStatusCode(err error) int {
	// Check for HTTP response error which contains status code
	var httpErr interface{ HTTPStatusCode() int }
	if errors.As(err, &httpErr) {
		return httpErr.HTTPStatusCode()
	}
	// Default to 500 if we can't determine the status code
	return http.StatusInternalServerError
}

func newAwsInvokeContext(c *gin.Context) (context.Context, context.CancelFunc) {
	parent := context.Background()
	if c != nil && c.Request != nil {
		parent = c.Request.Context()
	}
	// RELAY_TIMEOUT is a legacy response-header fallback. Applying it as an SDK
	// context deadline would once again turn it into a whole-response timeout and
	// truncate healthy long-running Bedrock streams.
	return context.WithCancel(parent)
}

type awsInvokeBudgetGuard struct {
	state atomic.Uint32 // 0=armed, 1=disarmed, 2=expired
	timer *time.Timer
}

func awsNonStreamTimeoutSeconds() int {
	return common.RelayNonStreamTimeout
}

func startAwsInvokeBudget(timeout time.Duration, cancel context.CancelFunc) *awsInvokeBudgetGuard {
	if timeout <= 0 || cancel == nil {
		return nil
	}
	guard := &awsInvokeBudgetGuard{}
	guard.timer = time.AfterFunc(timeout, func() {
		if guard.state.CompareAndSwap(0, 2) {
			cancel()
		}
	})
	return guard
}

func startAwsFirstEventBudget(c *gin.Context, info *relaycommon.RelayInfo, cancel context.CancelFunc) (*awsInvokeBudgetGuard, *types.NewAPIError) {
	remaining, limited := info.RemainingFirstValidEventBudget()
	if !limited || cancel == nil {
		return nil, nil
	}
	if remaining <= 0 {
		return nil, awsFirstEventBudgetError(c, false)
	}
	return startAwsInvokeBudget(remaining, cancel), nil
}

func startAwsNonStreamBudget(c *gin.Context, info *relaycommon.RelayInfo, cancel context.CancelFunc) (*awsInvokeBudgetGuard, *types.NewAPIError) {
	timeoutSeconds := awsNonStreamTimeoutSeconds()
	if timeoutSeconds <= 0 {
		return nil, nil
	}
	totalTimeout := time.Duration(timeoutSeconds) * time.Second
	timeout := totalTimeout
	if info != nil {
		info.EnsureNonStreamDeadline(info.StartTime, totalTimeout)
		if remaining, limited := info.RemainingNonStreamBudget(); limited {
			if remaining <= 0 {
				return nil, awsNonStreamBudgetError(c, info.UpstreamRequestMayHaveBeenAccepted())
			}
			timeout = remaining
		}
	}
	return startAwsInvokeBudget(timeout, cancel), nil
}

func (g *awsInvokeBudgetGuard) Stop() bool {
	if g == nil {
		return false
	}
	g.state.CompareAndSwap(0, 1)
	if g.timer != nil {
		g.timer.Stop()
	}
	return g.state.Load() == 2
}

func setAwsTimeoutMetadata(c *gin.Context, origin, phase string, timeoutSeconds int) {
	if c == nil {
		return
	}
	common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, origin)
	common.SetContextKey(c, constant.ContextKeyRelayTimeoutPhase, phase)
	common.SetContextKey(c, constant.ContextKeyRelayTimeoutSeconds, timeoutSeconds)
}

func awsFirstEventBudgetError(c *gin.Context, allowChannelPenalty bool) *types.NewAPIError {
	setAwsTimeoutMetadata(c, constant.RelayCancelOriginGatewayDeadline, "first_valid_event_total", common.RelayFirstEventTotalTimeout)
	options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
	if allowChannelPenalty {
		options = append(options, types.ErrOptionWithChannelPenalty())
	}
	return types.NewErrorWithStatusCode(
		errors.New("request-wide first valid event budget exhausted"),
		types.ErrorCodeUpstreamFirstEventTimeout,
		http.StatusGatewayTimeout,
		options...,
	)
}

func awsNonStreamBudgetError(c *gin.Context, allowChannelPenalty bool) *types.NewAPIError {
	setAwsTimeoutMetadata(c, constant.RelayCancelOriginGatewayDeadline, "non_stream_total", awsNonStreamTimeoutSeconds())
	options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
	if allowChannelPenalty {
		options = append(options, types.ErrOptionWithChannelPenalty())
	}
	return types.NewErrorWithStatusCode(
		errors.New("upstream non-stream response exceeded the total timeout"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		options...,
	)
}

func awsResponseHeaderTimeoutError(c *gin.Context) *types.NewAPIError {
	setAwsTimeoutMetadata(c, constant.RelayCancelOriginUpstreamTimeout, "response_headers", common.RelayResponseHeaderTimeout)
	return types.NewErrorWithStatusCode(
		errors.New("upstream response headers timed out before the first valid event"),
		types.ErrorCodeUpstreamResponseHeaderTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
}

func classifyAwsInvokeError(c *gin.Context, err error, budgetExpired bool, nonStream bool) *types.NewAPIError {
	if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
		return channel.ClassifyDoRequestError(c, err)
	}
	if budgetExpired {
		if nonStream {
			return awsNonStreamBudgetError(c, true)
		}
		return awsFirstEventBudgetError(c, true)
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return awsResponseHeaderTimeoutError(c)
	}
	statusCode := getAwsErrorStatusCode(err)
	return types.NewOpenAIError(errors.Wrap(err, "InvokeModel"), types.ErrorCodeAwsInvokeError, statusCode)
}

func newAwsClient(c *gin.Context, info *relaycommon.RelayInfo) (*bedrockruntime.Client, error) {
	var (
		httpClient *http.Client
		err        error
	)
	if info.ChannelSetting.Proxy != "" {
		httpClient, err = service.NewProxyHttpClient(info.ChannelSetting.Proxy)
		if err != nil {
			return nil, fmt.Errorf("new proxy http client failed: %w", err)
		}
	} else {
		httpClient = service.GetHttpClient()
	}

	awsSecret := strings.Split(info.ApiKey, "|")
	var client *bedrockruntime.Client
	switch len(awsSecret) {
	case 2:
		apiKey := awsSecret[0]
		region := awsSecret[1]
		client = bedrockruntime.New(bedrockruntime.Options{
			Region:                  region,
			BearerAuthTokenProvider: bearer.StaticTokenProvider{Token: bearer.Token{Value: apiKey}},
			HTTPClient:              httpClient,
			RetryMaxAttempts:        1,
		})
	case 3:
		ak := awsSecret[0]
		sk := awsSecret[1]
		region := awsSecret[2]
		client = bedrockruntime.New(bedrockruntime.Options{
			Region:           region,
			Credentials:      aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(ak, sk, "")),
			HTTPClient:       httpClient,
			RetryMaxAttempts: 1,
		})
	default:
		return nil, errors.New("invalid aws secret key")
	}

	return client, nil
}

func doAwsClientRequest(c *gin.Context, info *relaycommon.RelayInfo, a *Adaptor, requestBody io.Reader) (any, error) {
	awsCli, err := newAwsClient(c, info)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeChannelAwsClientError)
	}
	a.AwsClient = awsCli

	// 获取对应的AWS模型ID
	awsModelId := getAwsModelID(info.UpstreamModelName)

	awsRegionPrefix := getAwsRegionPrefix(awsCli.Options().Region)
	canCrossRegion := awsModelCanCrossRegion(awsModelId, awsRegionPrefix)
	if canCrossRegion {
		awsModelId = awsModelCrossRegion(awsModelId, awsRegionPrefix)
	}

	// init empty request.header
	requestHeader := http.Header{}
	a.SetupRequestHeader(c, &requestHeader, info)
	headerOverride, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range headerOverride {
		requestHeader.Set(key, value)
	}

	if isNovaModel(awsModelId) {
		var novaReq *NovaRequest
		err = common.DecodeJson(requestBody, &novaReq)
		if err != nil {
			return nil, types.NewError(errors.Wrap(err, "decode nova request fail"), types.ErrorCodeBadRequestBody)
		}

		// 使用InvokeModel API，但使用Nova格式的请求体
		awsReq := &bedrockruntime.InvokeModelInput{
			ModelId:     aws.String(awsModelId),
			Accept:      aws.String("application/json"),
			ContentType: aws.String("application/json"),
		}

		reqBody, err := common.Marshal(novaReq)
		if err != nil {
			return nil, types.NewError(errors.Wrap(err, "marshal nova request"), types.ErrorCodeBadResponseBody)
		}
		awsReq.Body = reqBody
		a.AwsReq = awsReq
		return nil, nil
	} else {
		awsClaudeReq, err := formatRequest(requestBody, requestHeader)
		if err != nil {
			return nil, types.NewError(errors.Wrap(err, "format aws request fail"), types.ErrorCodeBadRequestBody)
		}

		if info.IsStream {
			awsReq := &bedrockruntime.InvokeModelWithResponseStreamInput{
				ModelId:     aws.String(awsModelId),
				Accept:      aws.String("application/json"),
				ContentType: aws.String("application/json"),
			}
			awsReq.Body, err = buildAwsRequestBody(c, info, awsClaudeReq)
			if err != nil {
				return nil, types.NewError(errors.Wrap(err, "marshal aws request fail"), types.ErrorCodeBadRequestBody)
			}
			a.AwsReq = awsReq
			return nil, nil
		} else {
			awsReq := &bedrockruntime.InvokeModelInput{
				ModelId:     aws.String(awsModelId),
				Accept:      aws.String("application/json"),
				ContentType: aws.String("application/json"),
			}
			awsReq.Body, err = buildAwsRequestBody(c, info, awsClaudeReq)
			if err != nil {
				return nil, types.NewError(errors.Wrap(err, "marshal aws request fail"), types.ErrorCodeBadRequestBody)
			}
			a.AwsReq = awsReq
			return nil, nil
		}
	}
}

// buildAwsRequestBody prepares the payload for AWS requests, applying passthrough rules when enabled.
func buildAwsRequestBody(c *gin.Context, info *relaycommon.RelayInfo, awsClaudeReq any) ([]byte, error) {
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return nil, errors.Wrap(err, "get request body for pass-through fail")
		}
		body, err := storage.Bytes()
		if err != nil {
			return nil, errors.Wrap(err, "get request body bytes fail")
		}
		var data map[string]interface{}
		if err := common.Unmarshal(body, &data); err != nil {
			return nil, errors.Wrap(err, "pass-through unmarshal request body fail")
		}
		delete(data, "model")
		delete(data, "stream")
		return common.Marshal(data)
	}
	return common.Marshal(awsClaudeReq)
}

func getAwsRegionPrefix(awsRegionId string) string {
	parts := strings.Split(awsRegionId, "-")
	regionPrefix := ""
	if len(parts) > 0 {
		regionPrefix = parts[0]
	}
	return regionPrefix
}

func awsModelCanCrossRegion(awsModelId, awsRegionPrefix string) bool {
	regionSet, exists := awsModelCanCrossRegionMap[awsModelId]
	return exists && regionSet[awsRegionPrefix]
}

func awsModelCrossRegion(awsModelId, awsRegionPrefix string) string {
	modelPrefix, find := awsRegionCrossModelPrefixMap[awsRegionPrefix]
	if !find {
		return awsModelId
	}
	return modelPrefix + "." + awsModelId
}

func getAwsModelID(requestModel string) string {
	if awsModelIDName, ok := awsModelIDMap[requestModel]; ok {
		return awsModelIDName
	}
	return requestModel
}

func awsHandler(c *gin.Context, info *relaycommon.RelayInfo, a *Adaptor) (*types.NewAPIError, *dto.Usage) {

	ctx, cancel := newAwsInvokeContext(c)
	defer cancel()
	budgetGuard, budgetErr := startAwsNonStreamBudget(c, info, cancel)
	if budgetErr != nil {
		return budgetErr, nil
	}

	info.MarkUpstreamRequestMayHaveBeenAccepted()
	awsResp, err := a.AwsClient.InvokeModel(ctx, a.AwsReq.(*bedrockruntime.InvokeModelInput))
	budgetExpired := budgetGuard.Stop()
	if err != nil {
		return classifyAwsInvokeError(c, err, budgetExpired, true), nil
	}
	if budgetExpired {
		return awsNonStreamBudgetError(c, true), nil
	}

	claudeInfo := &claude.ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}

	// 复制上游 Content-Type 到客户端响应头
	if awsResp.ContentType != nil && *awsResp.ContentType != "" {
		c.Writer.Header().Set("Content-Type", *awsResp.ContentType)
	}

	handlerErr := claude.HandleClaudeResponseData(c, info, claudeInfo, nil, awsResp.Body)
	if handlerErr != nil {
		return handlerErr, nil
	}
	return nil, claudeInfo.Usage
}

func awsStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, a *Adaptor) (*types.NewAPIError, *dto.Usage) {
	previousStreamStatus := info.StreamStatus
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.CopyErrorsFrom(previousStreamStatus)

	ctx, cancel := newAwsInvokeContext(c)
	defer cancel()
	budgetGuard, budgetErr := startAwsFirstEventBudget(c, info, cancel)
	if budgetErr != nil {
		return budgetErr, nil
	}

	info.MarkUpstreamRequestMayHaveBeenAccepted()
	awsResp, err := a.AwsClient.InvokeModelWithResponseStream(ctx, a.AwsReq.(*bedrockruntime.InvokeModelWithResponseStreamInput))
	budgetExpired := budgetGuard.Stop()
	if err != nil {
		return classifyAwsInvokeError(c, err, budgetExpired, false), nil
	}
	stream := awsResp.GetStream()
	if budgetExpired {
		_ = stream.Close()
		return awsFirstEventBudgetError(c, true), nil
	}

	claudeInfo := &claude.ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}

	return consumeAwsResponseStream(c, info, ctx, stream, claudeInfo)
}

type awsResponseStream interface {
	Events() <-chan bedrockruntimeTypes.ResponseStream
	Err() error
	Close() error
}

func consumeAwsResponseStream(c *gin.Context, info *relaycommon.RelayInfo, ctx context.Context, stream awsResponseStream, claudeInfo *claude.ClaudeResponseInfo) (*types.NewAPIError, *dto.Usage) {
	defer stream.Close()
	events := stream.Events()
	streamTimer := time.NewTimer(info.BoundFirstValidEventWait(helper.StreamFirstEventTimeout()))
	defer streamTimer.Stop()

	firstEventSeen := false
	resetTimer := func(timeout time.Duration) {
		if !streamTimer.Stop() {
			select {
			case <-streamTimer.C:
			default:
			}
		}
		streamTimer.Reset(timeout)
	}

	processEvent := func(event bedrockruntimeTypes.ResponseStream) (bool, *types.NewAPIError) {
		switch v := event.(type) {
		case *bedrockruntimeTypes.ResponseStreamMemberChunk:
			eventType, respErr := claude.HandleStreamResponseData(c, info, claudeInfo, string(v.Value.Bytes), nil)
			if respErr != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonHandlerStop, respErr)
				if preOutputErr := helper.PreOutputStreamError(c, info); preOutputErr != nil {
					return true, preOutputErr
				}
				// Output may already have committed an SSE 200. Still return a
				// non-nil result so the controller suppresses trailing JSON while
				// avoiding success settlement/logging.
				return true, respErr
			}
			if eventType == "" || eventType == "ping" {
				return false, nil
			}
			if !firstEventSeen {
				firstEventSeen = true
				info.SetFirstResponseTime()
			}
			info.ReceivedResponseCount++
			resetTimer(helper.StreamIdleTimeout())
			if eventType == "message_stop" {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				return true, nil
			}
			return false, nil
		case *bedrockruntimeTypes.UnknownUnionMember:
			streamErr := fmt.Errorf("unknown AWS response stream tag: %s", v.Tag)
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonHandlerStop, streamErr)
			return true, helper.PreOutputStreamError(c, info)
		default:
			streamErr := errors.New("nil or unknown AWS response stream event")
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonHandlerStop, streamErr)
			return true, helper.PreOutputStreamError(c, info)
		}
	}

streamLoop:
	for {
		select {
		case <-ctx.Done():
			break streamLoop
		case event, ok := <-events:
			if !ok {
				break streamLoop
			}
			done, eventErr := processEvent(event)
			if eventErr != nil {
				return eventErr, nil
			}
			if done {
				break streamLoop
			}
		case <-streamTimer.C:
			// Prefer an event that became ready at the timer boundary.
			select {
			case event, ok := <-events:
				if !ok {
					break streamLoop
				}
				done, eventErr := processEvent(event)
				if eventErr != nil {
					return eventErr, nil
				}
				if done {
					break streamLoop
				}
				continue
			default:
			}
			if firstEventSeen {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
			} else {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonFirstEventTimeout, nil)
			}
			streamErr := helper.PreOutputStreamError(c, info)
			if streamErr == nil {
				claude.FinalizeClaudeUsage(c, info, claudeInfo)
			}
			return streamErr, claudeInfo.Usage
		}
	}

	if requestErr := c.Request.Context().Err(); requestErr != nil {
		if errors.Is(requestErr, context.DeadlineExceeded) {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonRequestDeadline, requestErr)
		} else {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, requestErr)
		}
		// Preserve partial usage for settlement without manufacturing a final chunk.
		claude.FinalizeClaudeUsage(c, info, claudeInfo)
		if preOutputErr := helper.PreOutputStreamError(c, info); preOutputErr != nil {
			return preOutputErr, claudeInfo.Usage
		}
		return nil, claudeInfo.Usage
	}
	if streamErr := stream.Err(); streamErr != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, streamErr)
		apiErr := helper.PreOutputStreamError(c, info)
		if apiErr == nil {
			claude.FinalizeClaudeUsage(c, info, claudeInfo)
		}
		return apiErr, claudeInfo.Usage
	}
	if info.StreamStatus.EndReason != relaycommon.StreamEndReasonDone {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	}
	if streamErr := helper.PreOutputStreamError(c, info); streamErr != nil {
		return streamErr, claudeInfo.Usage
	}
	if helper.ShouldFinalizeStream(info) {
		claude.HandleStreamFinalResponse(c, info, claudeInfo)
	} else {
		claude.FinalizeClaudeUsage(c, info, claudeInfo)
	}
	return nil, claudeInfo.Usage
}

// Nova模型处理函数
func handleNovaRequest(c *gin.Context, info *relaycommon.RelayInfo, a *Adaptor) (*types.NewAPIError, *dto.Usage) {

	ctx, cancel := newAwsInvokeContext(c)
	defer cancel()
	budgetGuard, budgetErr := startAwsNonStreamBudget(c, info, cancel)
	if budgetErr != nil {
		return budgetErr, nil
	}

	info.MarkUpstreamRequestMayHaveBeenAccepted()
	awsResp, err := a.AwsClient.InvokeModel(ctx, a.AwsReq.(*bedrockruntime.InvokeModelInput))
	budgetExpired := budgetGuard.Stop()
	if err != nil {
		return classifyAwsInvokeError(c, err, budgetExpired, true), nil
	}
	if budgetExpired {
		return awsNonStreamBudgetError(c, true), nil
	}

	// 解析Nova响应
	var novaResp struct {
		Output struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
			TotalTokens  int `json:"totalTokens"`
		} `json:"usage"`
	}

	if err := common.Unmarshal(awsResp.Body, &novaResp); err != nil {
		return types.NewError(errors.Wrap(err, "unmarshal nova response"), types.ErrorCodeBadResponseBody), nil
	}

	// 构造OpenAI格式响应
	response := dto.OpenAITextResponse{
		Id:      helper.GetResponseID(c),
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Model:   info.UpstreamModelName,
		Choices: []dto.OpenAITextResponseChoice{{
			Index: 0,
			Message: dto.Message{
				Role:    "assistant",
				Content: novaResp.Output.Message.Content[0].Text,
			},
			FinishReason: "stop",
		}},
		Usage: dto.Usage{
			PromptTokens:     novaResp.Usage.InputTokens,
			CompletionTokens: novaResp.Usage.OutputTokens,
			TotalTokens:      novaResp.Usage.TotalTokens,
		},
	}

	c.JSON(http.StatusOK, response)
	return nil, &response.Usage
}
