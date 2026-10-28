package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingExplicitAnthropicRejectsBeforeSelectingKey(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/engines/custom-model/embeddings"} {
		t.Run(path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", path, nil)
			// An empty key and unset database exercise the capability guard
			// before key selection or any billing work can occur.
			ch := &model.Channel{Id: 141, Type: constant.ChannelTypeAnthropic}
			err := SetupContextForSelectedChannel(c, ch, "synthetic-embedding")
			require.NotNil(t, err)
			require.Equal(t, 400, err.StatusCode)
			require.Equal(t, types.ErrorCodeRequestFeatureUnsupported, err.GetErrorCode())
			require.True(t, types.IsSkipRetryError(err))
			_, selected := c.Get(string(constant.ContextKeyChannelId))
			require.False(t, selected)
		})
	}
}
