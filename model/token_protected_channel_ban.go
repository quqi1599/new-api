package model

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TokenProtectedChannelBan struct {
	TokenId          int    `json:"token_id" gorm:"primaryKey;autoIncrement:false"`
	TriggerChannelId int    `json:"trigger_channel_id" gorm:"index"`
	ModerationId     string `json:"moderation_id" gorm:"size:128;index"`
	CreatedAt        int64  `json:"created_at" gorm:"autoCreateTime"`
}

type legacyTokenChannelExclusion struct {
	TokenId   int `gorm:"primaryKey;autoIncrement:false"`
	ChannelId int `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt int64
}

func (legacyTokenChannelExclusion) TableName() string {
	return "token_channel_exclusions"
}

type AdminProtectedChannelBan struct {
	TokenId            int    `json:"token_id"`
	TokenName          string `json:"token_name"`
	TokenKey           string `json:"-"`
	MaskedKey          string `json:"masked_key"`
	UserId             int    `json:"user_id"`
	Username           string `json:"username"`
	TriggerChannelId   int    `json:"trigger_channel_id"`
	TriggerChannelName string `json:"trigger_channel_name"`
	ModerationId       string `json:"moderation_id"`
	CreatedAt          int64  `json:"created_at"`
}

var tokenProtectedChannelBanCache sync.Map

type tokenProtectedChannelBanCacheEntry struct {
	mu               sync.Mutex
	loaded           bool
	banned           bool
	persistPending   bool
	triggerChannelId int
	moderationId     string
}

func getTokenProtectedChannelBanCacheEntry(tokenId int) *tokenProtectedChannelBanCacheEntry {
	cached, _ := tokenProtectedChannelBanCache.LoadOrStore(tokenId, &tokenProtectedChannelBanCacheEntry{})
	return cached.(*tokenProtectedChannelBanCacheEntry)
}

// IsTokenProtectedChannelBanned loads one API key on its first request, then serves it from memory.
func IsTokenProtectedChannelBanned(tokenId int) (bool, error) {
	if tokenId <= 0 {
		return false, nil
	}

	entry := getTokenProtectedChannelBanCacheEntry(tokenId)
	entry.mu.Lock()
	if entry.loaded {
		if entry.persistPending {
			result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&TokenProtectedChannelBan{
				TokenId:          tokenId,
				TriggerChannelId: entry.triggerChannelId,
				ModerationId:     entry.moderationId,
			})
			if result.Error != nil {
				entry.mu.Unlock()
				return true, result.Error
			}
			entry.persistPending = false
		}
		banned := entry.banned
		entry.mu.Unlock()
		return banned, nil
	}

	var ban TokenProtectedChannelBan
	err := DB.Select("token_id").Where("token_id = ?", tokenId).Take(&ban).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		entry.mu.Unlock()
		return false, err
	}
	entry.loaded = true
	entry.banned = err == nil
	banned := entry.banned
	entry.mu.Unlock()
	return banned, nil
}

func BanTokenFromProtectedChannels(tokenId int, triggerChannelId int, moderationId string) (bool, error) {
	if tokenId <= 0 || triggerChannelId <= 0 {
		return false, errors.New("令牌或渠道 id 无效")
	}
	moderationId = strings.TrimSpace(moderationId)

	entry := getTokenProtectedChannelBanCacheEntry(tokenId)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&TokenProtectedChannelBan{
		TokenId:          tokenId,
		TriggerChannelId: triggerChannelId,
		ModerationId:     moderationId,
	})
	if result.Error != nil {
		entry.loaded = true
		entry.banned = true
		entry.persistPending = true
		entry.triggerChannelId = triggerChannelId
		entry.moderationId = moderationId
		return false, result.Error
	}
	entry.loaded = true
	entry.banned = true
	entry.persistPending = false
	entry.triggerChannelId = triggerChannelId
	entry.moderationId = moderationId
	return result.RowsAffected > 0, nil
}

func DeleteTokenProtectedChannelBan(tokenId int) (bool, error) {
	if tokenId <= 0 {
		return false, errors.New("令牌 id 无效")
	}

	entry := getTokenProtectedChannelBanCacheEntry(tokenId)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	legacyTableExists := DB.Migrator().HasTable(&legacyTokenChannelExclusion{})
	var rowsAffected int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("token_id = ?", tokenId).Delete(&TokenProtectedChannelBan{})
		if result.Error != nil {
			return result.Error
		}
		rowsAffected += result.RowsAffected
		if legacyTableExists {
			result = tx.Where("token_id = ?", tokenId).Delete(&legacyTokenChannelExclusion{})
			if result.Error != nil {
				return result.Error
			}
			rowsAffected += result.RowsAffected
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	entry.loaded = true
	entry.banned = false
	entry.persistPending = false
	entry.triggerChannelId = 0
	entry.moderationId = ""
	return rowsAffected > 0, nil
}

func SearchAdminProtectedChannelBans(keyword string, offset int, limit int) ([]*AdminProtectedChannelBan, int64, error) {
	ensureTokenSearchCols()
	if limit <= 0 || limit > searchHardLimit {
		limit = searchHardLimit
	}
	if offset < 0 {
		offset = 0
	}

	baseQuery := DB.Table("token_protected_channel_bans AS bans").
		Joins("LEFT JOIN tokens ON tokens.id = bans.token_id AND tokens.deleted_at IS NULL").
		Joins("LEFT JOIN users ON users.id = tokens.user_id AND users.deleted_at IS NULL").
		Joins("LEFT JOIN channels ON channels.id = bans.trigger_channel_id")

	keyword = strings.TrimSpace(keyword)
	if keyword != "" {
		pattern := sanitizeAdminContainsPattern(keyword)
		if id, err := strconv.Atoi(keyword); err == nil {
			baseQuery = baseQuery.Where(
				"(bans.token_id = ? OR bans.trigger_channel_id = ? OR bans.moderation_id LIKE ? ESCAPE '!' OR tokens.name LIKE ? ESCAPE '!' OR users.username LIKE ? ESCAPE '!' OR channels.name LIKE ? ESCAPE '!')",
				id, id, pattern, pattern, pattern, pattern,
			)
		} else {
			baseQuery = baseQuery.Where(
				"(bans.moderation_id LIKE ? ESCAPE '!' OR tokens.name LIKE ? ESCAPE '!' OR users.username LIKE ? ESCAPE '!' OR channels.name LIKE ? ESCAPE '!')",
				pattern, pattern, pattern, pattern,
			)
		}
	}

	var total int64
	if err := baseQuery.Count(&total).Error; err != nil {
		common.SysError("failed to count protected channel bans: " + err.Error())
		return nil, 0, errors.New("查询 API Key 保护名单失败")
	}

	var items []*AdminProtectedChannelBan
	err := baseQuery.
		Select(
			"bans.token_id, bans.trigger_channel_id, bans.moderation_id, bans.created_at, " +
				"tokens.name AS token_name, tokens." + commonKeyCol + " AS token_key, tokens.user_id, " +
				"users.username, channels.name AS trigger_channel_name",
		).
		Order("bans.created_at DESC, bans.token_id DESC").
		Offset(offset).
		Limit(limit).
		Scan(&items).Error
	if err != nil {
		common.SysError("failed to query protected channel bans: " + err.Error())
		return nil, 0, errors.New("查询 API Key 保护名单失败")
	}
	for _, item := range items {
		item.MaskedKey = MaskTokenKey(item.TokenKey)
	}
	return items, total, nil
}

// migrateLegacyTokenChannelExclusions backfills only missing legacy DB rows; the request cache remains lazy.
func migrateLegacyTokenChannelExclusions() error {
	if !DB.Migrator().HasTable(&legacyTokenChannelExclusion{}) {
		return nil
	}

	rows, err := DB.Table("token_channel_exclusions AS legacy").
		Select("legacy.token_id, legacy.channel_id, legacy.created_at").
		Joins("LEFT JOIN token_protected_channel_bans AS bans ON bans.token_id = legacy.token_id").
		Where("bans.token_id IS NULL").
		Order("legacy.token_id ASC, legacy.created_at ASC, legacy.channel_id ASC").
		Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	bans := make([]TokenProtectedChannelBan, 0)
	lastTokenId := 0

	for rows.Next() {
		var legacy legacyTokenChannelExclusion
		if err := rows.Scan(&legacy.TokenId, &legacy.ChannelId, &legacy.CreatedAt); err != nil {
			return err
		}
		if legacy.TokenId == lastTokenId {
			continue
		}
		lastTokenId = legacy.TokenId
		bans = append(bans, TokenProtectedChannelBan{
			TokenId:          legacy.TokenId,
			TriggerChannelId: legacy.ChannelId,
			CreatedAt:        legacy.CreatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(bans) == 0 {
		return nil
	}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&bans, 100).Error; err != nil {
		return err
	}
	common.SysLog("migrated legacy token channel exclusions to global protected channel bans: " + strconv.Itoa(len(bans)))
	return nil
}

func resetTokenProtectedChannelBanCache() {
	tokenProtectedChannelBanCache.Range(func(key, _ any) bool {
		tokenProtectedChannelBanCache.Delete(key)
		return true
	})
}
