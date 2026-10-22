package service

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// ParseGatewayReplayBarrier parses response metadata only. The transport caller
// must establish the selected gateway's trust before accepting this contract.
// A 2xx response may still fail before the first downstream SSE event.
func ParseGatewayReplayBarrier(header http.Header, status int) string {
	if !(status >= 200 && status < 300 || status >= 400 && status <= 599) {
		return ""
	}
	scope, reasons, attempts := header.Values("X-CPA-Retry-Scope"), header.Values("X-CPA-Retry-Reason"), header.Values("X-CPA-Retry-Attempts")
	if len(scope) != 1 || strings.TrimSpace(scope[0]) != "request" || len(reasons) != 1 || len(attempts) != 1 {
		return ""
	}
	countText := strings.TrimSpace(attempts[0])
	count, err := strconv.Atoi(countText)
	if err != nil || count < 1 || count > types.MaxGatewayRetryAttempts || strconv.Itoa(count) != countText {
		return ""
	}
	reason := strings.TrimSpace(reasons[0])
	switch reason {
	case RetryStopReasonUpstreamOutputObserved, RetryStopReasonAcceptedOutcomeUnknown:
		return reason
	default:
		return ""
	}
}

// GatewayReplayStopReason reads only transport-owned attempt metadata, never
// client headers or JSON. It remains authoritative after status-code mapping.
func GatewayReplayStopReason(c *gin.Context) string {
	if c == nil {
		return ""
	}
	reason := common.GetContextKeyString(c, constant.ContextKeyRelayGatewayNoReplayReason)
	switch reason {
	case RetryStopReasonUpstreamOutputObserved, RetryStopReasonAcceptedOutcomeUnknown:
		return reason
	default:
		return ""
	}
}
