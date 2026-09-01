#!/usr/bin/env bash
# remote_migrate.sh — runs ON the GCE instance, with PostgreSQL already up
# and the application containers not yet started.
#
# Applies migrations as the postgres superuser and then sets the bss_app
# role's password. Both halves are required and neither can be skipped:
#
#   - Migrations must run as superuser. Migration 019 grants CONNECT on the
#     database and revokes privileges on goose_db_version from bss_app;
#     running it as bss_app fails on the grant and would break goose's own
#     bookkeeping partway through.
#   - Migration 019 creates bss_app with LOGIN but deliberately no password,
#     because that file is committed to git. The password is set here, from
#     .env, every run — ALTER ROLE is idempotent and simply asserts the
#     current value.

set -euo pipefail

[ -f .env ] || { echo "no .env — run remote_bootstrap.sh first" >&2; exit 1; }
# shellcheck disable=SC1091
set -a; source .env; set +a

: "${DB_SECURE_PASSWORD:?missing from .env}"
: "${APP_DB_PASSWORD:?missing from .env}"

COMPOSE_NETWORK="$(sudo docker network ls --format '{{.Name}}' | grep -m1 'bss_internal' || true)"
[ -n "$COMPOSE_NETWORK" ] || { echo "cannot find the bss_internal docker network — is the stack up?" >&2; exit 1; }

DSN="postgres://postgres:${DB_SECURE_PASSWORD}@postgres_primary:5432/isp_bss_oss?sslmode=disable"

# goose runs in a throwaway golang container on the compose network rather
# than being installed on the host: the VM needs no Go toolchain, and the
# version is pinned so a deploy months from now applies migrations with the
# same tool that was tested against them.
echo "applying migrations"
sudo docker run --rm --network "$COMPOSE_NETWORK" \
    -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-mod=mod \
    golang:1.23 \
    go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
        -dir ./migrations postgres "$DSN" up

echo "setting the bss_app role password"
sudo docker compose exec -T -e PGPASSWORD="${DB_SECURE_PASSWORD}" postgres_primary \
    psql -U postgres -d isp_bss_oss -v ON_ERROR_STOP=1 \
    -c "ALTER ROLE bss_app WITH PASSWORD '${APP_DB_PASSWORD}';" >/dev/null

# Confirms what the application will actually experience, rather than
# trusting that the two steps above lined up. A migration that applied
# cleanly but left bss_app unable to connect is a stack that starts and then
# fails every request.
echo "verifying bss_app can connect and read"
sudo docker compose exec -T -e PGPASSWORD="${APP_DB_PASSWORD}" postgres_primary \
    psql -U bss_app -h 127.0.0.1 -d isp_bss_oss -v ON_ERROR_STOP=1 \
    -c "SELECT count(*) FROM chart_of_accounts;" >/dev/null

echo "migrations applied and bss_app verified"
