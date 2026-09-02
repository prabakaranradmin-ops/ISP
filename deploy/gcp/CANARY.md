# Canary runbook — RADIUS storm hardening

Applying the UDP-storm hardening (non-blocking enqueue, socket buffers,
host networking, migration 046's verifier persistence) to a **staging** GCE
VM, verifying each layer independently, and rolling back if needed.

**Time:** ~30 minutes, most of it the storm test.
**Blast radius:** staging only. Do not run this against production first —
step 4 deliberately overloads the daemon.

> **Why each layer is verified separately.** Three of these changes fail
> *silently* when they don't take effect: a sysctl that was never applied, a
> socket buffer the kernel clamped, and a migration the binary is running
> ahead of. In all three cases the stack starts and serves traffic, and you
> find out during the storm you were trying to survive. Each step below
> confirms one layer before moving to the next.

---

## 0. Preconditions

```bash
export PROJECT=your-staging-project
export ZONE=asia-south1-a
export INSTANCE=isp-bss-staging
export REMOTE_DIR=/opt/isp-bss

gcloud compute instances describe "$INSTANCE" --zone "$ZONE" --project "$PROJECT" \
  --format='value(status,machineType)'
```

Capture the current state so you can tell what actually changed:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  echo "== git ==";      cd /opt/isp-bss && git rev-parse --short HEAD 2>/dev/null || echo "(tarball deploy, no git)"
  echo "== running ==";  sudo docker compose ps --format "table {{.Service}}\t{{.Status}}"
  echo "== sysctl ==";   sysctl net.core.rmem_max net.netfilter.nf_conntrack_max 2>/dev/null
' | tee ~/canary-before.txt
```

---

## 1. Deploy

`provision.sh` is **not** re-run — it owns networking and the static IP, and
nothing in this change touches those. Re-running it is safe but pointless,
and it would prompt for `NAS_SOURCE_RANGES` again.

```bash
cd deploy/gcp
DOMAIN=staging.bss.example.com \
INSTANCE="$INSTANCE" ZONE="$ZONE" PROJECT="$PROJECT" \
  ./deploy.sh
```

Watch for these in the output, in order:

| Line | Means |
|---|---|
| `docker compose 2.2x.x supports !reset` | The overlay will apply. Below 2.24 it stops here. |
| `postgres ready` | Database up before migrations — the ordering that matters. |
| `migrations applied` | **Migration 046 landed.** |
| `bss_app verified` | The app role can still connect after the migration. |
| `/readyz is serving` | Certificate intact; the deploy did not break TLS. |

If it stops at the Compose version check:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'sudo apt-get update && sudo apt-get install -y --only-upgrade docker-compose-plugin && docker compose version'
```

---

## 2. Verify the kernel tuning actually took effect

`provision.sh` writes the sysctl on **boot**. A deploy alone does not
re-run it, so on an instance provisioned before this change the file will
not exist yet.

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  echo "== file present? =="
  ls -l /etc/sysctl.d/60-isp-bss.conf 2>/dev/null || echo "MISSING"
  echo "== live values =="
  sysctl net.core.rmem_max net.core.rmem_default net.core.netdev_max_backlog
  sysctl net.netfilter.nf_conntrack_max net.netfilter.nf_conntrack_udp_timeout
'
```

**Expected:**

```
net.core.rmem_max = 16777216
net.core.rmem_default = 4194304
net.core.netdev_max_backlog = 5000
net.netfilter.nf_conntrack_max = 262144
net.netfilter.nf_conntrack_udp_timeout = 10
```

**If the file is MISSING** — the instance predates the change. Apply without
a reboot:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  sudo google_metadata_script_runner startup && sudo sysctl --system | grep -E "rmem_max|conntrack_max"
'
```

**If `nf_conntrack_*` keys are absent entirely**, the module isn't loaded —
normal on a host that has not NAT'd anything yet:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'sudo modprobe nf_conntrack && sudo sysctl --system >/dev/null && sysctl net.netfilter.nf_conntrack_max'
```

---

## 3. Verify the daemon got the buffer it asked for

This is the step that catches a clamped socket buffer. `SetReadBuffer` is a
*request*; the kernel silently caps it at `net.core.rmem_max`. The daemon
logs what it was actually granted precisely so this is checkable.

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'cd /opt/isp-bss && sudo docker compose logs aaa_core_daemon | grep -E "read buffer|verifier cache|RADIUS listening"'
```

**Healthy:**

```
INF radius: UDP read buffer configured listener=auth granted_bytes=8388608 requested_bytes=4194304
INF radius: UDP read buffer configured listener=acct granted_bytes=8388608 requested_bytes=4194304
INF radiusd: verifier cache warmed restored=0
INF radiusd: RADIUS listening addr=:1812 acct_addr=:1813
```

> `granted_bytes` being roughly **double** the request is correct, not a
> bug — Linux reports `SO_RCVBUF` including its own per-socket bookkeeping
> overhead. Anything **at or above** the request means it was honoured.

**Problem signatures:**

| Log line | Meaning | Fix |
|---|---|---|
| `WRN ... the kernel capped the UDP read buffer below the request` | Step 2 did not take, or Docker started before the sysctl | Redo step 2, then `docker compose restart aaa_core_daemon` |
| `WRN ... could not set UDP read buffer` | The `setsockopt` itself failed | Unexpected — capture the error and stop |
| No read-buffer line at all | Running an older binary | The deploy did not replace the image; re-run step 1 |
| `verifier cache warmup failed` | Migration 046 not applied, or DB unreachable | See step 5 |

`restored=0` on the **first** deploy is correct — the table is empty. It
should be non-zero on any subsequent restart, which step 5 confirms.

---

## 4. Confirm host networking is in effect

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  echo "== network mode (want: host) =="
  sudo docker inspect bss_aaa_core_daemon --format "{{.HostConfig.NetworkMode}}"
  echo "== memory limit (want: 2147483648) =="
  sudo docker inspect bss_aaa_core_daemon --format "{{.HostConfig.Memory}}"
  echo "== the daemon is bound directly on the host =="
  sudo ss -ulnp | grep -E ":1812|:1813"
'
```

Under host networking `ss` shows the **daemon process itself** bound to
1812/1813. If you see `docker-proxy` instead, the overlay did not apply —
check that `deploy.sh` passed both `-f` files.

Quick sanity check that RADIUS still answers at all:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'cd /opt/isp-bss && sudo docker compose logs --tail=20 aaa_core_daemon'
```

---

## 5. Verify migration 046 and the verifier cache

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  cd /opt/isp-bss
  sudo docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss -c "\d radius_verifier_cache"
  sudo docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss \
    -c "SELECT count(*) AS cached, min(expires_at) AS oldest FROM radius_verifier_cache;"
'
```

Then prove warmup actually works — authenticate something, restart, and
confirm the count is restored:

```bash
# after some real or simulated authentication has occurred
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  cd /opt/isp-bss
  echo "== rows before restart =="
  sudo docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss -tAc \
    "SELECT count(*) FROM radius_verifier_cache;"
  sudo docker compose restart aaa_core_daemon >/dev/null 2>&1
  sleep 10
  echo "== warmup after restart =="
  sudo docker compose logs --tail=50 aaa_core_daemon | grep "verifier cache warmed"
'
```

`restored=N` matching the row count is the whole feature working. `restored=0`
with a non-zero row count means the L2 read is failing — check for a
`verifier cache warmup failed` line.

---

## 6. Storm test

**Staging only.** This deliberately pushes past capacity.

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE"

# on the instance
cd /opt/isp-bss
go run ./scripts/seed_load -users-out .canary_users.csv   # or copy an existing CSV

COLD_RESTART=1 \
USERS_CSV=.canary_users.csv \
RADIUS_SECRET="$(grep '^RADIUS_SECRET=' .env | cut -d= -f2)" \
RATE=4000 DURATION=60s \
COMPOSE_FILES="-f docker-compose.yml -f deploy/gcp/docker-compose.gcp.yml" \
  bash scripts/storm_test_radius.sh 2>&1 | tee ~/canary-storm.txt
```

### Reading the result

| Observation | Verdict |
|---|---|
| App drops > 0, **kernel drops 0** | **Pass.** Shedding deliberately and counting it. The NAS retransmits. |
| Warm accepts several × cold accepts | **Pass.** The verifier cache is doing its job. |
| `UdpRcvbufErrors` > 0 | **Fail** — buffer still too small. Redo steps 2–3. |
| conntrack climbing toward max | **Fail** — host networking not in effect. Redo step 4. |
| Cold ≈ warm throughput | **Investigate** — the cache is not being hit. Check accepts > 0 and `radius_verifier_cache_hit_total` rising. |
| Both drop counts 0 but latency high | CPU-bound. Cold bcrypt is ~7 auths/sec on 2 vCPU; only cores or a warm cache move it. |

Confirm the new metrics are actually exported:

```bash
curl -s http://127.0.0.1:9100/metrics | grep -E \
  'radius_packets_dropped_total|radius_worker_queue_(depth|capacity)|radius_verifier_cache_hit_total'
```

---

## 7. Soak

Leave it for **24 hours** before promoting. The failure this change fixes is
a slow accumulation, so a green 60-second test proves less than a quiet day.

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command '
  echo "== daemon memory (was unbounded growth; should be flat) =="
  sudo docker stats --no-stream bss_aaa_core_daemon
  echo "== restarts (want 0 — a restart means OOM or crash) =="
  sudo docker inspect bss_aaa_core_daemon --format "{{.RestartCount}}"
  echo "== verifier table size (should plateau, not grow without bound) =="
  cd /opt/isp-bss && sudo docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss -tAc \
    "SELECT count(*) FROM radius_verifier_cache;"
'
```

**Promote when:** restart count 0, memory flat, verifier table plateaued
near your active-subscriber count, no `WRN` read-buffer lines, and no
kernel UDP errors accumulating.

---

## Rollback

Nothing here requires a database rollback, and you should not do one.

**Application and container changes** — redeploy the previous commit:

```bash
cd deploy/gcp
git -C ../.. checkout <previous-sha>
DOMAIN=staging.bss.example.com ./deploy.sh
git -C ../.. checkout -
```

**To drop only host networking** (keeping everything else), deploy without
the overlay:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'cd /opt/isp-bss && sudo docker compose -f docker-compose.yml up -d --force-recreate aaa_core_daemon'
```

**Migration 046 needs no rollback.** The table is additive, nothing else
references it, and an older binary ignores it completely — it simply stops
being written to and the reaper stops running. Rolling it back would delete
cached verifiers for no benefit and guarantee a cold cache on the next
start. If you genuinely must:

```bash
cd /opt/isp-bss && sudo docker run --rm --network "$(sudo docker network ls --format '{{.Name}}' | grep -m1 bss_internal)" \
  -v "$(pwd):/src" -w /src golang:1.23 \
  go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
    -dir ./migrations postgres "$DSN" down
```

**Sysctl** — delete the file and reboot, or reset the two keys live:

```bash
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --command \
  'sudo rm -f /etc/sysctl.d/60-isp-bss.conf && sudo sysctl --system >/dev/null'
```

---

## Known unknowns going in

Stated plainly so the canary is watched for the right things — none of the
following had been executed anywhere when this runbook was written:

- The sysctl values, host networking, and the `!reset` overlay were
  reasoned about and syntax-checked, never run on a Linux host.
- Migration 046 had not been applied to any database.
- `storm_test_radius.sh` had never been executed end to end.

The application logic *is* tested: the shed-not-block behaviour is
mutation-verified (reverting the fix makes its test fail), and the L1/L2
cache semantics are covered by `internal/db/verifiercache_integration_test.go`
against a real PostgreSQL. Run that suite before this canary:

```bash
./scripts/run_db_tests.sh -run 'TestMigration046|TestVerifierCache'
```
