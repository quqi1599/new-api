#!/usr/bin/env bash

set -euo pipefail

config_path="${1:-/etc/caddy/Caddyfile}"

if [[ ! -r "$config_path" ]]; then
  echo "Caddy config is not readable: $config_path" >&2
  exit 2
fi

failed=0
adapted_config=""

cleanup() {
  if [[ -n "$adapted_config" ]]; then
    rm -f -- "$adapted_config"
  fi
}
trap cleanup EXIT

if grep -En '^[[:space:]]*flush_interval[[:space:]]+-' "$config_path"; then
  echo "ERROR: negative flush_interval prevents downstream cancellation from reaching the upstream." >&2
  failed=1
fi

if grep -En '^[[:space:]]*request_buffers([[:space:]]|$)' "$config_path"; then
  echo "WARNING: request_buffers requires an explicit memory/disk and concurrency budget." >&2
fi

if grep -En '^[[:space:]]*lb_retry_match([[:space:]]|$)' "$config_path"; then
  echo "WARNING: verify that model POST requests cannot be replayed by this retry matcher." >&2
fi

if grep -En '^[[:space:]]*lb_retries([[:space:]]|$)' "$config_path"; then
  echo "WARNING: verify that configured load-balancer retries cannot replay model POST requests." >&2
fi

# Run this check on the target node so imports, environment placeholders, and
# installed Caddy modules match production. The adapted JSON catches policy in
# imported snippets that a raw top-level Caddyfile grep cannot see.
if command -v caddy >/dev/null 2>&1; then
  adapted_config="$(mktemp)"
  if ! caddy adapt --config "$config_path" --adapter caddyfile --validate >"$adapted_config"; then
    echo "ERROR: Caddy could not adapt and validate the candidate configuration." >&2
    exit 3
  fi
  if grep -En '"flush_interval"[[:space:]]*:[[:space:]]*-' "$adapted_config"; then
    echo "ERROR: adapted Caddy JSON contains a negative flush_interval." >&2
    failed=1
  fi
  if grep -En '"lb_retry_match"[[:space:]]*:' "$adapted_config"; then
    echo "WARNING: adapted Caddy JSON contains lb_retry_match; prove POST replay is impossible." >&2
  fi
else
  echo "WARNING: caddy is unavailable; only the literal file was linted, not imports or environment expansion." >&2
fi

if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

echo "Caddy streaming policy preflight checks passed: $config_path"
