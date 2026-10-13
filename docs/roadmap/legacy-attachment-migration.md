# Legacy attachment copy and verification

Date: 2026-10-12. This is an additive source-host migration command, not a production execution record. No real user files or production databases were migrated by this task.

## Preconditions and namespace

- Drain and stop all legacy API/worker processes and legacy temporary-file cleaners first. Disable new acceptance during cutover. Source files must remain immutable during the copy/verification window.
- Apply the reviewed additive async schema separately. The command checks schema compatibility but never creates or drops tables. Point its configuration only at the intended isolated/staging database until deployment approval is obtained.
- Run on the original host with access to its legacy files. Pass `--root` explicitly to allowlist the upload tree; `--source-base` is the original backend working directory used to resolve relative `stored_path` values. Absolute stored paths must also be inside the chosen root.
- Old `temp_files.id` and new `execution_attachments.id` are different namespaces. A JSONL report gives their mapping. New async requests with attachments require `attachment_namespace: "shared-v1"`; raw legacy IDs never fall back or silently select a new object with the same number. Old pending requests require history/operator reconciliation, not rewriting their IDs or payload under the old client ID.

## Dry Run First

Example paths are placeholders for an operator-controlled staging configuration and source tree; inspect them before running. Never put database passwords on the command line or in the report.

```sh
cd /Users/moyu114/Documents/agent_unity/UnichatGo/backend
go run ./cmd/migrate-attachments \
  --config /absolute/path/to/staging-config.json \
  --root /absolute/path/to/old-backend/data/uploads \
  --source-base /absolute/path/to/old-backend \
  --timeout 10m > /absolute/path/to/private/dry-run.jsonl
```

Default is dry-run. To write verified copies use the identical command plus `--apply`, directing output to a new private report. `--id <legacy-id>` restricts a run to one selected row. Rerunning `--apply` verifies the existing mapping/content; it does not create a second object, overwrite changed files, or silently recreate a deleted mapped object.

## Verification and failure semantics

Each active, unexpired row is bounded to 8 MiB. The command reads through Go's rooted filesystem API, rejects escaping symlinks and nonregular files, verifies metadata/size, hashes bytes, and rechecks source contents before transaction commit. In one SQL transaction it validates current ownership/metadata and storage quota, inserts shared bytes and the old-to-new mapping, and verifies the stored object hash. Original creation/expiry metadata is preserved; copying does not extend retention.

Report states are `would_copy`, `copied`, `verified`, `expired`, `inactive`, or `error`. Missing, changed, corrupt, out-of-root or vanished mapped objects are explicit errors; inspect and resolve rather than guessing paths or overwriting mappings. The command exits nonzero on unresolved rows, timeout or report-write failure. A committed row whose report write failed remains safely discoverable by rerun.

`executable: false` means the bytes were preserved but the current bounded provider adapter does not accept that format (for example PDF/office documents or animated images). It is not proof of full attachment feature parity. Such use cases must remain blocked from rollout until a reviewed parser/tool path exists, or a separately authorized product scope change is made.

Neither the source file nor the `temp_files` record is modified or deleted. A mapped shared object retains ordinary expiry/cleanup policy; its mapping remains to prevent silent recreation after cleanup. Remove source data only through a separately authorized, verified retention cleanup. The tool does not provide a live dual-write bridge and does not make an old binary safe after async cutover.

## Local verification

The test suite generates isolated users/files, verifies dry-run has no object writes, copy/rerun produces one mapping/object, another repository instance reads shared bytes, expiry is preserved, changed/missing sources fail, a vanished mapped object does not get recreated, escaping symlinks and FIFO sources are rejected. Real MySQL is required for transaction tests; without `UNICHATGO_TEST_MYSQL_DSN` those tests explicitly skip.

```sh
go test ./internal/execution -run 'TestLegacySource|TestMySQLLegacy' -count=1 -v
go test ./cmd/migrate-attachments
```

The environment variable must reference an isolated schema, not production. Runtime results and independent reviewer disposition are recorded in the task's implementation checkpoint and review records.
