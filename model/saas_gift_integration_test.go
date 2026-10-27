package model

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSaaSGiftExternalDatabases(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			env := "TEST_SAAS_GIFT_POSTGRES_DSN"
			if dialect == "mysql" {
				env = "TEST_SAAS_GIFT_MYSQL_DSN"
			}
			dsn := os.Getenv(env)
			if dsn == "" {
				t.Skip("set " + env + " to a new disposable database")
			}
			var dialector gorm.Dialector = postgres.Open(dsn)
			if dialect == "mysql" {
				dialector = mysql.Open(dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			tables := []any{&User{}, &Token{}, &Ability{}, &SaaSCreditOperation{}, &SaaSGiftAccount{}, &SaaSGiftOperation{}}
			for _, table := range tables {
				if db.Migrator().HasTable(table) {
					t.Fatal("refusing a database with existing business tables")
				}
			}
			require.NoError(t, db.AutoMigrate(tables...))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(tables...)) })
			oldDB, oldLogs, oldRedis, oldExcluded := DB, LOG_DB, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs
			DB, LOG_DB, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs = db, nil, false, map[int]struct{}{}
			t.Cleanup(func() {
				DB, LOG_DB, common.RedisEnabled, constant.SaaSTopupExcludedUserIDs = oldDB, oldLogs, oldRedis, oldExcluded
			})
			t.Setenv("SAAS_GIFT_GROUP", "双十一")
			oldRatio := ratio_setting.GroupRatio2JSONString()
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"双十一":1}`))
			t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatio)) })
			require.NoError(t, db.Create(&Ability{Group: "双十一", Model: SaaSGiftModel, ChannelId: 1, Enabled: true}).Error)
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := EnsureSaaSGiftAccount(context.Background(), giftEnsure("pg-wallet"))
					errs <- err
				}()
			}
			wg.Wait()
			for i := 0; i < 8; i++ {
				require.NoError(t, <-errs)
			}
			a := createGift(t, "pg-wallet")
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := "grant:pg-repeated"
					if i < 2 {
						id = fmt.Sprintf("grant:pg-%d", i)
					}
					_, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, id, 3000000000))
					errs <- err
				}(i)
			}
			wg.Wait()
			for i := 0; i < 8; i++ {
				require.NoError(t, <-errs)
			}
			u, k := giftBalance(t, a)
			assert.Equal(t, 9000000000, u)
			assert.Equal(t, u, k)
			var n int64
			require.NoError(t, db.Model(&User{}).Count(&n).Error)
			assert.EqualValues(t, 1, n)
			require.NoError(t, db.Model(&Token{}).Count(&n).Error)
			assert.EqualValues(t, 1, n)
			_, err = ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: "cancel:pg-late", AccountID: a.ID, Kind: "cancel_credit", TargetOperationID: "grant:pg-late"})
			require.NoError(t, err)
			late, err := ApplySaaSGiftOperation(context.Background(), giftCredit(a, "grant:pg-late", 100))
			require.NoError(t, err)
			assert.Equal(t, "CANCELED", late.State)
			_, err = ApplySaaSGiftOperation(context.Background(), SaaSGiftOperationRequest{OperationID: "freeze:pg", AccountID: a.ID, Kind: "freeze"})
			require.NoError(t, err)
			assert.Error(t, ReserveUserQuota(*a.UserID, 1))
			require.NoError(t, ReserveUserQuotaForSettlement(*a.UserID, 1))
		})
	}
}
