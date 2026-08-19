#!/usr/bin/env bash
# check_wiring.sh — every long-running component must be reachable from a
# binary, not just from tests.
#
# This exists because three components shipped complete, correct and fully
# tested with no caller anywhere outside tests:
#
#   TransitionDunning   nobody was reminded to pay, nobody was ever suspended
#   ReconcileJob        no revenue snapshot, no ledger variance, ever
#   TMPL-002..007       templates registered, nothing dispatched them
#
# None of that failed a test, because nothing was wrong — there was simply
# nothing there to fail. The suite was green, coverage was reported, and the
# features did not run. A checklist item that reads "is it tested" cannot
# catch this; only asking "is it called" can.
#
# Usage:
#   ./scripts/check_wiring.sh          # report and exit non-zero on findings
#   ./scripts/check_wiring.sh --list   # list what is checked, then report
#
# Exit 0 = every tracked component has a production caller.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
FAILURES=0

pass() { printf "${GREEN}[WIRED]${NC}  %s\n" "$1"; }
fail() { printf "${RED}[UNWIRED]${NC} %s\n" "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf "${YELLOW}[....]${NC}  %s\n" "$1"; }

# Constructors and entry points that only make sense if something runs them.
# A component belongs here when "nobody calls it" is a silent production
# outage rather than dead code a linter would flag.
COMPONENTS=(
    "NewScanner:FUP scanner (throttles subscribers over quota)"
    "NewDeadLetterMonitor:dead-letter monitor (surfaces stuck tasks)"
    "NewDunningScanner:dunning scanner (reminders and suspensions)"
    "NewRecurringBillingScanner:auto-renewal scanner (charges from wallet balance)"
    "NewReconcileScheduler:nightly revenue reconciliation"
    "NewDunningNoticeHandler:dunning notification worker"
    "NewPaymentReceiptHandler:payment receipt worker"
    "NewWarningHandler:80% FUP warning worker"
    "NewUpdateHandler:ticket status-change notification worker"
    "NewCoAHandler:CoA worker (applies rate limits)"
    "NewPoDHandler:PoD worker (disconnects sessions)"
    "NewVerifierCache:RADIUS fast-verifier cache"
    "NewSubscriberCache:RADIUS subscriber cache"
    "NewResolver:multi-vendor NAS attribute/secret resolver"
    "NewSLAScanner:SLA breach scanner (helpdesk deadlines)"
    "NewRefreshScanner:reporting view refresh (keeps mv_ticket_resolution current)"
    # Tracks the mount, not the constructor. Constructing the handler proves
    # nothing on its own — an unmounted captive portal is built, tested, and
    # serves no route, which is this script's whole subject. Matching the
    # method call covers both, since it can only be called on a handler that
    # was constructed. It does depend on the variable's name in cmd/api, which
    # is the right direction to be brittle in: a rename fails loudly here
    # rather than passing silently while nothing is served.
    "hotspotHandler.RegisterRoutes:captive portal (walled-garden voucher and login pages)"
    # Not merely a dependency of the above: with no limiter constructed, every
    # redemption answers 503 by design, so an unwired limiter is a silently
    # dead captive portal rather than an unmetered one.
    "hotspot.NewLimiter:captive-portal attempt limiter (bounds voucher guessing)"
    # The canonical instance of what this script is for. StartSession,
    # UpdateSessionOctets and StopSession were written, tested and correct, and
    # nothing called them — so subscriber_session_history stayed empty and FUP
    # enforcement, CoA targeting, LEA lookups and portal usage all read no rows
    # while their own tests passed.
    "daemon.SetAccountingStore:RADIUS accounting persistence (writes subscriber_session_history)"
    # Retention that nothing enforces is worse than none: retain_until would
    # record the date each document should have been deleted while it sat there,
    # which under the DPDP Act is a violation the system documented against
    # itself.
    "archive.NewPurgeScanner:document retention purge (deletes archives past retain_until)"
    # data_cap_bytes sat unread for a release: a voucher sold as "1 GB" was
    # limited only by its duration. Nothing else can enforce it, because
    # voucher sessions have no subscriber row for the FUP scanner to find.
    "hotspot.NewQuotaScanner:voucher data-cap enforcement (ends exhausted hotspot sessions)"
    # FR-OBS-005. The requirement is a proactive alert, so a monitor nobody
    # runs satisfies nothing at all.
    "NewAuthFailureMonitor:per-NAS RADIUS auth failure alerting (FR-OBS-005)"
)

if [ "${1:-}" = "--list" ]; then
    printf "${BOLD}Components checked for a production caller:${NC}\n"
    for entry in "${COMPONENTS[@]}"; do
        printf "  %-26s %s\n" "${entry%%:*}" "${entry#*:}"
    done
    echo ""
fi

printf "${BOLD}== Wiring check ==${NC}\n"

for entry in "${COMPONENTS[@]}"; do
    symbol="${entry%%:*}"
    description="${entry#*:}"

    # A caller counts only if it is production code: not a _test.go file, not
    # the declaration itself, and not a doc comment mentioning the name.
    callers=$(grep -rn --include="*.go" "\b${symbol}(" cmd/ internal/ 2>/dev/null \
        | grep -v "_test\.go:" \
        | grep -vE ":[0-9]+:\s*//" \
        | grep -vE "func ${symbol}\(" \
        | wc -l)

    if [ "$callers" -gt 0 ]; then
        pass "$(printf '%-26s %s' "$symbol" "$description")"
    else
        fail "$(printf '%-26s %s' "$symbol" "$description")"
        # Show where it IS referenced, which is usually tests only — that is
        # the shape of the bug and makes the report actionable rather than
        # merely accusatory.
        grep -rn --include="*.go" "\b${symbol}(" cmd/ internal/ 2>/dev/null \
            | grep -vE "func ${symbol}\(" | head -3 | sed 's/^/           /'
    fi
done

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}WIRING OK${NC} — every tracked component has a caller outside tests\n"
    exit 0
fi
printf "${RED}WIRING FAIL${NC} — %d component(s) are built and tested but never run\n" "$FAILURES"
printf "Add the caller, or delete the component. A third option — leaving it\n"
printf "wired to nothing while its tests pass — is what this check exists to stop.\n"
exit 1
