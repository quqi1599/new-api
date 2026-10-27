package middleware

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type distributorReadFailure struct{ err error }

func (r distributorReadFailure) Read([]byte) (int, error) { return 0, r.err }

type distributorBrokenStorage struct{}

func (distributorBrokenStorage) Read([]byte) (int, error) { return 0, io.EOF }
func (distributorBrokenStorage) Seek(int64, int) (int64, error) {
	return 0, errors.New("private-storage-path/secret-fixture")
}
func (distributorBrokenStorage) Close() error           { return nil }
func (distributorBrokenStorage) Size() int64            { return 0 }
func (distributorBrokenStorage) IsDisk() bool           { return true }
func (distributorBrokenStorage) Bytes() ([]byte, error) { return nil, nil }
func (distributorBrokenStorage) ReleaseAdmission()      {}

func TestDistributorBodyFailuresAbortBeforeSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	oldMax := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = oldMax })
	const body = `{"model":"fixture-model"}`
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		for _, tc := range []struct {
			name    string
			status  int
			code    types.ErrorCode
			message string
		}{
			{"short-valid-json", 400, types.ErrorCodeRequestBodyIncomplete, "request body incomplete"},
			{"unknown-length-truncated", 400, types.ErrorCodeRequestBodyIncomplete, "request body incomplete"},
			{"wire-read", 400, types.ErrorCodeReadRequestBodyFailed, "failed to read request body"},
			{"too-large", 413, types.ErrorCodeRequestBodyTooLarge, "request body too large"},
			{"internal-storage", 500, types.ErrorCodeInternalStorageError, "internal request body storage error"},
			{"invalid-json", 400, "", "Invalid request:"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				reached := false
				var captured *gin.Context
				router := gin.New()
				router.Use(func(c *gin.Context) {
					captured = c
					if tc.name == "internal-storage" {
						c.Set(common.KeyBodyStorage, distributorBrokenStorage{})
					}
					c.Next()
				}, BodyStorageCleanup(), Distribute())
				router.POST(path, func(c *gin.Context) { reached = true; c.Status(http.StatusOK) })
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				switch tc.name {
				case "short-valid-json":
					req.ContentLength += 7
				case "unknown-length-truncated":
					req.ContentLength = -1
					req.Body = io.NopCloser(io.MultiReader(strings.NewReader(body), distributorReadFailure{fmt.Errorf("private-reader-fixture: %w", io.ErrUnexpectedEOF)}))
				case "wire-read":
					req.ContentLength = -1
					req.Body = io.NopCloser(distributorReadFailure{errors.New("private-network-fixture")})
				case "too-large":
					req.ContentLength = 2 << 20
				case "invalid-json":
					req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":`))
					req.Header.Set("Content-Type", "application/json")
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
				var payload struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
				require.Equal(t, string(tc.code), payload.Error.Code)
				require.Contains(t, payload.Error.Message, tc.message)
				require.NotContains(t, recorder.Body.String(), "private-")
				require.NotContains(t, recorder.Body.String(), "secret-fixture")
				require.True(t, captured.IsAborted())
				require.False(t, reached, "controller/billing handler must not execute")
				require.True(t, captured.GetTime("relay_retry_started_at").IsZero(), "selection budget must not start")
				require.Zero(t, captured.GetInt("channel_id"))
			})
		}
	}
}
