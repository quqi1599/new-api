package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// BodyStorage 请求体存储接口
type BodyStorage interface {
	io.ReadSeeker
	io.Closer
	// Bytes 获取全部内容
	Bytes() ([]byte, error)
	// Size 获取数据大小
	Size() int64
	// IsDisk 是否是磁盘存储
	IsDisk() bool
	// ReleaseAdmission 在请求体读取和解析阶段结束后释放大请求准入槽。
	// 存储本身仍保留到 Close，用于上游传输和安全重试。
	ReleaseAdmission()
}

// ErrStorageClosed 存储已关闭错误
var ErrStorageClosed = fmt.Errorf("body storage is closed")

const (
	bodyStorageMemory                = "memory"
	bodyStorageDisk                  = "disk"
	largeBodyAdmissionThresholdBytes = 1 << 20
)

// IncompleteBodyError reports a request body that ended before its declared
// Content-Length, while preserving how many bytes were actually read.
// Declared is -1 when the decoded request length is unknown.
type IncompleteBodyError struct {
	Declared int64
	Received int64
	Storage  string
	Cause    error
}

// BodyReadError identifies a malformed compressed body or a transport read
// failure that is attributable to the inbound request rather than local
// storage. It maps to a stable 400 and must never be retried upstream.
type BodyReadError struct {
	Declared int64
	Received int64
	Cause    error
}

func (e *BodyReadError) Error() string {
	return fmt.Sprintf("failed to read request body: declared=%d received=%d: %v", e.Declared, e.Received, e.Cause)
}

func (e *BodyReadError) Unwrap() error { return e.Cause }

func IsBodyReadError(err error) bool {
	var bodyReadErr *BodyReadError
	return errors.As(err, &bodyReadErr)
}

// BodyAdmissionError means the process is already reading or parsing the
// configured number of large request bodies. Rejecting before another large
// allocation protects the gateway from concurrent upload/parse OOM.
type BodyAdmissionError struct{}

func (e *BodyAdmissionError) Error() string {
	return "request body capacity is temporarily exhausted"
}

func IsBodyAdmissionError(err error) bool {
	var admissionErr *BodyAdmissionError
	return errors.As(err, &admissionErr)
}

func (e *IncompleteBodyError) Error() string {
	if e == nil {
		return "request body incomplete"
	}
	message := fmt.Sprintf(
		"request body incomplete: declared=%d received=%d storage=%s",
		e.Declared,
		e.Received,
		e.Storage,
	)
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", message, e.Cause)
	}
	return message
}

func (e *IncompleteBodyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsIncompleteBodyError(err error) bool {
	var incompleteErr *IncompleteBodyError
	return errors.As(err, &incompleteErr)
}

// InternalBodyStorageError identifies failures in the local body-storage
// implementation, keeping them separate from malformed or truncated input.
type InternalBodyStorageError struct {
	Storage   string
	Operation string
	Cause     error
}

func (e *InternalBodyStorageError) Error() string {
	if e == nil {
		return "internal request body storage error"
	}
	return fmt.Sprintf("internal request body storage error: storage=%s operation=%s: %v", e.Storage, e.Operation, e.Cause)
}

func (e *InternalBodyStorageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsInternalBodyStorageError(err error) bool {
	var storageErr *InternalBodyStorageError
	return errors.As(err, &storageErr)
}

func classifyIncompleteBodyRead(err error, declared, received int64, storage string) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || (declared >= 0 && received < declared) {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return &IncompleteBodyError{
			Declared: declared,
			Received: received,
			Storage:  storage,
			Cause:    err,
		}
	}
	if err != nil {
		return &BodyReadError{Declared: declared, Received: received, Cause: err}
	}
	return nil
}

type bodyStorageWriter struct {
	writer io.Writer
	err    error
}

func (w *bodyStorageWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// memoryStorage 内存存储实现
type memoryStorage struct {
	data      []byte
	reader    *bytes.Reader
	size      int64
	closed    int32
	admission *bodyAdmissionLease
	mu        sync.Mutex
}

func newMemoryStorage(data []byte, admission *bodyAdmissionLease) *memoryStorage {
	size := int64(len(data))
	IncrementMemoryBuffers(size)
	observeRequestBodySize(size)
	return &memoryStorage{
		data:      data,
		reader:    bytes.NewReader(data),
		size:      size,
		admission: admission,
	}
}

func (m *memoryStorage) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Read(p)
}

func (m *memoryStorage) Seek(offset int64, whence int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Seek(offset, whence)
}

func (m *memoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.CompareAndSwapInt32(&m.closed, 0, 1) {
		DecrementMemoryBuffers(m.size)
		m.ReleaseAdmission()
	}
	return nil
}

func (m *memoryStorage) ReleaseAdmission() {
	if m != nil {
		m.admission.Release()
	}
}

func (m *memoryStorage) Bytes() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return nil, ErrStorageClosed
	}
	return m.data, nil
}

func (m *memoryStorage) Size() int64 {
	return m.size
}

func (m *memoryStorage) IsDisk() bool {
	return false
}

// diskStorage 磁盘存储实现
type diskStorage struct {
	file      *os.File
	filePath  string
	size      int64
	closed    int32
	admission *bodyAdmissionLease
	mu        sync.Mutex
}

func newDiskStorage(data []byte, cachePath string) (*diskStorage, error) {
	large := int64(len(data)) >= largeBodyAdmissionThresholdBytes
	var admission *bodyAdmissionLease
	if large {
		admission = acquireLargeRequestBodySlot()
		if admission == nil {
			return nil, &BodyAdmissionError{}
		}
	}
	releaseLargeSlot := func() {
		admission.Release()
	}
	reserved := int64(len(data))
	if !ReserveDiskCacheBytes(reserved) {
		releaseLargeSlot()
		return nil, &BodyAdmissionError{}
	}
	reservationActive := true
	releaseReservation := func() {
		if reservationActive {
			ReleaseDiskCacheReservation(reserved)
			reservationActive = false
		}
	}
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		releaseReservation()
		releaseLargeSlot()
		return nil, err
	}

	// 写入数据
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		file.Close()
		os.Remove(filePath)
		releaseReservation()
		releaseLargeSlot()
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(filePath)
		releaseReservation()
		releaseLargeSlot()
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	size := int64(n)
	CommitReservedDiskFile(reserved, size)
	reservationActive = false
	observeRequestBodySize(size)

	return &diskStorage{
		file:      file,
		filePath:  filePath,
		size:      size,
		admission: admission,
	}, nil
}

func newDiskStorageFromReader(reader io.Reader, contentLength int64, maxBytes int64, cachePath string, admission *bodyAdmissionLease) (*diskStorage, error) {
	large := admission != nil || contentLength >= largeBodyAdmissionThresholdBytes
	if large && admission == nil {
		admission = acquireLargeRequestBodySlot()
		if admission == nil {
			return nil, &BodyAdmissionError{}
		}
	}
	releaseLargeSlot := func() {
		admission.Release()
	}
	reserved := maxBytes
	if contentLength > 0 && contentLength < reserved {
		reserved = contentLength
	}
	if !ReserveDiskCacheBytes(reserved) {
		releaseLargeSlot()
		return nil, &BodyAdmissionError{}
	}
	reservationActive := true
	releaseReservation := func() {
		if reservationActive {
			ReleaseDiskCacheReservation(reserved)
			reservationActive = false
		}
	}
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		releaseReservation()
		releaseLargeSlot()
		return nil, &InternalBodyStorageError{
			Storage:   bodyStorageDisk,
			Operation: "create",
			Cause:     err,
		}
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(filePath)
		releaseReservation()
		releaseLargeSlot()
	}

	// 从 reader 读取并写入文件
	trackedWriter := &bodyStorageWriter{writer: file}
	written, err := io.Copy(trackedWriter, io.LimitReader(reader, maxBytes+1))
	if err != nil {
		cleanup()
		if IsRequestBodyTooLargeError(err) {
			return nil, ErrRequestBodyTooLarge
		}
		if trackedWriter.err != nil {
			return nil, &InternalBodyStorageError{
				Storage:   bodyStorageDisk,
				Operation: "write",
				Cause:     trackedWriter.err,
			}
		}
		return nil, classifyIncompleteBodyRead(err, contentLength, written, bodyStorageDisk)
	}

	if written > maxBytes {
		cleanup()
		return nil, ErrRequestBodyTooLarge
	}
	if err := classifyIncompleteBodyRead(nil, contentLength, written, bodyStorageDisk); err != nil {
		cleanup()
		return nil, err
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, &InternalBodyStorageError{
			Storage:   bodyStorageDisk,
			Operation: "seek",
			Cause:     err,
		}
	}

	CommitReservedDiskFile(reserved, written)
	reservationActive = false
	observeRequestBodySize(written)

	return &diskStorage{
		file:      file,
		filePath:  filePath,
		size:      written,
		admission: admission,
	}, nil
}

func (d *diskStorage) Read(p []byte) (n int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Read(p)
}

func (d *diskStorage) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Seek(offset, whence)
}

func (d *diskStorage) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.CompareAndSwapInt32(&d.closed, 0, 1) {
		d.file.Close()
		os.Remove(d.filePath)
		DecrementDiskFiles(d.size)
		d.ReleaseAdmission()
	}
	return nil
}

func (d *diskStorage) ReleaseAdmission() {
	if d != nil {
		d.admission.Release()
	}
}

func (d *diskStorage) Bytes() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if atomic.LoadInt32(&d.closed) == 1 {
		return nil, ErrStorageClosed
	}

	// 保存当前位置
	currentPos, err := d.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, &InternalBodyStorageError{Storage: bodyStorageDisk, Operation: "seek-current", Cause: err}
	}

	// 移动到开头
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		return nil, &InternalBodyStorageError{Storage: bodyStorageDisk, Operation: "seek-start", Cause: err}
	}

	// 读取全部内容
	data := make([]byte, d.size)
	_, err = io.ReadFull(d.file, data)
	if err != nil {
		return nil, &InternalBodyStorageError{Storage: bodyStorageDisk, Operation: "read", Cause: err}
	}

	// 恢复位置
	if _, err := d.file.Seek(currentPos, io.SeekStart); err != nil {
		return nil, &InternalBodyStorageError{Storage: bodyStorageDisk, Operation: "restore-position", Cause: err}
	}

	return data, nil
}

func (d *diskStorage) Size() int64 {
	return d.size
}

func (d *diskStorage) IsDisk() bool {
	return true
}

// CreateBodyStorage 根据数据大小创建合适的存储
func CreateBodyStorage(data []byte) (BodyStorage, error) {
	size := int64(len(data))
	threshold := GetDiskCacheThresholdBytes()

	// 检查是否应该使用磁盘缓存
	if IsDiskCacheEnabled() &&
		size >= threshold &&
		IsDiskCacheAvailable(size) {
		storage, err := newDiskStorage(data, GetDiskCachePath())
		if err != nil {
			// 如果磁盘存储失败，回退到内存存储
			SysError(fmt.Sprintf("failed to create disk storage, falling back to memory: %v", err))
			admission := acquireLargeRequestBodySlot()
			if admission == nil {
				return nil, &BodyAdmissionError{}
			}
			return newMemoryStorage(data, admission), nil
		}
		return storage, nil
	}

	large := size >= largeBodyAdmissionThresholdBytes
	var admission *bodyAdmissionLease
	if large {
		admission = acquireLargeRequestBodySlot()
		if admission == nil {
			return nil, &BodyAdmissionError{}
		}
	}
	return newMemoryStorage(data, admission), nil
}

// CreateBodyStorageFromReader 从 Reader 创建存储（用于大请求的流式处理）
func CreateBodyStorageFromReader(reader io.Reader, contentLength int64, maxBytes int64) (BodyStorage, error) {
	threshold := GetDiskCacheThresholdBytes()
	if contentLength > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}

	// 如果启用了磁盘缓存且内容长度超过阈值，直接使用磁盘存储
	if IsDiskCacheEnabled() &&
		contentLength > 0 &&
		contentLength >= threshold &&
		IsDiskCacheAvailable(contentLength) {
		storage, err := newDiskStorageFromReader(reader, contentLength, maxBytes, GetDiskCachePath(), nil)
		if err != nil {
			if IsRequestBodyTooLargeError(err) {
				return nil, err
			}
			// 磁盘路径已消费 reader，不能安全回退到内存。
			return nil, err
		}
		IncrementDiskCacheHits()
		return storage, nil
	}

	limitedReader := io.LimitReader(reader, maxBytes+1)
	var data []byte
	var err error
	var admission *bodyAdmissionLease
	releaseLargeSlot := func() {
		admission.Release()
	}

	admissionThreshold := int64(largeBodyAdmissionThresholdBytes)
	if maxBytes < admissionThreshold {
		admissionThreshold = maxBytes
	}

	// Decompression and chunked transfer make Content-Length unknown. Probe only
	// a small bounded prefix before acquiring a large-body slot, then continue to
	// the disk spill threshold. Probing all the way to a 10+ MiB disk threshold
	// before admission would let many concurrent unknown-length bodies exhaust
	// memory while each one was still nominally "small".
	if contentLength <= 0 && admissionThreshold > 0 && admissionThreshold < maxBytes {
		data, err = io.ReadAll(io.LimitReader(limitedReader, admissionThreshold+1))
		if err == nil && int64(len(data)) > admissionThreshold {
			admission = acquireLargeRequestBodySlot()
			if admission == nil {
				return nil, &BodyAdmissionError{}
			}

			// Continue only as far as the configured disk threshold before
			// deciding whether this body should remain in memory.
			if threshold > int64(len(data)) && threshold < maxBytes {
				var thresholdRemainder []byte
				thresholdRemainder, err = io.ReadAll(io.LimitReader(limitedReader, threshold+1-int64(len(data))))
				data = append(data, thresholdRemainder...)
			}
			if err == nil && threshold > 0 && int64(len(data)) > threshold && IsDiskCacheEnabled() && IsDiskCacheAvailable(maxBytes) {
				combinedReader := io.MultiReader(bytes.NewReader(data), limitedReader)
				// Transfer the already-acquired parsing lease to disk storage. The
				// controller releases it once request parsing completes; Close remains
				// an idempotent fallback for early failures.
				storage, diskErr := newDiskStorageFromReader(combinedReader, contentLength, maxBytes, GetDiskCachePath(), admission)
				if diskErr != nil {
					return nil, diskErr
				}
				IncrementDiskCacheHits()
				return storage, nil
			}
			if err == nil {
				var remainder []byte
				remainder, err = io.ReadAll(limitedReader)
				data = append(data, remainder...)
			}
		}
	} else {
		if contentLength > 0 && contentLength >= admissionThreshold {
			admission = acquireLargeRequestBodySlot()
			if admission == nil {
				return nil, &BodyAdmissionError{}
			}
		}
		data, err = io.ReadAll(limitedReader)
	}

	received := int64(len(data))
	if err != nil {
		releaseLargeSlot()
		if IsRequestBodyTooLargeError(err) {
			return nil, ErrRequestBodyTooLarge
		}
		if received > maxBytes {
			return nil, ErrRequestBodyTooLarge
		}
		return nil, classifyIncompleteBodyRead(err, contentLength, received, bodyStorageMemory)
	}
	if received > maxBytes {
		releaseLargeSlot()
		return nil, ErrRequestBodyTooLarge
	}
	if err := classifyIncompleteBodyRead(nil, contentLength, received, bodyStorageMemory); err != nil {
		releaseLargeSlot()
		return nil, err
	}

	storage := newMemoryStorage(data, admission)
	IncrementMemoryCacheHits()
	return storage, nil
}

// ReaderOnly wraps an io.Reader to hide io.Closer, preventing http.NewRequest
// from type-asserting io.ReadCloser and closing the underlying BodyStorage.
func ReaderOnly(r io.Reader) io.Reader {
	return struct{ io.Reader }{r}
}

// CleanupOldCacheFiles 清理旧的缓存文件（用于启动时清理残留）
func CleanupOldCacheFiles() {
	// 使用统一的缓存管理
	CleanupOldDiskCacheFiles(5 * time.Minute)
	// A previous process can leave recent spool files behind. Rebuild the
	// admission counter from disk after cleanup so a restart cannot temporarily
	// under-count disk usage and admit more data than the configured quota.
	SyncDiskCacheStats()
}
