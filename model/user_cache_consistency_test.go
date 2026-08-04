package model

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupUserCacheConsistencyTest(t *testing.T, user User) *miniredis.Miniredis {
	t.Helper()
	dirtyUserAuthCaches.Delete(user.Id)
	resetUserCacheEpochInitializationForTest()
	require.NoError(t, DB.Exec("DELETE FROM users").Error)
	require.NoError(t, DB.Create(&user).Error)

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	oldRedisEnabled := common.RedisEnabled
	oldSyncFrequency := common.SyncFrequency
	common.RDB = client
	common.RedisEnabled = true
	common.SyncFrequency = 300
	common.ResetUserCacheStatsForTest()

	t.Cleanup(func() {
		dirtyUserAuthCaches.Delete(user.Id)
		resetUserCacheEpochInitializationForTest()
		_ = client.Close()
		common.RDB = oldClient
		common.RedisEnabled = oldRedisEnabled
		common.SyncFrequency = oldSyncFrequency
		common.ResetUserCacheStatsForTest()
		_ = DB.Exec("DELETE FROM users").Error
	})
	return server
}

func newUserCacheConsistencyUser(id int, status int, quota int) User {
	return User{
		Id:       id,
		Username: "user-cache-" + strconv.Itoa(id),
		Password: "password",
		Group:    "default",
		Email:    "cache@example.com",
		Quota:    quota,
		Status:   status,
		Setting:  `{"language":"zh"}`,
	}
}

func requireCompleteUserCache(t *testing.T, user User) map[string]string {
	t.Helper()
	values, err := common.RDB.HGetAll(context.Background(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(user.Id), values["Id"])
	require.Equal(t, strconv.Itoa(user.Status), values["Status"])
	require.Equal(t, user.Group, values["Group"])
	require.Equal(t, strconv.Itoa(user.Quota), values["Quota"])
	require.Equal(t, strconv.Itoa(userCacheSchemaVersion), values["CacheSchema"])
	require.NotEmpty(t, values["CacheEpoch"])
	currentEpoch, err := common.RDB.Get(context.Background(), getUserCacheEpochKey()).Result()
	require.NoError(t, err)
	require.Equal(t, currentEpoch, values["CacheEpoch"])
	return values
}

func TestGetUserCacheRepairsPartialAndOldSchemaHashesSynchronously(t *testing.T) {
	testCases := []struct {
		name string
		seed func(user User) map[string]interface{}
	}{
		{
			name: "quota only",
			seed: func(user User) map[string]interface{} {
				return map[string]interface{}{"Quota": user.Quota + 999}
			},
		},
		{
			name: "status only",
			seed: func(user User) map[string]interface{} {
				return map[string]interface{}{"Status": common.UserStatusDisabled}
			},
		},
		{
			name: "old schema",
			seed: func(user User) map[string]interface{} {
				return map[string]interface{}{
					"Id":          user.Id,
					"Status":      common.UserStatusEnabled,
					"Group":       "stale-group",
					"Quota":       user.Quota + 999,
					"CacheSchema": userCacheSchemaVersion - 1,
				}
			},
		},
		{
			name: "invalid status",
			seed: func(user User) map[string]interface{} {
				return map[string]interface{}{
					"Id":          user.Id,
					"Status":      0,
					"Group":       user.Group,
					"Quota":       user.Quota,
					"CacheSchema": userCacheSchemaVersion,
				}
			},
		},
	}

	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			user := newUserCacheConsistencyUser(28100+index, common.UserStatusEnabled, 700+index)
			setupUserCacheConsistencyTest(t, user)
			key := getUserCacheKey(user.Id)
			require.NoError(t, common.RDB.HSet(context.Background(), key, testCase.seed(user)).Err())

			cached, err := GetUserCache(user.Id)
			require.NoError(t, err)
			require.Equal(t, user.Id, cached.Id)
			require.Equal(t, common.UserStatusEnabled, cached.Status)
			require.Equal(t, user.Quota, cached.Quota)
			requireCompleteUserCache(t, user)
		})
	}
}

func TestGetUserCacheRejectsInvalidDatabaseStatus(t *testing.T) {
	user := newUserCacheConsistencyUser(28110, common.UserStatusEnabled, 800)
	setupUserCacheConsistencyTest(t, user)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).UpdateColumn("status", 0).Error)

	cached, err := GetUserCache(user.Id)
	require.Error(t, err)
	assert.Nil(t, cached)
	assert.Contains(t, err.Error(), "invalid user status")
	exists, redisErr := common.RDB.Exists(context.Background(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, redisErr)
	assert.Zero(t, exists, "an invalid database status must not be published as an auth snapshot")
}

func TestGetUserCacheRechecksCachedDisabledStatusAgainstDatabase(t *testing.T) {
	t.Run("stale disabled cache heals to enabled", func(t *testing.T) {
		user := newUserCacheConsistencyUser(28120, common.UserStatusEnabled, 900)
		setupUserCacheConsistencyTest(t, user)
		require.NoError(t, populateUserCache(user))
		require.NoError(t, common.RDB.HSet(context.Background(), getUserCacheKey(user.Id), "Status", common.UserStatusDisabled).Err())

		cached, err := GetUserCache(user.Id)
		require.NoError(t, err)
		assert.Equal(t, common.UserStatusEnabled, cached.Status)
		requireCompleteUserCache(t, user)
		stats := common.GetUserCacheStats()
		assert.EqualValues(t, 1, stats.AuthDBFallbackTotal.DisabledRecheck)
	})

	t.Run("database disabled remains disabled", func(t *testing.T) {
		user := newUserCacheConsistencyUser(28121, common.UserStatusDisabled, 900)
		setupUserCacheConsistencyTest(t, user)
		require.NoError(t, populateUserCache(user))

		cached, err := GetUserCache(user.Id)
		require.NoError(t, err)
		assert.Equal(t, common.UserStatusDisabled, cached.Status)
		requireCompleteUserCache(t, user)
	})
}

func TestUserCacheGenerationRejectsDelayedEnabledSnapshotAfterDisable(t *testing.T) {
	user := newUserCacheConsistencyUser(28122, common.UserStatusEnabled, 901)
	setupUserCacheConsistencyTest(t, user)

	staleSnapshot := user
	oldGeneration, err := getUserCacheGeneration(user.Id)
	require.NoError(t, err)

	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).
		UpdateColumn("status", common.UserStatusDisabled).Error)
	require.NoError(t, invalidateUserCache(user.Id))

	applied, err := populateUserCacheAtGeneration(staleSnapshot, oldGeneration)
	require.NoError(t, err)
	assert.False(t, applied, "a pre-disable snapshot must not cross the generation fence")

	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, common.UserStatusDisabled, cached.Status)

	values, err := common.RDB.HGetAll(context.Background(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(common.UserStatusDisabled), values["Status"])
}

func TestUserCacheGenerationFenceRejectsOneThousandDelayedWrites(t *testing.T) {
	user := newUserCacheConsistencyUser(28124, common.UserStatusEnabled, 903)
	setupUserCacheConsistencyTest(t, user)

	for range 1_000 {
		oldGeneration, err := getUserCacheGeneration(user.Id)
		require.NoError(t, err)
		require.NoError(t, invalidateUserCache(user.Id))

		applied, err := populateUserCacheAtGeneration(user, oldGeneration)
		require.NoError(t, err)
		assert.False(t, applied, "a delayed snapshot crossed the generation fence")

		currentGeneration, err := getUserCacheGeneration(user.Id)
		require.NoError(t, err)
		applied, err = populateUserCacheAtGeneration(user, currentGeneration)
		require.NoError(t, err)
		require.True(t, applied)
	}
}

func TestAuthoritativeUserUpdateFencesCachedEnabledStatus(t *testing.T) {
	user := newUserCacheConsistencyUser(28123, common.UserStatusEnabled, 902)
	setupUserCacheConsistencyTest(t, user)
	require.NoError(t, populateUserCache(user))

	user.Status = common.UserStatusDisabled
	require.NoError(t, user.Update(false))

	exists, err := common.RDB.Exists(context.Background(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "an authoritative auth mutation must delete the old snapshot")

	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, common.UserStatusDisabled, cached.Status)
}

func TestUserCacheMutationNeverRecreatesMissingOrPartialHash(t *testing.T) {
	user := newUserCacheConsistencyUser(28130, common.UserStatusEnabled, 1000)
	setupUserCacheConsistencyTest(t, user)
	key := getUserCacheKey(user.Id)

	require.NoError(t, populateUserCache(user))
	require.NoError(t, common.RDB.Del(context.Background(), key).Err())
	require.NoError(t, cacheIncrUserQuota(user.Id, 25))
	exists, err := common.RDB.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "quota delta must not recreate a deleted hash")

	require.NoError(t, common.RDB.HSet(context.Background(), key, "Status", common.UserStatusEnabled).Err())
	require.NoError(t, updateUserEmailCache(user.Id, "new@example.com"))
	partial, err := common.RDB.HGetAll(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"Status": strconv.Itoa(common.UserStatusEnabled)}, partial)
}

func TestUserCacheMutationSkipsNonIntegerQuotaWithoutChangingHash(t *testing.T) {
	user := newUserCacheConsistencyUser(28131, common.UserStatusEnabled, 1001)
	setupUserCacheConsistencyTest(t, user)
	key := getUserCacheKey(user.Id)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, common.RDB.HSet(context.Background(), key, "Quota", "1.5").Err())

	require.NoError(t, cacheIncrUserQuota(user.Id, 25))

	values, err := common.RDB.HGetAll(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Equal(t, "1.5", values["Quota"])
	assert.Equal(t, strconv.Itoa(common.UserStatusEnabled), values["Status"])
	stats := common.GetUserCacheStats()
	assert.EqualValues(t, 1, stats.CacheMutationTotal.Skipped)
}

func TestCompleteUserCacheMutationAppliesDeltaWithoutRefreshingTTL(t *testing.T) {
	user := newUserCacheConsistencyUser(28140, common.UserStatusEnabled, 1100)
	server := setupUserCacheConsistencyTest(t, user)
	key := getUserCacheKey(user.Id)
	require.NoError(t, populateUserCache(user))
	server.SetTTL(key, 90*time.Second)
	originalTTL := server.TTL(key)

	require.NoError(t, cacheDecrUserQuota(user.Id, 125))
	require.NoError(t, updateUserEmailCache(user.Id, "new@example.com"))

	values, err := common.RDB.HGetAll(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Equal(t, "975", values["Quota"])
	assert.Equal(t, "new@example.com", values["Email"])
	assert.Equal(t, originalTTL, server.TTL(key), "safe mutations must not refresh cache TTL")
	stats := common.GetUserCacheStats()
	assert.EqualValues(t, 2, stats.CacheMutationTotal.Applied)
}

func TestDirtyUserCacheBypassesStaleEnabledHashAfterFenceFailure(t *testing.T) {
	user := newUserCacheConsistencyUser(28141, common.UserStatusEnabled, 1101)
	setupUserCacheConsistencyTest(t, user)
	key := getUserCacheKey(user.Id)
	require.NoError(t, populateUserCache(user))

	redisClient := common.RDB
	common.RDB = nil
	user.Status = common.UserStatusDisabled
	require.Error(t, user.Update(false), "the authoritative DB write succeeds but the Redis fence must report failure")
	common.RDB = redisClient

	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	require.Equal(t, common.UserStatusDisabled, cached.Status,
		"a locally dirty user must use the authoritative database instead of the stale enabled hash")

	staleStatus, err := common.RDB.HGet(context.Background(), key, "Status").Int()
	require.NoError(t, err)
	assert.Equal(t, common.UserStatusEnabled, staleStatus,
		"the test must retain the stale enabled hash to prove it was bypassed")

	// Simulate a process restart: process-local dirty state disappears, while the
	// external Redis hash survives. Initializing the new process epoch must make
	// the old enabled snapshot stale before it can be trusted.
	dirtyUserAuthCaches.Delete(user.Id)
	resetUserCacheEpochInitializationForTest()
	cached, err = GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, common.UserStatusDisabled, cached.Status)
	assert.False(t, isUserCacheDirty(user.Id))
	requireCompleteUserCache(t, user)
}

func TestGroupInvalidationRejectsDelayedOldGroupSnapshot(t *testing.T) {
	user := newUserCacheConsistencyUser(28142, common.UserStatusEnabled, 1102)
	user.Group = "premium"
	setupUserCacheConsistencyTest(t, user)
	require.NoError(t, populateUserCache(user))

	staleSnapshot := user
	oldGeneration, err := getUserCacheGeneration(user.Id)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).UpdateColumn("group", "default").Error)
	require.NoError(t, UpdateUserGroupCache(user.Id, "default"))

	applied, err := populateUserCacheAtGeneration(staleSnapshot, oldGeneration)
	require.NoError(t, err)
	assert.False(t, applied, "a pre-downgrade group snapshot must not cross the generation fence")

	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "default", cached.Group)
}

func TestReserveUserQuotaDoesNotDeleteOrMutateSharedUserCache(t *testing.T) {
	user := newUserCacheConsistencyUser(28150, common.UserStatusEnabled, 1200)
	server := setupUserCacheConsistencyTest(t, user)
	key := getUserCacheKey(user.Id)
	require.NoError(t, populateUserCache(user))
	server.SetTTL(key, 2*time.Minute)
	originalTTL := server.TTL(key)

	require.NoError(t, ReserveUserQuota(user.Id, 450))

	var databaseQuota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&databaseQuota).Error)
	assert.Equal(t, 750, databaseQuota)
	requireCompleteUserCache(t, user)
	assert.Equal(t, originalTTL, server.TTL(key))
}

func TestGetUserCacheFallsBackOnWrongTypeAndRedisFailure(t *testing.T) {
	t.Run("wrong type falls back to database without destructive repair", func(t *testing.T) {
		user := newUserCacheConsistencyUser(28160, common.UserStatusEnabled, 1300)
		setupUserCacheConsistencyTest(t, user)
		key := getUserCacheKey(user.Id)
		require.NoError(t, common.RDB.Set(context.Background(), key, "not-a-hash", time.Minute).Err())

		cached, err := GetUserCache(user.Id)
		require.NoError(t, err)
		assert.Equal(t, user.Quota, cached.Quota)
		value, redisErr := common.RDB.Get(context.Background(), key).Result()
		require.NoError(t, redisErr)
		assert.Equal(t, "not-a-hash", value)
	})

	t.Run("redis outage does not reject a valid database user", func(t *testing.T) {
		user := newUserCacheConsistencyUser(28161, common.UserStatusEnabled, 1301)
		server := setupUserCacheConsistencyTest(t, user)
		server.Close()

		cached, err := GetUserCache(user.Id)
		require.NoError(t, err)
		require.NotNil(t, cached)
		assert.Equal(t, user.Id, cached.Id)
		assert.Equal(t, common.UserStatusEnabled, cached.Status)
	})

	t.Run("nil redis client does not reject a valid database user", func(t *testing.T) {
		user := newUserCacheConsistencyUser(28162, common.UserStatusEnabled, 1302)
		setupUserCacheConsistencyTest(t, user)
		common.RDB = nil

		cached, err := GetUserCache(user.Id)
		require.NoError(t, err)
		require.NotNil(t, cached)
		assert.Equal(t, user.Id, cached.Id)
		assert.Equal(t, common.UserStatusEnabled, cached.Status)
	})
}
