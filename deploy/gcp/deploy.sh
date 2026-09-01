#!/usr/bin/env bash
# deploy.sh — ship the current working tree to the GCE instance provision.sh
# created, apply migrations, and start the stack.
#
# Re-runnable: this is the update path as well as the first deploy. Nothing
# here touches networking or the static IP, so a redeploy can never strand
# the routers pointed at this server.
#
# Usage:
#   DOMAIN=bss.example.com ./deploy.sh
#
# DOMAIN must already resolve to the instance's static IP before the first
# run — see provision.sh's closing notes on why (Caddy's ACME challenge).

set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-asia-south1}"
ZONE="${ZONE:-${REGION}-a}"
INSTANCE="${INSTANCE:-isp-bss}"
REMOTE_DIR="${REMOTE_DIR:-/opt/isp-bss}"
DOMAIN="${DOMAIN:-}"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
ok()   { printf "${GREEN}[ ok ]${NC} %s\n" "$1"; }
die()  { printf "${RED}[fail]${NC} %s\n" "$1" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

command -v gcloud >/dev/null 2>&1 || die "gcloud is not installed"
[ -n "$PROJECT" ] || die "No project set. Pass PROJECT=... or: gcloud config set project YOUR_PROJECT"
[ -n "$DOMAIN" ] || die "DOMAIN is required (e.g. DOMAIN=bss.example.com).

       Caddy uses it to decide what certificate to request. Left unset it
       defaults to 'localhost' and issues from its own local CA, which every
       browser and every API client will reject."

remote() { gcloud compute ssh "$INSTANCE" --zone "$ZONE" --project "$PROJECT" --command "$1"; }

# ── Preflight: does DNS actually point here? ────────────────────────────────
#
# Checked rather than assumed, because getting this wrong is expensive in a
# way that is not obvious for hours: Let's Encrypt rate-limits failed
# authorizations, so repeatedly starting the stack against a domain that does
# not resolve here can lock out certificate issuance for that name.

STATIC_IP="$(gcloud compute instances describe "$INSTANCE" --zone "$ZONE" --project "$PROJECT" \
    --format='value(networkInterfaces[0].accessConfigs[0].natIP)' 2>/dev/null || true)"
[ -n "$STATIC_IP" ] || die "Cannot find instance $INSTANCE in zone $ZONE — run provision.sh first."

RESOLVED="$(getent hosts "$DOMAIN" 2>/dev/null | awk '{print $1}' | head -1 || true)"
if [ -z "$RESOLVED" ]; then
    die "$DOMAIN does not resolve. Create an A record pointing it at $STATIC_IP and wait for propagation."
elif [ "$RESOLVED" != "$STATIC_IP" ]; then
    die "$DOMAIN resolves to $RESOLVED, but the instance is $STATIC_IP.
       Starting the stack now would burn Let's Encrypt authorization attempts
       against a name that cannot be validated here. Fix DNS first."
fi
ok "$DOMAIN resolves to $STATIC_IP"

# ── Wait for the VM's first-boot Docker install ─────────────────────────────

info "checking Docker is installed on the instance"
if ! remote "command -v docker >/dev/null 2>&1"; then
    die "Docker is not installed yet. The startup script runs on first boot and
       takes a minute or two; check progress with:
         gcloud compute ssh $INSTANCE --zone $ZONE --command 'sudo journalctl -u google-startup-scripts -n 50'"
fi
ok "docker present"

# ── Ship the working tree ───────────────────────────────────────────────────
#
# A tarball of tracked files rather than a git clone on the VM: no deploy
# key or repo credentials need to exist on a public-facing host, and what
# ships is exactly what is committed — `git archive` cannot accidentally
# include an untracked .env, key material, or a stray build artifact.

info "packaging tracked files at HEAD"
TARBALL="$(mktemp -t isp-bss-XXXXXX.tar.gz)"
trap 'rm -f "$TARBALL"' EXIT
git -C "$REPO_ROOT" archive --format=tar.gz -o "$TARBALL" HEAD
ok "packaged $(du -h "$TARBALL" | cut -f1)"

info "copying to $INSTANCE:$REMOTE_DIR"
remote "sudo mkdir -p $REMOTE_DIR && sudo chown \$(whoami) $REMOTE_DIR"
gcloud compute scp "$TARBALL" "$INSTANCE:/tmp/isp-bss.tar.gz" --zone "$ZONE" --project "$PROJECT" --quiet
remote "tar -xzf /tmp/isp-bss.tar.gz -C $REMOTE_DIR && rm -f /tmp/isp-bss.tar.gz"
ok "code in place"

# ── Secrets ─────────────────────────────────────────────────────────────────
#
# Generated on the VM and never printed, copied, or committed. A redeploy
# leaves an existing .env alone: regenerating AES_KEY_STORE_URL's key or
# APP_DB_PASSWORD against a live database would make encrypted PII columns
# unreadable and lock the services out of their own role.

info "ensuring .env and key material exist (left alone if already present)"
remote "cd $REMOTE_DIR && bash deploy/gcp/remote_bootstrap.sh '$DOMAIN'"
ok "secrets in place"

# ── Start: database first, migrate, then the application ────────────────────
#
# The ordering is not cosmetic. The services post to chart-of-accounts codes
# that arrive in migrations (5200 and 2100 in 045), and a binary running
# ahead of its schema refuses those postings rather than writing a ledger it
# cannot balance. Migrating before the app starts avoids that window
# entirely instead of relying on it failing gracefully.

info "starting PostgreSQL"
remote "cd $REMOTE_DIR && sudo docker compose up -d postgres_primary"

info "waiting for PostgreSQL to accept connections"
remote "cd $REMOTE_DIR && for i in \$(seq 1 60); do
          sudo docker compose exec -T postgres_primary pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && exit 0
          sleep 2
        done; echo 'postgres did not become ready'; exit 1"
ok "postgres ready"

info "applying migrations"
remote "cd $REMOTE_DIR && bash deploy/gcp/remote_migrate.sh"
ok "migrations applied"

info "starting the rest of the stack"
remote "cd $REMOTE_DIR && sudo docker compose up -d"

info "waiting for the API to report ready"
if remote "for i in \$(seq 1 45); do
             curl -fsS -o /dev/null https://$DOMAIN/readyz && exit 0
             sleep 2
           done; exit 1"; then
    ok "https://$DOMAIN/readyz is serving with a valid certificate"
else
    printf "${YELLOW}[warn]${NC} %s\n" "/readyz did not come up cleanly within 90s."
    echo "       Certificate issuance can take a moment on a first run. Check:"
    echo "         gcloud compute ssh $INSTANCE --zone $ZONE --command 'cd $REMOTE_DIR && sudo docker compose logs --tail=50 reverse_proxy api_service'"
fi

cat <<EOF

$(printf "${GREEN}Deployed.${NC}")

  Staff console : https://$DOMAIN/staff/login
  Subscriber    : https://$DOMAIN/ui/login
  RADIUS        : $STATIC_IP udp/1812,1813

The initial staff account and its one-time password are printed in the API
service log on a first deploy:

  gcloud compute ssh $INSTANCE --zone $ZONE --command \\
    'cd $REMOTE_DIR && sudo docker compose logs api_service | grep -i "initial admin"'

EOF
