package channel

import (
	"net/http"
	"testing"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestApplyCPAAuditIdentityHeadersSignsVerifiedCustomerMetadata(t *testing.T) {
	header := http.Header{
		"X-CPA-Audit-Signature": []string{"spoofed"},
		"X-CPA-Audit-User-Id":   []string{"999"},
	}
	info := &relaycommon.RelayInfo{
		UserId:          42,
		TokenId:         73,
		TokenName:       "production-token",
		RequestId:       "req-abc",
		OriginModelName: "gpt-5.4",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.4-cpa",
		},
	}
	info.ChannelOtherSettings.CPAAuditIdentityEnabled = true
	now := time.Unix(1_800_000_000, 0)

	err := applyCPAAuditIdentityHeaders(header, http.MethodPost, "https://cpa.example/v1/responses?beta=true", info, now, "shared-secret")
	require.NoError(t, err)
	require.Equal(t, "1", header.Get(cpaAuditHeaderVersion))
	require.Equal(t, "42", header.Get(cpaAuditHeaderUserID))
	require.Equal(t, "73", header.Get(cpaAuditHeaderTokenID))
	require.Equal(t, "production-token", header.Get(cpaAuditHeaderTokenName))
	require.Equal(t, "req-abc", header.Get(cpaAuditHeaderRequestID))
	require.Equal(t, "1800000000", header.Get(cpaAuditHeaderTimestamp))
	require.Equal(t, "gpt-5.4-cpa", header.Get(cpaAuditHeaderModel))

	canonical := cpaAuditCanonicalValues(
		"1",
		"1800000000",
		"req-abc",
		"42",
		"73",
		"production-token",
		http.MethodPost,
		"/v1/responses",
		"gpt-5.4-cpa",
		"0",
	)
	require.Equal(t, common2.HmacSha256(canonical, "shared-secret"), header.Get(cpaAuditHeaderSignature))
	require.NotEqual(t, "spoofed", header.Get(cpaAuditHeaderSignature))
}

func TestApplyCPAAuditIdentityHeadersStripsSpoofedHeadersWhenDisabled(t *testing.T) {
	header := http.Header{
		"X-CPA-Audit-Signature": []string{"spoofed"},
		"X-CPA-Audit-User-Id":   []string{"999"},
		"X-Unrelated":           []string{"kept"},
	}

	err := applyCPAAuditIdentityHeaders(header, http.MethodPost, "https://provider.example/v1/responses", &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}, time.Now(), "")
	require.NoError(t, err)
	require.Empty(t, header.Get(cpaAuditHeaderSignature))
	require.Empty(t, header.Get(cpaAuditHeaderUserID))
	require.Equal(t, "kept", header.Get("X-Unrelated"))
}

func TestApplyCPAAuditIdentityHeadersFailsClosedWithoutSecret(t *testing.T) {
	info := &relaycommon.RelayInfo{
		UserId:      42,
		TokenId:     73,
		RequestId:   "req-abc",
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	info.ChannelOtherSettings.CPAAuditIdentityEnabled = true

	err := applyCPAAuditIdentityHeaders(http.Header{}, http.MethodPost, "https://cpa.example/v1/responses", info, time.Now(), "")
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.True(t, types.IsSkipRetryError(apiErr))
	require.Equal(t, types.ErrorCodeChannelCPAAuditIdentityInvalid, apiErr.GetErrorCode())
}

func TestApplyCPAAuditIdentityHeadersSignsChannelTestsWithoutCustomerIdentity(t *testing.T) {
	header := http.Header{"X-CPA-Audit-Signature": []string{"spoofed"}}
	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta:   &relaycommon.ChannelMeta{},
	}
	info.ChannelOtherSettings.CPAAuditIdentityEnabled = true

	err := applyCPAAuditIdentityHeaders(header, http.MethodPost, "https://cpa.example/v1/chat/completions", info, time.Now(), "shared-secret")
	require.NoError(t, err)
	require.Equal(t, "0", header.Get(cpaAuditHeaderUserID))
	require.Equal(t, "0", header.Get(cpaAuditHeaderTokenID))
	require.Equal(t, "__channel_test__", header.Get(cpaAuditHeaderTokenName))
	require.Equal(t, "1", header.Get(cpaAuditHeaderChannelTest))
	require.NotEmpty(t, header.Get(cpaAuditHeaderSignature))
}

func TestShouldSkipPassthroughHeaderRejectsCPAIdentityNamespace(t *testing.T) {
	require.True(t, shouldSkipPassthroughHeader("X-CPA-Audit-User-ID"))
	require.True(t, shouldSkipPassthroughHeader("x-cpa-audit-signature"))
}
