package model

import (
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// The historical type:int tag is GORM's abstract integer type, not an SQL
// int32 declaration. Verify each actual dialect's generated storage contract;
// changing billing's MaxQuota must not silently shrink cumulative balances.
func TestSaaSCreditQuotaStorageUses64BitIntegers(t *testing.T) {
	for _, item := range []struct {
		value  any
		fields []string
	}{
		{&User{}, []string{"Quota", "UsedQuota"}},
		{&Token{}, []string{"RemainQuota", "UsedQuota", "SaaSCreditedQuota", "SaaSQuotaRevision"}},
		{&SaaSCreditOperation{}, []string{"Amount", "BeforeUserQuota", "AfterUserQuota", "BeforeRemainQuota", "AfterRemainQuota", "AfterTokenCreditTotal"}},
	} {
		parsed, err := schema.Parse(item.value, &sync.Map{}, schema.NamingStrategy{})
		require.NoError(t, err)
		for _, dialect := range []gorm.Dialector{mysql.Dialector{}, postgres.Dialector{}, sqlite.Dialector{}} {
			for _, name := range item.fields {
				field := parsed.LookUpField(name)
				require.NotNil(t, field)
				assert.Equal(t, 64, field.Size, parsed.Name+"."+name)
				expected := "bigint"
				if dialect.Name() == "sqlite" {
					expected = "integer"
				}
				assert.Equal(t, expected, dialect.DataTypeOf(field), dialect.Name()+" "+parsed.Name+"."+name)
			}
		}
	}
}
