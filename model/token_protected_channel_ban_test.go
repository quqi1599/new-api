package model

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func trackTokenProtectedChannelBanQueries(t *testing.T) (*atomic.Int64, *atomic.Bool, error) {
	t.Helper()
	var queryCount atomic.Int64
	var failNext atomic.Bool
	loadErr := errors.New("injected token protected channel ban load error")
	callbackName := "test:token_protected_channel_ban:" + t.Name()
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "token_protected_channel_bans" {
			return
		}
		queryCount.Add(1)
		if failNext.CompareAndSwap(true, false) {
			tx.AddError(loadErr)
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Query().Remove(callbackName)
	})
	return &queryCount, &failNext, loadErr
}

func resetProtectedChannelBanTestState(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Exec("DELETE FROM token_protected_channel_bans").Error)
	resetTokenProtectedChannelBanCache()
	t.Cleanup(func() {
		_ = DB.Exec("DELETE FROM token_protected_channel_bans").Error
		resetTokenProtectedChannelBanCache()
	})
}

func TestTokenProtectedChannelBanIsPersistentIdempotentAndCached(t *testing.T) {
	require.True(t, DB.Migrator().HasTable(&TokenProtectedChannelBan{}))
	resetProtectedChannelBanTestState(t)

	added, err := BanTokenFromProtectedChannels(1001, 131)
	require.NoError(t, err)
	require.True(t, added)

	added, err = BanTokenFromProtectedChannels(1001, 132)
	require.NoError(t, err)
	require.False(t, added)

	resetTokenProtectedChannelBanCache()
	banned, err := IsTokenProtectedChannelBanned(1001)
	require.NoError(t, err)
	require.True(t, banned)

	banned, err = IsTokenProtectedChannelBanned(1002)
	require.NoError(t, err)
	require.False(t, banned)

	require.NoError(t, DB.Exec("DELETE FROM token_protected_channel_bans").Error)
	banned, err = IsTokenProtectedChannelBanned(1001)
	require.NoError(t, err)
	require.True(t, banned, "hot path must read memory, not query the database")

	deleted, err := DeleteTokenProtectedChannelBan(1001)
	require.NoError(t, err)
	require.False(t, deleted)
	banned, err = IsTokenProtectedChannelBanned(1001)
	require.NoError(t, err)
	require.False(t, banned, "unban must update the loaded API key immediately")
}

func TestTokenProtectedChannelBanLoadErrorIsRetryable(t *testing.T) {
	resetProtectedChannelBanTestState(t)
	require.NoError(t, DB.Create(&TokenProtectedChannelBan{TokenId: 2001, TriggerChannelId: 131}).Error)

	queryCount, failNext, loadErr := trackTokenProtectedChannelBanQueries(t)
	failNext.Store(true)

	_, err := IsTokenProtectedChannelBanned(2001)
	require.ErrorIs(t, err, loadErr)
	require.Equal(t, int64(1), queryCount.Load())

	banned, err := IsTokenProtectedChannelBanned(2001)
	require.NoError(t, err)
	require.True(t, banned)
	require.Equal(t, int64(2), queryCount.Load())

	_, err = IsTokenProtectedChannelBanned(2001)
	require.NoError(t, err)
	require.Equal(t, int64(2), queryCount.Load())
}

func TestTokenProtectedChannelBanPersistErrorIsRetried(t *testing.T) {
	resetProtectedChannelBanTestState(t)
	persistErr := errors.New("injected token protected channel ban persist error")
	var failNext atomic.Bool
	failNext.Store(true)
	callbackName := "test:token_protected_channel_ban_create:" + t.Name()
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "token_protected_channel_bans" && failNext.CompareAndSwap(true, false) {
			tx.AddError(persistErr)
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Create().Remove(callbackName)
	})

	added, err := BanTokenFromProtectedChannels(2501, 131)
	require.False(t, added)
	require.ErrorIs(t, err, persistErr)

	banned, err := IsTokenProtectedChannelBanned(2501)
	require.NoError(t, err)
	require.True(t, banned)

	resetTokenProtectedChannelBanCache()
	banned, err = IsTokenProtectedChannelBanned(2501)
	require.NoError(t, err)
	require.True(t, banned, "retry must persist the ban before serving from a cold cache")
}

func TestTokenProtectedChannelBanConcurrentFirstLoadQueriesOnce(t *testing.T) {
	resetProtectedChannelBanTestState(t)
	require.NoError(t, DB.Create(&TokenProtectedChannelBan{TokenId: 3001, TriggerChannelId: 131}).Error)

	queryCount, _, _ := trackTokenProtectedChannelBanQueries(t)
	type result struct {
		banned bool
		err    error
	}
	const concurrency = 32
	start := make(chan struct{})
	results := make(chan result, concurrency)
	for range concurrency {
		go func() {
			<-start
			banned, err := IsTokenProtectedChannelBanned(3001)
			results <- result{banned: banned, err: err}
		}()
	}
	close(start)

	for range concurrency {
		got := <-results
		require.NoError(t, got.err)
		require.True(t, got.banned)
	}
	require.Equal(t, int64(1), queryCount.Load())
}

func TestSearchAndDeleteAdminProtectedChannelBan(t *testing.T) {
	resetProtectedChannelBanTestState(t)

	user := &User{Username: "protected-ban-owner"}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{UserId: user.Id, Name: "protected-ban-token", Key: "protected-ban-secret-key"}
	require.NoError(t, DB.Create(token).Error)
	channel := &Channel{Name: "protected-ban-channel", Key: "upstream-key"}
	require.NoError(t, DB.Create(channel).Error)
	t.Cleanup(func() {
		_ = DB.Unscoped().Delete(token).Error
		_ = DB.Unscoped().Delete(user).Error
		_ = DB.Delete(channel).Error
	})

	added, err := BanTokenFromProtectedChannels(token.Id, channel.Id)
	require.NoError(t, err)
	require.True(t, added)

	items, total, err := SearchAdminProtectedChannelBans("protected-ban", 0, 20)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)
	require.Equal(t, token.Id, items[0].TokenId)
	require.Equal(t, token.Name, items[0].TokenName)
	require.Equal(t, MaskTokenKey(token.Key), items[0].MaskedKey)
	require.Equal(t, user.Username, items[0].Username)
	require.Equal(t, channel.Id, items[0].TriggerChannelId)
	require.Equal(t, channel.Name, items[0].TriggerChannelName)

	deleted, err := DeleteTokenProtectedChannelBan(token.Id)
	require.NoError(t, err)
	require.True(t, deleted)
	banned, err := IsTokenProtectedChannelBanned(token.Id)
	require.NoError(t, err)
	require.False(t, banned)
}

func TestMigrateLegacyTokenChannelExclusionsIsIdempotent(t *testing.T) {
	resetProtectedChannelBanTestState(t)
	require.NoError(t, DB.AutoMigrate(&legacyTokenChannelExclusion{}))
	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	t.Cleanup(func() {
		_ = DB.Exec("DELETE FROM token_channel_exclusions").Error
	})

	require.NoError(t, DB.Create([]legacyTokenChannelExclusion{
		{TokenId: 4001, ChannelId: 132, CreatedAt: 20},
		{TokenId: 4001, ChannelId: 131, CreatedAt: 10},
		{TokenId: 4002, ChannelId: 131, CreatedAt: 30},
	}).Error)
	require.NoError(t, DB.Create(&TokenProtectedChannelBan{
		TokenId:          4002,
		TriggerChannelId: 999,
		CreatedAt:        5,
	}).Error)

	require.NoError(t, migrateLegacyTokenChannelExclusions())
	require.NoError(t, migrateLegacyTokenChannelExclusions())

	var bans []TokenProtectedChannelBan
	require.NoError(t, DB.Order("token_id").Find(&bans).Error)
	require.Equal(t, []TokenProtectedChannelBan{
		{TokenId: 4001, TriggerChannelId: 131, CreatedAt: 10},
		{TokenId: 4002, TriggerChannelId: 999, CreatedAt: 5},
	}, bans)

	deleted, err := DeleteTokenProtectedChannelBan(4001)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoError(t, migrateLegacyTokenChannelExclusions())
	require.NoError(t, DB.Order("token_id").Find(&bans).Error)
	require.Equal(t, []TokenProtectedChannelBan{
		{TokenId: 4002, TriggerChannelId: 999, CreatedAt: 5},
	}, bans, "unbanned legacy API keys must not be restored on restart")
}
