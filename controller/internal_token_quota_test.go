package controller

import (
	"bytes"
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
	"gorm.io/gorm"
)

type grantQuotaResponse struct {
	TokenID           int    `json:"tokenId"`
	UserID            int    `json:"userId"`
	BeforeRemainQuota int    `json:"beforeRemainQuota"`
	AfterRemainQuota  int    `json:"afterRemainQuota"`
	Group             string `json:"group"`
	Status            int    `json:"status"`
	ExpiredTime       int64  `json:"expiredTime"`
	UnlimitedQuota    bool   `json:"unlimitedQuota"`
}

func seedGrantQuotaToken(t *testing.T, db *gorm.DB, userID int, rawKey string, remainQuota int, usedQuota int, group string, expiredTime int64, unlimitedQuota bool) *model.Token {
	t.Helper()

	token := &model.Token{
		UserId:         userID,
		Name:           "grant-token",
		Key:            rawKey,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    1,
		AccessedTime:   1,
		ExpiredTime:    expiredTime,
		RemainQuota:    remainQuota,
		UsedQuota:      usedQuota,
		UnlimitedQuota: unlimitedQuota,
		Group:          group,
	}
	if err := db.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

func newAdminGrantQuotaRouter() *gin.Engine {
	router := gin.New()
	store := cookie.NewStore([]byte("test-session-secret"))
	router.Use(sessions.Sessions("test-session", store))

	apiRouter := router.Group("/api")
	tokenAdminRoute := apiRouter.Group("/token/admin")
	tokenAdminRoute.Use(middleware.AdminAuth())
	tokenAdminRoute.POST("/grant-quota", AdminGrantTokenQuota)

	return router
}

func newInternalGrantQuotaRequest(t *testing.T, body any) *bytes.Reader {
	t.Helper()

	payload, err := common.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	return bytes.NewReader(payload)
}

func TestGrantTokenQuotaSuccess(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	token := seedGrantQuotaToken(t, db, 4, "grant-key-123", 100000000, 100000000, "default", -1, false)
	beforeTotalGranted := token.RemainQuota + token.UsedQuota

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  4,
		"amount":  100000000,
		"note":    "Topup order TOP20260325xxxx",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var granted grantQuotaResponse
	if err := common.Unmarshal(response.Data, &granted); err != nil {
		t.Fatalf("failed to decode grant quota response: %v", err)
	}
	if granted.TokenID != token.Id || granted.UserID != token.UserId {
		t.Fatalf("unexpected token identity: %+v", granted)
	}
	if granted.BeforeRemainQuota != 100000000 || granted.AfterRemainQuota != 200000000 {
		t.Fatalf("unexpected quota response: %+v", granted)
	}
	if granted.Group != "default" || granted.Status != common.TokenStatusEnabled || granted.ExpiredTime != -1 || granted.UnlimitedQuota {
		t.Fatalf("unexpected token metadata response: %+v", granted)
	}

	updatedToken, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updatedToken.RemainQuota != 200000000 {
		t.Fatalf("expected remain quota 200000000, got %d", updatedToken.RemainQuota)
	}
	if updatedToken.UsedQuota != 100000000 {
		t.Fatalf("expected used quota to remain 100000000, got %d", updatedToken.UsedQuota)
	}
	if updatedToken.RemainQuota+updatedToken.UsedQuota != beforeTotalGranted+100000000 {
		t.Fatalf("expected total granted quota %d, got %d", beforeTotalGranted+100000000, updatedToken.RemainQuota+updatedToken.UsedQuota)
	}
	if updatedToken.Group != token.Group || updatedToken.ExpiredTime != token.ExpiredTime || updatedToken.UnlimitedQuota != token.UnlimitedQuota {
		t.Fatalf("token metadata changed unexpectedly: before=%+v after=%+v", token, updatedToken)
	}

	var logs []model.Log
	if err := db.Where("user_id = ? AND type = ?", token.UserId, model.LogTypeTopup).Find(&logs).Error; err != nil {
		t.Fatalf("failed to query topup logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected topup log to be created")
	}
	content := logs[len(logs)-1].Content
	if !strings.Contains(content, "管理员为令牌") {
		t.Fatalf("expected token quota grant log, got %q", content)
	}
	if !strings.Contains(content, "Topup order TOP20260325xxxx") {
		t.Fatalf("expected note in token quota grant log, got %q", content)
	}
}

func TestGrantTokenQuotaRejectsInvalidAmount(t *testing.T) {
	setupInternalTokenControllerTestDB(t)

	body := map[string]any{
		"tokenId": 1,
		"userId":  1,
		"amount":  0,
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected invalid amount failure")
	}
	if response.Message != "invalid amount" {
		t.Fatalf("expected invalid amount message, got %q", response.Message)
	}
}

func TestGrantTokenQuotaRestoresExhaustedStatus(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	token := seedGrantQuotaToken(t, db, 4, "grant-key-exhausted", 0, 100, "default", -1, false)
	if err := db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusExhausted).Error; err != nil {
		t.Fatalf("failed to set token exhausted status: %v", err)
	}

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  4,
		"amount":  10,
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var granted grantQuotaResponse
	if err := common.Unmarshal(response.Data, &granted); err != nil {
		t.Fatalf("failed to decode grant quota response: %v", err)
	}
	if granted.Status != common.TokenStatusEnabled {
		t.Fatalf("expected response status %d, got %d", common.TokenStatusEnabled, granted.Status)
	}

	updatedToken, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload token: %v", err)
	}
	if updatedToken.Status != common.TokenStatusEnabled {
		t.Fatalf("expected token status %d, got %d", common.TokenStatusEnabled, updatedToken.Status)
	}
	if updatedToken.RemainQuota != 10 {
		t.Fatalf("expected remain quota 10, got %d", updatedToken.RemainQuota)
	}
}

func TestGrantTokenQuotaReturnsNotFound(t *testing.T) {
	setupInternalTokenControllerTestDB(t)

	body := map[string]any{
		"tokenId": 9999,
		"userId":  1,
		"amount":  1,
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected token not found failure")
	}
	if response.Message != "token not found" {
		t.Fatalf("expected token not found message, got %q", response.Message)
	}
}

func TestGrantTokenQuotaRejectsTokenUserMismatch(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	token := seedGrantQuotaToken(t, db, 4, "grant-key-mismatch", 100, 100, "default", -1, false)

	body := map[string]any{
		"tokenId": token.Id,
		"userId":  5,
		"amount":  10,
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/admin/grant-quota", body, 1)
	GrantTokenQuota(ctx)

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected token user mismatch failure")
	}
	if response.Message != "token user mismatch" {
		t.Fatalf("expected token user mismatch message, got %q", response.Message)
	}
}

func TestAdminGrantQuotaRouteAcceptsAdmin(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	token := seedGrantQuotaToken(t, db, 4, "grant-route-key-456", 100, 100, "default", -1, false)

	router := newAdminGrantQuotaRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/token/admin/grant-quota", newInternalGrantQuotaRequest(t, map[string]any{
		"tokenId": token.Id,
		"userId":  4,
		"amount":  10,
		"note":    "Topup order TOP20260325xxxx",
	}))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for authorized request, got %d", recorder.Code)
	}

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}

	var granted grantQuotaResponse
	if err := common.Unmarshal(response.Data, &granted); err != nil {
		t.Fatalf("failed to decode route grant quota response: %v", err)
	}
	if granted.TokenID != token.Id || granted.UserID != token.UserId {
		t.Fatalf("unexpected route grant quota response: %+v", granted)
	}
}

func TestAdminGrantQuotaRouteRejectsNonAdminAccessToken(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 2, common.RoleCommonUser, "default", "common-access-token")
	token := seedGrantQuotaToken(t, db, 4, "grant-route-key-789", 100, 100, "default", -1, false)

	router := newAdminGrantQuotaRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/token/admin/grant-quota", newInternalGrantQuotaRequest(t, map[string]any{
		"tokenId": token.Id,
		"userId":  4,
		"amount":  10,
	}))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer common-access-token")
	request.Header.Set("New-Api-User", "2")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected existing AdminAuth semantics to keep 200 status, got %d", recorder.Code)
	}

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected non-admin request to fail")
	}
	if !strings.Contains(response.Message, "auth.insufficient_privilege") {
		t.Fatalf("expected permission denied message, got %q", response.Message)
	}
}
