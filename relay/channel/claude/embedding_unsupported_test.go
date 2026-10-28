package claude

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingConversionIsExplicitCapabilityError(t *testing.T) {
	request, err := (&Adaptor{}).ConvertEmbeddingRequest(nil, nil, dto.EmbeddingRequest{})
	require.Nil(t, request)
	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, 400, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeRequestFeatureUnsupported, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
	require.False(t, apiErr.HasUpstreamResponse())
}
