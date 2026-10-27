# Double 11 gift accounts

These admin-only routes keep campaign gift quota separate from paid API keys.
The campaign is `double11-2026`, policy `double11-2026-v1`. Set
`SAAS_GIFT_GROUP` to the existing gift group (default `双十一`). Its ratio must
be positive and its enabled abilities must contain only `claude-sonnet-4-6`.
The service validates this configuration; it does not create groups or alter
channels.

## Ensure an account

`POST /api/token/admin/saas-gifts/accounts/ensure`

```json
{
  "operationId": "gift:d11-2026:ensure:wallet-id",
  "campaignId": "double11-2026",
  "beneficiaryRef": "wallet-id",
  "policyVersion": "double11-2026-v1"
}
```

`beneficiaryRef` is a stable opaque SaaS wallet identity, not an email. The
unique identity is the SHA-256 of campaign ID, a zero separator and this ref.
Each identity has a separate ordinary owner and a single restricted key. The
owner and key start at zero quota; normal signup gifts do not run. The owner
cannot log into the console. Group, exact model whitelist, finite quota and
disabled cross-group retry are established in the same transaction as the
binding, before any key can be delivered.

The `{success:true,data:...}` response contains `accountId`, `campaignId`,
`policyVersion`, `policyFingerprint`, `group`, `userId`, `tokenId`, `state`,
`created`, `createdAt`. Timestamps are Unix seconds. Repeated ensure calls
return the same owner and token. Different payloads for the same operation ID
return HTTP 409. Replays validate the current binding and policy before
returning an active account. Deleted or modified targets are not recreated.

Ensure never returns a plaintext API key. The existing
`POST /api/token/admin/:id/key` endpoint requires RootAuth and is the secure
delivery path for the SaaS backend; the ordinary GET metadata endpoint is
masked. The key read returns a bare key, so an API client normally prefixes it
with `sk-`. Do not put plaintext keys in operation IDs, notes or logs.

## Operations

`POST /api/token/admin/saas-gifts/operations`

```json
{
  "operationId": "gift:d11-2026:grant:entitlement-id",
  "accountId": "<64-character account ID>",
  "kind": "credit",
  "amount": 120000000
}
```

Credit amounts are positive integer quota units, capped at `2^53-1`. The server
resolves owner/token from the binding and credits both the owner's wallet and
the key's usage ceiling. This is one gift, not two grants. The internal credit
transaction, immutable receipt and cumulative cache watermark reuse
[SaaS credit operations](saas-credit-operations.md).

The response has `operationId`, `accountId`, `kind`, `amount`, `state`,
`appliedAt`, `duplicated`, and an optional `receipt`. An applied credit has
`state: "APPLIED"` and the original SaaSCreditOperation receipt. The ordinary
top-up paths reject gift identities, including their final transaction check;
changing a group label cannot turn a gift key into a paid key.

`GET /api/token/admin/saas-gifts/operations/:operationId` reads the immutable
result. Persist ID and full payload before the first POST. After timeout,
query or repeat the same ID; missing results may race an unfinished commit.
Never change the ID to recover an uncertain request. A durable matching
receipt resolves a lost commit acknowledgement. Reading a receipt does not
write a log or change quota.

### Cancel a credit before or during delivery

```json
{
  "operationId": "gift:d11-2026:cancel:refund-id",
  "campaignId": "double11-2026",
  "beneficiaryRef": "wallet-id",
  "kind": "cancel_credit",
  "targetOperationId": "gift:d11-2026:grant:entitlement-id"
}
```

Cancellation competes with the target credit under the same identity lock.
If already committed, the response is `APPLIED` with the original target
receipt; no quota is removed. Otherwise it creates a permanent `CANCELED`
credit tombstone. Any later POST using that grant ID returns `CANCELED`, with
no credit receipt and amount zero (the cancellation did not grant money).
Consumers must handle this terminal state before checking applied amounts.
Cancellation does not erase other orders or silently create a replacement ID.

### Freeze for refund review

```json
{
  "operationId": "gift:d11-2026:freeze:refund-id",
  "campaignId": "double11-2026",
  "beneficiaryRef": "wallet-id",
  "kind": "freeze"
}
```

Freeze/cancel can use `accountId` instead of the stable identity; if both are
provided they must match. Stable identity works before ensure, including when
its response was lost. Freeze creates a `FROZEN_PENDING_REVIEW` binding with
null user/token IDs if needed. A later ensure returns that frozen identity
without creating usable resources. An already provisioned frozen owner cannot
start new wallet reservations, even with stale cache data or a Redis outage.
Requests accepted before the freeze can finish settling and returning unused
reservations. Querying existing credit receipts remains possible.

Freeze is **not a quota reversal**. This implementation does not automatically
deduct used gifts, permit negative credit amounts or debit paid balances.
SaaS should cancel pending grants and freeze the gift identity, then retain the
refund for manual reconciliation. There is intentionally no public unfreeze
or balance-override operation. Any later recovery feature must preserve the
canceled grant tombstones and account for in-flight settlement; ordinary admin
token editing is blocked for gift identities.

## Migration and verification

Both normal and fast migrations add `saas_gift_accounts` and
`saas_gift_operations`, plus immutable gift account IDs on users/tokens and a
gift freeze flag on users. Deploy/migrate every NewAPI replica before enabling
SaaS calls. Do not run an older cache writer or reserve implementation against
live gift keys. Rollback must preserve bindings, canceled IDs, receipts and
credit watermarks; disabling new claims does not erase existing obligations.

```sh
go test ./model ./controller ./middleware ./service ./router -count=1
DEVELOPER_DIR=/Library/Developer/CommandLineTools CGO_ENABLED=1 go test -race ./model ./controller -run 'TestSaaSGift|TestSaaSCredit' -count=1
```

`TestSaaSGiftExternalDatabases` accepts `TEST_SAAS_GIFT_POSTGRES_DSN` or
`TEST_SAAS_GIFT_MYSQL_DSN` only for new disposable databases, refusing any that
already contain the business tables. It verifies independent/replayed grants,
concurrent ensure, cancellation/freeze and balances exceeding 32-bit ranges.
Never point these tests at production.
