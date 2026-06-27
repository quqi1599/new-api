#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${LOG_DATABASE_URL:-}" ]]; then
  echo "LOG_DATABASE_URL is required" >&2
  exit 1
fi

LOCK_TIMEOUT="${LOCK_TIMEOUT:-30s}"

psql "$LOG_DATABASE_URL" -X -v ON_ERROR_STOP=1 -v lock_timeout="$LOCK_TIMEOUT" <<'SQL'
SET TIME ZONE 'UTC';
SET statement_timeout = 0;
SET lock_timeout = :'lock_timeout';

SELECT format(
  'CREATE INDEX CONCURRENTLY IF NOT EXISTS %I ON public.%I (channel_id, created_at DESC) INCLUDE (quota, prompt_tokens, completion_tokens) WHERE type = 2;',
  c.relname || '_channel_consume_created_cover_idx',
  c.relname
)
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
JOIN pg_class p ON p.oid = i.inhparent
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND p.relname = 'logs'
  AND c.relname ~ '^logs_y[0-9]{8}$'
  AND NOT EXISTS (
    SELECT 1
    FROM pg_class idx
    JOIN pg_namespace idxn ON idxn.oid = idx.relnamespace
    WHERE idxn.nspname = 'public'
      AND idx.relname = c.relname || '_channel_consume_created_cover_idx'
  )
ORDER BY c.relname
\gexec

ANALYZE public.logs;
SQL
