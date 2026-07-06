package model

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupLogMigrationTestDB(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()

	sourcePath := filepath.Join(t.TempDir(), "source.db")
	targetPath := filepath.Join(t.TempDir(), "target.db")

	sourceDB, err := gorm.Open(sqlite.Open(sourcePath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open source db: %v", err)
	}
	targetDB, err := gorm.Open(sqlite.Open(targetPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open target db: %v", err)
	}
	if err := sourceDB.AutoMigrate(&Log{}); err != nil {
		t.Fatalf("failed to migrate source db: %v", err)
	}
	if err := targetDB.AutoMigrate(&Log{}); err != nil {
		t.Fatalf("failed to migrate target db: %v", err)
	}

	return sourceDB, targetDB
}

func setupLogMigrationTestState(t *testing.T, sourceDB *gorm.DB, targetDB *gorm.DB) {
	t.Helper()

	sourceSQLDB, err := sourceDB.DB()
	if err != nil {
		t.Fatalf("failed to get source sql db: %v", err)
	}
	targetSQLDB, err := targetDB.DB()
	if err != nil {
		t.Fatalf("failed to get target sql db: %v", err)
	}

	oldDB := DB
	oldLogDB := LOG_DB
	oldAutoMigrate := common.AutoMigrateOldLogsToLogDB
	oldOldLogSqlDsn := common.OldLogSqlDsn
	oldBatchSize := common.LogMigrationBatchSize
	oldUsingSQLite := common.UsingSQLite
	oldUsingPostgreSQL := common.UsingPostgreSQL
	oldUsingMySQL := common.UsingMySQL
	oldLogSQLType := common.LogSqlType
	oldAllowNonEmptyTarget := common.AllowLogMigrationToNonEmptyTarget
	oldStage := logMigrationState.Stage
	oldSourceCount := logMigrationState.SourceCount
	oldMaxID := logMigrationState.MaxID
	oldMigrated := logMigrationState.Migrated
	t.Cleanup(func() {
		DB = oldDB
		LOG_DB = oldLogDB
		common.AutoMigrateOldLogsToLogDB = oldAutoMigrate
		common.OldLogSqlDsn = oldOldLogSqlDsn
		common.LogMigrationBatchSize = oldBatchSize
		common.UsingSQLite = oldUsingSQLite
		common.UsingPostgreSQL = oldUsingPostgreSQL
		common.UsingMySQL = oldUsingMySQL
		common.LogSqlType = oldLogSQLType
		common.AllowLogMigrationToNonEmptyTarget = oldAllowNonEmptyTarget
		setLogMigrationState(oldStage, oldSourceCount, oldMaxID, oldMigrated)
		_ = sourceSQLDB.Close()
		_ = targetSQLDB.Close()
	})

	DB = sourceDB
	LOG_DB = targetDB
	common.AutoMigrateOldLogsToLogDB = true
	common.OldLogSqlDsn = ""
	common.LogMigrationBatchSize = 1
	common.UsingSQLite = true
	common.UsingPostgreSQL = false
	common.UsingMySQL = false
	common.LogSqlType = common.DatabaseTypeSQLite
	common.AllowLogMigrationToNonEmptyTarget = false
	setLogMigrationState(logMigrationStageIdle, 0, 0, 0)
}

func TestMigrateOldLogsToLogDBFromIndependentSource(t *testing.T) {
	unusedMainDB, targetDB := setupLogMigrationTestDB(t)
	sourceDB, _ := setupLogMigrationTestDB(t)
	setupLogMigrationTestState(t, unusedMainDB, targetDB)

	logs := []Log{
		{Id: 3, UserId: 3, CreatedAt: 3, Type: LogTypeConsume, Content: "old-source", Username: "u3"},
		{Id: 8, UserId: 8, CreatedAt: 8, Type: LogTypeTopup, Content: "topup", Username: "u8"},
	}
	if err := sourceDB.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed independent source logs: %v", err)
	}

	if err := migrateLogsBetweenDBs(sourceDB, common.DatabaseTypeSQLite, LOG_DB, common.DatabaseTypeSQLite); err != nil {
		t.Fatalf("failed to migrate independent source logs: %v", err)
	}

	var mainCount int64
	if err := DB.Model(&Log{}).Count(&mainCount).Error; err != nil {
		t.Fatalf("failed to count main logs: %v", err)
	}
	if mainCount != 0 {
		t.Fatalf("expected main db to remain unused, got %d logs", mainCount)
	}

	var sourceCount int64
	if err := sourceDB.Model(&Log{}).Count(&sourceCount).Error; err != nil {
		t.Fatalf("failed to count source logs: %v", err)
	}
	if sourceCount != 0 {
		t.Fatalf("expected independent source logs to be cleared, got %d", sourceCount)
	}

	var targetLogs []Log
	if err := LOG_DB.Order("id asc").Find(&targetLogs).Error; err != nil {
		t.Fatalf("failed to query target logs: %v", err)
	}
	if len(targetLogs) != len(logs) {
		t.Fatalf("expected %d target logs, got %d", len(logs), len(targetLogs))
	}
	if targetLogs[0].Id != 3 || targetLogs[1].Id != 8 {
		t.Fatalf("unexpected target log ids: %+v", targetLogs)
	}
}

func TestMigrateOldLogsToLogDBIfNeeded(t *testing.T) {
	sourceDB, targetDB := setupLogMigrationTestDB(t)
	setupLogMigrationTestState(t, sourceDB, targetDB)

	logs := []Log{
		{Id: 1, UserId: 1, CreatedAt: 1, Type: LogTypeConsume, Content: "a", Username: "u1"},
		{Id: 2, UserId: 2, CreatedAt: 2, Type: LogTypeError, Content: "b", Username: "u2"},
		{Id: 5, UserId: 5, CreatedAt: 5, Type: LogTypeManage, Content: "e", Username: "u5"},
	}
	if err := DB.Create(&logs).Error; err != nil {
		t.Fatalf("failed to seed source logs: %v", err)
	}

	if err := migrateOldLogsToLogDB(); err != nil {
		t.Fatalf("failed to auto migrate logs: %v", err)
	}
	if logMigrationState.Stage != logMigrationStageCompleted {
		t.Fatalf("expected migration stage completed, got %s", logMigrationState.Stage)
	}
	if logMigrationState.SourceCount != int64(len(logs)) || logMigrationState.MaxID != 5 || logMigrationState.Migrated != int64(len(logs)) {
		t.Fatalf("unexpected migration state: %+v", logMigrationState)
	}

	var sourceCount int64
	if err := DB.Model(&Log{}).Count(&sourceCount).Error; err != nil {
		t.Fatalf("failed to count source logs: %v", err)
	}
	if sourceCount != 0 {
		t.Fatalf("expected source logs to be cleared, got %d", sourceCount)
	}

	var targetLogs []Log
	if err := LOG_DB.Order("id asc").Find(&targetLogs).Error; err != nil {
		t.Fatalf("failed to query target logs: %v", err)
	}
	if len(targetLogs) != len(logs) {
		t.Fatalf("expected %d target logs, got %d", len(logs), len(targetLogs))
	}
	for i, targetLog := range targetLogs {
		if targetLog.Id != logs[i].Id {
			t.Fatalf("expected target log id %d, got %d", logs[i].Id, targetLog.Id)
		}
		if targetLog.Content != logs[i].Content {
			t.Fatalf("expected target log content %q, got %q", logs[i].Content, targetLog.Content)
		}
	}
}

func TestMigrateOldLogsToLogDBRejectsNonEmptyTarget(t *testing.T) {
	sourceDB, targetDB := setupLogMigrationTestDB(t)
	setupLogMigrationTestState(t, sourceDB, targetDB)

	if err := DB.Create(&Log{Id: 1, UserId: 1, CreatedAt: 1, Type: LogTypeConsume, Content: "source"}).Error; err != nil {
		t.Fatalf("failed to seed source log: %v", err)
	}
	if err := LOG_DB.Create(&Log{Id: 99, UserId: 99, CreatedAt: 99, Type: LogTypeError, Content: "target"}).Error; err != nil {
		t.Fatalf("failed to seed target log: %v", err)
	}

	err := migrateOldLogsToLogDB()
	if err == nil || !strings.Contains(err.Error(), "target log database is not empty") {
		t.Fatalf("expected non-empty target error, got %v", err)
	}

	var sourceCount int64
	if err := DB.Model(&Log{}).Count(&sourceCount).Error; err != nil {
		t.Fatalf("failed to count source logs: %v", err)
	}
	if sourceCount != 1 {
		t.Fatalf("expected source log to remain, got %d", sourceCount)
	}
}

func TestShouldMigrateOldLogsToLogDBAllowsIndependentSourceWhenDBsMatch(t *testing.T) {
	sourceDB, _ := setupLogMigrationTestDB(t)
	setupLogMigrationTestState(t, sourceDB, sourceDB)

	if shouldMigrateOldLogsToLogDB() {
		t.Fatal("expected migration to be disabled when DB and LOG_DB match without independent source")
	}

	common.OldLogSqlDsn = "local"
	if !shouldMigrateOldLogsToLogDB() {
		t.Fatal("expected migration to be enabled when independent source DSN is configured")
	}
}
