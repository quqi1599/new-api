package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
	require.Equal(t, []int{131}, GetTokenChannelExclusionIds(1001))
	require.Empty(t, GetTokenChannelExclusionIds(1002))

	resetTokenChannelExclusionCache()
	require.Empty(t, GetTokenChannelExclusionIds(1001))
	require.NoError(t, InitTokenChannelExclusionCache())
	require.Equal(t, []int{131}, GetTokenChannelExclusionIds(1001))

	require.NoError(t, DB.Exec("DELETE FROM token_channel_exclusions").Error)
	require.Equal(t, []int{131}, GetTokenChannelExclusionIds(1001), "hot path must read memory, not query the database")
}
