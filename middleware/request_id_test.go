package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func runRequestIDMiddleware(t *testing.T, headers map[string]string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for name, value := range headers {
		c.Request.Header.Set(name, value)
	}
	RequestId()(c)
	return c, recorder
}

func TestRequestIDPreservesValidatedEdgeIDAndKeepsInternalID(t *testing.T) {
	c, recorder := runRequestIDMiddleware(t, map[string]string{"X-Request-Id": "edge:trace-123"})

	require.Equal(t, "edge:trace-123", c.GetString(common.RequestIdKey))
	require.Equal(t, "edge:trace-123", recorder.Header().Get(common.RequestIdKey))
	require.Equal(t, "edge:trace-123", c.Request.Context().Value(common.RequestIdKey))
	require.NotEmpty(t, c.GetString(common.InternalRequestIdKey))
	require.NotEqual(t, c.GetString(common.RequestIdKey), c.GetString(common.InternalRequestIdKey))
}

func TestRequestIDRejectsLogInjectionAndOversizedValues(t *testing.T) {
	for _, invalidID := range []string{"trace\nforged", "trace with spaces", strings.Repeat("a", 129)} {
		c, recorder := runRequestIDMiddleware(t, map[string]string{common.RequestIdKey: invalidID})

		require.NotEmpty(t, c.GetString(common.RequestIdKey))
		require.NotEqual(t, invalidID, c.GetString(common.RequestIdKey))
		require.Equal(t, c.GetString(common.RequestIdKey), recorder.Header().Get(common.RequestIdKey))
		require.Equal(t, c.GetString(common.RequestIdKey), c.GetString(common.InternalRequestIdKey))
	}
}

func TestRequestIDUsesCanonicalHeaderPrecedence(t *testing.T) {
	c, _ := runRequestIDMiddleware(t, map[string]string{
		common.RequestIdKey: "canonical-id",
		"X-Request-Id":     "fallback-id",
	})

	require.Equal(t, "canonical-id", c.GetString(common.RequestIdKey))
}
