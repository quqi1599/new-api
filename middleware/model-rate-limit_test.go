package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

func TestTokenRPMRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldRedisEnabled := common.RedisEnabled
	oldLimits := common.TokenRPMRateLimits
	common.RedisEnabled = false
	common.TokenRPMRateLimits = map[int]int{987654321: 100}
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
		common.TokenRPMRateLimits = oldLimits
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("token_id", 987654321)
	})
	router.Use(ModelRequestRateLimit())
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	for i := 0; i < 100; i++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d returned %d", i+1, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("request 101 returned %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
}
