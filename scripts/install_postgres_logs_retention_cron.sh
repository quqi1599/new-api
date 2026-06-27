#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${LOG_DATABASE_URL:-}" ]]; then
  echo "LOG_DATABASE_URL is required" >&2
  exit 1
fi
if [[ -z "${CRON_DATABASE_URL:-}" ]]; then
  echo "CRON_DATABASE_URL is required; use the same Aiven service defaultdb as avnadmin" >&2
  exit 1
fi

RETENTION_DAYS="${RETENTION_DAYS:-45}"
FUTURE_DAYS="${FUTURE_DAYS:-14}"
DELETE_BATCH_SIZE="${DELETE_BATCH_SIZE:-50000}"
LOG_DATABASE_NAME="${LOG_DATABASE_NAME:-log}"

if ! [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]] || (( RETENTION_DAYS < 1 )); then
  echo "RETENTION_DAYS must be a positive integer" >&2
  exit 1
fi
if ! [[ "$FUTURE_DAYS" =~ ^[0-9]+$ ]] || (( FUTURE_DAYS < 1 )); then
  echo "FUTURE_DAYS must be a positive integer" >&2
  exit 1
fi
if ! [[ "$DELETE_BATCH_SIZE" =~ ^[0-9]+$ ]] || (( DELETE_BATCH_SIZE < 1 )); then
  echo "DELETE_BATCH_SIZE must be a positive integer" >&2
  exit 1
fi
if ! [[ "$LOG_DATABASE_NAME" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
  echo "LOG_DATABASE_NAME must be a safe SQL identifier" >&2
  exit 1
fi
psql "$LOG_DATABASE_URL" -X -v ON_ERROR_STOP=1 <<'SQL'
SET TIME ZONE 'UTC';

CREATE OR REPLACE FUNCTION public.ensure_logs_partitions(future_days integer DEFAULT 14)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  d date := ((now() AT TIME ZONE 'UTC')::date - 1);
  end_day date := ((now() AT TIME ZONE 'UTC')::date + future_days + 1);
  from_epoch bigint;
  to_epoch bigint;
  partition_name text;
  partition_empty boolean;
BEGIN
  WHILE d <= end_day LOOP
    from_epoch := extract(epoch FROM d::timestamptz)::bigint;
    to_epoch := extract(epoch FROM (d + 1)::timestamptz)::bigint;
    partition_name := 'logs_y' || to_char(d, 'YYYYMMDD');
    EXECUTE format(
      'CREATE TABLE IF NOT EXISTS public.%I PARTITION OF public.logs FOR VALUES FROM (%s) TO (%s)',
      partition_name,
      from_epoch,
      to_epoch
    );
    EXECUTE format(
      'SELECT NOT EXISTS (SELECT 1 FROM public.%I LIMIT 1)',
      partition_name
    ) INTO partition_empty;
    IF partition_empty THEN
      EXECUTE format(
        'CREATE INDEX IF NOT EXISTS %I ON public.%I (channel_id, created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2',
        partition_name || '_channel_consume_created_cover_idx',
        partition_name
      );
    END IF;
    d := d + 1;
  END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION public.prune_logs_retention(retention_days integer DEFAULT 45, delete_batch_size integer DEFAULT 50000)
RETURNS TABLE(dropped_partitions integer, deleted_rows integer)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  cutoff_epoch bigint := extract(epoch FROM (now() - make_interval(days => retention_days)))::bigint;
  drop_before_day date := (to_timestamp(cutoff_epoch) AT TIME ZONE 'UTC')::date;
  partition_row record;
  partition_day date;
  deleted_count integer;
BEGIN
  dropped_partitions := 0;
  deleted_rows := 0;

  FOR partition_row IN
    SELECT c.relname AS partition_name
    FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_class p ON p.oid = i.inhparent
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
      AND p.relname = 'logs'
      AND c.relname ~ '^logs_y[0-9]{8}$'
  LOOP
    partition_day := to_date(substr(partition_row.partition_name, 7, 8), 'YYYYMMDD');
    IF partition_day < drop_before_day THEN
      EXECUTE format('DROP TABLE IF EXISTS public.%I', partition_row.partition_name);
      dropped_partitions := dropped_partitions + 1;
    END IF;
  END LOOP;

  DELETE FROM public.logs
  WHERE id IN (
    SELECT id
    FROM public.logs
    WHERE created_at < cutoff_epoch
    ORDER BY created_at ASC, id ASC
    LIMIT delete_batch_size
  );
  GET DIAGNOSTICS deleted_count = ROW_COUNT;
  deleted_rows := deleted_count;

  RETURN NEXT;
END;
$$;

SELECT public.ensure_logs_partitions(14);
SQL

psql "$CRON_DATABASE_URL" -X -v ON_ERROR_STOP=1 <<SQL
CREATE EXTENSION IF NOT EXISTS pg_cron;

DO \$\$
BEGIN
  IF EXISTS (SELECT 1 FROM cron.job WHERE jobname = 'newapi-log-ensure-partitions') THEN
    PERFORM cron.unschedule('newapi-log-ensure-partitions');
  END IF;
  IF EXISTS (SELECT 1 FROM cron.job WHERE jobname = 'newapi-log-retention-45d') THEN
    PERFORM cron.unschedule('newapi-log-retention-45d');
  END IF;
  IF EXISTS (SELECT 1 FROM cron.job WHERE jobname = 'newapi-log-vacuum-analyze') THEN
    PERFORM cron.unschedule('newapi-log-vacuum-analyze');
  END IF;
END \$\$;

SELECT cron.schedule_in_database(
  'newapi-log-ensure-partitions',
  '7 18 * * *',
  'SELECT public.ensure_logs_partitions($FUTURE_DAYS);',
  '$LOG_DATABASE_NAME'
);

SELECT cron.schedule_in_database(
  'newapi-log-retention-45d',
  '17,47 * * * *',
  'SELECT public.prune_logs_retention($RETENTION_DAYS, $DELETE_BATCH_SIZE);',
  '$LOG_DATABASE_NAME'
);

SELECT cron.schedule_in_database(
  'newapi-log-vacuum-analyze',
  '37 19 * * *',
  'VACUUM (ANALYZE) public.logs;',
  '$LOG_DATABASE_NAME'
);

SELECT jobid, jobname, schedule, database, username, active
FROM cron.job
WHERE jobname LIKE 'newapi-log-%'
ORDER BY jobname;
SQL
