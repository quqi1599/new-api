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
	case "output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool":
		// CPA's exact 502 output-validation contract describes a result that
		// was already generated. A different channel would generate again, and
		// request-scoped validation is not evidence of whole-channel failure.
		// Preserve the upstream status, machine code and original explanation.
		if e.StatusCode == http.StatusBadGateway {
			e.skipRetry = true
			e.allowChannelPenalty = false
		}
		return
	case "state_owner_unavailable", "state_owner_conflict", "state_owner_expired", "local_state_unavailable":
		// CPA continuation provenance cannot be repaired by replaying the same
		// history or sending it to an unrelated entrance. Preserve the upstream
		// status, code and recovery guidance; this is not channel-health evidence.
		// Match exact machine codes, never all HTTP 409s or message substrings.
		e.skipRetry = true
		e.allowChannelPenalty = false
		return
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
