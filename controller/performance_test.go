package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetPerformanceStatsIncludesUserCacheStats(t *testing.T) {
	common.ResetUserCacheStatsForTest()
	t.Cleanup(common.ResetUserCacheStatsForTest)

	common.RecordUserAuthCacheRead(common.UserAuthCacheReadPartial)
	common.RecordUserAuthDBFallback(common.UserAuthCacheReadPartial)
	common.RecordUserAuthDBFallbackFailure(common.UserAuthCacheReadRedisError)
	common.RecordUserCacheMutation(common.UserCacheMutationSkipped)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/performance/stats", nil)

	GetPerformanceStats(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool             `json:"success"`
		Data    PerformanceStats `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Equal(t, int64(1), response.Data.UserCacheStats.AuthCacheReadTotal.Partial)
	require.Equal(t, int64(1), response.Data.UserCacheStats.AuthDBFallbackTotal.Partial)
	require.Equal(t, int64(1), response.Data.UserCacheStats.AuthDBFallbackFailureTotal.RedisError)
	require.Equal(t, int64(1), response.Data.UserCacheStats.CacheMutationTotal.Skipped)
}
