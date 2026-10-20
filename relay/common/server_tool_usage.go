package common

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
)

// Invalid accounting arrived after the upstream generated a response. Reject
// the malformed result without authorizing another model generation.
func ServerToolUsageResponseError(err error) *types.NewAPIError {
	if !errors.Is(err, dto.ErrInvalidServerToolUsage) {
		return nil
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
}

func IsServerToolUsageResponseError(err *types.NewAPIError) bool {
	return err != nil && errors.Is(err.Err, dto.ErrInvalidServerToolUsage)
}
