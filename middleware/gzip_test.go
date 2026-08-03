package middleware

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDecompressRequestMiddlewareClearsCompressedContentLength(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalBody := bytes.Repeat([]byte(`{"input":"compressible"}`), 256)
	var compressedBody bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressedBody)
	_, err := gzipWriter.Write(originalBody)
	require.NoError(t, err)
	require.NoError(t, gzipWriter.Close())

	router := gin.New()
	router.Use(DecompressRequestMiddleware())
	router.POST("/", func(c *gin.Context) {
		require.Equal(t, int64(-1), c.Request.ContentLength)
		require.Empty(t, c.GetHeader("Content-Encoding"))

		storage, storageErr := common.CreateBodyStorageFromReader(c.Request.Body, c.Request.ContentLength, 1<<20)
		require.NoError(t, storageErr)
		defer storage.Close()

		decodedBody, readErr := storage.Bytes()
		require.NoError(t, readErr)
		require.Equal(t, originalBody, decodedBody)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(compressedBody.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	require.Greater(t, request.ContentLength, int64(0))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}
