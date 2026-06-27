package model

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

const postgresLogPartitionFutureDays = 14

func ensurePostgresPartitionedLogDB(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("postgres log db is nil")
	}
	if err := ensurePostgresLogTableIsPartitioned(db); err != nil {
		return err
	}
	for _, statement := range postgresPartitionedLogSchemaStatements() {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	if err := ensurePostgresLogPartitions(db, time.Now().UTC(), postgresLogPartitionFutureDays); err != nil {
		return err
	}
	return nil
}

func ensurePostgresLogTableIsPartitioned(db *gorm.DB) error {
	var relKind string
	if err := db.Raw(`
SELECT c.relkind
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname = 'logs'
`).Scan(&relKind).Error; err != nil {
		return err
	}
	if relKind == "" || relKind == "p" {
		return nil
	}
	return fmt.Errorf("public.logs exists but is not a partitioned table; run the log partition migration before starting new-api")
}

func postgresPartitionedLogSchemaStatements() []string {
	return []string{
		`CREATE SEQUENCE IF NOT EXISTS public.logs_id_seq AS bigint`,
		`CREATE TABLE IF NOT EXISTS public.logs (
	id bigint NOT NULL DEFAULT nextval('public.logs_id_seq'::regclass),
	user_id bigint,
	created_at bigint,
	type bigint,
	content text,
	username text DEFAULT ''::text,
	token_name text DEFAULT ''::text,
	model_name text DEFAULT ''::text,
	quota bigint DEFAULT 0,
	prompt_tokens bigint DEFAULT 0,
	completion_tokens bigint DEFAULT 0,
	use_time bigint DEFAULT 0,
	is_stream boolean,
	channel_id bigint,
	channel_name text,
	token_id bigint DEFAULT 0,
	"group" text,
	ip text DEFAULT ''::text,
	request_id varchar(64) DEFAULT ''::character varying,
	other text,
	upstream_request_id varchar(128) DEFAULT ''::character varying
) PARTITION BY RANGE (created_at)`,
		`ALTER SEQUENCE public.logs_id_seq OWNED BY public.logs.id`,
		`CREATE INDEX IF NOT EXISTS idx_logs_created_at_id ON public.logs (created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_user_created_id ON public.logs (user_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_token_created_id ON public.logs (token_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_type_created_id ON public.logs (type, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_channel_created_id ON public.logs (channel_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_group_created_id ON public.logs ("group", created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_consume_created_cover ON public.logs (created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2`,
		`CREATE INDEX IF NOT EXISTS idx_logs_request_id_present ON public.logs (request_id) WHERE request_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_logs_upstream_request_id_present ON public.logs (upstream_request_id) WHERE upstream_request_id <> ''`,
	}
}

func ensurePostgresLogPartitions(db *gorm.DB, now time.Time, futureDays int) error {
	start := postgresLogDayStart(now.AddDate(0, 0, -1))
	for i := 0; i <= futureDays+1; i++ {
		from := start.AddDate(0, 0, i)
		to := from.AddDate(0, 0, 1)
		if err := db.Exec(postgresCreateLogPartitionStatement(from, to)).Error; err != nil {
			return err
		}
	}
	return nil
}

func postgresLogDayStart(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func postgresCreateLogPartitionStatement(from time.Time, to time.Time) string {
	partitionName := postgresLogPartitionName(from)
	return fmt.Sprintf(
		`DO $$
BEGIN
	CREATE TABLE IF NOT EXISTS public.%s PARTITION OF public.logs FOR VALUES FROM (%d) TO (%d);
	IF NOT EXISTS (SELECT 1 FROM public.%s LIMIT 1) THEN
		CREATE INDEX IF NOT EXISTS %s ON public.%s (channel_id, created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2;
	END IF;
END;
$$`,
		partitionName,
		from.Unix(),
		to.Unix(),
		partitionName,
		postgresLogChannelConsumeIndexName(partitionName),
		partitionName,
	)
}

func postgresLogPartitionName(day time.Time) string {
	day = postgresLogDayStart(day)
	return fmt.Sprintf("logs_y%04d%02d%02d", day.Year(), day.Month(), day.Day())
}

func postgresLogChannelConsumeIndexName(partitionName string) string {
	return fmt.Sprintf("%s_channel_consume_created_cover_idx", partitionName)
}
