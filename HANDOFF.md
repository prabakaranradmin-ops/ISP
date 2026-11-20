# Handoff — state of play

Rewritten 2026-08-16, after the Phase 4A/4B close-out. The previous version
was written on 2026-08-12 and every number in it had drifted — it claimed 22
migrations against an actual 34, 13/13 wiring against 20/20, and "2 known
lint findings" against 116. Re-derive rather than trust if this file is more
than a few sessions old: `go test ./...`, `check_wiring.sh`, `golangci-lint`
and `git log` are the truth, this file is a convenience.

---

## Where the project is

The Jaze-parity roadmap (CRD §1.11, adopted as BO-007) is functionally
complete at **98 of 99 FRs**, with FR-AAA-005 deferred by decision — every
other requirement is implemented. The ledger below is reconciled against the
SRS rather than carried forward.

Phases 1–4 are shipped and pushed. The last two commits closed it out:

| Commit | Delivered |
|---|---|
| `33bfd89` | Phase 4A — captive portal (FR-HSP-001), hotspot CoA enforcement (FR-HSP-003), document archival (FR-DOC-001), NAS management API, **RADIUS accounting repair** |
| `1016d7f` | Phase 4B — report export and scheduling (FR-RPT-002), subscriber portal contract parity (FR-MOB-002) |

**Read the accounting repair before trusting any usage number that predates
it.** `subscriber_session_history` was never written: nothing called
`StartSession`/`StopSession`, and the daemon bound only `:1812` while every
NAS sends accounting to `:1813`. FUP enforcement, CoA targeting, LEA lookups
and the portal's usage history were therefore all reading an empty table
while their own tests passed. Any historical usage data from before
`33bfd89` does not exist, and any conclusion drawn from its absence was
wrong for this reason rather than a business one.

### The FR ledger, reconciled 2026-08-16

**98 implemented, 1 deferred (FR-AAA-005), 0 outstanding.**

The SRS contains exactly 99 `FR-*` requirements. Count them with a word
boundary — `grep -oE 'FR-[A-Z]+-[0-9]{3}'` returns 108, because it matches
`FR-AVAIL-001` *inside* `NFR-AVAIL-001`. `grep -oE '\bFR-...'` gives 99, plus
11 genuine NFRs.

Cross-referencing those against every `FR-` citation in Go, SQL and shell —
expanding range notation like `FR-NAS-001..004`, which otherwise matches only
its first id — leaves three unreferenced:

| FR | Status |
|---|---|
| FR-OBS-001 (Prometheus `/metrics`) | Implemented, never cited by id |
| FR-OBS-002 (structured JSON logs, `correlation_id`) | Implemented, never cited by id |
| FR-OBS-005 (alert when RADIUS auth failure rate on any NAS exceeds 20% over 5 min) | Implemented in `internal/radius/authalert.go` |

**FR-AAA-005** (plain CHAP) is the only remaining item, and it is deferred by
decision rather than pending: it requires storing recoverable plaintext
passwords.

FR-OBS-005 needed a new metric before the rule could be expressed at all —
`radius_auth_accept_total` and `radius_auth_reject_total` are unlabelled, so
"on **any NAS**" was not computable from them. `radius_auth_outcome_total`
now carries a `nas` label, and every accept and reject in PAP, EAP and MAB
routes through `authAccepted`/`authRejected` so the labelled and unlabelled
counters cannot drift. Unidentified sources collapse into one `unregistered`
bucket, so a spoofed address cannot mint a series per packet.

Evaluation is in-process because this repository ships no Prometheus and no
Alertmanager; `deploy/prometheus/radius_alerts.yml` carries the same rule in
PromQL, and a test asserts the two thresholds agree. Alerts go to
`logAlerter` — see the open decision below about where they should really go.

Note that FR-AAA-003 was counted complete throughout while being
non-functional until `33bfd89`, so any historical figure overstated reality
by one.

---

## Current numbers (measured 2026-08-16)

| | |
|---|---|
| Go packages | 35 total; **24 have tests** |
| Wiring check | **30/30** components have a production caller (2026-11-20) |
| Migrations | **66** applied (`001`–`066`) |
| Browser tests | **124**, all passing (2026-11-20) — `e2e/staff.spec.ts`, `e2e/portal.spec.ts` and `e2e/console-logs-catalogue.spec.ts` |
| Lint | **6** findings on default tags, **116** with `--build-tags=integration`; none introduced by recent work, all untriaged |
| DB integration suite | ~700s in Docker, **1000-1500s on the bundled native cluster**; carry `-timeout 45m` |
| NFR-PERF-001 (RADIUS auth p99 ≤15ms) | **PASS at 13.224ms**, 5,000 req/s, 30s, 0 errors — `run_nfr_tests.sh`, 2026-08-16 |
| NFR-PERF-002 (API p99 ≤200ms) | **PASS**, 49.01ms at 500 VUs |
| NFR-BIZ-001 (unbilled query ≤60s @20k subs) | **PASS**, 12.2ms |
| NFR-SEC-002 (no plaintext PII) | **PASS** |
| NFR-DUR-002, NFR-SEC-003, NFR-PERF-004 (archival, portal rate-limit, report export) | Merged into the SRS 2026-08-16, all with real code/tests behind them. Report-export threshold (4.5s p99) is set at 2.5× a measured empirical baseline — collection report, worst of the three, ran p99 1.69s over 30 iterations against a seeded 20,000-subscriber/430k-invoice 120-month dataset |

**`run_nfr_tests.sh` and the demo stack cannot run at the same time on this
machine.** The demo stack is 11 containers (Postgres, a 3-node Redis Sentinel
cluster with two replicas, the API, radiusd, the reverse proxy, Gotenberg);
the NFR harness spins up its own isolated set. Sharing this host's 12 CPUs
between both turned a genuine 13ms p99 into a false 20ms failure on
2026-08-16 — the daemon's own histogram showed 99.4% of requests under 15ms
while the client measured a 20ms tail, and the rate sweep was non-monotonic
(1,000 req/s scored worse than 3,000 req/s), both signatures of scheduling
contention rather than slow code. `docker compose stop` before running the
NFR suite, `docker compose start` after.

They no longer fight over the AES key store, though. Until 2026-08-17 both
`run_nfr_tests.sh` and `smoke_test.sh` wrote and then deleted
`config/keys/aes_keys.json` — the file `demo_up.sh` owns. That crash-looped the
demo API (fatal there by design: the key store encrypts KYC data at rest, so
`cmd/api` refuses to start without it, unlike `radiusd` where it is optional
and only enables multi-vendor NAS support) and silently re-keyed anything
encrypted under the previous key. Each script now writes
`config/keys/aes_keys.<run>-<pid>.json` and removes only its own.

The lint figure is not a regression — the integration-tagged count has been
that high for some time and simply was never measured before. It is mostly
`noctx` in tests (`httptest.NewRequest` cannot take a context until the
module moves past go1.22). It matters because 116 findings is enough noise
to hide a real one.

A correction to the lint row, and a caution about counting tests by grep. The
previous version's "2 known gosec findings" is now 6 and 116; that is a
measurement, not a regression.

The browser count is 45 and always was. Do not count these with
`grep -c 'test('` — that gives 37, because `staff.spec.ts` generates one test
per role from a matrix loop rather than declaring each. `npx playwright test
--list` is the only count worth quoting. This file briefly claimed 37 for
exactly that reason, which is a small example of the rule at the bottom of
this document: the grep looked authoritative and was wrong, and running it
settled the question in seconds.

---

## Open decisions that need a person, not a sprint

1. **L6-004 Sentinel failover target.** Measured 5055ms against a 3000ms
   budget; ~3.1s with `down-after-milliseconds` at 500. Recommendation:
   adopt 500 and move the target to 8s. **Separately**, decide whether the
   DNS/tilt failure mode is acceptable — when a master container leaves
   Docker DNS, Sentinel never promotes at all. Reproduce with
   `CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh`. See
   `DOD_STATUS_REPORT.md` Finding #6.

2. **The lint backlog needs triage.** 6 findings on default tags, 116 with
   the integration tag. Confirmed not introduced by recent work — the method
   is a `git worktree` at the previous HEAD, lint both, and diff the finding
   sets with line numbers stripped; that is more reliable than `git stash`,
   which re-CRLFs files and creates phantom `gofmt` findings. The ones worth
   a real decision rather than a suppression: G705 in
   `internal/api/invoices.go`, G710 in `internal/staffui/screens.go`, G117 in
   `internal/partner/dispatch.go`, and the missing cookie attributes in
   `internal/tr069/acs.go`.

3. **Alerts have nowhere to go but the log.** `logAlerter` is shared by the
   dead-letter monitor, the SLA scanner and now the per-NAS auth failure
   monitor (FR-OBS-005), and it writes a log line. That satisfies "emit an
   alert" literally, and satisfies nobody at 3am. Deciding the destination —
   Alertmanager, a webhook, the notification service that already sends
   WhatsApp — is a real choice nobody has made, and `staff_users` carries no
   contact details to route to.

4. **`staff_users` has no MFA, no lockout, no password rotation, and no
   account-creation screen** (accounts come from the seed). Migration 021
   added a new authentication surface and none of this was ever specified,
   so these are open decisions rather than omissions.

---

## Known gaps worth knowing about

- **The archive `Store` interface has no retrieval method.** `Put` and
  `Delete` only — there is currently no code path to read an archived
  document back out and re-verify it. Found 2026-08-16 while writing
  NFR-DUR-002 into the SRS: the checksum is real and verified at write time
  (`archive.LocalStore.Put` hashes while streaming), but "verify on
  retrieval" can't be tested because there is no retrieval. For a compliance
  feature holding 8-year GST invoices and 5-year KYC documents, this matters
  more than it would elsewhere — nobody can currently prove to an auditor
  that a specific archived document is intact without going around the
  application and hashing the file on disk by hand. Adding `Store.Get` and a
  restore/verify path is the natural next slice.
- **`scripts/seed_local.sql` publishes demo credentials.** Bcrypt hashes for
  five staff accounts, with the password named in `TESTERS_MANUAL.md`
  (`staffpassword`). Same category as the existing `testpassword` subscriber
  hashes. Fine for a localhost demo, but this is a public repo — those
  accounts must never exist in a real deployment.
- **The 1-hour soak (L6-005) has never been run.**
  `./scripts/run_soak_test.sh` is committed and validated on a 90s run.
  Needs one uninterrupted hour on a machine that will not sleep.
- **Voucher data caps end the session rather than throttling it.** Not a gap,
  but a decision worth knowing: `hotspot.QuotaScanner` (migration 035)
  disconnects an exhausted voucher instead of dropping it to a slower profile,
  because a voucher is prepaid for a fixed volume and a crawl afterwards reads
  as a broken network rather than a spent voucher. Subscriber-backed hotspot
  grants are deliberately excluded from that scan — they are metered in
  `subscriber_session_history` and throttled by the FUP scanner, and enforcing
  them in both places would disconnect somebody the other path only slowed.
- **The captive portal does not complete MikroTik's own login.** It issues a
  grant and relies on the NAS retrying MAC authentication, which needs
  `login-by=mac` on the hotspot profile and produces the "turn Wi-Fi off and
  on" instruction on the success page. The native flow — POSTing to
  `$(link-login-only)` so the router authenticates immediately — is the
  smoother path and is not built. Also note the walled garden must allow the
  portal host, or a client cannot reach the sign-in page at all.
- **Document archival is local-filesystem only.** The `archive.Store`
  interface is the seam for S3 or SFTP; neither is implemented, and a copy on
  the same machine is not disaster recovery.
- **`ARCHIVE_DIR` unset disables archival and its retention purge** — both
  say so at startup rather than failing silently, but a deployment that skips
  it has no document retention at all.

---

## Things a fresh session will not know

### Running anything

```bash
./scripts/demo_up.sh                     # full stack; generates .env secrets
export COMPOSE_PROJECT_NAME=isp_bss_demo # demo_up.sh hardcodes this
```

Without that variable every `docker compose` command silently targets a
different project and reports containers as not running.

### PostgreSQL HA is opt-in, not the default

The base `docker-compose.yml` runs one Postgres, same as it always has.
The Patroni/etcd HA topology is an overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml up -d
```

`demo_up.sh` and the whole test suite run against the single-node stack.
Note that `docker-compose.pg-ha.yml` uses fixed `container_name` values, so
it cannot run side-by-side with the demo stack on one machine without a
name/port override.

### Docker on this machine

- Docker Desktop drops out mid-session. Recover:
  `powershell -Command "Start-Process 'C:\Program Files\Docker\Docker\Docker Desktop.exe'"`
  then poll `until docker info; do sleep 3; done`.
- Registry pulls have failed with certificate errors when the host clock
  drifts (the cert reads as not-yet-valid). Cached images still work; do not
  "fix" it by changing the system clock.

### Running the DB suite with Docker down

`scripts/run_db_tests.sh` needs Docker. When the daemon is not running, the
suite can go against a throwaway cluster built from the PostgreSQL that
ships with the product, on a port of its own:

```bash
PGBIN="/c/Program Files/ISP BSS/pgsql/bin"
D=<a scratch dir>/pgtest
echo postgres > "$D.pw"
"$PGBIN/initdb.exe" -D "$D/data" -U postgres --pwfile="$D.pw" -A trust -E UTF8
"$PGBIN/pg_ctl.exe" -D "$D/data" -l "$D/server.log" \
    -o "-p 55432 -c listen_addresses=127.0.0.1" start   # blocks under MSYS; it
                                                        # has started anyway —
                                                        # confirm with pg_isready
"$PGBIN/createdb.exe" -h 127.0.0.1 -p 55432 -U postgres isp_bss_oss
export MSYS_NO_PATHCONV=1
export TEST_DB_DSN="postgres://postgres:postgres@127.0.0.1:55432/isp_bss_oss?sslmode=disable"
go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 -dir ./migrations postgres "$TEST_DB_DSN" up
go test -tags=integration -count=1 -p 1 -timeout 25m \
    ./internal/db/... ./internal/cache/... ./internal/fup/... \
    ./internal/reporting/... ./internal/jobqueue/...
```

Two things that will bite:

- **The database must be named `isp_bss_oss`.** Migration 019 hard-codes that
  name in its `GRANT CONNECT ON DATABASE` — any other name fails at 019 with
  `database "isp_bss_oss" does not exist`, which reads as a connection
  problem and is not one.
- **Never point `TEST_DB_DSN` at the live `ISPBSSPostgres` service on 5432.**
  The harness `TRUNCATE`s the tables it owns; against the installed system
  that is the operator's real data. Hence a separate cluster on 55432 rather
  than another database inside the running one.

`pg_ctl stop` the cluster and delete its directory when done.

### Paths and Docker

`MSYS_NO_PATHCONV=1` is required for most `docker run` arguments, but it
means POSIX paths reach the Windows Docker binary unconverted. A `/tmp/...`
path becomes `D:\tmp\...` and silently fails. Use `pwd -W` for any `-v` mount
or `-f` file argument. This exact bug produced two sessions' worth of wrong
measurements.

### Line endings

This repo has `core.autocrlf=true`. Any `git stash`/`pop` round-trip
re-CRLFs the files it touches, which makes `gofmt -l` flag entire files as
unformatted even though nothing semantic changed. Fix with
`sed -i 's/\r$//' <file>`; do not change the git config.

### The demo stack keeps serving old code until you rebuild it

`docker compose up -d` reuses the existing image. A stack left running across
a few sessions serves whatever was built when it started, so new routes
answer 404 and a fix appears not to work — which is indistinguishable from
having written it wrong. Check what you are actually talking to:

```bash
docker ps --format '{{.Names}}\t{{.Status}}'   # "Up 20 hours" is the warning
docker compose build api_service aaa_core_daemon && \
  docker compose up -d api_service aaa_core_daemon
```

Note the API container publishes only `9101` (metrics). Application traffic
goes through the reverse proxy, so probe `https://localhost/...` with
`curl -k`, not `localhost:8080`.

### The demo stack's seeded Redis session expires

`demo_up.sh` seeds `session:active:{id}` with a 24h TTL. A stack left running
longer than that fails the two Playwright tests that check the "online"
dashboard state, for reasons unrelated to whatever you just changed. Reseed
with the same `redis-cli SET ... EX 86400` command from `demo_up.sh`.

This is not hypothetical: it happened again on 2026-08-16. The two failures
are `portal.spec.ts` "live usage" and `staff.spec.ts` "the 360 view". Confirm
it is the seed and not your change before debugging anything —
`redis-cli TTL session:active:1` returning `-2` means the key is gone, and
reseeding makes both tests pass again in seconds.

### `-race` needs a C compiler

`go test -race` requires cgo. In the Git-Bash shell used for recent sessions
`gcc` was not on `PATH`, so those runs were without `-race`. Use a shell
where `gcc` resolves if race detection matters for what you are changing.

---

## Verification that keeps finding real bugs

```bash
gofmt -l internal/ cmd/ && go build ./... && go vet -tags=integration ./...
go test -count=1 ./...                      # 31 packages with tests
bash scripts/run_db_tests.sh -timeout 45m   # ~700s in Docker, up to 1500s native
                                            # (Docker down? see "Running the
                                            #  DB suite with Docker down")
golangci-lint run --build-tags=integration ./...   # 116 known, none recent
./scripts/check_wiring.sh                   # 30/30 wired
npx playwright test e2e/staff.spec.ts e2e/portal.spec.ts     e2e/console-logs-catalogue.spec.ts      # 124 browser tests
```

**Never run the DB integration suite and Playwright against the same
cluster.** The `internal/db` harness TRUNCATEs its tables, so an integration
run wipes `staff_users` and every browser test then fails at sign-in with no
server-side error — about 5.5s per test, which reads exactly like a newly
broken screen rather than a missing seed. This cost two full browser runs on
2026-11-20. Reset between them with `DROP DATABASE` + `goose up` +
`scripts/seed_local.sql`, never with a `TRUNCATE` loop: truncating everything
also removes the reference data migrations seed (`sla_policies` and
`category_priority_defaults` from 023), and ticket creation then fails for
every subscriber, because `resolveTicketSLA` refuses to file a ticket it
cannot attach an SLA to. The specs also hardcode subscriber 1, so the seed
has to run against reset sequences or `test_user` lands on a different id and
a dozen tests assert against the wrong subscriber.

The DB suite **must** carry a generous `-timeout`. Go's 10-minute default
kills it partway, and so does 25m on the native cluster.

`internal/db` alone measured 920s, 1035s, 1146s, 1169s and then **1500s**
across one session as tests were added — the bundled-PostgreSQL cluster
(see "Running the DB suite with Docker down") is roughly 2x slower than the
Docker one the old 25m figure was chosen for, and the suite only grows.
`-timeout 45m` is the current recommendation.

**A timeout looks exactly like a test failure and is not one.** Go reports
it as a bare `FAIL <package> <duration>s` with no failing test named, and
the duration is the giveaway: `FAIL internal/db 1500.271s` under
`-timeout 25m` is the timeout firing at 1500s, not an assertion. Check the
duration against the timeout before hunting for a defect.

Two things that make a timeout more likely, both avoidable:

- **Do not run other work against the machine while the suite runs.**
  Builds, `go test ./...`, goose and psql all contend for the same CPU and
  the same database, and the suite is what loses.
- **Do not filter the suite's output through `grep` when you need to
  diagnose a failure.** `grep -E "^(ok|FAIL|---)"` discards the panic line
  a timeout prints and the `--- FAIL` lines a real failure prints, which
  turns a 25-minute run into two. Redirect to a file and grep the file.

`check_wiring.sh` exists because three components once shipped complete,
tested, and never called. It is a grep, not a test, and it caught what the
suite could not. It has since caught a fourth: RADIUS accounting persistence
was the canonical case — written, tested, correct, and called by nothing.
When adding a long-running component, add it there too, and prefer tracking
the call that proves it is *mounted* (`handler.RegisterRoutes`) over the one
that proves it was merely constructed.

### The pattern that keeps working

**Run it, don't read it.** Defects that passed code review and failed
execution, across several sessions:

- an endpoint served subscriber IPs unauthenticated while every auth test passed
- the NFR harness reported a 100% error rate that was a daemon which never started
- a TLS test reported a downgrade that never happened
- a Sentinel config override silently never applied
- three components were built, tested, and never wired
- Patroni's bootstrap silently did not create the application database
  (found by running migrations against a real cluster, not by reading docs)
- RADIUS accounting acknowledged every record and stored none, with the
  accounting port unbound, while four features quietly read an empty table
- an accounting dedup test that passed while asserting the *wrong attribute*
  (NAS-Identifier as the session key) — the test and the bug agreed
- a captive-portal anti-oracle test that could not fail, because it compared
  two refusals that shared a MAC
- document retention of `8 * 365 * 24h`, two days short of eight calendar
  years, deleting GST records early in a way nobody would ever notice
- the JWT middleware answering `text/plain` while every handler answered
  JSON, making token expiry the one error a mobile client could not parse
- a console screen (Products & Charges, FR-BIL-022) whose handler, routes,
  role gate and template file were all correct and whose name was missing
  from `staffui.pageNames`, so the page answered 500 while the whole Go
  suite compiled and passed. `TestEveryPageTemplateIsRegistered` now
  compares the registry against the embedded `templates/` directory in both
  directions, which is the check that could not have been fooled

When a check could plausibly pass for the wrong reason, break it
deliberately and confirm it fails. And check *which* assertion the control
fired on: a control that fails on the first of several assertions has said
nothing about the rest.

The audit trail's append-only test (FR-OBS-006) ran four privilege checks
in one transaction. A failed statement aborts a Postgres transaction, so
once the UPDATE was denied, the DELETE and TRUNCATE assertions were passing
on "current transaction is aborted" rather than on "permission denied" —
and the negative control passed too, because it tripped the first check and
never reached them. Each statement now gets its own transaction and each
assertion requires the error text to say `permission denied`. Every negative control run so far has
found something — including two of the entries above, which were *test*
defects found only because the control was run.

---

## Where things are

| | |
|---|---|
| Roadmap and phasing | `specification_docs_v2/01_CRD_...` §1.11 |
| All 99 FRs | `specification_docs_v2/02_SRS_...` |
| Module designs | `specification_docs_v2/04_MDS_...` (§4.11 NAS, §4.12 PG HA, §4.13 SLA, §4.23 hotspot, §4.24 archival) |
| Captive portal | `internal/hotspot` → `/hotspot` (unauthenticated by design) |
| RADIUS accounting | `internal/radius/accounting.go` → `:1813` |
| Document archival | `internal/archive` → purge scanner in `cmd/radiusd` |
| Report export | `internal/reporting` → `/api/v1/reports` |
| Schema | `specification_docs_v2/06_DBD_...` |
| Infrastructure / HA topology | `specification_docs_v2/08_IDD_...` §8.2a |
| Incident runbook | `specification_docs_v2/12_OPS_...` |
| DoD scorecard, findings | `DOD_STATUS_REPORT.md` |
| Running/testing per persona | `TESTERS_MANUAL.md` |
| Subscriber portal | `internal/portalui` → `/ui` |
| Operations console | `internal/staffui` → `/staff` |
| Browser tests | `e2e/portal.spec.ts`, `e2e/staff.spec.ts` |
