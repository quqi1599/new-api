package vertex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func withVertexTokenServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previousURL := vertexOAuthTokenURL
	vertexOAuthTokenURL = server.URL
	t.Cleanup(func() {
		vertexOAuthTokenURL = previousURL
		server.Close()
	})
	return server
}

func TestVertexRelayTokenExchangeUsesRequestWideBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousTimeout := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previousTimeout })

	withVertexTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		StartTime:   time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	info.EnsureNonStreamDeadline(info.StartTime, 80*time.Millisecond)

	_, err := exchangeJwtForAccessTokenWithContext(c.Request.Context(), c, "signed-jwt", "", info)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.False(t, info.UpstreamRequestMayHaveBeenAccepted(), "safe token exchange must not close the generation replay gate")
}

func TestVertexTokenExchangeHonorsCallerDeadline(t *testing.T) {
	previousTimeout := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 5
	t.Cleanup(func() { common.RelayNonStreamTimeout = previousTimeout })

	withVertexTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := exchangeJwtForAccessTokenWithProxy(ctx, "signed-jwt", "")
	require.Error(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
}
