#!/usr/bin/env bash
# provision.sh — create the GCP infrastructure this stack needs on Compute
# Engine: a reserved static IP, firewall rules, and a Docker-ready VM.
#
# Compute Engine specifically, and not the serverless products, because the
# RADIUS daemon listens on UDP 1812/1813. Cloud Run and App Engine terminate
# HTTP(S) only and cannot carry UDP at all, so neither can host the half of
# this system that authenticates subscribers. GKE would work (its external
# passthrough Network Load Balancer does support UDP) but is a lot of moving
# parts for a single-tenant deployment.
#
# This script provisions infrastructure only. Getting the code onto the VM
# and starting it is deploy.sh / the README — kept separate so re-deploying a
# new build never risks touching networking that routers depend on.
#
# Usage:
#   NAS_SOURCE_RANGES=203.0.113.4/32,198.51.100.0/24 \
#   DOMAIN=bss.example.com \
#   ./provision.sh
#
# Everything else has a default; run with DRY_RUN=1 to print the commands
# without executing them.

set -euo pipefail

# ── Configuration ───────────────────────────────────────────────────────────

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-asia-south1}"          # Mumbai — closest region to an Indian ISP
ZONE="${ZONE:-${REGION}-a}"
INSTANCE="${INSTANCE:-isp-bss}"
MACHINE_TYPE="${MACHINE_TYPE:-e2-standard-2}"
BOOT_DISK_SIZE="${BOOT_DISK_SIZE:-50GB}"
NETWORK_TAG="${NETWORK_TAG:-isp-bss}"
ADDRESS_NAME="${ADDRESS_NAME:-${INSTANCE}-ip}"

# The source ranges permitted to reach RADIUS. Deliberately has no default.
#
# RADIUS authenticates on a shared secret with no transport security, so an
# open UDP 1812 is an invitation to offline-crack that secret against captured
# traffic — and a NAS estate is a known, small, static set of addresses, so
# there is never a legitimate reason to accept RADIUS from the whole internet.
# The script refuses to run rather than defaulting to 0.0.0.0/0.
NAS_SOURCE_RANGES="${NAS_SOURCE_RANGES:-}"

# Who may reach the staff console and subscriber portal. Defaults open,
# because subscribers legitimately browse the portal from arbitrary consumer
# IPs — unlike RADIUS, this is a public web surface behind TLS and login.
WEB_SOURCE_RANGES="${WEB_SOURCE_RANGES:-0.0.0.0/0}"

DRY_RUN="${DRY_RUN:-0}"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
ok()   { printf "${GREEN}[ ok ]${NC} %s\n" "$1"; }
die()  { printf "${RED}[fail]${NC} %s\n" "$1" >&2; exit 1; }

run() {
    if [ "$DRY_RUN" = "1" ]; then
        printf '  + %s\n' "$*"
        return 0
    fi
    "$@"
}

# ── Preflight ───────────────────────────────────────────────────────────────

command -v gcloud >/dev/null 2>&1 || die "gcloud is not installed — see https://cloud.google.com/sdk/docs/install"
[ -n "$PROJECT" ] || die "No project set. Pass PROJECT=... or run: gcloud config set project YOUR_PROJECT"

if [ -z "$NAS_SOURCE_RANGES" ]; then
    die "NAS_SOURCE_RANGES is required — the public IPs of the routers that will
       authenticate against this server, comma-separated (e.g.
       203.0.113.4/32,198.51.100.0/24).

       This is not defaulted on purpose. RADIUS has no transport security;
       exposing UDP 1812 to 0.0.0.0/0 lets anyone collect handshakes and
       attack the shared secret offline. If you genuinely do not know the
       router addresses yet, provision with a placeholder range you control
       and widen it deliberately later."
fi

info "project=$PROJECT region=$REGION zone=$ZONE instance=$INSTANCE"
info "RADIUS restricted to: $NAS_SOURCE_RANGES"
[ "$DRY_RUN" = "1" ] && info "DRY_RUN=1 — printing commands only"

# ── Static external IP ──────────────────────────────────────────────────────
#
# Reserved before the VM exists so the address can go into DNS immediately:
# Caddy's certificate issuance needs the A record already resolving when the
# stack first starts, or the ACME HTTP-01 challenge fails and it falls back
# to a self-signed local CA.

if gcloud compute addresses describe "$ADDRESS_NAME" --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    ok "static IP $ADDRESS_NAME already reserved"
else
    info "reserving static IP $ADDRESS_NAME"
    run gcloud compute addresses create "$ADDRESS_NAME" \
        --region "$REGION" --project "$PROJECT"
fi

STATIC_IP="$(gcloud compute addresses describe "$ADDRESS_NAME" \
    --region "$REGION" --project "$PROJECT" --format='value(address)' 2>/dev/null || echo 'DRY_RUN')"
ok "static IP: $STATIC_IP"

# ── Firewall ────────────────────────────────────────────────────────────────
#
# Two rules rather than one, because the two surfaces have genuinely
# different exposure: the web surface is public by necessity, RADIUS is not.
# Keeping them separate means widening one never silently widens the other.

ensure_firewall_rule() {
    local name="$1" allow="$2" sources="$3" description="$4"
    if gcloud compute firewall-rules describe "$name" --project "$PROJECT" >/dev/null 2>&1; then
        info "firewall rule $name exists — updating source ranges"
        run gcloud compute firewall-rules update "$name" \
            --project "$PROJECT" --source-ranges "$sources"
    else
        info "creating firewall rule $name ($allow from $sources)"
        run gcloud compute firewall-rules create "$name" \
            --project "$PROJECT" \
            --direction=INGRESS --action=ALLOW \
            --rules "$allow" \
            --source-ranges "$sources" \
            --target-tags "$NETWORK_TAG" \
            --description "$description"
    fi
}

# 80 is required alongside 443: Caddy answers the ACME HTTP-01 challenge
# there and redirects to HTTPS. Closing it means no automatic certificate.
ensure_firewall_rule "${NETWORK_TAG}-web" "tcp:443,tcp:80" "$WEB_SOURCE_RANGES" \
    "ISP BSS staff console, subscriber portal and API (TLS); port 80 for ACME"

ensure_firewall_rule "${NETWORK_TAG}-radius" "udp:1812,udp:1813" "$NAS_SOURCE_RANGES" \
    "ISP BSS RADIUS auth and accounting — NAS devices only"

# Deliberately NOT opened: 5432 (PostgreSQL) and 9101/9102 (Prometheus
# metrics). Both are reachable inside the Docker network and over an SSH
# tunnel when an operator needs them; publishing either to the internet adds
# attack surface for no operational gain.

# ── VM ──────────────────────────────────────────────────────────────────────

STARTUP_SCRIPT="$(cat <<'STARTUP'
#!/usr/bin/env bash
# Installs Docker Engine + compose plugin on first boot. Debian 12.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

if command -v docker >/dev/null 2>&1; then
    echo "docker already present; nothing to do"
    exit 0
fi

apt-get update
apt-get install -y ca-certificates curl gnupg git

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg \
    | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    > /etc/apt/sources.list.d/docker.list

apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

systemctl enable --now docker
echo "docker installed"
STARTUP
)"

if gcloud compute instances describe "$INSTANCE" --zone "$ZONE" --project "$PROJECT" >/dev/null 2>&1; then
    ok "instance $INSTANCE already exists — leaving it alone"
else
    info "creating instance $INSTANCE ($MACHINE_TYPE)"
    run gcloud compute instances create "$INSTANCE" \
        --project "$PROJECT" \
        --zone "$ZONE" \
        --machine-type "$MACHINE_TYPE" \
        --image-family debian-12 \
        --image-project debian-cloud \
        --boot-disk-size "$BOOT_DISK_SIZE" \
        --boot-disk-type pd-balanced \
        --tags "$NETWORK_TAG" \
        --address "$STATIC_IP" \
        --metadata startup-script="$STARTUP_SCRIPT"
fi

# ── Next steps ──────────────────────────────────────────────────────────────

cat <<EOF

$(printf "${GREEN}Infrastructure ready.${NC}")

  Static IP : $STATIC_IP
  Instance  : $INSTANCE  (zone $ZONE)
  RADIUS    : udp/1812,1813 from $NAS_SOURCE_RANGES
  Web       : tcp/443,80 from $WEB_SOURCE_RANGES

Before starting the stack:

  1. Point DNS at it — an A record for your domain to $STATIC_IP.
     Do this FIRST. Caddy requests a Let's Encrypt certificate on startup,
     and the ACME challenge fails if the name does not already resolve here;
     it then falls back to a self-signed cert that browsers will reject.

  2. Point each NAS at $STATIC_IP for RADIUS, and register those devices in
     the console (Routers screen) with matching shared secrets.

  3. Deploy the code and start it:  ./deploy.sh

  SSH:  gcloud compute ssh $INSTANCE --zone $ZONE --project $PROJECT

EOF
