package model

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSaaSCreditOperationTest(t *testing.T) (*User, *Token) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "credits.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &SaaSCreditOperation{}, &Log{}))
	oldFrequency := common.SyncFrequency
	common.SyncFrequency = 60
	oldDB, oldLogDB, oldRedis, oldBatch, oldExcluded := DB, LOG_DB, common.RedisEnabled, common.BatchUpdateEnabled, constant.SaaSTopupExcludedUserIDs
	DB, LOG_DB, common.RedisEnabled, common.BatchUpdateEnabled = db, db, false, true
	constant.SaaSTopupExcludedUserIDs = map[int]struct{}{}
	resetBatchUpdateStores()
	t.Cleanup(func() {
		common.SyncFrequency = oldFrequency
		DB, LOG_DB, common.RedisEnabled, common.BatchUpdateEnabled, constant.SaaSTopupExcludedUserIDs = oldDB, oldLogDB, oldRedis, oldBatch, oldExcluded
		resetBatchUpdateStores()
		dirtyUserAuthCaches.Delete(1)
		dirtySaaSTokenCaches.Delete(tokenCacheKey("saas-credit-test-key"))
		require.NoError(t, sqlDB.Close())
	})
	user := &User{Id: 1, Username: "saas-credit-user", Status: common.UserStatusEnabled, Quota: 100}
	token := &Token{Id: 1, UserId: 1, Key: "saas-credit-test-key", Status: common.TokenStatusEnabled, RemainQuota: 100, UsedQuota: 40}
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(token).Error)
	return user, token
}

func creditRequest(id string, token *Token, amount int) SaaSCreditOperationRequest {
	return SaaSCreditOperationRequest{OperationID: id, UserID: token.UserId, TokenID: &token.Id, Amount: amount, CreditUserQuota: true}
}

func assertSaaSCreditBalances(t *testing.T, expectedUser, expectedToken int, receipts int64) {
	t.Helper()
	var user User
	var token Token
	require.NoError(t, DB.First(&user, 1).Error)
	require.NoError(t, DB.First(&token, 1).Error)
	assert.Equal(t, expectedUser, user.Quota)
	assert.Equal(t, expectedToken, token.RemainQuota)
	assert.Equal(t, 40, token.UsedQuota, "grants must not alter usage history")
	var count int64
	require.NoError(t, DB.Model(&SaaSCreditOperation{}).Count(&count).Error)
	assert.Equal(t, receipts, count)
}

func TestSaaSCreditOperationConcurrentDifferentIDs(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	start := make(chan struct{})
	results := make(chan *SaaSCreditOperation, 2)
	errs := make(chan error, 2)
	for i, amount := range []int{50, 70} {
		go func(i, amount int) {
			<-start
			op, err := ApplySaaSCreditOperation(context.Background(), creditRequest(fmt.Sprintf("order:%d", i), token, amount))
			results <- op
			errs <- err
		}(i, amount)
	}
	close(start)
	firstErr, secondErr := <-errs, <-errs
	require.NoError(t, firstErr)
	require.NoError(t, secondErr)
	first, second := <-results, <-results
	assert.Equal(t, first.Amount, first.AfterUserQuota-first.BeforeUserQuota)
	assert.Equal(t, second.Amount, *second.AfterRemainQuota-*second.BeforeRemainQuota)
	assertSaaSCreditBalances(t, 220, 220, 2)
	batchUpdate()
	assertSaaSCreditBalances(t, 220, 220, 2)
}

func TestSaaSCreditOperationConcurrentReplayAndImmutableReceipt(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	req := creditRequest("order:replay", token, 50)
	var wg sync.WaitGroup
	start, results, errs := make(chan struct{}), make(chan *SaaSCreditOperation, 8), make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			op, err := ApplySaaSCreditOperation(context.Background(), req)
			results <- op
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	originals := 0
	for op := range results {
		if !op.Duplicated {
			originals++
		}
		assert.Equal(t, 150, op.AfterUserQuota)
	}
	assert.Equal(t, 1, originals)
	assertSaaSCreditBalances(t, 150, 150, 1)
	// A changing balance or a subsequently disabled target cannot change proof
	// of an already committed operation, and the note is not an idempotency key.
	require.NoError(t, DB.Model(&User{}).Where("id = 1").Updates(map[string]any{"quota": 9, "status": common.UserStatusDisabled}).Error)
	req.Note = "updated note"
	op, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, op.Duplicated)
	assert.Equal(t, 150, op.AfterUserQuota)
	read, err := GetSaaSCreditOperation(context.Background(), req.OperationID)
	require.NoError(t, err)
	assert.Equal(t, op.AppliedAt, read.AppliedAt)
	assert.False(t, read.Duplicated)
}

func TestSaaSCreditOperationConflict(t *testing.T) {
	for _, field := range []string{"user", "token", "amount", "policy"} {
		t.Run(field, func(t *testing.T) {
			_, token := setupSaaSCreditOperationTest(t)
			req := creditRequest("same-id", token, 50)
			_, err := ApplySaaSCreditOperation(context.Background(), req)
			require.NoError(t, err)
			switch field {
			case "user":
				req.UserID++
			case "token":
				req.TokenID = nil
			case "amount":
				req.Amount++
			case "policy":
				req.CreditUserQuota = false
			}
			_, err = ApplySaaSCreditOperation(context.Background(), req)
			require.ErrorIs(t, err, ErrSaaSCreditConflict)
			assertSaaSCreditBalances(t, 150, 150, 1)
		})
	}
}

func TestSaaSCreditOperationRollsBackPartialWrites(t *testing.T) {
	for _, failureTable := range []string{"tokens", "saas_credit_operations"} {
		t.Run(failureTable, func(t *testing.T) {
			_, token := setupSaaSCreditOperationTest(t)
			callback := "test:fail_credit_update"
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == failureTable {
					tx.AddError(errors.New("injected update failure"))
				}
			}))
			_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("rollback", token, 50))
			require.Error(t, err)
			assertSaaSCreditBalances(t, 100, 100, 0)
			require.NoError(t, DB.Callback().Update().Remove(callback))
			_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("rollback", token, 50))
			require.NoError(t, err)
			assertSaaSCreditBalances(t, 150, 150, 1)
		})
	}
}

func TestSaaSCreditOperationEligibility(t *testing.T) {
	for _, condition := range []string{"disabled-user", "disabled-token", "excluded", "owner-mismatch", "missing-user", "deleted-token"} {
		t.Run(condition, func(t *testing.T) {
			_, token := setupSaaSCreditOperationTest(t)
			req := creditRequest("eligibility", token, 50)
			switch condition {
			case "disabled-user":
				require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("status", common.UserStatusDisabled).Error)
			case "disabled-token":
				require.NoError(t, DB.Model(&Token{}).Where("id = 1").Update("status", common.TokenStatusDisabled).Error)
			case "excluded":
				constant.SaaSTopupExcludedUserIDs[1] = struct{}{}
			case "owner-mismatch":
				require.NoError(t, DB.Model(&Token{}).Where("id = 1").Update("user_id", 2).Error)
			case "missing-user":
				req.UserID = 999
			case "deleted-token":
				require.NoError(t, DB.Delete(token).Error)
			}
			_, err := ApplySaaSCreditOperation(context.Background(), req)
			require.ErrorIs(t, err, ErrSaaSCreditTargetNotFound)
			var count int64
			require.NoError(t, DB.Model(&SaaSCreditOperation{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestSaaSCreditOperationAccountOnlyAndTokenOnly(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	req := creditRequest("account-only", token, 50)
	req.TokenID = nil
	op, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.Nil(t, op.TokenID)
	assert.Nil(t, op.BeforeRemainQuota)
	assert.Nil(t, op.AfterRemainQuota)
	req = creditRequest("token-only", token, 70)
	req.CreditUserQuota = false
	op, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 150, op.BeforeUserQuota)
	assert.Equal(t, 150, op.AfterUserQuota)
	assertSaaSCreditBalances(t, 150, 170, 2)
}

func TestSaaSCreditOperationValidationAndOverflow(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	for _, id := range []string{"", "contains/slash", "x?y", "x y"} {
		_, err := ApplySaaSCreditOperation(context.Background(), creditRequest(id, token, 1))
		require.ErrorIs(t, err, ErrSaaSCreditInvalid)
	}
	_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("overflow", token, int(MaxSaaSCreditQuota)))
	require.ErrorIs(t, err, ErrSaaSCreditOverflow)
	assertSaaSCreditBalances(t, 100, 100, 0)
	_, err = GetSaaSCreditOperation(context.Background(), "missing")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("Case", token, 1))
	require.NoError(t, err)
	_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("case", token, 1))
	require.NoError(t, err)
	assertSaaSCreditBalances(t, 102, 102, 2)
}

func TestSaaSCreditOperationCachesPreserveBatchUsageAndReplay(t *testing.T) {
	user, token := setupSaaSCreditOperationTest(t)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	common.RDB, common.RedisEnabled = client, true
	resetUserCacheEpochInitializationForTest()
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		common.RDB = oldClient
		resetUserCacheEpochInitializationForTest()
	})
	require.NoError(t, populateUserCache(*user))
	require.NoError(t, cacheSetToken(*token))
	oldUserGeneration, err := getUserCacheGeneration(user.Id)
	require.NoError(t, err)
	// A pending usage debit is already in Redis while database still has 100.
	require.NoError(t, DecreaseTokenQuota(token.Id, token.Key, 20))
	drainCreditCacheWorkers(t)
	req := creditRequest("cache:credit", token, 50)
	op, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 150, *op.AfterRemainQuota, "receipt records the durable transaction, not unflushed usage")
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	require.NoError(t, cacheSetToken(*token)) // pre-credit asynchronous snapshot is rejected
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	cachedUser, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 150, cachedUser.Quota)
	applied, err := populateUserCacheAtGeneration(*user, oldUserGeneration)
	require.NoError(t, err)
	assert.False(t, applied)
	_, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	batchUpdate()
	var persisted Token
	require.NoError(t, DB.First(&persisted, token.Id).Error)
	assert.Equal(t, 130, persisted.RemainQuota)
	assert.Equal(t, 60, persisted.UsedQuota)
	require.NoError(t, cacheSetToken(persisted))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 130, cached.RemainQuota)
	assert.Equal(t, 60, cached.UsedQuota, "actual batch usage must become visible after DB flush")
	// An older cache delivery after a newer one must neither double-credit nor
	// lose a debit which was already reserved in Redis.
	require.NoError(t, cacheApplySaaSTokenCredit(token.Key, 120))
	require.NoError(t, cacheApplySaaSTokenCredit(token.Key, 50))
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 200, cached.RemainQuota)
}

func TestSaaSCreditOperationRedisFailureDoesNotChangeReceipt(t *testing.T) {
	user, token := setupSaaSCreditOperationTest(t)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	common.RDB, common.RedisEnabled = client, true
	resetUserCacheEpochInitializationForTest()
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		common.RDB = oldClient
		resetUserCacheEpochInitializationForTest()
	})
	require.NoError(t, populateUserCache(*user))
	require.NoError(t, cacheSetToken(*token))
	common.RDB = nil
	req := creditRequest("redis-failure", token, 50)
	op, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, op.Duplicated)
	assertSaaSCreditBalances(t, 150, 150, 1)
	assert.True(t, isUserCacheDirty(user.Id))
	_, dirty := dirtySaaSTokenCaches.Load(tokenCacheKey(token.Key))
	assert.True(t, dirty)
	common.RDB = client
	op, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, op.Duplicated)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 150, cached.RemainQuota)
	assert.False(t, isUserCacheDirty(user.Id))
	assertSaaSCreditBalances(t, 150, 150, 1)
}

func TestSaaSCreditOperationExhaustionRecoveryIsNotReplayed(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	token.RemainQuota, token.Status = 0, common.TokenStatusExhausted
	require.NoError(t, DB.Model(token).Updates(map[string]any{"remain_quota": 0, "status": token.Status}).Error)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldClient := common.RDB
	common.RDB, common.RedisEnabled = client, true
	resetUserCacheEpochInitializationForTest()
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		common.RDB = oldClient
		resetUserCacheEpochInitializationForTest()
	})
	require.NoError(t, cacheSetToken(*token))
	req := creditRequest("restore-exhausted", token, 50)
	_, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusEnabled, cached.Status)
	var persisted Token
	require.NoError(t, DB.First(&persisted, token.Id).Error)
	assert.Equal(t, common.TokenStatusEnabled, persisted.Status)
	// Later exhaustion is a different business event. Replaying the credit's
	// delivery must not re-enable the token or add that credit for a second time.
	require.NoError(t, cacheDecrTokenQuota(token.Key, 50))
	require.NoError(t, common.RDB.HSet(context.Background(), tokenCacheKey(token.Key), "Status", common.TokenStatusExhausted).Err())
	_, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 0, cached.RemainQuota)
	assert.Equal(t, common.TokenStatusExhausted, cached.Status)
}

func TestSaaSCreditOperationLargeBalancesAndTokenOnlyPolicy(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", int64(6_000_000_000)).Error)
	require.NoError(t, DB.Model(token).Update("remain_quota", int64(7_000_000_000)).Error)
	req := creditRequest("large:both", token, 3_000_000_000)
	op, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 6_000_000_000, op.BeforeUserQuota)
	assert.Equal(t, 9_000_000_000, op.AfterUserQuota)
	assert.Equal(t, 10_000_000_000, *op.AfterRemainQuota)
	assertSaaSCreditBalances(t, 9_000_000_000, 10_000_000_000, 1)
	_, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assertSaaSCreditBalances(t, 9_000_000_000, 10_000_000_000, 1)
	req.OperationID, req.CreditUserQuota = "large:token-only", false
	op, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 9_000_000_000, op.AfterUserQuota)
	assertSaaSCreditBalances(t, 9_000_000_000, 13_000_000_000, 2)
}

func TestSaaSCreditOperationRestoresLargeNegativeBalances(t *testing.T) {
	_, token := setupSaaSCreditOperationTest(t)
	require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", int64(-4_000_000_000)).Error)
	require.NoError(t, DB.Model(token).Updates(map[string]any{"remain_quota": int64(-4_000_000_000), "status": common.TokenStatusExhausted}).Error)
	req := creditRequest("negative:first", token, 3_000_000_000)
	_, err := ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assertSaaSCreditBalances(t, -1_000_000_000, -1_000_000_000, 1)
	var persisted Token
	require.NoError(t, DB.First(&persisted, token.Id).Error)
	assert.Equal(t, common.TokenStatusExhausted, persisted.Status)
	req.OperationID = "negative:second"
	_, err = ApplySaaSCreditOperation(context.Background(), req)
	require.NoError(t, err)
	assertSaaSCreditBalances(t, 2_000_000_000, 2_000_000_000, 2)
	require.NoError(t, DB.First(&persisted, token.Id).Error)
	assert.Equal(t, common.TokenStatusEnabled, persisted.Status)
}

func TestSaaSCreditOperationSafeIntegerAndWatermarkBoundaries(t *testing.T) {
	for _, boundary := range []string{"user-max", "token-max", "user-below-min", "token-below-min", "watermark-max", "watermark-negative"} {
		t.Run(boundary, func(t *testing.T) {
			_, token := setupSaaSCreditOperationTest(t)
			switch boundary {
			case "user-max":
				require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", MaxSaaSCreditQuota).Error)
			case "token-max":
				require.NoError(t, DB.Model(token).Update("remain_quota", MaxSaaSCreditQuota).Error)
			case "user-below-min":
				require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", -MaxSaaSCreditQuota-1).Error)
			case "token-below-min":
				require.NoError(t, DB.Model(token).Update("remain_quota", -MaxSaaSCreditQuota-1).Error)
			case "watermark-max":
				require.NoError(t, DB.Model(token).Update("saas_credited_quota", MaxSaaSCreditQuota).Error)
			case "watermark-negative":
				require.NoError(t, DB.Model(token).Update("saas_credited_quota", -1).Error)
			}
			_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("unsafe", token, 1))
			require.ErrorIs(t, err, ErrSaaSCreditOverflow)
			var count int64
			require.NoError(t, DB.Model(&SaaSCreditOperation{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
	t.Run("exact-maximum-amount-from-negative", func(t *testing.T) {
		_, token := setupSaaSCreditOperationTest(t)
		require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", -MaxSaaSCreditQuota).Error)
		require.NoError(t, DB.Model(token).Update("remain_quota", -MaxSaaSCreditQuota).Error)
		op, err := ApplySaaSCreditOperation(context.Background(), creditRequest("safe-max", token, int(MaxSaaSCreditQuota)))
		require.NoError(t, err)
		assert.Zero(t, op.AfterUserQuota)
		assert.Zero(t, *op.AfterRemainQuota)
		assert.Equal(t, MaxSaaSCreditQuota, op.AfterTokenCreditTotal)
		_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("unsafe-amount", token, int(MaxSaaSCreditQuota+1)))
		require.ErrorIs(t, err, ErrSaaSCreditInvalid)
	})
	t.Run("token-only-leaves-maximum-wallet", func(t *testing.T) {
		_, token := setupSaaSCreditOperationTest(t)
		require.NoError(t, DB.Model(&User{}).Where("id = 1").Update("quota", MaxSaaSCreditQuota).Error)
		req := creditRequest("safe-admin", token, 3_000_000_000)
		req.CreditUserQuota = false
		op, err := ApplySaaSCreditOperation(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, int(MaxSaaSCreditQuota), op.AfterUserQuota)
		assert.Equal(t, 3_000_000_100, *op.AfterRemainQuota)
	})
}
