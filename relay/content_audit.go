package relay

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

const contentAuditDefaultMaxBytes = 1 << 20

func contentAuditEnabled(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	return contentAuditListHas("CONTENT_AUDIT_TOKEN_IDS", strconv.Itoa(info.TokenId)) &&
		contentAuditListHas("CONTENT_AUDIT_MODELS", info.OriginModelName)
}

func contentAuditListHas(envName, value string) bool {
	if value == "" {
		return false
	}
	for _, item := range strings.Split(os.Getenv(envName), ",") {
		if strings.TrimSpace(item) == value {
			return true
		}
	}
	return false
}

func contentAuditBytes(raw []byte) (string, bool) {
	maxBytes := common.GetEnvOrDefault("CONTENT_AUDIT_MAX_BODY_BYTES", contentAuditDefaultMaxBytes)
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return string(raw), false
	}
	return string(raw[:maxBytes]), true
}

func contentAuditWrite(c *gin.Context, info *relaycommon.RelayInfo, stage string, statusCode int, raw []byte) {
	if !contentAuditEnabled(info) {
		return
	}
	body, truncated := contentAuditBytes(raw)
	record := map[string]interface{}{
		"ts":             time.Now().Format(time.RFC3339Nano),
		"stage":          stage,
		"request_id":     c.GetString(common.RequestIdKey),
		"token_id":       info.TokenId,
		"user_id":        info.UserId,
		"model":          info.OriginModelName,
		"upstream_model": info.UpstreamModelName,
		"channel_id":     info.ChannelId,
		"path":           c.Request.URL.Path,
		"client_ip":      c.ClientIP(),
		"status_code":    statusCode,
		"truncated":      truncated,
		"body":           body,
	}
	line, err := common.Marshal(record)
	if err != nil {
		common.SysError("content audit marshal failed: " + err.Error())
		return
	}
	dir := common.GetEnvOrDefaultString("CONTENT_AUDIT_DIR", "/data/content-audit")
	if err := os.MkdirAll(dir, 0700); err != nil {
		common.SysError("content audit mkdir failed: " + err.Error())
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("audit-%s.jsonl", time.Now().Format("20060102")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		common.SysError("content audit open failed: " + err.Error())
		return
	}
	defer f.Close()
	if _, err := f.Write(append(bytes.TrimSpace(line), '\n')); err != nil {
		common.SysError("content audit write failed: " + err.Error())
	}
}
