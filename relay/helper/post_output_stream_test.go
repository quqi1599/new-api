package helper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPostOutputStreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		reason  relaycommon.StreamEndReason
		status  int
		penalty bool
	}{
		{relaycommon.StreamEndReasonEOF, 502, true},
		{relaycommon.StreamEndReasonScannerErr, 502, true},
		{relaycommon.StreamEndReasonHandlerStop, 502, true},
		{relaycommon.StreamEndReasonTimeout, 504, true},
		{relaycommon.StreamEndReasonClientGone, 499, false},
		{relaycommon.StreamEndReasonRequestDeadline, 504, false},
		{relaycommon.StreamEndReasonPingFail, 502, false},
		{relaycommon.StreamEndReasonDone, 0, false},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Writer.WriteHeaderNow()
			info := &relaycommon.RelayInfo{ReceivedResponseCount: 1, StreamStatus: relaycommon.NewStreamStatus()}
			info.StreamStatus.SetEndReason(tc.reason, nil)
			err := PostOutputStreamError(c, info)
			if tc.status == 0 {
				require.Nil(t, err)
				require.Nil(t, info.PartialStreamError)
				return
			}
			require.NotNil(t, err)
			require.Equal(t, tc.status, err.StatusCode)
			require.True(t, types.IsSkipRetryError(err))
			require.Equal(t, tc.penalty, types.IsChannelPenaltyAllowed(err))
			require.Same(t, err, info.PartialStreamError)
		})
	}
}
