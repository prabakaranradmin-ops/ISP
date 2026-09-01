# Document 8: Infrastructure Design Document (IDD)
**Version:** 2.2 | **Status:** Draft | **Date:** 2026-09-01 — Redis removed throughout: it had not been in the code since sessions moved to `live_sessions` (036) and the task queue to `jobqueue_tasks` (037), but this document still specified a six-container Sentinel cluster as production infrastructure. §8.3 repurposed from "Redis Sentinel Configuration" to a record of where each of that tier's responsibilities went. §8.2 no longer inlines a copy of `docker-compose.yml` — that duplication is what drifted in the first place; it now points at the file and describes it. §8.2a's rationale rewritten where it argued from a Redis comparison that no longer holds.
**Document ID:** IDD
**Traces From:** [SAD](03_SAD_System_Architecture.md)
**Traces To:** [DXD](11_DXD_Developer_Setup.md) → [OPS](12_OPS_Operations_Runbook.md)

---

## 8.1 Production Environment Topology

All services run as Docker containers managed by Docker Compose on Ubuntu 22.04 LTS nodes. The topology isolates the external network entry plane from the internal data tier via dedicated Docker networks.

```
Internet / NAS (CCR2004)
        │
        ├── UDP :1812/:1813  ──► aaa_core_daemon
        │
        └── HTTPS :443       ──► reverse_proxy (nginx/caddy)
                                        │
                                        └── api_service :8080

Internal network (bss_internal):
  aaa_core_daemon ──► postgres_primary
  api_service     ──► postgres_primary
  api_service     ──► gotenberg_engine
```

PostgreSQL is the only datastore. Live session state (`live_sessions`,
migration 036) and the background task queue (`jobqueue_tasks`, migration
037) both live there; the subscriber authentication cache is in-process
(`internal/cache`, one map per service). There is no separate cache or
queue tier to deploy, scale or fail over — see § 8.3.

---

## 8.2 Production Docker Compose Blueprint

The deployment is defined by [`docker-compose.yml`](../docker-compose.yml) in
the repository root, with [`docker-compose.pg-ha.yml`](../docker-compose.pg-ha.yml)
as the optional HA overlay described in § 8.2a.

**This section deliberately does not reproduce those files.** It used to
inline the whole of `docker-compose.yml`, and that copy drifted: it still
described a six-container Redis tier (primary, two replicas, three
sentinels) for months after the code stopped using Redis at all, because
nothing makes a pasted copy fail when the original changes. The files
themselves are the specification; this section describes what they contain
and why.

### Services

| Service | Image | Purpose |
|---|---|---|
| `postgres_primary` | `postgres:15-alpine` | The only datastore — application tables, live session state (migration 036) and the task queue (037) |
| `aaa_core_daemon` | built from `Dockerfile` | RADIUS auth/accounting on UDP 1812/1813, plus every background scanner and the task worker pool |
| `api_service` | built from `Dockerfile.api` | HTTPS API, staff console and subscriber portal on 8080 behind the proxy |
| `gotenberg_engine` | `gotenberg/gotenberg:8` | HTML-to-PDF rendering for invoices |
| `reverse_proxy` | Caddy | TLS termination on 443, the only externally published TCP port |

Both application services `depend_on` `postgres_primary` with
`condition: service_healthy`, so neither starts against a database that is
not yet accepting connections.

### Networking

One internal bridge network, `bss_internal`. Only two things are published
to the host: 443 on the proxy, and UDP 1812/1813 on `aaa_core_daemon` —
the latter necessarily, since routers must reach it directly and RADIUS is
not proxyable over HTTP.

### Volumes

`pg_data_primary` for the database, `caddy_data` and `caddy_config` for
certificates and proxy state. Nothing else needs persistence.

---

## 8.2a PostgreSQL High Availability *(new — NFR-AVAIL-002, v3)*

### Why this exists

PostgreSQL, where `subscribers`, `wallet_ledgers`, and every billing record
live, has been a single container this whole time — and since the task
queue and live session state moved into it (migrations 036/037), it is now
the *only* datastore, so its availability is the platform's availability.
OPS §12.3.2 already documents a promotion procedure that assumes a replica
(`bss_postgres_replica`) and claims RPO = 0 — neither the replica nor any
automation behind that claim has ever existed until this section. This
closes that gap rather than leaving the runbook describing a system that
was never built.

### Mechanism: Patroni + etcd, not repmgr or pg_auto_failover

**Patroni**, coordinating through a 3-node **etcd** cluster, was chosen over
the alternatives for reasons specific to this deployment, not in the
abstract:

- **repmgr** needs its own daemon (`repmgrd`) per node for automatic
  failover and is generally considered less battle-tested for *automatic*
  promotion than Patroni — repmgr's strength is manual/assisted failover,
  which OPS §12.3.2 already has a procedure for and which this section is
  explicitly trying to move beyond.
- **pg_auto_failover** avoids a separate DCS (it uses a monitor node
  instead of etcd/Consul), which is a real point in its favor given the
  three extra containers etcd costs — but it sees
  meaningfully less production adoption and community troubleshooting
  material than Patroni, which matters for a small ops team debugging an
  incident at 3 a.m., not just for the failover mechanics themselves.
- **Patroni** is the most widely deployed, most actively maintained option,
  with the deepest community/StackOverflow surface for exactly the kind of
  incident OPS §12.3 is written for. The etcd dependency is real overhead —
  three coordinator containers whose only job is leader election — and it
  is the price of automatic promotion; the alternative is the manual
  procedure OPS §12.3.2 documents, with an operator in the loop for every
  failover.

### Topology

```
                    ┌─────────────┐
                    │  etcd_1/2/3 │  (DCS — leader election, quorum 2)
                    └──────┬──────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
┌───────▼──────┐   ┌───────▼────────┐  ┌───────▼────────┐
│postgres_primary│  │postgres_standby_1│ │postgres_standby_2│
│  (Patroni +    │◄─┤ (Patroni +      │◄┤ (Patroni +      │
│   postgres)    │  │  postgres,      │ │  postgres,      │
│                │  │  streaming      │ │  streaming      │
│                │  │  replica)       │ │  replica)       │
└───────▲────────┘  └────────────────┘ └────────────────┘
        │
   aaa_core_daemon / api_service
   DB_DSN lists all three hosts +
   target_session_attrs=read-write —
   pgx (this codebase's driver) tries
   each in order and picks the one
   currently accepting writes. No
   HAProxy/PgBouncer layer: pgx's
   native multi-host support (verified
   against pgx v5's pgconn — the same
   libpq-compatible behavior as
   `host=a,b,c target_session_attrs=
   read-write`) makes a proxy
   unnecessary for this Go-only
   client population.
```

`postgres_primary`/`postgres_standby_N` are static service-name labels, not
claims about runtime role — after a failover, `postgres_standby_1` may be the real
primary. `curl postgres_primary:8008/primary` (Patroni's REST API, 200 only
on the current leader) is the source of truth for current role, not the
container name.

### Deployment files

| File | Purpose |
|---|---|
| `Dockerfile.postgres-ha` | `postgres:15-alpine` + Patroni, one image for all three nodes |
| `config/postgres/patroni.yml` | Identical on every node; `PATRONI_NAME`/`PATRONI_*_CONNECT_ADDRESS`/passwords come from environment, not separate YAML files — see the file's own header comment for why that's the correct pattern for Patroni specifically |
| `docker-compose.pg-ha.yml` | Overlay, not an edit to `docker-compose.yml` — apply with `docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml up -d`. Local/demo use (`scripts/demo_up.sh`) is unaffected and keeps running a single Postgres |

### Commit-durability setting

`synchronous_mode: true` / `synchronous_mode_strict: false` in
`patroni.yml`: Patroni requires a synchronous standby when one is healthy,
but degrades to async rather than stalling every write if that standby
becomes unreachable — a single dead standby must not turn into a full write
outage on an otherwise-healthy primary. Within that, `synchronous_commit:
remote_write` (not the stricter `on`) acknowledges a commit once the
standby has *received* the WAL over the network, not once it has fsynced it
to its own disk — durable against a primary crash, without a second
disk-flush round-trip added to every `wallet_ledgers`/RADIUS-accounting
write (DBD §6.6 covers the query-routing side of this same trade-off).

### What is not solved by this file alone

The connection string change (multi-host DSN, below) makes every *new*
`internal/db` connection resolve to the correct current primary
automatically — no Go code changes needed for that part, confirmed against
`pgx/v5`'s `pgconn.ParseConfig`, which implements the same
`target_session_attrs` behavior as libpq. What it does **not** do: a
connection pgxpool already had checked out *at the moment of failover* is
still physically talking to the old primary, now a read-only standby. The
next write on that specific connection fails with SQLSTATE `25006`
(`read_only_sql_transaction`) — a normal SQL error, not a network fault, so
pgxpool has no built-in reason to evict that connection on its own. Closing
that gap is a small, real `internal/db` change (recognize `25006`, force
that one connection closed, let the pool dial fresh — which then resolves
correctly via `Fallbacks`) and is scoped as its own implementation pass once
this topology is actually running, not bundled into this configuration
change.

**Implemented as `internal/db/hapool.go`** (`dbPool` interface, `haPool`
wrapper around `*pgxpool.Pool`, `haRow`/`haTx` for the `QueryRow`/transaction
paths) — every store in `internal/db` now goes through it. Verified live,
not just unit-tested, using `scripts/pg_failover_drill` (a small program
using this codebase's own `internal/db.Connect` and a real store's write
method, run against an actual 3-node Patroni cluster while the primary was
killed/switched over):

| Test | What happened | Recovery mechanism | Time |
|---|---|---|---|
| `docker kill` on the primary | Connection died mid-write (`unexpected EOF`) | pgx's own connection-health tracking + `Fallbacks` reconnect — `haPool` never engaged | ~26s |
| Graceful `patroni ... switchover` | Patroni restarts the demoted node during role transition, terminating its connections (`SQLSTATE 57P01`) | Same as above — `haPool` never engaged | ~4s |
| Backend held read-only while the connection stayed alive (`default_transaction_read_only=on`, no restart) — the specific condition `haPool` targets | Write failed with `SQLSTATE 25006` exactly as designed | `haPool.checkFailover` detected it, logged, called `Pool.Reset()` | ~4s once a real primary existed again |

The honest finding: in this Patroni configuration, both real failure modes
tested (hard crash, graceful switchover) terminate connections outright
rather than leaving them alive-but-demoted — so `pgx`'s own native
reconnection already recovers them, without `haPool`'s SQLSTATE 25006 logic
ever engaging. That logic is still correct and worth keeping — the
alive-but-read-only condition it defends against is real (a manual
`pg_promote()` without full Patroni-orchestrated demotion of the old
primary, or certain proxy/timing configurations, can produce exactly this),
and the third test above confirms it fires precisely as designed when that
condition actually occurs. It just wasn't the mechanism that happened to
fire in either of the two most obvious failure drills.

---

## 8.3 Caching and Queueing — No Separate Tier

Earlier versions of this document specified a Redis Sentinel cluster here
(a primary, two replicas, three sentinels) backing the subscriber
authentication cache, live session state and the background task queue.
**That tier no longer exists** and this section records where each of its
responsibilities went, because the question "where is the cache?" has a
different answer now than the rest of this document's history implies.

| Was on Redis | Now | Why |
|---|---|---|
| Live session state | `live_sessions` table (migration 036) | It is read by the health endpoint and the portal's usage panel, and rebuilt from accounting traffic — durability was never the requirement, and one datastore is one thing to operate |
| Background task queue | `jobqueue_tasks` table (migration 037) | `SELECT … FOR UPDATE SKIP LOCKED` gives the same weighted multi-queue dequeue with transactional enqueue — a task and the row that caused it now commit together, which Redis could not offer |
| Subscriber auth cache | In-process map (`internal/cache`) | `radiusd` runs as a single process per host in the native install, so there are no peers to share a cache with; a network hop to a cache server was pure latency against NFR-PERF-001's 15 ms budget |

Consequences worth stating plainly, since they change how this system is
operated:

- **PostgreSQL is now a single point of failure for everything**, not just
  billing data. That is the argument § 8.2a exists to answer.
- **The auth cache does not survive a restart** and is not shared between
  services. This is fine — it is a read-through cache with a 60-second TTL
  (`cache.DefaultSubscriberTTL`); a cold start means the first
  authentication per subscriber reaches PostgreSQL, not that anything
  fails.
- **There is no cache tier to fail over, back up, or restore.** OPS § 12.3
  and § 12.4 were rewritten accordingly; a restore of PostgreSQL restores
  everything.

---

## 8.4 Environment Variables Reference

| Variable | Required | Description |
|---|---|---|
| `DB_SECURE_PASSWORD` | Yes | PostgreSQL superuser password |
| `PG_REPLICATION_PASSWORD` | Only with `docker-compose.pg-ha.yml` | Postgres replication user password (§8.2a) |
| `JWT_SECRET` | Yes | HMAC secret for JWT signing |
| `RAZORPAY_WEBHOOK_SECRET` | Yes | HMAC secret for Razorpay webhook validation |
| `AES_KEY_STORE_URL` | Yes | Secret manager URL for AES key retrieval |
| `PAGERDUTY_ROUTING_KEY` | Yes | PagerDuty Events API v2 routing key |
| `LOG_FORMAT` | No | `json` (default) or `text` |
| `LOG_LEVEL` | No | `debug`, `info`, `warn`, `error` |

Store all secrets in a `.env` file excluded from version control (`.gitignore`), or inject via your secret manager of choice.

---

## 8.5 Backup Procedures

### PostgreSQL

```bash
# WAL archiving: add to postgresql.conf
archive_mode = on
archive_command = 'aws s3 cp %p s3://your-bucket/wal/%f'
wal_level = replica

# Weekly full backup (run via cron on backup host)
pg_dump -h localhost -U postgres -Fc isp_bss_oss \
  | aws s3 cp - s3://your-bucket/pgdump/$(date +%Y%m%d).dump

# Point-in-time restore target (RPO = last WAL archive ~5 min)
```

### Everything else

Nothing else needs backing up. The PostgreSQL dump above is the complete
backup of this system's state: live sessions and the task queue are tables
inside it (§ 8.3), and the auth cache is in-process and rebuilt on demand.

Two things are deliberately *not* in that dump and are not recoverable from
it — they belong to whatever provisions the host, not to a database backup:

- `AES_KEY_STORE_URL`'s key material. Losing it makes every encrypted PII
  column unreadable, and no PostgreSQL restore brings it back.
- Caddy's certificate store (`caddy_data`), which is cheap to re-acquire
  from the CA but will re-trigger rate limits if lost repeatedly.

---

## 8.6 Health & Readiness Probe Summary

| Service | Probe Type | Endpoint / Command | Interval |
|---|---|---|---|
| `postgres_primary` | TCP + query | `pg_isready` | 10s |
| `postgres_primary`/`_standby_N` (pg-ha overlay only) | HTTP | `GET :8008/primary` returns 200 only on the current leader (§8.2a) | operator/OPS-script use, not a container healthcheck |
| `aaa_core_daemon` | Prometheus scrape | `:9100/metrics` | 15s |
| `api_service` | HTTP | `GET /health` | 15s |
| `gotenberg_engine` | HTTP | `GET /health` | 30s |

---

## 8.7 WhatsApp Business API Container *(new — MOD-NOTIF)*
**FR:** FR-NOTIF-001..011 | **SAD Ref:** SAD-COMP-005

The notification service connects to Meta's Cloud API externally (no self-hosted container). Add the following environment variables to `api_service` and `aaa_core_daemon`:

```yaml
# Add to api_service and aaa_core_daemon environment in docker-compose.yml
environment:
  - WHATSAPP_PHONE_NUMBER_ID=${WHATSAPP_PHONE_NUMBER_ID}
  - WHATSAPP_ACCESS_TOKEN=${WHATSAPP_ACCESS_TOKEN}
  - WHATSAPP_WEBHOOK_VERIFY_TOKEN=${WHATSAPP_WEBHOOK_VERIFY_TOKEN}
  - SMS_GATEWAY_PROVIDER=${SMS_GATEWAY_PROVIDER}   # twilio | msg91 | exotel
  - SMS_GATEWAY_API_KEY=${SMS_GATEWAY_API_KEY}
  - SMS_GATEWAY_SENDER_ID=${SMS_GATEWAY_SENDER_ID}
```

**WhatsApp Webhook Public URL:** The `api_service` must be reachable from Meta's servers on `POST /webhooks/whatsapp`. Configure your reverse proxy to forward this path. Meta requires HTTPS with a valid TLS certificate (self-signed not accepted).

**Additional env vars reference (v2 additions):**

| Variable | Required | Description |
|---|---|---|
| `WHATSAPP_PHONE_NUMBER_ID` | Yes | Meta Business phone number ID |
| `WHATSAPP_ACCESS_TOKEN` | Yes | Meta permanent/system user access token |
| `WHATSAPP_WEBHOOK_VERIFY_TOKEN` | Yes | Random string for Meta webhook verification |
| `SMS_GATEWAY_PROVIDER` | Yes | `twilio`, `msg91`, or `exotel` |
| `SMS_GATEWAY_API_KEY` | Yes | Provider API key |
| `SMS_GATEWAY_SENDER_ID` | Yes | Approved SMS sender ID (DLT registered) |
| `PORTAL_JWT_SECRET` | Yes | Separate secret for subscriber-scoped portal JWTs |
