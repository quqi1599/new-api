package model

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

var dirtySaaSTokenCaches sync.Map

func tokenCacheKey(key string) string { return "token:" + common.GenerateHMAC(key) }
func tokenCreditWatermarkKey(key string) string {
	return "token:saas-credit:" + common.GenerateHMAC(key)
}

func tokenQuotaRevisionKey(key string) string {
	return "token:saas-quota-revision:" + common.GenerateHMAC(key)
}

const populateTokenCacheLua = `
local snapshot = tonumber(ARGV[1])
local watermark = tonumber(redis.call('GET', KEYS[2]) or '0')
local current = tonumber(redis.call('HGET', KEYS[1], 'SaaSCreditedQuota') or '0')
local revision = tonumber(ARGV[3])
local revisionFloor = tonumber(redis.call('GET', KEYS[3]) or '0')
local currentRevision = tonumber(redis.call('HGET', KEYS[1], 'SaaSQuotaRevision') or '0')
if revision < math.max(revisionFloor, currentRevision) then return 0 end
local warm = redis.call('HEXISTS', KEYS[1], 'Id') == 1
local target = math.max(snapshot, watermark, current)
-- A newer explicit edit may race delivery of a later credit. Its assigned
-- balance needs that credit delta; an ordinary old read must simply be rejected.
if snapshot < target and (not warm or revision <= currentRevision) then return 0 end
local remain = redis.call('HGET', KEYS[1], 'RemainQuota')
if not warm then redis.call('DEL', KEYS[1]) end
redis.call('HSET', KEYS[1], unpack(ARGV, 4))
if warm and target > 0 and revision == currentRevision then
  -- Credit versions do not represent pending consumption. Preserve live quota.
  if remain then redis.call('HSET', KEYS[1], 'RemainQuota', remain) end
  if target > current then
    redis.call('HINCRBY', KEYS[1], 'RemainQuota', string.format('%.0f', target-current))
  end
elseif snapshot < target then
  redis.call('HINCRBY', KEYS[1], 'RemainQuota', string.format('%.0f', target-snapshot))
end
-- UsedQuota follows the database: this fork's consumer only updates cached
-- RemainQuota. Freezing UsedQuota would hide usage even after batch flush.
redis.call('HSET', KEYS[1], 'SaaSCreditedQuota', string.format('%.0f', target))
if target > watermark then redis.call('SET', KEYS[2], string.format('%.0f', target)) end
if revision > revisionFloor then redis.call('SET', KEYS[3], ARGV[3]) end
local ttl = tonumber(ARGV[2])
if ttl > 0 then redis.call('EXPIRE', KEYS[1], ttl) end
return 1`

// This is the credit-specific integration hook, not a new consumption engine.
func cachePopulateTokenForCredit(token Token) error {
	if !common.RedisEnabled {
		return nil
	}
	if common.RDB == nil {
		return fmt.Errorf("redis client is not initialized")
	}
	key := tokenCacheKey(token.Key)
	if pending, dirty := dirtySaaSTokenCaches.Load(key); dirty {
		if token.SaaSCreditedQuota < pending.(int64) {
			return nil
		}
		// A current DB read heals a failed delivery after Redis recovers, without
		// requiring another POST or leaving the token in permanent DB fallback.
		if err := cacheApplySaaSTokenCredit(token.Key, token.SaaSCreditedQuota); err != nil {
			return err
		}
	}
	watermarkKey := tokenCreditWatermarkKey(token.Key)
	revisionKey := tokenQuotaRevisionKey(token.Key)
	token.Clean()
	args := []interface{}{token.SaaSCreditedQuota, common.RedisKeyCacheSeconds(), token.SaaSQuotaRevision}
	v, typ := reflect.ValueOf(token), reflect.TypeOf(token)
	for i := 0; i < v.NumField(); i++ {
		field, value := typ.Field(i), v.Field(i)
		if field.Type.String() == "gorm.DeletedAt" {
			continue
		}
		if value.Kind() == reflect.Ptr {
			if value.IsNil() {
				args = append(args, field.Name, "")
				continue
			}
			value = value.Elem()
		}
		args = append(args, field.Name, fmt.Sprint(value.Interface()))
	}
	return common.RDB.Eval(context.Background(), populateTokenCacheLua, []string{key, watermarkKey, revisionKey}, args...).Err()
}

// A cumulative database credit watermark makes cache delivery idempotent and
// order-independent, including after a lost Redis acknowledgement. The delta
// is added to the live cached balance, preserving any pending usage deductions.
const applySaaSTokenCreditLua = `
local target = tonumber(ARGV[1])
local floor = tonumber(redis.call('GET', KEYS[2]) or '0')
if target > floor then
  redis.call('SET', KEYS[2], ARGV[1])
else
  target = floor
end
if redis.call('HEXISTS', KEYS[1], 'Id') == 0 then return 0 end
local current = tonumber(redis.call('HGET', KEYS[1], 'SaaSCreditedQuota') or '0')
if target > current then
  redis.call('HINCRBY', KEYS[1], 'RemainQuota', string.format('%.0f', target-current))
  redis.call('HSET', KEYS[1], 'SaaSCreditedQuota', string.format('%.0f', target))
  if redis.call('HGET', KEYS[1], 'Status') == ARGV[2] and tonumber(redis.call('HGET', KEYS[1], 'RemainQuota')) > 0 then
    redis.call('HSET', KEYS[1], 'Status', ARGV[3])
  end
end
return 1`

func cacheApplySaaSTokenCredit(key string, total int64) error {
	cacheKey := tokenCacheKey(key)
	for {
		pending, loaded := dirtySaaSTokenCaches.LoadOrStore(cacheKey, total)
		if !loaded {
			break
		}
		previous := pending.(int64)
		if previous >= total {
			total = previous
			break
		}
		if dirtySaaSTokenCaches.CompareAndSwap(cacheKey, previous, total) {
			break
		}
	}
	if common.RDB == nil {
		return fmt.Errorf("redis client is not initialized")
	}
	err := common.RDB.Eval(context.Background(), applySaaSTokenCreditLua,
		[]string{cacheKey, tokenCreditWatermarkKey(key)}, total,
		common.TokenStatusExhausted, common.TokenStatusEnabled).Err()
	if err == nil {
		dirtySaaSTokenCaches.CompareAndDelete(cacheKey, total)
	}
	return err
}
