package common

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCacheMetricsSnapshot(t *testing.T) {
	ResetUserCacheStatsForTest()
	t.Cleanup(ResetUserCacheStatsForTest)

	RecordUserAuthCacheRead(UserAuthCacheReadHit)
	RecordUserAuthCacheRead(UserAuthCacheReadHit)
	RecordUserAuthCacheRead(UserAuthCacheReadMiss)
	RecordUserAuthCacheRead(UserAuthCacheReadPartial)
	RecordUserAuthCacheRead(UserAuthCacheReadStale)
	RecordUserAuthCacheRead(UserAuthCacheReadRedisError)
	RecordUserAuthCacheRead("unknown")

	for _, reason := range []string{
		UserAuthCacheReadMiss,
		UserAuthCacheReadPartial,
		UserAuthCacheReadStale,
		UserAuthCacheReadRedisError,
		UserAuthDBFallbackDisabledRecheck,
	} {
		RecordUserAuthDBFallback(reason)
		RecordUserAuthDBFallbackFailure(reason)
	}
	RecordUserAuthDBFallback("unknown")
	RecordUserAuthDBFallbackFailure("unknown")

	RecordUserCacheMutation(UserCacheMutationApplied)
	RecordUserCacheMutation(UserCacheMutationSkipped)
	RecordUserCacheMutation(UserCacheMutationError)
	RecordUserCacheMutation("unknown")

	expected := UserCacheStats{
		AuthCacheReadTotal: UserAuthCacheReadStats{
			Hit:        2,
			Miss:       1,
			Partial:    1,
			Stale:      1,
			RedisError: 1,
		},
		AuthDBFallbackTotal: UserAuthDBFallbackStats{
			Miss:            1,
			Partial:         1,
			Stale:           1,
			RedisError:      1,
			DisabledRecheck: 1,
		},
		AuthDBFallbackFailureTotal: UserAuthDBFallbackStats{
			Miss:            1,
			Partial:         1,
			Stale:           1,
			RedisError:      1,
			DisabledRecheck: 1,
		},
		CacheMutationTotal: UserCacheMutationStats{
			Applied: 1,
			Skipped: 1,
			Error:   1,
		},
	}

	require.Equal(t, expected, SnapshotUserCacheStats())
	require.Equal(t, expected, GetUserCacheStats())
}

func TestUserCacheMetricsConcurrentRecording(t *testing.T) {
	ResetUserCacheStatsForTest()
	t.Cleanup(ResetUserCacheStatsForTest)

	const (
		workerCount = 16
		iterations  = 1_000
	)

	var waitGroup sync.WaitGroup
	waitGroup.Add(workerCount)
	for range workerCount {
		go func() {
			defer waitGroup.Done()
			for range iterations {
				RecordUserAuthCacheRead(UserAuthCacheReadPartial)
				RecordUserAuthDBFallback(UserAuthCacheReadPartial)
				RecordUserAuthDBFallbackFailure(UserAuthCacheReadPartial)
				RecordUserCacheMutation(UserCacheMutationApplied)
			}
		}()
	}
	waitGroup.Wait()

	expectedCount := int64(workerCount * iterations)
	stats := SnapshotUserCacheStats()
	require.Equal(t, expectedCount, stats.AuthCacheReadTotal.Partial)
	require.Equal(t, expectedCount, stats.AuthDBFallbackTotal.Partial)
	require.Equal(t, expectedCount, stats.AuthDBFallbackFailureTotal.Partial)
	require.Equal(t, expectedCount, stats.CacheMutationTotal.Applied)
}

func TestResetUserCacheStatsForTest(t *testing.T) {
	ResetUserCacheStatsForTest()
	RecordUserAuthCacheRead(UserAuthCacheReadHit)
	RecordUserAuthDBFallback(UserAuthCacheReadMiss)
	RecordUserAuthDBFallbackFailure(UserAuthCacheReadMiss)
	RecordUserCacheMutation(UserCacheMutationError)

	ResetUserCacheStatsForTest()
	t.Cleanup(ResetUserCacheStatsForTest)

	require.Equal(t, UserCacheStats{}, SnapshotUserCacheStats())
}
