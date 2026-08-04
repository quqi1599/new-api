package model

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"
	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSanitizeDBErrorStripsDriverMessage(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   string
		leaked string
	}{
		{name: "mysql", err: &mysql.MySQLError{Number: 1062, Message: "duplicate secret-value"}, want: "mysql error 1062", leaked: "secret-value"},
		{name: "postgres", err: &pgconn.PgError{Code: "23505", Detail: "secret-value"}, want: "postgres error SQLSTATE 23505", leaked: "secret-value"},
		{name: "clickhouse", err: &proto.Exception{Code: 241, Message: "secret-value"}, want: "clickhouse error 241", leaked: "secret-value"},
		{name: "wrapped", err: fmt.Errorf("exec: %w", &mysql.MySQLError{Number: 1064, Message: "secret-value"}), want: "mysql error 1064", leaked: "secret-value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeDBError(tt.err)
			require.EqualError(t, got, tt.want)
			require.NotContains(t, got.Error(), tt.leaked)
		})
	}
}

func TestSanitizeDBErrorSQLiteDriver(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	execErr := db.Exec("INSERT INTO missing_table (k) VALUES (?)", "secret-value").Error
	require.Error(t, execErr)
	got := sanitizeDBError(execErr)
	require.Regexp(t, `^sqlite error \d+$`, got.Error())
	require.NotContains(t, got.Error(), "secret-value")
}

func TestSanitizeDBErrorKeepsNonDriverErrors(t *testing.T) {
	err := fmt.Errorf("dial tcp 127.0.0.1:3306: connect: connection refused")
	require.Same(t, err, sanitizeDBError(err))
}

func TestGormLoggerEndToEndSanitizedOutput(t *testing.T) {
	previousDebug := common.DebugEnabled
	t.Cleanup(func() { common.DebugEnabled = previousDebug })

	execQuery := func() string {
		var buf bytes.Buffer
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: newGormLogger(&buf)})
		require.NoError(t, err)
		db.Exec("SELECT * FROM missing_table WHERE k = ?", "secret-value")
		return buf.String()
	}

	common.DebugEnabled = false
	out := execQuery()
	require.Contains(t, out, "k = ?")
	require.NotContains(t, out, "secret-value")
	require.Contains(t, out, "sqlite error")
	require.Contains(t, out, "gorm_logger_test.go")

	common.DebugEnabled = true
	debugOut := execQuery()
	require.Contains(t, debugOut, "secret-value")
	require.Contains(t, debugOut, "no such table")
}
