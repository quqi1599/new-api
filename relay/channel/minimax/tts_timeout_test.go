package minimax

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

type minimaxErrorReadCloser struct {
	err    error
	closed bool
}

func (r *minimaxErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *minimaxErrorReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestTTSResponsePreservesTypedBodyTimeout(t *testing.T) {
	typedTimeout := types.NewErrorWithStatusCode(
		errors.New("upstream response body timed out"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
	body := &minimaxErrorReadCloser{err: typedTimeout}
	resp := &http.Response{Body: body}

	_, got := handleTTSResponse(nil, resp, nil)

	require.Same(t, typedTimeout, got)
	require.True(t, body.closed)
}

func TestChatCompletionResponsePreservesTypedBodyTimeout(t *testing.T) {
	typedTimeout := types.NewErrorWithStatusCode(
		errors.New("upstream response body timed out"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
	body := &minimaxErrorReadCloser{err: typedTimeout}
	resp := &http.Response{Body: body}

	_, got := handleChatCompletionResponse(nil, resp, nil)

	require.Same(t, typedTimeout, got)
	require.True(t, body.closed)
}
