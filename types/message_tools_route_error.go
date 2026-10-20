package types

import (
	"errors"
	"net/http"
)

// MessageToolsRouteError marks a local lossless-routing rejection before any
// upstream request. An upstream error with the same code cannot gain this type.
type MessageToolsRouteError struct{}

func (*MessageToolsRouteError) Error() string {
	return "message-level tools require a native Chat Completions route; this channel cannot preserve their position in history"
}

func NewMessageToolsRouteError() *NewAPIError {
	return NewErrorWithStatusCode(&MessageToolsRouteError{}, "message_tools_route_unsupported", http.StatusUnprocessableEntity, ErrOptionWithSkipRetry())
}

func IsLocalMessageToolsRouteError(err *NewAPIError) bool {
	var routeErr *MessageToolsRouteError
	return err != nil && !err.HasUpstreamResponse() && err.StatusCode == http.StatusUnprocessableEntity && err.GetErrorCode() == "message_tools_route_unsupported" && errors.As(err.Err, &routeErr)
}
