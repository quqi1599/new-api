package types

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageToolsRouteErrorRequiresLocalIdentity(t *testing.T) {
	local := NewMessageToolsRouteError()
	require.True(t, IsLocalMessageToolsRouteError(local))
	require.True(t, IsSkipRetryError(local), "only the explicit local fallback may override SkipRetry")
	for _, err := range []*NewAPIError{
		nil,
		NewErrorWithStatusCode(errors.New(local.Error()), "message_tools_route_unsupported", http.StatusUnprocessableEntity),
		WithOpenAIError(OpenAIError{Code: "message_tools_route_unsupported", Message: local.Error()}, http.StatusUnprocessableEntity, ErrOptionWithUpstreamResponse()),
		NewErrorWithStatusCode(&MessageToolsRouteError{}, "message_tools_route_unsupported", http.StatusUnprocessableEntity, ErrOptionWithUpstreamResponse()),
		NewErrorWithStatusCode(&MessageToolsRouteError{}, "message_tools_route_unsupported", http.StatusBadRequest),
	} {
		require.False(t, IsLocalMessageToolsRouteError(err))
	}
}
