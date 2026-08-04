package common

import "sync/atomic"

const (
	UserAuthCacheReadHit        = "hit"
	UserAuthCacheReadMiss       = "miss"
	UserAuthCacheReadPartial    = "partial"
	UserAuthCacheReadStale      = "stale"
	UserAuthCacheReadRedisError = "redis_error"

	UserAuthDBFallbackDisabledRecheck = "disabled_recheck"

	UserCacheMutationApplied = "applied"
	UserCacheMutationSkipped = "skipped"
	UserCacheMutationError   = "error"
)

const (
	userAuthCacheReadHitIndex = iota
	userAuthCacheReadMissIndex
	userAuthCacheReadPartialIndex
	userAuthCacheReadStaleIndex
	userAuthCacheReadRedisErrorIndex
	userAuthCacheReadResultCount
)

const (
	userAuthDBFallbackMissIndex = iota
	userAuthDBFallbackPartialIndex
	userAuthDBFallbackStaleIndex
	userAuthDBFallbackRedisErrorIndex
	userAuthDBFallbackDisabledRecheckIndex
	userAuthDBFallbackReasonCount
)

const (
	userCacheMutationAppliedIndex = iota
	userCacheMutationSkippedIndex
	userCacheMutationErrorIndex
	userCacheMutationResultCount
)

// UserAuthCacheReadStats is a bounded snapshot of authentication cache reads.
// Fixed fields avoid dynamic labels and keep collection allocation-free.
type UserAuthCacheReadStats struct {
	Hit        int64 `json:"hit"`
	Miss       int64 `json:"miss"`
	Partial    int64 `json:"partial"`
	Stale      int64 `json:"stale"`
	RedisError int64 `json:"redis_error"`
}

// UserAuthDBFallbackStats is a bounded snapshot of authentication DB
// fallbacks, grouped by the cache outcome that required the fallback.
type UserAuthDBFallbackStats struct {
	Miss            int64 `json:"miss"`
	Partial         int64 `json:"partial"`
	Stale           int64 `json:"stale"`
	RedisError      int64 `json:"redis_error"`
	DisabledRecheck int64 `json:"disabled_recheck"`
}

// UserCacheMutationStats is a bounded snapshot of user-cache mutations.
type UserCacheMutationStats struct {
	Applied int64 `json:"applied"`
	Skipped int64 `json:"skipped"`
	Error   int64 `json:"error"`
}

// UserCacheStats exposes only aggregate cache outcomes. It intentionally does
// not contain user identifiers, cached user data, or credential material.
type UserCacheStats struct {
	AuthCacheReadTotal         UserAuthCacheReadStats  `json:"auth_cache_read_total"`
	AuthDBFallbackTotal        UserAuthDBFallbackStats `json:"auth_db_fallback_total"`
	AuthDBFallbackFailureTotal UserAuthDBFallbackStats `json:"auth_db_fallback_failure_total"`
	CacheMutationTotal         UserCacheMutationStats  `json:"cache_mutation_total"`
}

var userCacheMetrics struct {
	authCacheReads         [userAuthCacheReadResultCount]atomic.Int64
	authDBFallbacks        [userAuthDBFallbackReasonCount]atomic.Int64
	authDBFallbackFailures [userAuthDBFallbackReasonCount]atomic.Int64
	cacheMutations         [userCacheMutationResultCount]atomic.Int64
}

// RecordUserAuthCacheRead records one bounded authentication-cache outcome.
// Unknown results are ignored to prevent accidental high-cardinality metrics.
func RecordUserAuthCacheRead(result string) {
	if index, ok := userAuthCacheReadIndex(result); ok {
		userCacheMetrics.authCacheReads[index].Add(1)
	}
}

// RecordUserAuthDBFallback records one authentication DB fallback attempt.
func RecordUserAuthDBFallback(reason string) {
	if index, ok := userAuthDBFallbackIndex(reason); ok {
		userCacheMetrics.authDBFallbacks[index].Add(1)
	}
}

// RecordUserAuthDBFallbackFailure records one failed authentication DB
// fallback, preserving the reason that caused the fallback attempt.
func RecordUserAuthDBFallbackFailure(reason string) {
	if index, ok := userAuthDBFallbackIndex(reason); ok {
		userCacheMetrics.authDBFallbackFailures[index].Add(1)
	}
}

// RecordUserCacheMutation records one bounded cache-mutation outcome.
func RecordUserCacheMutation(result string) {
	if index, ok := userCacheMutationIndex(result); ok {
		userCacheMetrics.cacheMutations[index].Add(1)
	}
}

// SnapshotUserCacheStats returns a consistent-field atomic snapshot. Counters
// can advance independently while the snapshot is being assembled.
func SnapshotUserCacheStats() UserCacheStats {
	return UserCacheStats{
		AuthCacheReadTotal: UserAuthCacheReadStats{
			Hit:        userCacheMetrics.authCacheReads[userAuthCacheReadHitIndex].Load(),
			Miss:       userCacheMetrics.authCacheReads[userAuthCacheReadMissIndex].Load(),
			Partial:    userCacheMetrics.authCacheReads[userAuthCacheReadPartialIndex].Load(),
			Stale:      userCacheMetrics.authCacheReads[userAuthCacheReadStaleIndex].Load(),
			RedisError: userCacheMetrics.authCacheReads[userAuthCacheReadRedisErrorIndex].Load(),
		},
		AuthDBFallbackTotal:        snapshotUserAuthDBFallbackStats(userCacheMetrics.authDBFallbacks[:]),
		AuthDBFallbackFailureTotal: snapshotUserAuthDBFallbackStats(userCacheMetrics.authDBFallbackFailures[:]),
		CacheMutationTotal: UserCacheMutationStats{
			Applied: userCacheMetrics.cacheMutations[userCacheMutationAppliedIndex].Load(),
			Skipped: userCacheMetrics.cacheMutations[userCacheMutationSkippedIndex].Load(),
			Error:   userCacheMetrics.cacheMutations[userCacheMutationErrorIndex].Load(),
		},
	}
}

// GetUserCacheStats returns the current aggregate user-cache metrics.
func GetUserCacheStats() UserCacheStats {
	return SnapshotUserCacheStats()
}

// ResetUserCacheStatsForTest clears the process-local counters. Production code
// must keep the counters monotonic and must not call this helper.
func ResetUserCacheStatsForTest() {
	for index := range userCacheMetrics.authCacheReads {
		userCacheMetrics.authCacheReads[index].Store(0)
	}
	for index := range userCacheMetrics.authDBFallbacks {
		userCacheMetrics.authDBFallbacks[index].Store(0)
		userCacheMetrics.authDBFallbackFailures[index].Store(0)
	}
	for index := range userCacheMetrics.cacheMutations {
		userCacheMetrics.cacheMutations[index].Store(0)
	}
}

func userAuthCacheReadIndex(result string) (int, bool) {
	switch result {
	case UserAuthCacheReadHit:
		return userAuthCacheReadHitIndex, true
	case UserAuthCacheReadMiss:
		return userAuthCacheReadMissIndex, true
	case UserAuthCacheReadPartial:
		return userAuthCacheReadPartialIndex, true
	case UserAuthCacheReadStale:
		return userAuthCacheReadStaleIndex, true
	case UserAuthCacheReadRedisError:
		return userAuthCacheReadRedisErrorIndex, true
	default:
		return 0, false
	}
}

func userAuthDBFallbackIndex(reason string) (int, bool) {
	switch reason {
	case UserAuthCacheReadMiss:
		return userAuthDBFallbackMissIndex, true
	case UserAuthCacheReadPartial:
		return userAuthDBFallbackPartialIndex, true
	case UserAuthCacheReadStale:
		return userAuthDBFallbackStaleIndex, true
	case UserAuthCacheReadRedisError:
		return userAuthDBFallbackRedisErrorIndex, true
	case UserAuthDBFallbackDisabledRecheck:
		return userAuthDBFallbackDisabledRecheckIndex, true
	default:
		return 0, false
	}
}

func userCacheMutationIndex(result string) (int, bool) {
	switch result {
	case UserCacheMutationApplied:
		return userCacheMutationAppliedIndex, true
	case UserCacheMutationSkipped:
		return userCacheMutationSkippedIndex, true
	case UserCacheMutationError:
		return userCacheMutationErrorIndex, true
	default:
		return 0, false
	}
}

func snapshotUserAuthDBFallbackStats(counters []atomic.Int64) UserAuthDBFallbackStats {
	return UserAuthDBFallbackStats{
		Miss:            counters[userAuthDBFallbackMissIndex].Load(),
		Partial:         counters[userAuthDBFallbackPartialIndex].Load(),
		Stale:           counters[userAuthDBFallbackStaleIndex].Load(),
		RedisError:      counters[userAuthDBFallbackRedisErrorIndex].Load(),
		DisabledRecheck: counters[userAuthDBFallbackDisabledRecheckIndex].Load(),
	}
}
