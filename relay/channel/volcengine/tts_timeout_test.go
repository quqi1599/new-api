package volcengine

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

type volcengineErrorReadCloser struct {
	err    error
	closed bool
}

func (r *volcengineErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *volcengineErrorReadCloser) Close() error {
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
	body := &volcengineErrorReadCloser{err: typedTimeout}
	resp := &http.Response{Body: body}

	_, got := handleTTSResponse(nil, resp, nil, "mp3")

	require.Same(t, typedTimeout, got)
	require.True(t, body.closed)
}
