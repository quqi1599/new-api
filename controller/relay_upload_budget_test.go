package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

type beforeUploadReader struct {
	io.Reader
	once   sync.Once
	before func()
}

func (r *beforeUploadReader) Read(p []byte) (int, error) {
	r.once.Do(r.before)
	return r.Reader.Read(p)
}

func TestDistributorUploadDoesNotSpendSelectionBudget(t *testing.T) {
	t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "1")
	var uploadFinished time.Time
	got := runDistributorResilience(t, distributorResilienceFixture{
		candidates: []string{"empty", "healthy"},
		bodyReader: func(r io.Reader) io.Reader {
			return &beforeUploadReader{Reader: r, before: func() {
				time.Sleep(1100 * time.Millisecond)
				uploadFinished = time.Now()
			}}
		},
	})
	require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
	require.Equal(t, []int{10}, got.calls)
	require.Equal(t, 1, got.context.GetInt("retry_candidate_skip_count"))
	require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
	require.False(t, got.context.GetTime("relay_retry_started_at").Before(uploadFinished))
	require.GreaterOrEqual(t, got.context.GetInt64("relay_request_intake_ms"), int64(1100))
	require.Contains(t, got.response.Body.String(), `"type":"response.completed"`)
}

func TestDistributorUploadStillHonorsRequestCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			defer cancel()
			got := runDistributorResilience(t, distributorResilienceFixture{
				candidates: []string{"healthy"}, requestContext: ctx,
				bodyReader: func(r io.Reader) io.Reader {
					return &beforeUploadReader{Reader: r, before: cancel}
				},
			})
			status, reason := 499, service.RetryStopReasonClientGone
			if deadline {
				status, reason = http.StatusGatewayTimeout, service.RetryStopReasonDeadlineExceeded
			}
			require.Equal(t, status, got.response.Code, got.response.Body.String())
			require.Equal(t, reason, got.context.GetString("retry_stop_reason"))
			require.Empty(t, got.calls)
		})
	}
}

func TestDistributorChatUploadKeepsBothResponseModes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "1")
			got := runDistributorResilience(t, distributorResilienceFixture{
				chat: true, nonStream: !stream, candidates: []string{"healthy"},
				bodyReader: func(r io.Reader) io.Reader {
					return &beforeUploadReader{Reader: r, before: func() { time.Sleep(1100 * time.Millisecond) }}
				},
			})
			require.Equal(t, http.StatusOK, got.response.Code, got.response.Body.String())
			require.Equal(t, []int{9}, got.calls)
			require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
			require.Contains(t, got.response.Body.String(), "synthetic-ok")
			if stream {
				require.Contains(t, got.response.Body.String(), "data: [DONE]")
			} else {
				require.Contains(t, got.response.Body.String(), `"finish_reason":"stop"`)
			}
		})
	}
}

func TestDistributorUploadDoesNotRenewExpiredRelayBudget(t *testing.T) {
	t.Setenv("RELAY_RETRY_MAX_ELAPSED_SECONDS", "1")
	got := runDistributorResilience(t, distributorResilienceFixture{
		candidates: []string{"error", "healthy"}, errorDelay: 1100 * time.Millisecond,
	})
	require.Equal(t, []int{9}, got.calls, "an expired selection budget must prevent fallback")
	require.Equal(t, 1, got.context.GetInt("retry_attempt_no"))
	require.Equal(t, service.RetryStopReasonDeadlineExceeded, got.context.GetString("retry_stop_reason"))
	require.GreaterOrEqual(t, got.response.Code, http.StatusBadRequest)
}
