package types

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingRequestRequiresEndpointCapability(t *testing.T) {
	require.Equal(t, constant.EndpointTypeEmbeddings, RelayFormatToRequiredEndpointType(RelayFormatEmbedding))
	for _, path := range []string{"/v1/embeddings", "/v1/engines/custom-model/embeddings"} {
		require.Equal(t, constant.EndpointTypeEmbeddings, PathToRequiredEndpointType(path))
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/embeddings-extended", "/v1/engines/embeddings-helper/completions"} {
		require.Empty(t, PathToRequiredEndpointType(path))
	}
	err := NewError(NewEmbeddingRouteUnsupportedError(), ErrorCodeConvertRequestFailed, ErrOptionWithSkipRetry())
	require.Equal(t, http.StatusBadRequest, err.StatusCode)
	require.Equal(t, ErrorCodeRequestFeatureUnsupported, err.GetErrorCode())
	require.True(t, IsSkipRetryError(err))
	require.False(t, err.HasUpstreamResponse())
}
