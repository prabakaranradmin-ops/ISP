#!/usr/bin/env bash
# run_db_tests.sh — run the persistence-layer integration tests against a real
# PostgreSQL.
#
# Brings up a throwaway PostgreSQL, applies every migration with goose, then runs
# the Go integration suite in a container on the same Docker network so it can
# reach the database by name.
#
# Usage:
#   ./scripts/run_db_tests.sh                       # whole suite
#   ./scripts/run_db_tests.sh -run TestWallet       # extra `go test` flags pass through
#   KEEP_DB=1 ./scripts/run_db_tests.sh             # leave the database up afterwards

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MOUNT_ROOT="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

PG_IMAGE="${PG_IMAGE:-postgres:15-alpine}"
GO_IMAGE="${GO_IMAGE:-golang:1.22}"
NETWORK="db_tests_net_$$"
CONTAINER="db_tests_pg_$$"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }

cleanup() {
    if [ "${KEEP_DB:-0}" = "1" ]; then
        printf "${YELLOW}[....]${NC} KEEP_DB=1, leaving %s on network %s\n" "$CONTAINER" "$NETWORK"
        return
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { printf "${RED}docker is required${NC}\n"; exit 1; }

info "starting PostgreSQL ($PG_IMAGE)"
docker network create "$NETWORK" >/dev/null
docker run -d --rm --name "$CONTAINER" --network "$NETWORK" \
    -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=isp_bss_oss \
    "$PG_IMAGE" >/dev/null

READY_TIMEOUT="${READY_TIMEOUT:-120}"
for _ in $(seq 1 "$READY_TIMEOUT"); do
    docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && break
    # A container that died will never become ready; fail fast with its logs
    # rather than burning the whole timeout.
    if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
        printf "${RED}PostgreSQL container exited during startup${NC}\n"
        docker logs "$CONTAINER" 2>&1 | tail -20
        exit 1
    fi
    sleep 1
done
if ! docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1; then
    printf "${RED}PostgreSQL did not become ready within %ss${NC}\n" "$READY_TIMEOUT"
    docker logs "$CONTAINER" 2>&1 | tail -20
    exit 1
fi

DSN="postgres://postgres:postgres@${CONTAINER}:5432/isp_bss_oss?sslmode=disable"

in_go_container() {
    docker run --rm --network "$NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -v "isp_gomodcache:/go/pkg/mod" \
        -v "isp_gobuildcache:/root/.cache/go-build" \
        -e GOFLAGS=-mod=mod \
        -e "TEST_DB_DSN=${DSN}" \
        "$GO_IMAGE" "$@"
}

info "applying migrations"
if ! MIGRATE_OUT=$(in_go_container go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
        -dir ./migrations postgres "$DSN" up 2>&1); then
    printf "${RED}migrations failed${NC}\n"
    echo "$MIGRATE_OUT" | tail -20
    exit 1
fi

info "running persistence integration tests"
# -p 1 runs the packages one at a time rather than in parallel, which is
# mandatory rather than a tuning choice: they share one database and each
# TRUNCATEs the tables it owns to start from a known state. Run
# concurrently, one package empties a table another is mid-assertion on,
# and the failures land on whichever test happened to lose the race.
#
# internal/fup and internal/reporting joined this list when the task queue
# moved off Redis (migration 037): their queue tests used to get isolation
# for free from a per-test in-process miniredis, and now share
# jobqueue_tasks with everything else.
# -timeout 20m, well above go test's 600s default. ./internal/db alone runs
# 430-470s against a real PostgreSQL on an unloaded machine, which is close
# enough to the default that a busy one tips over it — and a timeout is
# reported as a bare package FAIL with no failing test named, which reads as
# a mystery rather than as "this needed longer".
in_go_container go test -tags=integration -count=1 -p 1 -timeout 20m "$@" \
    ./internal/db/... ./internal/cache/... ./internal/fup/... ./internal/reporting/... \
    ./internal/jobqueue/...
CODE=$?

echo ""
if [ "$CODE" -eq 0 ]; then
    printf "${GREEN}DB TESTS PASS${NC}\n"
else
    printf "${RED}DB TESTS FAIL${NC} (exit %d)\n" "$CODE"
fi
exit "$CODE"
