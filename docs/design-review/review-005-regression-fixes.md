# Review 005: user-reported regressions and output truncation

Date: 2026-10-12. Scope: the user's three reproducible findings and the explicit-output-limit design risk, plus directly related regressions discovered while independently reviewing those fixes. **Release status remains NO-GO**; this work does not complete the outstanding M3-M6 acceptance gates.

## Findings, changes and independent review

| ID | Severity / original location | Trigger and impact | Fix / proof |
| --- | --- | --- | --- |
| U5-01 | P1 `frontend/src/store/session.js`, `loadMessages` | Default legacy empty history returned null; first send aborted before SSE. | Backend session history now returns `[]`. Frontend narrowly accepts explicit null only in confirmed legacy mode; malformed/async history remains rejected. Actual store regression asserts exactly one first-send stream call. Backend legacy HTTP test asserts `[]`, done event and two message rows. |
| U5-02 | P2 `backend/internal/execution/provider.go`, `EstimateTokens` | Per-pixel reservation exceeded default TPM for ordinary 512px image, so accepted tasks could never execute. | Keep TPM 200000 and all quota dimensions; reserve static images by bounded 512px tiles with generous overhead. Add transactional budget preflight after winning-key lookup, sharing effective SQL context with Worker. Reject known-impossible input with 422 before durable acceptance writes. A real MySQL/Redis/HTTP mock test proves the 512px image actually completes under default limits; 2048px boundary fits, two such images are rejected without state writes. |
| U5-03 | P2 `frontend/src/store/session.js`, `refreshHistory` | Capabilities succeeded but initial list failed; refresh never retried list. | Independent `sessionsInitialized`, single-flight initialization, retry list on refresh until successful, reset on user change; successful empty list clears error without repeated polling. Deferred promise tests assert retry counts and recovered history. |
| D5-01 | Design clarification `parseProviderReply` / ADR-004 | Explicit length termination was treated like unknown execution, unnecessarily isolating session. | Complete typed `length` / `max_tokens` / `MAX_TOKENS` responses become `failed / provider_output_truncated`. No partial success, retry or enlarged budget. Missing/malformed/incomplete transport remains uncertain. All three adapters tested; real MySQL Runtime tests prove failure persists, following queued task executes with failed turn excluded, no extra Provider call. |

The image formula is `32768 + 8192 * ceil(width/512) * ceil(height/512)`, plus existing UTF-8/framing/default maximum output 3000. Images remain static PNG/JPEG/GIF/WebP <=2048x2048. API and Worker use the same resolved default quota configuration. The current request's budget callback is local-only and executes under user/session locks inside `AcceptChecked`; it does not make a network request while holding SQL locks. Earlier queued responses are unknowable, so Worker checks/reserves the actual execution context again. This is conservative policy, not a universal token bound for arbitrary upstreams. The reviewer verified the OpenAI image accounting reference: [Images and vision](https://developers.openai.com/api/docs/guides/images-vision), checked 2026-10-12. Other provider/custom accounting still requires deployment verification.

## Additional findings from independent review

- P2 preflight race in the initial proposed fix: a separate early lookup followed by budget rejection could reject a concurrently accepted same key. Before implementation, reviewer requested replay/budget checks inside the owned-session transaction. Implemented `AcceptChecked`; concurrent same-key tests assert one check invocation and identical receipts; subsequent replay bypasses a deliberately failing callback.
- P2 legacy successful reconnect left the old history error displayed. Fixed; recovered current-user/current-session history clears the warning.
- P2 delayed list/history/resume responses could resurrect a successfully deleted session. Fixed with current-user local deletion tombstones and existing response-generation guards; deterministic races cover each response path.
- P2 failed list initialization plus New Session/navigation (including concurrent creation) could clear the only recovery prompt while sending remained blocked. Fixed by preserving the list-init error/recovery path independently of history success, with regressions for sequential and concurrent cases.

Independent review separation:

- `fix_frontend_regressions` agent implemented store/tests; `fix_truncation` agent reviewed them, produced failing deferred reproductions, then independently reran the fixed tests and build.
- `fix_truncation` agent implemented parser/mock-provider/SQL truncation tests. `review_image_plan` agent independently reviewed those branches and tests; primary agent also read and checked the runtime/state behavior.
- Primary agent implemented backend empty-list, shared quota defaults, image estimator, atomic preflight, budget/legacy regression tests and docs. `review_image_plan` independently reviewed them and corrected the proposed preflight race before implementation; no remaining scoped P0/P1/P2 findings after fixes.
- Scope includes untracked new tests, current implementations and documentation, not only tracked Git diff. A review disposition is not a substitute for executed tests.

## Actual verification

Executed only with mock/local Providers and the existing explicitly isolated MySQL/Redis fixtures; no production data, paid models or remote push.

1. Parent targeted API/execution run: legacy empty history/first send; 512px image completion; over-budget transactional rollback; legacy context budget; concurrent checked acceptance; image boundaries. PASS: API 2.334s, execution 0.924s; no skips.
2. Truncation agent targeted `-race` suite: all three adapters, malformed/missing/type-invalid markers, complete JSON within interrupted HTTP body, and real MySQL Runtime failure -> following completion. PASS 2.383s; no skips. This is Runtime/SQL terminal-gate verification, not a new Redis PEL/OS-crash claim.
3. Frontend independent reviewer: `npm test` 53/53, five additional deterministic temporary repros 5/5, `npm run build` passed. Real store with mocked browser/network boundaries; not real-browser E2E.
4. Image reviewer: shared default configuration and pure image/parser regressions under `-race` passed (config 2.073s, execution 2.614s), no shared SQL fixture used by that run.
5. Parent `go vet ./...`: no diagnostics. Real MySQL targeted `-race` acceptance-check/context-growth/image-boundary regressions PASS 2.695s. The growth test deliberately injects a large previous SQL reply to prove the guard; it is not a measured real-provider output scenario. `git diff --check` passes.
6. Full serialized no-skip harness `run.sjEIBU`, started 2026-10-12 15:01:28 Singapore time, PASSED. Backend `go test -p 1 -race -count=1 -v ./...`: 76 top-level tests / 137 PASS including subtests, zero failures/skips. Frontend 53/53 with zero skips; production build passed (1.97s). API 27.349s, auth 2.015s, config 1.394s, execution 7.274s, assistant 2.534s, worker 2.856s. Two-API/two-Worker process scenario 21.39s, default image execution 0.69s, three-provider SQL truncation scenario 0.35s. Primary agent read the report and counted the actual test output; earlier 38/38 frontend results and runs without integration environment are historical, not evidence for these fixes.

Executed from the repository root (DSN omitted from this document):

```sh
UNICHATGO_TEST_ISOLATED=yes \
UNICHATGO_TEST_MYSQL_DSN='<isolated-test-dsn>' \
UNICHATGO_TEST_REDIS_ADDR=127.0.0.1:16386 \
bash backend/scripts/verify-async.sh
```

Local report: `/private/var/folders/0r/b3fs4y6d35jdkqbhsb2htwt40000gn/T/unichatgo-async-reports/run.sjEIBU/report.txt`; sanitized combined output: `/private/tmp/unichatgo-review-fixes-verification.log`. Temporary reports are local evidence, not committed artifacts or permanent storage. The reproducible script and this result record remain in the repository. Integration fixtures were not stopped or removed.

Nonblocking warnings remain: existing Vite bundle-size warning and macOS test linker `LC_DYSYMTAB` warning. No claim of capacity or production readiness follows from these tests.
