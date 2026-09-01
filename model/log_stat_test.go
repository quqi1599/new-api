package model

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSumUsedQuotaPreservesQuotaWhenLoadingRateStats(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))

	previousLogDB := LOG_DB
	LOG_DB = db
	t.Cleanup(func() { LOG_DB = previousLogDB })

	now := time.Now().Unix()
	require.NoError(t, db.Create(&Log{
		CreatedAt:        now,
		Type:             LogTypeConsume,
		Quota:            12345,
		PromptTokens:     20,
		CompletionTokens: 10,
	}).Error)

	stat, err := SumUsedQuota(LogTypeConsume, now-10, now+10, "", "", "", 0, "")
	require.NoError(t, err)
	require.Equal(t, 12345, stat.Quota)
	require.Equal(t, 1, stat.Rpm)
	require.Equal(t, 30, stat.Tpm)
}
