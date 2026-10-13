# Isolated async verification runbook

This runbook targets synthetic data and mock HTTP providers only. It is not permission to run against production, reuse real user databases, call paid providers, remove existing volumes, or deploy production services.

## Targets and prerequisites

Use Docker Compose, Go (module toolchain), Node/npm and Python 3. Frontend dependencies must already be installed with the lockfile (run `npm ci` from `frontend/` only when dependency installation is authorized). Do not give this environment real model keys or user content.

The dedicated Compose project is `unichatgo-async-isolated`, with MySQL 8.4 and Redis 7.4. Defaults bind loopback ports **13317 / 16387**, avoiding the earlier manually created **13316 / 16386** fixtures. Service names are project-scoped; no fixed container names or host data directory mounts are used. If ports are already occupied, choose other unused loopback ports rather than stopping an unrelated service.

```sh
export UNICHATGO_TEST_MYSQL_PASSWORD='choose-a-throwaway-test-only-password'
export UNICHATGO_TEST_MYSQL_PORT=13317
export UNICHATGO_TEST_REDIS_PORT=16387
# Run from the repository root; starting Docker requires normal tool approval.
docker compose -f docker-compose.async-test.yml up -d --wait
```

Named volumes preserve fixture data between runs. A different password does not reset an already initialized MySQL volume; use the original test password or a deliberately new Compose project, never delete the old volume as an automatic repair. Redis is unauthenticated and binds only loopback; do not expose the fixture publicly.

## Read-only readiness and configuration evidence

Run these exact read-only commands. They prove configuration/readiness, **not persistence under crash or power loss**. Keep their output with the run evidence; do not record a full Compose config containing the test password.

```sh
docker compose -f docker-compose.async-test.yml ps
docker compose -f docker-compose.async-test.yml exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqladmin -h 127.0.0.1 -u root ping'
docker compose -f docker-compose.async-test.yml exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -h 127.0.0.1 -u root -N -e "SELECT VERSION(), @@global.innodb_flush_log_at_trx_commit, @@global.sync_binlog, @@global.max_allowed_packet;"'
docker compose -f docker-compose.async-test.yml exec -T redis redis-cli PING
docker compose -f docker-compose.async-test.yml exec -T redis redis-cli CONFIG GET appendonly appendfsync maxmemory-policy
docker compose -f docker-compose.async-test.yml exec -T redis redis-cli INFO persistence
```

Expected Redis values are `appendonly yes`, `appendfsync everysec`, `maxmemory-policy noeviction`. `everysec` can lose recent signals; the actual application recovery contract comes from SQL evidence/outbox and call-boundary classification, not an AOF flag. MySQL packet limit is 64 MiB to permit bounded shared attachments.

## Automated checks

The verifier requires explicit acknowledgment and explicit targets; it never discovers or chooses a database for you. It permits only loopback ports >=1024 and a database whose name ends in `_test`, with the exact DSN query parameters shown. This is a safety guard, **not a substitute for checking that the server is disposable**. Existing acknowledged test services can be supplied instead of starting Compose.

```sh
export UNICHATGO_TEST_ISOLATED=yes
export UNICHATGO_TEST_MYSQL_DSN="root:${UNICHATGO_TEST_MYSQL_PASSWORD}@tcp(127.0.0.1:${UNICHATGO_TEST_MYSQL_PORT})/unichatgo_async_test?parseTime=true&multiStatements=true"
export UNICHATGO_TEST_REDIS_ADDR="127.0.0.1:${UNICHATGO_TEST_REDIS_PORT}"
# Optional: otherwise a private temp-directory report is used.
# export UNICHATGO_TEST_REPORT_DIR='/absolute/path/to/test-reports'
bash backend/scripts/verify-async.sh
```

The script runs `go test -p 1 -race -count=1 -v ./...`, then `npm test` and `npm run build`. It explicitly enables isolated subprocess tests with `UNICHATGO_TEST_PROCESS=1`; those tests reserve logical Redis DB 14, separate from cache-test DB 15. Package serialization prevents shared Redis-stream tests in separate packages from racing. There is no cache-based pass claim, automatic dependency download command, Compose teardown, volume removal, or model API smoke test.

Run only **one complete harness or targeted integration-test invocation at a time** against a fixture. Package serialization does not coordinate separate shells/agents. Redis DBs 0, 12, 14 and 15 contain shared fixed test stream/cache names; simultaneous invocations can delete each other's signals. Coordinate before starting a run, or provision a separate Redis/MySQL fixture with different ports.

The test suites intentionally create synthetic users and messages, delete test-owned rows, create/drop uniquely named `unichatgo_api_test_*` and `execution_test_*` databases, and delete/rebuild the **test Redis execution stream**. Legacy cache tests additionally **FLUSHDB logical Redis database 15**; the script overrides legacy `TEST_REDIS_ADDR`/`TEST_REDIS_DB` to this acknowledged fixture so inherited variables cannot target another server. Reserve DB 15 for synthetic cache tests. Therefore even a database named `_test` on a real shared/production Redis endpoint is unsafe. Root-equivalent database permissions are needed only inside the isolated server for schema/trigger fault injection. Failed-process cleanup can leave synthetic resources behind; inspect them manually rather than applying broad cleanup.

Reports are created in a fresh private `run.*` directory and contain sanitized command output and pass/fail status. The supplied DSN/password and known credential environment values are redacted before persistence. Avoid `set -x`, do not paste environment dumps, and inspect logs before sharing; arbitrary secret-shaped strings from custom test fixtures cannot be automatically recognized.

A failed step stops the script and marks remaining steps unexecuted. Any skipped Go test makes the run incomplete. A clean automated run still does not imply that all roadmap acceptance checks have passed.

## Shutdown without destructive cleanup

Only when these test services are no longer needed:

```sh
docker compose -f docker-compose.async-test.yml stop
```

The verifier has no teardown trap. It never calls `docker compose down -v`, prunes Docker, drops an externally supplied database, or resets another project. Keep fixture volumes and logs for investigation. Any deliberate deletion is a separate, explicitly authorized action.

## Evidence boundaries and remaining gates

- Repository tests exercise actual MySQL transactions, unique constraints, locks and shared-object rows; barrier tests do not rely on sleeping to infer ordering. Race detection supplements business invariants, not replaces them.
- Mock-provider adapter tests verify bounded request/response handling and one POST per attempt. They do not prove vendor billing exactly-once or real vendor model compatibility.
- The opt-in subprocess test has actually exercised two API and two Worker OS processes with distinct working directories on one host: A upload/B acceptance, API SIGKILL/restart, post-call Worker SIGKILL followed by SQL lease-expiry injection, and Redis stream deletion/rebuild. It does not prove separate physical hosts/disks, natural lease timing under host failure, power-loss persistence, or Redis durable restart. Those remain separate drills. Consult the latest run report because the presence of this test alone is not a fresh pass.
- This script does not test real-browser rendering/refresh/offline behavior. Frontend node store/parser tests and build have their own narrower evidence.
- No load benchmark or P95/P99/capacity tuning is included. Do not mark M6 capacity budgets or M7 optimization complete without measured mock-provider workload results.
- Legacy attachment migration, tools/summaries feature parity, rollout write gates, compatible rollback and production readiness each require their own evidence. Unsupported paths must remain explicit failures, not silent fallback.
- Production deploy, real-data migration, remote push and paid provider usage remain unperformed and outside this automation's authorization.

The earlier executed local evidence is recorded in `../design-review/review-004.md`; future runs should append their actual command, result, skips and relevant configuration outputs rather than changing unchecked roadmap rows based on this runbook alone.
