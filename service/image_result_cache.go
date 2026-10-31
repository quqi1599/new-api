package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const imageResultPath = "/v1/image-results/"

var imageResultCache atomic.Pointer[ImageResultCache]

type imageResultFlight struct {
	done    chan struct{}
	retryAt time.Time
}

// ImageResultCache 只接受上游结果生成的加密凭证，不提供任意 URL 下载接口。
type ImageResultCache struct {
	dir, baseURL string
	aead         cipher.AEAD
	key          []byte
	ttl          time.Duration
	maxBytes     int64
	maxDiskBytes int64
	client       *http.Client
	slots        chan struct{}
	readSlots    chan struct{}
	mu           sync.Mutex
	flights      map[string]*imageResultFlight
	usedBytes    int64
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// InitImageResultCacheFromEnv 仅在显式配置本站地址时启用；密钥必须跨重启保留。
func InitImageResultCacheFromEnv() error {
	base := strings.TrimSpace(os.Getenv("IMAGE_RESULT_CACHE_PUBLIC_BASE_URL"))
	if base == "" {
		return nil
	}
	keyBytes, err := os.ReadFile(os.Getenv("IMAGE_RESULT_CACHE_KEY_FILE"))
	if err != nil {
		return errors.New("image result cache key file is unavailable")
	}
	keyText := strings.TrimSpace(string(keyBytes))
	key, err := hex.DecodeString(keyText)
	if err != nil || len(key) != 32 {
		key, err = base64.StdEncoding.DecodeString(keyText)
	}
	if err != nil || len(key) != 32 {
		return errors.New("image result cache key must contain 32 bytes")
	}
	dir := os.Getenv("IMAGE_RESULT_CACHE_DIR")
	if dir == "" {
		return errors.New("IMAGE_RESULT_CACHE_DIR must name a persistent dedicated directory")
	}
	cache, err := newImageResultCache(dir, base, key)
	if err != nil {
		return err
	}
	if previous := imageResultCache.Swap(cache); previous != nil {
		previous.close()
	}
	return nil
}

func CloseImageResultCache() {
	if cache := imageResultCache.Swap(nil); cache != nil {
		cache.close()
	}
}

func newImageResultCache(dir, base string, key []byte) (*ImageResultCache, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("image result cache public base must be an HTTPS origin")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cache := &ImageResultCache{
		dir: dir, baseURL: strings.TrimRight(base, "/"), key: append([]byte(nil), key...), aead: aead,
		ttl: 24 * time.Hour, maxBytes: 32 << 20, maxDiskBytes: 10 << 30,
		slots: make(chan struct{}, 4), readSlots: make(chan struct{}, 4), flights: make(map[string]*imageResultFlight),
		ctx: ctx, cancel: cancel, client: newImageResultHTTPClient(),
	}
	cache.cleanup()
	cache.wg.Add(1)
	go func() {
		defer cache.wg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cache.cleanup()
			}
		}
	}()
	return cache, nil
}

func validImageResultURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 4096 && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func newImageResultHTTPClient() *http.Client {
	// 沿用现有域名/IP 规则，但原图缓存始终禁止内网，并在拨号时固定已核验的 IP。
	dialer := &protectedFetchDialer{
		resolver:    net.DefaultResolver,
		dialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) {
			protection, enabled, err := currentFetchProtection()
			if err != nil {
				return nil, true, err
			}
			if !enabled {
				protection = &common.SSRFProtection{}
			}
			copy := *protection
			copy.AllowPrivateIp, copy.ApplyIPFilterForDomain = false, true
			if len(copy.AllowedPorts) == 0 {
				copy.AllowedPorts = []int{80, 443}
			}
			return &copy, true, nil
		},
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 16, ForceAttemptHTTP2: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || !validImageResultURL(req.URL.String()) {
				return errors.New("invalid image redirect")
			}
			return nil
		},
	}
}

func (cache *ImageResultCache) seal(data []byte, purpose string) ([]byte, error) {
	nonce := make([]byte, cache.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return cache.aead.Seal(nonce, nonce, data, []byte(purpose)), nil
}

func (cache *ImageResultCache) open(data []byte, purpose string) ([]byte, error) {
	if len(data) < cache.aead.NonceSize()+cache.aead.Overhead() {
		return nil, errors.New("invalid encrypted image result")
	}
	return cache.aead.Open(nil, data[:cache.aead.NonceSize()], data[cache.aead.NonceSize():], []byte(purpose))
}

func (cache *ImageResultCache) id(raw string) string {
	h := hmac.New(sha256.New, cache.key)
	h.Write([]byte("image-cache\x00" + raw))
	return hex.EncodeToString(h.Sum(nil))
}

func (cache *ImageResultCache) link(raw string) (string, error) {
	if !validImageResultURL(raw) {
		return "", errors.New("invalid image result URL")
	}
	if strings.HasPrefix(raw, cache.baseURL+imageResultPath) {
		return raw, nil
	}
	// 固定 8 字节期限 + URL，避免 JSON 转义把长签名链接膨胀到代理 URI 上限。
	payload := make([]byte, 8, 8+len(raw))
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(cache.ttl).Unix()))
	payload = append(payload, raw...)
	sealed, err := cache.seal(payload, "image-download-ticket")
	if err != nil {
		return "", err
	}
	cache.start(raw) // 不阻塞生成响应；首次 GET 也会继续读取同一原图。
	return cache.baseURL + imageResultPath + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (cache *ImageResultCache) cached(id string) bool {
	info, err := os.Stat(filepath.Join(cache.dir, id+".image"))
	return err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) < cache.ttl
}

func (cache *ImageResultCache) start(raw string) *imageResultFlight {
	id := cache.id(raw)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.ctx.Err() != nil {
		return nil
	}
	if flight := cache.flights[id]; flight != nil {
		if flight.retryAt.IsZero() || time.Now().Before(flight.retryAt) {
			return flight
		}
		delete(cache.flights, id)
	}
	if cache.cached(id) {
		return nil
	}
	if len(cache.flights) >= 128 {
		for previousID, flight := range cache.flights {
			if !flight.retryAt.IsZero() && time.Now().After(flight.retryAt) {
				delete(cache.flights, previousID)
			}
		}
		if len(cache.flights) >= 128 {
			return nil
		}
	}
	flight := &imageResultFlight{done: make(chan struct{})}
	cache.flights[id] = flight
	cache.wg.Add(1)
	go func() {
		defer cache.wg.Done()
		started := time.Now()
		var err error
		// 有界排队，不因当前 4 个下载槽占满而漏掉下一张已生成的图片。
		select {
		case cache.slots <- struct{}{}:
			err = cache.download(raw, id)
			<-cache.slots
		case <-cache.ctx.Done():
			err = cache.ctx.Err()
		}
		cache.mu.Lock()
		if err == nil {
			delete(cache.flights, id)
		} else {
			flight.retryAt = time.Now().Add(30 * time.Second)
		}
		close(flight.done)
		cache.mu.Unlock()
		outcome := "cached"
		if err != nil {
			outcome = "download_failed"
		}
		// 不记录 URL、下载凭证、上游签名参数或客户内容。
		common.SysLog(fmt.Sprintf("image_result_cache id=%s outcome=%s duration_ms=%d", id[:16], outcome, time.Since(started).Milliseconds()))
	}()
	return flight
}

func (cache *ImageResultCache) download(raw, id string) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if cache.ctx.Err() != nil {
			return cache.ctx.Err()
		}
		body, err := cache.fetch(raw)
		if err == nil {
			return cache.save(id, body)
		}
		lastErr = err
		if attempt == 0 {
			select {
			case <-cache.ctx.Done():
				return cache.ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return lastErr
}

func (cache *ImageResultCache) fetch(raw string) ([]byte, error) {
	if !validImageResultURL(raw) {
		return nil, errors.New("invalid image result URL")
	}
	req, err := http.NewRequestWithContext(cache.ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cache.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > cache.maxBytes {
		return nil, errors.New("image result status or size rejected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cache.maxBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > cache.maxBytes || (resp.ContentLength >= 0 && resp.ContentLength != int64(len(body))) {
		return nil, errors.New("image result body incomplete or too large")
	}
	if imageResultMIME(body) == "" {
		return nil, errors.New("image result is not a supported raster image")
	}
	return body, nil
}

func imageResultMIME(body []byte) string {
	mime := http.DetectContentType(body)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return ""
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 100_000_000 {
		return ""
	}
	if mime == "image/png" && !bytes.HasSuffix(body, []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 174, 66, 96, 130}) {
		return ""
	}
	return mime
}

func (cache *ImageResultCache) save(id string, body []byte) error {
	data, err := cache.seal(body, "image-file:"+id)
	if err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.usedBytes+int64(len(data)) > cache.maxDiskBytes {
		return errors.New("image result cache capacity reached")
	}
	file, err := os.CreateTemp(cache.dir, ".image-tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	target := filepath.Join(cache.dir, id+".image")
	var previousSize int64
	if stat, err := os.Stat(target); err == nil {
		previousSize = stat.Size()
	}
	if err = os.Rename(file.Name(), target); err != nil {
		return err
	}
	cache.usedBytes += int64(len(data)) - previousSize
	return nil
}

func (cache *ImageResultCache) cleanup() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for id, flight := range cache.flights {
		if !flight.retryAt.IsZero() && time.Now().After(flight.retryAt) {
			delete(cache.flights, id)
		}
	}
	files, err := os.ReadDir(cache.dir)
	if err != nil {
		return
	}
	var used int64
	for _, file := range files {
		name := file.Name()
		if strings.HasPrefix(name, ".image-tmp-") {
			info, err := file.Info()
			if err == nil && info.Mode().IsRegular() {
				if time.Since(info.ModTime()) < time.Hour || os.Remove(filepath.Join(cache.dir, name)) != nil {
					used += info.Size()
				}
			}
			continue
		}
		if len(name) != 70 || !strings.HasSuffix(name, ".image") {
			continue
		}
		if _, err := hex.DecodeString(name[:64]); err != nil {
			continue
		}
		info, err := file.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if time.Since(info.ModTime()) >= cache.ttl && cache.flights[name[:64]] == nil {
			if err := os.Remove(filepath.Join(cache.dir, name)); err == nil {
				continue
			}
		}
		used += info.Size()
	}
	cache.usedBytes = used
}

func (cache *ImageResultCache) close() {
	cache.mu.Lock()
	cache.cancel()
	cache.mu.Unlock()
	cache.wg.Wait()
	cache.client.CloseIdleConnections()
}

func (cache *ImageResultCache) discardCorrupt(file *os.File) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	opened, err := file.Stat()
	if err != nil {
		return
	}
	current, err := os.Stat(file.Name())
	// 只删除本次读到的坏文件，避免误删另一请求刚恢复的缓存。
	if err == nil && os.SameFile(opened, current) && os.Remove(file.Name()) == nil {
		cache.usedBytes -= current.Size()
		if cache.usedBytes < 0 {
			cache.usedBytes = 0
		}
	}
}

// ServeImageResult 不走渠道分配、额度检查或生成处理器，重复 GET 不会重新生图。
func ServeImageResult(c *gin.Context) {
	cache := imageResultCache.Load()
	if cache == nil {
		c.Status(http.StatusNotFound)
		return
	}
	cache.serve(c.Writer, c.Request, c.Param("ticket"))
}

func (cache *ImageResultCache) serve(w http.ResponseWriter, req *http.Request, token string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(token) > 6144 {
		http.Error(w, "invalid_image_link", http.StatusNotFound)
		return
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		http.Error(w, "invalid_image_link", http.StatusNotFound)
		return
	}
	payload, err := cache.open(sealed, "image-download-ticket")
	if err != nil || len(payload) < 8 || !validImageResultURL(string(payload[8:])) {
		http.Error(w, "invalid_image_link", http.StatusNotFound)
		return
	}
	origin := string(payload[8:])
	expires := binary.BigEndian.Uint64(payload[:8])
	if uint64(time.Now().Unix()) >= expires {
		http.Error(w, "image_link_expired", http.StatusGone)
		return
	}
	select {
	case cache.readSlots <- struct{}{}:
		defer func() { <-cache.readSlots }()
	default:
		w.Header().Set("Retry-After", "3")
		http.Error(w, "image_download_busy_retry_same_link", http.StatusServiceUnavailable)
		return
	}
	id := cache.id(origin)
	if flight := cache.start(origin); flight != nil {
		select {
		case <-req.Context().Done():
			return
		case <-flight.done:
		}
	}
	if !cache.cached(id) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "image_download_pending_retry_same_link_do_not_regenerate", http.StatusServiceUnavailable)
		return
	}
	if uint64(time.Now().Unix()) >= expires {
		http.Error(w, "image_link_expired", http.StatusGone)
		return
	}
	file, err := os.Open(filepath.Join(cache.dir, id+".image"))
	if err != nil {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "image_download_pending_retry_same_link_do_not_regenerate", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cache.maxBytes+int64(cache.aead.NonceSize()+cache.aead.Overhead())+1))
	if err != nil || int64(len(data)) > cache.maxBytes+int64(cache.aead.NonceSize()+cache.aead.Overhead()) {
		cache.discardCorrupt(file)
		http.Error(w, "image_cache_read_failed", http.StatusServiceUnavailable)
		return
	}
	body, err := cache.open(data, "image-file:"+id)
	if err != nil || imageResultMIME(body) == "" {
		cache.discardCorrupt(file)
		http.Error(w, "image_cache_read_failed", http.StatusServiceUnavailable)
		return
	}
	mime := imageResultMIME(body)
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[mime]
	w.Header().Set("Content-Type", mime)
	disposition := "inline"
	if req.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=\"image.%s\"", disposition, ext))
	http.ServeContent(w, req, "image."+ext, time.Time{}, bytes.NewReader(body))
}
