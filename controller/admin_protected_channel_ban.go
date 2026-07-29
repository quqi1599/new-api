package controller

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func AdminGetProtectedChannelBans(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	keyword := strings.TrimSpace(c.Query("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(c.Query("q"))
	}

	items, total, err := model.SearchAdminProtectedChannelBans(
		keyword,
		pageInfo.GetStartIdx(),
		pageInfo.GetPageSize(),
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(items)
	common.ApiSuccess(c, pageInfo)
}

func AdminDeleteProtectedChannelBan(c *gin.Context) {
	tokenId, err := strconv.Atoi(c.Param("token_id"))
	if err != nil || tokenId <= 0 {
		common.ApiErrorMsg(c, "令牌 id 无效")
		return
	}

	deleted, err := model.DeleteTokenProtectedChannelBan(tokenId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if deleted {
		common.SysLog(fmt.Sprintf("admin #%d removed protected channel ban for token #%d", c.GetInt("id"), tokenId))
	}
	common.ApiSuccess(c, gin.H{"deleted": deleted})
}
