# Definition of Done — Status Report

**Original audit:** 2026-08-08
**Last revised:** 2026-08-11 (dunning and revenue reconciliation wired; L0-012 added — see Finding #8)
**Post-v1.0.0 update:** 2026-08-18 — see "Post-v1.0.0 Update" immediately below.
**Scope:** Whole-codebase audit against the 66-item DoD checklist in `bss_oss_dev_tracker_v3.xlsx` → sheet "✅ Definition of Done" (levels L0–L8). **This checklist predates everything the 2026-08-18 update covers** — it was written when the codebase was ~92 FRs earlier and has not been re-executed row by row since 2026-08-11. Read the update section for current state; read L0–L8 below it for the v1.0.0-era infrastructure work, which mostly still holds but was not re-verified today.
**Methodology:** Every check below was either (a) executed live against this repo/a real Postgres instance/the running demo stack, or (b) verified by direct code/config inspection with exact file:line evidence. Checks that genuinely require infrastructure not available are marked **NOT VERIFIED**, not silently assumed passing. See "Methodology & Limitations" at the end.

Of the tracker's rows plus L0-012 added here, **66 carry an actual check**; row `L0-011` is an empty placeholder in the source spreadsheet (all cells blank, and it is explicitly excluded from the tracker's own `COUNTIF(H3:H76,"Y")` formula range) — treated here as a tracker artifact, not a gradable item.

## Executive Summary

| Result | Count | % of 66 | Was (2026-08-08) |
|---|---|---|---|
| ✅ PASS | 53 | 80% | 41 |
| ❌ FAIL | 2 | 3% | 14 |
| 🟡 PARTIAL (spot-checked, not exhaustive) | 4 | 6% | 2 |
| ⬜ NOT VERIFIED (needs infrastructure not available) | 1 | 2% | 4 |
| ⬜ PENDING (manual tracker administration) | 4 | 6% | 4 |
| ⬜ N/A (accepted, unachievable retroactively) | 2 | 3% | 0 |

**All four of the original audit's critical findings are now closed**, and every L6 row except the 1-hour soak has been measured rather than assumed. Only one failure remains: the coverage shortfall (L2-002). L6-005's 1-hour soak did run, on 2026-08-18, and its row said otherwise until 2026-11-23. Sentinel failover timing (L6-004, Finding #6) was closed on 2026-08-18 and the component it measured has since been deleted — see the row and the finding below. The two TDD-ordering rows moved to N/A by decision rather than staying as failures no work could ever clear.

**One caveat on reading this scorecard.** Finding #7 — an unauthenticated endpoint disclosing subscriber IPs — was live while every row below already read PASS, and was found by a stakeholder question rather than by any check on this list. The L4 authorisation rows were not wrong; the leaking route simply never reached the middleware they test. Treat 52/65 as a floor, not a clean bill of health.

### What changed since the original audit

| ID | Check | Was | Now | Basis |
|---|---|---|---|---|
| L1-009 / L4-010 | `lea_audit_log` append-only via RLS | ❌ FAIL (critical) | ✅ PASS | Migration `019_create_app_role.sql` — least-privilege `bss_app` role (commit `9cdb1dd`) |
| L3-003 | Redis on the RADIUS auth hot path | ❌ FAIL | ✅ PASS | `internal/radius/verifiercache.go` (commit `9cdb1dd`) |
| L5-007 | E.164 validated before SMS send | ❌ FAIL | ✅ PASS | `pkg/validate/phone.go` + migration `020` (commit `40d04f5`) |
| L6-001 | RADIUS p99 ≤15ms @ 5,000 req/s | ❌ FAIL (blocked) | ✅ PASS | **Measured: p99 10.4ms**, 149,850 requests, 0 errors |
| L6-002 | API p99 ≤200ms @ 500 concurrent | ⬜ NOT VERIFIED | ✅ PASS | **Measured: p99 11.69ms** at 500 VUs |
| L6-003 | Unbilled report ≤60s for 20k subscribers | ⬜ NOT VERIFIED | ✅ PASS | **Measured: 9.716ms** at 20,000 subscribers |
| L7-003 | `go.mod`/`go.sum` committed | ❌ FAIL | ✅ PASS | Commit `ae75ee4` |
| L0-010 / L7-001 | Conventional commit format | ❌ FAIL | 🟡 PARTIAL | 6 of 7 commits conform; the docs-only root commit predates the convention |
| L0-009 | Pre-commit PII hook fires | ❌ FAIL | ✅ PASS | Installed and verified blocking a real commit (2026-08-10) |
| L2-001 | FR-traceable test names | ❌ FAIL | ✅ PASS | 102 tests renamed, all 30 FR IDs covered (commit `ac3ef26`) |
| L5-008 | Dispatch latency ≤5s | ❌ FAIL | ✅ PASS | **Measured: 1.011s idle, 92ms worst case under backlog** |
| L6-004 | Sentinel failover ≤3s | ⬜ NOT VERIFIED | ⬜ **N/A (component removed)** | Closed 2026-08-18 at the retargeted 8s budget (2913ms election, live-verified). Redis was then removed from the system entirely by `1ae0232`, so there is no Sentinel left to measure — see Finding #6 |
| L6-005 | No goroutine leak after 1h | ⬜ NOT VERIFIED | ✅ **PASS (with a caveat)** | The full 3600s hold ran to completion 2026-08-18: goroutines stable at 1.4% growth. The script also flagged FD growth of 15.7%, which the raw series does not support as a leak — it stepped once and then sat flat for 26 minutes. Treated as closed; see "1-hour soak test" above for the reasoning and its limits. **This row read NOT VERIFIED until 2026-11-23**, three months after the run, the same drift L6-004 had |
| L8-001..004 | Tracker Status / Notes populated | ⬜ PENDING | ⬜ PENDING (data now committed) | 96 tasks marked Done are now in git (commit `297d3bb`); NFR-PERF-001 corrected FAIL → PASS |
| L0-001 / L0-002 | TDD commit ordering | ❌ FAIL | ⬜ N/A (accepted) | Unachievable retroactively; forward-looking expectation recorded |
| L0-012 | Component has a caller outside tests | *(new rule)* | ✅ PASS | `./scripts/check_wiring.sh` — 11/11 wired (Finding #8) |
| L3-005 | notification_log row per dispatch attempt | ✅ PASS (code-verified) | ✅ PASS (now executed) | Failed sends wrote no row until 2026-08-11 — see Finding #8 |

## Post-v1.0.0 Update (2026-08-18)

Everything below is new since the original 66-item audit and is not row-mapped
to it — the tracker spreadsheet predates FR-HSP-001/003, FR-DOC-001, the NAS
management API, the RADIUS accounting rewrite, report export, FR-MOB-002, and
FR-OBS-005 entirely. This section states what is true now, with the evidence
behind each claim, in the same spirit as the methodology below: executed and
measured where that was practical today, explicitly marked when it was not.

### Feature completeness

**98 of 99 FRs implemented.** The SRS (`specification_docs_v2/02_SRS_...`)
contains exactly 99 `FR-*` requirements — counted with a word boundary,
since a naive grep returns 108 by matching `FR-AVAIL-001` inside
`NFR-AVAIL-001`. FR-AAA-005 (plain CHAP) is deferred by decision, not
outstanding: it requires storing recoverable plaintext passwords. Every other
FR, including three (FR-OBS-001, FR-OBS-002, FR-OBS-005) that were
implemented but never cited by id anywhere in code, is implemented. Full
reconciliation and the FR-OBS-005 build-out are in `HANDOFF.md`.

**Current measured counts** (`go list`, `scripts/check_wiring.sh`,
`ls migrations/`, all run 2026-08-18):

| | |
|---|---|
| Go packages | 34 total, 24 with tests |
| Migrations | 35 applied, `001`–`035` |
| Wiring check | 22/22 components have a production caller |
| Lint (`--build-tags=integration`) | 110 findings, down from a 118 baseline; **zero introduced by any work in this update** |
| DB integration suite | **PASS**, 757.4s, run after every fix in this update (`scripts/run_db_tests.sh -timeout 25m`) |

### NFR sign-off

Three NFRs were added to the SRS 2026-08-18 for features the original
checklist has no rows for, each backed by a real test rather than a stated
target:

| NFR | Requirement | Evidence |
|---|---|---|
| NFR-DUR-002 | Archive checksum integrity; retention floor enforced | `chk_archive_not_purged_before_retention` exercised directly against Postgres; checksum verified at write time. **Extended today** — see "Archive retrieval" below. |
| NFR-SEC-003 | Captive portal rate-limits and fails closed | `TestFR_HSP_001_GuessingIsRateLimited`, `TestFR_HSP_001_BrokenLimiterRefusesRatherThanWaves` |
| NFR-PERF-004 | Report export p99 ≤ 4.5s at the 120-month maximum window | **Measured**, not estimated: seeded 20,000 subscribers / 50 franchises / 434k invoices / 392k ledger rows over 120 months, 30 iterations per report. Collection (worst case, a CROSS JOIN over invoices and lco_ledger) measured p50 840ms / p99 1.69s; threshold set at 2.5× that. Growth (57ms) and ticket-resolution (6.7ms, materialised view) are both far inside budget. |

Locally-runnable NFRs from the original checklist, re-measured clean on
2026-08-17 with the demo stack stopped (see "A confound worth recording"
below): NFR-PERF-001 **13.224ms** p99 (budget 15ms, 5,000 req/s, 30s, 0
errors), NFR-PERF-002 **49.01ms** p99 (budget 200ms), NFR-BIZ-001
**12.2ms** (budget 60,000ms), NFR-SEC-002 zero plaintext PII.

**A confound worth recording**, since it wasted a full run before being
found: the demo stack (11 containers — Postgres, a 3-node Redis Sentinel
cluster, the API, radiusd) was running throughout an earlier NFR pass and
turned a real 13ms p99 into a false 20ms failure through CPU contention alone
— the daemon's own histogram showed 99.4% of requests under 15ms while the
client measured a 20ms tail, and the rate sweep was non-monotonic (1,000
req/s scored worse than 3,000 req/s), both signatures of scheduling
contention rather than slow code. `run_nfr_tests.sh` and the demo stack
cannot usefully run at the same time on one machine.

### Security review

A focused review of the session's delta (`37708c5..HEAD` at review time, the
one unauthenticated HTTP surface plus the new NAS-secret-handling API) ran
four parallel investigations, each independently re-verified rather than
reported on first pass. **Zero findings cleared the review's 8/10 confidence
bar.** Two of the four requested areas (partner webhook SSRF, EAP-MSCHAPv2
session state) turned out to be pre-existing code the session never touched
and were reviewed anyway, labelled as such rather than under a "new
vulnerability" banner:

- **SSRF via IPv4-compatible IPv6 notation** (`::10.0.0.1`, distinct from the
  correctly-blocked mapped form `::ffff:10.0.0.1`) — real gap in
  `IsBlockedIP`, confirmed by reproducing it, then confirmed *not*
  exploitable by dialling the equivalent literal against a real listener: no
  mainstream OS routes the deprecated form to its embedded target (RFC 5156,
  2008). **Fixed anyway 2026-08-18** — see below.
- Captive portal open redirect (`link-orig` host unchecked) — real and
  reachable, but leaks no session/voucher/credential; a generic open
  redirect, below this review's stated bar for that class.
- Captive portal NAS-scoping "bypass" via client-supplied `nasid` — refuted.
  The value never reaches the actual RADIUS-time authorisation check, which
  resolves the NAS from the physical device's own packet source address, not
  anything the HTTP client sends.
- NAS secret handling (the new `/api/v1/nas` API) and EAP-MSCHAPv2 session
  state — no findings from either investigation.

### Ship-readiness punch-list, closed 2026-08-18

Four concrete gaps identified when assessing ship-readiness, worked in
parallel with the 1-hour soak test below:

**SSRF blocklist gap, patched.** `IsBlockedIP` now recognises IPv4-compatible
IPv6 addresses (`embeddedIPv4Compatible` in `internal/partner/ssrf.go`),
closing the gap the security review found non-exploitable but real. Test
extended with the previously-missing cases; a negative control (reverting the
fix) confirmed the test catches it.

**Archive retrieval, built.** `archive.Store` had `Put` and `Delete` but no
way to read a document back — NFR-DUR-002's "verify on retrieval" was
untestable because there was no retrieval. Added `Store.Get`,
`Archiver.Retrieve` (fetches and re-verifies against the recorded SHA-256,
returning `ErrChecksumMismatch` rather than silently handing back corrupted
bytes), and `Archiver.RetrieveLatest` (looks up the live row and retrieves it
in one call). Tested against both the in-memory recorder and, separately,
real PostgreSQL plus a real filesystem — corrupting the on-disk file and
confirming the real `db.ArchiveStore`-backed path catches it, not just the
test double. Negative control (removing the checksum comparison) confirmed
the test catches the regression.

**Sentinel failover, retargeted and live-verified.**
`down-after-milliseconds` lowered from the default 3000 to 500 in
`config/redis/sentinel.conf`; the budget in `TST §13.4` and `SAD` moved from
3s to 8s, since the 3s target was shown unachievable on this topology rather
than merely unmet (5055ms at the old setting; 3086–3192ms at the lowest
detection setting that does not risk false-positive failovers on ordinary
network jitter — detection, not promotion, dominates the number). Verified
2026-08-18 with a clean run of `scripts/run_sentinel_failover_test.sh
DOWN_AFTER_MS=500 ELECTION_BUDGET_MS=8000 AUTH_RESUME_BUDGET_MS=10000`, run
after the soak test below released the shared demo-stack Sentinel cluster
(the two cannot run concurrently — the failover script's chaos step kills
`redis_primary` on the same stack the soak test loads):

- Detection (fault → `+sdown`): 1085ms, bounded by `down-after-milliseconds=500`
- Failover sequence (`+sdown` → `+switch-master`): 1828ms
- **Master elected: 2913ms (budget 8000ms) — PASS**
- **RADIUS auth resumed: 5784ms after the kill (budget 10000ms) — PASS**
- Load generator across the failover: 17998 requests, 0 errors, p99 19.9ms

L6-004 is now closed: both halves of NFR-AVAIL-001 pass against the
retargeted budget with real load in flight throughout.

**Four lint findings, fixed rather than suppressed.** All four confirmed
genuine on inspection, not blanket-suppressed:

- `internal/api/invoices.go` (G705) — initially mis-diagnosed against the
  wrong line (`Content-Disposition`, an `int`-only format with no possible
  taint) before re-checking gosec's actual line number, which was
  `w.Write(pdfBytes)`. Real fix: added `X-Content-Type-Options: nosniff`
  alongside the existing `Content-Type: application/pdf`, which is the
  concrete answer to what the finding gestures at — a browser cannot execute
  PDF bytes as HTML/JS regardless of content when nosniff is set.
- `internal/staffui/screens.go` (G710) — genuinely unvalidated:
  `subscriberID` was raw `PostFormValue`, concatenated unescaped into a
  redirect URL. Not an open redirect (the target is a fixed literal path,
  never attacker-controlled), but a real query-parameter-injection gap.
  Fixed with `url.Values.Encode`.
- `internal/partner/dispatch.go` (G117) — a struct field literally named
  `Secret` held ciphertext, which is both what triggered gosec's
  naming-pattern check and a real readability trap. Renamed to
  `SecretEncrypted`; wire format (`json:"secret_encrypted"`) unchanged.
- `internal/tr069/acs.go` (G124 + ST1005) — the CWMP session cookie had
  `HttpOnly` but not `Secure`/`SameSite`; added both (this endpoint is on the
  same TLS-terminated mux as everything else, so `Secure` costs nothing). An
  unrelated capitalised error string in the same file fixed alongside it.

**1-hour soak test (L6-005).** `scripts/run_soak_test.sh`, the full 3600s
hold at 333 req/s against 20,000 subscribers, ran to completion 2026-08-18
(119 samples, ~3600s). The script's own verdict:

- **PASS** — goroutines stable across the hold (1.4% growth, tolerance 10%)
- **FAIL** — open file descriptors grew 15.7% across the hold (tolerance
  10%), flagged "probable leak"

The FD result does not hold up as a leak on inspection of the raw series.
Growth was not continuous: FDs sat flat at 167–170 from 305s to 1889s, then
stepped once to ~197 over a single ~120s window (1889s→2010s, +28), then
stayed flat at 197–198 for the remaining ~1580s (26+ minutes) through 3467s,
dipping to 192 at the final sample. A genuine unbounded leak grows
continuously and does not plateau for half an hour, let alone dip. Two
follow-up checks against the still-running daemon (before it was recreated by
the later Sentinel test below) reinforced this: `process_open_fds` read 186
shortly after the load generator stopped — *below* the 197–198 plateau, i.e.
already releasing, not still climbing — and the FD breakdown inside the
container was 179/186 sockets, consistent with pooled DB/Redis connections
rather than leaked handles. The size of the one-time step (+28) is in the
right range for the pgx pool growing from `DBMinConns=5` toward
`DBMaxConns=25` under sustained load, plus the Sentinel-managed Redis pool.
*(Read with one correction: Redis has since been removed from the system
entirely, so on today's build the pgx pool is the whole explanation. That
does not change the conclusion, but a re-run would have fewer sockets.)*
Read together, this is pool growth to a new steady state, not a leak — and
it is also a real limitation of the soak script's pass/fail logic: a flat
two-point comparison (settle-point vs end-point) against a fixed tolerance
cannot distinguish "grew once to a new plateau" from "still growing," which
is exactly the ambiguity here.

This conclusion rests on the shape of the metric series and FD composition,
not a direct allocation profile — the soak run did not have pprof attached.
Treating L6-005 as closed on this evidence; if a future longer or
pprof-instrumented run is done, prefer its result over this one.

### What this update does not close

- **Archive backend is local-filesystem only.** The `Store` interface is the
  seam for S3/SFTP; neither is implemented.
- **Four NAS vendors are doc-derived, not bench-verified.** Huawei, ZTE,
  Cisco WLC and Ruckus rate-limit/QoS attribute type numbers came from public
  documentation and were never checked against deployed firmware (7
  `TODO-VERIFY` markers in `internal/nas/{huawei,zte,wireless}.go`).
  MikroTik is the one vendor actually tested this session.
- **`staff_users` has no MFA, lockout, or password rotation.**
- **TLS is self-signed only**, configured for the demo; no committed
  production-certificate deployment guide.
- **Payment gateway needs real credentials** (`RAZORPAY_KEY_ID`/`SECRET`) —
  operational, not code.
- **106 of the 110 remaining lint findings are untriaged.** The four fixed
  today were specifically the ones flagged as real during the ship-readiness
  assessment; the rest were not reviewed row by row (mostly `noctx` in tests
  per prior characterisation in `HANDOFF.md`, not independently reconfirmed
  here).

## Critical Findings (read this part first)

> **Findings 1-5, 7 and 8 are RESOLVED; Finding 6 is OPEN.** They are
> kept in full rather than deleted, because each records a real defect that was
> live in this codebase, and a resolution note is only meaningful next to the
> diagnosis it resolves. Each resolved finding carries a ✅ **Resolved** note at
> its end.

### 1. Nothing has ever been committed to git except documentation
`git log` shows exactly one commit, `9d47cb6 "first commit"`, containing only `README.txt`, the three tracker `.xlsx` files, and `specification_docs_v2/*.md` — **18 files, zero lines of code**. Every single line of Go, every migration, every config file, every Dockerfile, `go.mod`/`go.sum` — the entire application, including all six months-worth-looking of module work and all six Portal UI phases built across this session — sits **untracked in the working directory**. `git status` currently shows 16 top-level untracked paths (`cmd/`, `internal/`, `migrations/`, `pkg/`, `scripts/`, `config/`, `go.mod`, `go.sum`, `docker-compose.yml`, both Dockerfiles, `.golangci.yml`, `.gitignore`, `.githooks/`, `.env.example`).

This single fact is the root cause of most of the L0/L7/L8 failures below (L0-001, L0-002, L0-009, L0-010, L7-001, L7-003, and all of L8) — they all assume a commit history that does not exist yet. **This is not a code-quality problem; it's a "the work has never been checked in" problem**, and it's the highest-leverage single fix available: committing what exists today would immediately resolve L7-003 outright and materially change several others.

✅ **Resolved** (commit `ae75ee4`, 2026-08-08). The full application — `cmd/`, `internal/`, `migrations/`, `pkg/`, `scripts/`, `config/`, `go.mod`/`go.sum`, both Dockerfiles, `docker-compose.yml` — is now tracked. Every commit since has been verified by cloning the repository to a clean directory and building it there in an isolated container, so "it builds from a fresh clone" is a tested claim rather than an assumption. L7-003 is now PASS. L0-001/L0-002 remain FAIL and always will: TDD red-phase ordering cannot be demonstrated retroactively for code that was written before it was ever committed.

### 2. The LEA audit log is not actually tamper-proof (L1-009 / L4-010)
Migration 014 enables Row-Level Security on `lea_audit_log` with an INSERT-only policy, intended to make it append-only per DBD §6.5. I tested this live: **the UPDATE succeeded and the row was tampered.**

```
UPDATE lea_audit_log SET accessor_identity = 'tampered' WHERE ...;
UPDATE 1
SELECT accessor_identity FROM lea_audit_log WHERE id = ...;
 tampered
```

Root cause: `docker-compose.yml` has the application connect to Postgres as `POSTGRES_USER=postgres` for everything — there is no separate least-privilege application role anywhere in the stack. PostgreSQL RLS **always** exempts the table owner (and always exempts literal superusers, regardless of `FORCE ROW LEVEL SECURITY`), and `postgres` here is both. So the exact role the application uses in production can silently rewrite or delete LEA lookup audit records — the one table that exists specifically to be tamper-evident for compliance. This is a real, exploitable gap, not a theoretical one.
**Fix requires:** a dedicated non-superuser, non-owner Postgres role for the application (`GRANT INSERT, SELECT` only on `lea_audit_log`, no `UPDATE`/`DELETE`), wired through a new `DB_DSN` — a real scope change, not a one-line patch.

✅ **Resolved** (migration `019_create_app_role.sql`, commit `9cdb1dd`). A `bss_app` role that is neither superuser nor table owner now exists, so RLS is no longer bypassed — PostgreSQL exempts owners and superusers from RLS, which was the entire root cause. The same UPDATE that previously tampered a row is now rejected. L1-009 and L4-010 are both PASS.

### 3. RADIUS auth still cannot meet its 15ms p99 budget (L3-003 / L6-001)
`internal/radius/handlers.go` calls `d.db.GetSubscriberByUsername` (straight to Postgres, no cache) followed by `bcrypt.CompareHashAndPassword` at cost 12 on every single Access-Request. This matches a gap already known from earlier in this engagement (bcrypt cost-12 alone measured at ~282ms/comparison — ~19× the 15ms budget) and confirms the previously-agreed mitigation — a fast verifier cache in Redis for the RADIUS path specifically — **was never implemented**. It was not part of any phase actually requested this session (all six phases built were Portal UI). Flagging it here since it's a real, currently-failing NFR, not new information, but it has not been revisited.

✅ **Resolved** (`internal/radius/verifiercache.go`, commit `9cdb1dd`) and now **measured**, not merely reasoned about. At 5,000 req/s against 20,000 distinct seeded subscribers: **p99 10.4ms**, 149,850 requests, 0 errors. Daemon-side counters show 249,850 cache hits against 39,960 misses, confirming the cache — not a faster database — is what carries the result.

The cache stores `HMAC-SHA256(secret, password ‖ passwordHash)`, never the password or the bcrypt hash. Binding to the *current* password hash is what makes a password change self-invalidate every cached verifier for that subscriber immediately; keying on the password alone would have kept accepting a rotated-away password for the full 5-minute TTL. A wrong guess cannot forge a matching verifier without the server-side secret, so incorrect passwords still fall through to the full bcrypt comparison exactly as before.

### 4. `go.mod` / `go.sum` are not committed (L7-003)
Direct consequence of Finding #1, called out separately because it's independently checkable and independently blocking: a fresh `git clone` of this repository today would not even build, since the module files aren't in git.

✅ **Resolved** (commit `ae75ee4`). Re-verified at every commit since by cloning to a clean directory and running `go build ./...`, `go vet -tags=integration ./...` and the test suite there in an isolated container.

### 5. The NFR harness could not have passed since the verifier cache landed *(found 2026-08-10)*

This one was found while running the load tests rather than reading the code, and is recorded because the failure it produced was actively misleading.

`scripts/run_nfr_tests.sh` never set `RADIUS_VERIFIER_SECRET`. Finding #3's fix made that variable **required** at radiusd startup, so from the moment the fast-verifier cache landed the daemon exited during config validation on every NFR run. The container had `--rm`, so it deleted itself on exit and left no logs behind.

What the harness reported was:

```
requests    : 1920 (accept 0, reject 0, error 1920)
throughput  : 64 req/s
error rate  : 100.0000%
FAIL: NFR-PERF-001: RADIUS p99 (threshold 15ms) at 2000 req/s
```

That reads as "RADIUS authentication is catastrophically slow." The daemon was not running at all. Two further problems in the same harness:

- `RATE` and `API_VUS` defaulted to 2,000 and 100 while the DoD specifies **5,000 req/s** (L6-001) and **500 concurrent** (L6-002) — so a fully green run demonstrated roughly a fifth of the required load in both dimensions. Both now default to the DoD levels.
- `scripts/load_test_radius.sh` shelled out to `radperf`, a third-party binary that is not vendored and not installed by any setup step in this repository, so it exited at its own `command -v radperf` check on every machine and had never measured anything. It now drives `cmd/radload`, which needs no external install.

✅ **Resolved** (commit `cb1d2bf`). All three fixes are in the committed harness, and the numbers in L6 below come from runs with them applied.

### 6. Sentinel failover misses its budget, and hostname monitoring has a hard failure mode *(found 2026-08-10, CLOSED — twice)*

**Closed on 2026-08-18**, by adopting exactly the recommendation below:
`down-after-milliseconds` lowered to 500 and the budget retargeted from 3s to
8s, then live-verified at 2913ms election and 5784ms auth resume with 17,998
requests and no errors in flight. The detail is under "Post-v1.0.0 Update"
above.

**Then closed again, more thoroughly: the component no longer exists.**
`1ae0232` replaced Asynq with a PostgreSQL-backed job queue and removed Redis
from the system entirely — `a1477d6` took the tier out of Docker, and
`config/redis/sentinel.conf` and `scripts/run_sentinel_failover_test.sh` went
with it. There is no Sentinel to tune, no DNS failure mode to accept, and
nothing left for a reader to decide. Verified 2026-11-23: no Redis client in
`go.mod`, no Redis service in either compose file, and the only `redis` left
in Go code was a vestigial `RedisQuerier` interface name, now
`health.SessionQuerier`, which a PostgreSQL store satisfies.

Everything below is kept as the 2026-08-10 record, because the two
measurement defects under "How the first measurement lied" are the
transferable part and outlast the component.

> ⚠️ **Correction (same day).** The first published version of this finding said the budget was "not reachable by tuning `down-after-milliseconds`". **That conclusion was wrong**, and it was wrong because of two defects in the measuring instrument, not because of anything Sentinel did. Both are fixed and the numbers below are the corrected ones. The two defects are written up under "How the first measurement lied" because they are the more transferable lesson.

**The budget is missed, but narrowly, and detection dominates.** Measured from Sentinel's own event log on a clean stack:

| `down-after-milliseconds` | Detection (fault → `+sdown`) | Failover (`+sdown` → `+switch-master`) | **Total** |
|---|---|---|---|
| 3000 (committed) | 4110ms | 945ms | **5055ms** |
| 500 (verified applied) | 964ms | 2228ms | **3192ms** |
| 500 (repeat) | 883ms | 2203ms | **3086ms** |

Detection scales with the knob almost exactly as expected, and tuning it takes the total from **~5.1s to ~3.1s** — still over a 3000ms budget, but by roughly 100–200ms rather than by 2 seconds. Sentinel's own promotion sequence is 0.9–2.2s and varies run to run.

**How the first measurement lied.** Worth recording, because both defects produced confident, plausible, wrong numbers:

1. *The instrument was slower than the thing it measured.* Election time was taken from a poll loop calling `docker compose exec … redis-cli`, at a measured **~390ms per call** — while the event being timed completes in under a second. The loop was reporting its own latency. It now reads Sentinel's millisecond-resolution event log instead, which is authoritative and free of observer cost.
2. *The config override never took effect.* The compose override file was passed as `/tmp/…`, and with `MSYS_NO_PATHCONV=1` set (needed for other Docker arguments) the Docker Desktop binary resolved that raw POSIX path against the current drive as `D:\tmp\…` and could not find it. The command's output was silenced, so it failed quietly. **Every "tuned" run was actually running the committed 3000ms threshold** — which is precisely why detection appeared not to respond to the knob. The script now passes a Windows path, does not silence the command, and afterwards queries `SENTINEL MASTER` to confirm the value actually in force before measuring anything.

The second one is the sharper lesson: a silently-ignored override does not fail, it produces credible numbers for a configuration you are not running.

**Separately: hostname monitoring cannot survive a vanished DNS name.** `sentinel.conf` monitors the master as `sentinel monitor bss_master redis_primary` with `resolve-hostnames yes` — which its own comments correctly note is mandatory under Docker Compose, since Redis 7.4+ treats a hostname in `sentinel monitor` as fatal without it. But when the master container is removed rather than merely stopped, the name stops resolving and Sentinel logs:

```
# Failed to resolve hostname 'redis_primary'
# +tilt #tilt mode entered
```

It then never promotes a replica at all — 60 seconds later there is still no master and authentication has not recovered. Hostname monitoring survives a dead *process* but not a vanished *name*, and a node failure under an orchestrator is precisely the second case. `scripts/run_sentinel_failover_test.sh` reproduces this with `CHAOS_MODE=kill`; the default `pause` mode keeps the container in DNS so the script measures failover timing rather than re-demonstrating this each run.

**Decision needed** (not a code fix). The recommendation recorded for the tracker is: **lower `down-after-milliseconds` to 500 and move the target from 3s to 8s.** Rationale — 3s was never met even in principle by the shipped config, the tuned configuration lands at ~3.1s with run-to-run variance of ±100ms so a 3s target would be permanently flaky, and 8s leaves headroom for the promotion sequence's observed 0.9–2.2s spread while still being far inside any realistic subscriber-visible tolerance for a re-auth. The DNS/tilt behaviour is a separate defect that a target change does not address and should be tracked as its own item.

### 7. An unauthenticated endpoint leaked subscriber diagnostics *(found 2026-08-10, RESOLVED)*

Found while answering a stakeholder question about whether an admin UI existed — not by any check on this list, which is the part worth dwelling on.

`GET /api/v1/subscribers/{id}/health-detail` returned a subscriber's username, account status, wallet balance, live session, NAS address and **assigned IP** to any caller, with no token, through the public reverse proxy:

```
$ curl -k https://host/api/v1/subscribers/1/health-detail
{"subscriber_id":1,"username":"test_user","status":"active",
 "wallet_balance":"799","active_session":{"assigned_ip":"100.64.0.7",
 "nas_ip":"10.10.0.1",...},"open_tickets":0}
```

It was enumerable — `1` and `2` returned data, `3` returned 404, so account existence was discoverable too — and nothing was written to `lea_audit_log`.

**The leaked field that matters is `assigned_ip`.** Migration 014, the `bss_app` least-privilege role and `POST /api/v1/lea/lookup` exist specifically to keep IP-to-subscriber correlation access-controlled and tamper-evident. This route handed out the same correlation with no authentication and no audit trail, routing around the control Finding #2 was spent building. Fixing the database layer did not matter while an HTTP route gave the answer away for free.

**Cause.** `cmd/api/main.go` registered the real handler with `mux.HandleFunc` *after* `apiHandler.RegisterRoutes`. A route added to the mux that way carries no middleware. Its comment read "api.Handler only reserves the route, so bind the real implementation over it here" — but it bound a *different* path, so nothing was overridden: the documented route answered 501 while an undocumented one answered 200 with the data. The intent was right; the path was wrong, and the mistake was invisible because both routes existed and only one was ever tested.

**Why no check caught it.** L4-005 and L4-006 both pass and always did — the middleware was never broken. Every test aimed at this endpoint asserted the *placeholder's* 501, so the suite confirmed the documented route behaved as designed while the real data left by a door nobody had written a test for. A checklist that verifies middleware behaviour cannot catch a route that never reaches the middleware.

✅ **Resolved** (commit `4b6c2f8`). The handler is now injected through `api.HandlerDeps` and served behind the same `staffRead` authorisation as its neighbours; the `health-detail` registration is deleted. This also implements **FR-OBS-004**, which had been returning 501 while a complete, fully-covered implementation sat unused in `internal/health`. Four regression tests were added — the significant one asserts the handler is never *reached* without a token, since a 401 returned after the body was written would still be a disclosure. The three remaining raw mux registrations were audited: both WhatsApp webhooks self-verify and `/readyz` is a deliberately public probe.

### 8. Three components were built, tested, and never run *(found 2026-08-11, RESOLVED)*

Found by asking a question no check on this list asks: not "is it tested" but "is it called".

| Component | Tested | Called in production code | Consequence |
|---|---|---|---|
| `TransitionDunning` — the 9-edge dunning ladder | ✅ all edges | ❌ none | Nobody was reminded to pay; nobody was suspended for not paying |
| `ReconcileJob` — nightly revenue reconciliation | ✅ | ❌ never constructed | No revenue snapshot, no ledger-variance comparison, ever |
| `TMPL-002`…`007` | registry ✅ | ❌ nothing dispatched them | Five of seven notification types could not be sent |

None of it failed a test, because nothing was **wrong** — there was simply nothing there to fail. The suite was green, coverage was measured, and the features did not run. `ReconcileJob` even carried the comment "runs nightly at 02:00 IST via Asynq cron", and the radiusd image had installed tzdata for it. CRD **BO-003** promised "dunning automation" as a headline business outcome; the automation existed and was never started.

✅ **Resolved** (commits `4ac2004`, `8dbb469`). Both scanners now run in radiusd, and TMPL-003/004/005 dispatch on their real triggers. Verified live: a subscriber 40 days overdue and sitting at `active` climbed the whole ladder to `hard_suspended` with `subscribers.status` following, so RADIUS actually refuses them.

**A second defect surfaced only because the first was fixed.** With notifications finally firing, a WhatsApp rejection turned out to write no `notification_log` row at all — an operator could not distinguish "we never tried to warn this subscriber" from "we tried and Meta refused". Row **L3-005** recorded that requirement as passing on the strength of a code read. Failed attempts are now logged with `delivery_status = "failed"`.

**L0-012 exists so this class of defect fails a check rather than a customer.**

---

## L0 · Every Task (10 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L0-001 | Test written before impl (commit order) | ⬜ N/A (accepted 2026-08-11) | Unachievable retroactively: the code existed before it was first committed, so no red-phase commit can be produced for it. Marked N/A by decision rather than left as a permanent failure that no work can ever clear. **Applies to pre-v1.0.0 code only** — new modules are expected to show test-before-impl ordering, and this row should be re-graded, not inherited. |
| L0-002 | Test fails before impl (red phase) | ⬜ N/A (accepted 2026-08-11) | Same basis as L0-001. Red→green was observed live for individual edits during remediation, but the DoD check is git-log-based and that history cannot be reconstructed. Same forward-looking expectation applies. |
| L0-003 | `go build ./...` clean | ✅ PASS | Fresh run this audit: exit 0, no output. |
| L0-004 | `go vet ./...` clean | ✅ PASS | Fresh run this audit (`-tags=integration`): exit 0, no output. |
| L0-005 | Race detector clean ×3 | ✅ PASS | Fresh run this audit, full repo, 3 consecutive runs, 0 data races each time. |
| L0-006 | `golangci-lint` clean | ✅ PASS | Fresh run this audit: "0 issues." |
| L0-007 | No float64 for money | ✅ PASS | Literal grep (`float64\|math.Round\|Sprintf.*%.2f`) across all of `internal/`, `pkg/`, `cmd/`: 8 hits, all inspected — Prometheus metrics, percentage fields (`PctUsed`), latency histogram buckets, a load-test CLI's error-rate calc, and two comments *documenting* the no-float64-for-money rule. Zero money usage. |
| L0-008 | No plaintext PII in log statements | ✅ PASS | Grep for aadhaar/pan/mobile_number piped through `log\.`: zero matches. |
| L0-009 | Pre-commit hook passes on staged files | ✅ PASS | Installed 2026-08-10 via `./scripts/install-hooks.sh`. Verified two ways rather than assumed: `scripts/int_pii_001_precommit_hook.sh` passes all 8 sandbox cases (blocks raw aadhaar/pan/mobile/password in log calls, permits `*_encrypted`/`*_hash` and non-Go files), **and** a deliberate violation staged in this repo was blocked by the installed hook on a real `git commit`, leaving `HEAD` unmoved. |
| L0-010 | Conventional commit format | 🟡 PARTIAL | 4 of the 5 commits now conform (`feat:`, `fix:`, `feat:`, `test:`). The sole exception is the original docs-only root commit `9d47cb6 "first commit"`, which predates the convention and cannot be reworded without rewriting published history. Every commit containing code conforms. |
| **L0-012** | **Every non-CLI component has a caller outside test files** | ✅ PASS | **New rule, added 2026-08-11.** `./scripts/check_wiring.sh` — 11 of 11 components wired. Added because three shipped complete, tested and never run: the dunning state machine, `ReconcileJob`, and five of the seven notification templates. See "Why this rule exists" below. |

### Why L0-012 exists

Three components passed every check on this list while doing nothing at all:

| Component | Consequence of never running |
|---|---|
| `TransitionDunning` | No subscriber was ever reminded to pay or suspended for not paying |
| `ReconcileJob` | No revenue snapshot, no ledger-variance comparison, ever |
| `TMPL-002`…`007` | Templates registered; nothing dispatched them |

None of this failed a test, because nothing was *wrong* — there was simply nothing there to fail. The suite was green, coverage was measured and reported, and the features did not run. CRD **BO-003** promised "dunning automation" as a headline business outcome; the automation existed and was never started.

A checklist that asks *is it tested* cannot catch this. Only asking *is it called* can, which is a grep, not a test:

```bash
./scripts/check_wiring.sh          # exit non-zero if anything is unwired
./scripts/check_wiring.sh --list   # show what is tracked and why
```

The check was verified against a deliberately unwired component, so it fails when it should rather than passing vacuously. Add a component to its list when "nobody calls this" would be a silent production outage rather than dead code a linter would already flag.

## L1 · DB Migration (9 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L1-001 | Migrations apply cleanly on empty DB | ✅ PASS | All **20** migrations apply cleanly against a fresh throwaway `postgres:15-alpine` — re-confirmed repeatedly during the load-test runs, which rebuild the schema from zero on every invocation. |
| L1-002 | Migrations roll back cleanly (Down works) | ✅ PASS | Full round trip re-tested at **all 20** migrations (2026-08-10): `up` → version 20, `down-to 0` → version 0, `up` again → version 20. Every Down statement succeeded, including the new `019` and `020`. |
| L1-003 | Columns match DBD §6.2 exactly | 🟡 PARTIAL | Spot-checked (types/defaults verified for tables touched this session — `subscribers`, `wallet_ledgers`, `invoices`, `tickets`, `plans`, `subscriber_session_history`); not exhaustively diffed column-by-column against DBD §6.2 for all 17 logical tables. |
| L1-004 | All DBD §6.4 indexes exist | 🟡 PARTIAL | 69 indexes exist on the schema; not exhaustively cross-referenced line-by-line against DBD §6.4. |
| L1-005 | INCLUDE clause on LEA index | ✅ PASS | `psql`: `idx_lea_ipv4_time ... INCLUDE (subscriber_id, stop_time)` confirmed present verbatim. |
| L1-006 | Constraint enforcement (chk_gst_logic) | ✅ PASS | Live test: inserting an invoice with both `cgst_amount` and `igst_amount` set → `ERROR: violates check constraint "chk_gst_logic"`. |
| L1-007 | Partitioned tables have ≥3 future partitions | ✅ PASS | `subscriber_session_history`: parent + `_2026_08` (current) + `_2026_09/10/11` (3 future) = matches `create_monthly_partitions(..., 3)` design exactly. |
| L1-008 | FK constraints enforced | ✅ PASS | Live test: inserting a subscriber with `plan_id=9999999` → `ERROR: violates foreign key constraint "subscribers_plan_id_fkey"`. |
| L1-009 | `lea_audit_log` blocks UPDATE via RLS | ✅ PASS | Was the critical failure in Finding #2. Migration `019_create_app_role.sql` adds the least-privilege `bss_app` role (neither superuser nor table owner), so RLS is no longer bypassed and the previously-succeeding UPDATE is rejected. |

## L2 · Unit Tests (6 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L2-001 | Every FR has a `TestFR_{ID}_...`-named test | ✅ PASS | **102 tests** now carry the `TestFR_` prefix, covering **all 30 distinct FR IDs** referenced anywhere in the codebase. Renaming was scoped to tests whose doc comments already carried an FR ID, so every mapping has a source in the code rather than one invented to satisfy a count; the descriptive suffix is retained (`TestFR_BIL_003_WalletRecharge_DoubleLedger`). The remaining ~175 tests are deliberately left alone — a guessed FR ID is worse than an absent one. |
| L2-002 | Coverage ≥80% (≥90% crypto/middleware) | ❌ FAIL (improved) | **The ≥90% sub-requirement now passes**: `pkg/crypto` 58.7% → **93.5%**, `internal/middleware` 69.6% → **98.2%**. The general ≥80% bar does not: overall 53.1% → **60.4%** (merged native + real-Postgres profiles). Movers across this remediation: `internal/cache` 47.7% → **88.4%**, `internal/billing` 53.6% → **87.5%**, `internal/db` **76.9%**, `internal/api` 55.4% → **69.3%**, `internal/radius` 55.5% → **68.3%**. Also passing: `pkg/validate` 100%, `internal/health` 100%, `internal/revenue` 80.7%. Still short: `internal/portalui` 79.6%, `internal/fup` 76.8%, `internal/notifications` 71.7%, `internal/portal` 63.0%. **`cmd/api` (5.3%) carries an accepted exemption — see below.** |

### Accepted exemption: `cmd/api` startup coverage

**Accepted 2026-08-11.** `cmd/api/main.go` is excluded from the ≥80% target.

Its uncovered functions are `main`, `run`, and the config-to-struct helpers: a linear sequence of load config → dial PostgreSQL → dial Redis → build handlers → `ListenAndServe`. Covering them requires extracting a testable `newServer()` from `run()`, which means restructuring service startup — the one file where a mistake takes the whole API down — to satisfy a coverage number rather than to fix a defect.

The risk it would buy down is already covered elsewhere: every handler `run()` wires is tested in `internal/api`, and `./scripts/check_wiring.sh` (L0-012) now catches the failure mode that startup wiring actually produces, which is a component built and never called.

What is **not** exempt: the pure helpers in that file. `dbConfig`, `asynqRedisOpt`, `newRedisClient`, `configureLogging`, `readinessHandler` and `requestLogger` are table-driven or `httptest`-able with no refactor, and remain fair targets — roughly half a day for `cmd/api` to reach 45–55%. The exemption covers `main` and `run` only.
| L2-003 | Happy-path tests exist | ✅ PASS | Present throughout (not literally suffixed `_HappyPath`, but functionally covered — e.g. every new Phase 0–6 handler has a valid-input test). |
| L2-004 | Error/rejection-path tests exist | ✅ PASS | Present throughout (e.g. `TestLogin_InvalidPassword_RerendersFormWithError`, `TestRenew_InvalidCSRFToken_Returns403`). |
| L2-005 | Edge cases (nil/zero/empty) handled without panic | ✅ PASS | Verified pattern throughout this session's own work (e.g. `TestDashboard_OfflineSubscriber_ShowsEmptyState`, `TestUsage_NoHistory_ShowsEmptyState`) and pre-existing tests. |
| L2-006 | Decimal precision to 2dp (billing) | ✅ PASS | `TestCalculateGstInvoice_Intrastate`/`_Interstate` assert exact `"71.91"`-style strings, not float comparisons. |

**Caveat on the "220 tests exist" figure used throughout L2.** Raising coverage in `internal/api` and `internal/billing` surfaced four tests that passed without testing what their names claimed, which means test *count* is a weaker signal here than it looks:

- `TestDunningTransition_ValidEdge` assigned its inputs to `_` and never called `TransitionDunning` at all.
- `TestDunningToSubscriberStatus` compared `billing.DunningActive` against the literal `"active"` it is defined as — an assertion that could not fail — and never reached the unexported `dunningToSubscriberStatus` it was named for.
- `TestGetSubscriberNotFound` and `TestWalletRechargeValidation` both stopped at the 401 auth gate, never reaching `GetSubscriber` or `WalletRecharge`.

All four are fixed in commit `cb1d2bf` (renamed where they were testing something real but mis-named, rewritten where they were not). The dunning tests now drive a fake querier through all nine edges in `validTransitions` and assert the state and status actually persisted. These were found only because coverage measurement pointed at functions reported as 0% despite having named tests — which is a good argument for keeping the L2-002 coverage gate rather than trusting the test count.

## L3 · Integration (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L3-001 | Docker stack healthy before integration tests | ✅ PASS | All 11 demo containers currently `Up`/`healthy` (postgres, redis×6, gotenberg, aaa_core_daemon, api_service, reverse_proxy). |
| L3-002 | Integration suite passes (`-tags=integration`) | ✅ PASS | Run repeatedly and successfully throughout this session; full `internal/db` suite re-confirmed clean this audit (63s, all green). |
| L3-003 | Redis actually used on the RADIUS auth hot path | ✅ PASS | `internal/radius/verifiercache.go` puts a Redis-backed fast verifier directly on the auth path. Confirmed in the load run by daemon-side counters, not just by code reading: 249,850 cache hits vs 39,960 misses at 5,000 req/s. |
| L3-004 | Asynq task actually enqueued in correct queue | ✅ PASS (code-verified) | Confirmed real `asynq.NewTask(...)` + `.Enqueue(...)`/`.EnqueueContext(...)` call sites for PoD, CoA, and FUP-warning tasks in `internal/api/sessions.go` and `internal/fup/scanner.go`. Not observed live via the `asynq` CLI this audit. |
| L3-005 | `notification_log` row created per dispatch attempt | ✅ PASS (now executed, not inferred) | **Was wrong when code-verified.** `Dispatcher.Dispatch` does log the `suppressed_dnd` path, but delegates sending to `WhatsAppClient.SendTemplate`, which wrote a row only on success — a provider rejection left no trace, so an operator could not tell "never attempted" from "attempted and refused". Found by running it once notifications actually fired (Finding #8). Failed attempts now log `delivery_status = "failed"`, covered by `TestFR_NOTIF_009_SendTemplate_LogsFailedDispatch`. |
| L3-006 | Idempotency: same `transaction_token` twice = single credit | ✅ PASS | `TestRenewal_IdempotentCallback`, `TestRenewal_DistinctPaymentsBothCredit` exist and pass; this session additionally verified the renewal *expiry* side of idempotency live (Phase 4). |
| L3-007 | DND subscriber gets `suppressed_dnd` in log | ✅ PASS | Tested at both unit (`notifications_test.go`) and integration (`integration_test.go`) level; DND check confirmed (via direct code read) to unconditionally precede both the WhatsApp and SMS send branches. |
| L3-008 | Webhook HMAC: invalid signature → 400, no state change | ✅ PASS | `TestWebhookHMAC_InvalidSignatureRejected`, `TestWebhookHMAC_TamperedBodyRejected`, `TestRazorpayWebhook_InvalidSignatureRejected` all exist. |

## L4 · Security (10 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L4-001 | Zero plaintext Aadhaar/PAN in codebase logs | ✅ PASS | Grep scan clean. |
| L4-002 | Zero plaintext PII in DB column values | ✅ PASS (code-path verified) | `UpsertKYC` only ever receives pre-encrypted ciphertext from the API handler layer by construction (per its own doc comment); `Encrypt()` always produces the `{version}:{base64}` format. No live KYC rows currently seeded in the demo DB to visually spot-check. |
| L4-003 | AES ciphertext non-deterministic | ✅ PASS | `TestEncryptNonDeterministic` exists in `pkg/crypto`. |
| L4-004 | Cross-rotation decryption works | ✅ PASS | `TestCrossRotationDecrypt`, `TestDecrypt_CrossKeyVersion` exist. |
| L4-005 | Expired JWT → 401 | ✅ PASS | `TestJWTMiddleware_ExpiredToken` exists. |
| L4-006 | Wrong role → 403 | ✅ PASS | `TestRequireRole_WrongRoleReturns403`, `TestRequireRole_RoleMatrix` exist. **Re-verified 2026-08-10** after Finding #7: middleware coverage was never the problem, a route registered outside it was. Every `/api/v1/*` route is now confirmed to sit behind `RegisterRoutes`. |
| L4-007 | `hmac.Equal` (timing-safe), not `==` | ✅ PASS | Confirmed at all 3 HMAC comparison sites: `internal/billing/webhook.go`, `internal/notifications/webhook.go`, and this session's own `internal/portalui/auth.go` CSRF check. |
| L4-008 | HMAC validated before `json.Unmarshal` | ✅ PASS | `internal/api/webhook_razorpay.go`: `ValidateRazorpaySignature` at line 61, `json.Unmarshal` at line 68 — correct order. |
| L4-009 | `gosec` clean | ✅ PASS | `gosec` is enabled in `.golangci.yml`'s linter set; fresh `golangci-lint run` this audit: 0 issues. |
| L4-010 | LEA audit log append-only (DB-level) | ✅ PASS | Same fix as L1-009 — see Finding #2's resolution note. |

## L5 · Notifications (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L5-001 | WhatsApp uses `"type": "template"` | ✅ PASS | Confirmed in `internal/notifications/whatsapp.go:133-134`. |
| L5-002 | `provider_message_id` saved after send | ✅ PASS | Flows from `whatsapp.go` through to `internal/db/notifications.go`'s insert. |
| L5-003 | Meta webhook updates `delivery_status` sent→delivered | ✅ PASS | `TestWhatsAppWebhook_UpdatesDeliveryStatus` exists and exercises exactly this transition. |
| L5-004 | Meta webhook HMAC via `X-Hub-Signature-256` | ✅ PASS | `ValidateMetaSignature` reads and verifies the header before any DB write; 5 related tests exist. |
| L5-005 | DND check fires before any channel API call | ✅ PASS | Direct code read of `Dispatcher.Dispatch`: DND check at line 38 unconditionally precedes both the WhatsApp branch (line 63) and SMS branch (line 75). |
| L5-006 | All 8 templates (TMPL-001..008) registered | ✅ PASS | Confirmed in `scripts/seed_local.sql` — exactly 8, correctly numbered. |
| L5-007 | E.164 phone format validated before SMS send | ✅ PASS | `pkg/validate/phone.go` (100% covered) enforces `^\+[1-9]\d{1,14}$` at three layers: `CreateSubscriber`, both the SMS and WhatsApp send paths, and a DB `CHECK` constraint in migration `020_phone_e164_constraint.sql`. |
| L5-008 | Dispatch latency ≤5s (dequeue → `sent_at`) | ✅ PASS | `internal/fup/dispatch_latency_integration_test.go` measures enqueue → `sent_at` through a real Asynq server on a real Redis, the real Dispatcher and the real WhatsApp client against a stub Meta endpoint, reading `sent_at` back from the `notification_log` row production code writes. **Single task: 1.011s. Worst case across 50 queued tasks: 92ms.** The backlog case exists because the idle-queue case cannot detect a pipeline that only meets the budget when idle; it asserts the worst task, not the average. Both confirmed to fail when the budget is lowered to 1ms, so the assertion is not vacuous. |

## L6 · Performance (6 checks)

All L6 rows below were run at the levels the DoD itself specifies, against 20,000 seeded subscribers on a throwaway stack. Raw harness: `scripts/run_nfr_tests.sh` (L6-001/002/003) and `scripts/verify_tls.sh` (L6-006).

| ID | Check | Result | Evidence |
|---|---|---|---|
| L6-001 | RADIUS auth p99 ≤15ms @ 5,000 req/s | ✅ PASS | **Measured at the full 5,000 req/s**: p99 **10.4ms**, p50 4.538ms, p95 7.934ms across 149,850 requests, 0 errors, 0 dropped to saturation. Server-side mean auth 3.211ms over 289,810 requests; 289,315 of them completed within 15ms. A capacity sweep confirms the budget holds at 3,000 and 4,000 req/s too, so 5,000 is not a cliff edge. |
| L6-002 | API p99 ≤200ms @ 500 concurrent (k6) | ✅ PASS | **Measured at the full 500 VUs** for 30s via `scripts/k6_api_load.js`: p99 **11.69ms** against the 200ms budget, 286,386 checks, 100% succeeded, `http_req_failed` 0.00%. Both k6 thresholds (`health`, `subscriber_get`) passed independently. |
| L6-003 | Unbilled report ≤60s for 20k subscribers | ✅ PASS | **9.716ms** against a 60,000ms budget, on a real 20,000-subscriber / 18,000-invoice dataset. The planner chooses a sequential scan at this row count rather than an index scan — expected, and ~6,000× inside budget regardless. |
| L6-004 | Redis Sentinel failover ≤3s | ❌ **FAIL (now measured)** | `scripts/run_sentinel_failover_test.sh`, timed from Sentinel's own event log. Failover **works**; it is narrowly too slow. On the committed config (`down-after-milliseconds 3000`): **5055ms** (detection 4110ms + failover 945ms). With detection lowered to 500ms and the override verified in force: **3086–3192ms**, i.e. ~100–200ms over budget. Recommendation is to set detection to 500 and move the target to 8s — see Finding #6, which also corrects an earlier, wrong version of this row. **Superseded 2026-08-18: the budget itself was retargeted to 8s and the config change live-verified — PASS at 2913ms election / 5784ms auth resume. See "Post-v1.0.0 Update" above; this row is left as-is as the 2026-08-11 record.** |
| L6-005 | No goroutine leak after 1h @ 20k sessions | ⬜ NOT VERIFIED | Harness now exists and is validated: `scripts/run_soak_test.sh`, verified end to end on a 90s run (9,000 requests, 0 errors, p99 5.391ms, goroutines and FDs flat). **The 1-hour hold the DoD actually requires has not been run**, so this stays NOT VERIFIED rather than being extrapolated from 90 seconds. Detection needs no pprof — `go_goroutines`, `go_threads`, `go_memstats_heap_alloc_bytes` and `process_open_fds` are already exposed by the default Go collector; pprof (still absent from this codebase) would only be needed to locate a leak once detected. **Superseded 2026-08-18: the full 1-hour hold has now run to completion — goroutines PASS (1.4% growth); FDs FAILed the script's blunt tolerance check (15.7%) but the raw series shows a one-time step to a new plateau, not continuous growth — read as benign connection-pool sizing, not a leak. See "Post-v1.0.0 Update" above for the full analysis; this row is left as-is as the 2026-08-11 record.** |
| L6-006 | TLS 1.3 minimum, TLS 1.2 disabled | ✅ PASS | Now covered by a committed, repeatable script (`scripts/verify_tls.sh`) rather than a one-off `curl`, run against the **real** `config/caddy/Caddyfile`: TLS 1.3 connects (`TLS_AES_128_GCM_SHA256`), TLS 1.2 / 1.1 / 1.0 all refused, and an unpinned client negotiates 1.3. See the note below on why this test is trustworthy. |

**On L6-006's trustworthiness.** The first version of this script passed TLS 1.3 and reported TLS 1.2 as *negotiated* — a false alarm caused by reading OpenSSL's `Protocol :` session line, which echoes the version the client *attempted* even when the server rejected it outright. A refused TLS 1.2 handshake still prints `Protocol : TLSv1.2` there. The script now keys off the `New, <version>, Cipher is <cipher>` line instead, since a version is only real if a cipher was agreed alongside it (`New, (NONE), Cipher is (NONE)` on failure). It was then checked against a deliberately weakened Caddyfile permitting `tls1.2 tls1.3`, and correctly failed — so the pass above reflects the server's actual behaviour, not an assertion that cannot fail.

## L7 · Git Hygiene (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L7-001 | Commit message conventional format | 🟡 PARTIAL | Same as L0-010 — 4 of 5 conform; the docs-only root commit does not and cannot be reworded without rewriting published history. |
| L7-002 | No `.env`/secret files in git history | ✅ PASS | Re-checked after the code was committed, which is when this became a meaningful test rather than a trivially-true one: `git log --all --full-history -- ".env" "*.key" "*.pem"` → still empty. |
| L7-003 | `go.mod`/`go.sum` committed | ✅ PASS | Committed in `ae75ee4`. Verified by cloning the repo to a clean directory and building there — not merely by `git status`. |
| L7-004 | Migrations numbered sequentially, no gaps | ✅ PASS | Now `001` through `020` (019 = least-privilege app role, 020 = E.164 constraint), no gaps. |

## L8 · Sign-Off (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L8-001 | Tracker Status column = Done | ⬜ PENDING | **Materially advanced.** The tracker now carries 96 tasks marked Done (vs 103 all-Todo in the previously committed copy), and that work is finally in git as of commit `297d3bb` — it had been sitting uncommitted in the working tree for the whole engagement. Still PENDING because 5 rows remain Todo and no row-by-row reconciliation against this report has been done. |
| L8-002 | All prerequisite tasks also Done | ⬜ PENDING | Not audited row-by-row; depends on L8-001's remaining 5. |
| L8-003 | Notes column documents deviations | ⬜ PENDING | Partially satisfied in practice — the NFR sheet carries substantive verification notes, and NFR-PERF-001's was rewritten to record how the bcrypt/cost-12 spec conflict was actually resolved. Not audited across all module sheets. |
| L8-004 | Matching INT-* row marked Done in Integration Tests sheet | ⬜ PENDING | The Integration Tests sheet has the largest volume of uncommitted-until-now edits (68 changed lines); not reconciled against the INT-* rows in this report. |

---

## Gaps Requiring Explicit Attention — Ranked by Priority

Items 1, 2, 3, 4, 6 and 7 from the original 2026-08-08 list are closed; see the resolution notes in Critical Findings. What remains, in priority order:

1. ~~**Sentinel failover misses its budget** (L6-004)~~ — **closed.** The recommendation (detection=500, target 8s) was adopted and live-verified on 2026-08-18, and Redis was afterwards removed from the system entirely, taking Sentinel with it. See Finding #6.
2. **Test coverage below the ≥80% general bar** (L2-002) — 60.4% overall, up from 53.1%. The stricter ≥90% crypto/middleware requirement now passes (93.5% / 98.2%). The remaining shortfall is concentrated in `cmd/api` (5.3%, mostly wiring — extracting a testable `newServer()` from `main()` is the standard fix but is refactoring, not test-writing), `internal/portal` (63.0%) and `internal/notifications` (71.7%).
3. **Goroutine-leak soak test** (L6-005) — purely a time cost now. The harness exists, is committed and is validated end to end; it needs one uninterrupted hour on a machine that will not sleep.
4. **TDD commit ordering** (L0-001, L0-002) — permanently unachievable for existing code, since red-phase commits cannot be reconstructed after the fact. Worth deciding whether to enforce going forward or mark N/A in the tracker, rather than leaving two standing failures that can never clear.
5. **Tracker administration** (all of L8) — 96 tasks are now marked Done and committed, but 5 rows remain Todo and no row-by-row reconciliation against this report has been done.

## Methodology & Limitations

- Rows revised on 2026-08-10 carry their evidence inline and are summarised in "What changed since the original audit" at the top. Rows not mentioned there are unchanged from the 2026-08-08 audit and were **not** re-executed on 2026-08-10 — they retain their original basis, which is stated per row.
- All L0/L1/L4/L7 checks with concrete pass/fail language above were **executed live** during the original audit (fresh `go build`/`vet`/`race`×3/`lint` runs, a throwaway Postgres for migration up/down/constraint/FK/partition/RLS testing, live `curl`-based TLS verification, direct `git log`/`git status` inspection) — not inferred from memory of earlier session work.
- L2/L3/L5 checks are primarily **code-verified** (exact file:line citations, existing test names confirmed present) rather than re-executed live one-by-one, given the volume (220 existing tests); L3-002's full suite and the coverage numbers in L2-002 *were* freshly re-run.
- **L6 was re-graded from live measurement on 2026-08-10**, not from code reading. Every number in L6-001/002/003 comes from a run against a throwaway Postgres + Redis + API + radiusd stack with 20,000 seeded subscribers, at the load levels the DoD specifies. L6-004 and L6-005 remain **NOT VERIFIED** and are not being presented as anything else.
- **Caveat on the L6 runs:** the containers were built from current source, but the images were built earlier in the session and reused for the final measurement runs, because Docker Hub certificate validation was failing on this machine (the host clock is roughly 6 hours behind real time, so registry certs read as not-yet-valid). No source changed between the image build and the measurement runs. The system clock was left alone rather than adjusted for a test run.
- The load runs also exposed that a green NFR result is not self-validating: the harness reported a 100% RADIUS error rate that looked like a latency failure and was actually a daemon that never started, and the first TLS script reported a downgrade that never happened. Both are written up in Finding #5 and L6-006 respectively. Where a check could plausibly pass for the wrong reason, it was tested against a deliberately broken configuration to confirm it fails when it should.
- L1-003/L1-004 are marked **PARTIAL** rather than PASS because a full 17-table, column-by-column diff against DBD §6.2/§6.4 was not performed — spot checks on tables touched this session all matched.
- This report is a standalone Markdown file, not a rewrite of `bss_oss_dev_tracker_v3.xlsx`'s "Your Result"/"Pass?" columns — editing that spreadsheet's XML directly by hand risks corrupting its formulas/formatting. If you'd like those columns populated too, that's a reasonable next step now that the underlying findings are established here.
