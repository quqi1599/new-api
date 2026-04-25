package controller

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func AdminGetAllTokens(c *gin.Context) {
	adminSearchTokens(c)
}

func AdminSearchTokens(c *gin.Context) {
	adminSearchTokens(c)
}

func adminSearchTokens(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	keyword := strings.TrimSpace(c.Query("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(c.Query("q"))
	}
	tokenQuery := strings.TrimSpace(c.Query("token"))
	usernameQuery := strings.TrimSpace(c.Query("username"))

	tokens, total, err := model.SearchAdminTokens(
		keyword,
		tokenQuery,
		usernameQuery,
		pageInfo.GetStartIdx(),
		pageInfo.GetPageSize(),
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(buildMaskedTokenResponses(tokens))
	common.ApiSuccess(c, pageInfo)
}

func AdminGetToken(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}

	token, err := model.GetTokenById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := adminPopulateTokenFields(token); err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, buildMaskedTokenResponse(token))
}

func AdminGetTokenKey(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}

	token, err := model.GetTokenById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, gin.H{
		"key": token.GetFullKey(),
	})
}

func AdminGetTokenKeysBatch(c *gin.Context) {
	tokenBatch := TokenBatch{}
	if err := c.ShouldBindJSON(&tokenBatch); err != nil || len(tokenBatch.Ids) == 0 {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}
	if len(tokenBatch.Ids) > 100 {
		common.ApiErrorMsg(c, "too many ids")
		return
	}

	tokens, err := model.GetTokenKeysByIdsForAdmin(tokenBatch.Ids)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	keysMap := make(map[int]string)
	for _, t := range tokens {
		keysMap[t.Id] = t.GetFullKey()
	}
	common.ApiSuccess(c, gin.H{"keys": keysMap})
}

func AdminUpdateToken(c *gin.Context) {
	statusOnly := c.Query("status_only")
	token := model.Token{}
	err := c.ShouldBindJSON(&token)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if len(token.Name) > 50 {
		common.ApiErrorMsg(c, "令牌名称过长")
		return
	}
	if !token.UnlimitedQuota {
		if token.RemainQuota < 0 {
			common.ApiErrorMsg(c, "令牌额度不能为负数")
			return
		}
		maxQuotaValue := int((1000000000 * common.QuotaPerUnit))
		if token.RemainQuota > maxQuotaValue {
			common.ApiErrorMsg(c, "令牌额度超出上限")
			return
		}
	}
	cleanToken, err := model.GetTokenById(token.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if token.Status == common.TokenStatusEnabled {
		if cleanToken.Status == common.TokenStatusExpired && cleanToken.ExpiredTime <= common.GetTimestamp() && cleanToken.ExpiredTime != -1 {
			common.ApiErrorMsg(c, "令牌已过期，无法启用")
			return
		}
		if cleanToken.Status == common.TokenStatusExhausted && cleanToken.RemainQuota <= 0 && !cleanToken.UnlimitedQuota {
			common.ApiErrorMsg(c, "令牌额度已耗尽，无法启用")
			return
		}
	}
	if statusOnly != "" {
		cleanToken.Status = token.Status
	} else {
		cleanToken.Name = token.Name
		cleanToken.ExpiredTime = token.ExpiredTime
		cleanToken.RemainQuota = token.RemainQuota
		cleanToken.UnlimitedQuota = token.UnlimitedQuota
		cleanToken.ModelLimitsEnabled = token.ModelLimitsEnabled
		cleanToken.ModelLimits = token.ModelLimits
		cleanToken.AllowIps = token.AllowIps
		cleanToken.Group = token.Group
		cleanToken.CrossGroupRetry = token.CrossGroupRetry
	}
	err = cleanToken.Update()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := adminPopulateTokenFields(cleanToken); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, buildMaskedTokenResponse(cleanToken))
}

func AdminDeleteToken(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	token, err := model.GetTokenById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	err = token.Delete()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func AdminDeleteTokenBatch(c *gin.Context) {
	tokenBatch := TokenBatch{}
	if err := c.ShouldBindJSON(&tokenBatch); err != nil || len(tokenBatch.Ids) == 0 {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}
	count, err := model.BatchDeleteTokensByIds(tokenBatch.Ids)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, count)
}
func AdminGrantTokenQuota(c *gin.Context) {
	GrantTokenQuota(c)
}

func adminPopulateTokenFields(token *model.Token) error {
	if token == nil {
		return nil
	}
	userCache, err := model.GetUserCache(token.UserId)
	if err != nil {
		return err
	}
	token.Username = userCache.Username
	if token.Group == "" {
		token.Group = userCache.Group
	}
	return nil
}
