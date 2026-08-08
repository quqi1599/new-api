package replicate

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestReplicateUploadUsesRequestWideTimeoutAndClosesReplayGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousTimeout := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previousTimeout })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	part, err := writer.CreateFormFile("image", "input.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("fake-png"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &requestBody)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	require.NoError(t, req.ParseMultipartForm(1<<20))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	info := &relaycommon.RelayInfo{
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: server.URL,
			ChannelType:    constant.ChannelTypeReplicate,
			ApiKey:         "test-key",
		},
	}
	info.EnsureNonStreamDeadline(info.StartTime, 80*time.Millisecond)

	_, err = uploadFileFromForm(c, info, "image")
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
}
