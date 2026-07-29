package model

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func trackTokenChannelExclusionQueries(t *testing.T) (*atomic.Int64, *atomic.Bool, error) {
	t.Helper()
	var queryCount atomic.Int64
	var failNext atomic.Bool
	loadErr := errors.New("injected token channel exclusion load error")
	callbackName := "test:token_channel_exclusion:" + t.Name()
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "token_channel_exclusions" {
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

func TestTokenChannelExclusionIsPersistentIdempotentAndCached(t *testing.T) {
	require.True(t, DB.Migrator().HasTable(&TokenChannelExclusion{}))
	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	resetTokenChannelExclusionCache()
	t.Cleanup(func() {
		_ = DB.Exec("DELETE FROM token_channel_exclusions").Error
		resetTokenChannelExclusionCache()
	})

	added, err := AddTokenChannelExclusion(1001, 131)
	require.NoError(t, err)
	require.True(t, added)

	added, err = AddTokenChannelExclusion(1001, 131)
	require.NoError(t, err)
	require.False(t, added)

	resetTokenChannelExclusionCache()
	channelIds, err := GetTokenChannelExclusionIds(1001)
	require.NoError(t, err)
	require.Equal(t, []int{131}, channelIds)

	channelIds, err = GetTokenChannelExclusionIds(1002)
	require.NoError(t, err)
	require.Empty(t, channelIds)

	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	channelIds, err = GetTokenChannelExclusionIds(1001)
	require.NoError(t, err)
	require.Equal(t, []int{131}, channelIds, "hot path must read memory, not query the database")

	require.NoError(t, DB.Create(&TokenChannelExclusion{TokenId: 1002, ChannelId: 132}).Error)
	channelIds, err = GetTokenChannelExclusionIds(1002)
	require.NoError(t, err)
	require.Empty(t, channelIds, "an empty first load must also be cached")

	added, err = AddTokenChannelExclusion(1002, 133)
	require.NoError(t, err)
	require.True(t, added)
	channelIds, err = GetTokenChannelExclusionIds(1002)
	require.NoError(t, err)
	require.Equal(t, []int{133}, channelIds, "new exclusions must update an already loaded API key")
}

func TestTokenChannelExclusionLoadErrorIsRetryable(t *testing.T) {
	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	require.NoError(t, DB.Create(&TokenChannelExclusion{TokenId: 2001, ChannelId: 131}).Error)
	resetTokenChannelExclusionCache()
	t.Cleanup(func() {
		_ = DB.Exec("DELETE FROM token_channel_exclusions").Error
		resetTokenChannelExclusionCache()
	})

	queryCount, failNext, loadErr := trackTokenChannelExclusionQueries(t)
	failNext.Store(true)

	_, err := GetTokenChannelExclusionIds(2001)
	require.ErrorIs(t, err, loadErr)
	require.Equal(t, int64(1), queryCount.Load())

	channelIds, err := GetTokenChannelExclusionIds(2001)
	require.NoError(t, err)
	require.Equal(t, []int{131}, channelIds)
	require.Equal(t, int64(2), queryCount.Load())

	_, err = GetTokenChannelExclusionIds(2001)
	require.NoError(t, err)
	require.Equal(t, int64(2), queryCount.Load())
}

func TestTokenChannelExclusionConcurrentFirstLoadQueriesOnce(t *testing.T) {
	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	require.NoError(t, DB.Create([]TokenChannelExclusion{
		{TokenId: 3001, ChannelId: 131},
		{TokenId: 3001, ChannelId: 132},
	}).Error)
	resetTokenChannelExclusionCache()
	t.Cleanup(func() {
		_ = DB.Exec("DELETE FROM token_channel_exclusions").Error
		resetTokenChannelExclusionCache()
	})

	queryCount, _, _ := trackTokenChannelExclusionQueries(t)
	type result struct {
		channelIds []int
		err        error
	}
	const concurrency = 32
	start := make(chan struct{})
	results := make(chan result, concurrency)
	for range concurrency {
		go func() {
			<-start
			channelIds, err := GetTokenChannelExclusionIds(3001)
			results <- result{channelIds: channelIds, err: err}
		}()
	}
	close(start)

	for range concurrency {
		got := <-results
		require.NoError(t, got.err)
		require.Equal(t, []int{131, 132}, got.channelIds)
	}
	require.Equal(t, int64(1), queryCount.Load())
}
