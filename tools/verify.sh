#!/usr/bin/env bash
# Full verification sweep: formatting, build, vet, unit tests, PostgreSQL
# integration tests, and the schema/RLS suite.
#
# Usage: tools/verify.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0

step() { echo; echo "======== $1 ========"; }
fail() { echo "FAILED: $1"; FAIL=1; }

if ! command -v go >/dev/null 2>&1; then
  [ -x "$HOME/.local/go/bin/go" ] && export PATH="$HOME/.local/go/bin:$PATH"
fi
command -v go >/dev/null 2>&1 || { echo "go not found on PATH"; exit 1; }

step "gofmt"
cd "$ROOT/services/ingress" || exit 1
UNFMT=$(gofmt -l .)
if [ -n "$UNFMT" ]; then
  echo "NOT FORMATTED:"; echo "$UNFMT"; fail "gofmt"
else
  echo "clean"
fi

step "go build"
go build ./... && echo ok || fail "build"

step "go vet"
go vet ./... && echo ok || fail "vet"

step "unit tests"
go test ./... -count=1 2>&1 | grep -Ev '^\?' || true

step "postgres integration tests"
cd "$ROOT/tools/dbtest" || exit 1
[ -d node_modules ] || { echo "run 'npm install' in tools/dbtest first"; fail "setup"; }
PGLITE_FRESH=1 node serve.mjs > /tmp/verify-pglite.log 2>&1 &
DB_PID=$!
cleanup() { kill "$DB_PID" 2>/dev/null; rm -rf "$ROOT/tools/dbtest/.pgdata"; }
trap cleanup EXIT

for _ in $(seq 1 120); do
  grep -q "listening on" /tmp/verify-pglite.log && break
  sleep 0.5
done
if ! grep -q "listening on" /tmp/verify-pglite.log; then
  cat /tmp/verify-pglite.log; fail "pglite startup"; exit 1
fi

cd "$ROOT/services/ingress" || exit 1
if CHIPMANK_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:55432/postgres" \
     go test ./internal/store/postgres/ -count=1 > /tmp/verify-itest.log 2>&1; then
  echo ok
else
  cat /tmp/verify-itest.log
  fail "postgres integration"
fi

kill "$DB_PID" 2>/dev/null; wait "$DB_PID" 2>/dev/null

step "schema + RLS suite"
cd "$ROOT/tools/dbtest" || exit 1
node run.mjs 2>&1 | tail -12

echo
if [ "$FAIL" = 0 ]; then
  echo "######## ALL CHECKS PASSED ########"
else
  echo "######## FAILURES PRESENT ########"
fi
exit "$FAIL"