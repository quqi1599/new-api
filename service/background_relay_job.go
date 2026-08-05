package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

var ErrBackgroundRelayResultTooLarge = errors.New("background relay result exceeds configured limit")

type BackgroundRelaySnapshot struct {
	TaskID      string
	Status      string
	HTTPStatus  int
	ContentType string
	Headers     map[string]string
	Offset      int64
	NextOffset  int64
	TotalSize   int64
	Data        []byte
	Done        bool
	Available   bool
	StopReason  string
}

type BackgroundRelayJob struct {
	mu          sync.Mutex
	taskID      string
	status      string
	httpStatus  int
	contentType string
	headers     map[string]string
	data        []byte
	maxBytes    int64
	done        bool
	stopReason  string
	notify      chan struct{}
}

var backgroundRelayJobs sync.Map

var backgroundRelaySlots = struct {
	sync.Mutex
	global  int
	byToken map[int]int
}{byToken: make(map[int]int)}

func TryAcquireBackgroundRelaySlot(tokenID int) (func(), bool) {
	globalLimit := common.GetEnvOrDefault("BACKGROUND_RELAY_MAX_CONCURRENT", 32)
	perTokenLimit := common.GetEnvOrDefault("BACKGROUND_RELAY_MAX_CONCURRENT_PER_TOKEN", 3)
	backgroundRelaySlots.Lock()
	if globalLimit > 0 && backgroundRelaySlots.global >= globalLimit {
		backgroundRelaySlots.Unlock()
		return nil, false
	}
	if perTokenLimit > 0 && backgroundRelaySlots.byToken[tokenID] >= perTokenLimit {
		backgroundRelaySlots.Unlock()
		return nil, false
	}
	backgroundRelaySlots.global++
	backgroundRelaySlots.byToken[tokenID]++
	backgroundRelaySlots.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			backgroundRelaySlots.Lock()
			if backgroundRelaySlots.global > 0 {
				backgroundRelaySlots.global--
			}
			if backgroundRelaySlots.byToken[tokenID] <= 1 {
				delete(backgroundRelaySlots.byToken, tokenID)
			} else {
				backgroundRelaySlots.byToken[tokenID]--
			}
			backgroundRelaySlots.Unlock()
		})
	}, true
}

func NewBackgroundRelayJob(taskID string, maxBytes int64) *BackgroundRelayJob {
	if maxBytes <= 0 {
		maxBytes = 128 << 20
	}
	job := &BackgroundRelayJob{
		taskID:   taskID,
		status:   "queued",
		headers:  make(map[string]string),
		maxBytes: maxBytes,
		notify:   make(chan struct{}),
	}
	backgroundRelayJobs.Store(taskID, job)
	return job
}

func GetBackgroundRelayJob(taskID string) (*BackgroundRelayJob, bool) {
	value, ok := backgroundRelayJobs.Load(taskID)
	if !ok {
		return nil, false
	}
	job, ok := value.(*BackgroundRelayJob)
	return job, ok && job != nil
}

func DeleteBackgroundRelayJob(taskID string) {
	backgroundRelayJobs.Delete(taskID)
}

func (j *BackgroundRelayJob) Start() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.status = "in_progress"
	j.signalLocked()
	j.mu.Unlock()
}

func (j *BackgroundRelayJob) SetResponseMetadata(statusCode int, contentType string, headers map[string]string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	if statusCode > 0 {
		j.httpStatus = statusCode
	}
	if contentType != "" {
		j.contentType = contentType
	}
	for key, value := range headers {
		j.headers[key] = value
	}
	j.signalLocked()
	j.mu.Unlock()
}

func (j *BackgroundRelayJob) Append(data []byte) (int, error) {
	if j == nil {
		return 0, errors.New("background relay job is nil")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return 0, errors.New("background relay job is already complete")
	}
	if int64(len(j.data))+int64(len(data)) > j.maxBytes {
		return 0, ErrBackgroundRelayResultTooLarge
	}
	j.data = append(j.data, data...)
	j.signalLocked()
	return len(data), nil
}

func (j *BackgroundRelayJob) Complete(status string, statusCode int, contentType string, headers map[string]string, stopReason string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	if status != "" {
		j.status = status
	}
	if statusCode > 0 {
		j.httpStatus = statusCode
	}
	if contentType != "" {
		j.contentType = contentType
	}
	for key, value := range headers {
		j.headers[key] = value
	}
	j.stopReason = stopReason
	j.done = true
	j.signalLocked()
	j.mu.Unlock()
}

func (j *BackgroundRelayJob) Snapshot(offset int64, limit int) BackgroundRelaySnapshot {
	if j == nil {
		return BackgroundRelaySnapshot{}
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 1 << 20
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	total := int64(len(j.data))
	if offset > total {
		offset = total
	}
	end := offset + int64(limit)
	if end > total {
		end = total
	}
	data := append([]byte(nil), j.data[offset:end]...)
	headers := make(map[string]string, len(j.headers))
	for key, value := range j.headers {
		headers[key] = value
	}
	return BackgroundRelaySnapshot{
		TaskID:      j.taskID,
		Status:      j.status,
		HTTPStatus:  j.httpStatus,
		ContentType: j.contentType,
		Headers:     headers,
		Offset:      offset,
		NextOffset:  end,
		TotalSize:   total,
		Data:        data,
		Done:        j.done,
		Available:   true,
		StopReason:  j.stopReason,
	}
}

func (j *BackgroundRelayJob) SnapshotAll() BackgroundRelaySnapshot {
	if j == nil {
		return BackgroundRelaySnapshot{}
	}
	j.mu.Lock()
	limit := len(j.data)
	j.mu.Unlock()
	return j.Snapshot(0, limit)
}

func (j *BackgroundRelayJob) WaitForUpdate(ctx context.Context, offset int64, timeout time.Duration) BackgroundRelaySnapshot {
	if j == nil {
		return BackgroundRelaySnapshot{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	for {
		j.mu.Lock()
		if int64(len(j.data)) > offset || j.done {
			j.mu.Unlock()
			return j.Snapshot(offset, 1<<20)
		}
		notify := j.notify
		j.mu.Unlock()
		remaining := time.Until(deadline)
		if timeout <= 0 || remaining <= 0 {
			return j.Snapshot(offset, 1<<20)
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			continue
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return j.Snapshot(offset, 1<<20)
		case <-timer.C:
			return j.Snapshot(offset, 1<<20)
		}
	}
}

func (j *BackgroundRelayJob) signalLocked() {
	close(j.notify)
	j.notify = make(chan struct{})
}
