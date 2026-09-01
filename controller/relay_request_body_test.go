package controller

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestRequestBodyFailureStatus(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantCode       types.ErrorCode
		wantClassified bool
	}{
		{
			name:           "too large",
			err:            common.ErrRequestBodyTooLarge,
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantCode:       types.ErrorCodeRequestBodyTooLarge,
			wantClassified: true,
		},
		{
			name: "incomplete",
			err: &common.IncompleteBodyError{
				Declared: 10,
				Received: 3,
				Storage:  "memory",
				Cause:    io.ErrUnexpectedEOF,
			},
			wantStatus:     http.StatusBadRequest,
			wantCode:       types.ErrorCodeRequestBodyIncomplete,
			wantClassified: true,
		},
		{
			name: "internal storage",
			err: &common.InternalBodyStorageError{
				Storage:   "disk",
				Operation: "write",
				Cause:     errors.New("disk failure"),
			},
			wantStatus:     http.StatusInternalServerError,
			wantCode:       types.ErrorCodeInternalStorageError,
			wantClassified: true,
		},
		{
			name:           "other malformed body",
			err:            &common.BodyReadError{Declared: -1, Received: 12, Cause: errors.New("invalid gzip checksum")},
			wantStatus:     http.StatusBadRequest,
			wantCode:       types.ErrorCodeReadRequestBodyFailed,
			wantClassified: true,
		},
		{
			name:           "large body admission saturated",
			err:            &common.BodyAdmissionError{},
			wantStatus:     http.StatusServiceUnavailable,
			wantCode:       types.ErrorCodeRequestBodyCapacity,
			wantClassified: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusCode, classified := requestBodyFailureStatus(tt.err)
			require.Equal(t, tt.wantStatus, statusCode)
			require.Equal(t, tt.wantClassified, classified)

			apiErr := newRequestBodyFailure(nil, tt.err)
			require.Equal(t, tt.wantStatus, apiErr.StatusCode)
			require.Equal(t, tt.wantCode, apiErr.GetErrorCode())
			require.True(t, types.IsSkipRetryError(apiErr), "body read failures must not enter retry dispatch")
			if tt.wantCode == types.ErrorCodeInternalStorageError {
				require.Equal(t, "internal request body storage error", apiErr.Error())
				require.NotContains(t, apiErr.Error(), "disk failure")
			}
			if tt.wantCode == types.ErrorCodeReadRequestBodyFailed {
				require.Equal(t, "failed to read request body", apiErr.Error())
				require.NotContains(t, apiErr.Error(), "gzip checksum")
			}
		})
	}
}

func TestInvalidRequestErrorReturnsBadRequestWithoutRetry(t *testing.T) {
	apiErr := newInvalidRequestError(errors.New("missing required field"))
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiErr))
}
