# Document 3: System Architecture Design (SAD)
**Version:** 2.1 | **Status:** Draft | **Date:** 2026-09-03 — SAD-COMP-002/003 and §3.3 rewritten: the Redis Sentinel tier and Asynq were removed from the system (migrations 036/037) but this document still specified both as current architecture, including data-flow diagrams for a Redis stream that does not exist. §3.4 loses its Redis backup row and gains the AES key store, which a PostgreSQL restore genuinely cannot recover
**Document ID:** SAD
**Traces From:** [SRS](02_SRS_System_Requirements.md)
**Traces To:** [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md) → [IDD](08_IDD_Infrastructure_Design.md)

---

## 3.1 Architectural Style

The platform implements an asynchronous, decoupled, multi-tier architecture. The high-volume RADIUS control plane is fully isolated from the transactional relational storage layer through an intermediate caching and streaming pipeline.

**Design principles:**
- Cache-first auth: a warm RADIUS request touches neither PostgreSQL nor bcrypt
- One datastore: PostgreSQL holds application data, live session state and the task queue alike
- Observability by default: every tier emits Prometheus metrics and structured JSON logs
- Benefit-outcome traceability: every architectural decision maps to a business outcome in [CRD §1.1](01_CRD_Customer_Requirements.md)

---

## 3.2 Component Breakdown

### SAD-COMP-001 — AAA Control Plane Daemon
**Delivers:** FR-AAA-001..004, NFR-PERF-001, NFR-SCAL-001
**Detail:** [MDS §4.1](04_MDS_Module_Design.md), [DDS §5.1–5.3](05_DDS_Detailed_Design.md)

Go runtime executing `layeh.com/radius` bindings across a 128-worker bounded channel pool. Single master listener forwards UDP packets to workers. Workers authenticate from an in-process cache, falling through to PostgreSQL on a miss.

### SAD-COMP-002 — Caching and Session State (in-process + PostgreSQL)
**Delivers:** FR-AAA-002, FR-FUP-001
**Detail:** [IDD §8.3](08_IDD_Infrastructure_Design.md)

**There is no separate cache tier.** Earlier revisions of this document
specified a 3-node Redis Sentinel cluster here; it was removed once each of
its responsibilities found a better home, and Redis is no longer a
dependency of this system at all.

| Was on Redis | Now |
|---|---|
| Subscriber auth cache | In-process map per service (`internal/cache`), 60s TTL |
| Live session state | `live_sessions` table (migration 036) |
| Task and dead-letter queues | `jobqueue_tasks` table (migration 037) |
| Fast-verifier cache | In-process, with a PostgreSQL tier for restart survival (migration 046) |
| Dedup keys, rate-limit buckets | In-process (`internal/localcache`) |

The consequence is deliberate and worth stating: **PostgreSQL is now a
single point of failure for everything**, not only billing data. That is
what SAD-COMP-001's HA story (IDD §8.2a) has to carry, where previously the
risk was split across two datastores.

### SAD-COMP-003 — Asynchronous Processing (PostgreSQL-backed task queue)
**Delivers:** FR-FUP-002..003, FR-BIL-004, FR-NOTIF-001..011, FR-REV-002
**Detail:** [MDS §4.4](04_MDS_Module_Design.md)

A worker pool over `jobqueue_tasks` (`internal/jobqueue`), using
`SELECT … FOR UPDATE SKIP LOCKED` for weighted multi-queue dequeue. Handles
CoA/PoD delivery with retry, dunning stage execution, notification dispatch
(WhatsApp + SMS + email), PII re-encryption rotation, invoice PDF
generation, and revenue reconciliation.

This replaced Asynq when the queue moved off Redis. The gain beyond one
fewer datastore: a task and the row that caused it now commit in the same
transaction, which no external queue could offer — an enqueue can no longer
succeed against a write that then rolls back.

### SAD-COMP-004 — Relational Storage Core (PostgreSQL 15)
**Delivers:** FR-BIL-001..007, FR-REV-001..004, NFR-DUR-001
**Detail:** [DBD](06_DBD_Database_Design.md)

Primary + synchronous streaming read replica. Absolute authority for financial records, subscriber profiles, notification logs, CGNAT allocations, franchise ledgers, and audit trails. Analytics and dashboard queries route to replica.

### SAD-COMP-005 — Notification Service (WhatsApp Business API + SMS + Email)
**Delivers:** FR-NOTIF-001..011, CRD-NOTIF-001..002, CRD-REG-003
**Detail:** [MDS §4.7](04_MDS_Module_Design.md), [DDS §5.8](05_DDS_Detailed_Design.md)

Dedicated notification dispatcher invoked by queued tasks. Responsibilities:
- DND flag check before every outbound message
- WhatsApp Business API template dispatch with delivery status callback ingestion
- SMS gateway dispatch (configurable provider: Twilio / MSG91 / Exotel)
- Email dispatch via SMTP
- `notification_log` record creation per message (FR-NOTIF-009)
- WhatsApp delivery status webhook receiver: `sent → delivered → read / failed` (FR-NOTIF-011)

### SAD-COMP-006 — API Gateway & RBAC
**Delivers:** FR-SEC-005, FR-OBS-004, FR-REV-003..004, FR-SUB-001..005, FR-FRN-003
**Detail:** [DDS §5.7](05_DDS_Detailed_Design.md), [API](07_API_OpenAPI_Contract.md)

HTTP API layer with JWT middleware. Four roles + franchise role. Exposes subscriber health endpoint (SAD-COMP-008), revenue dashboard, collections forecast, and franchise P&L view.

### SAD-COMP-007 — Observability Stack
**Delivers:** FR-OBS-001..005, NFR-AVAIL-001, CRD PER-002
**Detail:** [IDD §8.6](08_IDD_Infrastructure_Design.md)

- Prometheus metrics on all services; Grafana dashboards
- Structured JSON logs via zerolog → Loki
- PagerDuty alerting for: uptime failures, dead-letter queue depth > 0, PostgreSQL failover and replication lag > 5s, **NAS auth failure rate > 20% over 5 min (FR-OBS-005)**
- Correlation IDs propagated through all service calls for cross-service tracing

### SAD-COMP-008 — Subscriber Health API *(new — gap PER-002, PER-004)*
**Delivers:** FR-OBS-004, CRD PER-002, PER-004, PER-005
**Detail:** [DDS §5.9](05_DDS_Detailed_Design.md), [API §7](07_API_OpenAPI_Contract.md)

Single-call `GET /api/v1/subscribers/{id}/health` that aggregates: active session state (from `live_sessions`), FUP status, current speed profile, last CoA result, wallet balance, plan expiry, open ticket count, last notification sent (from `notification_log`). Designed for CSR to answer a complaint call in under 30 seconds.

### SAD-COMP-009 — Revenue Assurance Module *(new — gap BO-001)*
**Delivers:** FR-REV-001..004, CRD-REV-001..002, CRD BO-001
**Detail:** [MDS §4.8](04_MDS_Module_Design.md)

Scheduled nightly job and on-demand API that: identifies unbilled active subscribers, reconciles ledger totals, computes 30-day forward collections forecast, and feeds the collections dashboard.

### SAD-COMP-010 — Subscriber Self-Service Portal *(new — gap PER-006)*
**Delivers:** FR-SUB-001..005, CRD PER-006, CRD BO-003
**Detail:** [MDS §4.9](04_MDS_Module_Design.md)

Web-based portal (responsive HTML/JS). Authenticated via subscriber-specific JWT. Shows real-time usage, plan details, wallet, invoices, notification history, and ticket management. Renewal deep-links to Razorpay/BBPS payment flow.

### SAD-COMP-011 — Franchise / LCO Module *(new — gap BO-004)*
**Delivers:** FR-FRN-001..003, CRD-FRN-001, CRD BO-004
**Detail:** [MDS §4.10](04_MDS_Module_Design.md)

Multi-tenant data isolation via `franchise_id` column on subscriber and ledger tables. LCO portal is a scoped view of the main API. Commission calculation runs as a queued task on each recharge event.

---

## 3.3 Data Flow & Concurrency Management

### RADIUS Authentication Hot Path
```
NAS → UDP :1812 → PacketServer read loop
  → bounded queue (512) → Worker Pool (128 goroutines)
    → in-process subscriber cache (60s TTL)
      → [HIT]  → fast-verifier cache: skip bcrypt if this exact
      │          (password, hash) pair was already verified
      → [MISS] → PostgreSQL; populate cache
    → Build Access-Accept with the vendor's rate-limit attribute
```

Two things about this path are load-bearing and easy to miss:

- **The queue sheds rather than blocks when full.** The RADIUS library
  spawns a goroutine per datagram with no bound, so blocking on a full
  queue retains memory instead of applying backpressure. A shed packet
  costs one NAS retransmit; a blocked one costs the daemon.
- **A cache miss costs bcrypt (~280ms), not a database round trip.** That
  is the real cost of a cold start, and why the verifier cache persists
  across restarts (migration 046).

### Accounting & FUP Path
```
NAS → UDP :1813 → Worker
  → in-process dedup check (retransmit window)
    → [DUPLICATE] → ACK; skip
    → [NEW]
        → UPDATE subscriber_session_history (octets)
        → UPDATE live_sessions (read surface for portal + health)
        → FUP scanner (10s tick), independently:
            sums octets across a subscriber's OPEN sessions
              → [80% reached]  → enqueue notification task
              → [100% breach]  → enqueue CoA task + notification
                  → [ACK]        → complete
                  → [Timeout ×5] → dead-letter; alert
```

> The FUP scanner sums across *open* sessions because a reconnect
> legitimately opens a new row mid-cycle while the quota is per-cycle. That
> makes closing sessions a correctness concern rather than housekeeping: a
> row left open by a missing Accounting-Stop keeps contributing its octets
> forever and throttles the subscriber early. `CloseSupersededSessions`
> exists for exactly that.

### Notification Dispatch Path
```
queued notification task
  → Check dnd_opt_out flag (PostgreSQL)
    → [opted out]  → skip dispatch; log suppression in notification_log
    → [allowed]
        → WhatsApp Business API (template dispatch)
        → SMS gateway
        → Email (if dunning reminder)
        → Write notification_log record (FR-NOTIF-009)
  → WhatsApp delivery webhook callback
        → Update notification_log.delivery_status (sent → delivered → read / failed)
```

### Accounting persistence

Accounting writes go straight to PostgreSQL on the worker that handled the
packet. There is no batching stage.

The earlier design buffered accounting in a Redis stream and flushed it with
a periodic COPY worker, which is why this section used to describe one. That
bought throughput at the cost of a window in which acknowledged accounting
existed nowhere durable — and the write turned out not to need it: it is a
single indexed UPDATE on a partitioned table, well inside the budget at the
volumes NFR-SCAL-001 specifies.

---

## 3.4 High Availability & Disaster Recovery

| Asset | Method | Frequency | RTO | RPO | Ops Ref |
|---|---|---|---|---|---|
| PostgreSQL data | WAL archiving + pg_dump | WAL: 5 min / Full: weekly | 5 min (failover) / 2h (restore) | 0 (sync replica) | OPS §12.3.2 |
| Configuration | Git | On every change | 10 min | Indefinite | — |
| TLS certificates | Auto-renew | 30d before expiry | 10 min | — | — |
| AES key store | **Manual, off-host** | On rotation | — | — | IDD §8.5 |

There is no second datastore to back up. Live session state and the task
queue are tables inside the PostgreSQL row above, and the in-process caches
rebuild on demand — a restore of PostgreSQL restores everything the system
persists.

With one exception, called out because a database restore will not save
you: **the AES key store is not in the dump.** It decrypts the PII columns,
it cannot be regenerated, and without it those rows survive a restore as
permanently unreadable ciphertext. It has to be backed up separately, off
the host.

PostgreSQL failover is Patroni's (IDD §8.2a), not this document's — see
there for the promotion path and the `ttl` that governs how quickly it
happens.
