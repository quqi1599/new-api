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
	switch code {
	case "1301", "content_policy_violation":
		code = "content_policy_violation"
		message = "上游内容安全策略拦截了本次请求。请调整相关内容后再提交；请勿原样重复尝试。"
	case "request_feature_unsupported":
	default:
		return
	}
	e.errorCode = ErrorCode(code)
	e.StatusCode = http.StatusBadRequest
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
