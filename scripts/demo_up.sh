#!/usr/bin/env bash
# demo_up.sh — bring up the real docker-compose.yml stack for a local demo.
#
# Unlike smoke_test.sh (which builds ad hoc containers to verify wiring) and
# run_nfr_tests.sh (which measures performance), this runs the actual stack a
# user would deploy: the same docker-compose.yml, the same Dockerfiles, no
# shortcuts. It only adds what docker-compose.yml itself does not do —
# generate the AES key file, run migrations, seed demo data — because compose
# has no clean way to express "run once before the app starts" for that.
#
# Usage:
#   ./scripts/demo_up.sh              # start everything, print demo instructions
#   ./scripts/demo_up.sh down         # tear down (keeps volumes)
#   ./scripts/demo_up.sh down -v      # tear down and delete volumes (fresh next time)

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# Compose derives its network name from the project name, which by default is
# the checkout directory lowercased — fragile to depend on (it changes if the
# folder is renamed or re-cloned with different casing, as happened testing
# this script: "ISP" the folder vs. "isp" the network Compose actually created).
# Pinning it here makes "isp_bss_demo_bss_internal" exact and predictable.
export COMPOSE_PROJECT_NAME=isp_bss_demo
COMPOSE_NETWORK="${COMPOSE_PROJECT_NAME}_bss_internal"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
head1() { printf "\n${BOLD}== %s ==${NC}\n" "$1"; }

if [ "${1:-}" = "down" ]; then
    shift
    info "stopping the stack"
    docker compose down "$@"
    exit 0
fi

command -v docker >/dev/null 2>&1 || { fail "docker is required"; exit 1; }

# ── .env ──────────────────────────────────────────────────────────────────────

if [ ! -f .env ]; then
    info "no .env found — copying .env.example (demo secrets, not for production)"
    cp .env.example .env
fi
# An .env from before a given feature was added won't have that feature's key
# yet — backfill it rather than requiring everyone to recreate their .env by
# hand each time the app grows a new required secret.
ensure_env_secret() {
    local key="$1"
    if ! grep -q "^${key}=" .env; then
        info "${key} missing from .env — generating one"
        # 48 raw bytes -> 64 base64 chars before stripping. Stripping '=+/'
        # (tr -d, not just trimming padding) can remove a handful of chars
        # unpredictably, so start with enough headroom that the result is
        # reliably >=32 chars (config.go's minSecretLength) even in the
        # worst case, then truncate to a fixed, deterministic length rather
        # than trust however many survived stripping.
        local value
        value=$(docker run --rm golang:1.22 sh -c "head -c 48 /dev/urandom | base64 -w0" | tr -d '=+/\n' | head -c 40)
        printf '\n%s=%s\n' "$key" "$value" >> .env
    fi
}
ensure_env_secret APP_DB_PASSWORD          # migration 019, the bss_app least-privilege role
ensure_env_secret RADIUS_VERIFIER_SECRET   # the RADIUS fast-verifier cache (NFR-PERF-001)

# shellcheck disable=SC1091
set -a; source .env; set +a

# ── AES key store ────────────────────────────────────────────────────────────
# config/keys/*.json is gitignored (it's key material); generate one locally
# if this is a fresh checkout. Idempotent — an existing key file is left alone
# so a restart does not re-key data encrypted under the old one.

mkdir -p config/keys
if [ ! -f config/keys/aes_keys.json ]; then
    info "generating a local AES key store"
    KEY_B64=$(docker run --rm golang:1.22 sh -c "head -c 32 /dev/urandom | base64 -w0")
    printf '{"active_version":"v1","keys":{"v1":"%s"}}\n' "$KEY_B64" > config/keys/aes_keys.json
    pass "config/keys/aes_keys.json created"
else
    info "config/keys/aes_keys.json already exists, leaving it alone"
fi

# ── Infrastructure ───────────────────────────────────────────────────────────

head1 "Starting infrastructure"
docker compose up -d postgres_primary gotenberg_engine

info "waiting for PostgreSQL"
for _ in $(seq 1 60); do
    docker compose exec -T postgres_primary pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && break
    sleep 1
done
if ! docker compose exec -T postgres_primary pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1; then
    fail "PostgreSQL did not become ready"
    exit 1
fi
pass "PostgreSQL ready"

# ── Migrations + seed ────────────────────────────────────────────────────────

head1 "Migrating and seeding"
DSN="postgres://postgres:${DB_SECURE_PASSWORD}@postgres_primary:5432/isp_bss_oss?sslmode=disable"

if ! docker run --rm --network "$COMPOSE_NETWORK" \
        -v "$REPO_ROOT:/src" -w /src \
        -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod golang:1.22 \
        go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 -dir ./migrations postgres "$DSN" up; then
    fail "migrations failed"
    exit 1
fi
pass "migrations applied"

# Migration 019 creates the bss_app role without a password (the migration
# file is committed to git, so it can't contain one) — set it here from
# APP_DB_PASSWORD instead, as the postgres superuser, every run. Idempotent:
# ALTER ROLE ... PASSWORD always succeeds and simply sets it to the current
# value, whether that's unchanged or new.
if ! docker compose exec -T -e PGPASSWORD="${DB_SECURE_PASSWORD}" postgres_primary \
        psql -U postgres -d isp_bss_oss -c "ALTER ROLE bss_app WITH PASSWORD '${APP_DB_PASSWORD}';" >/tmp/app_role_out.txt 2>&1; then
    fail "setting bss_app password failed"
    cat /tmp/app_role_out.txt
    exit 1
fi
pass "bss_app role password set"

if ! docker compose exec -T -e PGPASSWORD="${DB_SECURE_PASSWORD}" postgres_primary \
        psql -U postgres -d isp_bss_oss < scripts/seed_local.sql >/tmp/seed_out.txt 2>&1; then
    fail "seeding failed"
    cat /tmp/seed_out.txt
    exit 1
fi
pass "demo data seeded (2 subscribers, 3 plans, 1 invoice, 4 past sessions)"

# ── Live session for the dashboard ───────────────────────────────────────────
#
# The dashboard's "Live Usage" panel reads the live_sessions table (migration
# 036), so seeding subscribers alone leaves it showing "you appear to be
# offline". A real row only appears when a router actually authenticates,
# which a demo box has no way to do — so one is written here directly.
#
# This is demo scaffolding, not a fixture the application depends on: nothing
# reads it except that panel and the health endpoint. The usage figure is
# deliberately set to 67% of the plan quota — high enough that the panel shows
# a meaningful bar, below the 80% mark that would trigger a genuine FUP
# warning and hand testers a notification nobody sent on purpose.
#
# updated_at is set to now() rather than backdated with started_at: reads
# filter on it against SessionTTL (30 minutes), so a row written with a
# three-hour-old updated_at would be swept as stale and the panel would show
# offline anyway. started_at is what makes the session look established.
SUB_ID=$(docker compose exec -T -e PGPASSWORD="${DB_SECURE_PASSWORD}" postgres_primary \
    psql -U postgres -d isp_bss_oss -tAc \
    "SELECT id FROM subscribers WHERE username='test_user';" 2>/dev/null | tr -d '[:space:]')

if [ -n "$SUB_ID" ]; then
    if docker compose exec -T -e PGPASSWORD="${DB_SECURE_PASSWORD}" postgres_primary \
            psql -U postgres -d isp_bss_oss -v ON_ERROR_STOP=1 -c "
                INSERT INTO live_sessions (subscriber_id, session_id, nas_ip, assigned_ip,
                                           bytes_in, bytes_out, bytes_total,
                                           speed_profile, fup_throttled, started_at, updated_at)
                VALUES (${SUB_ID}, 'demo-live-001', '10.10.0.1', '100.64.0.7',
                        1800000000000, 600000000000, 3543348019200,
                        '100M/100M', false, now() - interval '3 hours', now())
                ON CONFLICT (subscriber_id) DO UPDATE SET
                    session_id = EXCLUDED.session_id,
                    bytes_in = EXCLUDED.bytes_in, bytes_out = EXCLUDED.bytes_out,
                    bytes_total = EXCLUDED.bytes_total,
                    started_at = EXCLUDED.started_at, updated_at = now();" >/dev/null 2>&1; then
        pass "live session seeded for test_user (dashboard shows 67% of quota used)"
    else
        # Not fatal: everything except one dashboard panel still works.
        info "could not seed the live session — the dashboard will show the offline state"
    fi
else
    info "test_user not found; skipping the live session"
fi

# ── Application services ─────────────────────────────────────────────────────

head1 "Starting the API and RADIUS daemon"
docker compose up -d --build aaa_core_daemon api_service

info "waiting for the API to report ready"
for _ in $(seq 1 60); do
    if docker compose exec -T api_service wget -q -O /dev/null http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
        pass "API ready"
        break
    fi
    sleep 2
done

# api_service itself is plain HTTP and not reachable from the host (SecD
# §9.7) — reverse_proxy is what terminates TLS and is the only externally
# reachable entry point, so start it after api_service is confirmed ready.
head1 "Starting the reverse proxy"
docker compose up -d reverse_proxy

info "waiting for the reverse proxy to answer over TLS"
proxy_ready=false
for _ in $(seq 1 60); do
    # Shell redirection, not curl's own -o flag: with MSYS2_ARG_CONV_EXCL='*'
    # (set above, required for the docker commands in this script), MSYS's
    # /dev/null -> NUL translation for curl's argv is disabled, so `curl -o
    # /dev/null` fails to open it (exit 23) even though the request succeeds.
    if curl -sk "https://localhost/readyz" >/dev/null; then
        pass "reverse proxy ready"
        proxy_ready=true
        break
    fi
    sleep 1
done
if [ "$proxy_ready" = false ]; then
    fail "reverse proxy did not answer https://localhost/readyz in time — check: docker compose logs reverse_proxy"
fi

# ── Demo script ──────────────────────────────────────────────────────────────

TOKEN=$(docker run --rm --network "$COMPOSE_NETWORK" \
    -v "$REPO_ROOT:/src" -w /src \
    -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
    -e GOFLAGS=-mod=mod golang:1.22 \
    go run ./scripts/gen_jwt -secret "${JWT_SECRET}" -role billing_admin -ttl 12h 2>/dev/null | tr -d '\r\n')

head1 "Demo is up"
cat <<EOF

  API           https://localhost  (self-signed local cert — use curl -k)
  Metrics       http://localhost:9101/metrics
  RADIUS        localhost:1812/udp (secret: ${RADIUS_SECRET})
  PostgreSQL    localhost:5432 (postgres / ${DB_SECURE_PASSWORD})

  Seeded subscribers (password for both: testpassword):
    test_user        — active,          wallet ₹799.00, plan TN_Super_100M
    suspended_user    — hard_suspended,  wallet ₹0.00,   plan TN_Basic_50M

  ── Portal (subscriber-facing) ──────────────────────────────────────────────
  curl -sk -X POST https://localhost/portal/login \\
       -H 'Content-Type: application/json' \\
       -d '{"username":"test_user","password":"testpassword"}'
  # -> {"token":"..."}; use it as: -H "Authorization: Bearer <token>"
  # then: GET /portal/dashboard, /portal/notifications, /portal/tickets

  ── Admin API (staff-facing) ────────────────────────────────────────────────
  # A 12-hour billing_admin token has been minted for you:
  export TOKEN="${TOKEN}"

  curl -sk https://localhost/api/v1/subscribers/1 \\
       -H "Authorization: Bearer \$TOKEN" | jq .

  curl -sk https://localhost/api/v1/wallets/1/ledger \\
       -H "Authorization: Bearer \$TOKEN" | jq .

  curl -sk https://localhost/api/v1/invoices/1 \\
       -H "Authorization: Bearer \$TOKEN" | jq .

  curl -sk -X POST https://localhost/api/v1/tickets \\
       -H "Authorization: Bearer \$TOKEN" -H 'Content-Type: application/json' \\
       -d '{"subscriber_id":1,"category":"connectivity","description":"Demo ticket"}' | jq .

  ── RADIUS (needs a client, e.g. radload from this repo) ────────────────────
  docker run --rm --network "$COMPOSE_NETWORK" \\
      -v "$REPO_ROOT:/src" -w /src -e GOFLAGS=-mod=mod golang:1.22 \\
      go run ./cmd/radload -addr bss_aaa_core_daemon:1812 -secret "${RADIUS_SECRET}" \\
      -username test_user -password testpassword

  ── Logs ─────────────────────────────────────────────────────────────────────
  docker compose logs -f api_service
  docker compose logs -f aaa_core_daemon
  docker compose logs -f reverse_proxy

  ── Shut down ────────────────────────────────────────────────────────────────
  ./scripts/demo_up.sh down        # keep data
  ./scripts/demo_up.sh down -v     # wipe data too

EOF
