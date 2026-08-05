package service

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

const (
	RetryStopReasonSuccess           = "success"
	RetryStopReasonNotRetryable      = "not_retryable"
	RetryStopReasonOutputStarted     = "output_started"
	RetryStopReasonAttemptsExhausted = "attempts_exhausted"
	RetryStopReasonRoundsExhausted   = "rounds_exhausted"
	RetryStopReasonDeadlineExceeded  = "deadline_exceeded"
	RetryStopReasonClientGone        = "client_gone"
	RetryStopReasonNoChannel         = "no_channel"
)

type RelayRetryPolicy struct {
	MaxRounds       int
	MaxAttempts     int
	MaxElapsed      time.Duration
	InitialBackoff  time.Duration
	MaxRoundBackoff time.Duration
}

type RelayRetryState struct {
	Policy          RelayRetryPolicy
	StartedAt       time.Time
	Round           int
	Attempts        int
	RoundAttempts   int
	DistinctChannel map[int]struct{}
	StopReason      string
}

func NewRelayRetryState(relayFormat types.RelayFormat, retryTimes int) *RelayRetryState {
	maxRounds := common.GetEnvOrDefault("RELAY_RETRY_MAX_ROUNDS", 3)
	if maxRounds < 1 {
		maxRounds = 1
	}
	perRoundAttempts := retryTimes + 1
	if perRoundAttempts < 1 {
		perRoundAttempts = 1
	}
	configuredMaxAttempts := common.GetEnvOrDefault("RELAY_RETRY_MAX_ATTEMPTS", 30)
	maxAttempts := perRoundAttempts * maxRounds
	if configuredMaxAttempts > 0 && maxAttempts > configuredMaxAttempts {
		maxAttempts = configuredMaxAttempts
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	maxElapsedSeconds := common.GetEnvOrDefault("RELAY_RETRY_MAX_ELAPSED_SECONDS", 180)
	if relayFormat == types.RelayFormatOpenAIImage {
		maxElapsedSeconds = common.GetEnvOrDefault("RELAY_RETRY_IMAGE_MAX_ELAPSED_SECONDS", 600)
	}
	if maxElapsedSeconds < 1 {
		maxElapsedSeconds = 1
	}
	initialBackoff := common.GetEnvOrDefault("RELAY_RETRY_ROUND_BACKOFF_MS", 500)
	if initialBackoff < 0 {
		initialBackoff = 0
	}
	maxBackoff := common.GetEnvOrDefault("RELAY_RETRY_MAX_ROUND_BACKOFF_MS", 5000)
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}
	return &RelayRetryState{
		Policy: RelayRetryPolicy{
			MaxRounds:       maxRounds,
			MaxAttempts:     maxAttempts,
			MaxElapsed:      time.Duration(maxElapsedSeconds) * time.Second,
			InitialBackoff:  time.Duration(initialBackoff) * time.Millisecond,
			MaxRoundBackoff: time.Duration(maxBackoff) * time.Millisecond,
		},
		StartedAt:       time.Now(),
		Round:           1,
		DistinctChannel: make(map[int]struct{}),
	}
}

func (s *RelayRetryState) RecordAttempt(channelID int) {
	if s == nil {
		return
	}
	s.Attempts++
	s.RoundAttempts++
	if channelID > 0 {
		s.DistinctChannel[channelID] = struct{}{}
	}
}

func (s *RelayRetryState) RemainingAttempts() int {
	if s == nil {
		return 0
	}
	remaining := s.Policy.MaxAttempts - s.Attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *RelayRetryState) DistinctChannelCount() int {
	if s == nil {
		return 0
	}
	return len(s.DistinctChannel)
}

func (s *RelayRetryState) CanAttempt(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.StopReason = RetryStopReasonDeadlineExceeded
		} else {
			s.StopReason = RetryStopReasonClientGone
		}
		return false
	}
	if s.Attempts >= s.Policy.MaxAttempts {
		s.StopReason = RetryStopReasonAttemptsExhausted
		return false
	}
	if time.Since(s.StartedAt) >= s.Policy.MaxElapsed {
		s.StopReason = RetryStopReasonDeadlineExceeded
		return false
	}
	return true
}

func (s *RelayRetryState) CanStartNextRound(ctx context.Context) bool {
	if !s.CanAttempt(ctx) {
		return false
	}
	if s.Round >= s.Policy.MaxRounds {
		s.StopReason = RetryStopReasonRoundsExhausted
		return false
	}
	return true
}

func (s *RelayRetryState) StartNextRound() {
	if s == nil {
		return
	}
	s.Round++
	s.RoundAttempts = 0
}

func (s *RelayRetryState) NextRoundDelay(circuitRetryAfter time.Duration) time.Duration {
	if s == nil {
		return 0
	}
	exponent := s.Round - 1
	if exponent < 0 {
		exponent = 0
	}
	delayFloat := float64(s.Policy.InitialBackoff) * math.Pow(2, float64(exponent))
	delay := time.Duration(delayFloat)
	if circuitRetryAfter > delay {
		delay = circuitRetryAfter
	}
	if delay > s.Policy.MaxRoundBackoff {
		delay = s.Policy.MaxRoundBackoff
	}
	remaining := s.Policy.MaxElapsed - time.Since(s.StartedAt)
	if delay > remaining {
		delay = remaining
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func WaitForRelayRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if ctx == nil {
		<-timer.C
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
