# Review 003: API, Runtime, Authentication and Deployment Integration

Date: 2026-10-12.

Reviewer: repository sub-agent, independent of the main agent's API/runtime/auth/deployment implementation. This reviewer also implemented the SQL repository and API integration tests; those portions require the separate reviews recorded in review-002 and review-004. This report is not self-review approval for repository implementation.

## Scope

Read tracked and untracked files, not only `git diff`: `backend/main.go`, `backend/internal/api/async.go`, handler routing changes, `backend/internal/execution/runtime.go`, runtime tests, provider call contract, auth service/middleware, assistant token crypto, config and storage changes. Frontend/provider-specific independent reviews are documented separately.

## Findings and Resolution

| Severity | Location | Trigger and impact | Resolution / re-review |
| --- | --- | --- | --- |
| P2 | `backend/internal/api/handlers.go`, `getSessionMessages` | Reading messages first and request states later can observe completed state without its assistant message, causing the browser to stop polling prematurely. | Main agent uses `SnapshotHistory` under the same user/session transaction lock as completion. Re-read fixed handler. API test asserts completed receipt and answer together. |
| P2 | `backend/internal/api/handlers.go`, `deleteUser`; `backend/internal/auth/service.go`, `ValidateToken` | Async user deletion cascades SQL token rows before Redis-key enumeration, leaving Redis auth entries. Redis-authoritative token checks could still authenticate removed users. | SQL is now authoritative even when Redis has a cached token. Re-read changed auth service. Redis-stale-specific regression remains separate from nil-cache API fixture. |
| P2 | `backend/internal/api/async.go`, `resolveAsync` and `startAsync` | Database errors were treated as absent requests/sessions (404), hiding outages as durable missing data. | Only actual absence maps to 404; dependency errors map to 503. Re-read fixed branches. |
| P2 | `backend/internal/execution/runtime.go`, `execute` attachment read | A temporary SQL failure before the call boundary was immediately committed as a final internal attachment failure. | Only missing attachment is terminal; transient storage errors hand off through fenced `Defer`. Re-read fixed branch. |
| P2 | `backend/internal/auth/middleware.go` and `backend/internal/api/async.go` | SQL-authoritative auth initially mapped storage errors to 401; provider-token storage errors mapped to 400. Browser could clear authentication or misrepresent a temporary dependency outage. | Explicit invalid-token and missing-provider-token sentinels separate user errors from 503 dependency failures. Real closed-DB history test returns 503 without task status or SQL detail. |
| P2 | `backend/internal/api/async.go`, `canonicalInput` | Trimming client IDs aliases distinct binary keys even though repository uniqueness is byte-sensitive. | Trimming removed; real API regression accepts `Case-ID`, `case-id` and ` Case-ID ` as distinct requests and returns the original key bytes. |

Additional reviewed safeguards: async constructor avoids starting the legacy worker pool; startup refuses legacy mode after additive migration; quota and decoded encryption-key fingerprints bind all deployment roles; HTTP request cancellation is not used as worker execution context; workers drain admitted work after pull shutdown; terminal/uncertain durable evidence precedes stream ACK; payload replay precedes current provider/default/key resolution.

## Executed Tests

The reviewer personally executed tests against isolated local MySQL and Redis. No paid provider calls were used. `async_integration_test.go` creates a uniquely named temporary MySQL schema and uses Redis logical DB 12; the test DSN must be authorized to create/drop these test schemas. Only schemas created by the test are dropped.

```sh
cd backend
GIN_MODE=release \
UNICHATGO_TEST_MYSQL_DSN='root:<isolated-test-password>@tcp(127.0.0.1:13316)/unichatgo_async_test?parseTime=true&multiStatements=true' \
UNICHATGO_TEST_REDIS_ADDR='127.0.0.1:16386' \
go test ./internal/api -run TestAsyncMySQL -count=1 -v
```

Result: 4 tests passed; package elapsed 2.604 seconds on the recorded run.

- Cross-handler barrier: one 202 acceptance, identical replay receipts, payload conflict 409, unauthorized path 403, missing owned session 404, empty history arrays, session admission 429, replay after key/config removal and acceptance pause.
- Actual `Runtime.Run`: SQL outbox to Redis pull to barrier-controlled mock provider after HTTP context cancellation; shutdown stops pulls but drains the running call; replacement handler restores the final answer and replay does not call the provider again.
- Uncertain state visible in history; new acceptance/session deletion blocked; empty audit reason rejected; abandonment rejected before lease expiry, succeeds after explicitly advanced SQL lease; acceptance then resumes.
- Real MySQL handle unavailability at authentication/history boundary returns 503, no invented request states, no raw SQL detail.

Follow-up command: same isolated environment with `go test -race ./internal/api -run TestAsyncMySQL -count=1`. Result: all 5 API tests passed in 4.656 seconds. The added test checks exact binary client-key identity, AES-GCM encrypted SQL snapshots with frozen credentials/model, and absence of credentials/owner/fence/snapshot fields in the history projection. macOS linker emitted an `LC_DYSYMTAB` warning; exit status was zero and no race report occurred. This does not replace the explicit business-invariant assertions.

The reviewer also ran 15 repository/migration/attachment tests against real MySQL after fixes; these are implementation validation, not independent review of the reviewer's own code. Their independent review is recorded elsewhere.

## Legacy Migration and Admission Addendum

Independently reviewed the design-review agent's `legacy_attachments.go`, its tests, `cmd/migrate-attachments/main.go`, and `docs/roadmap/legacy-attachment-migration.md`. Two P2 findings were fixed and re-reviewed:

- Opening an unexpected FIFO before checking regular-file mode could block indefinitely. Rooted nonblocking open plus regular-file verification now rejects it, with a FIFO regression test.
- Ignoring JSONL encoder errors could continue committing copies after losing the operator's mapping report. Errors now abort further rows and return failure; any already committed row is verified by idempotent rerun.

Personally executed `go test ./internal/execution ./cmd/migrate-attachments -run 'TestLegacySource|TestMySQLLegacy' -count=1` with the isolated MySQL DSN: 3 tests passed (0.742 seconds), migration command compiled. Static review confirms directory/symlink bounds, two content-hash reads, locked SQL metadata recheck, serialized storage quota, atomic object/mapping, original file preservation, preserved expiry, namespace declaration and fail-closed missing mapped objects. No remaining P0/P1/P2 found in this migration scope.

Also independently reviewed the design-review agent's `BindAdmission`, `checkAdmission`, startup wiring and four-dimension regression. Resolved admission defaults are deployment-bound, runtime drift rejects new acceptance, and existing-key replay remains before policy validation. No P0/P1/P2 found in that change.

Follow-up `BindProviderRouting` was implemented by this report's author and independently reviewed by the design-review agent, not self-approved: deterministic startup JSON binds provider keys to endpoints/account quota groups before starting workers or exposing routes; model defaults and credentials are excluded. The reviewer reported no remaining P0/P1/P2. A final current-code test run of `TestMySQLDeploymentPolicy|TestMySQLAdmissionPolicy|TestAsyncProcess` passed (`internal/execution` 1.228 seconds; `internal/api` 14.458 seconds).

The no-skip full-suite run subsequently exposed two legacy-suite issues: an old auth test expected a revoked SQL token to remain valid from Redis, and the legacy worker pubsub listener returned before subscription acknowledgement. The auth test now asserts rejection while separately proving the stale cache entry still exists; worker startup synchronously receives the subscription acknowledgement under a bounded context before returning and closes its subscription on failure/exit. The frontend agent independently reviewed these fixes and found no remaining P0/P1/P2. Their actual full-suite regression outcome belongs to the final harness record; this review does not infer it from static inspection.

## OS-Process Fault Validation

Personally executed:

```sh
GIN_MODE=release UNICHATGO_TEST_PROCESS=1 \
UNICHATGO_TEST_MYSQL_DSN='root:<isolated-test-password>@tcp(127.0.0.1:13316)/unichatgo_async_test?parseTime=true&multiStatements=true' \
UNICHATGO_TEST_REDIS_ADDR='127.0.0.1:16386' \
go test ./internal/api -run TestAsyncProcess -count=1 -v -timeout=180s
```

Result: `TestAsyncProcessIsolationRecoveryAndAttachments` passed in 18.63 seconds (package 20.309 seconds). It builds and launches the actual backend binary as two API and two worker OS processes, with separate working/upload directories, a unique MySQL schema and Redis logical DB 14. HTTP is a local controlled mock, not a paid endpoint.

- API A uploads shared text, API B accepts it, an independent worker includes those exact bytes in its HTTP input without shared upload disks.
- After 202 and confirmed provider entry, API B is SIGKILLed. Worker completes; a replacement API restores the answer and same-key replay; no duplicate call.
- With workers stopped, an accepted SQL outbox intent survives Redis stream deletion and is processed when workers restart.
- After provider entry, both worker processes are SIGKILLed; the test explicitly advances the SQL lease to expired rather than sleeping. Replacement recovery records uncertain, later acceptance is blocked, history shows uncertain, and the mock observes no retry.
- Remaining test-owned processes terminate gracefully. Exact total is three upstream mock HTTP calls for three accepted scenarios.

Channels establish call-entry/order assertions; bounded polling only observes durable readiness/status. Test-created processes and schema are cleaned up, not production services. This harness is supplementary evidence, not a durability guarantee for arbitrary SQL/Redis disk failures.

Independent frontend-agent test review found two P2 failure-path cleanup defects: a fatal assertion could leave the in-process barrier/provider running until task timeout; excessive unexpected calls could block the process mock's observer channel. Both were fixed with once-guarded release plus bounded runtime drain and a context-aware/nonblocking observer send, then independently re-reviewed with no remaining P0/P1/P2. After these fixes, `go test -race ./internal/api -run 'TestAsyncProcess|TestAsyncMySQL' -count=1 -timeout=180s` passed (20.717 seconds). Processes have separate local working/upload directories on one physical host, not separately provisioned machines or physically isolated disks; the test proves no dependency on the API's upload directory, not a network cross-host deployment.

## Remaining Validation Boundaries

The separately listed in-process API tests alone do not establish OS crash behavior; the opt-in subprocess harness supplies that evidence. Redis DEL tests do not establish disk-loss/AOF guarantees. No production failover, remote push, production migration, or paid-model load test was performed. Full SDK/tool-agent feature parity is not inferred from the new single-call HTTP path. The completed-turn attachment-retention/context policy and operator key/policy migration procedure must be made explicit in runbooks; changing a fingerprint without draining compatible workers is not a supported rolling migration.
