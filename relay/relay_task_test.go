package relay

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskErrorFromDoRequestPreservesClientCancellation(t *testing.T) {
	apiErr := types.NewErrorWithStatusCode(
		context.Canceled,
		types.ErrorCodeDoRequestFailed,
		499,
		types.ErrOptionWithSkipRetry(),
	)

	taskErr := taskErrorFromDoRequest(fmt.Errorf("provider request failed: %w", apiErr))

	require.Equal(t, 499, taskErr.StatusCode)
	require.Equal(t, string(types.ErrorCodeDoRequestFailed), taskErr.Code)
	require.True(t, taskErr.LocalError)
	require.True(t, taskErr.SkipRetry)
	require.ErrorIs(t, taskErr.Error, context.Canceled)
}

func TestTaskErrorFromDoRequestSeparatesNoReplayFromChannelHealth(t *testing.T) {
	apiErr := types.NewErrorWithStatusCode(
		context.DeadlineExceeded,
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)

	taskErr := taskErrorFromDoRequest(apiErr)

	require.True(t, taskErr.SkipRetry)
	require.False(t, taskErr.LocalError)
}

func TestTaskErrorFromDoRequestKeepsUpstreamFailureRetryable(t *testing.T) {
	taskErr := taskErrorFromDoRequest(fmt.Errorf("upstream unavailable"))

	require.Equal(t, http.StatusInternalServerError, taskErr.StatusCode)
	require.Equal(t, "do_request_failed", taskErr.Code)
	require.False(t, taskErr.LocalError)
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}
