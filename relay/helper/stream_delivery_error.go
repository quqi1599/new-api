package helper

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// downstreamWriteError preserves provenance: parsing/conversion errors must
// never be inferred to be a disconnected client from their message text.
type downstreamWriteError struct{ cause error }

const completedStreamScannerStatusKey = "relay_completed_stream_scanner_status"

// markStreamScannerCompleted must only run after scanner cleanup has joined all
// scanner, handler and heartbeat goroutines. Bind the marker to this particular
// status object so a reused Gin context cannot authorize an earlier attempt.
func markStreamScannerCompleted(c *gin.Context, info *relaycommon.RelayInfo) {
	if c != nil && info != nil && info.StreamStatus != nil {
		c.Set(completedStreamScannerStatusKey, info.StreamStatus)
	}
}

func setDownstreamStreamEnd(c *gin.Context, info *relaycommon.RelayInfo, reason relaycommon.StreamEndReason, err error) {
	status := info.StreamStatus
	status.SetEndReason(reason, err)
	if c == nil {
		return
	}
	completedStatus, _ := c.Get(completedStreamScannerStatusKey)
	if completedStatus != status || status.EndReason != relaycommon.StreamEndReasonDone {
		return
	}
	// An upstream terminal marker can precede the adapter's final usage/[DONE]
	// writes. Those writes must not leave a false successful terminal state on
	// delivery failure. The completion marker proves no scanner goroutine can
	// still read or mutate the state; during scanning, endOnce stays authoritative.
	status.EndReason, status.EndError = reason, err
	status.RecordError(err.Error())
}

func (e *downstreamWriteError) Error() string { return e.cause.Error() }
func (e *downstreamWriteError) Unwrap() error { return e.cause }

func wrapDownstreamWriteError(err error) error {
	if err == nil {
		return nil
	}
	return &downstreamWriteError{cause: err}
}

// DownstreamStreamError classifies only failures returned by actual downstream
// writes/flushes. Call it before StreamResult.Stop so a delivery failure cannot
// be replaced by the generic upstream handler-stop classification.
func DownstreamStreamError(c *gin.Context, info *relaycommon.RelayInfo, err error) *types.NewAPIError {
	var delivery *downstreamWriteError
	if !errors.As(err, &delivery) {
		return nil
	}
	reason := relaycommon.StreamEndReasonClientGone
	status := 499
	origin := constant.RelayCancelOriginDownstreamDisconnected
	message := "downstream stream delivery failed"
	if c != nil && c.Request != nil && errors.Is(c.Request.Context().Err(), context.DeadlineExceeded) {
		reason, status = relaycommon.StreamEndReasonRequestDeadline, http.StatusGatewayTimeout
		origin = constant.RelayCancelOriginGatewayDeadline
		message = "request deadline exceeded during stream delivery"
	}
	if c != nil {
		common.SetContextKey(c, constant.ContextKeyRelayCancelOrigin, origin)
	}
	result := types.NewErrorWithStatusCode(fmt.Errorf("%s: %w", message, err), types.ErrorCodeDoRequestFailed, status, types.ErrOptionWithSkipRetry())
	if info != nil {
		if info.StreamStatus == nil {
			info.StreamStatus = relaycommon.NewStreamStatus()
		}
		setDownstreamStreamEnd(c, info, reason, err)
		if info.ReceivedResponseCount > 0 && StreamStarted(c) {
			info.PartialStreamError = result
		}
	}
	return result
}
