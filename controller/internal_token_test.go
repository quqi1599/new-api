package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type adminTokenSearchPage struct {
	Total int            `json:"total"`
	Items []*model.Token `json:"items"`
}

func setupInternalTokenControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db := setupTokenControllerTestDB(t)
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatalf("failed to migrate user table: %v", err)
	}
	return db
}

func seedInternalUser(t *testing.T, db *gorm.DB, userID int, role int, group string, accessToken string) *model.User {
	t.Helper()

	token := accessToken
	user := &model.User{
		Id:          userID,
		Username:    fmt.Sprintf("user_%d", userID),
		Password:    "password123",
		DisplayName: fmt.Sprintf("User %d", userID),
		Role:        role,
		Status:      common.UserStatusEnabled,
		Email:       fmt.Sprintf("user_%d@example.com", userID),
		Group:       group,
		AffCode:     fmt.Sprintf("aff%d", userID),
		AccessToken: &token,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return user
}

func seedInternalLookupToken(t *testing.T, db *gorm.DB, userID int, name string, rawKey string, group string, unlimitedQuota bool) *model.Token {
	t.Helper()

	token := &model.Token{
		UserId:         userID,
		Name:           name,
		Key:            rawKey,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    1,
		AccessedTime:   1,
		ExpiredTime:    -1,
		RemainQuota:    100,
		UnlimitedQuota: unlimitedQuota,
		Group:          group,
	}
	if err := db.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

func newAdminTokenSearchRouter() *gin.Engine {
	router := gin.New()
	store := cookie.NewStore([]byte("test-session-secret"))
	router.Use(sessions.Sessions("test-session", store))

	apiRouter := router.Group("/api")
	tokenAdminRoute := apiRouter.Group("/token/admin")
	tokenAdminRoute.Use(middleware.AdminAuth())
	tokenAdminRoute.GET("/", AdminGetAllTokens)
	tokenAdminRoute.GET("/search", AdminSearchTokens)
	tokenAdminRoute.POST("/:id/key", AdminGetTokenKey)
	tokenAdminRoute.PUT("/", middleware.RootAuth(), AdminUpdateToken)
	tokenAdminRoute.DELETE("/:id", middleware.RootAuth(), AdminDeleteToken)

	return router
}

func TestAdminSearchTokensSuccess(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	token := seedInternalLookupToken(t, db, 4, "自动发货-3-24-V6rhKT", "resolve-key-123", "default", false)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?token=sk-resolve-key-123", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode search response: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected one search result, got %+v", page)
	}
	found := page.Items[0]
	if found.Id != token.Id || found.UserId != token.UserId {
		t.Fatalf("unexpected token identity: %+v", found)
	}
	if found.Name != token.Name || found.Group != token.Group {
		t.Fatalf("unexpected token details: %+v", found)
	}
	if found.Status != token.Status || found.ExpiredTime != token.ExpiredTime || found.UnlimitedQuota != token.UnlimitedQuota {
		t.Fatalf("unexpected token flags: %+v", found)
	}
	if strings.Contains(recorder.Body.String(), token.Key) {
		t.Fatalf("search response leaked raw token key: %s", recorder.Body.String())
	}
}

func TestAdminSearchTokensSupportsKeyWithoutPrefix(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user-access-token")
	seedInternalLookupToken(t, db, 4, "plain-token", "resolve-key-no-prefix", "default", false)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?token=resolve-key-no-prefix", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected search success")
	}
}

func TestAdminSearchTokensReturnsEmptyWhenNotFound(t *testing.T) {
	setupInternalTokenControllerTestDB(t)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?token=sk-missing-key", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected empty search success")
	}
	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode empty search response: %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("expected empty search result, got %+v", page)
	}
}

func TestAdminSearchTokensFallsBackToUserGroup(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 9, common.RoleCommonUser, "vip", "vip-access-token")
	seedInternalLookupToken(t, db, 9, "group-fallback-token", "fallback-key-123", "", true)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?token=sk-fallback-key-123", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode fallback response: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Group != "vip" {
		t.Fatalf("expected fallback group vip, got %+v", page.Items)
	}
}

func TestAdminSearchTokensSupportsFuzzyKeywordAndUsername(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 8, common.RoleCommonUser, "default", "user8-access-token")
	token := seedInternalLookupToken(t, db, 8, "alpha-diagnose-token", "alpha-key-123", "default", false)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?keyword=diagnose&username=user_8", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode fuzzy search response: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected one fuzzy search result, got %+v", page)
	}
	if page.Items[0].Id != token.Id || page.Items[0].Username != "user_8" {
		t.Fatalf("unexpected fuzzy search result: %+v", page.Items[0])
	}
}

func TestAdminSearchTokensSupportsFuzzyTokenKey(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 11, common.RoleCommonUser, "default", "user11-access-token")
	token := seedInternalLookupToken(t, db, 11, "beta-token", "diagnose-key-xyz", "default", false)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/admin/search?token=key-xy", nil, 1)
	AdminSearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode fuzzy token search response: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != token.Id {
		t.Fatalf("unexpected fuzzy token search response: %+v", page.Items)
	}
}

func TestAdminGetTokenKeyRouteAcceptsAdmin(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	token := seedInternalLookupToken(t, db, 4, "route-token", "route-real-key-123", "default", false)

	router := newAdminTokenSearchRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/token/admin/"+strconv.Itoa(token.Id)+"/key", nil)
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}
	var payload struct {
		Key string `json:"key"`
	}
	if err := common.Unmarshal(response.Data, &payload); err != nil {
		t.Fatalf("failed to decode admin token key response: %v", err)
	}
	if payload.Key != token.Key {
		t.Fatalf("expected full key %q, got %q", token.Key, payload.Key)
	}
}

func TestAdminUpdateTokenRouteRejectsAdminButAcceptsRoot(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	seedInternalUser(t, db, 2, common.RoleRootUser, "default", "root-access-token")
	seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "user4-access-token")
	token := seedInternalLookupToken(t, db, 4, "editable-token", "editable-real-key", "default", false)

	router := newAdminTokenSearchRouter()

	adminBody, err := common.Marshal(map[string]any{
		"id":                   token.Id,
		"name":                 "updated-by-admin",
		"remain_quota":         token.RemainQuota,
		"expired_time":         token.ExpiredTime,
		"unlimited_quota":      token.UnlimitedQuota,
		"model_limits":         "",
		"model_limits_enabled": false,
		"group":                token.Group,
		"cross_group_retry":    false,
	})
	if err != nil {
		t.Fatalf("failed to marshal admin update body: %v", err)
	}
	adminRequest := httptest.NewRequest(http.MethodPut, "/api/token/admin/", bytes.NewReader(adminBody))
	adminRequest.Header.Set("Content-Type", "application/json")
	adminRequest.Header.Set("Authorization", "Bearer admin-access-token")
	adminRequest.Header.Set("New-Api-User", "1")
	adminRecorder := httptest.NewRecorder()
	router.ServeHTTP(adminRecorder, adminRequest)
	adminResponse := decodeAPIResponse(t, adminRecorder)
	if adminResponse.Success {
		t.Fatalf("expected admin update to fail")
	}

	rootBody, err := common.Marshal(map[string]any{
		"id":                   token.Id,
		"name":                 "updated-by-root",
		"remain_quota":         token.RemainQuota,
		"expired_time":         token.ExpiredTime,
		"unlimited_quota":      token.UnlimitedQuota,
		"model_limits":         "",
		"model_limits_enabled": false,
		"group":                token.Group,
		"cross_group_retry":    false,
	})
	if err != nil {
		t.Fatalf("failed to marshal root update body: %v", err)
	}
	rootRequest := httptest.NewRequest(http.MethodPut, "/api/token/admin/", bytes.NewReader(rootBody))
	rootRequest.Header.Set("Content-Type", "application/json")
	rootRequest.Header.Set("Authorization", "Bearer root-access-token")
	rootRequest.Header.Set("New-Api-User", "2")
	rootRecorder := httptest.NewRecorder()
	router.ServeHTTP(rootRecorder, rootRequest)
	rootResponse := decodeAPIResponse(t, rootRecorder)
	if !rootResponse.Success {
		t.Fatalf("expected root update to succeed, got %+v", rootResponse)
	}

	updatedToken, err := model.GetTokenById(token.Id)
	if err != nil {
		t.Fatalf("failed to reload updated token: %v", err)
	}
	if updatedToken.Name != "updated-by-root" {
		t.Fatalf("expected token name updated-by-root, got %q", updatedToken.Name)
	}
}

func TestAdminTokenSearchRouteAcceptsAdmin(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	token := seedInternalLookupToken(t, db, 4, "route-token", "route-key-456", "default", false)

	router := newAdminTokenSearchRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/token/admin/search?token=sk-route-key-456", nil)
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

	var page adminTokenSearchPage
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode route search response: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != token.Id || page.Items[0].UserId != token.UserId {
		t.Fatalf("unexpected route search response: %+v", page)
	}
}

func TestAdminTokenSearchRouteRejectsNonAdminAccessToken(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 2, common.RoleCommonUser, "default", "common-access-token")
	seedInternalLookupToken(t, db, 4, "route-token", "route-key-789", "default", false)

	router := newAdminTokenSearchRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/token/admin/search?token=sk-route-key-789", nil)
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
func TestAdminGetTokenKeyRouteReturnsFullKey(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	token := seedInternalLookupToken(t, db, 4, "route-token", "route-key-secret", "default", false)

	router := newAdminTokenSearchRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/token/admin/"+strconv.Itoa(token.Id)+"/key", nil)
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

	var keyData tokenKeyResponse
	if err := common.Unmarshal(response.Data, &keyData); err != nil {
		t.Fatalf("failed to decode token key response: %v", err)
	}
	if keyData.Key != token.GetFullKey() {
		t.Fatalf("expected full key %q, got %q", token.GetFullKey(), keyData.Key)
	}
}
