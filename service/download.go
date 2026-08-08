package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

// WorkerRequest Worker请求的数据结构
type WorkerRequest struct {
	URL     string            `json:"url"`
	Key     string            `json:"key"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type downloadRequestExecutor func(client *http.Client, req *http.Request) (*http.Response, error)

func executeDownloadRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(req)
}

func doWorkerRequestContext(ctx context.Context, req *WorkerRequest, execute downloadRequestExecutor) (*http.Response, error) {
	if !system_setting.EnableWorker() {
		return nil, fmt.Errorf("worker not enabled")
	}
	if !system_setting.WorkerAllowHttpImageRequestEnabled && !strings.HasPrefix(req.URL, "https") {
		return nil, fmt.Errorf("only support https url")
	}

	// SSRF防护：验证请求URL
	fetchSetting := system_setting.GetFetchSetting()
	if err := common.ValidateURLWithFetchSetting(req.URL, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, fetchSetting.ApplyIPFilterForDomain); err != nil {
		return nil, fmt.Errorf("request reject: %v", err)
	}

	workerUrl := system_setting.WorkerUrl
	if !strings.HasSuffix(workerUrl, "/") {
		workerUrl += "/"
	}

	// 序列化worker请求数据
	workerPayload, err := common.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal worker payload: %v", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, workerUrl, bytes.NewReader(workerPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to create worker request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return execute(GetHttpClient(), request)
}

// DoWorkerRequestContext sends a worker request bound to the supplied caller context.
func DoWorkerRequestContext(ctx context.Context, req *WorkerRequest) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return doWorkerRequestContext(ctx, req, executeDownloadRequest)
}

// DoWorkerRequest sends a worker request for compatibility with maintenance callers.
func DoWorkerRequest(req *WorkerRequest) (*http.Response, error) {
	return DoWorkerRequestContext(context.Background(), req)
}

func doDownloadRequestContext(ctx context.Context, originURL string, execute downloadRequestExecutor, reason ...string) (resp *http.Response, err error) {
	if system_setting.EnableWorker() {
		common.SysLog(fmt.Sprintf("downloading file from worker: %s, reason: %s", originURL, strings.Join(reason, ", ")))
		req := &WorkerRequest{
			URL: originURL,
			Key: system_setting.WorkerValidKey,
		}
		return doWorkerRequestContext(ctx, req, execute)
	} else {
		// SSRF防护：验证请求URL（非Worker模式）
		if err := ValidateSSRFProtectedFetchURL(originURL); err != nil {
			return nil, fmt.Errorf("request reject: %v", err)
		}

		common.SysLog(fmt.Sprintf("downloading from origin: %s, reason: %s", common.MaskSensitiveInfo(originURL), strings.Join(reason, ", ")))
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create download request: %w", err)
		}
		return execute(GetSSRFProtectedHTTPClient(), request)
	}
}

// DoDownloadRequestContext downloads a URL using the caller's context. It does
// not add a relay-local deadline because generic callers do not carry relay
// stream/non-stream metadata.
func DoDownloadRequestContext(ctx context.Context, originURL string, reason ...string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return doDownloadRequestContext(ctx, originURL, executeDownloadRequest, reason...)
}

// DoDownloadRequest remains a Background-based compatibility wrapper for
// maintenance and notification callers outside an inbound relay request.
func DoDownloadRequest(originURL string, reason ...string) (*http.Response, error) {
	return DoDownloadRequestContext(context.Background(), originURL, reason...)
}

type relayDownloadBody struct {
	io.ReadCloser
	preflight *RelayPreflight
	closeOnce sync.Once
}

func (b *relayDownloadBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	return n, b.preflight.ClassifyError(err)
}

func (b *relayDownloadBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeOnce.Do(b.preflight.Close)
	if err == nil {
		return nil
	}
	return b.preflight.ClassifyError(err)
}

func relayDownloadInfo(c *gin.Context, info *relaycommon.RelayInfo) *relaycommon.RelayInfo {
	if info != nil || c == nil {
		return info
	}
	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	if startTime.IsZero() {
		startTime = time.Now()
		common.SetContextKey(c, constant.ContextKeyRequestStartTime, startTime)
	}
	return &relaycommon.RelayInfo{
		StartTime: startTime,
		IsStream:  common.GetContextKeyBool(c, constant.ContextKeyIsStream),
	}
}

// DoDownloadRequestWithRelayInfo binds request headers and body reads to the
// inbound caller and to the same absolute relay budget used by provider work.
// A nil info derives stream mode and the original request start from Gin, so
// multiple downloads still converge on one absolute deadline instead of each
// receiving a fresh timeout window.
func DoDownloadRequestWithRelayInfo(c *gin.Context, info *relaycommon.RelayInfo, originURL string, reason ...string) (*http.Response, error) {
	if c == nil && info == nil {
		return DoDownloadRequest(originURL, reason...)
	}
	caller := context.Background()
	if c != nil && c.Request != nil {
		caller = c.Request.Context()
	}
	preflight := NewRelayPreflightWithChannelHealth(c, caller, relayDownloadInfo(c, info), false, false)
	resp, err := doDownloadRequestContext(preflight.Context(), originURL, preflight.Do, reason...)
	if err != nil {
		preflight.Close()
		return nil, err
	}
	if resp.Body == nil {
		preflight.Close()
		return resp, nil
	}
	resp.Body = &relayDownloadBody{ReadCloser: resp.Body, preflight: preflight}
	return resp, nil
}
