package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDistributorReturnsStructuredServiceUnavailableWhenBodyAdmissionIsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldLimit := constant.MaxConcurrentLargeRequestBodies
	constant.MaxConcurrentLargeRequestBodies = 1
	t.Cleanup(func() { constant.MaxConcurrentLargeRequestBodies = oldLimit })
	common.ResetRequestBodyStats()
	t.Cleanup(common.ResetRequestBodyStats)

	body := bytes.Repeat([]byte("x"), 1<<20)
	occupied, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	Distribute()(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, string(types.ErrorCodeRequestBodyCapacity), payload.Error.Code)
	require.Contains(t, payload.Error.Message, "request body capacity is temporarily exhausted")
	require.Equal(t, int64(1), common.GetRequestBodyStats().RejectedLargeBodyAdmissionsTotal)
}
