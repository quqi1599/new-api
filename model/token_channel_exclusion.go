package model

import (
	"errors"
	"slices"
	"sync"

	"gorm.io/gorm/clause"
)

type TokenChannelExclusion struct {
	TokenId   int   `json:"token_id" gorm:"primaryKey;autoIncrement:false"`
	ChannelId int   `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
	CreatedAt int64 `json:"created_at" gorm:"autoCreateTime"`
}

var tokenChannelExclusionCache sync.Map

type tokenChannelExclusionCacheEntry struct {
	once       sync.Once
	mu         sync.RWMutex
	channelIds []int
	err        error
}

func (entry *tokenChannelExclusionCacheEntry) load(tokenId int) {
	entry.err = DB.Model(&TokenChannelExclusion{}).
		Where("token_id = ?", tokenId).
		Order("channel_id").
		Pluck("channel_id", &entry.channelIds).Error
}

// GetTokenChannelExclusionIds loads one API key on its first request, then serves it from memory.
func GetTokenChannelExclusionIds(tokenId int) ([]int, error) {
	if tokenId <= 0 {
		return nil, nil
	}

	cached, _ := tokenChannelExclusionCache.LoadOrStore(tokenId, &tokenChannelExclusionCacheEntry{})
	entry := cached.(*tokenChannelExclusionCacheEntry)
	entry.once.Do(func() {
		entry.load(tokenId)
	})
	if entry.err != nil {
		tokenChannelExclusionCache.CompareAndDelete(tokenId, entry)
		return nil, entry.err
	}

	entry.mu.RLock()
	channelIds := slices.Clone(entry.channelIds)
	entry.mu.RUnlock()
	return channelIds, nil
}

func AddTokenChannelExclusion(tokenId int, channelId int) (bool, error) {
	if tokenId <= 0 || channelId <= 0 {
		return false, errors.New("令牌或渠道 id 无效")
	}

	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&TokenChannelExclusion{
		TokenId:   tokenId,
		ChannelId: channelId,
	})
	if result.Error != nil {
		return false, result.Error
	}

	if cached, ok := tokenChannelExclusionCache.Load(tokenId); ok {
		entry := cached.(*tokenChannelExclusionCacheEntry)
		entry.once.Do(func() {
			entry.load(tokenId)
		})
		if entry.err != nil {
			tokenChannelExclusionCache.CompareAndDelete(tokenId, entry)
			return result.RowsAffected > 0, nil
		}

		entry.mu.Lock()
		if !slices.Contains(entry.channelIds, channelId) {
			entry.channelIds = append(entry.channelIds, channelId)
			slices.Sort(entry.channelIds)
		}
		entry.mu.Unlock()
	}
	return result.RowsAffected > 0, nil
}

func resetTokenChannelExclusionCache() {
	tokenChannelExclusionCache.Range(func(key, _ any) bool {
		tokenChannelExclusionCache.Delete(key)
		return true
	})
}
