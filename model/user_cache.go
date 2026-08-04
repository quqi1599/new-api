package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"golang.org/x/sync/singleflight"
)

const userCacheSchemaVersion = 1

const userCacheLoadMaxAttempts = 3

var errUserCacheGenerationChanged = errors.New("user cache generation changed during database fallback")

var userCacheLoadGroup singleflight.Group
var dirtyUserAuthCaches sync.Map
var userCacheEpochInitializer struct {
	sync.Mutex
	initialized bool
}

type userCacheReadError struct {
	result string
	err    error
}

func (e *userCacheReadError) Error() string {
	return e.err.Error()
}

func (e *userCacheReadError) Unwrap() error {
	return e.err
}

func newUserCacheReadError(result string, err error) error {
	return &userCacheReadError{result: result, err: err}
}

func userCacheReadResult(err error) string {
	var readErr *userCacheReadError
	if errors.As(err, &readErr) {
		return readErr.result
	}
	return common.UserAuthCacheReadRedisError
}

// UserBase is the Redis-only user snapshot. CacheSchema deliberately has no
// database tag or migration counterpart; it only versions the hash layout.
type UserBase struct {
	Id          int    `json:"id"`
	Group       string `json:"group"`
	Email       string `json:"email"`
	Quota       int    `json:"quota"`
	Status      int    `json:"status"`
	Username    string `json:"username"`
	Setting     string `json:"setting"`
	CacheSchema int    `json:"-"`
}

func (user *UserBase) WriteContext(c *gin.Context) {
	common.SetContextKey(c, constant.ContextKeyUserGroup, user.Group)
	common.SetContextKey(c, constant.ContextKeyUserQuota, user.Quota)
	common.SetContextKey(c, constant.ContextKeyUserStatus, user.Status)
	common.SetContextKey(c, constant.ContextKeyUserEmail, user.Email)
	common.SetContextKey(c, constant.ContextKeyUserName, user.Username)
	common.SetContextKey(c, constant.ContextKeyUserSetting, user.GetSetting())
}

func (user *UserBase) GetSetting() dto.UserSetting {
	setting := dto.UserSetting{}
	if user.Setting != "" {
		err := common.Unmarshal([]byte(user.Setting), &setting)
		if err != nil {
			common.SysLog("failed to unmarshal setting: " + err.Error())
		}
	}
	return setting
}

// getUserCacheKey returns the key for user cache
func getUserCacheKey(userId int) string {
	return fmt.Sprintf("user:%d", userId)
}

func getUserCacheGenerationKey(userId int) string {
	return fmt.Sprintf("user:auth-generation:%d", userId)
}

func getUserCacheEpochKey() string {
	return "user:auth-cache-epoch"
}

// ensureUserCacheEpoch advances a process-start epoch exactly once after Redis
// becomes available. Every cached snapshot carries the current shared epoch,
// so a process restart cannot make a pre-restart stale hash trustworthy after
// an invalidation failure. Other live instances read the shared epoch on every
// cache operation and converge without keeping process-local epoch values.
func ensureUserCacheEpoch() error {
	if !common.RedisEnabled {
		return nil
	}
	if common.RDB == nil {
		return fmt.Errorf("redis client is not initialized")
	}

	userCacheEpochInitializer.Lock()
	defer userCacheEpochInitializer.Unlock()
	if userCacheEpochInitializer.initialized {
		return nil
	}
	if err := common.RDB.Incr(context.Background(), getUserCacheEpochKey()).Err(); err != nil {
		return fmt.Errorf("failed to initialize user cache epoch: %w", err)
	}
	userCacheEpochInitializer.initialized = true
	return nil
}

func getUserCacheEpoch() (int64, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	if err := ensureUserCacheEpoch(); err != nil {
		return 0, err
	}
	value, err := common.RDB.Get(context.Background(), getUserCacheEpochKey()).Result()
	if err != nil {
		return 0, err
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch <= 0 {
		return 0, fmt.Errorf("invalid user cache epoch %q", value)
	}
	return epoch, nil
}

func resetUserCacheEpochInitializationForTest() {
	userCacheEpochInitializer.Lock()
	userCacheEpochInitializer.initialized = false
	userCacheEpochInitializer.Unlock()
}

// MarkUserCacheDirty makes the current process distrust this user's Redis
// authentication snapshot. Authoritative mutation paths call it before the DB
// write when possible; a failed Redis fence therefore degrades to DB-only
// authentication instead of continuing to trust an old enabled hash.
func MarkUserCacheDirty(userId int) {
	if userId > 0 {
		dirtyUserAuthCaches.Store(userId, struct{}{})
	}
}

func isUserCacheDirty(userId int) bool {
	_, dirty := dirtyUserAuthCaches.Load(userId)
	return dirty
}

const bumpUserCacheGenerationLua = `
local generation = redis.call('INCR', KEYS[2])
redis.call('DEL', KEYS[1])
return generation`

// bumpUserCacheGenerationAndDelete atomically fences every in-flight database
// snapshot before deleting the derived user hash. The generation key is kept
// without a TTL: expiring it could let an arbitrarily delayed old snapshot
// become current again.
func bumpUserCacheGenerationAndDelete(userId int) error {
	if !common.RedisEnabled {
		MarkUserCacheDirty(userId)
		return nil
	}
	if common.RDB == nil {
		MarkUserCacheDirty(userId)
		return fmt.Errorf("redis client is not initialized")
	}
	if err := ensureUserCacheEpoch(); err != nil {
		MarkUserCacheDirty(userId)
		return err
	}
	err := common.RDB.Eval(
		context.Background(),
		bumpUserCacheGenerationLua,
		[]string{getUserCacheKey(userId), getUserCacheGenerationKey(userId)},
	).Err()
	if err != nil {
		MarkUserCacheDirty(userId)
		return err
	}
	dirtyUserAuthCaches.Delete(userId)
	return nil
}

// invalidateUserCache clears user cache
func invalidateUserCache(userId int) error {
	return bumpUserCacheGenerationAndDelete(userId)
}

// InvalidateUserCache is the exported version of invalidateUserCache.
// 供 controller 等上层包在用户状态变更（如禁用、删除、角色变更）后主动清理缓存。
func InvalidateUserCache(userId int) error {
	return invalidateUserCache(userId)
}

func getUserCacheGeneration(userId int) (int64, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	if common.RDB == nil {
		return 0, fmt.Errorf("redis client is not initialized")
	}
	value, err := common.RDB.Get(context.Background(), getUserCacheGenerationKey(userId)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	generation, err := strconv.ParseInt(value, 10, 64)
	if err != nil || generation < 0 {
		return 0, fmt.Errorf("invalid user cache generation %q", value)
	}
	return generation, nil
}

const populateUserCacheLua = `
local current = redis.call('GET', KEYS[2]) or '0'
if current ~= ARGV[1] then
  return 0
end
local epoch = redis.call('GET', KEYS[3]) or '0'
if epoch ~= ARGV[2] then
  return 0
end
redis.call('HSET', KEYS[1],
  'Id', ARGV[3], 'Group', ARGV[4], 'Email', ARGV[5],
  'Quota', ARGV[6], 'Status', ARGV[7], 'Username', ARGV[8],
  'Setting', ARGV[9], 'CacheSchema', ARGV[10], 'CacheEpoch', ARGV[2])
local ttl = tonumber(ARGV[11])
if ttl ~= nil and ttl > 0 then
  redis.call('EXPIRE', KEYS[1], ttl)
end
return 1`

func populateUserCacheAtGeneration(user User, expectedGeneration int64) (bool, error) {
	if !common.RedisEnabled {
		return false, nil
	}
	if common.RDB == nil {
		return false, fmt.Errorf("redis client is not initialized")
	}
	epoch, err := getUserCacheEpoch()
	if err != nil {
		return false, err
	}

	base := user.ToBaseUser()
	result, err := common.RDB.Eval(
		context.Background(),
		populateUserCacheLua,
		[]string{getUserCacheKey(user.Id), getUserCacheGenerationKey(user.Id), getUserCacheEpochKey()},
		expectedGeneration,
		epoch,
		base.Id,
		base.Group,
		base.Email,
		base.Quota,
		base.Status,
		base.Username,
		base.Setting,
		base.CacheSchema,
		common.RedisKeyCacheSeconds(),
	).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func populateUserCache(user User) error {
	generation, err := getUserCacheGeneration(user.Id)
	if err != nil {
		return err
	}
	applied, err := populateUserCacheAtGeneration(user, generation)
	if err != nil {
		return err
	}
	if !applied && common.RedisEnabled {
		return errUserCacheGenerationChanged
	}
	return nil
}

// updateUserCache follows an authoritative user-row mutation. It fences and
// invalidates the derived snapshot so an older DB fallback cannot overwrite
// the new authentication state.
func updateUserCache(user User) error {
	// This function follows an authoritative database mutation. Fence and delete
	// the old snapshot instead of applying several field writes that could race
	// with an older database fallback. The next read repopulates one full hash.
	return bumpUserCacheGenerationAndDelete(user.Id)
}

// GetUserCache returns a complete Redis snapshot or synchronously falls back
// to the database. A cached disabled status is always rechecked against the
// database so a stale denial cannot be turned into a false 403.
func GetUserCache(userId int) (*UserBase, error) {
	if !common.RedisEnabled {
		return loadUserCacheFromDB(userId, "", false)
	}
	if isUserCacheDirty(userId) {
		common.RecordUserAuthCacheRead(common.UserAuthCacheReadRedisError)
		return loadUserCacheFromDB(userId, common.UserAuthCacheReadRedisError, true)
	}

	userCache, err := cacheGetUserBase(userId)
	if err == nil && userCache.Status == common.UserStatusEnabled {
		return userCache, nil
	}

	reason := common.UserAuthDBFallbackDisabledRecheck
	if err != nil {
		reason = userCacheReadResult(err)
	}
	return loadUserCacheFromDB(userId, reason, true)
}

func loadUserCacheFromDB(userId int, reason string, recordFallback bool) (*UserBase, error) {
	if recordFallback {
		common.RecordUserAuthDBFallback(reason)
	}

	loadKey := strconv.Itoa(userId) + ":fenced"
	if reason == common.UserAuthCacheReadRedisError {
		// A dirty/Redis-error DB-only read must never share an in-flight result
		// with a cache-fill read that started before the authoritative mutation.
		loadKey = strconv.Itoa(userId) + ":db-only"
	}
	value, err, _ := userCacheLoadGroup.Do(loadKey, func() (interface{}, error) {
		// A Redis read failure already proved the cache backend unavailable. Avoid
		// a second network timeout and use the database result without caching.
		useRedisFence := common.RedisEnabled && reason != common.UserAuthCacheReadRedisError

		for attempt := 0; attempt < userCacheLoadMaxAttempts; attempt++ {
			var expectedGeneration int64
			if useRedisFence {
				generation, generationErr := getUserCacheGeneration(userId)
				if generationErr != nil {
					useRedisFence = false
				} else {
					expectedGeneration = generation
				}
			}

			user, dbErr := GetUserById(userId, false)
			if dbErr != nil {
				return nil, dbErr
			}
			if user.Status != common.UserStatusEnabled && user.Status != common.UserStatusDisabled {
				return nil, fmt.Errorf("database returned invalid user status %d", user.Status)
			}

			userCache := user.ToBaseUser()
			if !useRedisFence {
				return *userCache, nil
			}

			applied, cacheErr := populateUserCacheAtGeneration(*user, expectedGeneration)
			if cacheErr != nil {
				// Redis is derived state. A backend failure must not turn a valid DB
				// result into an authentication failure or trigger another timeout.
				common.SysLog("failed to synchronously populate user cache: " + cacheErr.Error())
				return *userCache, nil
			}
			if applied {
				return *userCache, nil
			}
			// An authoritative mutation changed the generation after our DB read.
			// Retry so the response and the cache both use a post-mutation row.
		}

		return nil, errUserCacheGenerationChanged
	})
	if err != nil {
		if recordFallback {
			common.RecordUserAuthDBFallbackFailure(reason)
		}
		return nil, err
	}

	userCache := value.(UserBase)
	return &userCache, nil
}

func cacheGetUserBase(userId int) (*UserBase, error) {
	result, err := readUserCacheHash(userId)
	if common.RedisEnabled {
		if err != nil {
			common.RecordUserAuthCacheRead(userCacheReadResult(err))
		} else {
			common.RecordUserAuthCacheRead(common.UserAuthCacheReadHit)
		}
	}
	return result, err
}

const readUserCacheLua = `
local result = {redis.call('GET', KEYS[2]) or '0'}
local values = redis.call('HGETALL', KEYS[1])
for index = 1, #values do
  result[#result + 1] = values[index]
end
return result`

func readUserCacheHash(userId int) (*UserBase, error) {
	if !common.RedisEnabled {
		return nil, newUserCacheReadError(common.UserAuthCacheReadMiss, fmt.Errorf("redis is not enabled"))
	}
	if common.RDB == nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadRedisError, fmt.Errorf("redis client is not initialized"))
	}
	if err := ensureUserCacheEpoch(); err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadRedisError, err)
	}

	rawValues, err := common.RDB.Eval(
		context.Background(),
		readUserCacheLua,
		[]string{getUserCacheKey(userId), getUserCacheEpochKey()},
	).Slice()
	if err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadRedisError, fmt.Errorf("failed to load user cache: %w", err))
	}
	if len(rawValues) == 0 {
		return nil, newUserCacheReadError(common.UserAuthCacheReadRedisError, fmt.Errorf("user cache epoch result is empty"))
	}
	currentEpoch, ok := rawValues[0].(string)
	if !ok {
		return nil, newUserCacheReadError(common.UserAuthCacheReadRedisError, fmt.Errorf("invalid user cache epoch result"))
	}
	values := make(map[string]string, (len(rawValues)-1)/2)
	if (len(rawValues)-1)%2 != 0 {
		return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache hash result"))
	}
	for index := 1; index < len(rawValues); index += 2 {
		field, fieldOK := rawValues[index].(string)
		value, valueOK := rawValues[index+1].(string)
		if !fieldOK || !valueOK {
			return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache field encoding"))
		}
		values[field] = value
	}
	if len(values) == 0 {
		return nil, newUserCacheReadError(common.UserAuthCacheReadMiss, fmt.Errorf("user cache not found"))
	}

	for _, field := range []string{"Id", "Status", "Group", "Quota", "CacheSchema", "CacheEpoch"} {
		if _, present := values[field]; !present {
			return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("user cache missing required field %s", field))
		}
	}

	cachedID, err := strconv.Atoi(values["Id"])
	if err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache Id: %w", err))
	}
	cachedStatus, err := strconv.Atoi(values["Status"])
	if err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache Status: %w", err))
	}
	cachedQuota, err := strconv.Atoi(values["Quota"])
	if err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache Quota: %w", err))
	}
	cacheSchema, err := strconv.Atoi(values["CacheSchema"])
	if err != nil {
		return nil, newUserCacheReadError(common.UserAuthCacheReadPartial, fmt.Errorf("invalid user cache CacheSchema: %w", err))
	}

	if cachedID != userId {
		return nil, newUserCacheReadError(common.UserAuthCacheReadStale, fmt.Errorf("user cache id mismatch"))
	}
	if cachedStatus != common.UserStatusEnabled && cachedStatus != common.UserStatusDisabled {
		return nil, newUserCacheReadError(common.UserAuthCacheReadStale, fmt.Errorf("user cache status is invalid"))
	}
	if cacheSchema != userCacheSchemaVersion {
		return nil, newUserCacheReadError(common.UserAuthCacheReadStale, fmt.Errorf("user cache schema is stale"))
	}
	if values["CacheEpoch"] != currentEpoch {
		return nil, newUserCacheReadError(common.UserAuthCacheReadStale, fmt.Errorf("user cache epoch is stale"))
	}

	return &UserBase{
		Id:          cachedID,
		Group:       values["Group"],
		Email:       values["Email"],
		Quota:       cachedQuota,
		Status:      cachedStatus,
		Username:    values["Username"],
		Setting:     values["Setting"],
		CacheSchema: cacheSchema,
	}, nil
}

const mutateCompleteUserCacheLua = `
local values = redis.call('HMGET', KEYS[1], 'Id', 'Status', 'Group', 'Quota', 'CacheSchema', 'CacheEpoch')
for index = 1, 6 do
  if values[index] == false then
    return 0
  end
end
if values[1] ~= ARGV[1] then
  return 0
end
if values[2] ~= '1' and values[2] ~= '2' then
  return 0
end
if string.match(values[4], '^%-?%d+$') == nil then
  return 0
end
if values[5] ~= ARGV[2] then
  return 0
end
local epoch = redis.call('GET', KEYS[2]) or '0'
if values[6] ~= epoch then
  return 0
end
if ARGV[3] == 'quota_delta' then
  redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[4])
  return 1
end
if ARGV[3] == 'field_set' then
  if ARGV[4] ~= 'Status' and ARGV[4] ~= 'Quota' and ARGV[4] ~= 'Group' and ARGV[4] ~= 'Email' and ARGV[4] ~= 'Username' and ARGV[4] ~= 'Setting' then
    return redis.error_reply('unsupported user cache field')
  end
  if ARGV[4] == 'Status' and ARGV[5] ~= '1' and ARGV[5] ~= '2' then
    return redis.error_reply('invalid user cache status')
  end
  if ARGV[4] == 'Quota' and string.match(ARGV[5], '^%-?%d+$') == nil then
    return redis.error_reply('invalid user cache quota')
  end
  redis.call('HSET', KEYS[1], ARGV[4], ARGV[5])
  return 1
end
return redis.error_reply('unsupported user cache mutation')`

func mutateCompleteUserCache(userId int, operation string, args ...interface{}) error {
	if !common.RedisEnabled {
		return nil
	}
	if common.RDB == nil {
		common.RecordUserCacheMutation(common.UserCacheMutationError)
		return fmt.Errorf("redis client is not initialized")
	}
	if err := ensureUserCacheEpoch(); err != nil {
		common.RecordUserCacheMutation(common.UserCacheMutationError)
		return err
	}

	redisArgs := []interface{}{userId, userCacheSchemaVersion, operation}
	redisArgs = append(redisArgs, args...)
	result, err := common.RDB.Eval(
		context.Background(),
		mutateCompleteUserCacheLua,
		[]string{getUserCacheKey(userId), getUserCacheEpochKey()},
		redisArgs...,
	).Int()
	if err != nil {
		common.RecordUserCacheMutation(common.UserCacheMutationError)
		return fmt.Errorf("failed to mutate complete user cache: %w", err)
	}
	if result == 0 {
		common.RecordUserCacheMutation(common.UserCacheMutationSkipped)
		return nil
	}
	common.RecordUserCacheMutation(common.UserCacheMutationApplied)
	return nil
}

// cacheIncrUserQuota changes quota only when the complete, current-schema hash
// already exists. It never creates a fragment and intentionally leaves TTL
// untouched.
func cacheIncrUserQuota(userId int, delta int64) error {
	return mutateCompleteUserCache(userId, "quota_delta", delta)
}

func cacheDecrUserQuota(userId int, delta int64) error {
	return cacheIncrUserQuota(userId, -delta)
}

// Helper functions to get individual fields if needed
func getUserGroupCache(userId int) (string, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return "", err
	}
	return cache.Group, nil
}

func getUserQuotaCache(userId int) (int, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return 0, err
	}
	return cache.Quota, nil
}

func getUserStatusCache(userId int) (int, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return 0, err
	}
	return cache.Status, nil
}

func getUserNameCache(userId int) (string, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return "", err
	}
	return cache.Username, nil
}

func getUserSettingCache(userId int) (dto.UserSetting, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return dto.UserSetting{}, err
	}
	return cache.GetSetting(), nil
}

// New functions for individual field updates
func updateUserStatusCache(userId int, status bool) error {
	MarkUserCacheDirty(userId)
	return bumpUserCacheGenerationAndDelete(userId)
}

func updateUserQuotaCache(userId int, quota int) error {
	return mutateCompleteUserCache(userId, "field_set", "Quota", quota)
}

func UpdateUserGroupCache(userId int, _ string) error {
	// Subscription group changes are authoritative permission mutations. Fence
	// every in-flight snapshot and reload the full row instead of allowing an
	// older fallback to overwrite the new group afterwards.
	MarkUserCacheDirty(userId)
	return bumpUserCacheGenerationAndDelete(userId)
}

func updateUserEmailCache(userId int, email string) error {
	return mutateCompleteUserCache(userId, "field_set", "Email", email)
}

func updateUserNameCache(userId int, username string) error {
	return mutateCompleteUserCache(userId, "field_set", "Username", username)
}

func updateUserSettingCache(userId int, setting string) error {
	return mutateCompleteUserCache(userId, "field_set", "Setting", setting)
}

// GetUserLanguage returns the user's language preference from cache
// Uses the existing GetUserCache mechanism for efficiency
func GetUserLanguage(userId int) string {
	userCache, err := GetUserCache(userId)
	if err != nil {
		return ""
	}
	return userCache.GetSetting().Language
}
