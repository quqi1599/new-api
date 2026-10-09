package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func websocketAPIKey(headers http.Header) string {
	const prefix = "openai-insecure-api-key."
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, prefix) {
				if key := strings.TrimPrefix(protocol, prefix); key != "" {
					return key
				}
			}
		}
	}
	return ""
}

func tokenBearerValue(value string) string {
	parts := strings.Fields(value)
	if len(parts) > 0 && strings.EqualFold(parts[0], "Bearer") {
		if len(parts) == 1 {
			return ""
		}
		if len(parts) == 2 {
			return parts[1]
		}
	}
	// Retain existing support for raw tokens and provider-specific secrets.
	return value
}

// Keep public auth errors generic while retaining a fixed internal reason.
// Neither credentials, their hashes, token names nor request bodies are logged.
func recordTokenAuthRejection(c *gin.Context, key string, token *model.Token) {
	reason := tokenAuthRejectionReason(key, token, common.GetTimestamp())
	c.Set("token_auth_failure_reason", reason)
	logger.LogError(c.Request.Context(), fmt.Sprintf(
		"token_auth_rejected reason=%s authorization_present=%t websocket_key_present=%t",
		reason, c.Request.Header.Get("Authorization") != "", websocketAPIKey(c.Request.Header) != ""))
}

func tokenAuthRejectionReason(key string, token *model.Token, now int64) string {
	if key == "" {
		return "missing_api_key"
	}
	if token == nil {
		return "invalid_api_key"
	}
	switch token.Status {
	case common.TokenStatusDisabled:
		return "token_disabled"
	case common.TokenStatusExpired:
		return "token_expired"
	case common.TokenStatusExhausted:
		return "token_exhausted"
	case common.TokenStatusEnabled:
		if token.ExpiredTime != -1 && token.ExpiredTime < now {
			return "token_expired"
		}
		if !token.UnlimitedQuota && token.RemainQuota <= 0 {
			return "token_exhausted"
		}
	}
	return "invalid_api_key"
}
