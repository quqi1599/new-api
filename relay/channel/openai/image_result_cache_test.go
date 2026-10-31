package openai

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func TestImagesCachedURLsPreserveUsageCountAndSSEFraming(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0600))
	t.Setenv("IMAGE_RESULT_CACHE_PUBLIC_BASE_URL", "https://download.example")
	t.Setenv("IMAGE_RESULT_CACHE_KEY_FILE", keyFile)
	t.Setenv("IMAGE_RESULT_CACHE_DIR", filepath.Join(dir, "cache"))
	require.NoError(t, service.InitImageResultCacheFromEnv())
	t.Cleanup(service.CloseImageResultCache)
	// 此 fixture 不调用生成接口，也不允许网络取图成功。
	const rawURL = "http://127.0.0.1/never-generate.png"
	for _, edit := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, upstreamStream := range []bool{false, true} {
				t.Run(fmt.Sprintf("edit=%t/stream=%t/upstream=%t", edit, stream, upstreamStream), func(t *testing.T) {
					name := "image_generation.completed"
					if edit {
						name = "image_edit.completed"
					}
					body := `{"created":123,"data":[{"url":"` + rawURL + `","revised_prompt":"keep"}],"usage":` + imageUsageFixture + `}`
					contentType := "application/json"
					if upstreamStream {
						body = "event: " + name + "\ndata: {\"type\":\"" + name + "\",\"url\":\"" + rawURL + "\",\"usage\":" + imageUsageFixture + "}\n\ndata: [DONE]\n\n"
						contentType = "text/event-stream"
					}
					c, w, resp, info := imageCompatContext(t, body, contentType, common.GetPointer(stream), edit)
					result, apiErr := (&Adaptor{}).DoResponse(c, resp, info)
					require.Nil(t, apiErr)
					require.Contains(t, w.Body.String(), "https://download.example/v1/image-results/")
					require.NotContains(t, w.Body.String(), rawURL)
					count, exists := ConfirmedImageCount(c)
					require.True(t, exists)
					require.Equal(t, 1, count)
					usage := result.(*dto.Usage)
					require.Equal(t, 15, usage.PromptTokens)
					require.Equal(t, 1352, usage.CompletionTokens)
					if stream {
						require.Equal(t, 1, strings.Count(w.Body.String(), "data: [DONE]"))
						require.Contains(t, w.Body.String(), "event: "+name)
					} else {
						require.Equal(t, fmt.Sprint(w.Body.Len()), w.Header().Get("Content-Length"))
					}
				})
			}
		}
	}
}
