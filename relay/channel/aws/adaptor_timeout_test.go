package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConvertClaudeRequestPreservesTypedMediaDownloadDeadline(t *testing.T) {
	service.InitHttpClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	request := &dto.ClaudeRequest{
		Messages: []dto.ClaudeMessage{{
			Role: "user",
			Content: []dto.ClaudeMediaMessage{{
				Type: "image",
				Source: &dto.ClaudeMessageSource{
					Type: "url",
					Url:  "https://1.1.1.1/image.png",
				},
			}},
		}},
	}

	_, err := (&Adaptor{}).ConvertClaudeRequest(c, &relaycommon.RelayInfo{}, request)

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr), "conversion must retain the typed download timeout: %v", err)
	require.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsChannelPenaltyAllowed(apiErr))
}
