package limiter

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestBatch1LimiterScriptRecovery(t *testing.T) {
	srv := miniredis.RunT(t)
	srv.SetTime(time.Unix(1000, 0))
	client := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	sha, err := client.ScriptLoad(ctx, rateLimitScript).Result()
	require.NoError(t, err)
	rl := &RedisLimiter{client: client, limitScriptSHA: sha}
	allow := func(key string) bool {
		t.Helper()
		ok, err := rl.Allow(ctx, key, WithCapacity(1), WithRate(1))
		require.NoError(t, err)
		return ok
	}
	require.True(t, allow("existing"))
	require.False(t, allow("existing"))
	require.NoError(t, client.ScriptFlush(ctx).Err())
	require.False(t, allow("existing"), "script recovery must not reset quota")
	require.True(t, allow("new"))
	exists, err := client.ScriptExists(ctx, sha).Result()
	require.NoError(t, err)
	require.Equal(t, []bool{true}, exists)
	srv.SetError("ERR fixture")
	ok, err := rl.Allow(ctx, "error")
	require.ErrorContains(t, err, "ERR fixture")
	require.False(t, ok)
}
