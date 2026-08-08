package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestLocalChannelCircuitOpensAndAllowsSingleHalfOpenProbe(t *testing.T) {
	oldRedisEnabled := common.RedisEnabled
	oldConfig := defaultChannelCircuitConfig
	oldNow := channelCircuitNow
	common.RedisEnabled = false
	defaultChannelCircuitConfig = channelCircuitConfig{
		FailureThreshold: 2,
		FailureWindow:    time.Minute,
		OpenDuration:     10 * time.Second,
		HalfOpenLease:    5 * time.Second,
	}
	now := time.Unix(1_700_000_000, 0)
	channelCircuitNow = func() time.Time { return now }
	resetLocalChannelCircuitsForTest()
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		defaultChannelCircuitConfig = oldConfig
		channelCircuitNow = oldNow
		resetLocalChannelCircuitsForTest()
	})

	err := types.NewErrorWithStatusCode(errors.New("upstream 502"), types.ErrorCodeBadResponse, http.StatusBadGateway)
	if RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", err) {
		t.Fatal("first failure must not open the circuit")
	}
	if !RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", err) {
		t.Fatal("threshold failure must open the circuit")
	}

	decision := ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if decision.Allowed || decision.State != channelCircuitStateOpen || decision.RetryAfter != 10*time.Second {
		t.Fatalf("open decision = %#v", decision)
	}

	now = now.Add(11 * time.Second)
	decision = ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
		t.Fatalf("first half-open decision = %#v", decision)
	}
	decision = ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if decision.Allowed || decision.State != channelCircuitStateHalfOpen {
		t.Fatalf("second half-open decision = %#v", decision)
	}

	RecordChannelCircuitSuccess(context.Background(), 119, "claude-opus-4-8")
	decision = ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if !decision.Allowed || decision.State != channelCircuitStateClosed {
		t.Fatalf("closed decision = %#v", decision)
	}
}

func TestRedisChannelCircuitOpensAndRecovers(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldRedisEnabled := common.RedisEnabled
	oldRDB := common.RDB
	oldConfig := defaultChannelCircuitConfig
	oldNow := channelCircuitNow
	common.RedisEnabled = true
	common.RDB = client
	defaultChannelCircuitConfig = channelCircuitConfig{
		FailureThreshold: 2,
		FailureWindow:    time.Minute,
		OpenDuration:     10 * time.Second,
		HalfOpenLease:    5 * time.Second,
	}
	now := time.Unix(1_700_000_000, 0)
	channelCircuitNow = func() time.Time { return now }
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		common.RDB = oldRDB
		defaultChannelCircuitConfig = oldConfig
		channelCircuitNow = oldNow
		_ = client.Close()
	})

	relayErr := types.NewErrorWithStatusCode(errors.New("upstream 503"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
	if RecordChannelCircuitFailure(context.Background(), 9, "gpt-5.6-sol", relayErr) {
		t.Fatal("first Redis failure must not open the circuit")
	}
	if !RecordChannelCircuitFailure(context.Background(), 9, "gpt-5.6-sol", relayErr) {
		t.Fatal("second Redis failure must open the circuit")
	}
	decision := ChannelCircuitAllowAttempt(context.Background(), 9, "gpt-5.6-sol")
	if decision.Allowed || decision.State != channelCircuitStateOpen {
		t.Fatalf("open Redis decision = %#v", decision)
	}
	now = now.Add(11 * time.Second)
	decision = ChannelCircuitAllowAttempt(context.Background(), 9, "gpt-5.6-sol")
	if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
		t.Fatalf("half-open Redis decision = %#v", decision)
	}
	RecordChannelCircuitSuccess(context.Background(), 9, "gpt-5.6-sol")
	decision = ChannelCircuitAllowAttempt(context.Background(), 9, "gpt-5.6-sol")
	if !decision.Allowed || decision.State != channelCircuitStateClosed {
		t.Fatalf("closed Redis decision = %#v", decision)
	}
}

func TestChannelCircuitBypassSkipsOuterCircuit(t *testing.T) {
	oldRedisEnabled := common.RedisEnabled
	oldConfig := defaultChannelCircuitConfig
	common.RedisEnabled = false
	defaultChannelCircuitConfig = channelCircuitConfig{
		FailureThreshold: 1,
		FailureWindow:    time.Minute,
		OpenDuration:     30 * time.Second,
		HalfOpenLease:    30 * time.Second,
		BypassChannelIDs: parseChannelCircuitBypassIDs("9, 131,invalid,0,-1"),
	}
	resetLocalChannelCircuitsForTest()
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		defaultChannelCircuitConfig = oldConfig
		resetLocalChannelCircuitsForTest()
	})

	relayErr := types.NewErrorWithStatusCode(errors.New("upstream 503"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
	for range 3 {
		if RecordChannelCircuitFailure(context.Background(), 9, "gpt-5.6-luna", relayErr) {
			t.Fatal("bypassed channel must never open the outer circuit")
		}
	}
	decision := ChannelCircuitAllowAttempt(context.Background(), 9, "gpt-5.6-luna")
	if !decision.Allowed || decision.State != channelCircuitStateClosed {
		t.Fatalf("bypassed decision = %#v", decision)
	}
	if _, ok := defaultChannelCircuitConfig.BypassChannelIDs[131]; !ok {
		t.Fatal("comma-separated channel IDs must be parsed")
	}
}

func TestLocalOpenCircuitFailureDoesNotExtendOpenWindow(t *testing.T) {
	oldRedisEnabled := common.RedisEnabled
	oldConfig := defaultChannelCircuitConfig
	oldNow := channelCircuitNow
	common.RedisEnabled = false
	defaultChannelCircuitConfig = channelCircuitConfig{
		FailureThreshold: 1,
		FailureWindow:    time.Minute,
		OpenDuration:     10 * time.Second,
		HalfOpenLease:    5 * time.Second,
	}
	now := time.Unix(1_700_000_000, 0)
	channelCircuitNow = func() time.Time { return now }
	resetLocalChannelCircuitsForTest()
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		defaultChannelCircuitConfig = oldConfig
		channelCircuitNow = oldNow
		resetLocalChannelCircuitsForTest()
	})

	relayErr := types.NewErrorWithStatusCode(errors.New("upstream 503"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
	if !RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", relayErr) {
		t.Fatal("threshold failure must open the circuit")
	}
	now = now.Add(5 * time.Second)
	if RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", relayErr) {
		t.Fatal("an in-flight failure must not reopen an already-open circuit")
	}
	now = now.Add(6 * time.Second)
	decision := ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
		t.Fatalf("open window was unexpectedly extended: %#v", decision)
	}
}

func TestRedisOpenCircuitFailureDoesNotExtendOpenWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldRedisEnabled := common.RedisEnabled
	oldRDB := common.RDB
	oldConfig := defaultChannelCircuitConfig
	oldNow := channelCircuitNow
	common.RedisEnabled = true
	common.RDB = client
	defaultChannelCircuitConfig = channelCircuitConfig{
		FailureThreshold: 1,
		FailureWindow:    time.Minute,
		OpenDuration:     10 * time.Second,
		HalfOpenLease:    5 * time.Second,
	}
	now := time.Unix(1_700_000_000, 0)
	channelCircuitNow = func() time.Time { return now }
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		common.RDB = oldRDB
		defaultChannelCircuitConfig = oldConfig
		channelCircuitNow = oldNow
		_ = client.Close()
	})

	relayErr := types.NewErrorWithStatusCode(errors.New("upstream 503"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
	if !RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", relayErr) {
		t.Fatal("threshold failure must open the Redis circuit")
	}
	now = now.Add(5 * time.Second)
	if RecordChannelCircuitFailure(context.Background(), 119, "claude-opus-4-8", relayErr) {
		t.Fatal("an in-flight failure must not reopen an already-open Redis circuit")
	}
	now = now.Add(6 * time.Second)
	decision := ChannelCircuitAllowAttempt(context.Background(), 119, "claude-opus-4-8")
	if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
		t.Fatalf("Redis open window was unexpectedly extended: %#v", decision)
	}
}

func TestChannelCircuitFailureClassification(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
		want bool
	}{
		{name: "upstream 429", err: types.NewErrorWithStatusCode(errors.New("rate limit"), types.ErrorCodeBadResponse, http.StatusTooManyRequests), want: true},
		{name: "upstream 502", err: types.NewErrorWithStatusCode(errors.New("bad gateway"), types.ErrorCodeBadResponse, http.StatusBadGateway), want: true},
		{name: "client gone", err: types.NewErrorWithStatusCode(errors.New("gone"), types.ErrorCodeDoRequestFailed, 499, types.ErrOptionWithChannelPenalty()), want: false},
		{name: "invalid request", err: types.NewErrorWithStatusCode(errors.New("bad request"), types.ErrorCodeInvalidRequest, http.StatusBadRequest), want: false},
		{name: "plain skip retry", err: types.NewErrorWithStatusCode(errors.New("local"), types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry()), want: false},
		{name: "ambiguous transport penalty", err: types.NewErrorWithStatusCode(errors.New("reset"), types.ErrorCodeDoRequestFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
		{name: "connection timeout penalty", err: types.NewErrorWithStatusCode(errors.New("connect timeout"), types.ErrorCodeUpstreamConnectionTimeout, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
		{name: "tls handshake timeout penalty", err: types.NewErrorWithStatusCode(errors.New("tls timeout"), types.ErrorCodeUpstreamTLSHandshakeTimeout, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
		{name: "request write timeout penalty", err: types.NewErrorWithStatusCode(errors.New("write timeout"), types.ErrorCodeUpstreamRequestWriteTimeout, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
		{name: "response header timeout penalty", err: types.NewErrorWithStatusCode(errors.New("header timeout"), types.ErrorCodeUpstreamResponseHeaderTimeout, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
		{name: "non-stream total timeout penalty", err: types.NewErrorWithStatusCode(errors.New("body timeout"), types.ErrorCodeUpstreamNonStreamTimeout, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry(), types.ErrOptionWithChannelPenalty()), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldCountChannelCircuitFailure(tt.err); got != tt.want {
				t.Fatalf("ShouldCountChannelCircuitFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}
