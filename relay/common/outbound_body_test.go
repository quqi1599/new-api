package common

import (
	"io"
	"testing"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestNewOutboundJSONBodyPreservesBytesAndSize(t *testing.T) {
	payload := []byte(`{"model":"test","input":"payload"}`)
	body, size, closer, err := NewOutboundJSONBody(payload)
	require.NoError(t, err)
	defer closer.Close()
	require.Equal(t, int64(len(payload)), size)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestNewOutboundJSONBodyReleasesAdmissionLease(t *testing.T) {
	previousConfig := basecommon.GetDiskCacheConfig()
	previous := constant.MaxConcurrentLargeRequestBodies
	basecommon.SetDiskCacheConfig(basecommon.DiskCacheConfig{Enabled: false, ThresholdMB: 1})
	constant.MaxConcurrentLargeRequestBodies = 1
	t.Cleanup(func() {
		basecommon.SetDiskCacheConfig(previousConfig)
		constant.MaxConcurrentLargeRequestBodies = previous
	})

	payload := make([]byte, 1<<20)
	_, _, firstCloser, err := NewOutboundJSONBody(payload)
	require.NoError(t, err)
	defer firstCloser.Close()
	_, _, secondCloser, err := NewOutboundJSONBody(payload)
	require.NoError(t, err, "outbound network wait must not retain the parse admission slot")
	defer secondCloser.Close()
}
