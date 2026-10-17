package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestAggregateChannelCircuitBypassIgnoresExistingState(t *testing.T) {
	for _, backend := range []string{"local", "redis"} {
		for _, channelID := range []int{141, 142, 143} {
			for _, initialState := range []string{"missing", channelCircuitStateClosed, channelCircuitStateOpen, channelCircuitStateHalfOpen} {
				t.Run(fmt.Sprintf("%s/channel_%d/%s", backend, channelID, initialState), func(t *testing.T) {
					advance := setupAggregateChannelCircuitTest(t, backend == "redis")
					ctx := context.Background()
					modelName := "gpt-6.1-sol"
					relayErr := aggregateChannelCircuitFailure()

					// Seed the previous deployment's state through the public API.
					// Enabling bypass must work without deleting that persisted state.
					if initialState != "missing" {
						if RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
							t.Fatal("one failure must remain below the threshold")
						}
					}
					if initialState == channelCircuitStateOpen || initialState == channelCircuitStateHalfOpen {
						if !RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
							t.Fatal("threshold failure must create the old open circuit")
						}
						decision := ChannelCircuitAllowAttempt(ctx, channelID, modelName)
						if decision.Allowed || decision.State != channelCircuitStateOpen {
							t.Fatalf("expected seeded open circuit, got %#v", decision)
						}
					}
					if initialState == channelCircuitStateHalfOpen {
						advance(11 * time.Second)
						decision := ChannelCircuitAllowAttempt(ctx, channelID, modelName)
						if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
							t.Fatalf("expected seeded half-open probe, got %#v", decision)
						}
						if ChannelCircuitAllowAttempt(ctx, channelID, modelName).Allowed {
							t.Fatal("seeded half-open circuit must reject a second probe")
						}
					}

					defaultChannelCircuitConfig.BypassChannelIDs = parseChannelCircuitBypassIDs("9,141,142,143")
					before := snapshotAggregateChannelCircuit(t, channelID, modelName)
					for range 4 {
						advance(time.Second)
						for range 2 {
							decision := ChannelCircuitAllowAttempt(ctx, channelID, modelName)
							if !decision.Allowed || decision.State != channelCircuitStateClosed || decision.RetryAfter != 0 {
								t.Fatalf("aggregate channel must bypass old state: %#v", decision)
							}
						}
						if RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
							t.Fatal("aggregate channel failures must not open the outer circuit")
						}
					}
					// Only Allow/RecordFailure are asserted here. Success and probe
					// release may still clean up prior state as part of normal handling.
					after := snapshotAggregateChannelCircuit(t, channelID, modelName)
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("bypass accumulated failures, changed a probe, or extended the open window:\nbefore: %#v\nafter: %#v", before, after)
					}
				})
			}
		}
	}
}

func TestAggregateChannelCircuitBypassPreservesOrdinaryChannelProtection(t *testing.T) {
	for _, backend := range []string{"local", "redis"} {
		t.Run(backend, func(t *testing.T) {
			advance := setupAggregateChannelCircuitTest(t, backend == "redis")
			defaultChannelCircuitConfig.BypassChannelIDs = parseChannelCircuitBypassIDs("9,141,142,143")
			ctx := context.Background()
			const channelID = 119
			const modelName = "gpt-6.1-sol"
			relayErr := aggregateChannelCircuitFailure()

			if RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
				t.Fatal("ordinary channel must remain closed below the failure threshold")
			}
			decision := ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if !decision.Allowed || decision.State != channelCircuitStateClosed {
				t.Fatalf("expected closed ordinary channel, got %#v", decision)
			}
			if !RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
				t.Fatal("ordinary channel must open at the failure threshold")
			}
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if decision.Allowed || decision.State != channelCircuitStateOpen || decision.RetryAfter != 10*time.Second {
				t.Fatalf("ordinary channel lost its open protection: %#v", decision)
			}

			advance(11 * time.Second)
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
				t.Fatalf("ordinary channel must allow one recovery probe: %#v", decision)
			}
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if decision.Allowed || decision.State != channelCircuitStateHalfOpen || decision.RetryAfter != 5*time.Second {
				t.Fatalf("ordinary channel must reject another probe while leased: %#v", decision)
			}
			if !RecordChannelCircuitFailure(ctx, channelID, modelName, relayErr) {
				t.Fatal("a failed ordinary-channel probe must reopen the circuit")
			}
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if decision.Allowed || decision.State != channelCircuitStateOpen {
				t.Fatalf("failed recovery probe did not restore protection: %#v", decision)
			}

			advance(11 * time.Second)
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if !decision.Allowed || decision.State != channelCircuitStateHalfOpen {
				t.Fatalf("ordinary channel must allow the next recovery probe: %#v", decision)
			}
			RecordChannelCircuitSuccess(ctx, channelID, modelName)
			decision = ChannelCircuitAllowAttempt(ctx, channelID, modelName)
			if !decision.Allowed || decision.State != channelCircuitStateClosed {
				t.Fatalf("successful recovery probe must close the ordinary circuit: %#v", decision)
			}
		})
	}
}

func setupAggregateChannelCircuitTest(t *testing.T, useRedis bool) func(time.Duration) {
	t.Helper()
	oldRedisEnabled, oldRDB := common.RedisEnabled, common.RDB
	oldConfig, oldNow := defaultChannelCircuitConfig, channelCircuitNow
	common.RedisEnabled = useRedis
	var client *redis.Client
	if useRedis {
		server := miniredis.RunT(t)
		client = redis.NewClient(&redis.Options{Addr: server.Addr()})
		common.RDB = client
	}
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
		common.RedisEnabled, common.RDB = oldRedisEnabled, oldRDB
		defaultChannelCircuitConfig, channelCircuitNow = oldConfig, oldNow
		resetLocalChannelCircuitsForTest()
		if client != nil {
			_ = client.Close()
		}
	})
	return func(delta time.Duration) { now = now.Add(delta) }
}

func aggregateChannelCircuitFailure() *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New("no healthy upstream route"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
}

type aggregateChannelCircuitSnapshot struct {
	Local         *localChannelCircuitState
	RedisState    map[string]string
	RedisFailures string
	RedisProbe    string
	RedisTTLs     [3]time.Duration
}

func snapshotAggregateChannelCircuit(t *testing.T, channelID int, modelName string) aggregateChannelCircuitSnapshot {
	t.Helper()
	var snapshot aggregateChannelCircuitSnapshot
	localChannelCircuits.Lock()
	if state := localChannelCircuits.items[channelCircuitKey(channelID, modelName)]; state != nil {
		copy := *state
		snapshot.Local = &copy
	}
	localChannelCircuits.Unlock()
	if !common.RedisEnabled {
		return snapshot
	}
	ctx := context.Background()
	stateKey, failureKey, probeKey := channelCircuitKeys(channelID, modelName)
	var err error
	snapshot.RedisState, err = common.RDB.HGetAll(ctx, stateKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	for key, destination := range map[string]*string{failureKey: &snapshot.RedisFailures, probeKey: &snapshot.RedisProbe} {
		*destination, err = common.RDB.Get(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			t.Fatal(err)
		}
	}
	for i, key := range []string{stateKey, failureKey, probeKey} {
		snapshot.RedisTTLs[i], err = common.RDB.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}
