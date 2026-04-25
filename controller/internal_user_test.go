package controller

import (
	"bytes"
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
)

type internalUserResponse struct {
	ID           int    `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	Role         int    `json:"role"`
	Status       int    `json:"status"`
	Quota        int    `json:"quota"`
	UsedQuota    int    `json:"used_quota"`
	RequestCount int    `json:"request_count"`
	Group        string `json:"group"`
}

type internalUsersPageResponse struct {
	Items []internalUserResponse `json:"items"`
	Total int                    `json:"total"`
}

func newInternalUserRouter() *gin.Engine {
	router := gin.New()
	store := cookie.NewStore([]byte("test-session-secret"))
	router.Use(sessions.Sessions("test-session", store))

	apiRouter := router.Group("/api")
	internalAdminRoute := apiRouter.Group("/internal/admin")
	internalAdminRoute.Use(middleware.AdminAuth())
	internalAdminRoute.GET("/users", InternalAdminListUsers)
	internalAdminRoute.GET("/users/search", InternalAdminSearchUsers)
	internalAdminRoute.GET("/users/:id", InternalAdminGetUser)
	internalAdminRoute.POST("/users", InternalAdminCreateUser)
	internalAdminRoute.PUT("/users/:id", InternalAdminUpdateUser)
	internalAdminRoute.DELETE("/users/:id", InternalAdminDeleteUser)
	return router
}

func newInternalUserRequest(t *testing.T, body any) *bytes.Reader {
	t.Helper()
	payload, err := common.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	return bytes.NewReader(payload)
}

func TestInternalAdminListUsersWithAccessToken(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	seedInternalUser(t, db, 2, common.RoleCommonUser, "default", "user-access-token")

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/internal/admin/users?p=1&page_size=10", nil)
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin list users, got %d", recorder.Code)
	}

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}

	var page internalUsersPageResponse
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode users page response: %v", err)
	}
	if page.Total < 2 || len(page.Items) < 2 {
		t.Fatalf("expected at least two users in response, got %+v", page)
	}
}

func TestInternalAdminGetUserRejectsNonAdmin(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 2, common.RoleCommonUser, "default", "common-access-token")
	target := seedInternalUser(t, db, 4, common.RoleCommonUser, "default", "target-access-token")

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/internal/admin/users/"+strconv.Itoa(target.Id), nil)
	request.Header.Set("Authorization", "Bearer common-access-token")
	request.Header.Set("New-Api-User", "2")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected non-admin access to fail")
	}
	if !strings.Contains(response.Message, "auth.insufficient_privilege") {
		t.Fatalf("expected permission denied message, got %q", response.Message)
	}
}

func TestInternalAdminUpdateUserQuotaPreservesExistingFields(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	target := seedInternalUser(t, db, 7, common.RoleCommonUser, "vip", "target-access-token")
	target.Quota = 100
	if err := db.Model(&model.User{}).Where("id = ?", target.Id).Update("quota", 100).Error; err != nil {
		t.Fatalf("failed to update initial quota: %v", err)
	}

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodPut, "/api/internal/admin/users/"+strconv.Itoa(target.Id), newInternalUserRequest(t, map[string]any{
		"quota": 260,
	}))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for quota update, got %d", recorder.Code)
	}

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}

	var updated internalUserResponse
	if err := common.Unmarshal(response.Data, &updated); err != nil {
		t.Fatalf("failed to decode updated user: %v", err)
	}
	if updated.Quota != 260 {
		t.Fatalf("expected quota 260, got %d", updated.Quota)
	}
	if updated.Username != target.Username || updated.Group != "vip" {
		t.Fatalf("unexpected preserved fields after update: %+v", updated)
	}
}

func TestInternalAdminUpdateUserQuotaWritesManageLogWithNote(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	target := seedInternalUser(t, db, 8, common.RoleCommonUser, "default", "target-access-token")
	if err := db.Model(&model.User{}).Where("id = ?", target.Id).Update("quota", 100).Error; err != nil {
		t.Fatalf("failed to update initial quota: %v", err)
	}

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodPut, "/api/internal/admin/users/"+strconv.Itoa(target.Id), newInternalUserRequest(t, map[string]any{
		"quota": 180,
		"note":  "Topup order BUY20260406; source saas",
	}))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}

	var logs []model.Log
	if err := db.Where("user_id = ? AND type = ?", target.Id, model.LogTypeManage).Find(&logs).Error; err != nil {
		t.Fatalf("failed to query manage logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected manage log to be created")
	}
	content := logs[len(logs)-1].Content
	if !strings.Contains(content, "管理员将用户额度从") {
		t.Fatalf("expected quota change content, got %q", content)
	}
	if !strings.Contains(content, "Topup order BUY20260406; source saas") {
		t.Fatalf("expected note in log content, got %q", content)
	}
}

func TestInternalAdminCreateUserSucceeds(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/admin/users", newInternalUserRequest(t, map[string]any{
		"username":     "internal_created",
		"password":     "password123",
		"display_name": "Internal User",
		"role":         common.RoleCommonUser,
	}))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for create user, got %d", recorder.Code)
	}

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got %+v", response)
	}

	var created internalUserResponse
	if err := common.Unmarshal(response.Data, &created); err != nil {
		t.Fatalf("failed to decode created user: %v", err)
	}
	if created.Username != "internal_created" || created.DisplayName != "Internal User" {
		t.Fatalf("unexpected created user payload: %+v", created)
	}
}

func TestInternalAdminDeleteUserRejectsSameLevelAdmin(t *testing.T) {
	db := setupInternalTokenControllerTestDB(t)
	seedInternalUser(t, db, 1, common.RoleAdminUser, "default", "admin-access-token")
	target := seedInternalUser(t, db, 3, common.RoleAdminUser, "default", "other-admin-token")

	router := newInternalUserRouter()
	request := httptest.NewRequest(http.MethodDelete, "/api/internal/admin/users/"+strconv.Itoa(target.Id), nil)
	request.Header.Set("Authorization", "Bearer admin-access-token")
	request.Header.Set("New-Api-User", "1")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := decodeAPIResponse(t, recorder)
	if response.Success {
		t.Fatalf("expected same-level admin delete to fail")
	}
	if !strings.Contains(response.Message, "user.no_permission_higher_level") {
		t.Fatalf("expected permission message, got %q", response.Message)
	}
}
