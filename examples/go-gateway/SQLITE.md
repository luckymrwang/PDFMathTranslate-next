# Gateway SQLite storage

The gateway stores order metadata and referral balances in `VIRTUALPAY_DATA_DIR/gateway.db` using the pure Go `modernc.org/sqlite` driver. No database container or CGO is required. Keep the existing persistent `/data` bind mount; uploads, previews and result PDFs remain files in that directory.

## Transactions and deployment

A single gateway process owns a data directory. Do not run multiple gateways against it, edit the database while the gateway is running, or place it on a network filesystem. Order snapshots are cached in memory; SQL changes made outside the gateway are not picked up until restart.

WAL mode, `synchronous=FULL`, foreign keys and a five-second busy timeout are enabled. Order creation and points debit commit together. First login, invitation binding, both rewards and daily cap checks commit together. Refund/platform-confirmed closure, credit return and the return marker commit together. Local cancellation retains the credit reservation because payment may arrive late.

## First startup and JSON migration

1. Stop the old gateway and back up the complete data directory.
2. Start the new gateway with the same data mount. It imports `orders/*.json` and `referrals.json` in one transaction, including historical reward/return markers and all order fields.
3. Invalid JSON, mismatched identities or conflicting platform IDs abort startup and roll back the import. Correct the source data only after investigation, then retry.
4. A committed migration marker prevents importing legacy files again. The original JSON files remain untouched as a historical snapshot; they are no longer updated.
5. Check `/health`, user order history, invitation balances and archived downloads before accepting new traffic.

Do not roll back to the JSON binary after new SQLite writes: the old files are stale. Rollback requires restoring the pre-migration backup and accepting/reconciling any changes since it, or an explicit export tool. No production deployment is performed by the code migration itself.

## Inspection and backup

Inspect a backup with SQLite tooling. `orders` exposes indexed `id`, `openid`, `state`, `wx_order_id` and `created_at` columns; `record` contains the complete serialized order, including payment proof, page counts and archive paths. For example:

```sql
SELECT id, openid, state, json_extract(record, '$.Total') AS total_fen
FROM orders ORDER BY created_at DESC LIMIT 20;
SELECT openid, code, credit FROM referral_users;
SELECT inviter, count(*) FROM referral_rewards GROUP BY inviter;
PRAGMA integrity_check;
```

For the simplest consistent backup, stop the gateway, back up the entire data directory, then restart it. For a live database use SQLite's online backup API or the `sqlite3` CLI `.backup` command, and back up PDFs separately with retention coordinated with the gateway. Do not copy only `gateway.db` while it is running: committed writes can still reside in `gateway.db-wal`. Never delete WAL/SHM files manually.

The runtime image does not install the SQLite CLI. Use host tooling or a separate maintenance environment, with the gateway stopped for any manual modification. Backups contain user identities and payment metadata and must be protected accordingly.

## Version 2 points migration

Startup imports legacy JSON first, then upgrades version 1 to 2 in the same immediate transaction.
`referral_users.credit` now stores integer points: old unspent page balances are multiplied by 10 once.
Every old order receives explicit `CreditPoints = CreditPages * 10`, preserving `CreditReturned`
and `referral_returns` so historical reservations return in the right units exactly once.
The version marker prevents conversion on restart. Migration and returns do not consume earning caps.

`points_logins` records first login, `referral_rewards` records invitation bindings,
`points_daily` tracks Beijing-day earning/claim status, and `points_ledger` has unique events for
invites, daily claims, purchase debits and returns. Capped earning events have zero delta and
cannot be replayed on another day. Old return markers remain authoritative.
Known order owners, referral account owners, historical invitees and recorded inviters are seeded
as existing users. Previously login-only users had no durable record; their first persisted login
after upgrade is the registration baseline. Historical trial archival never grants new points.

Back up the complete database directory before upgrade. A version 1 binary refuses version 2;
rollback requires restoring the complete pre-upgrade backup, including coordinated PDF state.

Submission recovery distinguishes confirmed failure from uncertain acceptance. Local engine/source
preflight failures and explicit worker 4xx rejection consume the bounded delivery retry budget;
exhaustion marks delivery failed and returns full-points reservations once. Transport errors,
5xx responses, malformed success payloads and post-submit storage errors may hide an existing
worker task: recovery retains the same idempotency key and reservation until the worker resolves
the outcome. These ambiguous cases are not automatically refunded or resubmitted under a new key.
Released historical upgrade reservations do not block replacement orders from progressing.
