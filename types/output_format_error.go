package types

import (
	"errors"
	"net/http"
)

// OutputFormatRouteError is created only by local, pre-request conversion.
// Matching a provider's code/message cannot authorize this capability fallback.
type OutputFormatRouteError struct{ Message string }

func (e *OutputFormatRouteError) Error() string { return e.Message }

func NewOutputFormatRouteError(message string) *NewAPIError {
	return NewErrorWithStatusCode(&OutputFormatRouteError{Message: message}, "output_format_route_unsupported", http.StatusUnprocessableEntity, ErrOptionWithSkipRetry())
}

func IsLocalOutputFormatRouteError(err *NewAPIError) bool {
	var routeErr *OutputFormatRouteError
	return err != nil && !err.HasUpstreamResponse() && err.StatusCode == http.StatusUnprocessableEntity && err.GetErrorCode() == "output_format_route_unsupported" && errors.As(err.Err, &routeErr)
}
