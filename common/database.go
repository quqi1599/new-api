package common

type DatabaseType string

const (
	DatabaseTypeMySQL      DatabaseType = "mysql"
	DatabaseTypeSQLite     DatabaseType = "sqlite"
	DatabaseTypePostgreSQL DatabaseType = "postgres"
	DatabaseTypeClickHouse DatabaseType = "clickhouse"
)

var UsingSQLite = false
var UsingPostgreSQL = false
var LogSqlType = DatabaseTypeSQLite // Default to SQLite for logging SQL queries
var UsingMySQL = false
var UsingClickHouse = false

func MainDatabaseType() DatabaseType {
	switch {
	case UsingPostgreSQL:
		return DatabaseTypePostgreSQL
	case UsingMySQL:
		return DatabaseTypeMySQL
	default:
		return DatabaseTypeSQLite
	}
}

func LogDatabaseType() DatabaseType {
	return LogSqlType
}

func SetMainDatabaseType(databaseType DatabaseType) {
	UsingSQLite = databaseType == DatabaseTypeSQLite
	UsingPostgreSQL = databaseType == DatabaseTypePostgreSQL
	UsingMySQL = databaseType == DatabaseTypeMySQL
}

func SetLogDatabaseType(databaseType DatabaseType) {
	LogSqlType = databaseType
	UsingClickHouse = databaseType == DatabaseTypeClickHouse
}

func SetDatabaseTypes(mainType DatabaseType, logType DatabaseType) {
	SetMainDatabaseType(mainType)
	SetLogDatabaseType(logType)
}

func UsingMainDatabase(databaseType DatabaseType) bool {
	return MainDatabaseType() == databaseType
}

func UsingLogDatabase(databaseType DatabaseType) bool {
	return LogDatabaseType() == databaseType
}

var SQLitePath = "one-api.db?_busy_timeout=30000"
