package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func setupResponsesAuthFixture(t *testing.T) string {
	t.Helper()
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	oldDB, oldRedis, oldSQLite := model.DB, common.RedisEnabled, common.UsingSQLite
	model.DB, common.RedisEnabled, common.UsingSQLite = db, false, true
	t.Cleanup(func() {
		model.DB, common.RedisEnabled, common.UsingSQLite = oldDB, oldRedis, oldSQLite
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
	require.NoError(t, db.Create(&model.User{Id: 910001, Username: "responses-auth-fixture", Status: common.UserStatusEnabled, Group: "default"}).Error)
	key := "syntheticResponsesAuthKey12345678"
	require.NoError(t, db.Create(&model.Token{Id: 910002, UserId: 910001, Key: key, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	return key
}

func TestResponsesTokenAuthPreservesValidAuthorization(t *testing.T) {
	key := setupResponsesAuthFixture(t)
	for _, tc := range []struct {
		name, authorization, subprotocol string
		status                           int
	}{
		{"ordinary bearer", "Bearer sk-" + key, "", http.StatusNoContent},
		{"unrelated websocket protocols", "Bearer sk-" + key, "responses, openai-beta.responses-v1", http.StatusNoContent},
		{"mixed case scheme", "bEaReR sk-" + key, "", http.StatusNoContent},
		{"uppercase scheme", "BEARER sk-" + key, "", http.StatusNoContent},
		{"empty websocket credential", "Bearer sk-" + key, "openai-insecure-api-key.", http.StatusNoContent},
		{"malformed websocket key prefix", "Bearer sk-" + key, "openai-insecure-api-keySuffix.sk-unknown", http.StatusNoContent},
		{"websocket credential", "", "responses, openai-insecure-api-key.sk-" + key, http.StatusNoContent},
		{"explicit wrong websocket credential", "Bearer sk-" + key, "openai-insecure-api-key.sk-unknown", http.StatusUnauthorized},
		{"missing credential", "", "responses", http.StatusUnauthorized},
		{"invalid credential", "Bearer sk-unknown", "responses", http.StatusUnauthorized},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				called := false
				router := gin.New()
				router.Use(RequestId(), TokenAuth())
				router.Handle(method, "/v1/responses", func(c *gin.Context) {
					called = true
					require.Equal(t, key, c.GetString("token_key"))
					c.Status(http.StatusNoContent)
				})
				request := httptest.NewRequest(method, "/v1/responses", nil)
				request.Header.Set("Authorization", tc.authorization)
				request.Header.Set("Sec-WebSocket-Protocol", tc.subprotocol)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
				require.Equal(t, tc.status == http.StatusNoContent, called)
				require.NotContains(t, recorder.Body.String(), key)
				require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
			})
		}
	}
}

func TestResponsesTokenAuthRejectsUnusableTokensWithPrivateDiagnostics(t *testing.T) {
	key := setupResponsesAuthFixture(t)
	var logs bytes.Buffer
	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})
	for _, tc := range []struct {
		name, expected string
		status         int
		expires        int64
		quota          int
	}{
		{"disabled", "token_disabled", common.TokenStatusDisabled, -1, 10},
		{"expired status", "token_expired", common.TokenStatusExpired, -1, 10},
		{"expired time", "token_expired", common.TokenStatusEnabled, 1, 10},
		{"exhausted status", "token_exhausted", common.TokenStatusExhausted, -1, 10},
		{"exhausted quota", "token_exhausted", common.TokenStatusEnabled, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 910002).Updates(map[string]any{
				"status": tc.status, "expired_time": tc.expires, "remain_quota": tc.quota, "unlimited_quota": false,
			}).Error)
			logs.Reset()
			reason := ""
			router := gin.New()
			router.Use(RequestId(), func(c *gin.Context) {
				c.Next()
				reason = c.GetString("token_auth_failure_reason")
			}, TokenAuth())
			router.POST("/v1/responses", func(c *gin.Context) { t.Error("unusable token reached relay") })
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("private-fixture-body"))
			request.Header.Set("Authorization", "Bearer sk-"+key)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
			require.Equal(t, tc.expected, reason)
			require.Contains(t, logs.String(), "token_auth_rejected reason="+tc.expected)
			require.Contains(t, logs.String(), recorder.Header().Get(common.RequestIdKey))
			for _, private := range []string{key, "private-fixture-body", "responses-auth-fixture"} {
				require.NotContains(t, logs.String(), private)
				require.NotContains(t, recorder.Body.String(), private)
			}
			require.NotContains(t, recorder.Body.String(), tc.expected, "public error reveals token state")
		})
	}
}

func TestResponsesTokenAuthRetainsProviderSecretFallback(t *testing.T) {
	key := setupResponsesAuthFixture(t)
	router := gin.New()
	router.Use(TokenAuth())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer ")
	request.Header.Set("mj-api-secret", "Bearer sk-"+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code)
}
