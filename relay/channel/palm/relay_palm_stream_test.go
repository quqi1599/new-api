package palm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newPalmStreamTest(body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "palm-test"}}
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
	return c, resp, info, recorder
}

func TestPalmStreamUsesRequestWideFirstEventBudget(t *testing.T) {
	c, _, info, recorder := newPalmStreamTest("")
	reader, writer := io.Pipe()
	defer writer.Close()
	resp := &http.Response{StatusCode: http.StatusOK, Body: reader}
	info.SetFirstValidEventDeadline(time.Now().Add(30 * time.Millisecond))

	streamErr, text := palmStreamHandler(c, info, resp)

	require.NotNil(t, streamErr)
	require.Equal(t, http.StatusGatewayTimeout, streamErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamFirstEventTimeout, streamErr.GetErrorCode())
	require.Empty(t, text)
	require.Zero(t, info.ReceivedResponseCount)
	require.Empty(t, recorder.Body.String())
}

func TestPalmStreamOnlyFinalizesAfterValidCandidate(t *testing.T) {
	c, resp, info, recorder := newPalmStreamTest(`{"candidates":[{"author":"assistant","content":"hello"}]}`)

	streamErr, text := palmStreamHandler(c, info, resp)

	require.Nil(t, streamErr)
	require.Equal(t, "hello", text)
	require.Equal(t, 1, info.ReceivedResponseCount)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
}

func TestPalmStreamInvalidBodyDoesNotManufactureDone(t *testing.T) {
	for _, body := range []string{"not-json", `{"candidates":[]}`} {
		c, resp, info, recorder := newPalmStreamTest(body)

		streamErr, text := palmStreamHandler(c, info, resp)

		require.NotNil(t, streamErr)
		require.Empty(t, text)
		require.Zero(t, info.ReceivedResponseCount)
		require.Empty(t, recorder.Body.String())
		require.Empty(t, recorder.Header().Get("Content-Type"))
	}
}
