package model

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCreditCacheTest(t *testing.T) (*User, *Token, *redis.Client) {
	t.Helper()
	user, token := setupSaaSCreditOperationTest(t)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	common.RDB, common.RedisEnabled = client, true
	resetUserCacheEpochInitializationForTest()
	t.Cleanup(func() {
		drainCreditCacheWorkers(t)
		require.NoError(t, client.Close())
		common.RDB = oldClient
		resetUserCacheEpochInitializationForTest()
	})
	require.NoError(t, cacheSetToken(*token))
	return user, token, client
}

func drainCreditCacheWorkers(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 5*time.Second, time.Millisecond)
}

func TestSaaSCreditCacheReadbackAndExplicitEdits(t *testing.T) {
	_, token, _ := setupCreditCacheTest(t)
	// Uncredited tokens retain their existing read-through behavior and custom
	// metadata, rather than silently adopting the rejected global cold-only fill.
	copy := *token
	rpm, ips := 7, "127.0.0.1"
	copy.RemainQuota, copy.Name, copy.RPMRateLimit, copy.AllowIps = 90, "ordinary", &rpm, &ips
	require.NoError(t, cacheSetToken(copy))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 90, cached.RemainQuota)
	assert.Equal(t, rpm, *cached.RPMRateLimit)
	require.NoError(t, cacheSetToken(*token))
	require.NoError(t, cacheDecrTokenQuota(token.Key, 20))

	_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("metadata-credit", token, 50))
	require.NoError(t, err)
	var fresh Token
	require.NoError(t, DB.First(&fresh, token.Id).Error)
	fresh.Name, fresh.RPMRateLimit, fresh.AllowIps = "new metadata", &rpm, &ips
	require.NoError(t, cacheSetToken(fresh))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	assert.Equal(t, 40, cached.UsedQuota)
	assert.Equal(t, fresh.Name, cached.Name)
	assert.Equal(t, rpm, *cached.RPMRateLimit)
	assert.Equal(t, ips, *cached.AllowIps)
	// Flush the pending debit, then explicitly replace the balance and metadata.
	// This edit must not be swallowed by the read-through quota protection.
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"remain_quota": 130, "used_quota": 60}).Error)
	fresh.RemainQuota, fresh.UsedQuota, fresh.Name, fresh.Status = 300, 60, "explicit edit", common.TokenStatusDisabled
	require.NoError(t, fresh.Update())
	drainCreditCacheWorkers(t)
	cached, err = GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 300, cached.RemainQuota)
	assert.Equal(t, common.TokenStatusDisabled, cached.Status)
	assert.Equal(t, "explicit edit", cached.Name)
	drainCreditCacheWorkers(t)
	_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("metadata-credit", token, 50))
	require.NoError(t, err)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 300, cached.RemainQuota, "replay cannot undo a later explicit edit")
	assert.Equal(t, common.TokenStatusDisabled, cached.Status)
}

func TestSaaSCreditCacheFailedDeliveryHealsFromCurrentRead(t *testing.T) {
	_, token, client := setupCreditCacheTest(t)
	require.NoError(t, cacheDecrTokenQuota(token.Key, 20))
	common.RDB = nil
	_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("heal-read", token, 50))
	require.NoError(t, err)
	common.RDB = client
	require.NoError(t, cacheSetToken(*token)) // stale read must not clear the marker
	_, err = cacheGetTokenByKey(token.Key)
	require.Error(t, err)
	var fresh Token
	require.NoError(t, DB.First(&fresh, token.Id).Error)
	require.NoError(t, cacheSetToken(fresh))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	_, pending := dirtySaaSTokenCaches.Load(tokenCacheKey(token.Key))
	assert.False(t, pending)
	require.NoError(t, cacheApplySaaSTokenCredit(token.Key, 50))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
}

func TestSaaSCreditCacheConcurrentLegacyAndNew(t *testing.T) {
	_, token, _ := setupCreditCacheTest(t)
	require.NoError(t, cacheDecrTokenQuota(token.Key, 20))
	start, results := make(chan struct{}), make(chan error, 2)
	go func() { <-start; results <- GrantTokenRemainQuota(token.Id, token.Key, 50) }()
	go func() {
		<-start
		_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("mixed-concurrent", token, 70))
		results <- err
	}()
	close(start)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	assertSaaSCreditBalances(t, 170, 220, 1)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 200, cached.RemainQuota)
	assert.Equal(t, int64(120), cached.SaaSCreditedQuota)
	require.NoError(t, cacheApplySaaSTokenCredit(token.Key, 50))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 200, cached.RemainQuota)
}

func TestSaaSCreditCacheOldWalletReadCannotOverwriteCredit(t *testing.T) {
	user, token, _ := setupCreditCacheTest(t)
	require.NoError(t, populateUserCache(*user))
	read, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	callback := "test:credit-wallet-old-read"
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "quota" && armed.CompareAndSwap(true, false) {
			close(read)
			<-release
		}
	}))
	t.Cleanup(func() { unblock(); <-done; require.NoError(t, DB.Callback().Query().Remove(callback)) })
	go func() { defer close(done); _, _ = GetUserQuota(user.Id, true) }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("wallet read did not reach barrier")
	}
	_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("wallet-old-read", token, 50))
	require.NoError(t, err)
	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 150, cached.Quota)
	unblock()
	<-done
	drainCreditCacheWorkers(t)
	quota, err := GetUserQuota(user.Id, false)
	require.NoError(t, err)
	assert.Equal(t, 150, quota)
}

func TestSaaSCreditCacheExplicitEditRejectsLateSnapshots(t *testing.T) {
	_, token, _ := setupCreditCacheTest(t)
	_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("edit-base", token, 50))
	require.NoError(t, err)
	var before Token
	require.NoError(t, DB.First(&before, token.Id).Error)
	first := before
	first.RemainQuota = 300
	require.NoError(t, first.Update())
	drainCreditCacheWorkers(t)
	var middle Token
	require.NoError(t, DB.First(&middle, token.Id).Error)
	require.Equal(t, int64(1), middle.SaaSQuotaRevision)
	second := middle
	second.RemainQuota = 400
	require.NoError(t, second.Update())
	drainCreditCacheWorkers(t)
	var latest Token
	require.NoError(t, DB.First(&latest, token.Id).Error)
	require.Equal(t, int64(2), latest.SaaSQuotaRevision)
	// Model both an old ordinary read and an older explicit edit delivered late.
	require.NoError(t, cacheSetToken(before))
	require.NoError(t, cacheSetToken(middle))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 400, cached.RemainQuota)
	// The revision floor survives eviction of the main hash.
	require.NoError(t, cacheDeleteToken(token.Key))
	require.NoError(t, cacheSetToken(middle))
	_, err = cacheGetTokenByKey(token.Key)
	require.Error(t, err)
	require.NoError(t, cacheSetToken(latest))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 400, cached.RemainQuota)
}

func TestSaaSCreditCacheNewEditSnapshotRacesLaterCredit(t *testing.T) {
	_, token, _ := setupCreditCacheTest(t)
	_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("edit-race-base", token, 50))
	require.NoError(t, err)
	var edited Token
	// Pause between the atomic DB edit and its cache delivery. The real Update
	// path uses exactly this same DB revision plus a fresh row snapshot.
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{
		"remain_quota": 300, "saas_quota_revision": gorm.Expr("saas_quota_revision + 1"),
	}).Error)
	require.NoError(t, DB.First(&edited, token.Id).Error)
	_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("edit-race-later", token, 70))
	require.NoError(t, err)
	require.NoError(t, cacheSetToken(edited))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 370, cached.RemainQuota, "new explicit assignment retains credits committed after its read")
	assert.Equal(t, int64(120), cached.SaaSCreditedQuota)
	var fresh Token
	require.NoError(t, DB.First(&fresh, token.Id).Error)
	require.NoError(t, cacheSetToken(fresh))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 370, cached.RemainQuota)
}
