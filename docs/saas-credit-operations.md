# SaaS credit operations

The admin-only contract applies one top-up to the user wallet, a token, or both in one database transaction. `saas_credit_operations` is the immutable receipt and idempotency ledger. A successful receipt proves the credit committed; current balance changes are not used as evidence of delivery.

## API

`POST /api/token/admin/saas-topup/credit-operations`

```json
{
  "operationId": "saas:global:topup:TOP123",
  "userId": 123,
  "tokenId": 456,
  "amount": 500000,
  "creditUserQuota": true,
  "note": "Topup order TOP123"
}
```

- `operationId`: case-sensitive ASCII, `^[A-Za-z0-9][A-Za-z0-9._:-]{0,159}$`. It is globally unique within this NewAPI database. Its SHA-256 primary key avoids database collation differences.
- `amount`: positive integer quota units. Amounts are at most `9,007,199,254,740,991` (`Number.MAX_SAFE_INTEGER`). Before/after balances must remain within its signed range. Balances may be negative; a positive credit can partially or fully repay that debt. Existing balances above `2^31 - 1` and a `3,000,000,000` credit are supported.
- `creditUserQuota`: explicitly required, including when `false`. With a token, `true` credits both balances; `false` credits the token only.
- `tokenId`: optional/null for a wallet-only credit, in which case `creditUserQuota` must be `true`.
- `note`: optional informational text, at most 500 UTF-8 bytes. It is not part of the idempotency comparison. Never include token keys or other credentials.
- Request body limit: 4 KiB.

The wallet must exist and be enabled and eligible for SaaS top-up. If supplied, the token must belong to that wallet and must not be disabled or deleted. Expired tokens retain their expiry. Exhausted tokens become enabled only if the resulting remaining quota is positive. Replaying an already committed operation returns its receipt even if the target has subsequently been disabled or deleted.

```json
{
  "success": true,
  "message": "",
  "data": {
    "operationId": "saas:global:topup:TOP123",
    "userId": 123,
    "tokenId": 456,
    "amount": 500000,
    "creditUserQuota": true,
    "beforeUserQuota": 100000,
    "afterUserQuota": 600000,
    "beforeRemainQuota": 100000,
    "afterRemainQuota": 600000,
    "appliedAt": 1788600000,
    "duplicated": false
  }
}
```

`appliedAt` is Unix seconds. Token fields are explicit `null` for wallet-only credits. The balances are the database values inside the credit transaction: they exclude unflushed usage deltas and are not a current-balance endpoint. A repeated POST with identical user/token/amount/policy returns the same receipt with `duplicated: true`; changing any of those fields returns HTTP 409. A changed note does not create another credit.

`GET /api/token/admin/saas-topup/credit-operations/:operationId` returns the stored receipt with `duplicated: false`; it has no log-recovery side effects. Missing receipts return HTTP 404. Invalid input returns 400; an ineligible target returns 404; balance overflow returns 422; an unexpected backend failure returns 500. Existing admin authentication behavior is unchanged.

## Client recovery

Persist the operation ID and exact payload before the first POST. On timeout or a lost acknowledgement, GET the receipt or repeat the identical POST. A 404 lookup can race an operation still committing; retry the same ID. Never generate a new ID to recover an uncertain operation. Treat a 409 as a payload mismatch requiring reconciliation.

The unique ledger insertion, wallet increment, token increment, exhaustion recovery, and receipt fields commit together. The credit bypasses the in-memory batch queue. Go quota and ledger fields share 64-bit database storage: GORM maps their integer size to BIGINT on MySQL/PostgreSQL and INTEGER on SQLite. The historical `type:int` GORM tag is an abstract type, not an SQL int32 constraint. Existing wallet/token column tags are retained. Only the new receipt fields explicitly specify `size:64`; the token watermark is a new `int64` column, alongside the explicit quota-edit revision. Billing calculation constant `common.MaxQuota = 2,147,483,647` remains a separate per-request conversion guard and does not cap credit balances. SQLite takes the write lock by inserting first; MySQL/PostgreSQL then lock wallet and token rows in a consistent order. Known lock/deadlock/serialization failures receive a short bounded retry.

## Cache and rollout boundaries

- User cache generation invalidation fences old authentication snapshots. Direct quota reads no longer asynchronously overwrite a newer cached quota.
- `tokens.saas_credited_quota` is a cumulative credit watermark. Redis reconciles the difference into the live token balance, preserving already cached consumption that is still waiting for batch flush. Delivery is idempotent and can arrive out of order. The cumulative watermark is capped at the exact Lua integer range, `2^53 - 1`.
- Both legacy `GrantTokenRemainQuota` and the new operation transaction increment remaining quota and the cumulative watermark together. Legacy grant validation uses a DB-only read, avoiding a pre-credit async balance overwrite. Legacy routes keep their own authorization/eligibility rules; they do not gain HTTP idempotency.
- DB readback rejects a credit snapshot older than either the delivered watermark or the cached watermark. For a warm credited token at the same edit revision, it keeps live remaining quota and adds only previously unseen credit. Used quota and other metadata continue to refresh from the DB, matching the existing consumer which only adjusts cached remaining quota. Uncredited tokens retain the original snapshot behavior. The ordinary consumption increment/decrement implementation is retained.
- `tokens.saas_quota_revision` is an independent ordering guard for explicit balance assignments on credited tokens. `Token.Update` increments it atomically with the assignment and reloads the committed row. Newer edit snapshots replace the assigned balance while accounting for any later delivered credit; older edit snapshots are rejected, including after main-hash eviction. This avoids treating the cumulative credit amount as an edit version. Normal consumption does not increment this field.
- Redis failures do not reverse a committed credit. Local pending-credit totals force DB fallback; an up-to-date DB read or identical POST retries cache delivery. An older successful delivery cannot clear a newer pending total.
- This does not redesign the existing consumption batch queue. Cache misses and Redis outages still inherit that queue's eventual-consistency limits; other instances can briefly retain a stale derived balance until invalidation/expiry. A receipt is authoritative regardless of cache state.
- Both regular and fast startup migrations register `saas_credit_operations`; token migration adds `saas_credited_quota` and `saas_quota_revision` with zero defaults. There is no new log-database table or maintenance task. Deploy the backend and migrate before enabling the SaaS caller. Update every replica's token-cache writer before enabling the new path; an old binary can overwrite values without respecting the watermark.
- Keep the ledger/idempotency records for as long as operations can be replayed. A ledger row has no plaintext token key. Keep both token ordering columns and the corresponding Redis credit/revision keys through a rollback; do not reset cumulative totals while operation receipts can replay.
- The existing database-atomic wallet reservation (`ReserveUserQuota`, including `WalletFunding.PreConsume/Settle`) is unchanged. RPM, group, cross-group retry, IP/model restrictions, status and expiry remain part of the fork's token model.

## Separate follow-up changes

The rejected candidate is archived under `.audit/saas-credit-revision-20260906/rejected-before.patch` (SHA-256 `0a45fa6bfa3ccddb9d45fc39a10bfa21a65b94e65f236b5ceea4f4f2f0af1011`). Do not apply it: its global cold-only cache behavior caused cross-path regressions. This revision contains the operation contract and the narrow cache integration needed by credit paths.

The first successful credit uses the existing best-effort `RecordLog` path. Identical replay and GET do not write another log. A missing log is not evidence that a credit failed; the operation receipt is authoritative. There are no projection-state fields, projection table, or recovery jobs in this change. Crash-safe historical log delivery and the official cache/atomic-consumption package are separate review scopes in [deferred changes](saas-credit-deferred-changes.md).


## Verification

```sh
CGO_ENABLED=0 go test ./model ./controller ./router -count=1
DEVELOPER_DIR=/Library/Developer/CommandLineTools CGO_ENABLED=1 go test -race ./model ./controller -run TestSaaSCredit -count=1
```

External database tests are opt-in using `TEST_SAAS_CREDIT_POSTGRES_DSN` or `TEST_SAAS_CREDIT_MYSQL_DSN`, pointing to a new disposable database. They refuse any database where the business tables already exist. Run with `go test ./model -run TestSaaSCreditOperationExternalDatabases -count=1`.

Local validation on 2026-09-05: all tests in `model`, `controller`, `router`, `service`, and `middleware` passed; the new contract's race tests passed. A separately initialized PostgreSQL instance using its own Unix socket passed concurrent independent/replayed operations, receipt lookup, and conflict tests and was stopped and removed afterward. MySQL integration was not executed because no local MySQL server or running Docker daemon was available; its opt-in test remains available.

Log-projection validation additionally covers a separate SQLite LOG_DB failing after its marker insert, GET/replay recovery, a lost main-ledger acknowledgement after the log commit, eight concurrent projection requests, a bounded recovery page, original display text after target renames, and observable ClickHouse deferral.

Cross-repository verification on 2026-09-06: the SaaS client path, ID regex, request fields, envelope, explicit null token fields, boolean policy, and Unix-second receipt timestamp match. Admin-owned token grants with `creditUserQuota:false` preserve the wallet even with balances in the billions. The previous int32 credit cap was removed; storage mappings and real PostgreSQL column metadata confirm 64-bit quota/receipt fields. Tests cover a 3,000,000,000 credit on 6/7-billion balances, replay, token-only credit, negative-balance recovery, JS-safe cumulative limits, and watermark overflow.

## Revision validation (2026-09-06)

Evidence for this revised candidate is under `.audit/saas-credit-revision-20260906/`, separate from the rejected candidate's earlier test runs. The scoped cases cover real Gin handlers with SQLite/miniredis, legacy/new mixed credits, cold late reads, actual batch consumption/readback, reordered explicit balance snapshots, Redis-delivery recovery, wallet stale reads, and the fork's existing DB wallet reservation. External PostgreSQL tests also exercise the legacy/new transaction interleaving and explicit edit SQL. MySQL runtime remains unverified because no local database server is available; generated dialect storage contracts are tested. These are local tests, not a production deployment or production business probe.
