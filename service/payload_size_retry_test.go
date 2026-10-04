package service

import (
	"context"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpstreamPayloadSizeRejectionCannotRetry(t *testing.T) {
	for _, body := range []string{`{"error":{"type":"request_too_large","message":"qwen_large_base64_image"}}`, "<html>Payload too large</html>", ""} {
		err := RelayErrorHandler(context.Background(), &http.Response{StatusCode: http.StatusRequestEntityTooLarge, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, false)
		require.True(t, types.IsSkipRetryError(err))
		require.Equal(t, 413, err.StatusCode)
		ResetStatusCode(err, `{"413":"500"}`)
		require.True(t, types.IsSkipRetryError(err), "status mapping cannot reopen retry")
	}
}
