package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var cacheTestPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=")

func testImageResultCache(t *testing.T, handler http.HandlerFunc) (*ImageResultCache, string) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cache, err := newImageResultCache(t.TempDir(), "https://download.example", bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	// 仅测试 fixture 允许回环地址，生产客户端使用拨号时 SSRF 防护。
	cache.client = upstream.Client()
	t.Cleanup(cache.close)
	return cache, upstream.URL + "/original.png?secret=upstream-private-signature"
}

func serveCacheLink(cache *ImageResultCache, link string) *httptest.ResponseRecorder {
	u, _ := url.Parse(link)
	w := httptest.NewRecorder()
	cache.serve(w, httptest.NewRequest(http.MethodGet, link, nil), strings.TrimPrefix(u.Path, imageResultPath))
	return w
}

func TestImageResultCacheSameOriginPersistentAndNoRegeneration(t *testing.T) {
	var reads, mutations atomic.Int32
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
		}
		reads.Add(1)
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(cacheTestPNG)
	})
	link, err := cache.link(raw)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(link, "https://download.example/v1/image-results/"))
	require.NotContains(t, link, raw)
	require.NotContains(t, link, "upstream-private-signature")
	response := serveCacheLink(cache, link)
	require.Equal(t, 200, response.Code)
	require.Equal(t, cacheTestPNG, response.Body.Bytes())
	require.Equal(t, "image/png", response.Header().Get("Content-Type"))
	for i := 0; i < 3; i++ {
		require.Equal(t, cacheTestPNG, serveCacheLink(cache, link).Body.Bytes())
	}
	require.EqualValues(t, 1, reads.Load())
	require.Zero(t, mutations.Load())
	stored, err := os.ReadFile(filepath.Join(cache.dir, cache.id(raw)+".image"))
	require.NoError(t, err)
	require.False(t, bytes.Contains(stored, cacheTestPNG))
	require.False(t, bytes.Contains(stored, []byte(raw)))
	cache.close()
	restarted, err := newImageResultCache(cache.dir, cache.baseURL, cache.key)
	require.NoError(t, err)
	t.Cleanup(restarted.close)
	require.Equal(t, cacheTestPNG, serveCacheLink(restarted, link).Body.Bytes())
	require.EqualValues(t, 1, reads.Load())
}

func TestImageResultCacheConcurrentReadsCoalesceAndDetachFromCaller(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write(cacheTestPNG)
	})
	link, err := cache.link(raw)
	require.NoError(t, err)
	<-started
	u, _ := url.Parse(link)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	cache.serve(w, httptest.NewRequest("GET", link, nil).WithContext(ctx), strings.TrimPrefix(u.Path, imageResultPath))
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.Equal(t, cacheTestPNG, serveCacheLink(cache, link).Body.Bytes())
		}()
	}
	close(release)
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
}

func TestImageResultCacheRejectsTamperingExpiryAndSupportsRange(t *testing.T) {
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cacheTestPNG) })
	link, err := cache.link(raw)
	require.NoError(t, err)
	require.Equal(t, 200, serveCacheLink(cache, link).Code)
	require.Equal(t, 404, serveCacheLink(cache, link+"x").Code)
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(-time.Second).Unix()))
	payload = append(payload, raw...)
	sealed, err := cache.seal(payload, "image-download-ticket")
	require.NoError(t, err)
	require.Equal(t, 410, serveCacheLink(cache, cache.baseURL+imageResultPath+base64.RawURLEncoding.EncodeToString(sealed)).Code)
	u, _ := url.Parse(link)
	req := httptest.NewRequest("GET", link+"?download=1", nil)
	req.Header.Set("Range", "bytes=0-7")
	w := httptest.NewRecorder()
	cache.serve(w, req, strings.TrimPrefix(u.Path, imageResultPath))
	require.Equal(t, 206, w.Code)
	require.Equal(t, cacheTestPNG[:8], w.Body.Bytes())
	require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
}

func TestImageResultCacheDownloadFailureNeverPublishesPartialOrReplaysGeneration(t *testing.T) {
	for _, mode := range []string{"truncated", "png_header_only", "html", "too_large", "capacity", "unavailable", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				calls.Add(1)
				switch mode {
				case "timeout":
					<-r.Context().Done()
					return
				case "truncated":
					w.Header().Set("Content-Length", "900")
				case "html":
					_, _ = io.WriteString(w, "<html>not an image</html>")
					return
				case "png_header_only":
					_, _ = w.Write(cacheTestPNG[:40])
					return
				case "unavailable":
					w.WriteHeader(503)
					return
				}
				_, _ = w.Write(cacheTestPNG)
			})
			if mode == "too_large" {
				cache.maxBytes = 16
			}
			if mode == "capacity" {
				cache.maxDiskBytes = 16
			}
			if mode == "timeout" {
				cache.client.Timeout = 20 * time.Millisecond
			}
			link, err := cache.link(raw)
			require.NoError(t, err)
			response := serveCacheLink(cache, link)
			require.Equal(t, 503, response.Code)
			require.Contains(t, response.Body.String(), "do_not_regenerate")
			before := calls.Load()
			require.Equal(t, 503, serveCacheLink(cache, link).Code)
			require.Equal(t, before, calls.Load(), "失败冷却期不重复请求上游")
			require.LessOrEqual(t, calls.Load(), int32(2))
			files, err := os.ReadDir(cache.dir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

func TestImageResultRewriteLeavesBillingAndNonImageURLsUnchanged(t *testing.T) {
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cacheTestPNG) })
	require.Nil(t, imageResultCache.Swap(cache))
	t.Cleanup(func() { imageResultCache.Store(nil) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	cases := []struct{ payload, imagePath string }{
		{`{"created":123,"data":[{"url":"` + raw + `","revised_prompt":"keep"},{"b64_json":"unchanged"}],"usage":{"total_tokens":50}}`, "data.0.url"},
		{`{"type":"image_generation.completed","url":"` + raw + `","image_index":0,"usage":{"total_tokens":50}}`, "url"},
		{`{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"completed","result":"` + raw + `"}}`, "item.result"},
		{`{"output":[{"type":"image_generation_call","result":"` + raw + `"},{"type":"message","url":"https://leave.example"}]}`, "output.0.result"},
		{`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":{"url":"` + raw + `"}}]}}`, "response.output.0.result.url"},
	}
	for _, tc := range cases {
		before := []byte(tc.payload)
		after := RewriteImageResultURLs(c, before)
		link := gjson.GetBytes(after, tc.imagePath).String()
		require.True(t, strings.HasPrefix(link, cache.baseURL+imageResultPath), string(after))
		require.Equal(t, gjson.GetBytes(before, "usage").Raw, gjson.GetBytes(after, "usage").Raw)
		require.Equal(t, gjson.GetBytes(before, "data.1").Raw, gjson.GetBytes(after, "data.1").Raw)
		require.Equal(t, gjson.GetBytes(before, "output.1").Raw, gjson.GetBytes(after, "output.1").Raw)
		require.Equal(t, after, RewriteImageResultURLs(c, after), "不二次包装本站地址")
		require.Equal(t, cacheTestPNG, serveCacheLink(cache, link).Body.Bytes())
	}
	partial := []byte(`{"type":"image_generation.partial_image","url":"` + raw + `"}`)
	require.Equal(t, partial, RewriteImageResultURLs(c, partial))
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	text := []byte(`{"data":[{"url":"` + raw + `"}]}`)
	require.Equal(t, text, RewriteImageResultURLs(c, text))
}

func TestImageResultCacheProductionClientBlocksPrivateDestinations(t *testing.T) {
	client := newImageResultHTTPClient()
	defer client.CloseIdleConnections()
	for _, target := range []string{"http://127.0.0.1/a.png", "http://169.254.169.254/latest/meta-data", "http://[::1]/a.png"} {
		_, err := client.Get(target)
		require.Error(t, err)
	}
	for _, target := range []string{"file:///tmp/image", "http://user:password@example.com/image", "https://example.com/image#secret"} {
		require.False(t, validImageResultURL(target))
	}
}

func TestImageResultCacheWarmsQueuedImagesWithoutClientReads(t *testing.T) {
	started := make(chan struct{}, 15)
	release := make(chan struct{})
	var calls atomic.Int32
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		_, _ = w.Write(cacheTestPNG)
	})
	for i := 0; i < 15; i++ {
		_, err := cache.link(raw + "&index=" + string(rune('a'+i)))
		require.NoError(t, err)
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	require.EqualValues(t, 4, calls.Load())
	close(release)
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return len(cache.flights) == 0
	}, 5*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 15, calls.Load())
}

func TestImageResultCacheLongSignedURLAndExpiredFileCleanup(t *testing.T) {
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cacheTestPNG) })
	longURL := raw + "&x=" + strings.Repeat("&", 3500)
	link, err := cache.link(longURL)
	require.NoError(t, err)
	require.Less(t, len(link), 6144)
	require.Equal(t, 200, serveCacheLink(cache, link).Code)
	file := filepath.Join(cache.dir, cache.id(longURL)+".image")
	orphan := filepath.Join(cache.dir, ".image-tmp-test")
	unrelated := filepath.Join(cache.dir, "unrelated-file")
	require.NoError(t, os.WriteFile(orphan, []byte("old temporary ciphertext"), 0600))
	require.NoError(t, os.WriteFile(unrelated, []byte("leave alone"), 0600))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(file, past, past))
	require.NoError(t, os.Chtimes(orphan, past, past))
	cache.cleanup()
	_, err = os.Stat(file)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(orphan)
	require.True(t, os.IsNotExist(err))
	require.FileExists(t, unrelated)
	require.Zero(t, cache.usedBytes)
}

func TestImageResultCacheCorruptionOnlyRefetchesOriginal(t *testing.T) {
	var calls atomic.Int32
	cache, raw := testImageResultCache(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		calls.Add(1)
		_, _ = w.Write(cacheTestPNG)
	})
	link, err := cache.link(raw)
	require.NoError(t, err)
	require.Equal(t, 200, serveCacheLink(cache, link).Code)
	file := filepath.Join(cache.dir, cache.id(raw)+".image")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	data[len(data)-1] ^= 1
	require.NoError(t, os.WriteFile(file, data, 0600))
	require.Equal(t, 503, serveCacheLink(cache, link).Code)
	require.Equal(t, 200, serveCacheLink(cache, link).Code)
	require.EqualValues(t, 2, calls.Load())
}
