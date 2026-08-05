package controller

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

const backgroundRelayHeader = "X-Oneapi-Background"

var backgroundRelaySubmitMu sync.Mutex

type backgroundRelayExecution struct {
	Task        *model.Task
	RelayFormat types.RelayFormat
	Request     *http.Request
	Params      gin.Params
	Keys        map[string]any
	Body        []byte
	ReleaseSlot func()
}

type backgroundRelayStoredResult struct {
	HTTPStatus      int               `json:"http_status"`
	ContentType     string            `json:"content_type,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	BodyBase64      string            `json:"body_base64,omitempty"`
	BodySize        int64             `json:"body_size"`
	ResultPersisted bool              `json:"result_persisted"`
	StopReason      string            `json:"stop_reason,omitempty"`
}

type backgroundRelayHTTPWriter struct {
	job         *service.BackgroundRelayJob
	header      http.Header
	statusCode  int
	writeErr    error
	closeNotify chan bool
	mu          sync.Mutex
}

func newBackgroundRelayHTTPWriter(job *service.BackgroundRelayJob) *backgroundRelayHTTPWriter {
	return &backgroundRelayHTTPWriter{
		job:         job,
		header:      make(http.Header),
		closeNotify: make(chan bool),
	}
}

func (w *backgroundRelayHTTPWriter) Header() http.Header { return w.header }

func (w *backgroundRelayHTTPWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	if w.statusCode == 0 {
		w.statusCode = statusCode
		w.job.SetResponseMetadata(statusCode, w.header.Get("Content-Type"), safeBackgroundResponseHeaders(w.header))
	}
	w.mu.Unlock()
}

func (w *backgroundRelayHTTPWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
		w.job.SetResponseMetadata(w.statusCode, w.header.Get("Content-Type"), safeBackgroundResponseHeaders(w.header))
	}
	if w.writeErr != nil {
		err := w.writeErr
		w.mu.Unlock()
		return 0, err
	}
	w.mu.Unlock()
	n, err := w.job.Append(data)
	if err != nil {
		w.mu.Lock()
		w.writeErr = err
		w.mu.Unlock()
	}
	return n, err
}

func (w *backgroundRelayHTTPWriter) Flush() {}

func (w *backgroundRelayHTTPWriter) CloseNotify() <-chan bool { return w.closeNotify }

func (w *backgroundRelayHTTPWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("background relay does not support connection hijacking")
}

func (w *backgroundRelayHTTPWriter) status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *backgroundRelayHTTPWriter) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr
}

func RelayDispatch(c *gin.Context, relayFormat types.RelayFormat) {
	if backgroundRelayRequested(c) {
		RelayBackground(c, relayFormat)
		return
	}
	Relay(c, relayFormat)
}

func backgroundRelayRequested(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value := strings.ToLower(strings.TrimSpace(c.GetHeader(backgroundRelayHeader)))
	return value == "1" || value == "true" || value == "yes"
}

func RelayBackground(c *gin.Context, relayFormat types.RelayFormat) {
	if relayFormat == types.RelayFormatOpenAIRealtime || relayFormat == types.RelayFormatTask || relayFormat == types.RelayFormatMjProxy {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "background relay is not supported for this protocol", "type": "invalid_request_error"}})
		return
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		newAPIError := newRequestBodyFailure(c, err)
		c.JSON(newAPIError.StatusCode, gin.H{"error": newAPIError.ToOpenAIError()})
		return
	}
	body, err := storage.Bytes()
	if err != nil {
		newAPIError := newRequestBodyFailure(c, err)
		c.JSON(newAPIError.StatusCode, gin.H{"error": newAPIError.ToOpenAIError()})
		return
	}
	body = append([]byte(nil), body...)
	requestFingerprint := backgroundRelayRequestFingerprint(c.Request, relayFormat, body)
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Idempotency-Key is too long", "type": "invalid_request_error"}})
		return
	}
	taskID := backgroundRelayTaskID(c.GetInt("id"), c.GetInt("token_id"), idempotencyKey)

	backgroundRelaySubmitMu.Lock()
	defer backgroundRelaySubmitMu.Unlock()
	if existing, exists, lookupErr := model.GetByTaskId(c.GetInt("id"), taskID); lookupErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to look up background relay job", "type": "server_error"}})
		return
	} else if exists {
		if existing.Properties.Input != requestFingerprint {
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"message": "Idempotency-Key was already used with a different request", "type": "idempotency_conflict"}})
			return
		}
		service.ReleaseChannelCircuitProbe(c.Request.Context(), c.GetInt("channel_id"), c.GetString("original_model"))
		c.Set("background_relay_submitted", true)
		respondBackgroundRelayAccepted(c, existing)
		return
	}
	releaseSlot, acquired := service.TryAcquireBackgroundRelaySlot(c.GetInt("token_id"))
	if !acquired {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "too many concurrent background relay jobs", "type": "rate_limit_error"}})
		return
	}

	now := time.Now().Unix()
	task := &model.Task{
		CreatedAt:  now,
		UpdatedAt:  now,
		TaskID:     taskID,
		Platform:   constant.TaskPlatformBackgroundRelay,
		UserId:     c.GetInt("id"),
		Group:      common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
		ChannelId:  c.GetInt("channel_id"),
		Action:     string(relayFormat),
		Status:     model.TaskStatusQueued,
		SubmitTime: now,
		Progress:   "0%",
		Properties: model.Properties{
			Input:           requestFingerprint,
			OriginModelName: c.GetString("original_model"),
		},
		PrivateData: model.TaskPrivateData{TokenId: c.GetInt("token_id")},
	}
	if err := task.Insert(); err != nil {
		releaseSlot()
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to create background relay job", "type": "server_error"}})
		return
	}

	maxResultBytes := int64(common.GetEnvOrDefault("BACKGROUND_RELAY_MAX_RESULT_MB", 128)) << 20
	job := service.NewBackgroundRelayJob(task.TaskID, maxResultBytes)
	requestCopy := c.Request.Clone(context.Background())
	requestCopy.Header = c.Request.Header.Clone()
	requestCopy.Header.Del(backgroundRelayHeader)
	contextCopy := c.Copy()
	keys := make(map[string]any, len(contextCopy.Keys))
	for key, value := range contextCopy.Keys {
		if key == common.KeyBodyStorage || key == common.KeyRequestBody {
			continue
		}
		keys[key] = value
	}
	execution := backgroundRelayExecution{
		Task:        task,
		RelayFormat: relayFormat,
		Request:     requestCopy,
		Params:      append(gin.Params(nil), c.Params...),
		Keys:        keys,
		Body:        body,
		ReleaseSlot: releaseSlot,
	}
	c.Set("background_relay_submitted", true)
	respondBackgroundRelayAccepted(c, task)
	gopool.Go(func() { runBackgroundRelayJob(job, execution) })
}

func backgroundRelayTaskID(userID, tokenID int, idempotencyKey string) string {
	if idempotencyKey == "" {
		return model.GenerateTaskID()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", userID, tokenID, idempotencyKey)))
	return "task_bg_" + hex.EncodeToString(sum[:16])
}

func backgroundRelayRequestFingerprint(request *http.Request, relayFormat types.RelayFormat, body []byte) string {
	method := ""
	requestURI := ""
	if request != nil {
		method = request.Method
		if request.URL != nil {
			requestURI = request.URL.RequestURI()
		}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(method))
	_, _ = hash.Write([]byte{'\n'})
	_, _ = hash.Write([]byte(requestURI))
	_, _ = hash.Write([]byte{'\n'})
	_, _ = hash.Write([]byte(relayFormat))
	_, _ = hash.Write([]byte{'\n'})
	_, _ = hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func respondBackgroundRelayAccepted(c *gin.Context, task *model.Task) {
	statusURL := "/v1/background/" + task.TaskID
	c.Header("Location", statusURL)
	c.JSON(http.StatusAccepted, gin.H{
		"id":         task.TaskID,
		"object":     "background_relay.job",
		"status":     backgroundRelayPublicStatus(task.Status),
		"status_url": statusURL,
		"result_url": statusURL + "?raw=1",
	})
}

func runBackgroundRelayJob(job *service.BackgroundRelayJob, execution backgroundRelayExecution) {
	if execution.ReleaseSlot != nil {
		defer execution.ReleaseSlot()
	}
	finished := false
	defer func() {
		if recovered := recover(); recovered != nil && !finished {
			finishBackgroundRelayJob(job, execution.Task, http.StatusInternalServerError, "application/json", nil, "background_job_panic", false)
			logger.LogError(context.Background(), fmt.Sprintf("background relay job %s panicked: %v", execution.Task.TaskID, recovered))
		}
	}()
	job.Start()
	task := execution.Task
	now := time.Now().Unix()
	task.Status = model.TaskStatusInProgress
	task.StartTime = now
	task.UpdatedAt = now
	task.Progress = "1%"
	if err := task.Update(); err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("failed to mark background relay job %s in progress: %v", task.TaskID, err))
	}

	timeoutSeconds := common.GetEnvOrDefault("BACKGROUND_RELAY_JOB_TIMEOUT_SECONDS", 900)
	if execution.RelayFormat != types.RelayFormatOpenAIImage {
		timeoutSeconds = common.GetEnvOrDefault("BACKGROUND_RELAY_TEXT_JOB_TIMEOUT_SECONDS", 300)
	}
	jobCtxBase, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	jobCtxBase = context.WithValue(jobCtxBase, common.RequestIdKey, task.TaskID)
	jobCtxBase = context.WithValue(jobCtxBase, common.InternalRequestIdKey, task.TaskID)
	execution.Request = execution.Request.WithContext(jobCtxBase)

	bodyStorage, storageErr := common.CreateBodyStorage(execution.Body)
	if storageErr != nil {
		finishBackgroundRelayJob(job, task, 0, "", nil, storageErr.Error(), false)
		finished = true
		return
	}
	defer bodyStorage.Close()
	execution.Request.Body = io.NopCloser(bodyStorage)
	execution.Request.ContentLength = int64(len(execution.Body))

	writer := newBackgroundRelayHTTPWriter(job)
	jobCtx, _ := gin.CreateTestContext(writer)
	jobCtx.Request = execution.Request
	jobCtx.Params = execution.Params
	jobCtx.Keys = execution.Keys
	jobCtx.Set(common.KeyBodyStorage, bodyStorage)
	jobCtx.Set(common.RequestIdKey, task.TaskID)
	jobCtx.Set(common.InternalRequestIdKey, task.TaskID)
	common.SetContextKey(jobCtx, constant.ContextKeyRequestStartTime, time.Now())

	Relay(jobCtx, execution.RelayFormat)
	statusCode := writer.status()
	contentType := writer.Header().Get("Content-Type")
	headers := safeBackgroundResponseHeaders(writer.Header())
	stopReason := jobCtx.GetString("retry_stop_reason")
	writeErr := writer.err()
	succeeded := statusCode >= 200 && statusCode < 300 && stopReason == service.RetryStopReasonSuccess && writeErr == nil
	if writeErr != nil {
		stopReason = "result_write_failed"
	}
	task.ChannelId = jobCtx.GetInt("channel_id")
	task.Group = common.GetContextKeyString(jobCtx, constant.ContextKeyUsingGroup)
	if succeeded && task.ChannelId > 0 {
		service.RecordChannelAffinity(jobCtx, task.ChannelId)
	}
	finishBackgroundRelayJob(job, task, statusCode, contentType, headers, stopReason, succeeded)
	finished = true
}

func finishBackgroundRelayJob(job *service.BackgroundRelayJob, task *model.Task, statusCode int, contentType string, headers map[string]string, stopReason string, succeeded bool) {
	status := "failed"
	task.Status = model.TaskStatusFailure
	if succeeded {
		status = "succeeded"
		task.Status = model.TaskStatusSuccess
	}
	job.Complete(status, statusCode, contentType, headers, stopReason)
	snapshot := job.SnapshotAll()
	persistLimit := int64(common.GetEnvOrDefault("BACKGROUND_RELAY_PERSIST_MAX_RESULT_MB", 8)) << 20
	stored := backgroundRelayStoredResult{
		HTTPStatus:  snapshot.HTTPStatus,
		ContentType: snapshot.ContentType,
		Headers:     snapshot.Headers,
		BodySize:    snapshot.TotalSize,
		StopReason:  snapshot.StopReason,
	}
	if snapshot.TotalSize <= persistLimit {
		stored.BodyBase64 = base64.StdEncoding.EncodeToString(snapshot.Data)
		stored.ResultPersisted = true
	}
	storedBytes, marshalErr := common.Marshal(stored)
	if marshalErr == nil {
		task.Data = storedBytes
	}
	now := time.Now().Unix()
	task.UpdatedAt = now
	task.FinishTime = now
	task.Progress = "100%"
	if !succeeded {
		task.FailReason = stopReason
		if task.FailReason == "" {
			task.FailReason = fmt.Sprintf("background relay failed with HTTP %d", statusCode)
		}
	}
	if err := task.Update(); err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("failed to persist background relay job %s: %v", task.TaskID, err))
	}
	ttl := time.Duration(common.GetEnvOrDefault("BACKGROUND_RELAY_RESULT_TTL_MINUTES", 60)) * time.Minute
	time.AfterFunc(ttl, func() { service.DeleteBackgroundRelayJob(task.TaskID) })
}

func GetBackgroundRelayJob(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("id"))
	task, exists, err := model.GetByTaskId(c.GetInt("id"), taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to load background relay job", "type": "server_error"}})
		return
	}
	if !exists || task.Platform != constant.TaskPlatformBackgroundRelay || task.PrivateData.TokenId != c.GetInt("token_id") {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "background relay job not found", "type": "not_found"}})
		return
	}
	offset, _ := strconv.ParseInt(c.DefaultQuery("offset", "0"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(1<<20)))
	if limit <= 0 || limit > 4<<20 {
		limit = 1 << 20
	}
	waitSeconds, _ := strconv.Atoi(c.DefaultQuery("wait", "0"))
	if waitSeconds > 25 {
		waitSeconds = 25
	}
	raw := c.Query("raw") == "1" || strings.EqualFold(c.Query("raw"), "true")

	if job, active := service.GetBackgroundRelayJob(taskID); active {
		snapshot := job.Snapshot(offset, limit)
		if waitSeconds > 0 && snapshot.NextOffset <= offset && !snapshot.Done {
			snapshot = job.WaitForUpdate(c.Request.Context(), offset, time.Duration(waitSeconds)*time.Second)
			if len(snapshot.Data) > limit {
				snapshot = job.Snapshot(offset, limit)
			}
		}
		respondBackgroundRelaySnapshot(c, snapshot, raw)
		return
	}

	var stored backgroundRelayStoredResult
	if len(task.Data) > 0 {
		_ = common.Unmarshal(task.Data, &stored)
	}
	body := []byte(nil)
	if stored.ResultPersisted && stored.BodyBase64 != "" {
		body, _ = base64.StdEncoding.DecodeString(stored.BodyBase64)
	}
	total := int64(len(body))
	if stored.BodySize > total {
		total = stored.BodySize
	}
	if offset > int64(len(body)) {
		offset = int64(len(body))
	}
	end := offset + int64(limit)
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	snapshot := service.BackgroundRelaySnapshot{
		TaskID:      task.TaskID,
		Status:      backgroundRelayPublicStatus(task.Status),
		HTTPStatus:  stored.HTTPStatus,
		ContentType: stored.ContentType,
		Headers:     stored.Headers,
		Offset:      offset,
		NextOffset:  end,
		TotalSize:   total,
		Data:        append([]byte(nil), body[offset:end]...),
		Done:        task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure,
		Available:   len(task.Data) > 0 && (stored.ResultPersisted || stored.BodySize == 0),
		StopReason:  stored.StopReason,
	}
	respondBackgroundRelaySnapshot(c, snapshot, raw)
}

func respondBackgroundRelaySnapshot(c *gin.Context, snapshot service.BackgroundRelaySnapshot, raw bool) {
	if raw {
		for key, value := range snapshot.Headers {
			c.Header(key, value)
		}
		if snapshot.ContentType != "" {
			c.Header("Content-Type", snapshot.ContentType)
		}
		c.Header("X-Oneapi-Job-Status", snapshot.Status)
		c.Header("X-Oneapi-Job-Offset", strconv.FormatInt(snapshot.Offset, 10))
		c.Header("X-Oneapi-Job-Next-Offset", strconv.FormatInt(snapshot.NextOffset, 10))
		c.Header("X-Oneapi-Job-Done", strconv.FormatBool(snapshot.Done))
		c.Header("X-Oneapi-Job-Result-Available", strconv.FormatBool(snapshot.Available))
		if snapshot.Done && !snapshot.Available {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusGone, gin.H{"error": gin.H{"message": "background relay result is no longer available", "type": "result_unavailable"}})
			return
		}
		statusCode := snapshot.HTTPStatus
		if statusCode <= 0 {
			statusCode = http.StatusOK
		}
		c.Data(statusCode, snapshot.ContentType, snapshot.Data)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":               snapshot.TaskID,
		"object":           "background_relay.job",
		"status":           snapshot.Status,
		"http_status":      snapshot.HTTPStatus,
		"content_type":     snapshot.ContentType,
		"offset":           snapshot.Offset,
		"next_offset":      snapshot.NextOffset,
		"total_size":       snapshot.TotalSize,
		"data_base64":      base64.StdEncoding.EncodeToString(snapshot.Data),
		"done":             snapshot.Done,
		"result_available": snapshot.Available,
		"stop_reason":      snapshot.StopReason,
	})
}

func backgroundRelayPublicStatus(status model.TaskStatus) string {
	switch status {
	case model.TaskStatusSuccess:
		return "succeeded"
	case model.TaskStatusFailure:
		return "failed"
	case model.TaskStatusInProgress:
		return "in_progress"
	default:
		return "queued"
	}
}

func safeBackgroundResponseHeaders(headers http.Header) map[string]string {
	allowed := map[string]struct{}{
		"Content-Type":              {},
		"Cache-Control":             {},
		common.RequestIdKey:         {},
		common.UpstreamRequestIdKey: {},
		"OpenAI-Request-ID":         {},
		"Request-ID":                {},
		"X-Request-ID":              {},
	}
	result := make(map[string]string)
	for key := range allowed {
		if value := headers.Get(key); value != "" {
			result[key] = value
		}
	}
	return result
}
