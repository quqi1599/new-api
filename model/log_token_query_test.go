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

func TestGetLogByTokenIdKeysetUsesCreatedAtAndIdWithoutCountOrOffset(t *testing.T) {
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
		{UserId: 1, TokenId: 7, CreatedAt: 10, Type: LogTypeConsume, Content: "older", RequestId: "r1"},
		{UserId: 1, TokenId: 7, CreatedAt: 20, Type: LogTypeConsume, Content: "middle", RequestId: "r2"},
		{UserId: 1, TokenId: 8, CreatedAt: 30, Type: LogTypeConsume, Content: "other-token", RequestId: "r3"},
		{UserId: 1, TokenId: 7, CreatedAt: 30, Type: LogTypeConsume, Content: "same-time-older", RequestId: "r4"},
		{UserId: 1, TokenId: 7, CreatedAt: 30, Type: LogTypeConsume, Content: "same-time-newer", RequestId: "r5"},
		{UserId: 1, TokenId: 7, CreatedAt: 40, Type: LogTypeConsume, Content: "newest", RequestId: "r6"},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed logs: %v", err)
	}
	recorder.queries = nil

	first, next, hasMore, err := GetLogByTokenIdKeyset(context.Background(), 7, 10, 40, nil, 3, 0)
	if err != nil {
		t.Fatalf("failed to get first keyset page: %v", err)
	}
	if !hasMore || next == nil || next.CreatedAt != 30 || next.Id != logs[3].Id {
		t.Fatalf("unexpected first keyset cursor: has_more=%v cursor=%+v", hasMore, next)
	}
	if len(first) != 3 ||
		first[0].Content != "newest" ||
		first[1].Content != "same-time-newer" ||
		first[2].Content != "same-time-older" {
		t.Fatalf("unexpected first keyset page: %+v", first)
	}
	if len(recorder.queries) != 1 {
		t.Fatalf("keyset query executed %d queries, want 1: %v", len(recorder.queries), recorder.queries)
	}
	query := strings.ToUpper(recorder.queries[0])
	if strings.Contains(query, "COUNT(") || strings.Contains(query, " OFFSET ") {
		t.Fatalf("keyset query must not count or offset: %s", recorder.queries[0])
	}
	if !strings.Contains(query, "ORDER BY CREATED_AT DESC, ID DESC") || !strings.Contains(query, "LIMIT 4") {
		t.Fatalf("keyset query must use created_at/id order and one lookahead row: %s", recorder.queries[0])
	}

	recorder.queries = nil
	second, next, hasMore, err := GetLogByTokenIdKeyset(context.Background(), 7, 10, 40, next, 3, 3)
	if err != nil {
		t.Fatalf("failed to get second keyset page: %v", err)
	}
	if hasMore || next != nil {
		t.Fatalf("unexpected second keyset cursor: has_more=%v cursor=%+v", hasMore, next)
	}
	if len(second) != 2 || second[0].Content != "middle" || second[1].Content != "older" {
		t.Fatalf("unexpected second keyset page: %+v", second)
	}
	query = strings.ToUpper(recorder.queries[0])
	if !strings.Contains(query, "CREATED_AT <") || !strings.Contains(query, "CREATED_AT =") || !strings.Contains(query, "ID <") {
		t.Fatalf("keyset continuation must use created_at/id boundary: %s", recorder.queries[0])
	}
}

func TestGetLogByTokenIdKeysetUsesClickHouseRequestIdTieBreaker(t *testing.T) {
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
		{UserId: 1, TokenId: 7, CreatedAt: 20, Type: LogTypeConsume, Content: "older", RequestId: "r1"},
		{UserId: 1, TokenId: 7, CreatedAt: 30, Type: LogTypeConsume, Content: "same-time-older", RequestId: "r2"},
		{UserId: 1, TokenId: 7, CreatedAt: 30, Type: LogTypeConsume, Content: "same-time-newer", RequestId: "r3"},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed logs: %v", err)
	}
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	recorder.queries = nil

	got, _, _, err := GetLogByTokenIdKeyset(
		context.Background(),
		7,
		20,
		30,
		&TokenLogKeysetCursor{CreatedAt: 30, RequestId: "r3"},
		2,
		1,
	)
	if err != nil {
		t.Fatalf("failed to get clickhouse-style keyset page: %v", err)
	}
	if len(got) != 2 || got[0].Content != "same-time-older" || got[1].Content != "older" {
		t.Fatalf("unexpected clickhouse-style keyset page: %+v", got)
	}
	query := strings.ToUpper(recorder.queries[0])
	if !strings.Contains(query, "REQUEST_ID <") || !strings.Contains(query, "ORDER BY CREATED_AT DESC, REQUEST_ID DESC") {
		t.Fatalf("clickhouse keyset query must use request_id tie breaker: %s", recorder.queries[0])
	}
}
