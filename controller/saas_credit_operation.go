package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func respondSaaSCreditOperationError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, "credit operation failed"
	switch {
	case errors.Is(err, model.ErrSaaSCreditInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, model.ErrSaaSCreditConflict):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, model.ErrSaaSCreditOverflow):
		status, message = http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, model.ErrSaaSCreditTargetNotFound):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = http.StatusNotFound, "credit operation not found"
	default:
		common.SysLog("SaaS credit operation failed: " + err.Error())
	}
	c.AbortWithStatusJSON(status, gin.H{"success": false, "message": message})
}

func CreateSaaSCreditOperation(c *gin.Context) {
	// Require the explicit accounting policy, including explicit false.
	var req struct {
		OperationID     string `json:"operationId"`
		UserID          int    `json:"userId"`
		TokenID         *int   `json:"tokenId"`
		Amount          int    `json:"amount"`
		CreditUserQuota *bool  `json:"creditUserQuota"`
		Note            string `json:"note"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.CreditUserQuota == nil {
		respondSaaSCreditOperationError(c, model.ErrSaaSCreditInvalid)
		return
	}
	op, err := model.ApplySaaSCreditOperation(c.Request.Context(), model.SaaSCreditOperationRequest{
		OperationID: req.OperationID, UserID: req.UserID, TokenID: req.TokenID,
		Amount: req.Amount, CreditUserQuota: *req.CreditUserQuota, Note: req.Note,
	})
	if err != nil {
		respondSaaSCreditOperationError(c, err)
		return
	}
	common.ApiSuccess(c, op)
}

func GetSaaSCreditOperation(c *gin.Context) {
	op, err := model.GetSaaSCreditOperation(c.Request.Context(), c.Param("operationId"))
	if err != nil {
		respondSaaSCreditOperationError(c, err)
		return
	}
	common.ApiSuccess(c, op)
}
