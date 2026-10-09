package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const RelayCandidateSkipLimit = 32

const relayCandidateSkippedIDsKey = "relay_candidate_skipped_ids"

// NewRelayRetryStateForRequest keeps initial middleware selection and Relay
// inside the same elapsed budget without spending outbound attempt slots.
func NewRelayRetryStateForRequest(c *gin.Context, format types.RelayFormat, retryTimes int) *RelayRetryState {
	state := NewRelayRetryState(format, retryTimes)
	if c == nil {
		return state
	}
	if started := c.GetTime("relay_retry_started_at"); !started.IsZero() {
		state.StartedAt = started
	} else {
		c.Set("relay_retry_started_at", state.StartedAt)
	}
	return state
}

func relayCandidateSkippedIDs(c *gin.Context) []int {
	if c == nil {
		return nil
	}
	value, _ := c.Get(relayCandidateSkippedIDsKey)
	ids, _ := value.([]int)
	return ids
}

func RecordRelayCandidateSkip(c *gin.Context, channelID int) {
	if c == nil || channelID <= 0 {
		return
	}
	ids := relayCandidateSkippedIDs(c)
	if slices.Contains(ids, channelID) || len(ids) >= RelayCandidateSkipLimit {
		return
	}
	ids = append(append([]int(nil), ids...), channelID)
	c.Set(relayCandidateSkippedIDsKey, ids)
	c.Set("retry_candidate_skip_count", len(ids))
	c.Set("retry_candidate_skip_reason", "no_enabled_keys")
}

func RelayCandidateSelectionStopReason(c *gin.Context, state *RelayRetryState) string {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	if state != nil && !state.CanAttempt(ctx) {
		return state.StopReason
	}
	if c != nil && c.GetInt("retry_candidate_skip_count") >= RelayCandidateSkipLimit {
		if state != nil {
			state.StopReason = RetryStopReasonCandidateLimit
		}
		return RetryStopReasonCandidateLimit
	}
	return ""
}

// RelayCandidateSelectionError reports an initial-selection stop with fixed
// metadata. No credential, request payload or provider text is recorded.
func RelayCandidateSelectionError(c *gin.Context, state *RelayRetryState, reason string) *types.NewAPIError {
	status, message := http.StatusServiceUnavailable, "no usable channel is available for this request"
	switch reason {
	case RetryStopReasonClientGone:
		status, message = 499, "request canceled before an upstream attempt"
	case RetryStopReasonDeadlineExceeded:
		status, message = http.StatusGatewayTimeout, "request selection budget exhausted"
	case RetryStopReasonCandidateLimit:
		message = "request candidate selection limit reached"
	}
	if c != nil {
		c.Set("retry_stop_reason", reason)
		if state != nil {
			state.StopReason = reason
			logger.LogInfo(c, fmt.Sprintf("relay_selection_final attempts=%d stop_reason=%s elapsed_ms=%d max_elapsed_ms=%d candidate_skip_count=%d candidate_skip_limit=%d",
				state.Attempts, reason, time.Since(state.StartedAt).Milliseconds(), state.Policy.MaxElapsed.Milliseconds(), c.GetInt("retry_candidate_skip_count"), RelayCandidateSkipLimit))
		}
	}
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeGetChannelFailed, status, types.ErrOptionWithSkipRetry())
}
