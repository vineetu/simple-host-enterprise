#!/bin/sh
# Runs go test ./... with MIGRATE_TEST_DSN set (make test-db and CI both call
# this) and fails if any DB-backed test skipped anyway: a skip there means the
# database wiring broke, and a green run that tested nothing is worse than red.
set -u
[ -n "${MIGRATE_TEST_DSN:-}" ] || { echo "MIGRATE_TEST_DSN must be set" >&2; exit 1; }
log=$(mktemp)
trap 'rm -f "$log"' EXIT
go test -count=1 -v ./... >"$log" 2>&1
rc=$?
grep -E '^(ok|FAIL|panic:)' "$log"
if [ "$rc" -ne 0 ]; then
	grep -B2 -A20 -- '--- FAIL' "$log" | head -200
	exit "$rc"
fi
skipped=$(grep -c 'MIGRATE_TEST_DSN not set' "$log")
if [ "$skipped" -ne 0 ]; then
	grep -B1 'MIGRATE_TEST_DSN not set' "$log"
	echo "$skipped DB-backed tests skipped with MIGRATE_TEST_DSN set" >&2
	exit 1
fi
# The one test that proves the database was reached at all.
grep -q -- '^--- PASS: TestApplyAgainstPostgres' "$log" || { echo "TestApplyAgainstPostgres did not pass" >&2; exit 1; }
echo "all DB-backed tests ran ($(grep -c -- '^=== RUN' "$log") tests run)"
