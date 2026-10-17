package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGPTRetryBudgetUsesLatchedRequestDeadline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "")
			started := time.Now().Add(-181 * time.Second)
			deadline := started.Add(1200 * time.Second)
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol", IsStream: stream, StartTime: started}
			info.EnsureNonStreamDeadline(started, 1200*time.Second)
			info.SetFirstValidEventDeadline(deadline)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			state := NewRelayRetryState(types.RelayFormatOpenAIResponses, 2)
			state.StartedAt = started
			oldAttempts, oldRounds := state.Policy.MaxAttempts, state.Policy.MaxRounds
			state.RecordAttempt(9)
			require.False(t, state.CanAttempt(c.Request.Context()), "baseline 180s budget is exhausted")
			state.AlignGPTTextRequestBudget(c, info, types.RelayFormatOpenAIResponses)
			require.True(t, state.CanAttempt(c.Request.Context()), "original request deadline still permits recovery")
			require.WithinDuration(t, deadline, state.StartedAt.Add(state.Policy.MaxElapsed), time.Millisecond)
			require.Equal(t, started, state.StartedAt)
			require.Equal(t, oldAttempts, state.Policy.MaxAttempts)
			require.Equal(t, oldRounds, state.Policy.MaxRounds)
			state.StartNextRound()
			state.AlignGPTTextRequestBudget(c, info, types.RelayFormatOpenAIResponses)
			require.WithinDuration(t, deadline, state.StartedAt.Add(state.Policy.MaxElapsed), time.Millisecond, "a later round cannot restart the deadline")
		})
	}
}

func TestGPTRetryBudgetScopeAndExplicitOperatorLimit(t *testing.T) {
	for _, tc := range []struct {
		name, model, path, configured string
		format                        types.RelayFormat
	}{
		{"non GPT", "MiniMax-M3", "/v1/chat/completions", "", types.RelayFormatOpenAI},
		{"Claude entrance", "gpt-6.1-sol", "/v1/messages", "", types.RelayFormatClaude},
		{"image entrance", "gpt-image-2", "/v1/images/generations", "", types.RelayFormatOpenAIImage},
		{"compaction", "gpt-6.1-sol", "/v1/responses/compact", "", types.RelayFormatOpenAIResponsesCompaction},
		{"realtime", "gpt-6.1-sol", "/v1/realtime", "", types.RelayFormatOpenAIRealtime},
		{"explicit shorter", "gpt-6.1-sol", "/v1/responses", "90", types.RelayFormatOpenAIResponses},
		{"explicit longer", "gpt-6.1-sol", "/v1/responses", "1500", types.RelayFormatOpenAIResponses},
		{"explicit zero existing clamp", "gpt-6.1-sol", "/v1/responses", "0", types.RelayFormatOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", tc.configured)
			state := NewRelayRetryState(tc.format, 2)
			original := state.Policy
			info := &relaycommon.RelayInfo{OriginModelName: tc.model}
			info.EnsureNonStreamDeadline(time.Now(), 1200*time.Second)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			state.AlignGPTTextRequestBudget(c, info, tc.format)
			require.Equal(t, original, state.Policy)
		})
	}
}

func TestGPTRetryBudgetHonorsCallerAndExhaustedDeadline(t *testing.T) {
	t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "")
	for _, remaining := range []time.Duration{-time.Second, 50 * time.Second} {
		deadline := time.Now().Add(remaining)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
		state := NewRelayRetryState(types.RelayFormatOpenAI, 2)
		state.StartedAt = time.Now().Add(-200 * time.Second)
		info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol"}
		info.EnsureNonStreamDeadline(time.Now(), 1200*time.Second)
		state.AlignGPTTextRequestBudget(c, info, types.RelayFormatOpenAI)
		require.WithinDuration(t, deadline, state.StartedAt.Add(state.Policy.MaxElapsed), time.Millisecond)
		require.Equal(t, remaining > 0, state.CanAttempt(ctx))
		cancel()
		require.False(t, state.CanAttempt(ctx))
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	state := NewRelayRetryState(types.RelayFormatOpenAIResponses, 1)
	state.AlignGPTTextRequestBudget(c, &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol"}, types.RelayFormatOpenAIResponses)
	require.Equal(t, 180*time.Second, state.Policy.MaxElapsed, "an unbounded request keeps the finite default")
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol"}
	info.EnsureNonStreamDeadline(time.Now().Add(-2*time.Second), time.Second)
	state.AlignGPTTextRequestBudget(c, info, types.RelayFormatOpenAIResponses)
	require.False(t, state.CanAttempt(c.Request.Context()), "an expired request deadline cannot be revived")
}
