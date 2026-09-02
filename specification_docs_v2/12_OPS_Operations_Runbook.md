# Document 12: Operations & Incident Response Runbook
**Version:** 2.2 | **Status:** Draft | **Date:** 2026-08-30 — Redis/Asynq procedures replaced: both were removed from the system (sessions → `live_sessions`, migration 036; task queue → `jobqueue_tasks`, migration 037) but this runbook still instructed on-call to debug them. §12.3.1 repurposed from "Redis Primary Failure" to the task queue's own stall modes; §12.3.3/§12.3.4 rewritten against the real schema; §12.2.3's reference to `scripts/fup_rollback.go` replaced with a working procedure (that script does not exist). §12.3.2 (Patroni) unchanged and still valid — the Docker topology it describes is still present alongside the native Windows install.
**Document ID:** OPS
**Traces From:** [IDD](08_IDD_Infrastructure_Design.md) → [SAD](03_SAD_System_Architecture.md)
**Traces To:** —

---

## 12.1 On-Call Escalation Matrix

| Level | Role | Contact | Response SLA |
|---|---|---|---|
| L1 | NOC Engineer (on-call) | PagerDuty primary rotation | 5 minutes |
| L2 | Senior NOC / Network Lead | PagerDuty escalation | 15 minutes |
| L3 | Platform Engineering | Direct call / Slack `#oncall-eng` | 30 minutes |
| L4 | Engineering Lead | Direct call | 60 minutes (severity 1 only) |

### Incident Severity Definitions

| Severity | Definition | Example |
|---|---|---|
| S1 | Full platform outage; all subscribers disconnected | PostgreSQL primary down; AAA service down |
| S2 | Partial outage; subset of subscribers affected | NAS connectivity loss, FUP CoA failing |
| S3 | Degraded performance; no subscriber impact | High RADIUS latency; task queue depth growing |
| S4 | Minor issue; logged but no immediate action | Single webhook HMAC failure, one failed invoice PDF |

---

## 12.2 Routine Operational Procedures

### 12.2.1 Manually Disconnect a Subscriber (PoD)

Use when a subscriber must be force-disconnected (abuse, non-payment override, testing).

```bash
# Via API (preferred — audited)
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/disconnect \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}"
# Returns 202 Accepted; PoD is asynchronous

# Verify task was enqueued
psql -d isp_bss_oss -c "SELECT id, task_type, status, run_after FROM jobqueue_tasks
                         WHERE queue = 'network_commands' AND status = 'pending'
                         ORDER BY id DESC LIMIT 5;"
```

If the task fails, it retries with backoff and lands in the dead-letter state
(`status = 'dead'`) once retries are exhausted:

```bash
# Dead-letter depth for this queue
psql -d isp_bss_oss -c "SELECT count(*) FROM jobqueue_tasks
                         WHERE queue = 'network_commands' AND status = 'dead';"

# Inspect the failed tasks themselves
psql -d isp_bss_oss -c "SELECT id, task_type, retry_count, last_error, created_at
                         FROM jobqueue_tasks
                         WHERE queue = 'network_commands' AND status = 'dead'
                         ORDER BY id DESC LIMIT 20;"
```

To retry a dead task, set it back to `pending` and release any stale lease — a
worker picks it up on the next dequeue:

```bash
psql -d isp_bss_oss -c "UPDATE jobqueue_tasks
                           SET status = 'pending', retry_count = 0, run_after = now(),
                               locked_by = NULL, lease_expires_at = NULL
                         WHERE id = {TASK_ID};"
```

### 12.2.2 Manually Apply / Remove FUP Throttle

Use when FUP was incorrectly applied or needs to be manually enforced.

```bash
# Apply FUP throttle
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/fup-override \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"action": "apply"}'

# Remove FUP throttle (restore full speed)
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/fup-override \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"action": "remove"}'
```

### 12.2.3 Emergency FUP Policy Rollback Across All Live Sessions

Use when a plan configuration error has incorrectly triggered FUP throttling across a subscriber cohort.

> **No bulk rollback tool exists.** There is no single command for this — the
> procedure below is a loop over the same audited per-session endpoint
> § 12.2.2 uses. Do not clear `subscribers.fup_active` directly in SQL: that
> changes the flag without sending any CoA, so the NAS keeps enforcing the
> throttle it was last told about and the database then disagrees with the
> live network.

```bash
# Step 1: Count and list the affected live sessions
psql -d isp_bss_oss -c "SELECT count(*) FROM live_sessions WHERE fup_throttled;"

psql -d isp_bss_oss -At -c \
  "SELECT l.session_id
     FROM live_sessions l
     JOIN subscribers s ON s.id = l.subscriber_id
    WHERE l.fup_throttled AND s.plan_id = {PLAN_ID};" > /tmp/fup_sessions.txt

# Step 2: Remove the throttle per session (audited, and each enqueues its own
# restoring CoA). Paced deliberately — a few hundred at once floods the
# network_commands queue and the NAS with simultaneous CoA-Requests.
while read -r sid; do
  curl -fsS -X POST "https://api.yourdomain.com/api/v1/sessions/$sid/fup-override" \
    -H "Authorization: Bearer {NOC_JWT_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"action": "remove"}' || echo "FAILED: $sid"
  sleep 0.2
done < /tmp/fup_sessions.txt

# Step 3: Confirm the queue drains rather than backing up or dead-lettering
psql -d isp_bss_oss -c "SELECT status, count(*) FROM jobqueue_tasks
                         WHERE queue = 'network_commands' GROUP BY status;"
```

### 12.2.4 Extend Grace Period for a Subscriber

```bash
# Update plan_expiry directly via API
curl -X PATCH https://api.yourdomain.com/api/v1/subscribers/{id} \
  -H "Authorization: Bearer {BILLING_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"plan_expiry": "2025-02-15T23:59:59Z"}'
```

---

## 12.3 Infrastructure Incident Procedures

### 12.3.1 Background Task Queue Stalled

> Replaces this document's former "Redis Primary Failure" procedure. There is
> no Redis in this system: the task queue moved into PostgreSQL
> (`jobqueue_tasks`, migration 037) and live session state with it
> (`live_sessions`, migration 036). A datastore failure is now a PostgreSQL
> failure — § 12.3.2 — and this section covers the queue's own stall modes
> instead.

**Symptoms:** CoA/PoD requests accepted (202) but never take effect on the
NAS; dead-letter alert firing; `pending` depth climbing without draining.

**Response:**

1. Get the shape of the queue before changing anything:
   ```bash
   psql -d isp_bss_oss -c "SELECT queue, status, count(*) FROM jobqueue_tasks
                            GROUP BY queue, status ORDER BY queue, status;"
   ```

2. **`pending` climbing, nothing in `processing`** — no worker is consuming.
   The worker pool lives in the AAA service, so check it is actually running:
   ```powershell
   Get-Service ISPBSSAaaCore
   Get-Content "C:\Program Files\ISP BSS\logs\ISPBSSAaaCore.log" -Tail 50
   ```
   A healthy start logs `radiusd: task workers started`. If the service is
   stopped, `Start-Service ISPBSSAaaCore`.

3. **Rows stuck in `processing`** — a worker died mid-task. These recover on
   their own: the lease expires and the row returns to the pool, which is
   what `lease_expires_at` exists for. Confirm leases are actually expiring
   rather than being held by a live-but-wedged worker:
   ```bash
   psql -d isp_bss_oss -c "SELECT id, task_type, locked_by, lease_expires_at, now()
                            FROM jobqueue_tasks WHERE status = 'processing'
                            ORDER BY lease_expires_at LIMIT 20;"
   ```
   Only intervene if `lease_expires_at` is well in the past *and* the row has
   not been reclaimed — that indicates the reclaim path itself is stuck, which
   is an L3 escalation, not something to fix by hand-editing rows.

4. **Rows in `dead`** — retries were exhausted. Read `last_error` before
   retrying anything; a CoA that dead-lettered because the NAS is unreachable
   will simply dead-letter again. Retry procedure is in § 12.2.1.

**Escalate to L3 if:** the pool is running and leases are expiring, but
`pending` still does not drain — that points at the dequeue path rather than
at any individual task.

### 12.3.2 PostgreSQL Primary Failure

**Symptoms:** API 500 errors; Grafana `pg_up` = 0.

**This procedure assumes `docker-compose.pg-ha.yml` is deployed (IDD §8.2a,
NFR-AVAIL-002).** A deployment running only the base `docker-compose.yml`
has one Postgres container with no automated failover at all — restart is
the only lever, and an unrecoverable primary there means restore-from-backup
(§12.4), not promotion. Confirm which topology is actually running
(`docker compose ps | grep postgres`; three `postgres_*` containers means
the overlay is applied) before assuming automation exists.

**With the pg-ha overlay — automated path (try first):**

Patroni promotes a synchronous standby automatically, typically within
`ttl` (30s, `config/postgres/patroni.yml`) of the primary becoming
unreachable — no operator action needed for the promotion itself.

1. Confirm a promotion already happened rather than assuming one is needed:
   ```bash
   curl -s bss_postgres_standby_1:8008/primary   # 200 = this node is now primary
   curl -s bss_postgres_standby_2:8008/primary
   ```
2. If a standby has already been promoted, the applications need no DSN
   change — `DB_DSN` already lists all three hosts with
   `target_session_attrs=read-write` (IDD §8.2a), so `pgx` finds the new
   primary on its own for any *new* connection. Watch for a burst of
   `db_connection_retry_total` / `nas_...` — actually watch
   `radius_auth_duration_seconds` and API error rate — as pooled
   connections still pointed at the old primary hit SQLSTATE `25006` and
   get recycled; this should self-resolve within one pool cycle, not
   require a restart.
3. If self-resolution is not visible within a few minutes, restart the
   application containers to force a clean pool:
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml \
     up -d --force-recreate aaa_core_daemon api_service
   ```
4. Once the failed node recovers, Patroni rejoins it as a standby
   automatically (`use_pg_rewind: true`) — no manual `pg_basebackup` needed
   in the common case.

**Escalate to manual promotion if:** Patroni has not promoted anything
within 2 minutes (check `docker logs bss_postgres_primary` and
`bss_postgres_standby_1/2` for etcd connectivity — a lost etcd quorum, not
just a lost primary, is the failure mode that leaves Patroni unable to act).

**Manual fallback (automation itself is degraded, or no HA overlay is
deployed with a replica you provisioned by hand):**

```bash
# On the healthiest standby
docker exec bss_postgres_standby_1 patroni ctl -c /etc/patroni/patroni.yml \
  failover --candidate postgres_standby_1 --force
# or, if Patroni itself is unreachable, the raw promotion it would have run:
docker exec bss_postgres_standby_1 psql -U postgres -c "SELECT pg_promote();"
```

Update `DB_DSN` only if the manual `pg_promote()` path was used (Patroni's
own failover does not require a DSN change — see step 2 above).

**RPO/RTO:** RPO ≈ 0 in the common case (`synchronous_mode: true`), degrading
to seconds of possible loss if the synchronous standby was already
unreachable before the primary failed (`synchronous_mode_strict: false` —
IDD §8.2a explains why that trade-off is deliberate: a single dead standby
must not stall every write on an otherwise-healthy primary). Target RTO: under
1 minute for the automated Patroni path; 15 minutes for the manual fallback,
unchanged from before this section existed.

### 12.3.3 Dead-Letter Queue Non-Empty

**Symptoms:** PagerDuty alert `dead_letter_queue_non_empty`; Grafana `dead_letter_queue_depth` > 0.

**Response:**

1. Identify the dead tasks and, crucially, *why* they died — `last_error`
   carries the failure the final retry hit:
   ```bash
   psql -d isp_bss_oss -c "SELECT id, task_type, retry_count, last_error, created_at
                            FROM jobqueue_tasks
                            WHERE queue = 'network_commands' AND status = 'dead'
                            ORDER BY id DESC LIMIT 20;"
   ```
2. Read `last_error` before retrying anything. The common causes divide
   cleanly: a NAS that is unreachable (timeouts) versus a request the NAS
   actively refused (NAK) — the first is worth retrying once the device is
   back, the second will fail identically no matter how many times it runs.
3. **If the NAS is reachable**, return the tasks to the pool:
   ```bash
   psql -d isp_bss_oss -c "UPDATE jobqueue_tasks
                              SET status = 'pending', retry_count = 0, run_after = now(),
                                  locked_by = NULL, lease_expires_at = NULL
                            WHERE queue = 'network_commands' AND status = 'dead';"
   ```
4. **If the NAS is unreachable**, do not retry — escalate to the network team.
   Affected subscribers stay in their current state until the device recovers.
   Retrying against a down NAS just re-exhausts the retries and refills this
   queue.
5. Once the NAS recovers, run step 3.
6. For tasks that can never succeed (a subscriber since deleted, a session
   long gone), record why in the incident, then mark them completed so the
   alert clears and genuinely new failures stay visible:
   ```bash
   psql -d isp_bss_oss -c "UPDATE jobqueue_tasks SET status = 'completed', completed_at = now()
                            WHERE id IN ({TASK_IDS});"
   ```

### 12.3.4 High RADIUS Authentication Latency

**Symptoms:** Grafana `radius_auth_duration_seconds` p99 > 15 ms.

**Response:**

1. Check the subscriber auth cache hit rate. This cache is in-process
   (`internal/cache`, one map per running service — there is no external cache
   tier to check), so a collapsed hit rate means entries are being invalidated
   or expiring faster than they are reused:
   ```bash
   # Prometheus: radius_subscriber_cache_hits_total
   #             radius_subscriber_cache_misses_total
   # A miss rate climbing toward 100% sends every auth to PostgreSQL.
   ```
2. Check the RADIUS worker queue and whether anything is being shed:
   ```bash
   # radius_worker_queue_depth / radius_worker_queue_capacity
   #   Sustained utilisation above ~50% means the pool is not keeping up.
   #   Depth is the leading indicator — it rises before anything is dropped.
   #
   # radius_packets_dropped_total{listener="auth"|"acct"}
   #   Non-zero means the daemon is over capacity NOW and is shedding.
   #   This is deliberate (the NAS retransmits) but it is never normal.
   #
   # radius_verifier_cache_hit_total
   #   A hit rate collapsing toward zero means every authentication is
   #   paying bcrypt cost-12 (~280ms of CPU each). On a 2-vCPU host that
   #   caps throughput near 7 auths/sec regardless of queue tuning — see
   #   step 5.
   ```

   **If drops are non-zero but the kernel counters in step 5 are clean**, the
   daemon is shedding load it genuinely cannot serve. That is capacity, not
   configuration: the fix is CPU or a warm verifier cache, not tuning.
3. Check PostgreSQL for slow queries — with the cache in-process, PostgreSQL
   is the only tier behind it, so a slow `GetSubscriberByUsername` shows up
   directly in auth latency:
   ```sql
   SELECT query, mean_exec_time, calls
   FROM pg_stat_statements
   ORDER BY mean_exec_time DESC
   LIMIT 10;
   ```
4. Confirm `idx_sub_auth (username, status)` is still being used by the auth
   lookup — this index is what holds NFR-PERF-001's 15 ms budget:
   ```sql
   EXPLAIN ANALYZE
   SELECT s.id FROM subscribers s JOIN plans p ON p.id = s.plan_id
    WHERE s.username = 'known_user';
   ```
   A sequential scan here is the finding; a restart will not fix it.

5. **Check for loss the application cannot see.** Packets discarded by the
   kernel never reach the daemon, so no Prometheus metric will ever show
   them. A dashboard that looks entirely healthy while subscribers cannot
   authenticate is this, and it is the most misleading state the system has.
   ```bash
   nstat -az | grep -i udp        # RcvbufErrors / InErrors: socket buffer overflow
   netstat -su                    # same, on hosts without nstat
   cat /proc/sys/net/netfilter/nf_conntrack_count \
       /proc/sys/net/netfilter/nf_conntrack_max
   dmesg | grep -i conntrack      # "table full, dropping packet"
   ```
   - **RcvbufErrors rising** — the socket receive buffer is too small for
     the burst. The daemon requests 4 MiB per listener but the kernel caps
     it at `net.core.rmem_max`; it logs the size it was actually granted at
     startup, so check that line before assuming the request took effect.
     `deploy/gcp/provision.sh` writes the sysctl.
   - **conntrack near max** — when that table fills the kernel drops *new
     flows host-wide*, including the SSH session you are diagnosing from.
     `deploy/gcp/docker-compose.gcp.yml` puts the daemon on host networking
     specifically so RADIUS creates no conntrack entries at all.

6. **After a restart, expect a cold verifier cache.** The daemon logs
   `verifier cache warmed` with a count at startup (migration 046). A count
   of zero after a restart means every reconnecting subscriber will pay full
   bcrypt — check that the persistent tier is reachable, because this is the
   difference between a recovery measured in seconds and one measured in
   tens of minutes.

`scripts/storm_test_radius.sh` runs all of the above as a single measured
cold-vs-warm comparison, and is the right way to establish a baseline for
this host *before* an incident rather than during one.

---

## 12.4 Backup & Restore Procedures

### PostgreSQL Point-in-Time Restore

```bash
# Stop the application services
docker-compose stop aaa_core_daemon api_service

# Restore from WAL archive to a specific point in time
# (Example using pg_basebackup + WAL replay — adjust to your archiving setup)
pg_restore --jobs=4 -h localhost -U postgres -d isp_bss_oss /backup/pgdump/20250101.dump

# Restart services
docker-compose start aaa_core_daemon api_service

# Validate: row counts, wallet balance sum
psql -h localhost -U postgres -d isp_bss_oss \
  -c "SELECT COUNT(*) FROM subscribers WHERE status = 'active';"
```

### Live Session State — Nothing to Restore

There is no separate cache tier to restore. Live session state is the
`live_sessions` table (migration 036) inside the same PostgreSQL the restore
above covers, so it comes back with the database.

It also does not *need* restoring to be correct. The table is a read surface
for the health endpoint and the portal's usage panel, not the accounting
record of truth — that is `subscriber_session_history`. Rows are rebuilt by
the next Accounting-Interim-Update each live session sends (every few
minutes), and a stale row is reclaimed by the staleness sweeper after
`SessionTTL` (30 minutes). After a restore, expect the portal to briefly
report subscribers as offline who are in fact online; it self-corrects on the
next accounting round without operator action.

---

## 12.5 Scheduled Maintenance Checklist (Monthly)

```
□ Review and rotate RADIUS shared secrets on all NAS devices
□ Verify the PII re-encryption job completed successfully (check encryption_keys table)
□ Review dead-letter queue archive — any recurring failures indicate code or infra issues
□ Test PostgreSQL replica promotion procedure in staging
□ Confirm TLS certificate expiry dates (auto-renewal should trigger 30d before expiry)
□ Review Prometheus alert firing history — tune thresholds if noisy
□ Run database VACUUM ANALYZE on high-write tables
□ Rotate JWT signing secret (coordinate with all API consumers)
□ Confirm completed task rows are being reaped (jobqueue_tasks retention_until);
  investigate rather than hand-delete if the table is growing without bound
```

---

## 12.6 Key Dashboard URLs

| Dashboard | URL |
|---|---|
| RADIUS Performance | `https://grafana.internal/d/radius` |
| Network Commands (CoA/PoD) | `https://grafana.internal/d/network_cmds` |
| Billing & Wallet | `https://grafana.internal/d/billing` |
| Infrastructure Health | `https://grafana.internal/d/infra` |
| Task Queue Depths | `https://grafana.internal/d/jobqueue` |
