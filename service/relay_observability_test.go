package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAppendRelayObservabilityAdminInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, constant.RelayCancelOriginDownstreamDisconnected)
	common.SetContextKey(c, constant.ContextKeyRelayBodyComplete, true)
	common.SetContextKey(c, constant.ContextKeyRelayConnectedUpstream, true)
	common.SetContextKey(c, constant.ContextKeyRelayRequestWritten, true)

	adminInfo := map[string]interface{}{}
	AppendRelayObservabilityAdminInfo(c, &relaycommon.RelayInfo{}, adminInfo)

	require.Equal(t, constant.RelayCancelOriginDownstreamDisconnected, adminInfo["cancel_origin"])
	stage := adminInfo["relay_stage"].(map[string]bool)
	require.True(t, stage["body_complete"])
	require.True(t, stage["connected_upstream"])
	require.True(t, stage["request_written"])
	require.False(t, stage["response_headers_received"])
	require.False(t, stage["first_valid_event_received"])
	require.False(t, stage["output_started"])
}
