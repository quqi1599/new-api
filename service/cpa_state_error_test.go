package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestCPAStateOwnershipCannotPenalizeOrDisableChannel(t *testing.T) {
	oldEnabled, oldRanges := common.AutomaticDisableChannelEnabled, operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})
	for _, code := range []string{"state_owner_unavailable", "state_owner_conflict", "state_owner_expired", "local_state_unavailable"} {
		body := fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":%q,"message":"synthetic state provenance failure"}}`, code)
		err := RelayErrorHandler(context.Background(), &http.Response{
			StatusCode: http.StatusConflict,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, false)
		require.NotNil(t, err)
		require.True(t, err.HasUpstreamResponse())
		require.True(t, types.IsSkipRetryError(err))
		require.False(t, ShouldDisableChannel(err), "even configured 409 auto-disable must not punish a state ownership rejection")
		require.False(t, ShouldCountChannelCircuitFailure(err))
		// Existing status remapping must not remove the no-replay/no-penalty flags.
		ResetStatusCode(err, `{"409":"503"}`)
		require.Equal(t, http.StatusServiceUnavailable, err.StatusCode)
		require.True(t, types.IsSkipRetryError(err))
		require.False(t, ShouldDisableChannel(err))
		require.False(t, ShouldCountChannelCircuitFailure(err))
	}
}
