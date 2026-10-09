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

func TestCPAOutputValidationCannotDisableOrOpenChannelCircuit(t *testing.T) {
	oldEnabled, oldRanges := common.AutomaticDisableChannelEnabled, operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})
	for _, code := range []string{"output_invalid_json", "output_schema_mismatch", "output_not_json_object", "output_stream_mismatch", "output_unknown_tool"} {
		body := fmt.Sprintf(`{"error":{"code":%q,"type":"server_error","message":"上游输出未满足格式要求"}}`, code)
		err := RelayErrorHandler(context.Background(), &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, false)
		require.NotNil(t, err)
		require.True(t, err.HasUpstreamResponse())
		require.True(t, types.IsSkipRetryError(err))
		require.Equal(t, 502, err.StatusCode)
		require.Equal(t, types.ErrorCode(code), err.GetErrorCode())
		require.Equal(t, "上游输出未满足格式要求", err.Error())
		require.False(t, ShouldDisableChannel(err))
		require.False(t, ShouldCountChannelCircuitFailure(err))
		ResetStatusCode(err, `{"502":"503"}`)
		require.Equal(t, 503, err.StatusCode)
		require.True(t, types.IsSkipRetryError(err))
		require.False(t, ShouldDisableChannel(err))
		require.False(t, ShouldCountChannelCircuitFailure(err))
	}
}
