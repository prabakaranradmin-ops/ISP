#!/usr/bin/env bash
# storm_test_radius.sh — measure what actually happens to the RADIUS daemon
# under a reconnect storm, and prove the difference between a cold and a warm
# fast-verifier cache.
#
# WHY THIS IS SEPARATE FROM load_test_radius.sh
#
# That script answers NFR-PERF-001: "is p99 latency under 15ms in steady
# state". This one answers a different and harsher question: "what breaks
# when every subscriber reconnects at once, and where do the losses go".
# Those need opposite setups — the p99 test wants a warm cache and a
# sustainable rate, this one wants a cold cache and deliberate overload — so
# combining them into one script would mean neither measured its own thing
# honestly.
#
# WHAT IT MEASURES THAT AN APPLICATION METRIC CANNOT
#
# Packets lost to a full socket buffer or an exhausted conntrack table never
# reach the process. No Go metric can count them; from the daemon's point of
# view they were never sent. So this brackets each run with the kernel's own
# counters (nstat / netstat -su) and reports the delta alongside the
# application's numbers. A run where the daemon reports zero drops while the
# kernel discarded thousands looks perfectly healthy from Prometheus and is
# in fact the worst case, because nothing anywhere is reporting the loss.
#
# Usage:
#   RADIUS_SECRET=... USERS_CSV=.nfr_users.csv bash scripts/storm_test_radius.sh
#
#   COLD_RESTART=1   restart the daemon before the cold run (see below)
#   RATE=4000        offered rate; push past capacity on purpose
#   DURATION=60s
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RADIUS_HOST="${RADIUS_HOST:-127.0.0.1}"
RADIUS_PORT="${RADIUS_PORT:-1812}"
RADIUS_SECRET="${RADIUS_SECRET:-testing123}"
METRICS_URL="${METRICS_URL:-http://127.0.0.1:9100/metrics}"
RATE="${RATE:-4000}"
DURATION="${DURATION:-60s}"
CONCURRENT="${CONCURRENT:-128}"
USERS_CSV="${USERS_CSV:-}"
COLD_RESTART="${COLD_RESTART:-0}"
COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.yml}"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
ok()   { printf "${GREEN}[ ok ]${NC} %s\n" "$1"; }
warn() { printf "${RED}[warn]${NC} %s\n" "$1"; }
head1() { printf "\n${GREEN}══ %s ══${NC}\n" "$1"; }

if [ -z "$USERS_CSV" ]; then
    warn "USERS_CSV is unset."
    echo "      A single repeated account is served from the verifier cache after its"
    echo "      first request, so a cold-vs-warm comparison would show no difference"
    echo "      at all — the exact effect this script exists to measure."
    echo "      Seed one first:  go run ./scripts/seed_load -users-out .nfr_users.csv"
    exit 1
fi
[ -f "$USERS_CSV" ] || { warn "$USERS_CSV not found"; exit 1; }

# ── Kernel counter helpers ──────────────────────────────────────────────────
#
# Two sources because they do not overlap and neither is complete alone:
#
#   Udp*        — per-protocol receive errors and buffer overflows. This is
#                 where a socket receive buffer too small for the burst shows
#                 up, as RcvbufErrors / InErrors.
#   conntrack   — table occupancy against its maximum. A full table drops
#                 new flows host-wide and logs to dmesg, but increments
#                 nothing in the Udp counters, so it is invisible above.
udp_counters() {
    if command -v nstat >/dev/null 2>&1; then
        # -a: absolute values rather than since-last-run; -z: include zeroes,
        # so a counter that never fired still appears and the delta arithmetic
        # below does not have to special-case a missing key.
        nstat -az 2>/dev/null | awk '/Udp/ {print $1"="$2}'
    else
        netstat -su 2>/dev/null | tr -d ',' | awk '
            /packet receive errors/  {print "UdpInErrors="$1}
            /receive buffer errors/  {print "UdpRcvbufErrors="$1}'
    fi
}

conntrack_state() {
    local count max
    count="$(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo 0)"
    max="$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo 0)"
    echo "count=$count max=$max"
}

metric() {
    # $1 = metric name (optionally with labels). Sums matching samples;
    # prints 0 when the metric is absent, so a daemon built before these
    # metrics existed still produces a readable report.
    curl -fsS --max-time 5 "$METRICS_URL" 2>/dev/null \
        | awk -v pat="^$1" '$0 ~ pat && $0 !~ /^#/ {sum += $NF} END {printf "%.0f", sum+0}'
}

snapshot_app() {
    printf 'dropped_auth=%s dropped_acct=%s queue_depth=%s cache_hits=%s accepts=%s rejects=%s' \
        "$(metric 'radius_packets_dropped_total\{listener="auth"\}')" \
        "$(metric 'radius_packets_dropped_total\{listener="acct"\}')" \
        "$(metric 'radius_worker_queue_depth')" \
        "$(metric 'radius_verifier_cache_hit_total')" \
        "$(metric 'radius_auth_accept_total')" \
        "$(metric 'radius_auth_reject_total')"
}

delta() { echo $(( ${2:-0} - ${1:-0} )); }

kv() { printf '%s' "$1" | tr ' ' '\n' | awk -F= -v k="$2" '$1==k {print $2}'; }

# ── One run ─────────────────────────────────────────────────────────────────

run_phase() {
    local label="$1"

    head1 "$label"

    local udp_before conntrack_before app_before
    udp_before="$(udp_counters)"
    conntrack_before="$(conntrack_state)"
    app_before="$(snapshot_app)"

    info "offering ${RATE}/s for ${DURATION} (concurrency ${CONCURRENT})"
    local out
    out="$(go run ./cmd/radload \
        -addr "${RADIUS_HOST}:${RADIUS_PORT}" \
        -secret "${RADIUS_SECRET}" \
        -users "${USERS_CSV}" \
        -rate "${RATE}" \
        -duration "${DURATION}" \
        -concurrency "${CONCURRENT}" 2>&1)"
    echo "$out"

    local udp_after app_after
    udp_after="$(udp_counters)"
    app_after="$(snapshot_app)"

    echo ""
    info "kernel-level loss (invisible to the application):"
    local key before after d
    for key in UdpInErrors UdpRcvbufErrors UdpNoPorts; do
        before="$(printf '%s\n' "$udp_before" | awk -F= -v k="$key" '$1==k {print $2}')"
        after="$(printf '%s\n' "$udp_after"  | awk -F= -v k="$key" '$1==k {print $2}')"
        d="$(delta "${before:-0}" "${after:-0}")"
        if [ "$d" -gt 0 ]; then
            warn "  $key +$d"
            case "$key" in
              UdpRcvbufErrors)
                echo "        The socket receive buffer overflowed. Raise net.core.rmem_max"
                echo "        (deploy/gcp/provision.sh writes it) — the daemon requests 4MiB"
                echo "        but the kernel silently caps it at that ceiling." ;;
              UdpInErrors)
                echo "        Datagrams discarded before delivery. Usually the same cause" ;;
            esac
        else
            echo "    $key +0"
        fi
    done

    echo "    conntrack before: $conntrack_before"
    echo "    conntrack after : $(conntrack_state)"
    echo "      (approaching max means the table is filling; when it fills the"
    echo "       kernel drops NEW FLOWS HOST-WIDE, including your SSH session)"

    echo ""
    info "application-level shedding (deliberate, and counted):"
    printf '    auth drops   +%s\n' "$(delta "$(kv "$app_before" dropped_auth)" "$(kv "$app_after" dropped_auth)")"
    printf '    acct drops   +%s\n' "$(delta "$(kv "$app_before" dropped_acct)" "$(kv "$app_after" dropped_acct)")"
    printf '    accepts      +%s\n' "$(delta "$(kv "$app_before" accepts)"      "$(kv "$app_after" accepts)")"
    printf '    rejects      +%s\n' "$(delta "$(kv "$app_before" rejects)"      "$(kv "$app_after" rejects)")"
    printf '    verifier hits+%s\n' "$(delta "$(kv "$app_before" cache_hits)"   "$(kv "$app_after" cache_hits)")"
    printf '    queue depth (at sample): %s\n' "$(kv "$app_after" queue_depth)"

    # Exported for the comparison at the end.
    PHASE_ACCEPTS="$(delta "$(kv "$app_before" accepts)" "$(kv "$app_after" accepts)")"
    PHASE_HITS="$(delta "$(kv "$app_before" cache_hits)" "$(kv "$app_after" cache_hits)")"
}

# ── Cold phase ──────────────────────────────────────────────────────────────
#
# "Cold" means the verifier cache is empty, so every authentication pays
# bcrypt cost-12. That is the state after any restart — which is exactly
# when a reconnect wave is most likely, and the reason the persistent tier
# (migration 046) exists.
#
# With that tier enabled a restart no longer produces a cold cache, which is
# the point: to measure the cold path you must also clear the table.

if [ "$COLD_RESTART" = "1" ]; then
    info "restarting the daemon to empty the in-process cache"
    docker compose $COMPOSE_FILES restart aaa_core_daemon >/dev/null 2>&1 || \
        warn "could not restart aaa_core_daemon — is the stack up?"
    info "clearing the persistent verifier cache (migration 046)"
    docker compose $COMPOSE_FILES exec -T postgres_primary \
        psql -U postgres -d isp_bss_oss -c "TRUNCATE radius_verifier_cache;" >/dev/null 2>&1 || \
        warn "could not clear radius_verifier_cache — the 'cold' run may in fact be warm"
    sleep 8
fi

run_phase "COLD — verifier cache empty, every auth pays bcrypt (~280ms CPU each)"
COLD_ACCEPTS="$PHASE_ACCEPTS"

# ── Warm phase ──────────────────────────────────────────────────────────────
#
# Immediately after, with no restart: every account in the CSV now has a
# verifier, so bcrypt is skipped and the same offered rate should complete
# far more work.

run_phase "WARM — verifier cache populated by the cold run"
WARM_ACCEPTS="$PHASE_ACCEPTS"
WARM_HITS="$PHASE_HITS"

# ── Comparison ──────────────────────────────────────────────────────────────

head1 "Cold vs warm"
echo "  accepts, cold run : $COLD_ACCEPTS"
echo "  accepts, warm run : $WARM_ACCEPTS"
echo "  verifier hits warm: $WARM_HITS"
echo ""

if [ "${COLD_ACCEPTS:-0}" -gt 0 ] && [ "${WARM_ACCEPTS:-0}" -gt 0 ]; then
    ratio=$(( WARM_ACCEPTS * 100 / COLD_ACCEPTS ))
    echo "  warm throughput is ${ratio}% of cold"
    if [ "$ratio" -lt 150 ]; then
        warn "Expected the warm run to be several times faster. If it is not, the"
        echo "        verifier cache is not being hit — check that USERS_CSV accounts"
        echo "        actually authenticated (accepts > 0, rejects ~ 0) and that"
        echo "        radius_verifier_cache_hit_total is rising."
    else
        ok "The verifier cache is doing its job. This gap is what a restart costs"
        echo "        you without migration 046's persistent tier — and why warmup"
        echo "        matters more than raw capacity during a reconnect event."
    fi
fi

cat <<'EOF'

Reading the result
──────────────────
  Application drops > 0, kernel drops 0
      Working as designed. The daemon shed load it could not serve; the NAS
      will retransmit. Capacity is the constraint, not configuration.

  Kernel drops > 0
      Packets died before the daemon saw them. Nothing in Prometheus will
      ever show this. Raise net.core.rmem_max, and put the daemon on host
      networking (deploy/gcp/docker-compose.gcp.yml) so RADIUS traffic
      creates no conntrack entries at all.

  Both zero, but latency high
      Neither buffers nor shedding — this is CPU. Cold bcrypt on 2 vCPUs is
      roughly 7 auths/sec no matter how the queues are tuned. Warm the cache
      or add cores; nothing else moves it.
EOF
