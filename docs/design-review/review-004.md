# Review 004: Durable repository, provider adapters, and frontend recovery

Date: 2026-10-12. Scope: independent implementation review, not a production-release approval.

## Review separation

- Frontend agent independently read the complete new `backend/internal/execution/repository.go`, `quota.go`, `attachments.go`, `migrations.go`, `types.go`, `repository_integration_test.go`, `provider.go`, and `provider_test.go`, including untracked files. It did not implement those production files.
- Design-review agent independently reviewed the frontend implementation and its tests, and then re-reviewed fixes. It also reviewed the frontend agent's `attachment_race_test.go`; this test file is not counted as independently reviewed by its author.
- Provider review occurred after the static-image bounds and token estimator changes. Runtime/API/deployment review belongs to the other recorded review rounds; this record does not substitute for them.

## Findings and disposition

| ID | Severity | Location / trigger / impact | Fix and re-review |
| --- | --- | --- | --- |
| R4-01 | P2 | `backend/internal/execution/migrations.go`, migration schema statements: startup upsert reset an existing FALSE execution write latch to TRUE. This defeated an explicitly closed guard. | Repository owner changed to `INSERT IGNORE`; reviewer confirmed existing values remain unchanged. Advisory lock release also now has a five-second context limit. |
| F4-01 | P2 | `frontend/src/store/session.js`, pending acceptance: tab-scoped storage lost the frozen client ID on tab close, allowing a new ID after an ambiguous POST. | Durable origin `localStorage`, user/session-scoped journal; Web Locks serialize writes across tabs. Async send/retry fails closed without safe storage/locks. Matching-ID cleanup prevents an older response clearing another journal. Reopening and two-tab race regressions pass. |
| F4-02 | P2 | `frontend/src/utils/execution.js`, failed-state description: frontend expected `user_abandoned`, backend emits `abandoned_uncertain`; history hid the abandonment/possible-charge explanation. | Contract aligned and regression test added. |
| F4-03 | P2 | `frontend/src/store/auth.js`, account deletion: clearing auth in `finally` signed the user out when in-flight execution caused a legitimate rejection. | Clear auth only after successful deletion; dashboard reports rejection. Regression preserves login on deletion failure. |
| F4-04 | P2 | `frontend/src/store/session.js`, batch uploads: later upload failure dropped earlier successful selections and leaked preview ownership. | Commit each successful upload to reactive state before proceeding. Regression retains first file on second-file failure. |
| F4-05 | P2 | `frontend/src/store/session.js`, history reconciliation after lost receipt: accepted attachment selections stayed in composer and could be resent unintentionally. | Shared matching-file cleanup used by both receipt and history paths. Regression reconciles accepted attachment IDs and clears selection. |
| F4-06 | P2 | `frontend/src/store/session.js`, new lock integration: legacy delete attempted Web Locks after successful server deletion, breaking nonsecure/older-browser legacy operation. | Legacy/no-journal deletion bypasses lock; async deletion obtains lock before network. Regression proves no async DELETE without locks and successful legacy delete without locks. |

The design-review agent re-reviewed F4-01 through F4-06 and independently ran the 34-test frontend suite. No remaining P0/P1/P2 findings were identified in this review scope. This is not proof that no defects remain.

## Namespace and verification-harness follow-up

- Frontend async payloads now always freeze `attachment_namespace: shared-v1`, including text-only sends. Old journals containing file IDs without a namespace fail closed without changing their key or issuing a POST; history may still reconcile an already accepted request. Empty-file old journals normalize only the wire payload to the backend's canonical shared namespace. Async capabilities with another namespace fail closed.
- Added four pure/store regressions; frontend suite is now 38/38 passed and production build passed. Design-review agent independently reviewed and accepted this namespace follow-up.
- Added `backend/scripts/verify-async.sh`, `docker-compose.async-test.yml`, and `docs/roadmap/async-test-runbook.md`. The independent design-review agent found P2 H4-01: an initial credential redactor masked `Bearer` but left the credential value. Fixed by masking the entire header value, with a built-in regression probe for Bearer and quoted API-key headers; independently re-reviewed and accepted.
- Harness requires explicit acknowledged loopback test services and a `_test` database, overrides legacy cache tests onto test Redis DB 15, enables isolated subprocess tests, rejects any skipped Go test, and never tears down services/volumes. Compose configuration syntax validated with `docker compose ... config --quiet`; new Compose services were not started by this subtask.
- First full harness execution on 2026-10-12 13:16 Singapore time: backend `-p 1 -race -count=1 -v ./...` passed, frontend 38 tests/build passed, but **three legacy Redis cache tests skipped** because the original harness did not yet map their environment variables. Therefore this first run is **partial validation, not a complete no-skip pass**. It predates later runtime/subprocess additions. Its sanitized report is at `/var/folders/0r/b3fs4y6d35jdkqbhsb2htwt40000gn/T/unichatgo-async-reports/run.OrX0NV/report.txt`. Fixed environment mapping and any-skip guard require a fresh run after other agents finish their active test edits; that rerun was not yet executed at this checkpoint.

## API/process and transient-save test follow-up

The frontend agent independently reviewed the repository agent's complete `backend/internal/api/async_integration_test.go` and `process_integration_test.go`, plus the design-review agent's `backend/internal/execution/save_recovery_test.go`.

- P2 T4-01: the API disconnect/drain test only canceled the worker pull context on failure, leaving the independently executing Provider blocked while cleanup closed its database. Fixed with an once-only barrier release and bounded runtime join registered for cleanup; independently re-reviewed.
- P2 T4-02: subprocess mock observation used an unbounded channel send; an actual retry storm could fill the observation buffer and hang mock-server cleanup instead of failing. Fixed with context/nonblocking branches and explicit error; independently re-reviewed.
- P2 T4-03: transient-save fault test could likewise leave its Provider waiting for a renewal signal after an assertion timeout. Fixed with an independent abort channel and bounded join before SQL cleanup. Lease increased from one to three seconds for scheduler margin; independently re-reviewed. The fault connector now enables parameter interpolation so successful renewal statements are observed directly rather than hidden by a driver's prepared-statement fallback.
- Subprocess fixtures build the actual backend binary, use distinct local working directories, unique SQL schemas, mock HTTP Provider and reserved Redis DB 14. The assertions cover A-upload/B-accept, API process kill/restart, worker post-call kill followed by SQL lease expiry injection, uncertain history and Redis stream rebuild. This is **local multi-process evidence**, not separate physical hosts or power-loss durability. Graceful stop after completion is not the same as active subprocess drain; the API/runtime barrier test separately checks in-flight drain.
- Transient-save tests inject selected statement/renewal errors before the driver executes them, while other transactions and locks use real MySQL. They assert one original reply, one Provider call and one three-scope quota reservation. They do **not** constitute a real database service-stop/network-partition drill.
- No remaining P0/P1/P2 findings were identified in these test files after re-review. Fresh full-harness execution after these changes is tracked separately; static acceptance here is not a substituted test pass.

## No-skip harness checkpoint

The first strict no-skip run (`run.jXi7gH`) completed backend execution but **failed** in legacy Redis tests, so its frontend steps were **not executed**. API package passed (19.369s), including subprocess fault test (14.34s); execution package passed (6.374s), including the two new real-MySQL transient-statement regressions. The failures were `TestAuthTokenCacheUsesRedis` (an old cache-only authentication expectation after SQL revocation) and `TestStateCachePubSub` (publish raced asynchronous subscription startup). The parent was notified for correction and independent review before another full run. This failed run is not overall acceptance evidence; its report is `/var/folders/0r/b3fs4y6d35jdkqbhsb2htwt40000gn/T/unichatgo-async-reports/run.jXi7gH/report.txt`.

The repository agent corrected the stale authentication test to assert SQL revocation despite an explicitly still-present Redis token, and changed legacy listener initialization to wait for the Redis subscription acknowledgement before returning (bounded initialization context, close on failure). The frontend agent independently reviewed both fixes and accepted them before the fresh run.

**Final fresh no-skip run: passed.** Started 2026-10-12 13:29:01 Singapore time; sanitized report `/var/folders/0r/b3fs4y6d35jdkqbhsb2htwt40000gn/T/unichatgo-async-reports/run.H2A3tS/report.txt`.

- `go test -p 1 -race -count=1 -v ./...`: 65 top-level tests passed, 108 PASS entries including subtests, zero failures, **zero skips**. API 20.096s; auth 1.962s; execution 6.212s; assistant 2.501s; worker 2.792s.
- Actual subprocess scenario passed in 14.95s; transient renewal/result-save scenario passed in 2.35s. Mock Providers only, preserved isolated services/volumes.
- Frontend `npm test`: 38/38 passed, no skips. `npm run build`: passed in 1.89s; pre-existing large-chunk warning remains. macOS linker emitted LC_DYSYMTAB warnings but test binaries executed successfully; no race report was emitted.
- Saved report scan found no MySQL DSN literals. The final run occurred after the prior shared-fixture runs completed; no concurrent integration invocation was authorized during it.

This supersedes the earlier incomplete/failed runs as current automated regression evidence without erasing their history. It remains narrower than complete M1-M6 acceptance: browser E2E, full DB/Redis outage/durable restart, physical-host storage/power-loss drills, complete legacy feature parity and measured capacity/rollout gates are still not proven by this script.

### Post-run test-construction correction

The main agent subsequently ran `go vet ./...`, which identified `rt2 := *rt` copying Runtime's atomic counters/noCopy marker in `runtime_integration_test.go`. The main agent replaced this with explicit construction of a fresh Runtime retaining the required repository, Redis, Provider, decryption, quota and lease dependencies. The frontend agent independently inspected and accepted the fix: fresh statistics are appropriate for a restarted runtime and the completed-redelivery/no-new-call assertion is unchanged. This changes test construction, not production execution semantics.

The main agent personally reran the targeted race test (passed, 2.223s) and `go vet ./...` (no diagnostics). These are parent-reported and inspected dispositions, not an additional full-suite run by the frontend agent. The prior 65/108/38 full-run evidence remains labeled with its actual run; the narrow post-run correction has its separate targeted evidence.

## Invariants inspected

- Acceptance ownership check precedes idempotency lookup; user/session row locks serialize sequence, queue/storage admission, pins, and deletes. Durable message/evidence/outbox commit is atomic, and binary client keys are case-sensitive.
- SQL context includes legacy prefix, completed prior turns and current user message once, excluding future/failed/uncertain input. History status/messages share a locked committed view.
- Owner/fence/live lease checks protect call boundary and final commit; stale post-boundary ownership becomes uncertain. Explicit abandonment requires expired lease and audit reason, and does not refund unknown upstream quota.
- Sorted global quota locks prevent cross-scope races. Reservation facts survive worker expiry and Redis loss; rate/token history is not refunded on completion. Cleanup removes only expired unpinned attachment objects.
- Provider adapters use a single nonstreaming POST with no SDK retry or redirect replay; transport ambiguity and incomplete completion markers are uncertain, never a successful partial reply. Error text excludes upstream bodies and credentials.
- Image dimensions/static-container restrictions bound the conservative per-pixel reservation. Tests cover PNG/GIF/WebP and animation rejection. This estimator is not a measured tokenizer or a guarantee about arbitrary custom-provider billing.

## Executed verification

Executed against isolated local MySQL at `127.0.0.1:13316`, database `unichatgo_async_test`, with `UNICHATGO_TEST_MYSQL_DSN` supplied by the main agent. Credentials are not production credentials. No paid provider calls were used.

```sh
cd backend
go test ./internal/execution -run 'TestMySQL(AttachmentAcceptanceDeleteRace|ExpiredAttachmentAcceptanceCleanupRace|PinnedAttachmentCompletionCleanupRace)$' -count=1 -v
go test ./internal/execution -run 'Test(MySQL|HTTPProvider|Provider|Image|WebP)' -count=1 -v
cd ../frontend
npm test
npm run build
```

- Attachment race run: 3/3 tests passed against real MySQL. Barrier-controlled acceptance/delete, expired acceptance/cleanup, and completion/pinned cleanup assertions inspect persisted outcomes, not sleeps. Twelve acceptance/delete iterations allow either lock winner; they do not prove both occurred.
- Independent backend regression: all selected MySQL/provider/image tests passed (`ok unichatgo/internal/execution 1.719s`). Includes transaction rollback trigger, concurrent same-key acceptance, replay, context ordering, fences, uncertain resolution, outbox generation CAS, quota retention, attachments and migration rerun. Mock HTTP providers only.
- Frontend: 34/34 node tests passed; production Vite build passed. Existing large-chunk warning remains (~1.02 MB uncompressed entry bundle); no unsupported optimization claim is made.
- One initial command ran from the repository root without a Go module and failed before execution. The corrected backend-directory command first hit sandbox denial of the Go cache; it was rerun with normal tool approval and passed. Neither failed invocation is counted as validation.

## Limits and operational notes

- These repository races use two repository handles and real transactions, not two independent machines/disks. They do not by themselves prove cross-host attachment serving, SQL outage recovery, deployment rollback, browser rendering, or network-end-to-end behavior. Other matrix rows need separate evidence.
- Browser history recovery/uncertain UI has deterministic store and parser tests; this review did not run real-browser end-to-end tests.
- Async retry storage requires HTTPS or localhost and browser Web Locks. The saved journal contains message/provider/model/attachment IDs, never provider credentials. Pending content remains on the local device through logout/tab close until server confirmation or definitive rejection; deleting it during ambiguous acceptance risks losing the safe retry ID.
- Runtime feature parity for tools, summaries and legacy attachment migration is outside these adapter tests and must not be inferred from text/static-image success.
- There is no exactly-once billing assertion and no production deployment, production-data change, remote push, or destructive production migration in this record.
