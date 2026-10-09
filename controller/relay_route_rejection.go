package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/types"
)

// A known unavailable entrance may still fail over to another channel, but
// repeating that entrance in a later round of the same request adds no useful
// fallback. This is request-local; it never disables a channel or a model.
func isKnownUnavailableRouteError(err *types.NewAPIError) bool {
	if err == nil || !err.HasUpstreamResponse() {
		return false
	}
	switch string(err.GetErrorCode()) {
	case "model_not_supported", "model_not_found":
		return err.StatusCode == http.StatusNotFound
	case "model_cooldown":
		return err.StatusCode == http.StatusTooManyRequests || err.StatusCode == http.StatusServiceUnavailable
	case "upstream_connection_refused":
		return err.StatusCode == http.StatusBadGateway
	case "reasoning_budget_unsupported", "reasoning_effort_unsupported", "reasoning_extension_unsupported":
		// CPA rejected this entrance before sending because it cannot preserve
		// the requested thinking control. Another compatible entrance may work,
		// but revisiting this one in the same request cannot restore capability.
		return err.StatusCode == http.StatusUnprocessableEntity
	default:
		return false
	}
}
