package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/types"
)

// 图片接口只根据明确的上游失败分类，不把通用 400 当成参数或审核结论。
// 传输结果不明、已输出、指定渠道等限制仍由 Relay 的公共检查决定。
func imageFailureRetryable(err *types.NewAPIError) bool {
	if err == nil || !err.HasUpstreamResponse() || types.IsSkipRetryError(err) {
		return false
	}
	detail := err.ToOpenAIError()
	code := strings.ToLower(strings.TrimSpace(string(err.GetErrorCode())))
	switch code {
	case "content_policy_violation", "content_filter", "moderation_blocked", "safety_violation", "image_safety_rejection",
		"invalid_request", "invalid_request_error", "invalid_value", "invalid_parameter", "invalid_image_input",
		"invalid_image", "invalid_image_format", "invalid_image_url", "missing_required_parameter", "unsupported_parameter",
		"request_too_large", "image_too_large", "request_feature_unsupported":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(detail.Type)) {
	case "content_policy_violation", "content_safety_blocked", "moderation_blocked", "safety_violation":
		return false
	}
	if detail.Param != "" {
		return false
	}
	switch err.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// request_failed 是当前图片上游的通用失败码；不匹配其文案里列举的可能原因。
		// 未识别的 400/422 保持停止，避免把真实的调用方错误反复提交。
		switch code {
		case "request_failed", "server_error", "internal_server_error", "upstream_error", "upstream_unavailable",
			"rate_limit_exceeded", "insufficient_quota", "invalid_api_key", "model_not_found", "model_not_supported":
			return true
		default:
			return false
		}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout,
		http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return err.StatusCode >= http.StatusInternalServerError && err.StatusCode <= 599
	}
}

func imageFailureExhaustsChannel(err *types.NewAPIError) bool {
	if err == nil || !err.HasUpstreamResponse() {
		return false
	}
	switch err.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		// 当前请求不再重复使用已拒绝的入口；限流和临时服务错误仍可按现有退避进入下一轮。
		return true
	default:
		return false
	}
}
