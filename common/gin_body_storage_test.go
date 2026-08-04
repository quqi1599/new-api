package common

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalBodyReusableStreamsDiskBackedJSONAndRewinds(t *testing.T) {
	previousConfig := GetDiskCacheConfig()
	previousLimit := constant.MaxConcurrentLargeRequestBodies
	SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: 1, MaxSizeMB: 8, Path: t.TempDir()})
	constant.MaxConcurrentLargeRequestBodies = 2
	ResetDiskCacheUsage()
	t.Cleanup(func() {
		SetDiskCacheConfig(previousConfig)
		constant.MaxConcurrentLargeRequestBodies = previousLimit
		ResetDiskCacheUsage()
	})

	payload := append([]byte(`{"payload":"`), bytes.Repeat([]byte("a"), 1<<20)...)
	payload = append(payload, []byte(`"}`)...)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")

	var decoded struct {
		Payload string `json:"payload"`
	}
	require.NoError(t, UnmarshalBodyReusable(ctx, &decoded))
	require.Len(t, decoded.Payload, 1<<20)
	storage, err := GetBodyStorage(ctx)
	require.NoError(t, err)
	require.True(t, storage.IsDisk())
	firstByte := make([]byte, 1)
	_, err = io.ReadFull(ctx.Request.Body, firstByte)
	require.NoError(t, err)
	require.Equal(t, byte('{'), firstByte[0])
	CleanupBodyStorage(ctx)
}

func TestUnmarshalBodyReusableDiskBackedJSONRejectsTrailingContent(t *testing.T) {
	previousConfig := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: 1, MaxSizeMB: 8, Path: t.TempDir()})
	ResetDiskCacheUsage()
	t.Cleanup(func() {
		SetDiskCacheConfig(previousConfig)
		ResetDiskCacheUsage()
	})

	payload := append([]byte(`{"payload":"`), bytes.Repeat([]byte("a"), 1<<20)...)
	payload = append(payload, []byte(`"} {"second":true}`)...)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")

	var decoded map[string]any
	require.Error(t, UnmarshalBodyReusable(ctx, &decoded))
	CleanupBodyStorage(ctx)
}
