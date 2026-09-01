# Deploying to Google Cloud

A working GCP deployment of the whole stack on a single Compute Engine VM:
PostgreSQL, both Go services, the PDF renderer, and Caddy terminating TLS
with an automatically issued Let's Encrypt certificate.

```bash
# 1. Infrastructure — static IP, firewall, VM
NAS_SOURCE_RANGES=203.0.113.4/32 ./provision.sh

# 2. Point DNS at the address it prints, then:
DOMAIN=bss.example.com ./deploy.sh
```

---

## Why Compute Engine, and not the serverless products

This is the part of a GCP deployment that catches people out, so it is worth
being direct about: **Cloud Run and App Engine cannot host this system.**

The RADIUS daemon listens on **UDP** 1812 and 1813. Cloud Run and App Engine
terminate HTTP(S) — and gRPC and WebSockets, all of which are still TCP.
Neither can carry a UDP datagram to your process at all. Since RADIUS is how
routers authenticate subscribers, a deployment without it is a set of
screens with no product behind them.

That leaves:

| Option | Verdict |
|---|---|
| **Compute Engine** | What these scripts use. A VM speaks any protocol. |
| **GKE** | Works — an external passthrough Network Load Balancer does support UDP — but it is a lot of moving parts for a single-tenant system. |
| **Cloud Run / App Engine / Cloud Functions** | Cannot work. HTTP only. |

`Cloud SQL for PostgreSQL` is a reasonable later swap for the in-VM
database, at the cost of a network hop on the RADIUS authentication path.

## Before you decide: should the RADIUS half be in the cloud at all?

Worth settling deliberately rather than by default, because the failure mode
is severe and not obvious.

Putting `aaa_core_daemon` in GCP means every subscriber authentication
crosses the public internet from the ISP's routers to a Google datacenter:

- **Latency.** NFR-PERF-001 targets 15 ms p99 for authentication. An
  internet round trip to the nearest region can consume that budget before
  any of this code runs. The in-process subscriber cache absorbs repeat
  auths; a cold one will not make the number.
- **Availability.** If the ISP's transit to GCP drops, *no subscriber can
  get online* — including subscribers whose own connection is perfectly
  healthy. That makes the entire customer base dependent on one uplink, a
  worse failure than most of the outages it is meant to survive.
- **Secrets in transit.** RADIUS has no transport security. Over the public
  internet you want a VPN or Cloud Interconnect between the ISP and GCP,
  which adds back cost and complexity.

The conventional shape for an ISP is **AAA on-premises next to the routers,
billing and console in the cloud**. The complication here is that both
services share one PostgreSQL and `radiusd` reads it on the auth path, so
splitting them puts the database on one side and a network hop on the
other — and since RADIUS is the latency-critical half, the database wants to
stay with it.

**A fair summary:** this deployment is excellent for demos, staging, pilots,
and a real address you can point a customer's test router at. For production
AAA at an ISP with real subscribers, deploy on-premises with the MSI
(`installer/`) and consider GCP for the console and portal later.

---

## Prerequisites

- `gcloud` CLI, authenticated (`gcloud auth login`) with a project set
- A domain you control, and the ability to add an A record
- The public IPs of the routers that will authenticate against this server
- Billing enabled on the project

## Step 1 — Provision

```bash
cd deploy/gcp

NAS_SOURCE_RANGES=203.0.113.4/32,198.51.100.0/24 \
  ./provision.sh
```

Creates a reserved static IP, two firewall rules, and a Debian 12 VM that
installs Docker on first boot. Re-running is safe: existing resources are
detected and left alone, except firewall source ranges, which are updated so
you can widen or narrow access without deleting rules.

`NAS_SOURCE_RANGES` is **required and has no default.** RADIUS authenticates
on a shared secret with no transport security, so an open UDP 1812 lets
anyone capture handshakes and attack that secret offline. Your NAS estate is
a small, known, static set of addresses — there is no legitimate reason to
accept RADIUS from anywhere else. The script refuses to run rather than
quietly defaulting to `0.0.0.0/0`.

Useful overrides: `PROJECT`, `REGION` (default `asia-south1`), `ZONE`,
`MACHINE_TYPE` (default `e2-standard-2`), `WEB_SOURCE_RANGES`, and
`DRY_RUN=1` to print commands without running them.

## Step 2 — DNS

Point an A record for your domain at the static IP the script printed.

**Do this before step 3.** Caddy requests a certificate the moment the stack
starts, and the ACME challenge fails if the name does not already resolve
here — leaving you on a self-signed certificate that browsers reject. Worse,
repeated failures count against Let's Encrypt's rate limits for that name.
`deploy.sh` checks DNS and refuses to continue rather than burning those
attempts.

## Step 3 — Deploy

```bash
DOMAIN=bss.example.com ./deploy.sh
```

This is also the update path — run it again for every subsequent release. It
ships `git archive HEAD`, so **what deploys is what is committed**; uncommitted
working-tree changes are deliberately not included, and neither is anything
untracked such as `.env` or key material.

The ordering inside is deliberate: PostgreSQL starts, migrations apply, and
only then do the application services start. Both matter — the services post
to chart-of-accounts codes that arrive in migrations (5200 and 2100 in
migration 045), and a binary running ahead of its schema refuses those
postings rather than writing a ledger it cannot balance.

## Step 4 — Register the routers

On each NAS, point RADIUS authentication and accounting at the static IP,
ports 1812 and 1813. Then add each device on the console's **Routers** screen
with a matching shared secret.

The first-run admin credentials are in the API service log:

```bash
gcloud compute ssh isp-bss --zone asia-south1-a \
  --command 'cd /opt/isp-bss && sudo docker compose logs api_service | grep -i "initial admin"'
```

---

## What is not exposed, on purpose

PostgreSQL (5432) and the Prometheus metrics endpoints (9101/9102) get no
firewall rule. They are reachable inside the Docker network, and to an
operator over an SSH tunnel:

```bash
gcloud compute ssh isp-bss --zone asia-south1-a -- -L 5432:localhost:5432
```

Publishing either to the internet adds attack surface for no operational
gain.

## Back up the AES key store

`config/keys/aes_keys.json` is generated on the VM at first deploy and is
**not** in any PostgreSQL dump. It decrypts the encrypted PII columns.

Lose it and a database restore will not bring those values back — the rows
survive and are permanently unreadable. Copy it somewhere off the VM as soon
as the first deploy finishes.

## Operating it

```bash
# from the instance, in /opt/isp-bss
sudo docker compose ps
sudo docker compose logs -f api_service aaa_core_daemon
sudo docker compose restart api_service
```

Everything in [OPS](../../specification_docs_v2/12_OPS_Operations_Runbook.md)
applies — its procedures are written against `docker compose` and the
PostgreSQL-backed task queue, which is what this deployment runs.

## Cost shape

An `e2-standard-2` with a 50 GB balanced disk and a static IP is the whole
footprint; there is no managed database, load balancer, or Kubernetes control
plane in this design. Egress is dominated by RADIUS accounting, which is
small. Consult current GCP pricing for actual figures — they change, and
this file will not.
