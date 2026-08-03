package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestShouldDisableChannelSeparatesNoReplayFromNoPenalty(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	oldRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusInternalServerError,
		End:   http.StatusInternalServerError,
	}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})

	localCancellation := types.NewErrorWithStatusCode(
		errors.New("client canceled"),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
	)
	require.False(t, ShouldDisableChannel(localCancellation))

	postWriteUpstreamFailure := types.NewErrorWithStatusCode(
		errors.New("upstream response header timeout"),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithChannelPenalty(),
	)
	require.True(t, ShouldDisableChannel(postWriteUpstreamFailure))
}
