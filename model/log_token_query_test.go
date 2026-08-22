package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type tokenLogQueryRecorder struct {
	queries []string
}

func (r *tokenLogQueryRecorder) LogMode(logger.LogLevel) logger.Interface      { return r }
func (r *tokenLogQueryRecorder) Info(context.Context, string, ...interface{})  {}
func (r *tokenLogQueryRecorder) Warn(context.Context, string, ...interface{})  {}
func (r *tokenLogQueryRecorder) Error(context.Context, string, ...interface{}) {}
func (r *tokenLogQueryRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	query, _ := fc()
	r.queries = append(r.queries, query)
}

func TestGetLogByTokenIdLegacyQuerySkipsCount(t *testing.T) {
	recorder := &tokenLogQueryRecorder{}
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: recorder},
	)
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&Log{}); err != nil {
		t.Fatalf("failed to migrate log table: %v", err)
	}

	originalLogDB := LOG_DB
	originalLogDatabaseType := common.LogDatabaseType()
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		LOG_DB = originalLogDB
		common.SetLogDatabaseType(originalLogDatabaseType)
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	logs := []Log{
		{UserId: 1, TokenId: 7, CreatedAt: 10, Type: LogTypeConsume, Content: "older"},
		{UserId: 1, TokenId: 8, CreatedAt: 20, Type: LogTypeConsume, Content: "other-token"},
		{UserId: 1, TokenId: 7, CreatedAt: 30, Type: LogTypeConsume, Content: "newer"},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed logs: %v", err)
	}
	recorder.queries = nil

	got, err := GetLogByTokenId(7)
	if err != nil {
		t.Fatalf("failed to get token logs: %v", err)
	}
	if len(got) != 2 || got[0].Content != "newer" || got[1].Content != "older" {
		t.Fatalf("unexpected token logs: %+v", got)
	}
	if len(recorder.queries) != 1 {
		t.Fatalf("legacy token log lookup executed %d queries, want 1: %v", len(recorder.queries), recorder.queries)
	}
	query := strings.ToUpper(recorder.queries[0])
	if strings.Contains(query, "COUNT(") {
		t.Fatalf("legacy token log lookup must not count full history: %s", recorder.queries[0])
	}
	if !strings.Contains(query, "LIMIT 1000") {
		t.Fatalf("legacy token log lookup must stay bounded to 1000 rows: %s", recorder.queries[0])
	}
}
