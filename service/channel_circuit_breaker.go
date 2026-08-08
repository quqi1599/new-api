package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

const (
	channelCircuitStateClosed   = "closed"
	channelCircuitStateOpen     = "open"
	channelCircuitStateHalfOpen = "half_open"
)

type ChannelCircuitDecision struct {
	Allowed    bool
	State      string
	RetryAfter time.Duration
}

type channelCircuitConfig struct {
	FailureThreshold int
	FailureWindow    time.Duration
	OpenDuration     time.Duration
	HalfOpenLease    time.Duration
	BypassChannelIDs map[int]struct{}
}

var defaultChannelCircuitConfig = channelCircuitConfig{
	FailureThreshold: common.GetEnvOrDefault("CHANNEL_CIRCUIT_FAILURE_THRESHOLD", 3),
	FailureWindow:    time.Duration(common.GetEnvOrDefault("CHANNEL_CIRCUIT_FAILURE_WINDOW_SECONDS", 60)) * time.Second,
	OpenDuration:     time.Duration(common.GetEnvOrDefault("CHANNEL_CIRCUIT_OPEN_SECONDS", 30)) * time.Second,
	HalfOpenLease:    time.Duration(common.GetEnvOrDefault("CHANNEL_CIRCUIT_HALF_OPEN_LEASE_SECONDS", 30)) * time.Second,
	BypassChannelIDs: parseChannelCircuitBypassIDs(common.GetEnvOrDefaultString("CHANNEL_CIRCUIT_BYPASS_CHANNEL_IDS", "")),
}

func parseChannelCircuitBypassIDs(raw string) map[int]struct{} {
	channelIDs := make(map[int]struct{})
	for _, item := range strings.Split(raw, ",") {
		channelID, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil || channelID <= 0 {
			continue
		}
		channelIDs[channelID] = struct{}{}
	}
	return channelIDs
}

func channelCircuitBypassed(config channelCircuitConfig, channelID int) bool {
	_, bypassed := config.BypassChannelIDs[channelID]
	return bypassed
}

func normalizedChannelCircuitConfig() channelCircuitConfig {
	config := defaultChannelCircuitConfig
	if config.FailureThreshold < 1 {
		config.FailureThreshold = 1
	}
	if config.FailureWindow < time.Millisecond {
		config.FailureWindow = time.Millisecond
	}
	if config.OpenDuration < time.Millisecond {
		config.OpenDuration = time.Millisecond
	}
	if config.HalfOpenLease < time.Millisecond {
		config.HalfOpenLease = time.Millisecond
	}
	return config
}

type localChannelCircuitState struct {
	State       string
	Failures    int
	WindowStart time.Time
	OpenUntil   time.Time
	ProbeUntil  time.Time
}

var localChannelCircuits = struct {
	sync.Mutex
	items map[string]*localChannelCircuitState
}{items: make(map[string]*localChannelCircuitState)}

var channelCircuitNow = time.Now

var channelCircuitAllowScript = `
local state = redis.call('HGET', KEYS[1], 'state')
if not state then
  return {1, 'closed', 0}
end
local now_ms = tonumber(ARGV[1])
local open_ms = tonumber(ARGV[2])
local lease_ms = tonumber(ARGV[3])
if state == 'open' then
  local until_ms = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
  if now_ms < until_ms then
    return {0, 'open', until_ms - now_ms}
  end
  if redis.call('SET', KEYS[3], '1', 'NX', 'PX', lease_ms) then
    redis.call('HSET', KEYS[1], 'state', 'half_open')
    redis.call('PEXPIRE', KEYS[1], lease_ms + open_ms)
    return {1, 'half_open', 0}
  end
  return {0, 'half_open', lease_ms}
end
if state == 'half_open' then
  local ttl = redis.call('PTTL', KEYS[3])
  if ttl > 0 then
    return {0, 'half_open', ttl}
  end
  local until_ms = now_ms + open_ms
  redis.call('HSET', KEYS[1], 'state', 'open', 'open_until', until_ms)
  redis.call('PEXPIRE', KEYS[1], open_ms + lease_ms)
  return {0, 'open', open_ms}
end
return {1, 'closed', 0}
`

var channelCircuitFailureScript = `
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
local now_ms = tonumber(ARGV[1])
local threshold = tonumber(ARGV[2])
local window_ms = tonumber(ARGV[3])
local open_ms = tonumber(ARGV[4])
local lease_ms = tonumber(ARGV[5])
local count = 0
if state == 'open' then
  local until_ms = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
  return {0, math.max(until_ms - now_ms, 0)}
elseif state == 'half_open' then
  count = threshold
else
  count = redis.call('INCR', KEYS[2])
  if count == 1 then
    redis.call('PEXPIRE', KEYS[2], window_ms)
  end
end
redis.call('DEL', KEYS[3])
if count >= threshold then
  local until_ms = now_ms + open_ms
  redis.call('HSET', KEYS[1], 'state', 'open', 'open_until', until_ms)
  redis.call('PEXPIRE', KEYS[1], open_ms + lease_ms + window_ms)
  redis.call('DEL', KEYS[2])
  return {1, open_ms}
end
return {0, count}
`

func channelCircuitKey(channelID int, modelName string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("newapi:channel-circuit:%s:%d", hex.EncodeToString(sum[:8]), channelID)
}

func channelCircuitKeys(channelID int, modelName string) (string, string, string) {
	base := channelCircuitKey(channelID, modelName)
	return base + ":state", base + ":failures", base + ":probe"
}

func ChannelCircuitAllowAttempt(ctx context.Context, channelID int, modelName string) ChannelCircuitDecision {
	if channelID <= 0 || strings.TrimSpace(modelName) == "" {
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateClosed}
	}
	config := normalizedChannelCircuitConfig()
	if channelCircuitBypassed(config, channelID) {
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateClosed}
	}
	if common.RedisEnabled && common.RDB != nil {
		stateKey, failureKey, probeKey := channelCircuitKeys(channelID, modelName)
		redisCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		result, err := common.RDB.Eval(
			redisCtx,
			channelCircuitAllowScript,
			[]string{stateKey, failureKey, probeKey},
			channelCircuitNow().UnixMilli(),
			config.OpenDuration.Milliseconds(),
			config.HalfOpenLease.Milliseconds(),
		).Result()
		if err == nil {
			if decision, ok := parseChannelCircuitDecision(result); ok {
				return decision
			}
		}
	}
	return localChannelCircuitAllow(channelID, modelName)
}

func parseChannelCircuitDecision(result any) (ChannelCircuitDecision, bool) {
	values, ok := result.([]any)
	if !ok || len(values) < 3 {
		return ChannelCircuitDecision{}, false
	}
	allowed, ok := redisInt64(values[0])
	if !ok {
		return ChannelCircuitDecision{}, false
	}
	state, ok := values[1].(string)
	if !ok {
		return ChannelCircuitDecision{}, false
	}
	retryMillis, ok := redisInt64(values[2])
	if !ok {
		return ChannelCircuitDecision{}, false
	}
	return ChannelCircuitDecision{
		Allowed:    allowed == 1,
		State:      state,
		RetryAfter: time.Duration(retryMillis) * time.Millisecond,
	}, true
}

func redisInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		return parsed, err == nil
	case []byte:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func localChannelCircuitAllow(channelID int, modelName string) ChannelCircuitDecision {
	key := channelCircuitKey(channelID, modelName)
	now := channelCircuitNow()
	config := normalizedChannelCircuitConfig()
	if channelCircuitBypassed(config, channelID) {
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateClosed}
	}
	localChannelCircuits.Lock()
	defer localChannelCircuits.Unlock()
	state := localChannelCircuits.items[key]
	if state == nil {
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateClosed}
	}
	switch state.State {
	case channelCircuitStateOpen:
		if now.Before(state.OpenUntil) {
			return ChannelCircuitDecision{Allowed: false, State: channelCircuitStateOpen, RetryAfter: state.OpenUntil.Sub(now)}
		}
		state.State = channelCircuitStateHalfOpen
		state.ProbeUntil = now.Add(config.HalfOpenLease)
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateHalfOpen}
	case channelCircuitStateHalfOpen:
		if now.Before(state.ProbeUntil) {
			return ChannelCircuitDecision{Allowed: false, State: channelCircuitStateHalfOpen, RetryAfter: state.ProbeUntil.Sub(now)}
		}
		state.State = channelCircuitStateOpen
		state.OpenUntil = now.Add(config.OpenDuration)
		return ChannelCircuitDecision{Allowed: false, State: channelCircuitStateOpen, RetryAfter: config.OpenDuration}
	default:
		return ChannelCircuitDecision{Allowed: true, State: channelCircuitStateClosed}
	}
}

func RecordChannelCircuitFailure(ctx context.Context, channelID int, modelName string, relayErr *types.NewAPIError) bool {
	if !ShouldCountChannelCircuitFailure(relayErr) || channelID <= 0 || strings.TrimSpace(modelName) == "" {
		return false
	}
	config := normalizedChannelCircuitConfig()
	if channelCircuitBypassed(config, channelID) {
		return false
	}
	if common.RedisEnabled && common.RDB != nil {
		stateKey, failureKey, probeKey := channelCircuitKeys(channelID, modelName)
		redisCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		result, err := common.RDB.Eval(
			redisCtx,
			channelCircuitFailureScript,
			[]string{stateKey, failureKey, probeKey},
			channelCircuitNow().UnixMilli(),
			config.FailureThreshold,
			config.FailureWindow.Milliseconds(),
			config.OpenDuration.Milliseconds(),
			config.HalfOpenLease.Milliseconds(),
		).Result()
		if err == nil {
			if values, ok := result.([]any); ok && len(values) > 0 {
				opened, parsed := redisInt64(values[0])
				return parsed && opened == 1
			}
			return false
		}
	}
	return localRecordChannelCircuitFailure(channelID, modelName)
}

func localRecordChannelCircuitFailure(channelID int, modelName string) bool {
	key := channelCircuitKey(channelID, modelName)
	now := channelCircuitNow()
	config := normalizedChannelCircuitConfig()
	localChannelCircuits.Lock()
	defer localChannelCircuits.Unlock()
	state := localChannelCircuits.items[key]
	if state == nil {
		state = &localChannelCircuitState{State: channelCircuitStateClosed, WindowStart: now}
		localChannelCircuits.items[key] = state
	}
	if state.State == channelCircuitStateOpen {
		return false
	}
	if state.State == channelCircuitStateHalfOpen {
		state.State = channelCircuitStateOpen
		state.OpenUntil = now.Add(config.OpenDuration)
		state.ProbeUntil = time.Time{}
		state.Failures = 0
		return true
	}
	if state.WindowStart.IsZero() || now.Sub(state.WindowStart) > config.FailureWindow {
		state.WindowStart = now
		state.Failures = 0
	}
	state.Failures++
	if state.Failures < config.FailureThreshold {
		return false
	}
	state.State = channelCircuitStateOpen
	state.OpenUntil = now.Add(config.OpenDuration)
	state.ProbeUntil = time.Time{}
	state.Failures = 0
	return true
}

func RecordChannelCircuitSuccess(ctx context.Context, channelID int, modelName string) {
	if channelID <= 0 || strings.TrimSpace(modelName) == "" {
		return
	}
	if common.RedisEnabled && common.RDB != nil {
		stateKey, failureKey, probeKey := channelCircuitKeys(channelID, modelName)
		redisCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := common.RDB.Del(redisCtx, stateKey, failureKey, probeKey).Err(); err == nil {
			localDeleteChannelCircuit(channelID, modelName)
			return
		}
	}
	localDeleteChannelCircuit(channelID, modelName)
}

func ReleaseChannelCircuitProbe(ctx context.Context, channelID int, modelName string) {
	if channelID <= 0 || strings.TrimSpace(modelName) == "" {
		return
	}
	if common.RedisEnabled && common.RDB != nil {
		_, _, probeKey := channelCircuitKeys(channelID, modelName)
		redisCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_ = common.RDB.Del(redisCtx, probeKey).Err()
	}
	key := channelCircuitKey(channelID, modelName)
	config := normalizedChannelCircuitConfig()
	localChannelCircuits.Lock()
	if state := localChannelCircuits.items[key]; state != nil && state.State == channelCircuitStateHalfOpen {
		state.State = channelCircuitStateOpen
		state.OpenUntil = channelCircuitNow().Add(config.OpenDuration)
		state.ProbeUntil = time.Time{}
	}
	localChannelCircuits.Unlock()
}

func localDeleteChannelCircuit(channelID int, modelName string) {
	key := channelCircuitKey(channelID, modelName)
	localChannelCircuits.Lock()
	delete(localChannelCircuits.items, key)
	localChannelCircuits.Unlock()
}

func ShouldCountChannelCircuitFailure(relayErr *types.NewAPIError) bool {
	if relayErr == nil || relayErr.StatusCode == 499 || !types.IsChannelPenaltyAllowed(relayErr) {
		return false
	}
	switch relayErr.GetErrorCode() {
	case types.ErrorCodeDoRequestFailed,
		types.ErrorCodeUpstreamConnectionTimeout,
		types.ErrorCodeUpstreamTLSHandshakeTimeout,
		types.ErrorCodeUpstreamRequestWriteTimeout,
		types.ErrorCodeUpstreamResponseHeaderTimeout,
		types.ErrorCodeUpstreamNonStreamTimeout,
		types.ErrorCodeUpstreamFirstEventTimeout,
		types.ErrorCodeUpstreamStreamIncomplete,
		types.ErrorCodeEmptyResponse,
		types.ErrorCodeReadResponseBodyFailed:
		return true
	}
	statusCode := relayErr.StatusCode
	return statusCode == http.StatusUnauthorized ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooEarly ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
}

func resetLocalChannelCircuitsForTest() {
	localChannelCircuits.Lock()
	localChannelCircuits.items = make(map[string]*localChannelCircuitState)
	localChannelCircuits.Unlock()
}
