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

var tokenChannelExclusionCache = struct {
	sync.RWMutex
	byToken map[int][]int
}{
	byToken: make(map[int][]int),
}

// InitTokenChannelExclusionCache loads the durable exclusions once at startup.
// ponytail: full preload fits the current small table; use a bounded cache if it grows beyond active-key scale.
func InitTokenChannelExclusionCache() error {
	var exclusions []TokenChannelExclusion
	if err := DB.Order("token_id, channel_id").Find(&exclusions).Error; err != nil {
		return err
	}

	byToken := make(map[int][]int)
	for _, exclusion := range exclusions {
		byToken[exclusion.TokenId] = append(byToken[exclusion.TokenId], exclusion.ChannelId)
	}

	tokenChannelExclusionCache.Lock()
	tokenChannelExclusionCache.byToken = byToken
	tokenChannelExclusionCache.Unlock()
	return nil
}

func GetTokenChannelExclusionIds(tokenId int) []int {
	if tokenId <= 0 {
		return nil
	}
	tokenChannelExclusionCache.RLock()
	channelIds := slices.Clone(tokenChannelExclusionCache.byToken[tokenId])
	tokenChannelExclusionCache.RUnlock()
	return channelIds
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

	tokenChannelExclusionCache.Lock()
	channelIds := tokenChannelExclusionCache.byToken[tokenId]
	if !slices.Contains(channelIds, channelId) {
		channelIds = append(channelIds, channelId)
		slices.Sort(channelIds)
		tokenChannelExclusionCache.byToken[tokenId] = channelIds
	}
	tokenChannelExclusionCache.Unlock()
	return result.RowsAffected > 0, nil
}

func resetTokenChannelExclusionCache() {
	tokenChannelExclusionCache.Lock()
	tokenChannelExclusionCache.byToken = make(map[int][]int)
	tokenChannelExclusionCache.Unlock()
}
