package controller

import (
	"context"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The controllers and model functions below are real. Each case owns its
// SQLite database and local Redis server; no external service or credentials
// are read. These tests must not use t.Parallel because model.DB/RDB are global.
func setupSaaSCreditCacheIntegration(t *testing.T) (*gin.Engine, *model.Token, *redis.Client) {
	t.Helper()
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldClient, oldBatch := common.RedisEnabled, common.RDB, common.BatchUpdateEnabled
	oldSQLite, oldMySQL, oldPostgreSQL := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	router, token := setupSaaSCreditController(t)
	group := router.Group("/api/token/admin", middleware.AdminAuth(), middleware.DisableCache())
	group.POST("/grant-quota", AdminGrantTokenQuota)
	group.POST("/saas-topup/grant-quota", GrantSaaSTopupTokenQuota)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RedisEnabled, common.RDB, common.BatchUpdateEnabled = true, client, false
	// The process-wide user-cache initializer may already have run in another
	// case. Each new private Redis instance still needs a valid epoch of its own.
	require.NoError(t, client.Set(context.Background(), "user:auth-cache-epoch", "1", 0).Err())
	t.Cleanup(func() {
		waitSaaSCreditCacheWorkers(t)
		common.RedisEnabled, common.RDB, common.BatchUpdateEnabled = oldRedis, oldClient, oldBatch
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgreSQL
		model.DB, model.LOG_DB = oldDB, oldLogDB
		require.NoError(t, client.Close())
	})
	return router, token, client
}

// GetTokenById/GetTokenByKey can enqueue asynchronous cache population. Wait
// for the actual pool to drain rather than guessing its completion with sleep.
func waitSaaSCreditCacheWorkers(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for gopool.WorkerCount() != 0 {
		select {
		case <-deadline.C:
			t.Fatal("token cache workers did not drain")
		default:
			runtime.Gosched()
		}
	}
}

func saasIntegrationCacheKey(token *model.Token) string {
	return "token:" + common.GenerateHMAC(token.Key)
}

func warmSaaSIntegrationToken(t *testing.T, token *model.Token) {
	t.Helper()
	waitSaaSCreditCacheWorkers(t)
	require.NoError(t, common.RedisHSetObj(saasIntegrationCacheKey(token), token, time.Hour))
}

func assertSaaSIntegrationTokenCache(t *testing.T, token *model.Token, client *redis.Client, remain, used int, credited int64) {
	t.Helper()
	waitSaaSCreditCacheWorkers(t)
	cached, err := model.GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, remain, cached.RemainQuota, "relay lookup must return the live cached balance")
	assert.Equal(t, used, cached.UsedQuota, "credit and DB readback must preserve live usage")
	assert.Equal(t, credited, cached.SaaSCreditedQuota)
	waitSaaSCreditCacheWorkers(t)
	fields, err := client.HGetAll(context.Background(), saasIntegrationCacheKey(token)).Result()
	require.NoError(t, err)
	require.NotEmpty(t, fields, "a DB fallback must not hide a missing/broken Redis hash")
	assert.Equal(t, strconv.Itoa(token.Id), fields["Id"])
	assert.Equal(t, strconv.Itoa(remain), fields["RemainQuota"])
	assert.Equal(t, strconv.Itoa(used), fields["UsedQuota"])
	assert.Equal(t, strconv.FormatInt(credited, 10), fields["SaaSCreditedQuota"])
}

func assertSaaSIntegrationDurableQuota(t *testing.T, token *model.Token, remain, userQuota int, credited int64, operations int64) {
	t.Helper()
	var stored model.Token
	require.NoError(t, model.DB.First(&stored, token.Id).Error)
	assert.Equal(t, remain, stored.RemainQuota)
	assert.Equal(t, 40, stored.UsedQuota)
	assert.Equal(t, credited, stored.SaaSCreditedQuota)
	var user model.User
	require.NoError(t, model.DB.First(&user, token.UserId).Error)
	assert.Equal(t, userQuota, user.Quota)
	var count int64
	require.NoError(t, model.DB.Model(&model.SaaSCreditOperation{}).Count(&count).Error)
	assert.Equal(t, operations, count)
}

func legacySaaSIntegrationGrant(t *testing.T, router *gin.Engine, token *model.Token, path string, amount int) {
	t.Helper()
	rr := callSaaSCreditController(t, router, http.MethodPost, path, map[string]any{
		"tokenId": token.Id, "userId": token.UserId, "amount": amount,
	}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.True(t, decodeAPIResponse(t, rr).Success, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), token.Key)
	waitSaaSCreditCacheWorkers(t)
}

func newSaaSIntegrationGrant(t *testing.T, router *gin.Engine, token *model.Token, operationID string, amount int) model.SaaSCreditOperation {
	t.Helper()
	rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, map[string]any{
		"operationId": operationID, "userId": token.UserId, "tokenId": token.Id, "amount": amount, "creditUserQuota": true,
	}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	response := decodeAPIResponse(t, rr)
	require.True(t, response.Success, rr.Body.String())
	var receipt model.SaaSCreditOperation
	require.NoError(t, common.Unmarshal(response.Data, &receipt))
	assert.NotContains(t, rr.Body.String(), token.Key)
	waitSaaSCreditCacheWorkers(t)
	return receipt
}

func TestSaaSCreditCacheIntegrationLegacyGrantAccurate(t *testing.T) {
	for _, path := range []string{"/api/token/admin/grant-quota", "/api/token/admin/saas-topup/grant-quota"} {
		for _, warm := range []bool{false, true} {
			t.Run(path+"/warm="+strconv.FormatBool(warm), func(t *testing.T) {
				router, token, client := setupSaaSCreditCacheIntegration(t)
				if warm {
					warmSaaSIntegrationToken(t, token)
				}
				legacySaaSIntegrationGrant(t, router, token, path, 50)
				assertSaaSIntegrationTokenCache(t, token, client, 150, 40, 50)
				assertSaaSIntegrationDurableQuota(t, token, 150, 100, 50, 0)
			})
		}
	}
}

func TestSaaSCreditCacheIntegrationInterleavedLegacyNewAndReplay(t *testing.T) {
	router, token, client := setupSaaSCreditCacheIntegration(t)
	warmSaaSIntegrationToken(t, token)
	legacySaaSIntegrationGrant(t, router, token, "/api/token/admin/grant-quota", 50)
	first := newSaaSIntegrationGrant(t, router, token, "cache:interleaved", 70)
	require.False(t, first.Duplicated)
	require.Equal(t, 150, *first.BeforeRemainQuota)
	require.Equal(t, 220, *first.AfterRemainQuota)
	legacySaaSIntegrationGrant(t, router, token, "/api/token/admin/saas-topup/grant-quota", 30)
	assertSaaSIntegrationTokenCache(t, token, client, 250, 40, 150)
	replayed := newSaaSIntegrationGrant(t, router, token, "cache:interleaved", 70)
	require.True(t, replayed.Duplicated)
	assert.Equal(t, first.BeforeRemainQuota, replayed.BeforeRemainQuota)
	assert.Equal(t, first.AfterRemainQuota, replayed.AfterRemainQuota, "receipt remains the original transaction snapshot")
	rr := callSaaSCreditController(t, router, http.MethodGet, saasCreditPath+"/cache:interleaved", nil, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var queried model.SaaSCreditOperation
	require.NoError(t, common.Unmarshal(decodeAPIResponse(t, rr).Data, &queried))
	assert.Equal(t, first.AfterRemainQuota, queried.AfterRemainQuota)
	assertSaaSIntegrationTokenCache(t, token, client, 250, 40, 150)
	assertSaaSIntegrationDurableQuota(t, token, 250, 170, 150, 1)
	newSaaSIntegrationGrant(t, router, token, "cache:next", 10)
	assertSaaSIntegrationTokenCache(t, token, client, 260, 40, 160)
	assertSaaSIntegrationDurableQuota(t, token, 260, 180, 160, 2)
}

func TestSaaSCreditCacheIntegrationWarmUsageSurvivesCreditsAndReadback(t *testing.T) {
	router, token, client := setupSaaSCreditCacheIntegration(t)
	warmSaaSIntegrationToken(t, token)
	// Exercise the real relay debit: Redis deducts RemainQuota immediately,
	// while the pending batch has not yet changed database quota or UsedQuota.
	common.BatchUpdateEnabled = true
	require.NoError(t, model.DecreaseTokenQuota(token.Id, token.Key, 20))
	t.Cleanup(func() {
		// Cancel this test's queued delta before restoring shared package globals.
		require.NoError(t, model.IncreaseTokenQuota(token.Id, token.Key, 20))
		waitSaaSCreditCacheWorkers(t)
	})
	assertSaaSIntegrationTokenCache(t, token, client, 80, 40, 0)
	assertSaaSIntegrationDurableQuota(t, token, 100, 100, 0, 0)
	legacySaaSIntegrationGrant(t, router, token, "/api/token/admin/grant-quota", 50)
	assertSaaSIntegrationTokenCache(t, token, client, 130, 40, 50)
	newSaaSIntegrationGrant(t, router, token, "cache:usage", 70)
	assertSaaSIntegrationTokenCache(t, token, client, 200, 40, 120)
	_, err := model.GetTokenById(token.Id)
	require.NoError(t, err)
	_, err = model.GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	assertSaaSIntegrationTokenCache(t, token, client, 200, 40, 120)
	newSaaSIntegrationGrant(t, router, token, "cache:usage", 70)
	assertSaaSIntegrationTokenCache(t, token, client, 200, 40, 120)
	assertSaaSIntegrationDurableQuota(t, token, 220, 170, 120, 1)
}

func TestSaaSCreditCacheIntegrationColdPreCreditReadCannotRefillStaleQuota(t *testing.T) {
	router, token, client := setupSaaSCreditCacheIntegration(t)
	snapshotRead, releaseSnapshot := make(chan struct{}), make(chan struct{})
	readFinished := make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSnapshot) }) }
	callbackName := "test:saas-cold-credit-read"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "tokens" && armed.CompareAndSwap(true, false) {
			close(snapshotRead)
			<-releaseSnapshot
		}
	}))
	t.Cleanup(func() {
		release()
		select {
		case <-readFinished:
		case <-time.After(5 * time.Second):
			t.Error("cold token query did not finish during cleanup")
		}
		require.NoError(t, model.DB.Callback().Query().Remove(callbackName))
	})
	readDone := make(chan error, 1)
	go func() {
		defer close(readFinished)
		_, err := model.GetTokenByKey(token.Key, false)
		readDone <- err
	}()
	select {
	case <-snapshotRead:
	case <-time.After(5 * time.Second):
		t.Fatal("cold token query never reached its post-read barrier")
	}
	newSaaSIntegrationGrant(t, router, token, "cache:cold-race", 50)
	release()
	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cold token query did not finish after releasing its barrier")
	}
	assertSaaSIntegrationTokenCache(t, token, client, 150, 40, 50)
	assertSaaSIntegrationDurableQuota(t, token, 150, 150, 50, 1)
}
