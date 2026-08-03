package common

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

type partialErrorReader struct {
	data []byte
	done bool
}

type genericErrorReader struct {
	data []byte
	done bool
}

func (r *genericErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), errors.New("connection reset by peer")
}

func (r *partialErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), io.ErrUnexpectedEOF
}

func setBodyStorageTestConfig(t *testing.T, config DiskCacheConfig) {
	t.Helper()
	original := GetDiskCacheConfig()
	SetDiskCacheConfig(config)
	t.Cleanup(func() {
		SetDiskCacheConfig(original)
	})
}

func TestCreateBodyStorageFromReaderMemoryShortReadPreservesCounts(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false})

	storage, err := CreateBodyStorageFromReader(strings.NewReader("abc"), 10, 64)
	require.Nil(t, storage)
	require.Error(t, err)

	var incompleteErr *IncompleteBodyError
	require.ErrorAs(t, err, &incompleteErr)
	require.Equal(t, int64(10), incompleteErr.Declared)
	require.Equal(t, int64(3), incompleteErr.Received)
	require.Equal(t, bodyStorageMemory, incompleteErr.Storage)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestCreateBodyStorageFromReaderMemoryReaderErrorPreservesCounts(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false})

	storage, err := CreateBodyStorageFromReader(&partialErrorReader{data: []byte("partial")}, -1, 64)
	require.Nil(t, storage)
	require.Error(t, err)

	var incompleteErr *IncompleteBodyError
	require.ErrorAs(t, err, &incompleteErr)
	require.Equal(t, int64(-1), incompleteErr.Declared)
	require.Equal(t, int64(len("partial")), incompleteErr.Received)
	require.Equal(t, bodyStorageMemory, incompleteErr.Storage)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestCreateBodyStorageFromReaderUnknownLengthAcceptsCleanEOF(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false})

	storage, err := CreateBodyStorageFromReader(strings.NewReader("abc"), -1, 64)
	require.NoError(t, err)
	require.NotNil(t, storage)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	require.False(t, storage.IsDisk())
	require.Equal(t, int64(3), storage.Size())

	data, err := storage.Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte("abc"), data)
}

func TestCreateBodyStorageFromReaderGenericWireErrorIsClientBodyReadFailure(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false})

	storage, err := CreateBodyStorageFromReader(&genericErrorReader{data: []byte("partial")}, -1, 64)
	require.Nil(t, storage)
	require.Error(t, err)
	require.True(t, IsBodyReadError(err))
	require.False(t, IsInternalBodyStorageError(err))

	var bodyReadErr *BodyReadError
	require.ErrorAs(t, err, &bodyReadErr)
	require.Equal(t, int64(len("partial")), bodyReadErr.Received)
}

func TestLargeMemoryBodyAdmissionCapsConcurrentRetainedBodies(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false, ThresholdMB: 1})
	oldLimit := constant.MaxConcurrentLargeRequestBodies
	constant.MaxConcurrentLargeRequestBodies = 1
	t.Cleanup(func() { constant.MaxConcurrentLargeRequestBodies = oldLimit })

	body := bytes.Repeat([]byte("x"), 1<<20)
	first, err := CreateBodyStorage(body)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := CreateBodyStorage(body)
	require.Nil(t, second)
	require.True(t, IsBodyAdmissionError(err))

	require.NoError(t, first.Close())
	third, err := CreateBodyStorage(body)
	require.NoError(t, err)
	require.NotNil(t, third)
	require.NoError(t, third.Close())
}

func TestUnknownLengthBodyAcquiresAdmissionBeforeDiskThreshold(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false, ThresholdMB: 10})
	oldLimit := constant.MaxConcurrentLargeRequestBodies
	constant.MaxConcurrentLargeRequestBodies = 1
	t.Cleanup(func() { constant.MaxConcurrentLargeRequestBodies = oldLimit })

	body := bytes.Repeat([]byte("x"), largeBodyAdmissionThresholdBytes+1)
	first, err := CreateBodyStorageFromReader(bytes.NewReader(body), -1, 16<<20)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := CreateBodyStorageFromReader(bytes.NewReader(body), -1, 16<<20)
	require.Nil(t, second)
	require.True(t, IsBodyAdmissionError(err), "unknown-length bodies must be gated before probing to the 10 MiB disk threshold")

	require.NoError(t, first.Close())
}

func TestDiskCacheReservationIsAtomicAgainstConfiguredLimit(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: true, ThresholdMB: 1, MaxSizeMB: 1, Path: t.TempDir()})
	ResetDiskCacheUsage()
	t.Cleanup(ResetDiskCacheUsage)

	require.True(t, ReserveDiskCacheBytes(1<<20))
	require.False(t, ReserveDiskCacheBytes(1), "a concurrent reservation must not exceed the configured cap")
	ReleaseDiskCacheReservation(1 << 20)
	require.True(t, ReserveDiskCacheBytes(1))
	ReleaseDiskCacheReservation(1)
}

func TestCreateBodyStorageFromReaderUnknownLengthSpillsAfterThreshold(t *testing.T) {
	cacheRoot := t.TempDir()
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        cacheRoot,
	})

	body := bytes.Repeat([]byte("x"), (1<<20)+1024)
	storage, err := CreateBodyStorageFromReader(bytes.NewReader(body), -1, 2<<20)
	require.NoError(t, err)
	require.NotNil(t, storage)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	require.True(t, storage.IsDisk())
	require.Equal(t, int64(len(body)), storage.Size())

	storedBody, err := storage.Bytes()
	require.NoError(t, err)
	require.Equal(t, body, storedBody)
}

func TestCreateBodyStorageFromReaderOversizeRemainsDistinct(t *testing.T) {
	setBodyStorageTestConfig(t, DiskCacheConfig{Enabled: false})

	storage, err := CreateBodyStorageFromReader(strings.NewReader("12345"), 5, 4)
	require.Nil(t, storage)
	require.ErrorIs(t, err, ErrRequestBodyTooLarge)
	require.False(t, IsIncompleteBodyError(err))
}

func TestCreateBodyStorageFromReaderDiskShortReadRemovesTemporaryFile(t *testing.T) {
	cacheRoot := t.TempDir()
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        cacheRoot,
	})

	const declared = int64(1 << 20)
	storage, err := CreateBodyStorageFromReader(strings.NewReader("partial"), declared, 2<<20)
	require.Nil(t, storage)
	require.Error(t, err)

	var incompleteErr *IncompleteBodyError
	require.ErrorAs(t, err, &incompleteErr)
	require.Equal(t, declared, incompleteErr.Declared)
	require.Equal(t, int64(len("partial")), incompleteErr.Received)
	require.Equal(t, bodyStorageDisk, incompleteErr.Storage)

	entries, readErr := os.ReadDir(filepath.Join(cacheRoot, diskCacheDir))
	require.NoError(t, readErr)
	require.Empty(t, entries, "partial disk bodies must not leave temporary files")
}

func TestCreateBodyStorageFromReaderDiskStorageFailureIsInternal(t *testing.T) {
	cacheRoot := t.TempDir()
	invalidCacheRoot := filepath.Join(cacheRoot, "not-a-directory")
	require.NoError(t, os.WriteFile(invalidCacheRoot, []byte("file"), 0o600))
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        invalidCacheRoot,
	})

	storage, err := CreateBodyStorageFromReader(bytes.NewReader(make([]byte, 1<<20)), 1<<20, 2<<20)
	require.Nil(t, storage)
	require.Error(t, err)
	require.True(t, IsInternalBodyStorageError(err))
	require.False(t, IsIncompleteBodyError(err))
	require.False(t, errors.Is(err, ErrRequestBodyTooLarge))

	var storageErr *InternalBodyStorageError
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, bodyStorageDisk, storageErr.Storage)
	require.Equal(t, "create", storageErr.Operation)
}

func TestCreateBodyStorageFromReaderDiskCloseRemovesTemporaryFile(t *testing.T) {
	cacheRoot := t.TempDir()
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        cacheRoot,
	})

	body := bytes.Repeat([]byte("x"), 1<<20)
	storage, err := CreateBodyStorageFromReader(bytes.NewReader(body), int64(len(body)), 2<<20)
	require.NoError(t, err)
	require.True(t, storage.IsDisk())

	diskBody, ok := storage.(*diskStorage)
	require.True(t, ok)
	_, err = os.Stat(diskBody.filePath)
	require.NoError(t, err)

	require.NoError(t, storage.Close())
	_, err = os.Stat(diskBody.filePath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDiskBodyRetainsLargeAdmissionUntilClose(t *testing.T) {
	cacheRoot := t.TempDir()
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        cacheRoot,
	})
	oldLimit := constant.MaxConcurrentLargeRequestBodies
	constant.MaxConcurrentLargeRequestBodies = 1
	t.Cleanup(func() { constant.MaxConcurrentLargeRequestBodies = oldLimit })

	body := bytes.Repeat([]byte("x"), 1<<20)
	first, err := CreateBodyStorageFromReader(bytes.NewReader(body), int64(len(body)), 2<<20)
	require.NoError(t, err)
	require.True(t, first.IsDisk())

	second, err := CreateBodyStorageFromReader(bytes.NewReader(body), int64(len(body)), 2<<20)
	require.Nil(t, second)
	require.True(t, IsBodyAdmissionError(err))

	require.NoError(t, first.Close())
	third, err := CreateBodyStorageFromReader(bytes.NewReader(body), int64(len(body)), 2<<20)
	require.NoError(t, err)
	require.NotNil(t, third)
	require.NoError(t, third.Close())
}

func TestCleanupOldCacheFilesSyncsRecentCrashResidueIntoQuota(t *testing.T) {
	cacheRoot := t.TempDir()
	setBodyStorageTestConfig(t, DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 1,
		MaxSizeMB:   16,
		Path:        cacheRoot,
	})
	ResetDiskCacheUsage()
	t.Cleanup(ResetDiskCacheUsage)

	require.NoError(t, EnsureDiskCacheDir())
	residue := bytes.Repeat([]byte("x"), 4096)
	require.NoError(t, os.WriteFile(filepath.Join(GetDiskCacheDir(), "body-crash-residue.tmp"), residue, 0o600))

	CleanupOldCacheFiles()

	stats := GetDiskCacheStats()
	require.Equal(t, int64(1), stats.ActiveDiskFiles)
	require.Equal(t, int64(len(residue)), stats.CurrentDiskUsageBytes)
}
