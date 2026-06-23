package controller

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

const tokenLogMaxPageSize = 1000
const tokenLogExportBatchSize = 1000

func GetAllLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	username := c.Query("username")
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	requestId := c.Query("request_id")
	logs, total, err := model.GetAllLogs(logType, startTimestamp, endTimestamp, modelName, username, tokenName, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), channel, group, requestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

func GetUserLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	userId := c.GetInt("id")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	group := c.Query("group")
	requestId := c.Query("request_id")
	logs, total, err := model.GetUserLogs(userId, logType, startTimestamp, endTimestamp, modelName, tokenName, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), group, requestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

// Deprecated: SearchAllLogs 已废弃，前端未使用该接口。
func SearchAllLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

// Deprecated: SearchUserLogs 已废弃，前端未使用该接口。
func SearchUserLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

func GetLogByKey(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if tokenId == 0 {
		c.JSON(200, gin.H{
			"success": false,
			"message": "无效的令牌",
		})
		return
	}

	if hasTokenLogPageQuery(c) {
		pageInfo := getTokenLogPageQuery(c)
		startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
		endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
		logs, total, err := model.GetLogByTokenIdPage(tokenId, startTimestamp, endTimestamp, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
		if err != nil {
			common.ApiError(c, err)
			return
		}
		pageInfo.SetTotal(int(total))
		pageInfo.SetItems(logs)
		common.ApiSuccess(c, pageInfo)
		return
	}

	logs, err := model.GetLogByTokenId(tokenId)
	if err != nil {
		c.JSON(200, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data":    logs,
	})
}

func hasTokenLogPageQuery(c *gin.Context) bool {
	return c.Query("p") != "" ||
		c.Query("page_size") != "" ||
		c.Query("ps") != "" ||
		c.Query("size") != "" ||
		c.Query("start_timestamp") != "" ||
		c.Query("end_timestamp") != ""
}

func getTokenLogPageQuery(c *gin.Context) *common.PageInfo {
	pageInfo := common.GetPageQuery(c)
	if pageInfo.Page < 1 {
		pageInfo.Page = 1
	}
	requestedPageSize := getTokenLogPageSize(c)
	if requestedPageSize > pageInfo.PageSize {
		pageInfo.PageSize = requestedPageSize
	}
	if pageInfo.PageSize < 1 {
		pageInfo.PageSize = common.ItemsPerPage
	}
	if pageInfo.PageSize > tokenLogMaxPageSize {
		pageInfo.PageSize = tokenLogMaxPageSize
	}
	return pageInfo
}

func getTokenLogPageSize(c *gin.Context) int {
	for _, key := range []string{"page_size", "ps", "size"} {
		if pageSize, err := strconv.Atoi(c.Query(key)); err == nil && pageSize > 0 {
			return pageSize
		}
	}
	return 0
}

func ExportLogByKey(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if tokenId == 0 {
		c.JSON(200, gin.H{
			"success": false,
			"message": "无效的令牌",
		})
		return
	}

	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	logs, nextBeforeId, err := model.GetLogByTokenIdCursor(tokenId, startTimestamp, endTimestamp, 0, tokenLogExportBatchSize, 0)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	filename := fmt.Sprintf("token-logs-%d-%s.csv", tokenId, time.Now().Format("20060102150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Status(http.StatusOK)

	writer := csv.NewWriter(c.Writer)
	if err := writer.Write(tokenLogCsvHeader()); err != nil {
		common.SysError("failed to write token log csv header: " + err.Error())
		return
	}

	exported := 0
	for {
		for _, log := range logs {
			if err := writer.Write(tokenLogCsvRow(log)); err != nil {
				common.SysError("failed to write token log csv row: " + err.Error())
				return
			}
		}
		exported += len(logs)
		writer.Flush()
		if err := writer.Error(); err != nil {
			common.SysError("failed to flush token log csv: " + err.Error())
			return
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		if len(logs) < tokenLogExportBatchSize || nextBeforeId == 0 {
			return
		}
		if err := c.Request.Context().Err(); err != nil {
			return
		}
		logs, nextBeforeId, err = model.GetLogByTokenIdCursor(tokenId, startTimestamp, endTimestamp, nextBeforeId, tokenLogExportBatchSize, exported)
		if err != nil {
			common.SysError("failed to query token log csv rows: " + err.Error())
			return
		}
	}
}

func tokenLogCsvHeader() []string {
	return []string{
		"id",
		"created_at",
		"type",
		"content",
		"username",
		"token_name",
		"model_name",
		"quota",
		"prompt_tokens",
		"completion_tokens",
		"use_time",
		"is_stream",
		"channel",
		"channel_name",
		"group",
		"ip",
		"request_id",
		"other",
	}
}

func tokenLogCsvRow(log *model.Log) []string {
	return []string{
		strconv.Itoa(log.Id),
		strconv.FormatInt(log.CreatedAt, 10),
		strconv.Itoa(log.Type),
		log.Content,
		log.Username,
		log.TokenName,
		log.ModelName,
		strconv.Itoa(log.Quota),
		strconv.Itoa(log.PromptTokens),
		strconv.Itoa(log.CompletionTokens),
		strconv.Itoa(log.UseTime),
		strconv.FormatBool(log.IsStream),
		strconv.Itoa(log.ChannelId),
		log.ChannelName,
		log.Group,
		log.Ip,
		log.RequestId,
		log.Other,
	}
}

func GetLogsStat(c *gin.Context) {
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	username := c.Query("username")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	stat, err := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, "")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": stat.Quota,
			"rpm":   stat.Rpm,
			"tpm":   stat.Tpm,
		},
	})
	return
}

func GetLogsSelfStat(c *gin.Context) {
	username := c.GetString("username")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	quotaNum, err := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, tokenName)
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": quotaNum.Quota,
			"rpm":   quotaNum.Rpm,
			"tpm":   quotaNum.Tpm,
			//"token": tokenNum,
		},
	})
	return
}

func DeleteHistoryLogs(c *gin.Context) {
	targetTimestamp, _ := strconv.ParseInt(c.Query("target_timestamp"), 10, 64)
	if targetTimestamp == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "target timestamp is required",
		})
		return
	}
	count, err := model.DeleteOldLog(c.Request.Context(), targetTimestamp, 100)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    count,
	})
	return
}
