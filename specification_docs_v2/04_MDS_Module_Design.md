# Document 4: Module Design Specification (MDS)
**Version:** 2.2 | **Status:** Draft | **Date:** 2026-08-12 — §4.13 added (CRD §1.11 Phase 3); §4.11–4.12 unchanged from v2.1, §4.1–4.10 unchanged from v2.0
**Document ID:** MDS
**Traces From:** [SAD](03_SAD_System_Architecture.md) → [SRS](02_SRS_System_Requirements.md)
**Traces To:** [DDS](05_DDS_Detailed_Design.md) → [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 4.1 Module 1: AAA Core Network Daemon
**Module ID:** MOD-AAA | **SAD Ref:** SAD-COMP-001 | **FR:** FR-AAA-001..004

`internal/radius` — Processes UDP streams on ports 1812/1813. Fixed 128-worker channel pool; never touches PostgreSQL on hot path.

**Responsibilities:** Receive and parse RADIUS packets, authenticate from Redis (PostgreSQL fallback on miss), write-through new sessions, deduplicate Interim-Update packets via SetNX, increment per-session traffic counters atomically, publish accounting events to Redis stream, enforce brute-force rate limiting via token bucket.

**Key Metrics Emitted:**
- `radius_auth_duration_seconds` (histogram, p50/p95/p99)
- `radius_auth_total` (counter, labels: `result=accept|reject|error`)
- `radius_accounting_packets_total` (counter, labels: `type=start|interim|stop`)
- `radius_dedup_skipped_total` (counter)
- `radius_worker_queue_depth` (gauge)
- `radius_nas_failure_rate` (gauge per NAS IP — feeds FR-OBS-005 alert)

---

## 4.2 Module 2: Cache & Real-Time FUP Monitoring
**Module ID:** MOD-FUP | **SAD Ref:** SAD-COMP-002 | **FR:** FR-FUP-001..005

`internal/fup` — Maintains write-through subscriber profiles in Redis. Background goroutine samples active sessions every 10s, compares usage vs threshold, enqueues tasks.

**FUP Threshold Events:**

| Threshold | Task Enqueued | Idempotency Key |
|---|---|---|
| 80% of plan bytes | `notif:fup_warning` | `fup_warn:{session_id}:{day}` |
| 100% of plan bytes | `coa:send` + `notif:fup_throttle` | `coa:{session_id}:{breach_epoch_minute}` |
| Plan expiry | `pod:send` | `pod:{session_id}:{expiry_date}` |

**Task Definitions:**

| Task | Queue | Max Retries | Backoff | Dead-letter Action |
|---|---|---|---|---|
| `coa:send` | `network_commands` | 5 | Exponential (2s base) | Alert + dead-letter |
| `pod:send` | `network_commands` | 5 | Exponential (2s base) | Alert + dead-letter |
| `notif:fup_warning` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `notif:fup_throttle` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `dunning:remind` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `dunning:throttle` | `network_commands` | 5 | Exponential | Alert + dead-letter |
| `dunning:suspend` | `network_commands` | 5 | Exponential | Alert + dead-letter |

**Key Metrics:**
- `fup_breaches_total` (counter, label: `plan_id`)
- `fup_warnings_80pct_total` (counter)
- `coa_ack_total` (counter, labels: `result=ack|nak|timeout`)
- `dead_letter_queue_depth` (gauge) — PagerDuty if > 0

---

## 4.3 Module 3: Transactional Billing & Tax
**Module ID:** MOD-BIL | **SAD Ref:** SAD-COMP-004 | **FR:** FR-BIL-001..007

`internal/billing` — GST invoices with `decimal.Decimal`, dunning state machine, idempotent wallet, HMAC webhook validation, Gotenberg PDF, GSTR-1 export.

**Dunning State Machine:**
```
ACTIVE
  → [T-7d]    REMINDED_7       (WhatsApp + SMS + email)
  → [T-3d]    REMINDED_3       (WhatsApp + SMS)
  → [T-1d]    REMINDED_1       (WhatsApp + SMS)
  → [T+0]     GRACE_PERIOD
  → [T+24h]   SOFT_SUSPENDED   (CoA throttle + WhatsApp + SMS with payment link)
  → [T+72h]   HARD_SUSPENDED   (PoD + WhatsApp + SMS)
  → [Recharge → any state] → ACTIVE  (CoA restore + WhatsApp + SMS confirmation)
```
All transition notifications must pass DND check (FR-NOTIF-008).

**GSTR-1 Export (FR-BIL-006):** Monthly export job producing JSON/CSV with:
- B2B invoices (GSTIN-registered businesses): invoice-level detail
- B2C invoices (residential): state-wise aggregate
- HSN/SAC summary
- Nil-rated / exempt supplies if applicable

**Invoice PDF — Plain Language (FR-BIL-007):** Every invoice PDF must include a usage summary block:
```
Data used this cycle:  2,847 GB of 3,300 GB included
Speed applied:         100 Mbps / 100 Mbps (full speed)
```

**Key Metrics:**
- `wallet_recharge_total` (counter, labels: `method=razorpay|bbps|cash|manual`)
- `webhook_hmac_failures_total` (counter, labels: `provider=razorpay|bbps`)
- `dunning_transitions_total` (counter, labels: `to_state`)
- `invoice_generation_duration_seconds` (histogram)

---

## 4.4 Module 4: Asynq Background Tasks & Worker Orchestration
**Module ID:** MOD-TASK | **SAD Ref:** SAD-COMP-003 | **FR:** FR-FUP-002, FR-NOTIF-001..011, FR-REV-002

`internal/tasks` — 75-worker Asynq pool. Bulk PostgreSQL COPY flush (every 300s), dead-letter monitor (poll every 30s → PagerDuty if depth > 0), PII re-encryption rotation (90-day), invoice PDF generation, notification dispatch, revenue reconciliation.

**PII Re-encryption Safety:** Batch size 500, transactional commit per batch, resumable on failure, skips already-rotated records via `key_version_id` comparison.

---

## 4.5 Module 5: RBAC & API Security Middleware
**Module ID:** MOD-AUTH | **SAD Ref:** SAD-COMP-006 | **FR:** FR-SEC-005

`internal/middleware` — JWT validation + role enforcement at HTTP handler layer. Emits structured audit log on all state-modifying calls (actor, role, action, target, timestamp, correlation_id).

**Role Matrix:**

| Role | Subscribers | Sessions/CoA | Billing/Wallet | Tickets | LEA | Franchise | Revenue Dashboard |
|---|---|---|---|---|---|---|---|
| `noc_engineer` | Read | Read/Write | — | Read | With flag | — | — |
| `billing_admin` | Read/Write | — | Full | Read | — | — | Full |
| `csr` | Read | Read | Read | Read/Write | — | — | — |
| `technician` | Read | Read | — | Read/Write | — | — | — |
| `lco_partner` | Own only | Own only | Own only | Own only | — | Own | — |

---

## 4.6 Module 6: CGNAT & LEA Export
**Module ID:** MOD-CGNAT | **SAD Ref:** SAD-COMP-004 | **FR:** FR-NET-001..003

`internal/cgnat` — Records port-block allocations, provides LEA lookup API, writes tamper-evident audit record on every lookup. Access restricted to `noc_engineer` + `lea_access` claim.

---

## 4.7 Module 7: Notification Service (WhatsApp + SMS + Email) *(new — gap CRD-NOTIF-001)*
**Module ID:** MOD-NOTIF | **SAD Ref:** SAD-COMP-005 | **FR:** FR-NOTIF-001..011

`internal/notifications` — Dedicated dispatcher invoked exclusively by queued tasks. Never called synchronously from the request path.

**WhatsApp Business API Integration:**
- Provider: Meta Cloud API (v17+)
- Authentication: Bearer token from secret manager; rotated every 30 days
- Message type: Template messages only (pre-approved via Meta Business Manager)
- Template storage: `notification_templates` table with `channel`, `template_id`, `template_name`, `variables_schema`
- Delivery callback endpoint: `POST /webhooks/whatsapp` — receives `sent`, `delivered`, `read`, `failed` status updates; updates `notification_log.delivery_status`

**Template Catalogue:**

| Template ID | Event | Variables | Channel |
|---|---|---|---|
| `TMPL-001` | FUP 80% warning | `{{subscriber_name}}`, `{{gb_used}}`, `{{gb_total}}`, `{{plan_name}}` | WhatsApp + SMS |
| `TMPL-002` | FUP throttle applied | `{{subscriber_name}}`, `{{speed_reduced_to}}`, `{{payment_link}}` | WhatsApp + SMS |
| `TMPL-003` | Renewal reminder | `{{subscriber_name}}`, `{{days_left}}`, `{{amount}}`, `{{payment_link}}` | WhatsApp + SMS + Email |
| `TMPL-004` | Soft suspension | `{{subscriber_name}}`, `{{reason}}`, `{{payment_link}}` | WhatsApp + SMS |
| `TMPL-005` | Hard suspension | `{{subscriber_name}}`, `{{contact_number}}` | WhatsApp + SMS |
| `TMPL-006` | Service restored | `{{subscriber_name}}`, `{{plan_name}}`, `{{expiry_date}}` | WhatsApp + SMS |
| `TMPL-007` | Payment received | `{{subscriber_name}}`, `{{amount}}`, `{{transaction_id}}`, `{{new_expiry}}` | WhatsApp + SMS |
| `TMPL-008` | Ticket update | `{{subscriber_name}}`, `{{ticket_id}}`, `{{status}}` | WhatsApp |

**DND Check Flow:**
```
Notification task arrives
  → Load subscriber.dnd_opt_out from DB
    → [TRUE]  → Skip all marketing/reminder channels
               → Allow payment receipts and service restoration (transactional class)
               → Write notification_log with status='suppressed_dnd'
    → [FALSE] → Proceed with dispatch
```

**Notification Log Record (FR-NOTIF-009):**
Every dispatch attempt writes a `notification_log` row regardless of outcome. Fields: `subscriber_id`, `channel`, `template_id`, `triggered_by_event`, `triggered_by_entity_id`, `sent_at`, `delivery_status`, `failure_reason`, `provider_message_id`.

**Key Metrics:**
- `notification_dispatch_total` (counter, labels: `channel`, `template_id`, `status=sent|failed|suppressed`)
- `notification_delivery_latency_seconds` (histogram, labels: `channel`)
- `whatsapp_delivery_status_total` (counter, labels: `status=delivered|read|failed`)

---

## 4.8 Module 8: Revenue Assurance *(new — gap BO-001)*
**Module ID:** MOD-REV | **SAD Ref:** SAD-COMP-009 | **FR:** FR-REV-001..004

`internal/revenue` — Nightly Asynq job + on-demand API endpoint.

**Unbilled Subscriber Report (FR-REV-001):**
```sql
-- Subscribers active but with no invoice in current billing cycle
SELECT s.id, s.username, s.plan_expiry, s.wallet_balance
FROM subscribers s
LEFT JOIN invoices i ON i.subscriber_id = s.id
  AND i.created_at >= date_trunc('month', NOW())
WHERE s.status = 'active'
  AND i.id IS NULL;
```

**Ledger Reconciliation (FR-REV-002):**
```sql
-- Must return zero variance
SELECT
  SUM(s.wallet_balance)                            AS system_balance_total,
  SUM(CASE WHEN wl.entry_type='credit' THEN wl.amount ELSE 0 END)
  - SUM(CASE WHEN wl.entry_type='debit'  THEN wl.amount ELSE 0 END) AS ledger_net,
  ABS(SUM(s.wallet_balance) - (
    SUM(CASE WHEN wl.entry_type='credit' THEN wl.amount ELSE 0 END)
    - SUM(CASE WHEN wl.entry_type='debit' THEN wl.amount ELSE 0 END)
  ))                                               AS variance
FROM subscribers s
CROSS JOIN (SELECT entry_type, amount FROM wallet_ledgers) wl;
```

**Collections Forecast (FR-REV-004):** 30-day rolling window of subscribers with expiry dates, multiplied by plan price. Segments: will auto-renew (wallet ≥ plan price), at-risk (wallet < plan price), already lapsed.

---

## 4.9 Module 9: Subscriber Self-Service Portal *(new — gap PER-006)*
**Module ID:** MOD-PORTAL | **SAD Ref:** SAD-COMP-010 | **FR:** FR-SUB-001..005

`web/portal` — Responsive web app (server-side rendered or React SPA). Authenticated via subscriber-scoped JWT (separate from admin JWT; contains `subscriber_id` claim only, no admin roles).

**Portal Pages:**

| Page | Data Source | FR |
|---|---|---|
| Dashboard | Real-time usage from Redis, plan/wallet from DB | FR-SUB-001..002 |
| Usage history | `subscriber_session_history` monthly aggregate | FR-SUB-001 |
| Invoices & payments | `invoices`, `wallet_ledgers` | FR-SUB-002 |
| Renew plan | Razorpay/BBPS deeplink; idempotent `transaction_token` | FR-SUB-003 |
| Support tickets | `tickets` table; create + view status | FR-SUB-004 |
| Notification history | `notification_log` filtered by subscriber | FR-SUB-005 |

**Real-Time Usage Display:** Portal polls `GET /api/v1/subscribers/{id}/usage` (reads from Redis, not DB) every 60 seconds. Displays: GB used, GB remaining, FUP status, speed profile.

---

## 4.10 Module 10: Franchise / LCO Module *(new — gap BO-004)*
**Module ID:** MOD-FRN | **SAD Ref:** SAD-COMP-011 | **FR:** FR-FRN-001..003

`internal/franchise` — Multi-tenant isolation via `franchise_id` on subscriber, invoice, and ledger tables.

**LCO Commission Flow:**
1. Subscriber recharge event fires
2. `commission:calculate` task runs, applies LCO commission rate (configurable per LCO)
3. Credit entry posted to `lco_ledger` (separate from subscriber `wallet_ledger`)
4. Parent ISP dashboard aggregates across all LCOs for consolidated P&L

**Data Isolation:** Every DB query from LCO-scoped JWT automatically includes `AND franchise_id = {caller_franchise_id}` via middleware row-filter. LCO users cannot access other franchises' data even with direct API calls.

---

## 4.11 Module 11: Multi-Vendor NAS Attribute Engine *(new — gap CRD-EXP-001, FR-NAS-001..004)*
**Module ID:** MOD-NAS | **SAD Ref:** extends SAD-COMP-001 (AAA Control Plane Daemon) — new SAD-COMP entry pending a dedicated SAD pass | **FR:** FR-NAS-001..004

### Why this module exists

`internal/radius/handlers.go` and `internal/fup/coa_task.go` today hand-encode
one hardcoded MikroTik VSA (vendor 14988, attribute 8) into every
Access-Accept and CoA packet, regardless of what actually sent the request.
Any Cisco, Juniper, Huawei, ZTE, or wireless-controller NAS authenticates
subscribers correctly but receives an attribute it doesn't understand — the
subscriber connects at whatever the NAS's own default is, not their plan
speed, with no error anywhere because RADIUS silently ignores unrecognized
vendor attributes by design. This module replaces the single hand-rolled
builder with a per-NAS vendor strategy, without touching the working
MikroTik path (which becomes the reference implementation of the same
interface, and remains the fallback default — see rollout note below).

### Two fundamentally different attribute models

The vendor split isn't just "different attribute numbers" — it's two
different provisioning models, and the design has to carry both:

| Model | Vendors (attribute family — **verify against deployed firmware before relying on exact numbers**) | What RADIUS sends |
|---|---|---|
| **Dynamic numeric rate** | MikroTik (vendor 14988, `Mikrotik-Rate-Limit`, already implemented) · Huawei (vendor 2011, `Huawei-Input-Average-Rate` / `Huawei-Output-Average-Rate`) · ZTE (vendor-specific rx/tx rate pair, model-dependent) | A literal bps/rate value computed from `plans.rate_limit_string` at request time — no NAS-side pre-configuration needed per plan |
| **Policy/profile reference** | Cisco (vendor 9, `cisco-avpair`, e.g. `subscriber:sub-qos-policy-in=<name>`) · Juniper (named firewall filter / hierarchical policer reference, `Filter-Id` or vendor-specific) · Wireless controllers — Cisco WLC (vendor 14179, `Airespace-Data-Bandwidth-Average-Contract`), Aruba (vendor 14823, role/QoS reference), Ruckus (vendor-specific role reference) | A **name** the NAS resolves against a QoS policy/profile it already has provisioned locally — RADIUS never sends a raw number to these vendors |

The practical consequence: for reference-vendors, a plan tier must exist as a
matching named policy on the NAS *before* RADIUS can select it. That's an
operational/runbook dependency (OPS §12, to be added when this module is
scheduled), not something this module can provision remotely — CWMP/TR-069
(FR-CPE, Phase 4) provisions the CPE, not the NAS's own QoS policy-map table.

### Interface

```go
// internal/nas — new package
type RateProfile struct {
    RateLimitString string // "50M/50M" — plans.rate_limit_string, source for dynamic-rate vendors
    ProfileName     string // pre-provisioned NAS-side policy name, source for reference vendors
}

type AttributeBuilder interface {
    BuildAccept(p RateProfile) ([]*radius.Attribute, error) // Access-Accept
    BuildCoA(p RateProfile) ([]*radius.Attribute, error)    // CoA-Request
    // PoD carries no vendor attribute (RFC 3576 Disconnect-Request needs
    // only Acct-Session-Id) — not part of this interface.
}

var builders = map[Vendor]AttributeBuilder{
    VendorMikrotik: mikrotikBuilder{}, // wraps the existing, unmodified VSA logic
    VendorHuawei:   huaweiBuilder{},
    VendorZTE:      zteBuilder{},
    VendorCisco:    ciscoBuilder{},
    VendorJuniper:  juniperBuilder{},
    VendorWireless: wirelessBuilder{},
}
```

`internal/radius/handlers.go` (Access-Accept) and `internal/fup/coa_task.go`
(CoA) both call `nas.BuilderFor(vendor).BuildAccept/BuildCoA(profile)` instead
of constructing the VSA inline. `internal/fup/pod_task.go` is unchanged — PoD
needs no vendor-specific attribute.

### Vendor resolution and rollout safety

Vendor is resolved by looking up the requesting NAS's source IP (Access-
Request) or the session's recorded NAS IP (CoA, via the existing
`FUPStore.GetSubscriberNASSession` lookup) against the new `nas_devices`
table (DBD §6.2). **An IP with no matching row defaults to
`VendorMikrotik`** — this is deliberate, not an oversight: it reproduces
today's actual behavior exactly, so an existing MikroTik-only deployment
needs zero new rows to keep working after this ships. A
`nas_unclassified_total{nas_ip}` counter increments on every such fallback,
giving NOC an actionable list of NAS devices worth registering rather than a
silent assumption.

### Per-NAS RADIUS secret

Today's `internal/radius/daemon.go` verifies every packet against one global
`RADIUS_SECRET`. Per-NAS secrets require the packet server's secret lookup
itself to become IP-aware — `layeh.com/radius`'s `PacketServer.SecretSource`
accepts a `func(ctx, *net.UDPAddr) ([]byte, error)` precisely for this case.
Resolution order: `nas_devices` row for the source IP (decrypted — see DBD
§6.2) → fall back to the existing global `RADIUS_SECRET` if no row exists,
same backward-compatible default as vendor resolution above.

### Key Metrics

- `nas_unclassified_total` (counter, label: `nas_ip`) — NAS traffic seen with
  no `nas_devices` row, currently served on the MikroTik-fallback default
- `nas_attribute_build_errors_total` (counter, labels: `vendor`, `reason`) —
  e.g. a reference-vendor plan with no matching `plan_nas_profiles` row
- `radius_auth_total` gains a `nas_vendor` label (extends the existing
  metric from §4.1, not a new one)

---

## 4.12 Module 12: PostgreSQL High Availability & Failover *(new — gap CRD-EXP-001, NFR-AVAIL-002)*
**Module ID:** MOD-PGHA | **SAD Ref:** extends SAD-COMP-004 (Relational Storage Core) | **FR:** NFR-AVAIL-002

### Why this module exists

Redis has real Sentinel HA today (3 sentinels, quorum 2, tested failover —
IDD §8.3). PostgreSQL — where `subscribers`, `wallet_ledgers`, and every
billing record live — is a single container with no replica and no
failover. A crashed or corrupted primary is a full outage with a restore-
from-backup RTO, not a supervised promotion. This module's scope is
deliberately the **application-layer contract** a failover needs (connection
behavior, what's safe to route to a replica); the deployment topology itself
(which failover manager, how many standbys, DNS/VIP mechanics) belongs in
IDD §8 and is a separate infrastructure design pass — noted here as a
dependency, not designed in this document.

### Application-layer failover contract

- **Connection string carries both hosts.** `pgx`'s multi-host DSN
  (`host=pg_primary,pg_standby port=5432,5432 target_session_attrs=read-write`)
  lets the driver itself find the current primary after a promotion, rather
  than the application needing to watch for a failover event. `db.Connect`
  (`internal/db`) takes this DSN form; no application code changes on
  failover, only the DSN config.
- **Retry-with-backoff on connection-layer errors**, distinct from query
  errors: a promotion takes a real, bounded window (seconds, not
  milliseconds) during which every connection attempt fails. The existing
  Asynq retry pattern (`internal/fup`, `internal/billing` — exponential
  backoff, max 5 attempts) is the template; this module applies the same
  shape to the DB connection pool's own reconnect logic rather than
  inventing a second retry convention.
- **RADIUS auth path must fail closed, not open, on a DB outage during
  failover.** The subscriber cache (`internal/cache/subscriber_cache.go`)
  already serves reads from Redis with a 60s TTL and Postgres only on a
  cache miss — during a short promotion window, already-cached subscribers
  keep authenticating normally and only *new* logins or cache-miss re-auths
  are affected. This existing cache-first design is what keeps a Postgres
  failover from being a RADIUS outage, and should not be weakened while
  adding HA.

### Read-routing candidates (once a standby exists)

Not every read needs the primary. Reports that tolerate replica lag are
routing candidates, cutting primary load without a schema change:

| Query | Current source | Replica-safe? |
|---|---|---|
| Unbilled-subscriber report (FR-REV-001) | Primary | Yes — nightly batch, lag-tolerant |
| Ledger reconciliation (FR-REV-002) | Primary | **No** — must read a consistent point-in-time snapshot; replica lag would produce false variance alerts |
| Revenue/collections dashboards (staffui) | Primary | Yes — dashboard, not a transactional read |
| RADIUS auth fallback on cache miss | Primary | **No** — must read the current, not-lagged, subscriber state (status, plan) |
| CoA/PoD NAS/session lookup | Primary | **No** — same currency requirement as auth |

The rule of thumb this table encodes: anything that gates a live decision
(auth, CoA target, ledger truth) stays on the primary; anything that
summarizes history for a human is a replica candidate.

### Key Metrics

- `pg_replication_lag_seconds` (gauge, from `pg_stat_replication` on the
  primary) — feeds an OPS alert threshold (to be set in a later OPS pass)
- `db_connection_retry_total` (counter, labels: `outcome=recovered|exhausted`)
- `db_failover_detected_total` (counter) — incremented when the pool
  observes a primary-target change mid-session

---

## 4.13 Module 13: Helpdesk & SLA Engine *(new — gap CRD-EXP-002, FR-SUP-001..003)*
**Module ID:** MOD-SUP | **SAD Ref:** new component, extends the ticket write
paths already covered informally under SAD-COMP-006 (API Gateway & RBAC) —
a dedicated SAD-COMP entry is pending a full SAD pass | **FR:** FR-SUP-001..003

### What exists today, and the gap

`tickets` (migration 009) has `category`, `status`, and a bare, **unconstrained**
`assigned_to INTEGER` — the migration's own comment promises "FK to
admin_users.id added in future migration"; that migration was never written,
and `admin_users` never existed. The real staff table (`staff_users`,
migration 021) postdates the ticket table by twelve migrations. No priority,
no due date, no breach tracking, no index beyond the primary key — a ticket
created a week ago and one created a minute ago are indistinguishable in a
list query without reading every row's `created_at`. Three separate call
sites write to this table today (`internal/api/tickets.go`,
`internal/staffui/screens.go`, `internal/portal` for subscriber-raised
tickets) and none of them sets anything SLA-related, because there is
nothing to set.

### Design decisions, and why

**Priority is category-derived by default, staff-overridable, subscriber
never sets it directly.** A subscriber choosing "critical" for every ticket
is not a hypothetical — it is the default outcome of letting the reporter
set their own urgency. `category` already carries a real urgency signal
(`connectivity` — the subscriber has no service — is categorically more
urgent than `plan_change`), so priority defaults from a category → priority
table staff can retune without a deploy, and only staff (console/API) can
override it after triage. Portal-created tickets always get the default.

**SLA has two clocks, not one: response and resolution.** "Time until
someone even looks at this" and "time until this is actually resolved" are
different operational signals — a ticket sitting at `open` past its
response SLA means nobody has started; one past its resolution SLA but
already `in_progress` is a different problem. `FR-SUP-001`'s "a computed
SLA due-by timestamp" becomes two: `sla_response_due_at` (breached if still
`open` when it passes) and `sla_resolution_due_at` (breached if not
`resolved`/`closed` when it passes).

**SLA targets live in a table (`sla_policies`), not Go constants.** Same
reasoning as `plan_nas_profiles` in §4.11: an ops team retuning "how fast
must a critical connectivity ticket be resolved" is a data change, not a
code change. Keyed on `(category, priority)`, not priority alone — a
critical billing dispute and a critical connectivity outage plausibly
deserve different resolution windows even at the same priority label.

**Due-by timestamps are a snapshot at creation, not a live recomputation.**
Both are computed once, from the ticket's `created_at`, at insert time (and
recomputed, still anchored to the *original* `created_at`, if staff change
priority during triage). Anchoring to a floating "last touched" time instead
would let repeated updates push a deadline out indefinitely — the same
"snapshot, not live" reasoning already applied to CoA/PoD's NAS-session
lookup happening at task-execution time rather than trusting a stale
enqueue-time payload (MDS §4.2), just in the opposite direction: there, the
snapshot is deliberately *not* trusted; here, it deliberately *is*, because
letting it drift is the bug.

**Breach and warning events are a log, not columns.** Four extra booleans/
timestamps on the hot `tickets` table (`response_warned_at`,
`response_breached_at`, `resolution_warned_at`, `resolution_breached_at`)
would work, but `sla_events` (ticket_id, event_type, occurred_at,
`UNIQUE(ticket_id, event_type)`) is the same append-only-log shape this
codebase already uses for `notification_log` and `lea_audit_log`, and the
uniqueness constraint *is* the idempotency mechanism the scanner needs
(insert, check whether a row was actually written, only alert if so) rather
than a hand-rolled "have I already warned about this" check.

**Warning threshold: 80% of the window elapsed** — reusing FR-FUP-004's
already-shipped 80%-warning pattern for FUP quota exactly, not inventing a
second convention for the same shape of problem.

**Routing targets a role, not a specific staff member.** Auto-assigning to
an individual needs a workload/availability model this codebase has no data
for (nothing tracks how many open tickets a given `staff_users` row already
has). `ticket_routing_rules` (`category` nullable, `franchise_id` nullable,
`target_role`, `priority_order`) resolves to a role at creation time,
stored on the ticket (`routed_role`) — a human still picks the specific
assignee from that role's queue. Rule matching is explicit-precedence, not
inferred specificity: lowest `priority_order` among rules whose nullable
columns match (or are null, meaning "any") wins; no match leaves
`routed_role` null and the ticket in the general queue.

### Write-path integration

`internal/db/tickets.go`'s `CreateTicketAdmin` (and the portal's
`PortalStore.CreateTicket`, `internal/db/subscribers.go`) both gain the same
three-step sequence before insert:

```sql
-- 1. Resolve priority (skip if caller supplied an explicit override — staff only)
--    Category → default priority is itself a small lookup table
--    (category_priority_defaults) rather than a Go switch, for the same
--    retune-without-deploy reason as sla_policies.
SELECT default_priority FROM category_priority_defaults WHERE category = $1;

-- 2. Resolve SLA targets for (category, resolved priority)
SELECT response_minutes, resolution_minutes FROM sla_policies
WHERE category = $1 AND priority = $2;

-- 3. Resolve routing (first match by priority_order; franchise_id comes from
--    a join to subscribers, since tickets does not carry it independently
--    except as the denormalized copy this module adds — see DBD)
SELECT target_role FROM ticket_routing_rules
WHERE (category = $1 OR category IS NULL)
  AND (franchise_id = $2 OR franchise_id IS NULL)
ORDER BY priority_order ASC LIMIT 1;
```

Then a single `INSERT` carries `priority`, `sla_response_due_at =
created_at + response_minutes`, `sla_resolution_due_at = created_at +
resolution_minutes`, `franchise_id`, and `routed_role`. A category/priority
pair with no `sla_policies` row is a configuration gap, not a silent
no-op: the insert should fail loudly (`NOT NULL` on the resolution columns
with no `DEFAULT`) rather than create a ticket with no SLA at all — the same
"never fail silently" stance FR-NAS's `nas_attribute_build_errors_total`
already takes (MDS §4.11) for a materially identical failure shape (a
lookup with no matching row).

`UpdateTicketAdmin`'s priority-change path recomputes step 2 and rewrites
both due-at columns from the ticket's existing `created_at` — never from
`now()`.

### SLA breach scanner

`internal/tickets/sla_scanner.go` (new file, existing package — the one
`notify_task.go` already lives in). Same shape as `fup.Scanner` and
`billing.DunningScanner` (MDS §4.2, §4.3): a ticker-driven loop registered
in `cmd/radiusd/main.go` alongside them, not a new pattern.

```go
type SLAScanner struct {
    db     SLAQuerier
    client *asynq.Client
}

func (s *SLAScanner) Run(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute) // SLA windows are hours, not seconds — FUP's 10s cadence is the wrong reference point here; billing's hourly scan is the closer analog, halved for tighter breach detection
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.scan(ctx)
        }
    }
}
```

Each tick, for both response and resolution clocks independently: find open
tickets past the 80%-warning threshold or past the due-at itself, attempt
`INSERT INTO sla_events (ticket_id, event_type) VALUES (...) ON CONFLICT
(ticket_id, event_type) DO NOTHING`, and only enqueue an alert task when
that insert actually added a row (`RowsAffected() == 1`) — the same
insert-and-check-rows-affected idempotency shape already used for dead-letter
alerting (`internal/fup/deadletter_monitor.go`).

### Alerting: dashboard first, reusing the existing Alerter — not a new channel

FR-SUP-002 asks for "dashboard + notification." The dashboard half is new
UI work (a breach count/badge on staffui's Support section, `internal/
staffui/screens.go`) — out of scope for this schema/module pass, tracked
for the implementation pass. The notification half deliberately does **not**
introduce a new staff notification channel: `staff_users` has no email or
phone column today, and building one now would be scope creep into
FR-NOTIF-012 (email channel, Phase 4, not yet implemented) or a staff-facing
SMS/WhatsApp channel that doesn't exist either. Instead, a breach event
enqueues through the same `Alerter` interface (`Trigger(event string, detail
any)`) `fup.DeadLetterMonitor` already depends on and `cmd/radiusd/main.go`
already wires to `logAlerter{}` (PagerDuty-shaped, log-only until PagerDuty
delivery is actually implemented — an existing, already-documented
limitation, not a new one this module introduces). A real per-staff
notification is a real future need; it is not manufactured here just to
check a box.

### Key Metrics

- `sla_events_total` (counter, labels: `event_type`) — mirrors
  `fup_breach_detected_total`'s shape (MDS §4.2)
- `sla_scan_duration_seconds` (histogram) — the scan touches every open
  ticket every 5 minutes; worth watching as ticket volume grows
- `tickets_unrouted_total` (gauge) — tickets with no matching
  `ticket_routing_rules` row, i.e. sitting in the general queue with no
  role signal at all

## 4.14 Module 14: Billing Lifecycle — Auto-Renewal, Adjustments, Refunds & Subscriber Lifecycle *(new — gap BO-007, "subscriber lifecycle management, invoice generation, recurring billing, and payment adjustments")*

**Module ID:** MOD-BILLC | **SAD Ref:** SAD-COMP-004 (extends MOD-BIL) | **FR:** FR-BIL-008..011, FR-LC-001..003

### What exists today, and the gaps

Three pieces of infrastructure this module depends on were already built and
tested in isolation, each with a caller missing exactly the way
`DunningScanner` was missing before MDS §4.13:

1. `billing.CalculateGstInvoice` and `db.BillingStore.CreateInvoice` compute
   and persist a correct GST invoice — but nothing in the renewal path calls
   them. A subscriber can renew via the portal (`portal.RenewalProcessor`)
   and receive service, with no invoice ever created for that charge.
2. `billing.WalletService.Recharge` posts a double-entry credit — but the
   only way a subscriber's wallet balance is ever consumed is a direct,
   side-effect-free `SET wallet_balance` that does not exist yet either: no
   code path debits the wallet for a plan renewal. Renewal today is entirely
   "top up the wallet, then separately push `plan_expiry` out" — never "pay
   for the plan out of what's already there."
3. `billing.NextDunningState` is, by design, a pure function of `plan_expiry`
   vs. `now` (MDS §4.13's design note), with no awareness of
   `wallet_balance`. That decoupling is correct for what dunning does — but
   it means a subscriber who tops up their wallet with more than enough to
   cover their next cycle, then does nothing else, is suspended on schedule
   anyway. Nothing converts "has the money" into "renewed."

Separately, `UpdateSubscriber` (`internal/db/subscribers.go:167`) —
the handler behind `PATCH /api/v1/subscribers/{id}` — accepts a bare
`plan_id` change and applies it as a single `SET` with zero side effects: no
proration, no Redis auth-cache invalidation, no CoA. This is the exact gap
SRS FR-AAA-007 already specifies ("A plan change or top-up must invalidate
the subscriber's Redis auth-cache entry and enqueue a CoA... so the new rate
limit applies without waiting for reauthentication") but which no code path
actually implements. This module closes it via a dedicated endpoint rather
than by changing `PATCH`'s existing contract (see below).

### Scope note: not Razorpay auto-charge

The CRD is explicit that v1 is prepaid-only; there is no stored payment
instrument and no Razorpay auto-debit mandate anywhere in this codebase.
"Recurring billing" here means **auto-renewal from an existing wallet
balance** — if a subscriber has already topped up enough to cover their next
cycle, the system renews them automatically from that balance rather than
making them take a manual action, or worse, suspending them while their
money sits uncharged in the wallet. This is both a real reading of what
"recurring billing" can mean in a prepaid-wallet architecture, and a genuine
correctness gap (item 3 above) that predates this module.

### Design decisions, and why

**Invoice creation is folded into every path that extends `plan_expiry`
through a wallet debit — portal renewal and the new auto-renewal scanner
— not into `WalletService.Recharge` itself.** `Recharge` is also used for
plain wallet top-ups that are not, by themselves, a renewal (a subscriber
topping up ahead of when they intend to use it). Invoicing at the recharge
boundary would create an invoice for money that has not yet paid for
anything. Invoicing at the point `plan_expiry` actually advances ties the
invoice to the thing it is supposed to represent: one cycle of service.

**A new `WalletService.Post` method is added alongside `Recharge`, not
inside it.** `Recharge` is FR-BIL-003/005's tested, idempotent, always-credit
entry point and is left untouched. `Post` is the general primitive
auto-renewal debits, staff adjustments, and refunds all need: an arbitrary
direction (credit or debit) against `AccountSubscriberWallet`, with a
caller-supplied counter account. It reuses the existing
`WalletQuerier.RecordRecharge` DB primitive unchanged — that method already
takes an arbitrary debit/credit leg pair and writes them atomically with the
new balance; nothing about it is actually recharge-specific except the name.

**Two new ledger accounts, not a parallel taxonomy.** `wallet_ledgers.account`
gains `revenue_clearing` (the counter-leg for a plan charge consumed from the
wallet — auto-renewal or, in a future pass, staff-recorded cash renewal) and
`adjustment_clearing` (the counter-leg for staff-issued credits, debits, and
refunds). `(account, entry_type, description)` already disambiguates every
posting; a separate `reason`/`source` enum column would duplicate that.

**A DB-level `CHECK (wallet_balance >= 0)` backstops the application-level
balance check.** `Post`'s debit path reads the balance, computes the new
one, and rejects the request with `ErrInsufficientBalance` before writing —
but that read-then-write is not itself atomic against a second concurrent
debit on the same subscriber (auto-renewal scanner and a staff adjustment
racing, for instance). The application check is the normal path (a clean
422); the CHECK constraint is what makes an overdraft actually impossible
under a race, at the cost of that rare case surfacing as a 500 instead of a
422. Both are cheap and neither replaces the other.

**Auto-renewal restores `dunning_state` by extending `plan_expiry`, not by
writing `dunning_state` directly.** Same reasoning as MDS §4.13: one
scanner (`DunningScanner`) owns every transition of that column. Extending
`plan_expiry` is enough — `NextDunningState` already computes `active` for a
subscriber whose expiry has been pushed back into the future, so the very
next `DunningScanner.Scan` walks them home.

**Fixing `dunning.go` while verifying that path: `remind_7d`/`remind_3d`/
`remind_1d` had no restore edge to `active` in `validTransitions`.**
`NextDunningState` computes `active` as the correct target for a `remind_*`
subscriber whose `plan_expiry` moved back out past 7 days (the switch
statement's fall-through path, not its explicit restore branch, which only
lists `grace_period`/`soft_suspended`/`hard_suspended`). `stepToward`
special-cases any restore as a single hop straight to `active` regardless of
current state, and `TransitionDunning` then rejects that hop for a `remind_*`
subscriber because the edge was never listed — `advance` logs the error and
the subscriber's `dunning_state` sticks at `remind_7d` forever. `status`
stays `active` throughout (`dunningToSubscriberStatus` maps all three remind
states to `active`), so this was invisible to any user-facing check — it
only ever showed up as a stuck cosmetic value and a harmless recurring log
error. It predates this module, but the new auto-renewal restore path
exercises exactly this edge, so it is fixed here rather than left for a
scanner that would otherwise error every single hour for any subscriber who
renews while still in a reminder stage. Added: `remind_7d → active`,
`remind_3d → active`, `remind_1d → active`.

**Plan change is a dedicated endpoint (`POST
/subscribers/{id}/plan-change`), not an extension of `PATCH
/subscribers/{id}`.** The existing `PATCH` is already used for plain
corrections (fixing a misrecorded `plan_id`, flipping `status` outside the
dunning lifecycle) with call sites and tests that expect zero side effects.
Overloading it to sometimes prorate, invalidate cache and fire a CoA — based
on which field changed — makes its behavior conditional on intent that isn't
visible in the request. A new endpoint makes "this changes what the
subscriber owes and their live session" an explicit, auditable action
instead of an inferred one, and closes FR-AAA-007 without touching `PATCH`'s
existing contract.

**Proration formula.** On a plan change from old→new with `now`:

```
remaining_days   = max(0, plan_expiry - now) in days
old_daily_value  = old_plan.price / old_plan.validity_days
credit           = remaining_days * old_daily_value      // unused value of the old plan
new_daily_value  = new_plan.price / new_plan.validity_days
bonus_days       = floor(credit / new_daily_value)
new_plan_expiry  = now + new_plan.validity_days + bonus_days
```

The subscriber always gets the new plan's full validity from the moment of
the change; unused value from the old plan is converted to bonus days on the
new plan rather than a separate cash refund, since a staff-initiated plan
change is not a payment event and there is no amount collected to refund
against. A downgrade with no remaining old-plan value simply grants the new
plan's own validity with zero bonus days.

**Termination is a dedicated endpoint, not a `status` value reachable
through `PATCH`.** `terminated` was already a legal value in the
`subscribers.status` CHECK (migration 003) with no code path that ever wrote
it and no PoD triggered when it did. Termination is irreversible and
disconnects a live session (PoD, not CoA — the subscriber is not getting a
new rate limit, they are leaving), which is different enough in
consequence from every other status transition that it gets its own
audited action rather than being one more value `PATCH` happens to accept.

**Refunds are tracked in their own table (`payment_refunds`), separate from
the `wallet_ledgers` posting that moves the money.** A refund is both a
ledger event (money left the wallet) and a business event with its own
lifecycle a wallet posting has no room to express — this deployment applies
it synchronously (no live Razorpay refund API integration exists), so every
refund is created with `status = 'processed'` immediately, but the column
exists so a future asynchronous gateway refund can move through
`requested → processed/failed` without a schema change. `payment_refunds`
carries a `ledger_entry_id` FK to the `wallet_ledgers` row it corresponds to,
so the two are always traceable to each other.

### Write-path integration

```
Portal renewal (existing) ─┐
                            ├─→ WalletService.{Recharge,Post} debits/credits
Auto-renewal scanner (new)─┘        │
                                     ▼
                          plan_expiry advances (renewal only)
                                     │
                                     ▼
                    CalculateGstInvoice → BillingStore.CreateInvoice
                                     │
                                     ▼
                     next DunningScanner.Scan restores dunning_state
```

```
Staff plan-change  → proration → SetSubscriberPlan(plan_id, plan_expiry)
                                → cache.InvalidateSubscriber(username)
                                → enqueue CoA (if an active session exists)

Staff terminate    → status = terminated
                                → enqueue PoD (if an active session exists)

Staff adjustment   → WalletService.Post (credit or debit, adjustment_clearing)
Staff refund       → WalletService.Post (debit, adjustment_clearing) + payment_refunds row
```

### RecurringBillingScanner

Mirrors `DunningScanner`'s shape exactly (`Run`/`Scan`, injectable clock,
Prometheus counters, per-item error logging that does not halt the batch)
and is wired to run on a *shorter* interval (15 minutes vs. dunning's hourly)
so it always gets a chance to renew a funded subscriber before the hourly
dunning tick would otherwise escalate them — though because `NextDunningState`
walks back to `active` from any suspended state once `plan_expiry` moves to
the future, a renewal that lands after an escalation self-heals on dunning's
next tick regardless of ordering.

Candidate query: `status != 'terminated' AND plan_expiry <= NOW() AND
wallet_balance >= plans.price` (joined on the subscriber's current plan).
Only subscribers who have *already* reached their expiry are renewed —
this is reactive top-up-triggered renewal, not early renewal, matching
`portal.Renew`'s existing "renew when it's actually due" behavior.

For each candidate: debit the plan price via `WalletService.Post`
(`revenue_clearing` counter-leg), extend `plan_expiry` by the same
`max(now, currentExpiry) + validity_days` rule `extendPlanExpiry` already
uses for portal renewal, create the invoice, and — since a subscriber can be
caught by this scanner while already `grace_period`/`soft_suspended`/
`hard_suspended` (e.g. the first run after deployment, for subscribers who
lapsed before auto-renewal existed) — call `TransitionDunning(..., active)`
directly rather than waiting up to an hour for the dunning scanner to notice.

### Key Metrics

- `billing_autorenewal_total` (counter, labels: `result` = renewed/
  insufficient_balance/error) — insufficient_balance is expected volume
  (most candidates the query considers), not a failure signal
- `billing_autorenewal_invoice_failures_total` (counter) — the wallet debit
  already committed by the time invoicing runs; a failure here needs
  reconciliation, not a retried debit
- `billing_lifecycle_actions_total` (counter, labels: `action` =
  plan_change/terminate/adjustment/refund) — staff-lifecycle action volume,
  the same shape as `billing_dunning_transitions_total`

## 4.15 Module 15: Task & Approval Workflows *(new — gap BO-007, FR-WFL-001..002)*

**Module ID:** MOD-WFL | **SAD Ref:** SAD-COMP-004 (extends MOD-BILLC) | **FR:** FR-WFL-001..002

### What this closes

CRD-EXP-002 asks for "second-approver sign-off" before a sensitive account
action takes effect — named examples: large wallet credits, plan downgrades,
termination. MDS §4.14 built exactly the three highest-stakes actions this
would gate (staff wallet credit, refund, termination) as immediate,
single-operator actions with no second party in the loop: a single
`billing_admin` token can move money or end an account unilaterally, with
only an audit-log entry after the fact. This module inserts a second,
distinct approver *before* the action executes, not just a record of it
having happened.

### Scope: which actions are gated, and why not more

Gated: **wallet credit adjustments**, **refunds**, and **termination** — the
three the task explicitly names. Debit adjustments are left ungated:
a debit only ever *reduces* what a subscriber can spend and is typically
itself a correction of an earlier erroneous credit, so gating it would add
friction to the safer direction of the same feature while leaving the
risky direction (crediting money, or ending service) exactly as gated as
before. Plan-change is left ungated too, even though CRD-EXP-002 mentions
"plan downgrades": FR-LC-001 (MDS §4.14) already computes the new
`plan_expiry` deterministically from both plans' price and validity — there
is no free-form amount an operator chooses, which is the specific risk a
second approver exists to catch. Extending the gate to plan-change is a
reasonable future step but not one this pass forces.

### Design decisions, and why

**A request-then-decide model, not a hold-and-notify one.** The three gated
endpoints (`POST /subscribers/{id}/adjustments` with `direction=credit`,
`/refunds`, `/terminate`) now create an `approval_requests` row and return
`202 Accepted` instead of performing the action. Nothing happens to the
wallet or the subscriber's status until a second, different staff member
calls `POST /approvals/{id}/approve`. This is the only way to honor "before
taking effect" literally — a design that executed first and asked for
sign-off after would be an audit trail, not an approval gate.

**The self-approval guarantee is enforced twice, not once.** The API handler
checks `decider != requested_by` before attempting anything; the schema
carries the identical rule as `CONSTRAINT chk_approval_distinct_approver`.
Neither is redundant: the app check produces a clean 403 for the normal
case, while the constraint is what makes self-approval structurally
impossible even from a future code path (a script, a different handler, a
bug) that forgets to check.

**Claiming a request is a single atomic conditional UPDATE, because two
approvers can race.** `ClaimApprovalRequest` runs `UPDATE approval_requests
SET status='approved', decided_by_username=$actor ... WHERE id=$1 AND
status='pending'`. Only one of two concurrent `/approve` calls on the same
request can match `status='pending'`; the loser sees zero rows affected and
is told the request was already decided, rather than both callers going on
to execute the underlying wallet debit or credit twice. Reject uses the
identical atomic claim, straight to `rejected`, for the same reason —
a reject racing an approve must not let both happen.

**`approved` is a persisted, not transient, intermediate status.** Between
the claim and the underlying action actually executing (`FinalizeApprovalExecution`
writing `executed` or `execution_failed`), a request can be observed sitting
at `approved`. This is deliberate: if the process crashes in that window,
the request is left in an honest, inspectable state — "someone approved
this and execution did not finish" — rather than either silently retried
(risking a double execution) or silently lost (the approver's decision
disappearing). Recovering a stuck `approved` row is an operational action,
not something this module automates, matching FR-BIL-009's "log for
reconciliation, do not auto-retry a money movement" precedent.

**Money-moving execution reuses `billing.WalletService.Post` and the
existing refund/lifecycle stores unchanged.** The approval flow is purely
what decides *whether and when* the action runs; it is not a parallel
implementation of what the action does. `wallet_ledgers.adjusted_by_username`
is set to the *requester*, with the approver's identity folded into the
ledger description (`"... (approved by X)"`) — the ledger attributes the
transaction to whoever's judgment call it fundamentally was, while the
`approval_requests` row itself is the complete, queryable record of both
parties for any dispute.

**Field-task assignment (FR-WFL-002) is a separate, much simpler table with
no approval gate of its own.** CRD's own wording — "independent of the
ticket system" — is the whole design brief: `field_tasks` is a flat
assign/track/complete record (`open → in_progress → completed/cancelled`)
with no SLA engine, no routing rules, and no relationship to
`approval_requests` beyond living in the same migration. Building it as an
extension of `tickets` would couple two features (subscriber-facing
support, and internal staff coordination) that the CRD is explicit about
keeping apart.

**Both new tables use free-form `*_username` columns, not `staff_users.id`
foreign keys.** Every JWT already carries the acting staff member's username
in `Subject` (`middleware.SubjectFromContext`) with no numeric staff id
anywhere in the claims. Resolving that to `staff_users.id` on every gated
call would be new lookup machinery this module does not otherwise need —
the same call MDS §4.14 already made for `wallet_ledgers.adjusted_by_username`
and `payment_refunds.refunded_by_username`, extended here for consistency.

### Write-path integration

```
POST /subscribers/{id}/adjustments (credit)  ─┐
POST /subscribers/{id}/refunds                ├─→ approval_requests (status=pending) ─→ 202
POST /subscribers/{id}/terminate              ─┘

POST /approvals/{id}/approve → ClaimApprovalRequest (atomic, self-approval blocked)
                              → executeApprovedAction (WalletService.Post / TerminateSubscriber)
                              → FinalizeApprovalExecution (executed | execution_failed)

POST /approvals/{id}/reject  → RejectApprovalRequest (atomic) — never executes
```

### Key Metrics

- `billing_lifecycle_actions_total` (MDS §4.14) gains two new label values
  per gated action — `*_requested` at creation and `*_approved` at
  execution — so the funnel from request to execution is visible without a
  new metric family.
- `workflow_approval_execution_failures_total` (counter, labels:
  `action_type`) — an approval that executed the underlying action and had
  it fail (e.g. balance moved between request and approval) is the one case
  where an operator must look, not just reconcile later.

## 4.16 Module 16: CRM Lead Pipeline & CPE Inventory *(new — gap CRD-EXP-002, FR-CRM-001..003, FR-INV-001..003)*

**Module ID:** MOD-CRM, MOD-INV | **SAD Ref:** SAD-COMP-004 | **FR:** FR-CRM-001..003, FR-INV-001..003

### Why these two ship together

They are separate domains — a sales pipeline and a hardware warehouse — and
they get separate packages (`internal/crm`, `internal/inventory`) and
separate stores. What couples them is one moment: a lead converting into a
paying subscriber is the same moment a CPE leaves the shelf and goes to that
subscriber's flat. FR-CRM-002 ("converting a lead must carry over prospect
data") and FR-INV-002 ("issuing a CPE during onboarding must link the serial
to the subscriber") are two halves of one transaction boundary, and
designing them a phase apart would mean designing that boundary twice.

### The conversion handoff, and the race it has to survive

`CreateSubscriber` today validates, bcrypts the password, inserts the
subscriber, then encrypts and stores KYC — with KYC deliberately best-effort
(a failure is logged, the subscriber still exists). Conversion cannot simply
call it and then mark the lead, because that sequence has a failure mode
subscriber creation alone does not: **two staff converting the same lead
concurrently produces two real, billable subscribers from one prospect**, and
FR-CRM-003's conversion rate then counts that lead twice.

So conversion claims the lead first, with the same atomic conditional UPDATE
MDS §4.15 uses for approval decisions:

```sql
UPDATE leads SET status='converted', converted_subscriber_id=...
 WHERE id=$1 AND status <> 'converted'
```

Only one caller matches a not-yet-converted row; the loser is told the lead
is already converted rather than going on to create a second subscriber. The
claim and the subscriber insert run in **one transaction** (both stores share
the pool, so `inTx` spans them), which is stricter than the existing
subscriber/KYC pairing — and deliberately so: a half-finished KYC record is
recoverable by re-submitting, whereas a duplicate subscriber has already been
issued a username, a CAF number and a bill.

**A dedicated `POST /leads/{id}/convert` rather than an optional `lead_id` on
`POST /subscribers`.** The endpoint carries over the lead's name, mobile and
email and asks only for what a subscriber needs and a lead does not
(username, password, caf_number, plan_id, registered_state). Threading a
`lead_id` through the existing endpoint instead would leave the carry-over to
the caller — who could pass a mobile number that disagrees with the lead's,
quietly breaking the provenance FR-CRM-002 exists to record — and would make
"did this create a conversion?" invisible in the response.

**The shared work is extracted, not duplicated.** Validation, bcrypt and KYC
encryption move into one internal helper both `CreateSubscriber` and
`ConvertLead` call. Two copies of the password-hashing and PII-encryption
path is exactly the kind of drift that ends with one of them quietly missing
an encryption step.

### CPE issuance: optional at conversion, and never fatal to it

Conversion accepts an optional `cpe_serial`. When present, the device is
claimed with the same conditional-update pattern (`WHERE status='in_stock'`),
which is what stops one physical router from being issued to two subscribers.

Crucially, a CPE failure does **not** roll back the subscriber. Once the
conversion transaction commits, that person is a customer who can be billed
and can authenticate; refusing to create them because the warehouse was out
of stock would be the tail wagging the dog. An issuance failure is reported
in the response and logged, leaving the device to be issued later through
`POST /cpe/{serial}/issue` — the same endpoint used when no serial was named
at conversion at all.

### Low-stock alerting is event-driven, not another scanner

This codebase has four polling scanners (FUP, dunning, SLA, auto-renewal),
and a fifth would be the obvious-looking choice. It would also be wrong here:
stock levels change *only* when a device is issued or a purchase is recorded,
both of which are already code paths we control. Checking the threshold at
those two points is exact and immediate, where a 15-minute poll would be
strictly later and mostly redundant work. `GET /cpe/low-stock` serves the
dashboard view of the same computation.

### Assignment history: current state only, and what that costs

`cpe_devices` carries the current `subscriber_id`, cleared on return. There
is no per-assignment ledger, so "every device this subscriber has ever held"
is not answerable from the schema — only "what they hold now," plus whatever
the audit log recorded at issuance time. FR-INV-002 asks to link a serial to
a subscriber, which this satisfies; a full `cpe_assignments` history table is
a reasonable later addition and is called out here so its absence is a
recorded decision rather than an oversight.

### Termination opens a recovery task rather than silently restocking

When an approved termination executes (MDS §4.15), any CPE still issued to
that subscriber gets a `field_tasks` row assigning device recovery to the
technician queue — reusing FR-WFL-002 rather than inventing a second
work-tracking mechanism. The device deliberately stays `issued` until someone
physically confirms it is back: auto-flipping it to `returned` would make the
stock count claim hardware is on the shelf while it is still in a former
customer's flat, and that number is what FR-INV-003's reorder alerts are
computed from.

### Key Metrics

- `crm_leads_total` (counter, labels: `status`) — pipeline movement; the
  funnel in FR-CRM-003 is computed from the table, this tracks the flow
- `crm_lead_conversion_conflicts_total` (counter) — concurrent conversions
  refused by the atomic claim. Expected to be near-zero; a rising value means
  two operators are working the same leads
- `inventory_cpe_issued_total` / `inventory_low_stock_types` (counter /
  gauge) — issuance volume, and how many device types are currently under
  their reorder threshold

## 4.17 Module 17: Notification Channel Completion & Announcements *(new — gap CRD-EXP-003, CRD-EXP-002; FR-NOTIF-012..013, FR-ANN-001..002)*

**Module ID:** MOD-NOTIF (extends §4.7) | **FR:** FR-NOTIF-012, FR-NOTIF-013, FR-ANN-001..002

### What was actually missing

MDS §4.7 describes a four-channel notification service. `Dispatcher.Dispatch`
implements two: its `switch task.Channel` has a `whatsapp` case, an `sms`
case, and a `default` that returns "unsupported channel". `NotificationTask.Channel`
has carried the comment `whatsapp | sms | email` since v2, and
`notification_log.channel`'s CHECK has allowed `email` since migration 008 —
so an email notification could be *logged* but never *sent*. Push was not
represented anywhere.

This module closes both channels and then uses all four for the thing they
were always for: a staff-composed broadcast (FR-ANN-001).

### Design decisions, and why

**Both new channels degrade rather than fail startup.** SMTP and OneSignal
credentials are optional config, exactly like Gotenberg and Razorpay: an
unconfigured channel returns a clear error from `Dispatch` and the process
still serves every other channel. A deployment that has not bought a push
provider yet should not lose dunning SMS.

**Destination resolution moves into the dispatcher, per channel.** WhatsApp
and SMS both address a phone; email needs `subscribers.email`, and push needs
a device token that a subscriber may have several of (one per phone/tablet)
or none of. Rather than widening `NotificationTask` with an
address-per-channel, `Dispatch` resolves the destination from the subscriber
record for whichever channel it is routing to — the caller keeps saying
"notify subscriber 42", which is the only thing every call site actually
knows.

**A missing destination is a logged failure, not an error.** A subscriber
with no email address, or no registered push token, is a normal state — most
subscribers will never install the app. Returning an error would send the
task into retry-and-dead-letter for a condition retrying cannot fix,
so these are written to `notification_log` as `failed` with a reason and
return nil. This is the same judgment `PoDHandler` makes with
`asynq.SkipRetry` for a subscriber with no live session.

**Push tokens are a table, not a column.** `subscriber_push_tokens` carries
`(subscriber_id, token, platform)` with the token unique — one subscriber can
register several devices, and the same physical device re-registering must
update rather than duplicate. This is also the storage FR-MOB-001 needs when
the mobile app lands, so it is built once here rather than twice.

### Announcements: a segment query, then the existing fan-out

**An announcement is not a new delivery mechanism.** FR-ANN-002 asks that it
reuse the DND and `notification_log` machinery, and the cleanest reading of
that is that the announcement layer's only jobs are (a) deciding *who*, and
(b) enqueuing one ordinary notification task per recipient per channel.
Everything after that — suppression, sending, logging, delivery callbacks —
is the path that already exists and is already tested.

**Segments are three optional filters, not a query language.** CRD-EXP-002
names franchise, plan and area. `franchise_id` and `plan_id` are columns;
"area" has no representation in this schema at all (there is no address or
region on `subscribers`), so it is deliberately **not** implemented rather
than approximated by something that would look like area targeting and
quietly not be. `status` is offered instead, since "tell every suspended
subscriber about the payment portal outage" is the operationally common case.
The absent area filter is recorded here so it reads as a known gap.

**Marketing class by default, which is what makes DND meaningful.**
Announcements are created as `marketing` unless explicitly marked
`transactional`, so `Dispatcher`'s existing check (`sub.DndOptOut &&
task.Class == "marketing"`) suppresses them for opted-out subscribers and
records `suppressed_dnd`. A network-maintenance notice can be marked
transactional deliberately — which is a decision a human makes and the audit
log records, not a default that quietly overrides everyone's opt-out.

**The portal banner is not a dispatched channel.** `notification_log.channel`
is constrained to actually-sent channels; a banner is *displayed*, not sent,
and has no delivery status to track. It is a boolean on the announcement plus
a portal read endpoint, so the banner cannot pollute delivery statistics with
rows that were never transmitted anywhere.

**Fan-out is bounded and recorded, not fire-and-forget.** `POST
/announcements/{id}/send` claims the announcement with the same conditional
UPDATE pattern used for approvals and lead conversion (`WHERE status='draft'`),
so a double-click cannot broadcast twice to the same segment. The recipient
count is written back on completion, which is both the operator's receipt and
the thing that makes a partially-failed send visible.

### Key Metrics

- `notifications_dispatched_total` gains `email` and `push` label values —
  the existing per-channel counter needs no new family
- `notifications_missing_destination_total` (counter, labels: `channel`) —
  subscribers who could not be reached because they have no address or token
  for that channel; the number that tells you whether a channel is worth
  buying
- `announcements_sent_total` / `announcement_recipients_total` (counters) —
  broadcast volume and reach

## 4.18 Module 18: EAP-MSCHAPv2 Authentication *(new — gap CRD-EXP-001, FR-AAA-006)*

**Module ID:** MOD-AAA (extends §4.1) | **FR:** FR-AAA-006 | **Defers:** FR-AAA-005

### Why this sat unimplemented

PAP transmits the password, so `bcrypt.CompareHashAndPassword` can verify it.
CHAP and MS-CHAPv2 transmit a *response to a challenge*, and verifying one
means recomputing it:

```
CHAP:      MD5(id ‖ plaintext_password ‖ challenge)   -> needs the PLAINTEXT
MSCHAPv2:  f(NT_hash, challenges), NT_hash = MD4(UTF-16LE(pw)) -> needs the NT HASH
```

bcrypt is one-way and can answer neither. A second credential representation
was unavoidable; the only question was which, and for whom.

### The storage decision, and what it costs

`subscribers.nt_hash` is **nullable and opt-in** (migration 029). NULL — the
default for the entire existing base — means PAP against bcrypt, exactly as
before. A row gains an NT hash only when somebody deliberately enrols it.

That choice enables MS-CHAPv2 and EAP-MSCHAPv2 but **not plain CHAP**, which
needs the plaintext. FR-AAA-005 is therefore deferred rather than
implemented: storing reversibly-encrypted passwords for the whole subscriber
base is a blast radius (keystore + DB dump = every plaintext password) not
worth one legacy protocol.

An NT hash is unsalted MD4 and is credential-equivalent for MSCHAPv2 if the
database leaks. Keeping enrolment opt-in is what bounds that exposure to the
subscribers who actually need wireless/hotspot auth.

**Enrolment cannot be backfilled.** MD4(UTF-16LE(password)) needs the
plaintext, and all we store is bcrypt — so `POST /subscribers/{id}/eap`
requires the password to be re-presented, and verifies it against bcrypt
first. Without that check the endpoint would be a way to *set* a second,
divergent credential: an operator could enrol a password of their choosing
and authenticate as the subscriber over EAP while the subscriber's own PAP
password kept working, leaving nothing visibly wrong.

### The conversation

```
Access-Request(EAP-Identity)          -> Access-Challenge(MSCHAPv2 Challenge) [challenge_issued]
Access-Request(EAP-Response/MSCHAPv2) -> Access-Challenge(MSCHAPv2 Success)   [awaiting_success_ack]
Access-Request(EAP-Response ack)      -> Access-Accept(+ vendor rate-limit VSAs)
```

RADIUS carries no connection, so the only thread tying three independent UDP
exchanges into one authentication is the **State** attribute the server
issues and the NAS echoes back. State is therefore the session key, and the
server must remember the challenge it issued — a challenge it forgets is one
it cannot verify against.

**Session state lives in Redis, not process memory**, with a 60-second TTL. A
NAS load-balancing across radiusd instances will send consecutive packets of
one conversation to different processes; an in-memory map would authenticate
only when the round trips happened to land on the same one. The short TTL is
deliberate — every entry holds a live challenge, and expiring aggressively is
the safe direction.

**Sessions are deleted on every terminal outcome.** A completed session left
behind keeps a used challenge alive until its TTL, and a challenge that
outlives its single use is exactly what replay protection exists to prevent.
Verified by removing the delete and watching a replayed success ack return a
second Access-Accept.

**The status gate and brute-force lockout apply to EAP too**, or suspension
and rate-limiting would both be bypassable by switching auth method.

### Verification

The crypto is pinned against **RFC 2759 §9.2's published test vectors**
rather than self-generated fixtures. A self-generated fixture proves the code
agrees with itself — which it would even with the DES key expansion or the
UTF-16LE encoding wrong. Only the RFC's own numbers prove it agrees with the
Windows supplicants that will authenticate against it.

Real-supplicant interop remains unproven until field-tested: the conversation
is exercised end to end against a real Redis in integration tests, but no
physical wireless controller has authenticated against it yet.

### Key Metrics

- `radius_eap_sessions_started_total` / `radius_eap_sessions_completed_total`
  (labels: `result`) — the funnel from first Identity to Accept, with each
  failure reason distinguishable (`not_enrolled` is an operational state, not
  an attack)
- `radius_eap_sessions_lost_total` — responses arriving with a State no
  longer held. A rising value means the 60s TTL is too short for the
  network's latency, which is a tuning signal rather than a security one

---

## 4.19 Module 19: TR-069 ACS — CPE Provisioning & Remote Control *(new — gap CRD-EXP-003, FR-CPE-001..003)*

**Module ID:** MOD-CPE | **FR:** FR-CPE-001..003 | **DBD:** §6.2 (`cpe_devices` extension, `cpe_tasks`) | **Migration:** 030

### Scope, and what is deliberately outside it

This is not a general-purpose ACS. It speaks six RPCs — Inform,
GetParameterValues, SetParameterValues, Reboot, Download, FactoryReset — which
is what FR-CPE-001..003 need. Anything depending on the full TR-069 data model
or on vendor-specific RPCs is out of scope and better served by integrating
GenieACS, at the cost of adding MongoDB and Node.js to a Go/Postgres/Redis
stack.

### The constraint that shapes the whole design: CWMP is CPE-initiated

TR-069 has exactly one mechanism for the ACS to start a conversation — a
**Connection Request**, an HTTP GET to a URL the device advertises. Indian
residential CPE sits behind CGNAT (this platform allocates the CGNAT ranges
itself, §4.9), so that URL routinely names an address no packet from the ACS
can reach.

**Connection Request is therefore not implemented, and the engine does not
depend on it.** Every operator action is queued and delivered inside a session
the *device* opens — a `1 BOOT` after a restart, or the routine `2 PERIODIC`
check-in. `connection_request_url` is still recorded, because it is useful for
the minority of deployments on public addressing and costs one column.

The honest consequence is surfaced rather than hidden: NOC endpoints answer
**202 Accepted**, never 200, with a `delivered_when` string naming the next
CWMP session. A technician standing next to a router that has not rebooted
should be able to read the API response and know why.

### Session state machine

```
POST /tr069  Inform(events, DeviceId, ParameterList)
   -> upsert device, record identity/firmware/last_inform_at
   -> open session (Redis, 10 min TTL, cwmp-session cookie)
   -> 200 InformResponse
POST /tr069  <empty body>          # "I have nothing more to send"
   -> claim next pending task -> 200 <RPC>       (or 204 if the queue is empty)
POST /tr069  <RPC>Response | Fault
   -> record outcome, then claim next task -> 200 <RPC>  (or 204)
```

Sessions live in Redis rather than process memory for the same reason EAP
sessions do (§4.18): more than one API replica may serve consecutive POSTs of
one session. The 10-minute TTL bounds a device that walks away mid-session.

### What triggers provisioning, and what does not

| Inform event | Action |
|---|---|
| `0 BOOTSTRAP` | provision — first contact ever, or post-factory-reset |
| `1 BOOT` | provision — a reboot may have lost configuration |
| `2 PERIODIC` | drain the queue only; **do not** re-provision |
| any, device in `needs_reprovision` or `unknown` | provision |

`2 PERIODIC` deliberately does not re-provision. A periodic Inform arrives
from every managed device every few minutes; re-pushing configuration on each
one would rewrite a subscriber's SSID on a schedule and turn a working
deployment into a broadcast storm of SetParameterValues.

`POST /cpe/devices/{serial}/reprovision` sets the *state* rather than queueing
an RPC, so the parameter set is rebuilt from the subscriber's plan when the
device next Informs — a plan change between button-press and check-in is
reflected rather than baked in.

### Provisioning templates live in the database

TR-069 parameter paths differ per model: a TP-Link's SSID is not at a Nokia's
path, and TR-098 and TR-181 devices disagree about the root element entirely.
`cpe_device_types.provisioning_template` (JSONB) holds path → value-template
pairs, so supporting a new router model is a row rather than a release.

Values substitute `{{plan_name}}`, `{{ssid}}`, `{{downstream_kbps}}`,
`{{pppoe_username}}` and friends. Shaping is derived from the plan's
`rate_limit_string` — the same value that drives the RADIUS rate limit
(FR-NAS-001), which is what keeps both ends of the link agreeing.

**A parameter whose value renders empty is dropped, not pushed.** This is a
safety property. Subscriber passwords are bcrypt, so `{{pppoe_password}}`
usually has nothing to substitute, and pushing an empty PPPoE password would
disconnect the subscriber while reporting successful provisioning. Omitting
the parameter leaves whatever the device already has — the safe failure
direction. Pushing PPPoE credentials needs a separately stored secret, the
same constraint that defers FR-AAA-005.

### Task delivery is exactly-once

`ClaimNextTask` is the atomic conditional claim used throughout this codebase
(§4.15, §4.16, §4.17): the `status = 'pending'` predicate inside the sub-select
is the guard. Under READ COMMITTED a second updater that blocks on the row
lock re-evaluates the outer WHERE when it wakes, matches nothing, and
correctly receives `(nil, nil)`.

Verified by removing that predicate: ten concurrent claimers were handed the
same reboot — a second, unexplained outage for one subscriber. Removing
`FOR UPDATE SKIP LOCKED` instead left the test passing, which is what
established that the hint is a throughput optimisation and the predicate is
the correctness mechanism.

Tasks expire after seven days and expired ones are skipped rather than
delivered. A reboot queued a fortnight ago arriving now is an unexplained
outage too.

### Unknown devices are recorded, flagged, and never counted as stock

A device Informing with no warehouse record is inserted with
`acs_discovered = TRUE` and status `returned`, and
`chk_cpe_discovered_not_in_stock` prevents it from being counted as sellable
inventory — an ACS-discovered device is physically in a subscriber's home, not
on a shelf. `tr069_unknown_device_informs_total` makes a spike visible: a
trickle is normal field swaps, a spike usually means somebody has pointed
third-party CPE at the ACS.

### Interop: declare every namespace prefix you use

The RPC builders emit `xsi:type="xsd:string"` and `soap-enc:arrayType="..."`,
and the envelope must bind `xsi`, `xsd` and `soap-enc` as well as `soap` and
`cwmp`. An unbound prefix is a **fatal** error under the XML Namespaces spec,
and the CWMP stacks inside real CPE are built on expat and libxml2, which
enforce it.

This was caught in live verification, not by the test suite: Go's
`encoding/xml` treats an unknown prefix as if the prefix itself were the
namespace URI and parses on happily, so every Go test passed against an
envelope that a namespace-aware parser rejects with "unbound prefix" — meaning
every real router would have refused to be provisioned. The regression test
now asserts the property directly rather than round-tripping through a parser
that forgives it, and live verification pipes the actual delivered envelope
through an expat-class parser.

### Verification

Beyond unit and integration suites, a live run against the demo stack drives a
real CWMP session with curl holding the session cookie: BOOTSTRAP Inform →
provisioning SetParameterValues rendered from the live plan → acknowledgement
→ 204; then a NOC-queued reboot delivered on the following PERIODIC check-in.
It asserts the safety properties as well as the happy path — that the empty
PPPoE password was dropped, that PERIODIC did not re-provision, that
`billing_admin` is refused (403) on NOC control endpoints, and that a rogue
serial is flagged rather than silently trusted.

### Known limitations

- **No real-CPE interop testing.** Verified against a simulated device and a
  strict XML parser; no physical router has been provisioned.
- **No Connection Request**, so operator actions land at the next check-in
  rather than on demand — bounded by the device's periodic interval.
- **No parameter-history ledger.** `GetParameterValues` results are recorded
  against the task; there is no time series of what a device reported.

---

## 4.20 Module 20: Lifecycle State Capture *(new — FR-RPT-001 precondition, migration 031)*

**Module ID:** MOD-RPT (extends §4.8) | **FR:** FR-RPT-001 | **DBD:** §6.2
(`subscriber_status_history`, `ticket_status_history`) | **Migration:** 031

### Why this exists before any reporting view

`subscribers.status` and `tickets.status` are both overwritten in place.
`updated_at` is bumped by every unrelated edit — a wallet top-up, an FUP flag —
so it cannot date a status change. `sla_events` records only the four warning
and breach types; there is no `resolved` event.

The consequence is that **churn trends and ticket-resolution metrics are not
computable from the current schema at any level of SQL skill.** A view can
aggregate history but cannot invent it. Plan-mix and collection performance
*are* answerable from current-state columns, which is what splits FR-RPT-001
into a part that needed a migration and a part that did not.

This is why capture ships as its own migration, ahead of the views that read
it: every day without these tables is churn and resolution data destroyed as
it is produced, and unlike most gaps it cannot be backfilled later.

### Capture is by trigger, attribution is by the application

There are only four status-writing paths today, so application-level writes
would have worked. Triggers were chosen for the fifth path somebody adds next
year, and for the DBA fixing a row by hand at 2am — the cases where a missing
audit row is least likely to be noticed and most likely to matter.

What a trigger cannot see is *who* and *why*, which live in the request
context. The bridge is a transaction-local GUC:

```
WITH ctx AS (
    SELECT set_config('app.actor', $n, true)         AS actor,
           set_config('app.change_reason', $m, true) AS reason
), upd AS (
    UPDATE subscribers SET status = ...
    FROM ctx WHERE subscribers.id = $1 AND ctx.actor IS NOT NULL
    RETURNING subscribers.*
) SELECT ... FROM upd
```

Three properties of that shape are load-bearing:

- **`is_local = true`.** pgx hands the same physical connection to unrelated
  requests; a session-level setting would misattribute the next one.
- **One statement, not an explicit `BEGIN`.** The CTE keeps the config and the
  update in the same implicit transaction with no block to leak on an early
  return.
- **The CTE must be referenced.** `AND ctx.actor IS NOT NULL` is always true
  and exists only to force evaluation. This is not defensive
  over-engineering — removing the reference was verified to silently drop
  attribution to `unknown` while the update itself still succeeded.

**Forgetting to set the actor is not a failure.** The transition is still
captured, with `changed_by = 'unknown'`. Losing who made a change is
recoverable from other logs; losing the fact that it happened is not. That
asymmetry is the whole argument for the trigger.

Background workers annotate their own context — `middleware.WithSubject(ctx,
"system:dunning-scanner")` — so an automatic suspension stays distinguishable
from one a person decided on. Without it, a churn report cannot separate "our
collections process is working" from "staff are suspending customers".

### A baseline is not an event

Accounts that predate the migration get one seeded row recording where they
stand now, dated from `created_at` and flagged `is_baseline`. The reporting
views exclude those rows from every growth and churn count, and
`chk_ssh_baseline_has_no_predecessor` stops one ever being written with an
`old_status` — a baseline carrying a predecessor is a transition claiming to
be a snapshot.

Terminated subscribers and resolved/closed tickets are skipped entirely. Their
transition moment is unrecoverable, and a baseline row for one would let a
churn we cannot date masquerade as a signup we cannot date.

### Cost

`AFTER UPDATE OF status ... WHEN (OLD.status IS DISTINCT FROM NEW.status)`
means the trigger function is not called at all for the far more frequent
`wallet_balance`, `plan_expiry` and `fup_active` writes. The hot paths pay
nothing. The `WHEN` clause also makes a no-op re-save a non-event: without it,
an operator re-saving an unchanged form inflates every count — verified by
removing it and watching the test fail.

### Verification

Six integration tests, three negative controls, and a live run against the
demo stack. The live run is what found the real gap: capture had been wired
into the status *update* paths but not the *create* ones, so the first row of
every subscriber's and every ticket's history read `unknown` — the one entry
an audit of "who opened this account" most needs. Fixed across all four
creation paths, with a test that fails if it regresses.

### Known limitations

- **No history before 2026-08-15.** Reports covering earlier periods show the
  baseline only. This is deliberate and must be labelled as partial history in
  any UI that renders it.
- **`reason` is set only where a caller supplies it.** Operator edits,
  dunning, signup and lead conversion are labelled; other paths leave it NULL.

---

## 4.21 Module 21: Reporting Views *(new — FR-RPT-001, FR-RPT-003, migration 032)*

**Module ID:** MOD-RPT (extends §4.8) | **FR:** FR-RPT-001, FR-RPT-003 |
**DBD:** §6.7 | **Migration:** 032 | **Reads:** §4.20's capture tables

### Four objects, one materialised

| Object | Kind | Serves | Why this kind |
|---|---|---|---|
| `v_plan_mix` | view | FR-RPT-001 | Current-state question; needs no history and no refresh |
| `v_subscriber_growth_monthly` | view | FR-RPT-001 | Reads §4.20's capture; cheap to aggregate |
| `mv_ticket_resolution` | **materialised** | FR-RPT-001 | Computes a percentile across every ticket ever filed — not a per-page-load query |
| `v_franchise_collection` | view | FR-RPT-003 | Franchise is the reporting area (decision 2026-08-15) |

Plain views are the default. They are always current, need no refresh story,
and at this data volume cost nothing worth optimising. Materialising by habit
would have bought staleness for no gain.

### The judgement calls the SQL encodes

These are the parts a reader should be able to disagree with explicitly, and
each has a test that fails if it is quietly changed:

- **Suspension is not churn.** A hard-suspended account is a collections
  problem that usually reverses. Folding it into churn makes every dunning run
  look like a customer exodus and leaves the two numbers impossible to act on
  separately. `suspended` is reported beside `churned`, never inside it.
- **MRR counts active subscribers only.** Revenue from a suspended account is
  revenue the business is not currently collecting.
- **Resolution time is the FIRST arrival at `resolved`, not the last.** A
  ticket closed, reopened and closed again is a support failure; taking the
  last timestamp reports it as one slow success and hides the reopen. Verified
  by switching `min()` to `max()` and watching the median jump from ~2h to 10h.
- **Median, not mean.** One ticket left open over a long weekend drags an
  average far enough to hide an otherwise healthy month.
- **`LEFT JOIN` to resolutions.** An inner join drops exactly the tickets a
  resolution report exists to surface, and the numbers would improve the worse
  things actually got. Verified: the inner-join variant returned zero rows for
  three unresolved tickets.
- **A NULL median beats a zero.** Reporting 0.0 hours for a month in which
  nobody was helped claims the fastest possible support.
- **A NULL collection rate beats a zero.** A franchise that raised no invoices
  has no collection rate; 0% ranks a new territory bottom of a league table it
  has not joined. Confirmed live: a freshly created franchise shows blank, not
  0%.
- **Billed and collected come from different tables.** `invoices` records what
  was charged, `lco_ledger` what a franchise actually took in. Deriving one
  from the other would make the collection rate definitionally 100%.
- **Baseline rows are excluded.** §4.20's seeded snapshots are starting
  positions, not events; counting one as a signup invents a growth curve for a
  period with no capture.

### Two defects live verification found and the tests did not

**The unique index must be over plain columns.** `REFRESH MATERIALIZED VIEW
CONCURRENTLY` requires a unique index that is neither partial nor over an
expression. The natural formulation — `coalesce(franchise_id, -1)`, to cope
with direct subscribers having no franchise — is exactly an expression index,
and Postgres refuses the refresh outright. `NULLS NOT DISTINCT` (PostgreSQL
15+) solves what the coalesce was reaching for while keeping the index over
plain columns.

**The application cannot refresh a view it does not own.** PostgreSQL has no
REFRESH privilege; the command requires ownership. The app connects as
`bss_app` (migration 019) while migrations run as the superuser, so the worker
failed on the demo stack with `must be owner of materialized view` — while the
integration suite passed, because it connects as the superuser and never
exercised the real privilege boundary.

Making `bss_app` the owner would have fixed it and also handed the role the
right to drop the view. A `SECURITY DEFINER` function grants exactly the
refresh and nothing else, with `search_path` pinned to `pg_catalog, public`
because a SECURITY DEFINER function with a caller-controlled search_path is a
privilege-escalation vector. Verified both directions live: `bss_app` can call
`refresh_reporting_views()` and is still refused a direct `REFRESH`.

### Refresh

`reporting.RefreshScanner` (wired in `cmd/radiusd`, tracked by
`check_wiring.sh`) refreshes every 15 minutes and once at startup. A
materialised view with nothing refreshing it reports the numbers that were true
the day it was created, forever, with no outward sign — which is why the
interval is code rather than a cron entry somebody has to remember to add.

Fifteen minutes is chosen against use: monthly medians and SLA attainment do
not move minute to minute, and refreshing far more often would spend real CPU
recomputing a percentile over the whole ticket table to change a second decimal
place.

`reporting_matview_last_refresh_timestamp` is the gauge an alert should watch.
A refresh that stops happening leaves a dashboard showing confident, plausible,
wrong numbers — the failure mode with no visible symptom, so staleness has to
be measurable. It earned its keep immediately: the ownership defect above
surfaced as `refresh_failures_total 1` within seconds of the container starting.

### Known limitations

- **No history before 2026-08-15.** Growth and resolution figures cover only
  the period since §4.20's capture began, and any UI must label earlier periods
  as partial history rather than reporting zero.
- **Area is franchise, not geography.** No address, region or pincode column
  exists; deferred to the Batch 4 address work (FR-RPT-003).
- **No export or scheduling yet.** FR-RPT-002 is untouched — these views are
  what an export would read.

---

## 4.22 Module 22: Partner API & Outbound Webhooks *(new — FR-API-001..003, migration 033)*

**Module ID:** MOD-API | **FR:** FR-API-001..003 | **DBD:** §6.8 |
**Migration:** 033

### Partner credentials are structurally separate from staff tokens

FR-API-001 asks for API-key authentication "distinct from internal staff
JWTs". The way that is made true rather than aspirational is that
`APIKeyMiddleware` **never sets a role in the context**. Every `RequireRole`
check downstream therefore fails closed for a partner key, whatever route it
reaches and however that route was wired. A convention would have to be
remembered; this cannot be forgotten.

Scopes (`read:subscribers`, `write:tickets`, `manage:webhooks`, …) are a closed
set validated at key creation. A key requesting an unknown scope is refused
rather than stored, because a scope no route checks reads as working right up
until somebody depends on it.

### SHA-256, not bcrypt — and why that is not an inconsistency

Subscriber passwords are bcrypt. API keys are SHA-256. The two cases are not
alike:

| | Subscriber password | API key |
|---|---|---|
| Entropy | human-chosen, low | 192 bits of CSPRNG output |
| Threat on a stolen hash | offline dictionary attack | none feasible |
| Cost of a slow hash | paid once per login | paid on **every** partner request |

A work factor exists to make a dictionary search expensive. There is no
dictionary for a 192-bit random token, so bcrypt would add ~100ms to every API
call and buy nothing. Salting is pointless for the same reason: there are no
duplicate keys to correlate.

The key format is `pk_{env}_{prefix}_{secret}`. The prefix is a **lookup
handle, not a secret**: keys are stored hashed, so the server cannot search by
the key itself. It parses the prefix, fetches that one row and compares — one
hash per request rather than one per stored key. A key sharing only the prefix
must not verify, which is asserted directly, because a prefix leaks easily
(logs, console screenshots) in a way the secret does not.

Authentication returns the same "invalid API key" for every failure — unknown
prefix, wrong secret, revoked, expired. Telling an attacker which part of a
credential was wrong tells them what to fix.

### Thin payloads

```json
{"event_id": "...", "event_type": "ticket.created", "entity_id": 42, "occurred_at": "..."}
```

Decision 2026-08-15. Identifiers and a timestamp, never the subscriber record.
Two reasons, and the second is load-bearing:

1. A fat payload puts PII in `webhook_deliveries`, which is otherwise a pure
   audit log with no retention obligation. Thin keeps DPDP out of it entirely.
2. A payload captured at enqueue time is **stale** by the time it is delivered.
   A partner that re-reads through the API with its own key always sees current
   truth, and cannot act on a plan change that was superseded while the
   delivery was retrying.

`event_id` is the partner's idempotency key, echoed in every retry of the same
event, so a timeout that actually succeeded is recognisable rather than
double-processed.

### Signing

`X-ISP-Signature: t=<unix>,v1=<hmac-sha256 hex>`

The timestamp is **inside** the signed material (`ts . body`), not merely
alongside it. Signing the body alone would let an attacker who captured one
delivery replay it forever with a fresh timestamp header; binding the two means
a replay must reuse the original timestamp, which the freshness check rejects.
Verified by rewriting the timestamp in a valid header and asserting it no
longer verifies.

The `v1=` prefix leaves room to revise the scheme without breaking partners who
parse it — a `v2` can appear beside `v1` during a migration rather than in one
cutover.

`VerifySignature` is exported deliberately: it is the reference implementation
handed to partners in the integration docs, and the server testing itself
against the same function is what stops documentation and behaviour drifting
apart.

### SSRF: the check that matters runs at dial time

A webhook URL is supplied by a third party and fetched by our server from
inside the private network. Unconstrained, that is a server-side request
forgery primitive — `https://169.254.169.254/latest/meta-data/` asks us to read
cloud instance credentials and POST them somewhere.

The guard runs **twice**, and only the second is a security boundary:

- **At registration** — a friendly, immediate error, so an operator does not
  get an endpoint that registers cleanly and fails silently forever.
- **At dial time**, in the `net.Dialer.Control` hook — after DNS resolution and
  immediately before the connection. This is the only place a **DNS rebinding**
  attack cannot slip between the check and the use: a hostname that resolves
  publicly at registration can resolve to link-local by delivery time.

Redirects are refused (`ErrUseLastResponse`) for the same reason: a 302 to
`169.254.169.254` would otherwise walk straight past both checks.

A blocked target is **abandoned, not retried**. No amount of retrying makes a
private address public, and retrying for a day would occupy the queue with a
permanent misconfiguration.

Two things the tests corrected here:

- `::ffff:0:0/96` was in the blocklist to close the IPv4-mapped bypass. Go
  represents *every* parsed IPv4 address in 16-byte mapped form, so that CIDR
  matched all of them — the guard blocked `8.8.8.8` and every legitimate
  partner endpoint. Caught by the test asserting public addresses stay allowed.
- The explicit `To4()` normalisation written to replace it turned out to be
  dead code: `net.IPNet.Contains` already calls `To4` on its argument.
  Confirmed by removing it and observing no test change, then removed and the
  comment corrected rather than left crediting it with a protection the
  standard library provides.

### Retry and the delivery log

Asynq does the retrying; `webhook_deliveries` does the remembering. They are
different jobs — the queue knows about the next attempt, the table is what a
partner's support ticket gets answered from three weeks later, long after the
queue entry is gone.

- Fan-out is **one task per endpoint**, not per event, so a partner whose
  server is down cannot delay delivery to every other partner.
- The `webhooks` queue is separate from transactional notifications: a partner
  retrying against a dead host must never sit in front of a payment receipt.
- **4xx retries too.** A partner returning 401 because their own auth broke is
  transient from our side, and giving up immediately would lose the event.
- `abandoned` is distinct from `failed`: failed is one bad attempt, abandoned
  means retries are exhausted and nobody will try again. Collapsing them hides
  the state that needs a human.
- A unique index on `(endpoint_id, event_id)` makes a retry **update** the
  attempt trail rather than fork it. Without it a retry after a mid-write crash
  would double-log and the attempt count — the number used to spot a flapping
  partner — would be meaningless.

### Emission never breaks the core product

`Emit` logs its errors and returns nothing. Emission hangs off business
operations — creating a subscriber, resolving a ticket, generating an invoice —
and failing one of those because a third party's webhook could not be queued
would let a partner's configuration break the platform.

Six events are emitted: `subscriber.created`, `subscriber.status_changed`,
`payment.received`, `invoice.generated`, `ticket.created`, `ticket.resolved`.
Each is emitted **after** its write committed — a partner told a payment
arrived must not be reacting to one a rollback then unwound.

`subscriber.status_changed` fires only on an actual status change, not a
plan-only edit; `ticket.resolved` only on resolution, not on every intermediate
step. A webhook per intermediate state is noise a partner has to filter.

### Revocation is complete

`SubscribersFor` joins `api_keys` and requires the key to be active and
unexpired. Deactivating a key silences its webhooks too — otherwise revocation
would be only half a revocation, leaving a terminated partner still receiving
subscriber events.

### Known limitations

- **No partner-facing rate limiting.** A partner can call the read endpoints as
  fast as they like; nothing throttles them yet.
- **No webhook replay endpoint.** A partner who missed events during an outage
  can read `webhook_deliveries` but cannot ask for redelivery.
- **Secret rotation requires re-registration.** There is no endpoint to roll an
  endpoint's signing secret in place.
- **The read surface is one route** (`GET /partner/subscribers/{id}`). The
  scope vocabulary anticipates more; the routes do not exist yet.
