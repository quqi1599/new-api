package middleware

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBatch1AccessLogCredentials(t *testing.T) {
	writer := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = writer })
	for _, target := range []string{
		"/v1/models?%6bey=SECRET&key=SECOND&API-Key=THIRD&session_token=FOURTH&alt=sse",
		"/v1/models?key=SECRET%ZZ&token=SECOND;other=true&alt=sse",
		"/v1/models%23test%25?key=SECRET&alt=sse",
		"/%61pi/oauth/test?code=SECRET&state=SECOND",
		"/oauth/test?code=SECRET&state=SECOND",
		"/v1/image-results/SECRET?download=1",
		"/v1/%69mage-results/SECRET?download=1",
	} {
		t.Run(target, func(t *testing.T) {
			var output bytes.Buffer
			gin.DefaultWriter = &output
			r := gin.New()
			SetUpLogger(r)
			req := httptest.NewRequest("GET", target, nil)
			original := *req.URL
			r.NoRoute(func(c *gin.Context) {
				require.Equal(t, original, *c.Request.URL)
				require.Equal(t, target, c.Request.RequestURI)
				c.Status(401)
			})
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			require.Equal(t, 401, response.Code)
			for _, secret := range []string{"SECRET", "SECOND", "THIRD", "FOURTH"} {
				require.NotContains(t, output.String(), secret)
			}
			require.Equal(t, original, *req.URL)
			if strings.Contains(target, "alt=sse") && !strings.Contains(target, "%ZZ") {
				require.Contains(t, output.String(), "alt=sse")
			}
		})
	}
}

func TestBatch1AccessLogPathControls(t *testing.T) {
	for _, target := range []string{"/v1/models", "/v1/models?alt=sse&api-version=2024-02-01", "//v1%2Fmodels?alt=sse", "/v1/models%23test%25?alt=%23sse"} {
		require.Equal(t, target, accessLogPath(target))
	}
	for _, key := range []string{"key", "API_KEY", "api-key", "apikey", "x-api-key", "x-goog-api-key", "authorization", "auth", "password", "passwd", "sig", "AWSAccessKeyId", "x-amz-credential", "download_token", "client_secret", "x-amz-signature"} {
		require.NotContains(t, accessLogPath("/v1/models?"+key+"=SECRET"), "SECRET", key)
	}
	require.Equal(t, "/api/oauth/test", accessLogPath("/api/oauth/test?code=SECRET&state=SECRET"))
	require.Equal(t, "/v1/models", accessLogPath("/v1/models?key=SECRET%ZZ"))
	require.Equal(t, "[invalid request URI]", accessLogPath("/%ZZ?key=SECRET"))
}

func TestBatch1AccessLogRetainsClientPathAfterAdapterRewrite(t *testing.T) {
	writer := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = writer })
	var output bytes.Buffer
	gin.DefaultWriter = &output
	r := gin.New()
	SetUpLogger(r)
	r.POST("/client/video", func(c *gin.Context) {
		c.Request.URL.Path = "/v1/video/generations"
		c.Status(200)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/client/video?key=SECRET&alt=sse", nil))
	require.Contains(t, output.String(), "/client/video?")
	require.NotContains(t, output.String(), "/v1/video/generations")
	require.NotContains(t, output.String(), "SECRET")
}
