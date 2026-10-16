package common

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGenerationDeliveryOverridesTerminalWithoutLosingUsage(t *testing.T) {
	for _, tc := range []struct {
		end           StreamEndReason
		state, reason string
	}{
		{StreamEndReasonDone, "incomplete", "max_output_tokens"},
		{StreamEndReasonClientGone, "cancelled", "client_gone"},
		{StreamEndReasonEOF, "failed", "eof"},
		{StreamEndReasonRequestDeadline, "failed", "request_deadline"},
		{StreamEndReasonHandlerStop, "failed", "handler_stop"},
	} {
		info := &RelayInfo{IsStream: true, StreamStatus: NewStreamStatus(), ResponsesOutcome: &ResponsesOutcome{State: "incomplete", Reason: "max_output_tokens", InputTokensSource: "reported", OutputTokensSource: "reported"}}
		info.StreamStatus.SetEndReason(tc.end, nil)
		info.FinalizeGenerationOutcome()
		require.Equal(t, tc.state, info.GenerationOutcome.State)
		require.Equal(t, tc.reason, info.GenerationOutcome.Reason)
		require.Equal(t, "incomplete", info.ResponsesOutcome.State)
		require.Equal(t, "max_output_tokens", info.ResponsesOutcome.Reason)
		require.Equal(t, "reported", info.ResponsesOutcome.InputTokensSource)
		require.Equal(t, "reported", info.ResponsesOutcome.OutputTokensSource)
		require.False(t, info.ShouldRecordChannelSuccess())
	}
}
