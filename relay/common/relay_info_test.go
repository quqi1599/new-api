package common

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoBoundsFirstEventWaitBySharedDeadline(t *testing.T) {
	info := &RelayInfo{}
	info.SetFirstValidEventDeadline(time.Now().Add(100 * time.Millisecond))

	remaining, limited := info.RemainingFirstValidEventBudget()
	require.True(t, limited)
	require.Positive(t, remaining)
	require.LessOrEqual(t, remaining, 100*time.Millisecond)
	require.LessOrEqual(t, info.BoundFirstValidEventWait(time.Second), 100*time.Millisecond)
	require.Equal(t, 25*time.Millisecond, info.BoundFirstValidEventWait(25*time.Millisecond))
}

func TestRelayInfoWithoutSharedDeadlineKeepsPhaseTimeout(t *testing.T) {
	info := &RelayInfo{}
	require.Equal(t, 3*time.Second, info.BoundFirstValidEventWait(3*time.Second))
}

func TestRelayInfoEnsureFirstEventDeadlineUsesOriginalStartAndDoesNotReplaceIt(t *testing.T) {
	startTime := time.Now().Add(-250 * time.Millisecond)
	info := &RelayInfo{StartTime: startTime}
	info.EnsureFirstValidEventDeadline(time.Time{}, time.Second)

	remaining, limited := info.RemainingFirstValidEventBudget()
	require.True(t, limited)
	require.Positive(t, remaining)
	require.Less(t, remaining, 800*time.Millisecond,
		"dynamic stream budget must include time already spent waiting for response headers")

	info.EnsureFirstValidEventDeadline(time.Now(), 10*time.Second)
	remainingAfterSecondEnsure, limited := info.RemainingFirstValidEventBudget()
	require.True(t, limited)
	require.LessOrEqual(t, remainingAfterSecondEnsure, remaining,
		"a retry or late stream detection must not reset the request-wide deadline")
}

func TestRelayInfoNonStreamDeadlineUsesOriginalStartAndDoesNotReset(t *testing.T) {
	startTime := time.Now().Add(-250 * time.Millisecond)
	info := &RelayInfo{StartTime: startTime}
	info.EnsureNonStreamDeadline(time.Time{}, time.Second)

	remaining, limited := info.RemainingNonStreamBudget()
	require.True(t, limited)
	require.Positive(t, remaining)
	require.Less(t, remaining, 800*time.Millisecond)

	info.EnsureNonStreamDeadline(time.Now(), 10*time.Second)
	remainingAfterRetry, limited := info.RemainingNonStreamBudget()
	require.True(t, limited)
	require.LessOrEqual(t, remainingAfterRetry, remaining,
		"a preflight or channel retry must not reset the non-stream request deadline")
}

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}
