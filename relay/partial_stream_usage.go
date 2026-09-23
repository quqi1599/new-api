package relay

import (
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// Only an adapter's exact post-output failure may preserve its previous partial
// usage settlement. Other errors (including explicit Responses failed events)
// retain their existing refund policy; no new success or retry is synthesized.
func canSettlePartialStreamUsage(c *gin.Context, info *relaycommon.RelayInfo, usage any, err *types.NewAPIError) bool {
	u, ok := usage.(*dto.Usage)
	return err != nil && info != nil && info.IsStream && info.PartialStreamError == err &&
		info.ReceivedResponseCount > 0 && helper.StreamStarted(c) && ok && u != nil && types.IsSkipRetryError(err)
}
