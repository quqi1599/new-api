package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/types"
)

func TestRelayRetryStateBoundsRoundsAttemptsAndChannels(t *testing.T) {
	state := NewRelayRetryState(types.RelayFormatClaude, 2)
	state.Policy = RelayRetryPolicy{
		MaxRounds:       2,
		MaxAttempts:     4,
		MaxElapsed:      time.Minute,
		InitialBackoff:  10 * time.Millisecond,
		MaxRoundBackoff: 50 * time.Millisecond,
	}
	state.RecordAttempt(119)
	state.RecordAttempt(120)
	if state.Attempts != 2 || state.RoundAttempts != 2 || state.DistinctChannelCount() != 2 {
		t.Fatalf("unexpected retry state: %#v", state)
	}
	if !state.CanStartNextRound(context.Background()) {
		t.Fatal("second round should be available")
	}
	state.StartNextRound()
	if state.Round != 2 || state.RoundAttempts != 0 {
		t.Fatalf("unexpected second round state: %#v", state)
	}
	if state.CanStartNextRound(context.Background()) {
		t.Fatal("third round must be rejected")
	}
	if state.StopReason != RetryStopReasonRoundsExhausted {
		t.Fatalf("stop reason = %q", state.StopReason)
	}
}

func TestRelayRetryConfiguredAttemptLimitIsHardCap(t *testing.T) {
	oldValue, hadValue := os.LookupEnv("RELAY_RETRY_MAX_ATTEMPTS")
	if err := os.Setenv("RELAY_RETRY_MAX_ATTEMPTS", "3"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadValue {
			_ = os.Setenv("RELAY_RETRY_MAX_ATTEMPTS", oldValue)
		} else {
			_ = os.Unsetenv("RELAY_RETRY_MAX_ATTEMPTS")
		}
	})
	state := NewRelayRetryState(types.RelayFormatOpenAI, 100)
	if state.Policy.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want hard cap 3", state.Policy.MaxAttempts)
	}
}

func TestRelayRetryStateHonorsClientCancellation(t *testing.T) {
	state := NewRelayRetryState(types.RelayFormatOpenAI, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if state.CanAttempt(ctx) {
		t.Fatal("canceled client context must stop retries")
	}
	if state.StopReason != RetryStopReasonClientGone {
		t.Fatalf("stop reason = %q", state.StopReason)
	}
}
