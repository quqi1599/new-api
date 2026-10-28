package types

import (
	"errors"
	"net/http"
)

// NewEmbeddingRouteUnsupportedError is a deterministic local capability error,
// not evidence of an upstream outage or a reason to replay a dispatched request.
func NewEmbeddingRouteUnsupportedError() *NewAPIError {
	return NewErrorWithStatusCode(errors.New("Anthropic Messages channels do not support embeddings; select a compatible embedding model and route"), ErrorCodeRequestFeatureUnsupported, http.StatusBadRequest, ErrOptionWithSkipRetry())
}
