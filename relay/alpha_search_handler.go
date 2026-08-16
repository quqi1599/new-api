package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func AlphaSearchHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.InitChannelMeta(c)
	if info.ChannelType != constant.ChannelTypeCodex && !info.ChannelSetting.AlphaSearchEnabled {
		return types.NewError(errors.New("channel does not support /v1/alpha/search"), types.ErrorCodeInvalidRequest)
	}

	request, ok := info.Request.(*dto.AlphaSearchRequest)
	if !ok {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("invalid request type, expected *dto.AlphaSearchRequest, got %T", info.Request),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	jsonData, err := buildAlphaSearchRequestBody(request.RawBody, info.OriginModelName, info.UpstreamModelName)
	if err != nil {
		return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	logger.LogDebug(c, "alpha search request prepared: bytes=%d model_mapped=%t", len(jsonData), info.IsModelMapped)

	body, size, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	defer closer.Close()
	info.UpstreamRequestBodySize = size

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	response, err := adaptor.DoRequest(c, info, body)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	httpResponse, ok := response.(*http.Response)
	if !ok || httpResponse == nil {
		return types.NewOpenAIError(errors.New("invalid http response"), types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		newAPIError := service.RelayErrorHandler(c.Request.Context(), httpResponse, false)
		service.ResetStatusCode(newAPIError, c.GetString("status_code_mapping"))
		return newAPIError
	}

	if contentType := httpResponse.Header.Get("Content-Type"); contentType != "" {
		c.Header("Content-Type", contentType)
	}
	info.SetFirstResponseTime()
	c.Status(httpResponse.StatusCode)
	if _, err := io.Copy(c.Writer, httpResponse.Body); err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}

	if info.ResponsesUsageInfo == nil {
		info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{}
	}
	if info.ResponsesUsageInfo.BuiltInTools == nil {
		info.ResponsesUsageInfo.BuiltInTools = make(map[string]*relaycommon.BuildInToolInfo)
	}
	tool := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]
	if tool == nil {
		tool = &relaycommon.BuildInToolInfo{ToolName: dto.BuildInToolWebSearchPreview}
		info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview] = tool
	}
	tool.CallCount = 1
	service.PostTextConsumeQuota(c, info, &dto.Usage{}, nil)
	return nil
}

// buildAlphaSearchRequestBody returns the original bytes unless a model map is
// active. When mapped, only the top-level model value is replaced; every other
// field remains an opaque RawMessage.
func buildAlphaSearchRequestBody(rawBody []byte, originModel, upstreamModel string) ([]byte, error) {
	if len(rawBody) == 0 {
		return nil, errors.New("empty alpha search request body")
	}
	if upstreamModel == "" || upstreamModel == originModel {
		return rawBody, nil
	}
	var body map[string]json.RawMessage
	if err := common.Unmarshal(rawBody, &body); err != nil {
		return nil, err
	}
	mappedModel, err := common.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	body["model"] = mappedModel
	return common.Marshal(body)
}
