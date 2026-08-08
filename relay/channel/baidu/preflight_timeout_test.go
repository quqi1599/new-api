package baidu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

func TestBaiduTokenExchangeUsesRelayBudgetAndReturnsTypedTimeout(t *testing.T) {
	previousTimeout := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previousTimeout })
	baiduTokenStore.Clear()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	previousURL := baiduOAuthTokenURL
	baiduOAuthTokenURL = server.URL
	t.Cleanup(func() { baiduOAuthTokenURL = previousURL })

	info := &relaycommon.RelayInfo{
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey: "client|secret",
		},
	}
	info.EnsureNonStreamDeadline(info.StartTime, 80*time.Millisecond)

	_, err := getBaiduAccessToken(context.Background(), info)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, info.UpstreamRequestMayHaveBeenAccepted(), "safe token exchange must not close the generation replay gate")
}
