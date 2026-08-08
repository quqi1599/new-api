package taskcommon

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestDoLegacyPollingRequestUsesBoundedContextAndCancelsOnClose(t *testing.T) {
	var requestContext context.Context
	resp, err := DoLegacyPollingRequest(func(ctx context.Context) (*http.Response, error) {
		requestContext = ctx
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	select {
	case <-requestContext.Done():
		require.ErrorIs(t, requestContext.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("legacy polling context was not canceled when the response body closed")
	}
}

func TestPollingRequestTimeoutCapsAtTenMinutes(t *testing.T) {
	previous := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1_200
	t.Cleanup(func() { common.RelayNonStreamTimeout = previous })

	require.Equal(t, 10*time.Minute, PollingRequestTimeout())
}
