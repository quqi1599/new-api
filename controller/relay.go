package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const statusCodeCloudflareTimeout = 524

var moderationReviewIdPattern = regexp.MustCompile(`(?i)\bmoderation(?:[\s_-]+review)?[\s_-]+id\s*[:=]\s*([a-z0-9][a-z0-9_-]{7,127})\b`)

func requestBodyFailureClassification(err error) (statusCode int, errorCode types.ErrorCode, classified bool) {
	switch {
	case common.IsRequestBodyTooLargeError(err), errors.Is(err, common.ErrRequestBodyTooLarge):
		return http.StatusRequestEntityTooLarge, types.ErrorCodeRequestBodyTooLarge, true
	case common.IsIncompleteBodyError(err):
		return http.StatusBadRequest, types.ErrorCodeRequestBodyIncomplete, true
	case common.IsBodyReadError(err):
		return http.StatusBadRequest, types.ErrorCodeReadRequestBodyFailed, true
	case common.IsBodyAdmissionError(err):
		return http.StatusServiceUnavailable, types.ErrorCodeRequestBodyCapacity, true
	case common.IsInternalBodyStorageError(err):
		return http.StatusInternalServerError, types.ErrorCodeInternalStorageError, true
	default:
		return http.StatusBadRequest, types.ErrorCodeReadRequestBodyFailed, false
	}
}

func requestBodyFailureStatus(err error) (statusCode int, classified bool) {
	statusCode, _, classified = requestBodyFailureClassification(err)
	return statusCode, classified
}

func publicRequestBodyError(c *gin.Context, err error) error {
	if common.IsBodyReadError(err) {
		return errors.New("failed to read request body")
	}
	if common.IsBodyAdmissionError(err) {
		return errors.New("request body capacity is temporarily exhausted")
	}
	if !common.IsInternalBodyStorageError(err) {
		return err
	}
	if c != nil {
		logger.LogError(c, "internal request body storage failure: "+err.Error())
	} else {
		common.SysError("internal request body storage failure: " + err.Error())
	}
	return errors.New("internal request body storage error")
}

func newRequestBodyFailure(c *gin.Context, err error) *types.NewAPIError {
	statusCode, errorCode, _ := requestBodyFailureClassification(err)
	return types.NewErrorWithStatusCode(
		publicRequestBodyError(c, err),
		errorCode,
		statusCode,
		types.ErrOptionWithSkipRetry(),
	)
}

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(newAPIError.Error())))
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			writeRelayError(c, ws, relayFormat, newAPIError)
		}
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		if _, isBodyReadFailure := requestBodyFailureStatus(err); isBodyReadFailure {
			newAPIError = newRequestBodyFailure(c, err)
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:                   c,
		TokenGroup:            relayInfo.TokenGroup,
		ModelName:             relayInfo.OriginModelName,
		Retry:                 common.GetPointer(0),
		PreferredChannelTypes: types.RelayFormatToPreferredChannelTypes(relayInfo.RelayFormat),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil
	retryLimit := common.RetryTimes
	forcedFallbackStarted := false

	for ; retryParam.GetRetry() <= retryLimit; retryParam.IncreaseRetry() {
		wasForcedFallbackAttempt := forcedFallbackStarted
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			newAPIError = channelErr
			break
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			newAPIError = newRequestBodyFailure(c, bodyErr)
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}

		if newAPIError == nil {
			relayInfo.LastError = nil
			logRelayRetryRoute(c)
			return
		}

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		relayInfo.LastError = newAPIError

		sessionBlocked := shouldBanTokenFromProtectedChannels(relayInfo, newAPIError)
		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError, sessionBlocked)

		retryParam.ExcludedChannelIds = append(retryParam.ExcludedChannelIds, channel.Id)

		policyProtectionReady := !sessionBlocked || relayInfo.TokenId > 0
		if sessionBlocked && relayInfo.TokenId > 0 {
			moderationId := extractModerationReviewId(newAPIError)
			added, err := model.BanTokenFromProtectedChannels(relayInfo.TokenId, channel.Id, moderationId)
			if err != nil {
				policyProtectionReady = false
				logger.LogError(c, fmt.Sprintf("failed to persist protected channel ban for token #%d after channel #%d rejection: %v", relayInfo.TokenId, channel.Id, err))
			} else if added {
				logger.LogWarn(c, fmt.Sprintf("globally protected token #%d after upstream session policy rejection on channel #%d", relayInfo.TokenId, channel.Id))
			}

			protectedChannelIds, err := model.GetAPIKeyPolicyProtectedChannelIds()
			if err != nil {
				policyProtectionReady = false
				logger.LogError(c, fmt.Sprintf("failed to load protected channels for token #%d: %v", relayInfo.TokenId, err))
			} else {
				retryParam.ExcludedChannelIds = append(retryParam.ExcludedChannelIds, protectedChannelIds...)
				common.SetContextKey(c, constant.ContextKeyTokenExcludedChannels, protectedChannelIds)
			}
		}

		gptFallback := isGPTChannelFallbackError(relayInfo, newAPIError) || sessionBlocked
		canGPTFallback := gptFallback && policyProtectionReady &&
			canRetryGPTChannelFallback(relayInfo, len(c.GetStringSlice("use_channel")))
		forceGPTFallback := sessionBlocked || gptFallback
		if canGPTFallback {
			service.ClearChannelAffinityForRequest(c)
		}
		if forceGPTFallback && canGPTFallback {
			forcedFallbackStarted = true
			retryLimit = extendRetryLimitForForcedGPTFallback(retryLimit, retryParam.GetRetry())
		}
		retryAllowed := shouldRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry())
		if forceGPTFallback {
			retryAllowed = canGPTFallback
		}
		if relayProgressStarted(c, relayInfo) {
			// A valid upstream event or any downstream byte proves this stream has
			// started. Replaying the POST could duplicate generation, tools, or
			// billing and could splice a second stream into an already committed one.
			retryAllowed = false
		}
		if !canContinueRelayRetry(retryAllowed, gptFallback, canGPTFallback, wasForcedFallbackAttempt) {
			break
		}
	}

	logRelayRetryRoute(c)
}

func writeRelayError(c *gin.Context, ws *websocket.Conn, relayFormat types.RelayFormat, relayErr *types.NewAPIError) {
	if relayErr == nil {
		return
	}
	if relayFormat == types.RelayFormatOpenAIRealtime {
		helper.WssError(c, ws, relayErr.ToOpenAIError())
		return
	}
	if c == nil || c.Writer == nil {
		return
	}
	if c.Writer.Written() {
		// HTTP status and framing are immutable after the first SSE byte. Appending
		// a plain JSON error would corrupt the event stream and confuse clients.
		logger.LogError(c, "relay failed after downstream output; suppressing trailing JSON error")
		return
	}

	// An adapter may have prepared SSE headers before discovering a pre-output
	// protocol error. Restore a normal JSON response before committing it.
	for _, header := range []string{"Content-Type", "Cache-Control", "Connection", "Transfer-Encoding", "X-Accel-Buffering"} {
		c.Writer.Header().Del(header)
	}

	switch relayFormat {
	case types.RelayFormatClaude:
		c.JSON(relayErr.StatusCode, gin.H{
			"type":  "error",
			"error": relayErr.ToClaudeError(),
		})
	default:
		c.JSON(relayErr.StatusCode, gin.H{
			"error": relayErr.ToOpenAIError(),
		})
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func logRelayRetryRoute(c *gin.Context) {
	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) <= 1 {
		return
	}
	retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
	logger.LogInfo(c, retryLogStr)
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
		}, nil
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	if c != nil && c.Writer != nil && c.Writer.Written() {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func relayProgressStarted(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if c != nil && c.Writer != nil && c.Writer.Written() {
		return true
	}
	if info == nil {
		return false
	}
	return info.ReceivedResponseCount > 0 ||
		info.SendResponseCount > 0 ||
		info.HasSendResponse() ||
		info.UpstreamRequestMayHaveBeenAccepted()
}

func isGPTChannelFallbackError(info *relaycommon.RelayInfo, openaiErr *types.NewAPIError) bool {
	if info == nil || openaiErr == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(info.OriginModelName)), "gpt-") {
		return false
	}
	if types.IsSkipRetryError(openaiErr) || operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	code := openaiErr.StatusCode
	if code < 100 || code > 599 {
		return true
	}
	if code == http.StatusGatewayTimeout || code == statusCodeCloudflareTimeout {
		return true
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func isGatewaySessionBlockedError(openaiErr *types.NewAPIError) bool {
	return openaiErr != nil &&
		strings.Contains(strings.ToLower(openaiErr.Error()), "this session has been blocked by the gateway content policy")
}

func extractModerationReviewId(openaiErr *types.NewAPIError) string {
	if openaiErr == nil {
		return ""
	}
	matches := moderationReviewIdPattern.FindStringSubmatch(openaiErr.Error())
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

func shouldBanTokenFromProtectedChannels(info *relaycommon.RelayInfo, openaiErr *types.NewAPIError) bool {
	return info != nil && info.ChannelMeta != nil &&
		info.ChannelOtherSettings.APIKeyPolicyProtectionEnabled &&
		isGatewaySessionBlockedError(openaiErr)
}

func canRetryGPTChannelFallback(info *relaycommon.RelayInfo, attemptedChannels int) bool {
	return info != nil && attemptedChannels < 2 &&
		!info.HasSendResponse() && info.SendResponseCount == 0 && info.ReceivedResponseCount == 0 &&
		!info.UpstreamRequestMayHaveBeenAccepted()
}

func extendRetryLimitForForcedGPTFallback(retryLimit int, currentRetry int) int {
	if currentRetry >= retryLimit {
		return currentRetry + 1
	}
	return retryLimit
}

func canContinueRelayRetry(retryAllowed bool, fallbackError bool, canFallback bool, wasPolicyFallbackAttempt bool) bool {
	return !wasPolicyFallbackAttempt && retryAllowed && (!fallbackError || canFallback)
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, skipAutoDisable bool) {
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.Error())))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if !skipAutoDisable && service.ShouldDisableChannel(err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, err.ErrorWithStatusCode())
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}

}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *dto.TaskError
	defer func() {
		if taskErr != nil && relayInfo.Billing != nil {
			relayInfo.Billing.Refund(c)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			statusCode, errorCode, _ := requestBodyFailureClassification(bodyErr)
			taskErr = service.TaskErrorWrapperLocal(publicRequestBodyError(c, bodyErr), string(errorCode), statusCode)
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)
		if taskErr == nil {
			break
		}
		if relayInfo.UpstreamRequestMayHaveBeenAccepted() {
			// The upstream may already have created the non-idempotent task even
			// when it returned a retryable-looking status. Cross-channel replay
			// could create and bill a second task.
			taskErr.SkipRetry = true
		}

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode),
				false)
		}

		if !shouldRetryTaskRelay(c, relayInfo, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		service.LogTaskConsumption(c, relayInfo)

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			ModelPrice:      relayInfo.PriceData.ModelPrice,
			GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:      relayInfo.PriceData.ModelRatio,
			OtherRatios:     relayInfo.PriceData.OtherRatios(),
			OriginModelName: relayInfo.OriginModelName,
			PerCallBilling:  common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
		}
		task.Quota = result.Quota
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
		}
	}

	if taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *dto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, info *relaycommon.RelayInfo, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if info != nil && info.UpstreamRequestMayHaveBeenAccepted() {
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.SkipRetry {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
