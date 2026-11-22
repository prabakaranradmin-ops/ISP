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
| Wiring check | **33/33** components have a production caller (2026-11-21) |
| Migrations | **74** applied (`001`–`074`) |
| Browser tests | **145**, all passing (2026-11-21) — `e2e/staff.spec.ts`, `e2e/portal.spec.ts` and `e2e/console-logs-catalogue.spec.ts` |
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

- **Closed 2026-11-21 (FR-DOC-003): archived documents can now be retrieved
  and verified.** This entry used to say the archive `Store` had `Put`/
  `Delete` only and no way to read a document back. `Store.Get` and
  `Archiver.Retrieve` — which hashes what it read and refuses bytes that
  disagree with the recorded checksum — were in fact added with FR-DOC-001.
  What stayed missing until now was a caller: only `cmd/radiusd` built an
  `Archiver`, and only the purge sweep used it, so the archive could delete
  documents and never read one back. An auditor still could not be shown
  that a specific document was intact without hashing the file by hand.
  `cmd/api` now builds one, invoices are archived when their PDF renders,
  and the console's Documents screen verifies on every read.
  `scripts/check_wiring.sh` tracks the retrieval path so it cannot lose its
  caller again — which is the lesson here: a capability with no caller is
  indistinguishable from an absent one, and this gap survived three months
  of the code to close it already existing.
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

- **Closed 2026-11-22 (migration 074): an announcement's reach can no longer
  widen to everybody.** A NULL segment means "no restriction", so anything
  that left an announcement with no segment and no list addressed it to the
  whole base. Three routes did:
  - **Bulk "Notify these" with the portal banner ticked** was returned to
    *every* subscriber by the banner read — it checked only the segment, and
    a list announcement has none: a payment reminder for three customers was
    a banner for all. Latent only because no subscriber could fetch banners
    at all (the endpoint checked the staff signing key — see the next
    entry); it would have gone live the day that was fixed.
  - **A list announcement whose recipients were deleted** —
    `announcement_recipients` cascades, and "no recipient rows" was taken to
    mean "segment announcement", so a draft would send to everybody.
  - **Deleting a targeted plan or franchise** nulled the segment
    (`ON DELETE SET NULL`), including on sent announcements. "Remove demo
    data" deletes demo plans, so this was reachable from the console — and a
    *draft* aimed at a demo plan, sent after removal, went to everybody.

  `announcements.addressed_to_list` now records which kind an announcement
  is, backfilled from existing recipient rows; the portal, the fan-out and
  the console list all decide by it. The two segment keys are `RESTRICT`,
  like `segment_group_id`; demo removal deletes the announcements addressed
  to its demo plans itself. An announcement already widened before 074 is
  indistinguishable from one addressed to everyone and was not repaired.

  Verified by execution, not review: the five reach tests in
  `internal/db/announcement_reach_integration_test.go` pass against a real
  cluster at migration 74, and each of the six guards was broken deliberately
  and confirmed to fail on the assertion it is there for — the portal's list
  clause, the list/segment decision, each of the two `RESTRICT` keys, the
  CHECK, demo removal's own delete, and the console's two.

  **One of those controls did not fire, and that was the real finding.**
  Removing the list/segment decision from `listExplicitRecipients` left every
  reach test green — and silently made *every segment announcement reach
  nobody*, which is the opposite failure and a worse one. Nothing caught it
  because every subtest in
  `TestFR_ANN_001_SegmentAddressesExactlyTheIntendedSubscribers` passes
  announcement id **0** on purpose, so that no recipient row can match and
  the segment query alone is under test; the cost is that no test resolved an
  announcement that exists, which is where the decision is made. pgx scans
  the empty recipient array into a non-nil empty slice rather than nil, so
  the fall-through to the segment filters never happened and the fan-out
  returned zero. `TestAnnouncementReach_ASegmentAnnouncementStillReachesItsSegment`
  now resolves real ids in both directions and is the test that fails. The
  comment at that id-0 constant also cited
  `TestFR_ANN_001_ExplicitRecipientsOverrideSegmentFilters` as covering the
  other path; no such test has ever existed. Both are corrected.

  Also visible only by running it: without the console's guard, the send
  confirmation for a three-recipient list read "Send ... to about 42
  subscribers?" — the operator was told the wrong number at the one moment
  they could still stop.

  Related, not changed: "Remove demo data" deletes demo subscribers'
  tickets, invoices and wallet entries but nothing else that references a
  subscriber without cascading — `notification_log`, `live_sessions`,
  session history, proformas and others. A walkthrough that left a row in any
  of those makes removal fail as a whole (it is one transaction, so nothing
  is half-removed); the console says "check the server log". Not verified
  whether loading demo data on its own produces such rows.

- **Closed 2026-11-22: subscribers can read portal banners and register push
  tokens.** `GET /api/v1/announcements/portal` and `POST /api/v1/push-tokens`
  were wrapped in `api.RegisterRoutes`'s `auth`, which verifies against
  `JWT_SECRET`, while subscriber tokens from `POST /portal/login` are signed
  with `PORTAL_JWT_SECRET` — and every deployment path makes those differ
  (`cmd/bootstrap` generates both; `config.Load` derives the portal one as
  `JWT_SECRET + "_portal"` when unset, so it can never be equal). Both routes
  therefore 401'd every real subscriber, and FR-ANN-002's banner and
  FR-NOTIF-013's push registration had never worked on any deployment. The
  tests passed throughout by minting tokens with whichever secret the route
  checked, and the banner feed had no route-level test at all — its only
  consumer in the whole repo was the 401-walled route, so the feature was
  reachable by no client by any path.

  The two routes now verify the portal key, which `cmd/api` passes as
  `HandlerDeps.PortalJWTSecret`. That is a new dep rather than a second
  argument to `RegisterRoutes` on purpose: the signature has 86 test call
  sites, and making each pass a secret twice would have rebuilt the same
  blind spot by letting the harness use one key for both audiences.
  `itPortalJWTSecret` is deliberately **not** `itJWTSecret`, and a control
  that collapses them fails
  `TestFR_ANN_002_APortalSignedTokenReachesTheBannerFeed`.

  **This was also a live replay hole, not only a dead feature.** Under the
  old wiring a subscriber-role token signed with the *staff* key was accepted
  by the banner feed — exactly what the comment at `config.Load` says the
  split exists to prevent ("a leaked staff token cannot be replayed against
  subscriber endpoints, and vice versa"). Confirmed by negative control
  (200 before, 401 now) and against a running server.

  A missing portal secret now refuses these routes with
  `ERR_PORTAL_AUTH_UNCONFIGURED` instead of being handed to the verifier: an
  empty HMAC key verifies a token anybody can mint, and a control proved it
  — with the guard removed, a token signed with the empty string got 200 on
  a subscriber's feed. `cmd/api` cannot reach that state today, because
  `JWT_SECRET` is `RequiredSecret` for the api service and the portal key
  derives from it, so the guard is defence in depth rather than a live fix.
  Note that the handler also answers 503 when its store is nil, so the test
  asserts the error *code*, not the status.

  Verified against a running server with the two secrets deliberately
  different, which is the only check that proves `cmd/api` passes the right
  config field — the unit tests drive the handler directly and would pass
  either way. A real `test_user` token: 200 from `/portal/me` (as before),
  and now 200 from both previously-401 routes. A staff-signed token and a
  subscriber-role token forged with the staff key both get 401.

  The same run closed migration 074 end to end, in production rather than in
  a test: a broadcast banner and a list banner addressed only to subscriber 4
  were both sent, and subscriber 1 saw **only** the broadcast while
  subscriber 4 saw both. Checking the recipient too is what makes that
  meaningful — a check that only confirmed subscriber 1's absence would have
  passed just as well had the banner reached nobody at all, which is the
  shape of the captive-portal anti-oracle defect in the list below.

- **FR-NET-006 has never polled a real router.** This machine has no router
  and no SNMP agent, so the poller is proven only against
  `internal/nastraffic/snmptest`, an agent built on the same library's encoder.
  That proves the OIDs, the paged walks, the 64-bit handling, the timeouts and
  the whole path into the database; it cannot prove how a real agent behaves.
  On the first real deployment, check against one router before trusting the
  graphs:
  - its interfaces appear with the names shown on the router (`ifName`), and
    PPPoE sessions do **not** appear (they are skipped as IANA ifType 23 —
    if this RouterOS version reports them otherwise, the cap of 256 is all that
    stops them filling the table);
  - an uplink's rate matches what the router's own traffic monitor shows over
    the same five minutes;
  - a wrong community produces "No answer … check the community" within a
    minute, and fixing it clears it on the next poll.
  `snmpwalk -v2c -c <community> <router> 1.3.6.1.2.1.31.1.1.1` from this
  server is the quickest way to see what the router actually reports.

  Known limits of the same feature: SNMPv2c only, so the community crosses the
  network in clear text and the router should accept it only from this
  server's address on a management network; no per-interface "don't graph
  this" toggle; with many routers that do not answer, a cycle can run long
  enough (eight workers, up to a minute each) that intervals exceed the
  fifteen-minute gap rule and show as holes. Every radiusd instance polls, so
  two instances double the SNMP load on each router; the compare-and-swap
  only stops them double-recording.

- **Captive-portal branding is one per deployment, not one per venue.**
  FR-HSP-004 asked for per-deployment branding and migration 072 holds a single
  row. An operator running hotspots for several venues off one deployment gets
  the same name and logo on all of them. If that is needed, the shape is a table
  keyed by `nas_id` that falls back to the singleton, chosen from the `nasid`
  the NAS already puts in its redirect — not a second singleton.

  Two smaller limits of the same feature. The console's preview shows what is
  *saved*, not the form's unsaved edits, so an operator saves to see a change.
  And on a deployment with more than one API instance, an instance that did not
  take the save shows the old branding for up to `hotspot.BrandingCacheTTL`
  (30 s); only the instance that saved is invalidated at once.

- **The FR-INV-004 vendor backfill is a guess, and the evidence for undoing
  it is the column that now looks redundant.** Migration 071 merged the three
  free-text vendor columns into one master by matching names
  case-insensitively with runs of whitespace collapsed. It deliberately does
  not merge on punctuation, so “Sri Balaji Cables” and “Sri Balaji, Cables
  Division” are two suppliers — which is right if they are, and leaves a
  duplicate to clean up if they are not. The reverse mistake is the dangerous
  one: if two spellings that were merged turn out to be different companies,
  one company's spend and input tax credit are now attributed to the other.
  The `vendor` text column on `cpe_device_types`, `cpe_purchases` and
  `purchase_orders` is retained for exactly this: it still holds what each row
  originally said, and it is the only way to split a wrong merge back apart.
  Do not drop those columns to tidy up. They are commented in the database to
  say so.

- **The vendor master may contain manufacturers as well as suppliers, and
  nothing can tell them apart.** `cpe_device_types.vendor` was free text that
  operators used for both — sometimes the distributor a model is bought from,
  sometimes its make (“MikroTik”, “TP-Link”). Migration 027's own comment
  calls it the supplier default, so FR-INV-004 links it to the master like the
  other two, but the backfill creates a master row from whatever was typed. A
  deployment may therefore open the Vendors screen and find manufacturer names
  sitting beside real suppliers, with no GSTIN and no purchases behind them.
  That is a faithful reflection of what the data said, not a migration defect,
  and it is a cleanup an operator does by retiring the rows that are not
  companies they pay. If the distinction ever needs to be held properly, it is
  a `manufacturer` column on `cpe_device_types` separate from `vendor_id`, not
  a second kind of row in `vendors`.

- **A ticket rating has no history: re-rating overwrites.** FR-SUP-004 keeps
  the score, its timestamp and its comment as columns on `tickets`, so
  changing a rating replaces it and there is no record of what it was before.
  That was a deliberate trade for not adding a join to the queue, the
  subscriber's list and the helpdesk summary to store one row per ticket. It
  is fine while the rating is read as current feedback; it would not be fine
  if the business ever wanted to show that support pressured a customer into
  changing one. If that day comes, the change is a `ticket_ratings` table
  with the current values kept on `tickets` as a cache, not a migration of
  the columns away.

  Related: nothing stops staff *asking* a customer to re-rate. What is
  prevented is staff entering a rating themselves — there is no console path
  that writes one, and `RateTicket` takes the subscriber id from the portal
  session.

- **An immediate plan change leaves no history row.** Migration 031's
  trigger is `AFTER UPDATE OF status ... WHEN (OLD.status IS DISTINCT FROM
  NEW.status)`, so changing a subscriber's plan without changing their status
  writes nothing to `subscriber_status_history` — and there is no
  plan-change-history table. `scheduled_plan_changes` records only changes
  that went through the *scheduled* path (FR-BIL-023), with its own
  from/to/effective/status columns.

  FR-RPT-004's plan-change-history report is therefore built on
  `scheduled_plan_changes` and says so on the screen. Immediate changes are
  recoverable only from the owner-only `staff_audit_log`, and that was
  deliberately not pulled into the report: it would widen a record *about the
  staff* to every role that can read a report, and the audit detail is JSON
  that would have to be parsed per row. Making the history complete means
  either a `plan_change_history` table written by both paths, or widening
  migration 031's trigger to fire on `plan_id` as well — the second is
  cheaper and is probably the right answer, but it changes what an existing
  table means, so it is its own change.

- **The invoice register has no paid/unpaid column, and cannot.** `invoices`
  carries no status: this schema settles an invoice by debiting the wallet,
  so "paid" is a property of the wallet's balance and ledger over time rather
  than of the invoice row. Any per-invoice status would have to be inferred
  (wallet balance at the time? a matching debit within N days?) and would be
  wrong often enough to be worse than absent. The register reports what was
  raised; what was collected is the two collection reports. Said on the
  screen so nobody looks for the column.

- **The four aggregate reports cannot be filtered by subscriber group.** Plan
  mix, growth, ticket resolution and collection read pre-aggregated views —
  `mv_ticket_resolution` is materialised — so no individual subscriber
  survives in them to match against a group membership. FR-GRP-001 delivers
  the group filter as a fifth report (`subscribers`, a roster) and *refuses*
  the filter on the other four with a 422 rather than dropping it silently.
  Making them group-aware means rewriting each view against the underlying
  tables, with a refresh decision for the materialised one.

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
./scripts/check_wiring.sh                   # 33/33 wired
npx playwright test e2e/staff.spec.ts e2e/portal.spec.ts     e2e/console-logs-catalogue.spec.ts      # 145 browser tests
```

**Run `scripts/refresh_e2e_session.sh` before a browser suite.** Eight tests
assert the "online" state — the portal's live-usage panel, the console's
Online tile, and the Sessions screen including its disconnect — and all read
`live_sessions`, whose reads filter `updated_at` against a 30-minute TTL. The
row goes stale two ways: time passing (this machine suspends, and a seed from
last night is nine hours old by morning), and the disconnect test itself,
which closes the accounting row it disconnected and leaves nothing for the
next run. Both fail with "element(s) not found", which reads exactly like a
broken page. The script reseeds the live row and reopens the accounting row;
it is idempotent, so run it whenever that cluster of eight fails together.

**The DB suite is now close to its own timeout.** `internal/db` measured
2539s against `-timeout 45m` (2700s) on 2026-11-20, with builds and unit
tests running against the same machine throughout. Left alone it is nearer
1300s. If a run reports a bare `FAIL internal/db` with a duration in the
2600s range, check whether anything else was using the machine before
looking for a defect — and consider `-timeout 60m` for a run you cannot
babysit.

**Stop the Playwright API server before running the DB integration suite.**
The harness TRUNCATEs about a hundred tables per test, which needs ACCESS
EXCLUSIVE, and a leftover `api_e2e.exe` keeps idle connections whose
background scanners (FUP every 10s, renewal, the purges) take ACCESS SHARE on
those same tables every few seconds — enough to starve the TRUNCATE
indefinitely. The suite then hangs in `truncateAll` and is killed at the
timeout, reported as a bare `FAIL internal/db 2700.182s` with a goroutine
dump naming whichever test was unlucky, which reads exactly like that test
deadlocking. `powershell -NoProfile -Command "Stop-Process -Name api_e2e
-Force"`, then check `SELECT count(*) FROM pg_stat_activity WHERE
datname='isp_bss_oss'` is zero. It inflates runs that do finish, too: 1848s
and 1697s with the server up against 1289s without.

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
- two subscriber-facing routes that verified the *staff* JWT secret while
  every real portal token is signed with the portal one, so FR-ANN-002's
  banner feed and FR-NOTIF-013's push registration 401'd every customer on
  every deployment and had never once worked. Their tests passed by minting
  tokens with whichever key the route checked: a harness that uses one secret
  for two audiences cannot tell a route that checks the right key from one
  that checks the wrong one
- a whole family of fan-out tests that pinned the announcement id to `0` so
  the segment query alone was under test, and in doing so could not fail when
  the list-or-segment decision broke and every segment announcement reached
  nobody. The id that makes a test precise can be the id that makes it blind;
  cover the real one too
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
