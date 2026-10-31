package router

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestImageResultDownloadRouteUsesTicketWithoutAPIKeyOrBilling(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(key)), 0600))
	t.Setenv("IMAGE_RESULT_CACHE_PUBLIC_BASE_URL", "https://download.example")
	t.Setenv("IMAGE_RESULT_CACHE_KEY_FILE", keyFile)
	t.Setenv("IMAGE_RESULT_CACHE_DIR", filepath.Join(dir, "cache"))
	require.NoError(t, service.InitImageResultCacheFromEnv())
	t.Cleanup(service.CloseImageResultCache)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(-time.Minute).Unix()))
	payload = append(payload, "https://upstream.example/original.png"...)
	nonce := make([]byte, aead.NonceSize())
	ticket := base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, payload, []byte("image-download-ticket")))
	engine := gin.New()
	SetRelayRouter(engine)
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(method, "/v1/image-results/"+ticket, nil))
		require.Equal(t, 410, w.Code, "不依赖 API Key、数据库、额度或渠道，直接验证下载凭证")
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("GET", "/v1/image-results/tampered", nil))
	require.Equal(t, 404, w.Code)
	require.Contains(t, w.Body.String(), "invalid_image_link")
}
