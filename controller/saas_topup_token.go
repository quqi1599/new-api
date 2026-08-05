package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ResolveSaaSTopupTokenRequest struct {
	Key string `json:"key"`
}

type ValidateSaaSTopupTokenTargetRequest struct {
	TokenID int `json:"tokenId"`
	UserID  int `json:"userId"`
}

type ResolveSaaSTopupTokenResponse struct {
	TokenID            int             `json:"tokenId"`
	UserID             int             `json:"userId"`
	Name               string          `json:"name"`
	Group              string          `json:"group"`
	Status             int             `json:"status"`
	ExpiredTime        int64           `json:"expiredTime"`
	UnlimitedQuota     bool            `json:"unlimitedQuota"`
	RemainQuota        int             `json:"remainQuota"`
	UsedQuota          int             `json:"usedQuota"`
	TotalGranted       int             `json:"totalGranted"`
	ModelLimits        map[string]bool `json:"modelLimits"`
	ModelLimitsEnabled bool            `json:"modelLimitsEnabled"`
}

func isSaaSTopupUserExcluded(userID int) bool {
	_, excluded := constant.SaaSTopupExcludedUserIDs[userID]
	return excluded
}

func isSaaSTopupTokenEligible(token *model.Token) (bool, error) {
	if token == nil || isSaaSTopupUserExcluded(token.UserId) || token.Status == common.TokenStatusDisabled {
		return false, nil
	}

	user, err := model.GetUserCache(token.UserId)
	if err != nil {
		return false, err
	}

	return user.Status == common.UserStatusEnabled, nil
}

func respondSaaSTopupTokenNotFound(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
		"success": false,
		"message": "token not found",
	})
}

func ResolveSaaSTopupToken(c *gin.Context) {
	var req ResolveSaaSTopupTokenRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}

	key := strings.TrimSpace(req.Key)
	if !strings.HasPrefix(key, "sk-") {
		common.ApiErrorMsg(c, "invalid key format")
		return
	}
	key = strings.TrimSpace(strings.TrimPrefix(key, "sk-"))
	if key == "" {
		common.ApiErrorMsg(c, "invalid key format")
		return
	}

	token, err := model.GetTokenByKey(key, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondSaaSTopupTokenNotFound(c)
			return
		}
		common.ApiError(c, err)
		return
	}
	eligible, err := isSaaSTopupTokenEligible(token)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !eligible {
		respondSaaSTopupTokenNotFound(c)
		return
	}
	if err := adminPopulateTokenFields(token); err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, ResolveSaaSTopupTokenResponse{
		TokenID:            token.Id,
		UserID:             token.UserId,
		Name:               token.Name,
		Group:              token.Group,
		Status:             token.Status,
		ExpiredTime:        token.ExpiredTime,
		UnlimitedQuota:     token.UnlimitedQuota,
		RemainQuota:        token.RemainQuota,
		UsedQuota:          token.UsedQuota,
		TotalGranted:       token.RemainQuota + token.UsedQuota,
		ModelLimits:        token.GetModelLimitsMap(),
		ModelLimitsEnabled: token.ModelLimitsEnabled,
	})
}

func ValidateSaaSTopupTokenTarget(c *gin.Context) {
	var req ValidateSaaSTopupTokenTargetRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.TokenID <= 0 || req.UserID <= 0 {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}

	token, err := model.GetTokenById(req.TokenID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondSaaSTopupTokenNotFound(c)
			return
		}
		common.ApiError(c, err)
		return
	}
	if token.UserId != req.UserID {
		respondSaaSTopupTokenNotFound(c)
		return
	}
	eligible, err := isSaaSTopupTokenEligible(token)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !eligible {
		respondSaaSTopupTokenNotFound(c)
		return
	}

	common.ApiSuccess(c, gin.H{
		"tokenId": token.Id,
		"userId":  token.UserId,
	})
}
