package controller

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type GrantTokenQuotaRequest struct {
	TokenID int    `json:"tokenId"`
	UserID  int    `json:"userId"`
	Amount  int    `json:"amount"`
	Note    string `json:"note"`
}

type GrantTokenQuotaResponse struct {
	TokenID           int    `json:"tokenId"`
	UserID            int    `json:"userId"`
	BeforeRemainQuota int    `json:"beforeRemainQuota"`
	AfterRemainQuota  int    `json:"afterRemainQuota"`
	Group             string `json:"group"`
	Status            int    `json:"status"`
	ExpiredTime       int64  `json:"expiredTime"`
	UnlimitedQuota    bool   `json:"unlimitedQuota"`
}

func formatTokenQuotaGrantLog(token *model.Token, amount int, note string) string {
	message := fmt.Sprintf("管理员为令牌 %s(ID:%d) 增加额度 %s", token.Name, token.Id, logger.LogQuota(amount))
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return message
	}
	return fmt.Sprintf("%s，备注：%s", message, trimmed)
}

func GrantTokenQuota(c *gin.Context) {
	var req GrantTokenQuotaRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}

	if req.TokenID <= 0 || req.UserID <= 0 {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}
	if req.Amount <= 0 {
		common.ApiErrorMsg(c, "invalid amount")
		return
	}

	token, err := model.GetTokenById(req.TokenID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorMsg(c, "token not found")
			return
		}
		common.ApiError(c, err)
		return
	}

	if token.UserId != req.UserID {
		common.ApiErrorMsg(c, "token user mismatch")
		return
	}

	beforeRemainQuota := token.RemainQuota
	if err := model.GrantTokenRemainQuota(token.Id, token.Key, req.Amount); err != nil {
		common.ApiError(c, err)
		return
	}
	model.RecordLog(token.UserId, model.LogTypeTopup, formatTokenQuotaGrantLog(token, req.Amount, req.Note))

	token, err = model.GetTokenById(req.TokenID)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	if err := adminPopulateTokenFields(token); err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, GrantTokenQuotaResponse{
		TokenID:           token.Id,
		UserID:            token.UserId,
		BeforeRemainQuota: beforeRemainQuota,
		AfterRemainQuota:  beforeRemainQuota + req.Amount,
		Group:             token.Group,
		Status:            token.Status,
		ExpiredTime:       token.ExpiredTime,
		UnlimitedQuota:    token.UnlimitedQuota,
	})
}
