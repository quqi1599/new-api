package channel

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
)

const (
	cpaAuditIdentitySecretEnv = "CPA_AUDIT_IDENTITY_SECRET"
	cpaAuditHeaderPrefix      = "x-cpa-audit-"
	cpaAuditVersion           = "1"

	cpaAuditHeaderVersion     = "X-CPA-Audit-Version"
	cpaAuditHeaderUserID      = "X-CPA-Audit-User-ID"
	cpaAuditHeaderTokenID     = "X-CPA-Audit-Token-ID"
	cpaAuditHeaderTokenName   = "X-CPA-Audit-Token-Name"
	cpaAuditHeaderRequestID   = "X-CPA-Audit-Request-ID"
	cpaAuditHeaderTimestamp   = "X-CPA-Audit-Timestamp"
	cpaAuditHeaderModel       = "X-CPA-Audit-Model"
	cpaAuditHeaderChannelTest = "X-CPA-Audit-Channel-Test"
	cpaAuditHeaderSignature   = "X-CPA-Audit-Signature"
)

func stripCPAAuditHeaders(header http.Header) {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), cpaAuditHeaderPrefix) {
			header.Del(name)
		}
	}
}

func sanitizeCPAAuditHeaderValue(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		default:
			return r
		}
	}, value))
	if maxRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func cpaAuditCanonicalValues(version, timestamp, requestID, userID, tokenID, tokenName, method, path, model, channelTest string) string {
	values := url.Values{}
	values.Set("channel_test", channelTest)
	values.Set("method", strings.ToUpper(strings.TrimSpace(method)))
	values.Set("model", model)
	values.Set("path", path)
	values.Set("request_id", requestID)
	values.Set("timestamp", timestamp)
	values.Set("token_id", tokenID)
	values.Set("token_name", tokenName)
	values.Set("user_id", userID)
	values.Set("version", version)
	return values.Encode()
}

func applyCPAAuditIdentityHeaders(header http.Header, method, rawURL string, info *common.RelayInfo, now time.Time, secret string) error {
	stripCPAAuditHeaders(header)
	if info == nil || info.ChannelMeta == nil || !info.ChannelOtherSettings.CPAAuditIdentityEnabled {
		return nil
	}

	secret = strings.TrimSpace(secret)
	if secret == "" {
		return types.NewError(
			fmt.Errorf("%s is required for a CPA audit identity channel", cpaAuditIdentitySecretEnv),
			types.ErrorCodeChannelCPAAuditIdentityInvalid,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if !info.IsChannelTest && (info.UserId <= 0 || info.TokenId <= 0 || strings.TrimSpace(info.RequestId) == "") {
		return types.NewError(
			fmt.Errorf("CPA audit identity requires user_id, token_id, and request_id"),
			types.ErrorCodeChannelCPAAuditIdentityInvalid,
			types.ErrOptionWithSkipRetry(),
		)
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return types.NewError(
			fmt.Errorf("parse CPA request URL: %w", err),
			types.ErrorCodeChannelCPAAuditIdentityInvalid,
			types.ErrOptionWithSkipRetry(),
		)
	}
	path := parsedURL.EscapedPath()
	if path == "" {
		path = "/"
	}

	timestamp := strconv.FormatInt(now.Unix(), 10)
	requestID := sanitizeCPAAuditHeaderValue(info.RequestId, 128)
	tokenName := sanitizeCPAAuditHeaderValue(info.TokenName, 128)
	userIDValue := info.UserId
	tokenIDValue := info.TokenId
	if info.IsChannelTest {
		userIDValue = 0
		tokenIDValue = 0
		tokenName = "__channel_test__"
		if requestID == "" {
			requestID = "channel-test"
		}
	}
	model := sanitizeCPAAuditHeaderValue(info.UpstreamModelName, 256)
	if model == "" {
		model = sanitizeCPAAuditHeaderValue(info.OriginModelName, 256)
	}
	userID := strconv.Itoa(userIDValue)
	tokenID := strconv.Itoa(tokenIDValue)
	channelTest := "0"
	if info.IsChannelTest {
		channelTest = "1"
	}
	canonical := cpaAuditCanonicalValues(
		cpaAuditVersion,
		timestamp,
		requestID,
		userID,
		tokenID,
		tokenName,
		method,
		path,
		model,
		channelTest,
	)

	header.Set(cpaAuditHeaderVersion, cpaAuditVersion)
	header.Set(cpaAuditHeaderUserID, userID)
	header.Set(cpaAuditHeaderTokenID, tokenID)
	header.Set(cpaAuditHeaderTokenName, tokenName)
	header.Set(cpaAuditHeaderRequestID, requestID)
	header.Set(cpaAuditHeaderTimestamp, timestamp)
	header.Set(cpaAuditHeaderModel, model)
	if info.IsChannelTest {
		header.Set(cpaAuditHeaderChannelTest, channelTest)
	}
	header.Set(cpaAuditHeaderSignature, common2.HmacSha256(canonical, secret))
	return nil
}

func applyCPAAuditIdentity(req *http.Request, info *common.RelayInfo) error {
	if req == nil {
		return nil
	}
	return applyCPAAuditIdentityHeaders(
		req.Header,
		req.Method,
		req.URL.String(),
		info,
		time.Now(),
		common2.CPAAuditIdentitySecret,
	)
}
