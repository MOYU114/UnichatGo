#!/usr/bin/env bash
# Run only against explicitly acknowledged, disposable, loopback test services.
set -euo pipefail
umask 077

fail() { printf 'Verification refused: %s\n' "$1" >&2; exit 2; }
[[ "${UNICHATGO_TEST_ISOLATED:-}" == "yes" ]] || fail 'set UNICHATGO_TEST_ISOLATED=yes after checking both services contain only isolated test data'
[[ -n "${UNICHATGO_TEST_MYSQL_DSN:-}" ]] || fail 'UNICHATGO_TEST_MYSQL_DSN is required; integration skips are not acceptance'
[[ -n "${UNICHATGO_TEST_REDIS_ADDR:-}" ]] || fail 'UNICHATGO_TEST_REDIS_ADDR is required; integration skips are not acceptance'
command -v python3 >/dev/null || fail 'python3 is required to validate targets and redact logs'
command -v go >/dev/null || fail 'go is required'
command -v npm >/dev/null || fail 'npm is required'

# Deliberately narrower than the MySQL driver's complete DSN grammar. No remote
# endpoints, Unix sockets, encoded database names, extra parameters or production DB.
python3 - <<'PY'
import os, re, sys
value = os.environ['UNICHATGO_TEST_MYSQL_DSN']
match = re.fullmatch(r'[^\r\n]*@tcp\((?:127\.0\.0\.1|localhost):([0-9]{1,5})\)/([A-Za-z0-9_]+_test)\?parseTime=true&multiStatements=true', value)
redis = re.fullmatch(r'(?:127\.0\.0\.1|localhost):([0-9]{1,5})', os.environ['UNICHATGO_TEST_REDIS_ADDR'])
if not match or not redis or any(not 1024 <= int(port) <= 65535 for port in (match[1], redis[1])):
    sys.exit('Verification refused: require loopback ports >=1024, database ending _test, and exact DSN parameters parseTime=true&multiStatements=true; targets are not printed')
PY

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Legacy cache tests use different variable names and FLUSHDB; bind them only to
# the acknowledged fixture's reserved logical DB, never an inherited target.
export TEST_REDIS_ADDR="$UNICHATGO_TEST_REDIS_ADDR"
export TEST_REDIS_DB=15
export UNICHATGO_TEST_PROCESS=1
REPORT_ROOT="${UNICHATGO_TEST_REPORT_DIR:-${TMPDIR:-/tmp}/unichatgo-async-reports}"
mkdir -p "$REPORT_ROOT"
RUN_DIR="$(mktemp -d "$REPORT_ROOT/run.XXXXXX")"
REPORT="$RUN_DIR/report.txt"
printf 'UnichatGo isolated async verification\nStarted (Singapore): %s\nTargets: explicitly acknowledged loopback test services (credentials omitted)\n' "$(TZ=Asia/Singapore date '+%Y-%m-%d %H:%M:%S %Z')" > "$REPORT"
printf 'Reports: %s\n' "$RUN_DIR"

# Raw process output is only piped through this redactor, never saved first.
# Test fixtures use synthetic data/tokens. Do not provide live provider credentials.
REDACTOR="$(cat <<'PY'
import os, re, sys
secrets = [os.environ.get(name, '') for name in ('UNICHATGO_TEST_MYSQL_DSN', 'UNICHATGO_TEST_MYSQL_PASSWORD', 'UNICHATGO_APIKEY_KEY', 'GOOGLE_API_KEY', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY')]
dsn = os.environ.get('UNICHATGO_TEST_MYSQL_DSN', '')
credentials = dsn.split('@tcp(', 1)[0]
if ':' in credentials: secrets.append(credentials.split(':', 1)[1])
for line in sys.stdin:
    for secret in secrets:
        if secret: line = line.replace(secret, '[REDACTED]')
    line = re.sub(r'(?i)(authorization|x-api-key|x-goog-api-key)(["\s:=]+)[^\r\n]*', r'\1\2[REDACTED]\n', line)
    sys.stdout.write(line)
PY
)"

# Regression: mask complete Bearer values, not just the authentication scheme.
redaction_probe="$(printf 'Authorization: Bearer probe-secret\n{"x-api-key":"probe-key"}\n' | python3 -c "$REDACTOR")"
[[ "$redaction_probe" != *probe-secret* && "$redaction_probe" != *probe-key* ]] || fail 'credential redaction self-test failed'

run_step() {
  local name="$1" directory="$2"
  shift 2
  printf '\n[%s] %s\n' "$name" "$*" | tee -a "$REPORT"
  local result=0
  (cd "$directory" && "$@") 2>&1 | python3 -c "$REDACTOR" | tee "$RUN_DIR/$name.log" || result=$?
  if [[ "$result" -ne 0 ]]; then
    printf '%s: FAILED (exit %s)\nRemaining steps: NOT EXECUTED\n' "$name" "$result" | tee -a "$REPORT"
    exit "$result"
  fi
  printf '%s: PASSED\n' "$name" | tee -a "$REPORT"
}

[[ -d "$ROOT/frontend/node_modules" ]] || fail 'frontend/node_modules missing; explicitly install locked dependencies before verification'
# Serial package execution avoids shared Redis stream interference across packages.
# -count=1 avoids test cache; -v records any skips; -race is supplementary evidence.
run_step backend "$ROOT/backend" go test -p 1 -race -count=1 -v ./...
if grep -q -- '--- SKIP:' "$RUN_DIR/backend.log"; then
  printf 'Integration suite contains skipped tests: inspect backend.log; verification incomplete.\n' | tee -a "$REPORT"
  exit 1
fi
run_step frontend-tests "$ROOT/frontend" npm test
run_step frontend-build "$ROOT/frontend" npm run build
printf '\nSelected automated checks passed. NOT production approval.\nNot covered: real-browser E2E, multi-host/power-loss persistence, load/capacity budget, production rollout.\nServices/volumes were NOT stopped or removed.\n' | tee -a "$REPORT"
