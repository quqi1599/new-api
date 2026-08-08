package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAITTSErrorReadCloser struct {
	err error
}

func (r openAITTSErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (openAITTSErrorReadCloser) Close() error               { return nil }

func TestOpenaiTTSHandlerPreservesTypedBodyTimeoutBeforeCommitting200(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	typedTimeout := types.NewErrorWithStatusCode(
		errors.New("upstream response body timed out"),
		types.ErrorCodeUpstreamNonStreamTimeout,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"audio/mpeg"}},
		Body:       openAITTSErrorReadCloser{err: typedTimeout},
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	usage, got := OpenaiTTSHandler(c, resp, info)

	require.Nil(t, usage)
	require.Same(t, typedTimeout, got)
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
}
