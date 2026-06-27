#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${LOG_DATABASE_URL:-}" ]]; then
  echo "LOG_DATABASE_URL is required" >&2
  exit 1
fi

RETENTION_DAYS="${RETENTION_DAYS:-45}"
FUTURE_DAYS="${FUTURE_DAYS:-14}"
ARCHIVE_TABLE="${ARCHIVE_TABLE:-logs_archive_$(date -u +%Y%m%d%H%M%S)}"
CREATE_TRIGRAM_INDEXES="${CREATE_TRIGRAM_INDEXES:-0}"

if ! [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]] || (( RETENTION_DAYS < 1 )); then
  echo "RETENTION_DAYS must be a positive integer" >&2
  exit 1
fi
if ! [[ "$FUTURE_DAYS" =~ ^[0-9]+$ ]] || (( FUTURE_DAYS < 1 )); then
  echo "FUTURE_DAYS must be a positive integer" >&2
  exit 1
fi
if ! [[ "$ARCHIVE_TABLE" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
  echo "ARCHIVE_TABLE must be a safe SQL identifier" >&2
  exit 1
fi
if [[ "$CREATE_TRIGRAM_INDEXES" != "0" && "$CREATE_TRIGRAM_INDEXES" != "1" ]]; then
  echo "CREATE_TRIGRAM_INDEXES must be 0 or 1" >&2
  exit 1
fi

relkind="$(psql "$LOG_DATABASE_URL" -X -A -t -v ON_ERROR_STOP=1 -c "SELECT COALESCE((SELECT c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'logs'), '')")"
if [[ "$relkind" == "p" ]]; then
  echo "public.logs is already partitioned; skipping table rewrite"
  exit 0
fi
if [[ "$relkind" != "r" && "$relkind" != "" ]]; then
  echo "public.logs has unsupported relkind '$relkind'" >&2
  exit 1
fi

psql "$LOG_DATABASE_URL" -X -v ON_ERROR_STOP=1 <<SQL
SET TIME ZONE 'UTC';
SET lock_timeout = '30s';
SET statement_timeout = 0;

DO \$\$
BEGIN
  IF to_regclass('public.logs') IS NULL THEN
    RETURN;
  END IF;
  IF to_regclass('public.$ARCHIVE_TABLE') IS NOT NULL THEN
    RAISE EXCEPTION 'archive table public.% already exists', '$ARCHIVE_TABLE';
  END IF;
END \$\$;

BEGIN;
LOCK TABLE public.logs IN ACCESS EXCLUSIVE MODE;
ALTER TABLE public.logs RENAME TO $ARCHIVE_TABLE;
COMMIT;

DO \$\$
DECLARE
  index_row record;
  new_name text;
BEGIN
  FOR index_row IN
    SELECT c.relname AS index_name
    FROM pg_class c
    JOIN pg_index i ON i.indexrelid = c.oid
    JOIN pg_class t ON t.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
    WHERE n.nspname = 'public' AND t.relname = '$ARCHIVE_TABLE'
  LOOP
    new_name := left('$ARCHIVE_TABLE' || '_' || index_row.index_name, 63);
    EXECUTE format('ALTER INDEX public.%I RENAME TO %I', index_row.index_name, new_name);
  END LOOP;
END \$\$;

CREATE SEQUENCE IF NOT EXISTS public.logs_id_seq AS bigint;

CREATE TABLE public.logs (
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
) PARTITION BY RANGE (created_at);

ALTER SEQUENCE public.logs_id_seq OWNED BY public.logs.id;

DO \$\$
DECLARE
  d date := ((now() AT TIME ZONE 'UTC')::date - $RETENTION_DAYS);
  end_day date := ((now() AT TIME ZONE 'UTC')::date + $FUTURE_DAYS + 1);
  from_epoch bigint;
  to_epoch bigint;
  partition_name text;
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
    d := d + 1;
  END LOOP;
END \$\$;

INSERT INTO public.logs (
  id,
  user_id,
  created_at,
  type,
  content,
  username,
  token_name,
  model_name,
  quota,
  prompt_tokens,
  completion_tokens,
  use_time,
  is_stream,
  channel_id,
  channel_name,
  token_id,
  "group",
  ip,
  request_id,
  other,
  upstream_request_id
)
SELECT
  id,
  user_id,
  created_at,
  type,
  content,
  username,
  token_name,
  model_name,
  quota,
  prompt_tokens,
  completion_tokens,
  use_time,
  is_stream,
  channel_id,
  channel_name,
  token_id,
  "group",
  ip,
  request_id,
  other,
  upstream_request_id
FROM public.$ARCHIVE_TABLE
WHERE created_at >= extract(epoch FROM (now() - make_interval(days => $RETENTION_DAYS)))::bigint
ORDER BY id;

CREATE INDEX idx_logs_created_at_id ON public.logs (created_at DESC, id DESC);
CREATE INDEX idx_logs_user_created_id ON public.logs (user_id, created_at DESC, id DESC);
CREATE INDEX idx_logs_token_created_id ON public.logs (token_id, created_at DESC, id DESC);
CREATE INDEX idx_logs_type_created_id ON public.logs (type, created_at DESC, id DESC);
CREATE INDEX idx_logs_channel_created_id ON public.logs (channel_id, created_at DESC, id DESC);
CREATE INDEX idx_logs_group_created_id ON public.logs ("group", created_at DESC, id DESC);
CREATE INDEX idx_logs_consume_created_cover ON public.logs (created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2;
CREATE INDEX idx_logs_channel_consume_created_cover ON public.logs (channel_id, created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2;
CREATE INDEX idx_logs_request_id_present ON public.logs (request_id) WHERE request_id <> '';
CREATE INDEX idx_logs_upstream_request_id_present ON public.logs (upstream_request_id) WHERE upstream_request_id <> '';

DO \$\$
BEGIN
  IF '$CREATE_TRIGRAM_INDEXES' <> '1' THEN
    RAISE NOTICE 'skipping optional trigram indexes; set CREATE_TRIGRAM_INDEXES=1 to build them';
    RETURN;
  END IF;

  CREATE EXTENSION IF NOT EXISTS pg_trgm;
  CREATE INDEX idx_logs_model_name_trgm ON public.logs USING gin (model_name gin_trgm_ops);
  CREATE INDEX idx_logs_username_trgm ON public.logs USING gin (username gin_trgm_ops);
  CREATE INDEX idx_logs_token_name_trgm ON public.logs USING gin (token_name gin_trgm_ops);
EXCEPTION WHEN others THEN
  RAISE NOTICE 'optional trigram index setup failed: %', SQLERRM;
END \$\$;

DO \$\$
DECLARE
  max_id bigint;
  has_rows boolean;
BEGIN
  EXECUTE format(
    'SELECT max(id), count(*) > 0 FROM (SELECT id FROM public.logs UNION ALL SELECT id FROM public.%I) s',
    '$ARCHIVE_TABLE'
  ) INTO max_id, has_rows;
  IF max_id IS NULL THEN
    max_id := 1;
    has_rows := false;
  END IF;
  PERFORM setval('public.logs_id_seq', max_id, has_rows);
END \$\$;

ANALYZE public.logs;

SELECT '$ARCHIVE_TABLE' AS archived_from_logs,
       pg_size_pretty(pg_total_relation_size('public.logs')) AS new_logs_size,
       (SELECT count(*) FROM public.logs WHERE created_at < extract(epoch FROM (now() - make_interval(days => $RETENTION_DAYS)))::bigint) AS rows_older_than_retention;
SQL
