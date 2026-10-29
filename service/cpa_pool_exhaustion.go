package service

import (
	"context"
	"net/http"
)

type cpaPoolResponseEvidence struct{}

// SetCPAPoolResponseTrusted is called only by the transport after validating
// the administrator-owned CPA setting and the complete same-origin chain.
// Clone the response request so evidence cannot leak into a later attempt.
func SetCPAPoolResponseTrusted(resp *http.Response, trusted bool) {
	if resp != nil && resp.Request != nil {
		resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), cpaPoolResponseEvidence{}, trusted))
	}
}
func trustedCPAPoolResponse(resp *http.Response) bool {
	if resp == nil || resp.Request == nil {
		return false
	}
	trusted, _ := resp.Request.Context().Value(cpaPoolResponseEvidence{}).(bool)
	return trusted
}
