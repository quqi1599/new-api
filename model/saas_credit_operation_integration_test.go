package model

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests require a new, disposable database. They refuse to run when any
// of the business tables already exists, even if a DSN was supplied by mistake.
func TestSaaSCreditOperationExternalDatabases(t *testing.T) {
	for _, dialect := range []string{"mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			env := "TEST_SAAS_CREDIT_MYSQL_DSN"
			if dialect == "postgres" {
				env = "TEST_SAAS_CREDIT_POSTGRES_DSN"
			}
			dsn := os.Getenv(env)
			if dsn == "" {
				t.Skip("set " + env + " to a disposable database")
			}
			var dialector gorm.Dialector = mysql.Open(dsn)
			if dialect == "postgres" {
				dialector = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			for _, target := range []any{&User{}, &Token{}, &SaaSCreditOperation{}, &Log{}} {
				if db.Migrator().HasTable(target) {
					t.Fatal("refusing to run against a database with existing business tables")
				}
			}
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &SaaSCreditOperation{}))
			if dialect == "postgres" {
				require.NoError(t, ensurePostgresPartitionedLogDB(db))
			} else {
				require.NoError(t, db.AutoMigrate(&Log{}))
			}
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&SaaSCreditOperation{}, &Log{}, &Token{}, &User{}))
			})
			oldDB, oldLogDB, oldLogSQLType, oldRedis, oldExcluded := DB, LOG_DB, common.LogSqlType, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs
			DB, LOG_DB, common.LogSqlType, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs = db, db, common.DatabaseType(dialect), false, map[int]struct{}{}
			t.Cleanup(func() {
				DB, LOG_DB, common.LogSqlType, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs = oldDB, oldLogDB, oldLogSQLType, oldRedis, oldExcluded
			})
			user := &User{Id: 1, Username: "saas-credit-test", Status: common.UserStatusEnabled, Quota: 100}
			token := &Token{Id: 1, UserId: 1, Key: "disposable-credit-key", Status: common.TokenStatusEnabled, RemainQuota: 100, UsedQuota: 40}
			require.NoError(t, db.Create(user).Error)
			require.NoError(t, db.Create(token).Error)
			var wg sync.WaitGroup
			start, errs := make(chan struct{}), make(chan error, 10)
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					opID, amount := "repeated-operation", 10
					if i < 2 {
						opID, amount = fmt.Sprintf("independent-%d", i), 50+i*20
					}
					_, err := ApplySaaSCreditOperation(context.Background(), creditRequest(opID, token, amount))
					errs <- err
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}
			assertSaaSCreditBalances(t, 230, 230, 3)
			var logCount int64
			require.NoError(t, db.Model(&Log{}).Where("type = ?", LogTypeTopup).Count(&logCount).Error)
			assert.Equal(t, int64(3), logCount)
			_, err = ApplySaaSCreditOperation(context.Background(), creditRequest("repeated-operation", token, 11))
			require.ErrorIs(t, err, ErrSaaSCreditConflict)
			receipt, err := GetSaaSCreditOperation(context.Background(), "repeated-operation")
			require.NoError(t, err)
			assert.Equal(t, 10, receipt.AfterUserQuota-receipt.BeforeUserQuota)
			// Inspect real SQL metadata, not merely the model tags.
			for _, spec := range []struct {
				value  any
				fields []string
			}{
				{&User{}, []string{"quota", "used_quota"}},
				{&Token{}, []string{"remain_quota", "used_quota", "saas_credited_quota", "saas_quota_revision"}},
				{&SaaSCreditOperation{}, []string{"amount", "before_user_quota", "after_user_quota", "before_remain_quota", "after_remain_quota"}},
			} {
				columns, err := db.Migrator().ColumnTypes(spec.value)
				require.NoError(t, err)
				for _, name := range spec.fields {
					found := false
					for _, column := range columns {
						if column.Name() == name {
							found = true
							assert.Contains(t, []string{"int8", "bigint"}, column.DatabaseTypeName(), name)
						}
					}
					assert.True(t, found, name)
				}
			}
			require.NoError(t, db.Model(&User{}).Where("id = 1").Update("quota", int64(6_000_000_000)).Error)
			require.NoError(t, db.Model(token).Update("remain_quota", int64(7_000_000_000)).Error)
			largeReq := creditRequest("large-db-credit", token, 3_000_000_000)
			largeOp, err := ApplySaaSCreditOperation(context.Background(), largeReq)
			require.NoError(t, err)
			assert.Equal(t, 9_000_000_000, largeOp.AfterUserQuota)
			assert.Equal(t, 10_000_000_000, *largeOp.AfterRemainQuota)
			largeReq.OperationID, largeReq.CreditUserQuota = "large-db-admin-token", false
			largeOp, err = ApplySaaSCreditOperation(context.Background(), largeReq)
			require.NoError(t, err)
			assert.Equal(t, 9_000_000_000, largeOp.AfterUserQuota)
			assert.Equal(t, 13_000_000_000, *largeOp.AfterRemainQuota)
			require.NoError(t, db.Model(&User{}).Where("id = 1").Update("quota", int64(-4_000_000_000)).Error)
			require.NoError(t, db.Model(token).Update("remain_quota", int64(-4_000_000_000)).Error)
			negativeReq := creditRequest("large-db-negative", token, 3_000_000_000)
			negativeOp, err := ApplySaaSCreditOperation(context.Background(), negativeReq)
			require.NoError(t, err)
			assert.Equal(t, -1_000_000_000, negativeOp.AfterUserQuota)
			assert.Equal(t, -1_000_000_000, *negativeOp.AfterRemainQuota)
			// Exercise the revised legacy transaction and explicit edit SQL on the
			// actual external dialect, alongside a concurrent idempotent credit.
			mixedUser := &User{Id: 2, Username: "mixed-credit-test", AffCode: "mixed-credit", Status: common.UserStatusEnabled, Quota: 100}
			mixedToken := &Token{Id: 2, UserId: 2, Key: "disposable-mixed-credit", Status: common.TokenStatusEnabled, RemainQuota: 100, UsedQuota: 40}
			require.NoError(t, db.Create(mixedUser).Error)
			require.NoError(t, db.Create(mixedToken).Error)
			mixedStart, mixedResults := make(chan struct{}), make(chan error, 2)
			go func() { <-mixedStart; mixedResults <- GrantTokenRemainQuota(mixedToken.Id, mixedToken.Key, 50) }()
			go func() {
				<-mixedStart
				_, err := ApplySaaSCreditOperation(context.Background(), creditRequest("mixed-external", mixedToken, 70))
				mixedResults <- err
			}()
			close(mixedStart)
			require.NoError(t, <-mixedResults)
			require.NoError(t, <-mixedResults)
			require.NoError(t, db.First(mixedToken, mixedToken.Id).Error)
			require.NoError(t, db.First(mixedUser, mixedUser.Id).Error)
			assert.Equal(t, 220, mixedToken.RemainQuota)
			assert.Equal(t, int64(120), mixedToken.SaaSCreditedQuota)
			assert.Equal(t, 170, mixedUser.Quota)
			mixedToken.RemainQuota = 300
			require.NoError(t, mixedToken.Update())
			require.NoError(t, db.First(mixedToken, mixedToken.Id).Error)
			assert.Equal(t, 300, mixedToken.RemainQuota)
			assert.Equal(t, int64(1), mixedToken.SaaSQuotaRevision)

		})
	}
}
