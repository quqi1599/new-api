package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupCanceledLogQueryTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
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
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestAdminLogQueriesRespectCanceledRequestContext(t *testing.T) {
	setupCanceledLogQueryTestDB(t)

	if _, _, err := GetAllLogsWithContext(canceledContext(), LogTypeUnknown, 0, 0, "", "", "", 0, 10, 0, "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetAllLogsWithContext error = %v, want context canceled", err)
	}
	if _, _, err := GetUserLogsWithContext(canceledContext(), 1, LogTypeUnknown, 0, 0, "", "", 0, 10, "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetUserLogsWithContext error = %v, want context canceled", err)
	}
	if _, err := SumUsedQuotaWithContext(canceledContext(), LogTypeUnknown, 0, 0, "", "", "", 0, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("SumUsedQuotaWithContext error = %v, want context canceled", err)
	}
}
