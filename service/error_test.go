package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type relayErrorReadCloser struct {
	err    error
	closed bool
}

func (r *relayErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *relayErrorReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
		})
	}
}

func TestRelayErrorHandlerTruncatesInvalidJSONBodyInLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.True(t, newAPIError.HasUpstreamResponse())
	require.Equal(t, "bad response status code 500", newAPIError.Error())
	require.Contains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), fmt.Sprintf("original_length=%d", len(body)))
	require.NotContains(t, logBuffer.String(), strings.Repeat("b", common.LocalLogContentLimit+1))
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.True(t, newAPIError.HasUpstreamResponse())
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.True(t, newAPIError.HasUpstreamResponse())
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerPreservesAuthUnavailableCode(t *testing.T) {
	body := `{"error":{"message":"requested route is temporarily unavailable","type":"upstream_error","code":"auth_unavailable"}}`
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.True(t, newAPIError.HasUpstreamResponse())
	require.Equal(t, types.ErrorCodeAuthUnavailable, newAPIError.GetErrorCode())
}

func TestRelayErrorHandlerKeepsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), body)
}

func TestRelayErrorHandlerPreservesTypedBodyTimeout(t *testing.T) {
	typedTimeout := types.NewErrorWithStatusCode(
		errors.New("upstream response body timed out"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
	body := &relayErrorReadCloser{err: typedTimeout}
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       body,
	}

	got := RelayErrorHandler(context.Background(), resp, false)

	require.Same(t, typedTimeout, got)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, got.GetErrorCode())
	require.Equal(t, http.StatusGatewayTimeout, got.StatusCode)
	require.True(t, types.IsSkipRetryError(got))
	require.True(t, types.IsChannelPenaltyAllowed(got))
	require.False(t, got.HasUpstreamResponse())
	require.True(t, body.closed)
}

func TestTaskErrorWrapperPreservesTypedTimeout(t *testing.T) {
	typedTimeout := types.NewErrorWithStatusCode(
		errors.New("upstream response headers timed out"),
		types.ErrorCodeUpstreamResponseHeaderTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)

	taskErr := TaskErrorWrapper(typedTimeout, "read_response_body_failed", http.StatusInternalServerError)

	require.Equal(t, string(types.ErrorCodeUpstreamResponseHeaderTimeout), taskErr.Code)
	require.Equal(t, http.StatusGatewayTimeout, taskErr.StatusCode)
	require.True(t, taskErr.SkipRetry)
	var preserved *types.NewAPIError
	require.ErrorAs(t, taskErr.Error, &preserved)
	require.Same(t, typedTimeout, preserved)
	require.True(t, types.IsChannelPenaltyAllowed(preserved))
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}
