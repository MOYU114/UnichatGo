# Async execution operations

Date: 2026-10-12. **Development/isolated verification only; not approved for production canary.**

## Implemented contract

Enable `async.enabled` only with `UNICHATGO_DB=mysql`. Roles `api`, `worker`, `all` share MySQL and Redis but do not need a shared upload directory. Each worker slot blocks on Redis Streams; SQL compact execution evidence authorizes calls and fences result writes. The stream carries only a UUID. Final reply/history state commits before ACK. An API connection or process disappearing does not cancel the independently managed worker.

The additive migration is version 1, connection-scoped advisory locked and rerunnable after partial DDL. It installs a one-way legacy startup latch. **This cannot stop an already-running old binary**: all legacy writers and cleaners must drain and stop before migration. A binary from before this change does not understand the latch and is not a supported rollback target.

Configuration examples keep async disabled and paused. Placeholder `${...}` strings in existing JSON examples are not automatically environment-expanded; render a protected config file explicitly. Database pool budget is 12 open / 4 idle per process, 5-minute connection lifetime, MySQL connect/read/write timeouts 5/10/10 seconds unless explicitly configured. Multiply pools by every API/worker instance before deployment. `parseTime` is forced on for MySQL.

## Startup, drain and rollback

1. Back up and verify restoration of MySQL in a separate environment. Inventory legacy files on every source host; use the dry-run/copy tool described in `legacy-attachment-migration.md`. Do not delete original files.
2. Stop legacy acceptance, drain old tasks, stop old workers and file cleaners. Do not run old and new model calls in shadow mode. Only then start an async-compatible process for additive migration.
3. All roles must use the same decoded `UNICHATGO_APIKEY_KEY` and resolved admission/quota configuration. SQL fingerprints reject mixed key/quota/admission/provider-routing configurations; tokens are AES-GCM sealed in each request snapshot. Existing request credentials remain frozen if a user changes their token. Key rotation is not an arbitrary environment change: it needs a separately tested decrypt/re-encrypt migration after drain.
4. Set `async.paused=true` on **every** API process and remove old API routes from traffic before a rollback/drain. This flag is per process, not a magical distributed switch. Keep compatible workers running to finish/recover evidence. Completed-key replay remains available while paused.
5. SIGTERM stops new pulls and HTTP acceptance, then drains already claimed tasks under their original bounded task deadline. Supervisor grace must exceed task timeout plus DB timeout; forced kill may correctly produce uncertain. Worker-only processes expose readiness/operations endpoints but no user routes.
6. Roll back only to an artifact that understands the schema/evidence/fencing protocol, keep additive tables and shared objects, re-run fault tests. Never remove evidence, reset quota, or switch back to legacy workers to clear a backlog. No production rollout or rollback was performed in this work.

## Global limits

SQL serializes short user/session and quota transactions. Defaults are conservative placeholders, not measured production capacity: session outstanding 8, user outstanding 32, user accept RPM 60, task lifetime 300 seconds from acceptance; process pull slots 4; platform/credential concurrency 4, user concurrency 2, RPM 60, TPM reservation 200000. Rate facts survive session deletion. Definitive responses release concurrency; uncertain holds remain until task deadline plus safety grace. Rate/token reservations are not refunded.

By default credentials are grouped by provider/endpoint/actual token digest. For multiple credentials sharing one upstream account, set the same non-secret `providers.<name>.quota_group` and coordinate changes after drain. The platform cap remains a hard global limiter. Alias/endpoint routing configuration is fingerprint-bound across API replicas; mismatched startup is rejected. Changes require a coordinated drained migration, not an unvalidated rolling edit. Redis cannot reset these SQL reservations.

Token accounting reserves UTF-8 bytes/framing/output cap and `32768 + 8192 * ceil(width/512) * ceil(height/512)` per validated static image. A short question with one 512x512 image reserves about 45000 tokens in total; one 2048x2048 image also fits the unchanged default TPM 200000. Multiple images or long context can exceed it. Images remain limited to static PNG/JPEG/GIF/WebP <=2048x2048. This is a conservative policy with margin above documented OpenAI tile/patch rates, not a universal tokenizer theorem for other providers/custom endpoints; production accounting still needs validation.

New requests are checked against the currently knowable effective SQL history and attachments inside the acceptance transaction, after same-key replay checking. Impossible budgets return 422 `token_budget_exceeded` without acceptance writes; no quota protection is removed and no default TPM increase occurs. The Worker still validates actual full context and atomically reserves all quota scopes. Earlier queued replies are unknown at acceptance, so later context growth can still cause a truthful final budget failure without a paid call. Provider timeout does not prove the remote service physically stopped; exactly-once execution/billing is not claimed.

## Attachments and feature limitations

Shared attachment objects live in bounded MySQL `MEDIUMBLOB` rows: 8 MiB/object, 64 MiB/user, 24-hour availability, ownership checks and SHA-256. SQL admission locks storage quota and active evidence pins objects. Cleanup and deletion serialize with acceptance. An uncertain request keeps its pins; a session/user with outstanding requests cannot be deleted. The migration tool copies old files without deleting originals and reports old/new ID mappings.

Async payloads with files must declare `attachment_namespace: shared-v1`; raw legacy file IDs are never interpreted as new IDs. Text/current-turn static images are supported. **PDF/Office processing, Agent web/file tools, paid summaries, semantic titles and historical attachment reuse are not parity-complete.** Unsupported uploaded document types are rejected rather than silently ignored. New sessions currently keep the local `New Conversation` title. A completed turn releases its pin; its raw attachment bytes are not automatically inserted into later turns. Do not open production traffic requiring those legacy features until that migration is completed and reviewed.

## Recovery and uncertain

Publisher scans durable due Outbox rows in batches of 100, CAS-marks the selected generation after XADD, and keeps active inputs. Re-publication is bounded to a 30-second normal interval; handoff waiting is 2 seconds. Redis loss does not grant execution: SQL head, owner, live lease, fence, call boundary and quota are rechecked. Recovery runs at one-second intervals in workers; actual recovery latency is workload dependent.

- Before-call owner loss: safe to return to queued with a new fence.
- After-call owner loss or ambiguous transport/response: persist uncertain; never automatically regenerate.
- A complete valid response explicitly ending with `length`, `max_tokens` or `MAX_TOKENS`: persist failed with `provider_output_truncated`, discard incomplete assistant output, continue later turns only after the failure commits. No automatic retry, no uncertain quarantine or abandon requirement. A response transfer error overrides any marker seen in partial bytes and remains uncertain.
- Commit succeeded but ACK lost: re-delivery reads durable terminal result and never invokes Provider.
- SQL temporarily unavailable after response: retry saving the **same** response, five bounded attempts (100/200/400/800/1600ms backoff). After budget exhaustion/crash, recover conservatively as uncertain; no durable copy of an uncommitted response is promised.
- Invalid envelopes are durably quarantined before ACK. Do not use stream trimming/DEL/FLUSHDB as a backlog operation.

Use authenticated `POST /api/users/:id/conversation/requests/:request_id/resolve` with `{"action":"abandon","reason":"operator/user explanation"}`. Only the owning user may resolve; the lease must expire first. The operation audits actor/reason/fence, records `failed / abandoned_uncertain`, fences the old worker and unblocks the session. It does not claim cancellation, refund quota early or automatically create a retry. A subsequent explicit new request can incur an additional charge.

## Readiness, metrics and alerts

`GET /readyz` checks exact schema version/write latch and Redis availability. `GET /internal/execution/metrics` requires `Authorization: Bearer <UNICHATGO_OPERATIONS_TOKEN>`; unset token disables access. Bind/proxy it to operations networks only, never put that token in the browser. Metrics contain bounded aggregate counts: queued/running/uncertain/failed, oldest queued age, due outbox, PEL pending, per-process publication/completion/error totals and DB pool pressure. Core logs include request ID/sequence/fence/state/error code, not content or Provider credentials.

Operational alerts to wire in the deployment's existing monitor: uncertain >0 needs human triage; oldest queued age approaching task timeout; growing due outbox/PEL with no completions; DB waits/errors; readiness failures. No external alerting service/dashboard was configured. Aggregate failed counts include retained history, so alert on increases/rates, not an absolute nonzero count. Readiness does not prove storage durability or full feature parity.

Frontend history is authoritative, polling is observation only. Transport failures display connection/unknown-acceptance state. The frozen retry journal lives in browser localStorage under a user/session namespace, protected by Web Locks; HTTPS or localhost and Web Locks support are required for async submission. It contains message text/provider/model/file IDs but no Provider credentials. Do not clear this journal to force a retry when acceptance is unknown; reconcile history or retry the same key. Shared-device users should understand this retention; no automatic cross-tab/new-key resubmission occurs.

## Verified storage boundary

Read-only checks on the isolated containers recorded MySQL 8.4.11, `innodb_flush_log_at_trx_commit=1`, `sync_binlog=1`, `max_allowed_packet=67108864`; Redis appendonly=yes, appendfsync=always, maxmemory-policy=noeviction and AOF last-write status=ok. These are actual configuration observations, **not** measured power-loss RPO, backup restoration, cross-host failover or production settings. Keep task delivery Redis separate from evictable cache resource policies. Refer to the checkpoint/test report for executed scenarios and missing gates.
