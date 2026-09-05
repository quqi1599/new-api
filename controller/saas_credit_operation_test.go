package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const saasCreditPath = "/api/token/admin/saas-topup/credit-operations"

func setupSaaSCreditController(t *testing.T) (*gin.Engine, *model.Token) {
	t.Helper()
	db := setupInternalTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SaaSCreditOperation{}))
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "saas-admin")
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "saas-user")
	require.NoError(t, db.Model(&model.User{}).Where("id = 4").Update("quota", 100).Error)
	token := seedGrantQuotaToken(t, db, 4, "private-saas-credit-key", 100, 40, "default", -1, false)
	setSaaSTopupExcludedUsersForTest(t)
	router := gin.New()
	router.Use(sessions.Sessions("saas-test", cookie.NewStore([]byte("saas-test-secret"))))
	group := router.Group("/api/token/admin", middleware.AdminAuth(), middleware.DisableCache())
	group.POST("/saas-topup/credit-operations", CreateSaaSCreditOperation)
	group.GET("/saas-topup/credit-operations/:operationId", GetSaaSCreditOperation)
	return router, token
}

func callSaaSCreditController(t *testing.T, router *gin.Engine, method, path string, body any, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, newInternalGrantQuotaRequest(t, body))
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("Authorization", "Bearer saas-admin")
		req.Header.Set("New-Api-User", "1")
	} else {
		req.Header.Set("Authorization", "Bearer saas-user")
		req.Header.Set("New-Api-User", "4")
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSaaSCreditControllerCreateReplayGetAndConflict(t *testing.T) {
	router, token := setupSaaSCreditController(t)
	body := map[string]any{"operationId": "saas:order-1", "userId": 4, "tokenId": token.Id, "amount": 50, "creditUserQuota": true}
	for _, duplicate := range []bool{false, true} {
		rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, body, true)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		response := decodeAPIResponse(t, rr)
		require.True(t, response.Success)
		var op model.SaaSCreditOperation
		require.NoError(t, common.Unmarshal(response.Data, &op))
		assert.Equal(t, duplicate, op.Duplicated)
		assert.Equal(t, 100, op.BeforeUserQuota)
		assert.Equal(t, 150, op.AfterUserQuota)
		assert.Equal(t, 100, *op.BeforeRemainQuota)
		assert.Equal(t, 150, *op.AfterRemainQuota)
		assert.NotZero(t, op.AppliedAt)
		assert.NotContains(t, rr.Body.String(), token.Key)
	}
	rr := callSaaSCreditController(t, router, http.MethodGet, saasCreditPath+"/saas:order-1", nil, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, decodeAPIResponse(t, rr).Success)
	body["amount"] = 60
	rr = callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, body, true)
	assert.Equal(t, http.StatusConflict, rr.Code)
	rr = callSaaSCreditController(t, router, http.MethodGet, saasCreditPath+"/missing", nil, true)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestSaaSCreditControllerAccountOnlyNullsAndValidation(t *testing.T) {
	router, token := setupSaaSCreditController(t)
	rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, map[string]any{
		"operationId": "account:1", "userId": 4, "amount": 10, "creditUserQuota": true,
	}, true)
	require.Equal(t, http.StatusOK, rr.Code)
	var data map[string]any
	require.NoError(t, common.Unmarshal(decodeAPIResponse(t, rr).Data, &data))
	for _, field := range []string{"tokenId", "beforeRemainQuota", "afterRemainQuota"} {
		value, present := data[field]
		assert.True(t, present, field)
		assert.Nil(t, value, field)
	}
	for _, body := range []map[string]any{
		{"operationId": "x", "userId": 4, "tokenId": token.Id, "amount": 1},
		{"operationId": "x", "userId": 4, "amount": 1, "creditUserQuota": false},
		{"operationId": "x", "userId": 4, "tokenId": 0, "amount": 1, "creditUserQuota": true},
		{"operationId": "x y", "userId": 4, "amount": 1, "creditUserQuota": true},
		{"operationId": strings.Repeat("x", 161), "userId": 4, "amount": 1, "creditUserQuota": true},
		{"operationId": "x", "userId": 4, "amount": 0, "creditUserQuota": true},
	} {
		rr = callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, body, true)
		assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestSaaSCreditControllerHidesIneligibleTargets(t *testing.T) {
	for _, reason := range []string{"disabled-user", "disabled-token", "excluded", "mismatched-owner"} {
		t.Run(reason, func(t *testing.T) {
			router, token := setupSaaSCreditController(t)
			switch reason {
			case "disabled-user":
				require.NoError(t, model.DB.Model(&model.User{}).Where("id = 4").Update("status", common.UserStatusDisabled).Error)
			case "disabled-token":
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error)
			case "excluded":
				setSaaSTopupExcludedUsersForTest(t, 4)
			case "mismatched-owner":
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("user_id", 99).Error)
			}
			rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, map[string]any{
				"operationId": "ineligible", "userId": 4, "tokenId": token.Id, "amount": 50, "creditUserQuota": true,
			}, true)
			assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
			var quota int
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = 4").Select("quota").Scan(&quota).Error)
			assert.Equal(t, 100, quota)
		})
	}
}

func TestSaaSCreditControllerRequiresAdmin(t *testing.T) {
	router, token := setupSaaSCreditController(t)
	rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, map[string]any{
		"operationId": "unauthorized", "userId": 4, "tokenId": token.Id, "amount": 50, "creditUserQuota": true,
	}, false)
	assert.False(t, decodeAPIResponse(t, rr).Success)
	var count int64
	require.NoError(t, model.DB.Model(&model.SaaSCreditOperation{}).Count(&count).Error)
	assert.Zero(t, count)
	rr = callSaaSCreditController(t, router, http.MethodGet, saasCreditPath+"/unauthorized", nil, false)
	assert.False(t, decodeAPIResponse(t, rr).Success)
}

func TestSaaSCreditControllerAdminOwnedTokenLargeCredit(t *testing.T) {
	router, token := setupSaaSCreditController(t)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{
		"user_id": 1, "remain_quota": int64(7_000_000_000),
	}).Error)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = 1").Update("quota", int64(6_000_000_000)).Error)
	rr := callSaaSCreditController(t, router, http.MethodPost, saasCreditPath, map[string]any{
		"operationId": "saas:admin-large", "userId": 1, "tokenId": token.Id, "amount": int64(3_000_000_000), "creditUserQuota": false,
	}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	response := decodeAPIResponse(t, rr)
	require.True(t, response.Success)
	var receipt model.SaaSCreditOperation
	require.NoError(t, common.Unmarshal(response.Data, &receipt))
	assert.False(t, receipt.CreditUserQuota)
	assert.Equal(t, 6_000_000_000, receipt.BeforeUserQuota)
	assert.Equal(t, 6_000_000_000, receipt.AfterUserQuota)
	assert.Equal(t, 7_000_000_000, *receipt.BeforeRemainQuota)
	assert.Equal(t, 10_000_000_000, *receipt.AfterRemainQuota)
}
