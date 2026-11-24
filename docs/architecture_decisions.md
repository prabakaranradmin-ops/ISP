# Architecture decisions

A short record of the decisions that shaped this system, why each was taken,
and what it costs. One entry per decision, newest first.

This index exists because the reasoning was already written down — in code
comments, commit messages, `HANDOFF.md` and the specification set — and
nowhere that someone could read in one sitting. Three times in November 2026
a decision was re-raised months after it had been settled, once with a
reproduction command for a component that had already been deleted. A
decision nobody can find is a decision that gets made again.

Each entry names where the detail lives. **The code comments remain the
authority**; this is a map, not a replacement.

---

## ADR-016 — Backups are nightly dumps the operator schedules, and the superuser takes them
**2026-11-24 · Accepted**

`scripts/windows/backup.ps1` captures the database, the key store and `app.env` together,
nightly, via a scheduled task the operator registers with
`scripts/windows/schedule_backup.ps1`. It connects as `postgres`. There is no WAL archiving,
so the recovery point is the last nightly backup.

*Why the superuser:* `DB_DSN` names `bss_app`, which cannot read
`lea_audit_log` or `goose_db_version` (migration 019). `pg_dump` reads every
table it dumps and stops with "permission denied for table" on the first one
it cannot — so dumping as the application role produces no backup at all.
This script dumped as `$dbUser` and had therefore never worked, while
`scripts/windows/verify_restore.ps1` had always connected as `postgres`. The two halves
disagreed and only the restoring half was right.

*Why the installer does not schedule it:* the backup folder holds live
encryption keys and credentials, and where it lives is an operator's decision.
The installer names the command instead — which is a compromise, and the
reason it is loud about it: on the machine this was developed on, the backup
root was still empty months after installation. No database backup had ever
been taken.

*Cost:* worst-case data loss is 24 hours. Continuous archiving would reduce
that to seconds and is a separate piece of work; the previous runbook section
was headed "Point-in-Time Restore" over a command that restores a nightly
dump, which is the claim this replaces.

*Detail:* `scripts/windows/backup.ps1`, `scripts/windows/schedule_backup.ps1`,
runbook §12.4. Checked by `internal/db/backup_integration_test.go`, which
derives the rule from the catalogue rather than asserting a string: if
`bss_app` is ever granted SELECT on everything, dumping as it becomes
legitimate and the test stops demanding the superuser.

---

## ADR-015 — The services keep their own partitions, and never drop one
**2026-11-24 · Accepted**

The monthly partition roll-forward runs inside radiusd, daily, and provisions
three months ahead. `create_monthly_partitions` is `SECURITY DEFINER` so the
application role can call it. Nothing drops an old partition.

*Why:* three tables are RANGE partitioned by month with no default partition,
and an insert outside every partition is refused rather than absorbed. The
function had carried a comment since migration 010 saying it "must be called
from the monthly maintenance job (OPS §12)". There was no such job: the
monthly checklist in §12.5 had nine items and partitions were not among them.
Every deployment therefore had about four months before it stopped recording
session history, CGNAT port-block allocations and NetFlow flows — silently,
because authentication is unaffected and the first symptom is a
lawful-interception request that cannot be answered.

An operational requirement that depends on someone remembering it every month
is a defect with a delay on it. The list of which tables to roll forward is
read from the catalogue, not written down, for the same reason.

*Cost:* `SECURITY DEFINER` widens what the application role can cause. The
services connect as `bss_app`, which does not own the partitioned parents, so
without it the scanner fails every pass with "permission denied for schema
public" — confirmed by running it that way, not assumed. The function is
pinned to a fixed `search_path`, is revoked from PUBLIC, and refuses any
argument that is not a partitioned table in `public`.

*Not decided:* retention. Nothing drops old partitions, so these tables grow
without bound. Dropping one destroys evidence, and how long CGNAT and session
records must be kept is a licence and DPDP question, not an engineering one.
Deliberately left for someone who can answer it (FR-NET-001).

*Detail:* `internal/db/partitions.go`, `migrations/076_partition_rollforward_runs_as_the_app.sql`, runbook §12.3.5.

---

## ADR-014 — One deployment per ISP, not shared tenancy
**2026-11-23 · Accepted**

Several ISPs are served by running several deployments, each with its own
database. There is no tenant model in the schema and none is planned.

*Why:* the alternative was estimated at 2–3 weeks with PostgreSQL row-level
security, which is the only version worth shipping — enforcing isolation in
several hundred queries by remembering a predicate is a cross-customer data
breach waiting to happen. Nothing in the schema isolates one ISP from
another today: no tenant table, no tenant column across 93 tables, and four
global `UNIQUE` constraints (`subscribers.username`, `subscribers.caf_number`,
`staff_users.username`, `nas_devices.ip`) that two ISPs would collide on.

*Cost:* one database, one process set and one upgrade per ISP. Acceptable at
the scale this is sold into; revisit only as an architecture project.

*Detail:* `HANDOFF.md`, "UI readiness" section.

---

## ADR-013 — Off-machine archival behind the existing Store seam
**2026-11-23 · Accepted**

Archived documents go to any S3-compatible bucket, selected by
`ARCHIVE_S3_BUCKET` and taking precedence over `ARCHIVE_DIR`.

*Why:* a copy on the same disk as the database is not disaster recovery.
`archive.Store` had been the seam since FR-DOC-001, so this needed no design.

*Cost:* one dependency (minio-go), which **raised the project's minimum Go
from 1.23 to 1.26** as a side effect — `go get` edits the `go` directive when
a dependency needs a newer toolchain.

*Detail:* `internal/archive/s3store.go`; ADR-010 for the toolchain fallout.

---

## ADR-012 — Prepaid only; postpaid closed, CHAP stays deferred
**2026-11-23 · Accepted**

FR-BIL-024 (postpaid with credit limits) is closed as not applicable.
FR-AAA-005 (CHAP) remains deferred.

*Why:* the platform is prepaid by design (FR-BIL-009) and there are no
postpaid customers. CHAP requires the server to hold a recoverable password
to recompute `MD5(id ‖ pw ‖ challenge)`, which trades a real security
property for a protocol no deployed NAS requires — every one in use speaks
PAP or EAP.

*Cost:* an ISP with postpaid customers cannot use this without the work
described in FR-BIL-024, which touches billing, suspension and grace
together.

---

## ADR-011 — A ledger posting happens at most once per source event
**2026-11-23 · Accepted**

`gl_journal_entries` carries a partial unique index on
`(source_type, source_id)`; a duplicate is reported as
`ledger.ErrAlreadyPosted` and treated by callers as success.

*Why:* three event kinds post automatically and all arrive through retrying
paths, so the same cause could be posted twice. A doubled journal entry is
the kind of wrong nobody notices until a trial balance is held against a bank
statement months later.

*Open question:* revenue is recognised when money moves through the wallet,
not when an invoice is raised, which makes the automated side cash-basis.
Moving to accrual needs an Accounts Receivable account and changes the
existing wallet posting — see `docs/ledger_phase2_posting_rules_DRAFT.md`.

---

## ADR-010 — The Go version is declared once, in go.mod
**2026-11-23 · Accepted**

CI reads `go-version-file: go.mod`; the Docker images track it; a test fails
if either falls behind.

*Why:* the pin drifted three times. CI said 1.20 against go.mod's 1.22 and
builds refused; the images said 1.22 against 1.23, which left the demo stack
unbuildable from 2026-11-16 serving months-old code rather than failing; and
adding minio-go moved go.mod to 1.26 with nothing else touched.

*Detail:* `internal/spec/toolchain_test.go`.

---

## ADR-009 — Subscriber and staff tokens are signed with different secrets
**Accepted, enforced 2026-11-23**

`PORTAL_JWT_SECRET` signs subscriber tokens; `JWT_SECRET` signs staff
tokens. `config.Load` derives the first as `JWT_SECRET + "_portal"` when
unset, so they can never be equal.

*Why:* a leaked staff token must not be replayable against subscriber
endpoints, or the reverse.

*Cost:* every route must verify the right one, and getting it wrong is
invisible in tests that mint tokens with whichever secret the route checks.
Two routes shipped for months verifying the staff secret, 401-ing every real
subscriber. `internal/api` now has a source-level test requiring every
subscriber-role route to use the portal key.

---

## ADR-008 — Requirement status is derived, not written down
**2026-11-23 · Accepted**

`internal/spec` reads the SRS and fails when a requirement is neither cited
in the code nor marked as deliberately unbuilt.

*Why:* `HANDOFF.md` claimed "98 implemented, 1 deferred" while the SRS had
held 143 requirements for forty-plus commits. A number maintained by hand in
prose is a number that will be wrong.

---

## ADR-007 — The job queue is PostgreSQL, and Redis is gone
**Accepted · commits `1ae0232`, `a1477d6`, `6fd5028`**

Asynq and Redis were replaced by a Postgres-backed queue, and Redis removed
from the system entirely.

*Why:* one datastore instead of two, no second failure domain, and no
Sentinel cluster to operate. The RADIUS hot path was moved off Redis
separately (`6fd5028`).

*Cost:* queue throughput is bounded by PostgreSQL. Not a constraint at the
scale measured (NFR-PERF-001 passes at 5,000 req/s).

*Note:* this invalidated L6-004 (Sentinel failover), which went on being
reported as an open finding for months afterwards — the clearest example of
why this index exists.

---

## ADR-006 — PostgreSQL HA is Patroni and etcd
**Accepted · `docker-compose.pg-ha.yml`**

A 3-node Patroni cluster over a 3-node etcd DCS, addressed by role rather
than by container name.

*Detail:* `specification_docs_v2/08_IDD` §8.2a; drill in
`scripts/pg_failover_drill`.

---

## ADR-005 — Invoice PDFs render in a local headless browser
**Accepted · commit `104fb35`**

Replaced Gotenberg with a locally detected Chromium/Edge.

*Why:* removed a container from the deployment and a network hop from the
billing path. *Cost:* the host needs a Chromium-family browser; absence is
detected and reported rather than crashing.

---

## ADR-004 — Console access is gated by section, not per handler
**Accepted**

`staffui.sections` lists which roles may open each screen, and
`requireSection` enforces it.

*Why:* one list to read when answering "who can see this".

*Cost:* a new screen is unreachable until listed — a handler, route and
template can all be correct and the page still answer 500. A test now
compares the registry against the embedded templates in both directions.

---

## ADR-003 — Archived documents are verified on read
**Accepted · FR-DOC-001/003**

Every archived object carries a SHA-256 recorded at write time and is
verified when retrieved.

*Why:* an archive that cannot prove a document is unchanged is not an archive
for GST or DPDP purposes.

---

## ADR-002 — PII is encrypted at rest with a versioned key store
**Accepted · `specification_docs_v2/09_SecD` §9.3**

AES-GCM, keys held in a store with an active version and rotation protocol.
`cmd/api` refuses to start without it.

*Why:* KYC scans and identity documents. *Cost:* losing the key store loses
the data; refusing to start is deliberate, so a misconfiguration cannot
silently write plaintext.

---

## ADR-001 — The LEA audit trail is append-only in the database
**Accepted · migrations 014 and 019, FR-OBS-003**

`lea_audit_log` is append-only via an insert-only RLS policy (migration 014)
*and* by revoking SELECT, UPDATE and DELETE from the role the services
actually run as (migration 019), so the application cannot rewrite it.

Both halves are needed: RLS alone never applied, because a policy does not
constrain the table's owner. The same pattern was later applied to
`staff_audit_log` (FR-OBS-006, migration 062).

*Why:* an audit trail the application can rewrite is not evidence.

*Cost:* corrections are impossible by design. The append-only test now runs
each statement in its own transaction — an earlier version ran four checks in
one, so once the UPDATE was denied the rest passed on "current transaction is
aborted" rather than on "permission denied".
