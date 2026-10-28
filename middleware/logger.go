package middleware

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			accessLogPath(param.Request.RequestURI),
		)
	}))
}

// Sanitize only the log copy. Authentication and OAuth handlers must retain
// the original URL, query and RequestURI, including duplicate parameters.
func accessLogPath(requestURI string) string {
	// Some adapters rewrite URL.Path before the logger runs. RequestURI retains
	// the client-facing path and its original escaping.
	u, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return "[invalid request URI]"
	}
	loggedPath := u.EscapedPath()
	decodedPath := path.Clean(u.Path)
	if decodedPath == "/api/oauth" || strings.HasPrefix(decodedPath, "/api/oauth/") ||
		decodedPath == "/oauth" || strings.HasPrefix(decodedPath, "/oauth/") {
		return loggedPath
	}
	if u.RawQuery == "" {
		return loggedPath
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return loggedPath
	} // Never fall back to an unparsed credential.
	for key := range query {
		if sensitiveAccessLogKey(key) {
			query.Set(key, "***masked***")
		}
	}
	return loggedPath + "?" + query.Encode()
}

func sensitiveAccessLogKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "key", "api_key", "api-key", "apikey", "x-api-key", "x-goog-api-key",
		"authorization", "auth", "password", "passwd", "sig", "awsaccesskeyid", "x-amz-credential":
		return true
	}
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "signature")
}
