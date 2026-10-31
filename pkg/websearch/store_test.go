package websearch

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestWebHistoryIsCallerBoundAndExpires(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	old := common.RDB
	common.RDB = client
	t.Cleanup(func() { common.RDB = old; client.Close() })
	alice, bob := RedisStore{Scope: "1:10"}, RedisStore{Scope: "2:20"}
	ctx := context.Background()
	record := Record{URL: "https://example.com", Call: Raw(map[string]string{"call_id": "c1"}), Result: Raw(map[string]string{"content": "source"})}
	require.NoError(t, alice.Put(ctx, "ws_na_fixture", record))
	got, err := alice.Get(ctx, "ws_na_fixture")
	require.NoError(t, err)
	require.Equal(t, record, got)
	_, err = bob.Get(ctx, "ws_na_fixture")
	require.Error(t, err)
	server.FastForward(25 * time.Hour)
	_, err = alice.Get(ctx, "ws_na_fixture")
	require.Error(t, err)
}
