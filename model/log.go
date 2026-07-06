package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"

	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var logMigrationOnce sync.Once

type logMigrationStage string

const (
	logMigrationStageIdle      logMigrationStage = "idle"
	logMigrationStageStarted   logMigrationStage = "started"
	logMigrationStageCopied    logMigrationStage = "copied"
	logMigrationStageVerified  logMigrationStage = "verified"
	logMigrationStageCleared   logMigrationStage = "cleared"
	logMigrationStageCompleted logMigrationStage = "completed"
	logMigrationStageFailed    logMigrationStage = "failed"
)

var logMigrationState = struct {
	sync.Mutex
	Stage       logMigrationStage
	SourceCount int64
	MaxID       int
	Migrated    int64
}{
	Stage: logMigrationStageIdle,
}

func applyExplicitLogTextFilter(tx *gorm.DB, column string, value string) (*gorm.DB, error) {
	if value == "" {
		return tx, nil
	}
	if strings.Contains(value, "%") {
		condition, pattern, err := buildLogLikeCondition(column, value)
		if err != nil {
			return nil, err
		}
		return tx.Where(condition, pattern), nil
	}
	return tx.Where(column+" = ?", value), nil
}

func buildLogLikeCondition(column string, value string) (string, string, error) {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		pattern, err := sanitizeClickHouseLikePattern(value)
		if err != nil {
			return "", "", err
		}
		return column + " LIKE ?", pattern, nil
	}

	pattern, err := sanitizeLikePattern(value)
	if err != nil {
		return "", "", err
	}
	return column + " LIKE ? ESCAPE '!'", pattern, nil
}

func sanitizeClickHouseLikePattern(input string) (string, error) {
	input = strings.ReplaceAll(input, `\`, `\\`)
	input = strings.ReplaceAll(input, `_`, `\_`)
	if err := validateLikePattern(input); err != nil {
		return "", err
	}
	return input, nil
}

func ensureLogRequestId(log *Log) {
	if log != nil && log.RequestId == "" {
		log.RequestId = common.NewRequestId()
	}
}

func createLog(log *Log) error {
	ensureLogRequestId(log)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) && log.Id == 0 {
		log.Id = int(time.Now().UnixMicro())
	}
	return LOG_DB.Create(log).Error
}

func clickHouseLogOrder(prefix string) string {
	return prefix + "created_at desc, " + prefix + "request_id desc"
}

func assignDisplayLogIds(logs []*Log, startIdx int) {
	for i := range logs {
		logs[i].Id = startIdx + i + 1
	}
}

type Log struct {
	Id                int    `json:"id" gorm:"index:idx_created_at_id,priority:2;index:idx_user_id_id,priority:2;index:idx_token_created_id,priority:3"`
	UserId            int    `json:"user_id" gorm:"index;index:idx_user_id_id,priority:1"`
	CreatedAt         int64  `json:"created_at" gorm:"bigint;index:idx_created_at_id,priority:1;index:idx_created_at_type;index:idx_token_created_id,priority:2"`
	Type              int    `json:"type" gorm:"index:idx_created_at_type"`
	Content           string `json:"content"`
	Username          string `json:"username" gorm:"index;index:index_username_model_name,priority:2;default:''"`
	TokenName         string `json:"token_name" gorm:"index;default:''"`
	ModelName         string `json:"model_name" gorm:"index;index:index_username_model_name,priority:1;default:''"`
	Quota             int    `json:"quota" gorm:"default:0"`
	PromptTokens      int    `json:"prompt_tokens" gorm:"default:0"`
	CompletionTokens  int    `json:"completion_tokens" gorm:"default:0"`
	UseTime           int    `json:"use_time" gorm:"default:0"`
	IsStream          bool   `json:"is_stream"`
	ChannelId         int    `json:"channel" gorm:"index"`
	ChannelName       string `json:"channel_name" gorm:"->"`
	TokenId           int    `json:"token_id" gorm:"default:0;index;index:idx_token_created_id,priority:1"`
	Group             string `json:"group" gorm:"index"`
	Ip                string `json:"ip" gorm:"index;default:''"`
	RequestId         string `json:"request_id,omitempty" gorm:"type:varchar(64);index:idx_logs_request_id;default:''"`
	UpstreamRequestId string `json:"upstream_request_id,omitempty" gorm:"type:varchar(128);index:idx_logs_upstream_request_id;default:''"`
	Other             string `json:"other"`
}

// don't use iota, avoid change log type value
const (
	LogTypeUnknown = 0
	LogTypeTopup   = 1
	LogTypeConsume = 2
	LogTypeManage  = 3
	LogTypeSystem  = 4
	LogTypeError   = 5
	LogTypeRefund  = 6
)

func formatUserLogs(logs []*Log, startIdx int) {
	for i := range logs {
		logs[i].ChannelName = ""
		var otherMap map[string]interface{}
		otherMap, _ = common.StrToMap(logs[i].Other)
		if otherMap != nil {
			// Remove admin-only debug fields.
			delete(otherMap, "admin_info")
			// delete(otherMap, "reject_reason")
			delete(otherMap, "stream_status")
		}
		logs[i].Other = common.MapToJsonStr(otherMap)
	}
	assignDisplayLogIds(logs, startIdx)
}

func GetLogByTokenId(tokenId int) (logs []*Log, err error) {
	logs, _, err = GetLogByTokenIdPage(tokenId, 0, 0, 0, common.MaxRecentItems)
	return logs, err
}

func GetLogByTokenIdPage(tokenId int, startTimestamp int64, endTimestamp int64, startIdx int, num int) (logs []*Log, total int64, err error) {
	if startIdx < 0 {
		startIdx = 0
	}
	if num <= 0 {
		num = common.ItemsPerPage
	}

	tx := LOG_DB.Model(&Log{}).Where("token_id = ?", tokenId)
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}

	err = tx.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	order := "id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("")
	}
	err = tx.Order(order).Limit(num).Offset(startIdx).Find(&logs).Error
	if err != nil {
		return nil, 0, err
	}
	formatUserLogs(logs, startIdx)
	return logs, total, nil
}

func GetLogByTokenIdCursor(ctx context.Context, tokenId int, startTimestamp int64, endTimestamp int64, beforeId int, num int, startIdx int) (logs []*Log, nextBeforeId int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if num <= 0 {
		num = common.ItemsPerPage
	}
	if startIdx < 0 {
		startIdx = 0
	}

	tx := LOG_DB.WithContext(ctx).Model(&Log{}).Where("token_id = ?", tokenId)
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if beforeId > 0 {
		tx = tx.Where("id < ?", beforeId)
	}

	err = tx.Order("id desc").Limit(num).Find(&logs).Error
	if err != nil {
		return nil, 0, err
	}
	if len(logs) > 0 {
		nextBeforeId = logs[len(logs)-1].Id
	}
	formatUserLogs(logs, startIdx)
	return logs, nextBeforeId, nil
}

func CountLogByTokenIdRange(tokenId int, startTimestamp int64, endTimestamp int64) (total int64, err error) {
	tx := LOG_DB.Model(&Log{}).Where("token_id = ?", tokenId)
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	err = tx.Count(&total).Error
	return total, err
}

func RecordLog(userId int, logType int, content string) {
	if logType == LogTypeConsume && !common.LogConsumeEnabled {
		return
	}
	username, _ := GetUsernameById(userId, false)
	log := &Log{
		UserId:    userId,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      logType,
		Content:   content,
	}
	err := createLog(log)
	if err != nil {
		common.SysLog("failed to record log: " + err.Error())
	}
}

// RecordLogWithAdminInfo 记录操作日志，并将管理员相关信息存入 Other.admin_info，
func RecordLogWithAdminInfo(userId int, logType int, content string, adminInfo map[string]interface{}) {
	if logType == LogTypeConsume && !common.LogConsumeEnabled {
		return
	}
	username, _ := GetUsernameById(userId, false)
	log := &Log{
		UserId:    userId,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      logType,
		Content:   content,
	}
	if len(adminInfo) > 0 {
		other := map[string]interface{}{
			"admin_info": adminInfo,
		}
		log.Other = common.MapToJsonStr(other)
	}
	if err := createLog(log); err != nil {
		common.SysLog("failed to record log: " + err.Error())
	}
}

func RecordTopupLog(userId int, content string, callerIp string, paymentMethod string, callbackPaymentMethod string) {
	username, _ := GetUsernameById(userId, false)
	adminInfo := map[string]interface{}{
		"server_ip":               common.GetIp(),
		"node_name":               common.NodeName,
		"caller_ip":               callerIp,
		"payment_method":          paymentMethod,
		"callback_payment_method": callbackPaymentMethod,
		"version":                 common.Version,
	}
	other := map[string]interface{}{
		"admin_info": adminInfo,
	}
	log := &Log{
		UserId:    userId,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      LogTypeTopup,
		Content:   content,
		Ip:        callerIp,
		Other:     common.MapToJsonStr(other),
	}
	err := createLog(log)
	if err != nil {
		common.SysLog("failed to record topup log: " + err.Error())
	}
}

func RecordErrorLog(c *gin.Context, userId int, channelId int, modelName string, tokenName string, content string, tokenId int, useTimeSeconds int,
	isStream bool, group string, other map[string]interface{}) {
	logger.LogInfo(c, fmt.Sprintf("record error log: userId=%d, channelId=%d, modelName=%s, tokenName=%s, content=%s", userId, channelId, modelName, tokenName, common.LocalLogPreview(content)))
	username := c.GetString("username")
	requestId := c.GetString(common.RequestIdKey)
	upstreamRequestId := c.GetString(common.UpstreamRequestIdKey)
	otherStr := common.MapToJsonStr(other)
	// 判断是否需要记录 IP
	needRecordIp := false
	if settingMap, err := GetUserSetting(userId, false); err == nil {
		if settingMap.RecordIpLog {
			needRecordIp = true
		}
	}
	log := &Log{
		UserId:           userId,
		Username:         username,
		CreatedAt:        common.GetTimestamp(),
		Type:             LogTypeError,
		Content:          content,
		PromptTokens:     0,
		CompletionTokens: 0,
		TokenName:        tokenName,
		ModelName:        modelName,
		Quota:            0,
		ChannelId:        channelId,
		TokenId:          tokenId,
		UseTime:          useTimeSeconds,
		IsStream:         isStream,
		Group:            group,
		Ip: func() string {
			if needRecordIp {
				return c.ClientIP()
			}
			return ""
		}(),
		RequestId:         requestId,
		UpstreamRequestId: upstreamRequestId,
		Other:             otherStr,
	}
	err := createLog(log)
	if err != nil {
		logger.LogError(c, "failed to record log: "+err.Error())
	}
}

type RecordConsumeLogParams struct {
	ChannelId        int                    `json:"channel_id"`
	PromptTokens     int                    `json:"prompt_tokens"`
	CompletionTokens int                    `json:"completion_tokens"`
	ModelName        string                 `json:"model_name"`
	TokenName        string                 `json:"token_name"`
	Quota            int                    `json:"quota"`
	Content          string                 `json:"content"`
	TokenId          int                    `json:"token_id"`
	UseTimeSeconds   int                    `json:"use_time_seconds"`
	IsStream         bool                   `json:"is_stream"`
	Group            string                 `json:"group"`
	Other            map[string]interface{} `json:"other"`
}

func RecordConsumeLog(c *gin.Context, userId int, params RecordConsumeLogParams) {
	if !common.LogConsumeEnabled {
		return
	}
	logger.LogInfo(c, fmt.Sprintf("record consume log: userId=%d, params=%s", userId, common.GetJsonString(params)))
	username := c.GetString("username")
	requestId := c.GetString(common.RequestIdKey)
	upstreamRequestId := c.GetString(common.UpstreamRequestIdKey)
	otherStr := common.MapToJsonStr(params.Other)
	// 判断是否需要记录 IP
	needRecordIp := false
	if settingMap, err := GetUserSetting(userId, false); err == nil {
		if settingMap.RecordIpLog {
			needRecordIp = true
		}
	}
	log := &Log{
		UserId:           userId,
		Username:         username,
		CreatedAt:        common.GetTimestamp(),
		Type:             LogTypeConsume,
		Content:          params.Content,
		PromptTokens:     params.PromptTokens,
		CompletionTokens: params.CompletionTokens,
		TokenName:        params.TokenName,
		ModelName:        params.ModelName,
		Quota:            params.Quota,
		ChannelId:        params.ChannelId,
		TokenId:          params.TokenId,
		UseTime:          params.UseTimeSeconds,
		IsStream:         params.IsStream,
		Group:            params.Group,
		Ip: func() string {
			if needRecordIp {
				return c.ClientIP()
			}
			return ""
		}(),
		RequestId:         requestId,
		UpstreamRequestId: upstreamRequestId,
		Other:             otherStr,
	}
	err := createLog(log)
	if err != nil {
		logger.LogError(c, "failed to record log: "+err.Error())
	}
	if common.DataExportEnabled {
		gopool.Go(func() {
			LogQuotaData(userId, username, params.ModelName, params.Quota, common.GetTimestamp(), params.PromptTokens+params.CompletionTokens)
		})
	}
}

type RecordTaskBillingLogParams struct {
	UserId    int
	LogType   int
	Content   string
	ChannelId int
	ModelName string
	Quota     int
	TokenId   int
	Group     string
	Other     map[string]interface{}
}

func RecordTaskBillingLog(params RecordTaskBillingLogParams) {
	if params.LogType == LogTypeConsume && !common.LogConsumeEnabled {
		return
	}
	username, _ := GetUsernameById(params.UserId, false)
	tokenName := ""
	if params.TokenId > 0 {
		if token, err := GetTokenById(params.TokenId); err == nil {
			tokenName = token.Name
		}
	}
	log := &Log{
		UserId:    params.UserId,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      params.LogType,
		Content:   params.Content,
		TokenName: tokenName,
		ModelName: params.ModelName,
		Quota:     params.Quota,
		ChannelId: params.ChannelId,
		TokenId:   params.TokenId,
		Group:     params.Group,
		Other:     common.MapToJsonStr(params.Other),
	}
	err := createLog(log)
	if err != nil {
		common.SysLog("failed to record task billing log: " + err.Error())
	}
}

func GetAllLogs(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, startIdx int, num int, channel int, group string, requestId string, upstreamRequestId string) (logs []*Log, total int64, err error) {
	var tx *gorm.DB
	if logType == LogTypeUnknown {
		tx = LOG_DB
	} else {
		tx = LOG_DB.Where("logs.type = ?", logType)
	}

	if tx, err = applyExplicitLogTextFilter(tx, "logs.model_name", modelName); err != nil {
		return nil, 0, err
	}
	if tx, err = applyExplicitLogTextFilter(tx, "logs.username", username); err != nil {
		return nil, 0, err
	}
	if tokenName != "" {
		tx = tx.Where("logs.token_name = ?", tokenName)
	}
	if requestId != "" {
		tx = tx.Where("logs.request_id = ?", requestId)
	}
	if upstreamRequestId != "" {
		tx = tx.Where("logs.upstream_request_id = ?", upstreamRequestId)
	}
	if startTimestamp != 0 {
		tx = tx.Where("logs.created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("logs.created_at <= ?", endTimestamp)
	}
	if channel != 0 {
		tx = tx.Where("logs.channel_id = ?", channel)
	}
	if group != "" {
		tx = tx.Where("logs."+logGroupCol+" = ?", group)
	}
	err = tx.Model(&Log{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	order := "logs.created_at desc, logs.id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("logs.")
	}
	err = tx.Order(order).Limit(num).Offset(startIdx).Find(&logs).Error
	if err != nil {
		return nil, 0, err
	}

	channelIds := types.NewSet[int]()
	for _, log := range logs {
		if log.ChannelId != 0 {
			channelIds.Add(log.ChannelId)
		}
	}

	if channelIds.Len() > 0 {
		var channels []struct {
			Id   int    `gorm:"column:id"`
			Name string `gorm:"column:name"`
		}
		if common.MemoryCacheEnabled {
			// Cache get channel
			for _, channelId := range channelIds.Items() {
				if cacheChannel, err := CacheGetChannel(channelId); err == nil {
					channels = append(channels, struct {
						Id   int    `gorm:"column:id"`
						Name string `gorm:"column:name"`
					}{
						Id:   channelId,
						Name: cacheChannel.Name,
					})
				}
			}
		} else {
			// Bulk query channels from DB
			if err = DB.Table("channels").Select("id, name").Where("id IN ?", channelIds.Items()).Find(&channels).Error; err != nil {
				return logs, total, err
			}
		}
		channelMap := make(map[int]string, len(channels))
		for _, channel := range channels {
			channelMap[channel.Id] = channel.Name
		}
		for i := range logs {
			logs[i].ChannelName = channelMap[logs[i].ChannelId]
		}
	}

	return logs, total, err
}

const logSearchCountLimit = 10000

func GetUserLogs(userId int, logType int, startTimestamp int64, endTimestamp int64, modelName string, tokenName string, startIdx int, num int, group string, requestId string, upstreamRequestId string) (logs []*Log, total int64, err error) {
	var tx *gorm.DB
	if logType == LogTypeUnknown {
		tx = LOG_DB.Where("logs.user_id = ?", userId)
	} else {
		tx = LOG_DB.Where("logs.user_id = ? and logs.type = ?", userId, logType)
	}

	if tx, err = applyExplicitLogTextFilter(tx, "logs.model_name", modelName); err != nil {
		return nil, 0, err
	}
	if tokenName != "" {
		tx = tx.Where("logs.token_name = ?", tokenName)
	}
	if requestId != "" {
		tx = tx.Where("logs.request_id = ?", requestId)
	}
	if upstreamRequestId != "" {
		tx = tx.Where("logs.upstream_request_id = ?", upstreamRequestId)
	}
	if startTimestamp != 0 {
		tx = tx.Where("logs.created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("logs.created_at <= ?", endTimestamp)
	}
	if group != "" {
		tx = tx.Where("logs."+logGroupCol+" = ?", group)
	}
	err = tx.Model(&Log{}).Limit(logSearchCountLimit).Count(&total).Error
	if err != nil {
		common.SysError("failed to count user logs: " + err.Error())
		return nil, 0, errors.New("查询日志失败")
	}
	order := "logs.created_at desc, logs.id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("logs.")
	}
	err = tx.Order(order).Limit(num).Offset(startIdx).Find(&logs).Error
	if err != nil {
		common.SysError("failed to search user logs: " + err.Error())
		return nil, 0, errors.New("查询日志失败")
	}

	formatUserLogs(logs, startIdx)
	return logs, total, err
}

type Stat struct {
	Quota int `json:"quota"`
	Rpm   int `json:"rpm"`
	Tpm   int `json:"tpm"`
}

func SumUsedQuota(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, channel int, group string) (stat Stat, err error) {
	tx := LOG_DB.Table("logs").Select("COALESCE(sum(quota), 0) quota")

	// 为rpm和tpm创建单独的查询
	rpmTpmQuery := LOG_DB.Table("logs").Select("count(*) rpm, COALESCE(sum(prompt_tokens), 0) + COALESCE(sum(completion_tokens), 0) tpm")

	if tx, err = applyExplicitLogTextFilter(tx, "username", username); err != nil {
		return stat, err
	}
	if rpmTpmQuery, err = applyExplicitLogTextFilter(rpmTpmQuery, "username", username); err != nil {
		return stat, err
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
		rpmTpmQuery = rpmTpmQuery.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if tx, err = applyExplicitLogTextFilter(tx, "model_name", modelName); err != nil {
		return stat, err
	}
	if rpmTpmQuery, err = applyExplicitLogTextFilter(rpmTpmQuery, "model_name", modelName); err != nil {
		return stat, err
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
		rpmTpmQuery = rpmTpmQuery.Where("channel_id = ?", channel)
	}
	if group != "" {
		tx = tx.Where(logGroupCol+" = ?", group)
		rpmTpmQuery = rpmTpmQuery.Where(logGroupCol+" = ?", group)
	}

	tx = tx.Where("type = ?", LogTypeConsume)
	rpmTpmQuery = rpmTpmQuery.Where("type = ?", LogTypeConsume)

	// 只统计最近60秒的rpm和tpm
	rpmTpmQuery = rpmTpmQuery.Where("created_at >= ?", time.Now().Add(-60*time.Second).Unix())

	// 执行查询
	if err := tx.Scan(&stat).Error; err != nil {
		common.SysError("failed to query log stat: " + err.Error())
		return stat, errors.New("查询统计数据失败")
	}
	if err := rpmTpmQuery.Scan(&stat).Error; err != nil {
		common.SysError("failed to query rpm/tpm stat: " + err.Error())
		return stat, errors.New("查询统计数据失败")
	}

	return stat, nil
}

func SumUsedToken(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string) (token int) {
	tx := LOG_DB.Table("logs").Select("COALESCE(sum(prompt_tokens), 0) + COALESCE(sum(completion_tokens), 0)")
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	tx.Where("type = ?", LogTypeConsume).Scan(&token)
	return token
}

func DeleteOldLog(ctx context.Context, targetTimestamp int64, limit int, logType int) (int64, error) {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		var total int64
		if err := LOG_DB.WithContext(ctx).Model(&Log{}).Where("created_at < ? AND type = ?", targetTimestamp, logType).Count(&total).Error; err != nil {
			return 0, err
		}
		if total == 0 {
			return 0, nil
		}
		err := LOG_DB.WithContext(ctx).Exec(
			"ALTER TABLE logs DELETE WHERE created_at < ? AND type = ? SETTINGS mutations_sync = 1",
			targetTimestamp,
			logType,
		).Error
		return total, err
	}

	var total int64 = 0

	for {
		if nil != ctx.Err() {
			return total, ctx.Err()
		}

		result := LOG_DB.Where("created_at < ? AND type = ?", targetTimestamp, logType).Limit(limit).Delete(&Log{})
		if nil != result.Error {
			return total, result.Error
		}

		total += result.RowsAffected

		if result.RowsAffected < int64(limit) {
			break
		}
	}

	return total, nil
}

func MigrateOldLogsToLogDBIfNeeded() {
	if !shouldMigrateOldLogsToLogDB() {
		return
	}
	logMigrationOnce.Do(func() {
		gopool.Go(func() {
			if err := migrateOldLogsToLogDB(); err != nil {
				setLogMigrationState(logMigrationStageFailed, 0, 0, 0)
				common.SysError("log migration failed: " + err.Error())
			}
		})
	})
}

func migrateOldLogsToLogDB() error {
	sourceDB, sourceType, cleanup, err := getOldLogMigrationSource()
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	return migrateLogsBetweenDBs(sourceDB, sourceType, LOG_DB, common.LogDatabaseType())
}

func getOldLogMigrationSource() (*gorm.DB, common.DatabaseType, func(), error) {
	if common.OldLogSqlDsn == "" {
		return DB, common.MainDatabaseType(), nil, nil
	}
	if common.OldLogSqlDsn == os.Getenv("LOG_SQL_DSN") {
		return nil, "", nil, fmt.Errorf("old log migration aborted: OLD_LOG_SQL_DSN must not equal LOG_SQL_DSN")
	}
	oldLogDB, oldLogType, err := openLogMigrationSourceDB(common.OldLogSqlDsn)
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() {
		if sqlDB, err := oldLogDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return oldLogDB, oldLogType, cleanup, nil
}

func openLogMigrationSourceDB(dsn string) (*gorm.DB, common.DatabaseType, error) {
	previous, hadPrevious := os.LookupEnv("OLD_LOG_SQL_DSN")
	if err := os.Setenv("OLD_LOG_SQL_DSN", dsn); err != nil {
		return nil, "", err
	}
	defer func() {
		if hadPrevious {
			_ = os.Setenv("OLD_LOG_SQL_DSN", previous)
		} else {
			_ = os.Unsetenv("OLD_LOG_SQL_DSN")
		}
	}()
	return chooseDB("OLD_LOG_SQL_DSN", true)
}

func migrateLogsBetweenDBs(sourceDB *gorm.DB, sourceType common.DatabaseType, targetDB *gorm.DB, targetType common.DatabaseType) error {
	batchSize := getLogMigrationBatchSize()

	var sourceCount int64
	if err := sourceDB.Model(&Log{}).Count(&sourceCount).Error; err != nil {
		return err
	}
	if sourceCount == 0 {
		setLogMigrationState(logMigrationStageCompleted, 0, 0, 0)
		return nil
	}
	var maxLog Log
	if err := sourceDB.Select("id").Order("id desc").First(&maxLog).Error; err != nil {
		return err
	}

	var targetCount int64
	if err := targetDB.Model(&Log{}).Count(&targetCount).Error; err != nil {
		return err
	}
	if targetCount > 0 && !common.AllowLogMigrationToNonEmptyTarget {
		return fmt.Errorf("log migration aborted: target log database is not empty")
	}

	setLogMigrationState(logMigrationStageStarted, sourceCount, maxLog.Id, 0)
	common.SysLog(fmt.Sprintf("starting log migration: %d rows, max_id=%d, batch_size=%d", sourceCount, maxLog.Id, batchSize))

	lastID := 0
	var migrated int64
	lastProgressAt := time.Now()
	lastProgressRows := int64(0)
	for {
		var batch []Log
		if err := sourceDB.Where("id > ? AND id <= ?", lastID, maxLog.Id).Order("id asc").Limit(batchSize).Find(&batch).Error; err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		if err := createMigratedLogBatch(targetDB, targetType, batch, batchSize); err != nil {
			return err
		}
		lastID = batch[len(batch)-1].Id
		migrated += int64(len(batch))
		setLogMigrationState(logMigrationStageStarted, sourceCount, maxLog.Id, migrated)
		if migrated-lastProgressRows >= int64(batchSize) || time.Since(lastProgressAt) >= 10*time.Second {
			common.SysLog(fmt.Sprintf("log migration progress: %d/%d rows", migrated, sourceCount))
			lastProgressAt = time.Now()
			lastProgressRows = migrated
		}
	}
	setLogMigrationState(logMigrationStageCopied, sourceCount, maxLog.Id, migrated)
	common.SysLog(fmt.Sprintf("log migration copy completed: %d/%d rows", migrated, sourceCount))

	if err := syncLogIDSequence(targetDB, targetType); err != nil {
		return err
	}

	if err := verifyMigratedLogs(sourceDB, targetDB, maxLog.Id, batchSize); err != nil {
		return err
	}
	setLogMigrationState(logMigrationStageVerified, sourceCount, maxLog.Id, migrated)

	if err := clearSourceLogs(sourceDB, sourceType); err != nil {
		return err
	}
	setLogMigrationState(logMigrationStageCleared, sourceCount, maxLog.Id, migrated)
	common.SysLog(fmt.Sprintf("log migration source cleanup completed: cleared %d rows", sourceCount))

	var sourceCountAfter int64
	if err := sourceDB.Model(&Log{}).Count(&sourceCountAfter).Error; err != nil {
		return err
	}
	if sourceCountAfter != 0 {
		return fmt.Errorf("log migration cleanup failed: source log table still has %d rows", sourceCountAfter)
	}

	setLogMigrationState(logMigrationStageCompleted, sourceCount, maxLog.Id, migrated)
	common.SysLog(fmt.Sprintf("log migration completed: migrated %d rows", migrated))
	return nil
}

func createMigratedLogBatch(targetDB *gorm.DB, targetType common.DatabaseType, batch []Log, batchSize int) error {
	if targetType == common.DatabaseTypeClickHouse {
		for i := range batch {
			ensureLogRequestId(&batch[i])
		}
		return targetDB.CreateInBatches(batch, batchSize).Error
	}
	return targetDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoNothing: true,
	}).CreateInBatches(batch, batchSize).Error
}

func setLogMigrationState(stage logMigrationStage, sourceCount int64, maxID int, migrated int64) {
	logMigrationState.Lock()
	defer logMigrationState.Unlock()
	logMigrationState.Stage = stage
	logMigrationState.SourceCount = sourceCount
	logMigrationState.MaxID = maxID
	logMigrationState.Migrated = migrated
}

func verifyMigratedLogs(sourceDB *gorm.DB, targetDB *gorm.DB, maxID int, batchSize int) error {
	lastID := 0
	for {
		var ids []int
		if err := sourceDB.Model(&Log{}).
			Where("id > ? AND id <= ?", lastID, maxID).
			Order("id asc").
			Limit(batchSize).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		var targetCount int64
		if err := targetDB.Model(&Log{}).Where("id IN ?", ids).Count(&targetCount).Error; err != nil {
			return err
		}
		if targetCount != int64(len(ids)) {
			return fmt.Errorf("log migration verification failed: expected %d rows for id range %d-%d, got %d", len(ids), ids[0], ids[len(ids)-1], targetCount)
		}
		lastID = ids[len(ids)-1]
	}
}

func shouldMigrateOldLogsToLogDB() bool {
	return common.AutoMigrateOldLogsToLogDB && DB != nil && LOG_DB != nil && (DB != LOG_DB || common.OldLogSqlDsn != "")
}

func getLogMigrationBatchSize() int {
	if common.LogMigrationBatchSize > 0 {
		return common.LogMigrationBatchSize
	}
	return 10000
}

func clearSourceLogs(sourceDB *gorm.DB, sourceType common.DatabaseType) error {
	switch sourceType {
	case common.DatabaseTypePostgreSQL, common.DatabaseTypeMySQL:
		return sourceDB.Exec("TRUNCATE TABLE logs").Error
	default:
		return sourceDB.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&Log{}).Error
	}
}

func syncLogIDSequence(db *gorm.DB, dbType common.DatabaseType) error {
	switch dbType {
	case common.DatabaseTypePostgreSQL:
		return db.Exec("SELECT setval(pg_get_serial_sequence('logs', 'id'), COALESCE((SELECT MAX(id) FROM logs), 1), (SELECT COUNT(*) > 0 FROM logs))").Error
	default:
		return nil
	}
}
