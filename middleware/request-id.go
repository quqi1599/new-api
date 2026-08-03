package middleware

import (
	"context"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

var inboundRequestIDHeaders = [...]string{
	common.RequestIdKey,
	"X-Request-Id",
	"X-Trace-Id",
	"X-B3-Traceid",
}

// validInboundRequestID accepts common trace/request identifiers while
// rejecting whitespace and control characters that could inject log lines.
// The separately generated internal ID remains available when a caller reuses
// or intentionally collides a valid external identifier.
func validInboundRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
			continue
		}
		switch ch {
		case '-', '_', '.', ':', '/':
			continue
		default:
			return false
		}
	}
	return true
}

func inboundRequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, header := range inboundRequestIDHeaders {
		id := strings.TrimSpace(c.Request.Header.Get(header))
		if validInboundRequestID(id) {
			return id
		}
	}
	return ""
}

func RequestId() func(c *gin.Context) {
	return func(c *gin.Context) {
		internalID := common.NewRequestId()
		id := inboundRequestID(c)
		if id == "" {
			id = internalID
		}
		c.Set(common.RequestIdKey, id)
		c.Set(common.InternalRequestIdKey, internalID)
		ctx := context.WithValue(c.Request.Context(), common.RequestIdKey, id)
		ctx = context.WithValue(ctx, common.InternalRequestIdKey, internalID)
		c.Request = c.Request.WithContext(ctx)
		c.Header(common.RequestIdKey, id)
		c.Next()
	}
}
