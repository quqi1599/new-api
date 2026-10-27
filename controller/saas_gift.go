package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func respondSaaSGiftError(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "gift_operation_failed", "gift operation failed"
	switch {
	case errors.Is(err, model.ErrSaaSGiftInvalid), errors.Is(err, model.ErrSaaSCreditInvalid):
		status, code, message = http.StatusBadRequest, "invalid_gift_operation", "invalid gift operation"
	case errors.Is(err, model.ErrSaaSGiftConflict), errors.Is(err, model.ErrSaaSCreditConflict):
		status, code, message = http.StatusConflict, "gift_operation_conflict", "operation identity conflicts with its stored request"
	case errors.Is(err, model.ErrSaaSGiftFrozen):
		status, code, message = http.StatusConflict, "gift_account_frozen", "gift account is frozen pending review"
	case errors.Is(err, model.ErrSaaSGiftPolicy), errors.Is(err, model.ErrSaaSGiftRestricted):
		status, code, message = http.StatusConflict, "gift_policy_not_ready", "gift group or account policy is not ready"
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, model.ErrSaaSCreditTargetNotFound):
		status, code, message = http.StatusNotFound, "gift_not_found", "gift account or operation not found"
	case errors.Is(err, model.ErrSaaSCreditOverflow):
		status, code, message = http.StatusUnprocessableEntity, "gift_quota_overflow", "gift quota exceeds the supported limit"
	}
	c.AbortWithStatusJSON(status, gin.H{"success": false, "code": code, "message": message})
}

func EnsureSaaSGiftAccount(c *gin.Context) {
	var req model.SaaSGiftEnsureRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		respondSaaSGiftError(c, model.ErrSaaSGiftInvalid)
		return
	}
	account, err := model.EnsureSaaSGiftAccount(c.Request.Context(), req)
	if err != nil {
		respondSaaSGiftError(c, err)
		return
	}
	common.ApiSuccess(c, account)
}

func CreateSaaSGiftOperation(c *gin.Context) {
	var req model.SaaSGiftOperationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		respondSaaSGiftError(c, model.ErrSaaSGiftInvalid)
		return
	}
	op, err := model.ApplySaaSGiftOperation(c.Request.Context(), req)
	if err != nil {
		respondSaaSGiftError(c, err)
		return
	}
	common.ApiSuccess(c, op)
}

func GetSaaSGiftOperation(c *gin.Context) {
	op, err := model.GetSaaSGiftOperation(c.Request.Context(), c.Param("operationId"))
	if err != nil {
		respondSaaSGiftError(c, err)
		return
	}
	common.ApiSuccess(c, op)
}
