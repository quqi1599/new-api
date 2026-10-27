package service

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

// ClassifyRequestBodyFailure keeps intake errors consistent before and after routing.
func ClassifyRequestBodyFailure(err error) (statusCode int, errorCode types.ErrorCode, publicMessage string, classified bool) {
	switch {
	case common.IsRequestBodyTooLargeError(err), errors.Is(err, common.ErrRequestBodyTooLarge):
		return http.StatusRequestEntityTooLarge, types.ErrorCodeRequestBodyTooLarge, "request body too large", true
	case common.IsIncompleteBodyError(err):
		return http.StatusBadRequest, types.ErrorCodeRequestBodyIncomplete, "request body incomplete", true
	case common.IsBodyReadError(err):
		return http.StatusBadRequest, types.ErrorCodeReadRequestBodyFailed, "failed to read request body", true
	case common.IsBodyAdmissionError(err):
		return http.StatusServiceUnavailable, types.ErrorCodeRequestBodyCapacity, "request body capacity is temporarily exhausted", true
	case common.IsInternalBodyStorageError(err):
		return http.StatusInternalServerError, types.ErrorCodeInternalStorageError, "internal request body storage error", true
	default:
		return http.StatusBadRequest, types.ErrorCodeReadRequestBodyFailed, "", false
	}
}
