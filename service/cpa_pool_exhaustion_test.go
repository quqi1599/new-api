package service

import (
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPoolExhaustionRequiresTrustedExactResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
		status  int
		values  []string
		code    string
		want    bool
	}{
		{"valid", true, 503, []string{"capacity-v1"}, "model_cooldown", true},
		{"untrusted", false, 503, []string{"capacity-v1"}, "model_cooldown", false},
		{"unknown_version", true, 503, []string{"capacity-v2"}, "model_cooldown", false},
		{"duplicate", true, 503, []string{"capacity-v1", "capacity-v1"}, "model_cooldown", false},
		{"wrong_status", true, 429, []string{"capacity-v1"}, "model_cooldown", false},
		{"capability", true, 503, []string{"capacity-v1"}, "request_feature_unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/", nil)
			c.Request.Header.Set("X-CPA-Pool-Exhausted", "capacity-v1")
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Cpa-Pool-Exhausted": tc.values}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"` + tc.code + `","message":"synthetic"}}`))}
			resp.Request = httptest.NewRequest("POST", "https://cpa.example/v1/messages", nil)
			if tc.trusted {
				SetCPAPoolResponseTrusted(resp, true)
			}
			e := RelayErrorHandler(c.Request.Context(), resp, false)
			if e.GatewayPoolExhausted() != tc.want || e.GatewayRetryAttempts() != 0 {
				t.Fatalf("scope=%v attempts=%d", e.GatewayPoolExhausted(), e.GatewayRetryAttempts())
			}
			ResetStatusCode(e, `{"503":429}`)
			if e.GatewayPoolExhausted() != tc.want {
				t.Fatal("status mapping erased contract")
			}
		})
	}
}
