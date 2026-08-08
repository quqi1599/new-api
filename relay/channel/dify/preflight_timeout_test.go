package dify

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDifyConvertPropagatesUploadTimeoutInsteadOfDroppingMedia(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousTimeout := common.RelayNonStreamTimeout
	common.RelayNonStreamTimeout = 1
	t.Cleanup(func() { common.RelayNonStreamTimeout = previousTimeout })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: server.URL,
			ApiKey:         "test-key",
		},
	}
	info.EnsureNonStreamDeadline(info.StartTime, 80*time.Millisecond)
	request := &dto.GeneralOpenAIRequest{
		User: json.RawMessage(`"test-user"`),
		Messages: []dto.Message{{
			Role: "user",
			Content: []any{
				dto.MediaContent{
					Type: dto.ContentTypeImageURL,
					ImageUrl: &dto.MessageImageUrl{
						Url:      "data:image/png;base64,aGVsbG8=",
						MimeType: "image/png",
					},
				},
			},
		}},
	}

	adaptor := &Adaptor{}
	converted, err := adaptor.ConvertOpenAIRequest(c, info, request)
	require.Nil(t, converted)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeUpstreamNonStreamTimeout, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.True(t, types.IsChannelPenaltyAllowed(apiErr))
	require.True(t, info.UpstreamRequestMayHaveBeenAccepted())
}
