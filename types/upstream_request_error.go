package types

import (
	"errors"
	"net/http"
)

// Only structured identifiers establish these deterministic request failures.
// Do not classify arbitrary upstream message text as a safety rejection.
func normalizeDeterministicUpstreamError(e *NewAPIError) {
	code := string(e.errorCode)
	message := e.Error()
	status := http.StatusBadRequest
	switch code {
	case "1301", "content_policy_violation":
		code = "content_policy_violation"
		message = "上游内容安全策略拦截了本次请求。请调整相关内容后再提交；请勿原样重复尝试。"
	case "request_feature_unsupported":
	case "request_too_large", "image_too_large":
		// CPA may report this inside HTTP-200 SSE after output has begun. The
		// structured request rejection is terminal even when an adapter used 502.
		code = "request_too_large"
		status = http.StatusRequestEntityTooLarge
	case "invalid_image_input":
	default:
		return
	}
	e.errorCode = ErrorCode(code)
	e.StatusCode = status
	e.skipRetry = true
	e.allowChannelPenalty = false
	e.Err = errors.New(message)
	switch detail := e.RelayError.(type) {
	case OpenAIError:
		detail.Code, detail.Type, detail.Message = code, "invalid_request_error", message
		e.RelayError = detail
	case ClaudeError:
		detail.Code, detail.Type, detail.Message = code, "invalid_request_error", message
		e.RelayError = detail
	}
}
