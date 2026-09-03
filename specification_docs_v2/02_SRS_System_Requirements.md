# Document 2: System Requirements Specification (SRS)
**Version:** 3.0 | **Status:** Draft | **Date:** 2026-08-12
**Document ID:** SRS
**Traces From:** [CRD](01_CRD_Customer_Requirements.md)
**Traces To:** [SAD](03_SAD_System_Architecture.md) → [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md) → [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 2.1 Functional Requirements Matrix

### AAA / Authentication

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-AAA-001 | Process concurrent UDP RADIUS requests on port 1812 (Auth) and 1813 (Accounting) | — | MDS §4.1 |
| FR-AAA-002 | Authenticate users against Redis cache in ≤ 5 ms | — | MDS §4.1 |
| FR-AAA-003 | Deduplicate Interim-Update packets via atomic Redis SetNX (session ID + octet count key) | — | DDS §5.2 |
| FR-AAA-004 | Write subscriber session to Redis on first auth; TTL = plan validity period | — | MDS §4.1 |
| FR-AAA-005 | ~~Support CHAP~~ **Deferred by decision (2026-08-14):** plain CHAP requires the plaintext password to recompute `MD5(id ‖ pw ‖ challenge)`. The chosen storage strategy is an opt-in NT-hash (see FR-AAA-006), which cannot answer a CHAP challenge. Enabling CHAP would require storing reversibly-encrypted passwords, a blast radius deliberately not accepted | CRD-EXP-001 | MDS §4.1 — deferred |
| FR-AAA-006 | Support EAP-MSCHAPv2 for wireless-controller/hotspot deployments. Verification uses a nullable `subscribers.nt_hash`, populated **only** for subscribers who opt into EAP; bcrypt remains the PAP path and the sole credential for everyone else | CRD-EXP-001 | MDS §4.1, §4.18 |
| FR-AAA-007 | A plan change or top-up must invalidate the subscriber's Redis auth-cache entry and enqueue a CoA to any active session, so the new rate limit applies without waiting for reauthentication | CRD-EXP-001 | MDS §4.1, §4.2 |

### Billing & Finance

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-BIL-001 | Calculate GST: 9% CGST + 9% SGST if `registered_state == 'TN'`; 18% IGST otherwise | — | MDS §4.3 |
| FR-BIL-002 | All tax computations must use `decimal.Decimal` with banker's rounding | — | DDS §5.4 |
| FR-BIL-003 | Wallet recharge must execute as atomic double-entry ledger transaction | — | DDS §5.6 |
| FR-BIL-004 | Dunning engine must fire notifications at T-7d, T-3d, T-1d; CoA throttle at T+24h; PoD at T+72h | CRD §1.7 | MDS §4.3 |
| FR-BIL-005 | Wallet recharge endpoint must enforce `transaction_token` idempotency | CRD-PAY-001 | DDS §5.6 |
| FR-BIL-006 | System must generate GSTR-1 compatible export: HSN summary, B2B/B2C split, state-wise breakdown | CRD §1.3 | MDS §4.3 |
| FR-BIL-007 | Invoices must include plain-language usage summary (GB used / GB included) for subscriber clarity | CRD PER-006 | MDS §4.3 |
| FR-BIL-008 | Every plan renewal that extends `plan_expiry` via a wallet debit (portal one-tap renewal or auto-renewal) must generate a GST invoice for that cycle | BO-007 | MDS §4.14 |
| FR-BIL-009 | Subscribers whose plan has expired and whose wallet balance covers the plan price must be auto-renewed from that balance before dunning escalates them, rather than suspended while funded | BO-007 | MDS §4.14 |
| FR-BIL-010 | Staff must be able to post a manual wallet credit or debit adjustment against a subscriber, distinct from a recharge, attributed to the issuing staff member with a required reason, and audit-logged | BO-007 | MDS §4.14 |
| FR-BIL-011 | Staff must be able to issue a refund against a subscriber's wallet balance, tracked with its own status distinct from a wallet ledger adjustment, and audit-logged | BO-007 | MDS §4.14 |

### Subscriber Lifecycle Management *(new — gap BO-007)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-LC-001 | A staff-initiated plan change must recompute `plan_expiry` with proration for unused value on the old plan, invalidate the subscriber's Redis auth-cache entry, and enqueue a CoA to any active session — closing FR-AAA-007, which was specified but never implemented | BO-007, CRD-EXP-001 | MDS §4.14 |
| FR-LC-002 | Subscriber termination must be a dedicated action that sets status to `terminated` and enqueues a PoD (forced disconnect) to any active session, distinct from suspension (which only throttles) | BO-007 | MDS §4.14 |
| FR-LC-003 | All lifecycle-affecting actions (plan change, termination, adjustment, refund) must be audit-logged with staff attribution | BO-007, CRD-REG-001 | MDS §4.14 |

### FUP & Session Management

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-FUP-001 | Continuously evaluate Redis traffic counters vs plan bounds; trigger CoA on breach | — | MDS §4.2 |
| FR-FUP-002 | CoA and PoD must retry via Asynq with exponential backoff, max 5 attempts | — | DDS §5.3 |
| FR-FUP-003 | Failed CoA/PoD tasks exhausting retries → dead-letter queue + operator alert ≤ 60s | — | MDS §4.2 |
| FR-FUP-004 | System must send WhatsApp + SMS notification when subscriber reaches 80% of FUP threshold | CRD-NOTIF-001, CRD §1.7 | MDS §4.7 |
| FR-FUP-005 | System must send WhatsApp + SMS notification when FUP throttle is applied (with reason and restore instructions) | CRD-NOTIF-001 | MDS §4.7 |

### Security

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-SEC-001 | Block identity after 10 invalid credentials within 60 s (Redis token bucket; 15-min ban) | — | MDS §4.1 |
| FR-SEC-002 | All PII fields (Aadhaar, PAN) must be AES-GCM-256 encrypted before any DB write | CRD-REG-002 | DDS §5.5 |
| FR-SEC-003 | Encrypted PII must store key version ID in ciphertext for cross-rotation decryption | CRD-REG-002 | DDS §5.5 |
| FR-SEC-004 | All inbound payment webhooks must be HMAC-SHA256 validated before state mutation | CRD-PAY-001 | DDS §5.6 |
| FR-SEC-005 | All admin API routes must require valid JWT bearer token; role enforced per route | — | DDS §5.7 |

### Notifications — WhatsApp, SMS, Email

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NOTIF-001 | Send dunning reminders via WhatsApp + SMS + email at T-7d, T-3d, T-1d | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-002 | Send WhatsApp + SMS when subscriber reaches 80% FUP threshold | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-003 | Send WhatsApp + SMS when FUP throttle is applied (speed reduced) | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-004 | Send WhatsApp + SMS payment receipt on successful wallet recharge | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-005 | Send WhatsApp + SMS on soft suspension (T+24h), stating reason and payment link | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-006 | Send WhatsApp + SMS on service restoration after recharge | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-007 | Send WhatsApp notification on ticket status change | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-008 | All notifications must validate `dnd_opt_out` flag before dispatch | CRD-REG-003 | MDS §4.7 |
| FR-NOTIF-009 | Every outbound notification must create a `notification_log` record with: channel, template ID, subscriber ID, event, timestamp, delivery status, failure reason | CRD-NOTIF-002 | DBD §6.2 |
| FR-NOTIF-010 | WhatsApp messages must use pre-approved Business API templates; template ID must be stored in notification config | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-011 | System must store WhatsApp delivery status callbacks: sent → delivered → read / failed | CRD-NOTIF-001 | DBD §6.2 |
| FR-NOTIF-012 | System must support email as a notification channel alongside WhatsApp and SMS, degrading gracefully when SMTP is unconfigured | CRD-EXP-003 | MDS §4.7, §4.17 |
| FR-NOTIF-013 | System must support push notifications (OneSignal or FCM/APNs) for mobile app users, sharing the same DND/notification_log path as other channels, with per-device token registration | CRD-EXP-003 | MDS §4.7, §4.17 |

### Observability

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-OBS-001 | Expose Prometheus `/metrics`: RADIUS auth latency histograms, CoA ACK rates, active sessions, FUP breach counters | — | MDS §4.1 |
| FR-OBS-002 | All logs emitted as structured JSON: `timestamp`, `level`, `service`, `correlation_id`, `message`, `subscriber_id` | — | SAD §3.2 |
| FR-OBS-003 | All LEA data access events must write tamper-evident audit record | CRD-REG-001 | DBD §6.2 |
| FR-OBS-004 | Expose subscriber health endpoint (single-call diagnostic view) for CSR and NOC use | CRD PER-002, PER-004 | API §7 |
| FR-OBS-005 | System must emit proactive alert when RADIUS auth failure rate on any NAS exceeds 20% over 5 min | CRD PER-002 | SAD §3.2 |

### CGNAT & Network

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NET-001 | Record CGNAT port-block allocations: public IP, port range, subscriber ID, timestamps | CRD-REG-001 | DBD §6.2 |
| FR-NET-002 | Expose secured LEA lookup API: public IP + port + timestamp → subscriber identity | CRD-REG-001 | API §7 |
| FR-NET-003 | Record IPv6 prefix delegations in `subscriber_session_history` | — | DBD §6.2 |

### Revenue Assurance *(new — gap BO-001)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-REV-001 | System must produce unbilled-subscriber report: all active subscribers with no invoice in current cycle | CRD-REV-001 | MDS §4.8 |
| FR-REV-002 | System must reconcile sum(wallet_balance) against sum(ledger credits) and flag variance > ₹0.01 | CRD-REV-001 | MDS §4.8 |
| FR-REV-003 | Collections dashboard must show outstanding balance by dunning stage and month-over-month recovery rate | CRD-REV-002 | API §7 |
| FR-REV-004 | System must generate 30-day forward collections forecast based on expiry dates and wallet balances | CRD-REV-002 | MDS §4.8 |

### Subscriber Self-Service Portal *(new — gap PER-006)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-SUB-001 | Subscriber portal must display real-time data usage (current session GB used / plan GB total) | CRD PER-006 | MDS §4.9 |
| FR-SUB-002 | Portal must display current plan, expiry date, wallet balance, and last 3 invoices | CRD PER-006 | MDS §4.9 |
| FR-SUB-003 | Portal must allow one-tap plan renewal via Razorpay / BBPS payment link | CRD PER-006 | MDS §4.9 |
| FR-SUB-004 | Portal must allow subscriber to raise a support ticket and view its status | CRD PER-006 | MDS §4.9 |
| FR-SUB-005 | Portal must display notification delivery history (what was sent, when, channel) | CRD-NOTIF-002 | MDS §4.9 |

### Franchise / LCO *(new — gap BO-004)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-FRN-001 | System must support multi-tenant LCO accounts: each LCO manages their own subscribers under a parent ISP | CRD-FRN-001 | MDS §4.10 |
| FR-FRN-002 | LCO commission must be calculated per recharge and tracked in a separate LCO ledger | CRD-FRN-001 | MDS §4.10 |
| FR-FRN-003 | Parent ISP must see consolidated P&L across all LCO partners via a franchise analytics view | CRD-FRN-001 | API §7 |
| FR-FRN-004 | The franchise commission/P&L engine must be reachable via routed API endpoints and a staff-console section, gated to the `franchise_admin`/`franchise_staff` roles already defined in code | CRD-EXP-002 | MDS §4.10, API §7 |
| FR-FRN-005 | LCO/franchise partners must have their own restricted portal login, scoped so they cannot see another franchise's subscribers | CRD-FRN-001, CRD-EXP-002 | MDS §4.10 |
| FR-FRN-006 | Onboarding a new LCO/franchise partner must be a staff-console workflow, not a direct database insert | CRD-EXP-002 | MDS §4.10 |

### NAS Vendor Support *(new — gap CRD-EXP-001, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NAS-001 | Send vendor-appropriate bandwidth-control RADIUS attributes per NAS vendor (Cisco `cisco-avpair`, Juniper, Huawei vendor-2011, MikroTik vendor-14988, generic RFC 3576) instead of a single hardcoded MikroTik VSA | CRD-EXP-001 | MDS §4.1 |
| FR-NAS-002 | A `nas_devices` table must record each NAS's IP, vendor, RADIUS secret, and CoA/PoD port, replacing the single global shared secret | CRD-EXP-001 | DBD §6.2 |
| FR-NAS-003 | CoA/PoD attribute construction must be selected per session based on the originating NAS's recorded vendor | CRD-EXP-001 | MDS §4.2 |
| FR-NAS-004 | Support wireless-controller-specific attributes (Cisco, Aruba, Ruckus) for hotspot/WiFi bandwidth and session control | CRD-EXP-001 | MDS §4.1 |

### Hotspot / Captive Portal *(new — gap CRD-EXP-003, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-HSP-001 | Serve a captive-portal web page for hotspot subscribers, redirecting unauthenticated MAC addresses to a walled-garden login page | CRD-EXP-003 | MDS §4.1, new module |
| FR-HSP-002 | Support MAC-address-based authentication (MAC auth bypass) as an alternative to username/password for hotspot NAS devices | CRD-EXP-003 | MDS §4.1 |
| FR-HSP-003 | Hotspot sessions must use the same FUP/CoA machinery as PPPoE sessions for rate limiting | CRD-EXP-003 | MDS §4.2 |

### Partner / 3rd-Party Integration *(new — gap CRD-EXP-003, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-API-001 | ✅ Support API-key-based authentication for 3rd-party integrations, distinct from internal staff JWTs, scoped to specific read/write permissions. The separation is structural: `APIKeyMiddleware` sets no role in the context, so a partner key fails every `RequireRole` check whatever route it reaches. Keys are `pk_{env}_{prefix}_{secret}`, stored as SHA-256 — not bcrypt, since 192 bits of CSPRNG output has no dictionary to slow down and bcrypt would cost ~100ms per partner request | CRD-EXP-003 | MDS §4.22, DDS §5.7 |
| FR-API-002 | ✅ Support outbound webhooks: a partner registers a callback URL and receives signed HTTP POSTs on subscriber lifecycle events. Six events: `subscriber.created`, `subscriber.status_changed`, `payment.received`, `invoice.generated`, `ticket.created`, `ticket.resolved`. **Thin payloads** (decision 2026-08-15) — `{event_id, event_type, entity_id, occurred_at}` only, keeping PII out of the delivery log and out of DPDP retention. Signed `X-ISP-Signature: t=<unix>,v1=<hmac-sha256>` with the timestamp inside the signed material so a captured delivery cannot be replayed with a fresh clock value. Partner-supplied URLs are SSRF-guarded at registration **and** at dial time, the latter being the only check DNS rebinding cannot defeat | CRD-EXP-003 | MDS §4.22 |
| FR-API-003 | ✅ Outbound webhook deliveries retry with exponential backoff on a dedicated queue (8 attempts over ~a day) and log every attempt to `webhook_deliveries`. A unique `(endpoint_id, event_id)` index makes a retry update the trail rather than fork it. 4xx retries as well as 5xx — a partner's own broken auth is transient from our side. A target that resolves to a private range is abandoned rather than retried, since no retry makes it public | CRD-EXP-003 | MDS §4.22 |

### Mobile *(new — gap CRD-EXP-003, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-MOB-001 | Expose a mobile-facing API (or reuse the existing portal API) with push-token registration for iOS/Android apps | CRD-EXP-003 | MDS §4.9 |
| FR-MOB-002 | Mobile app must support the same self-service capabilities as the web portal: usage, invoices, renewal, tickets, notification history | CRD-EXP-003 | MDS §4.9 |

### Helpdesk / SLA *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-SUP-001 | Tickets must carry a priority (low/medium/high/critical) and a computed SLA due-by timestamp based on priority and category | CRD-EXP-002 | MDS §4.13, DBD §6.2 |
| FR-SUP-002 | Alert (dashboard + notification) when a ticket is approaching or has breached its SLA due-by time | CRD-EXP-002 | MDS §4.13, DBD §6.2 |
| FR-SUP-003 | Support ticket assignment rules/routing (e.g., by category or franchise) beyond manual `assigned_to` | CRD-EXP-002 | MDS §4.13, DBD §6.2 |

### CRM / Lead Management *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-CRM-001 | Track prospects (pre-subscriber leads) with contact info, source, and status through a pipeline (new → contacted → qualified → converted/lost), franchise-scoped like subscribers | CRD-EXP-002 | MDS §4.16 |
| FR-CRM-002 | Converting a lead to a subscriber must carry over prospect data, create the subscriber and mark the lead converted in one transaction, and be safe against two staff converting the same lead concurrently | CRD-EXP-002 | MDS §4.16 |
| FR-CRM-003 | Report lead-to-subscriber conversion rate and pipeline funnel by source/stage | CRD-EXP-002 | MDS §4.16 |

### Inventory / CPE Management *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-INV-001 | Track CPE inventory: device type, serial number, vendor, warehouse/location, and status (in-stock / issued / returned / faulty) | CRD-EXP-002 | MDS §4.16 |
| FR-INV-002 | Issuing a CPE during onboarding must link the device serial number to the subscriber record, and one device must never be issuable to two subscribers | CRD-EXP-002 | MDS §4.16 |
| FR-INV-003 | Track vendor purchase records and low-stock alerts per device type, evaluated at issuance/purchase time rather than by polling | CRD-EXP-002 | MDS §4.16 |

### Task & Approval Workflows *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-WFL-001 | Sensitive account actions (staff wallet credit, refund, termination) must require second-approver sign-off — created as a pending request, and taking effect only when a different staff member approves it — with self-approval blocked at both the API and the schema | CRD-EXP-002 | MDS §4.15 |
| FR-WFL-002 | Support ad hoc field-task assignment independent of the ticket system, with due dates and completion tracking | CRD-EXP-002 | MDS §4.15 |

### Announcements *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-ANN-001 | Staff can compose and broadcast an announcement to all subscribers or a filtered segment (franchise, plan, status) via WhatsApp/SMS/email/push and/or a portal banner. Area targeting is deferred: `subscribers` carries no address/region column (MDS §4.17) | CRD-EXP-002 | MDS §4.17 |
| FR-ANN-002 | Announcement delivery reuses the existing DND/notification_log machinery for auditability, and defaults to marketing class so opt-out is honoured | CRD-EXP-002 | MDS §4.7, §4.17 |

### General Reporting *(new — gap CRD-EXP-002, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-RPT-001 | ✅ Report subscriber growth/churn trends, plan-mix distribution, and ticket-resolution metrics, beyond revenue reconciliation. Churn and resolution were not computable from the schema at all — `subscribers.status` and `tickets.status` are overwritten in place and `sla_events` records no `resolved` event — so lifecycle transitions are captured append-only by trigger (031) and aggregated by four views (032). Suspension is reported beside churn, never inside it; resolution time is the first arrival at `resolved`, not the last. **Reports before 2026-08-15 show the seeded baseline only and must be labelled partial history** | CRD-EXP-002 | MDS §4.20 (capture), §4.21 (views) |
| FR-RPT-002 | Reports must be exportable (CSV/PDF) and schedulable for periodic email delivery to owner/franchise roles | CRD-EXP-002 | MDS §4.8 (extended) |
| FR-RPT-003 | ✅ Report per-area collection performance for franchise/LCO partners, via `v_franchise_collection` (032). Billed comes from `invoices` and collected from `lco_ledger` — deriving one from the other would make the collection rate definitionally 100%. A franchise with nothing billed reports a NULL rate, not 0%. Decision (2026-08-15): **franchise territory is the reporting area** — no address, region or pincode column exists anywhere in the schema, and franchise is the only grouping that maps to real geography today. A true `service_area` is deferred to the Batch 4 address work | CRD-EXP-002 | MDS §4.21, §4.10 |

### Document Storage *(new — gap CRD-EXP-003, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-DOC-001 | Support archiving generated documents (invoices, KYC scans) to external storage (S3-compatible, Google Drive, or SFTP) in addition to local Gotenberg-only generation | CRD-EXP-003 | new module |

### CPE Auto-Provisioning (TR-069/CWMP) *(new — gap CRD-EXP-003, v3)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-CPE-001 | ✅ Run a **minimal built-in** TR-069 ACS to auto-provision CPE (SSID, bandwidth profile, firmware) on first connect. Decision (2026-08-14): implement the RPC subset this platform needs — Inform, GetParameterValues, SetParameterValues, Reboot, Download, FactoryReset — rather than integrating GenieACS, which would add MongoDB and Node.js to a Go/Postgres/Redis stack. Not a general-purpose ACS. **Connection Request is deliberately not implemented** (see FR-CPE-003) | CRD-EXP-003 | MDS §4.19 |
| FR-CPE-002 | ✅ CPE provisioning profiles must derive from the subscriber's plan, pushing a bandwidth profile to the CPE alongside the NAS-side RADIUS limit. Per-model parameter paths live in `cpe_device_types.provisioning_template`. **Caveat:** PPPoE credentials cannot be pushed — passwords are stored as bcrypt, so `{{pppoe_password}}` has nothing to substitute and the parameter is dropped rather than pushed empty (same constraint that defers FR-AAA-005) | CRD-EXP-003 | MDS §4.19 |
| FR-CPE-003 | ✅ Support remote CPE reboot/firmware-update/diagnostics via TR-069 RPCs, surfaced to NOC/technician roles. **Delivery is queued, not immediate:** CWMP is CPE-initiated and residential CPE sits behind this platform's own CGNAT, so a Connection Request cannot reach it. RPCs execute inside the next device-opened session (`1 BOOT` or `2 PERIODIC`) and the endpoints answer 202 Accepted with `delivered_when` rather than implying the action has happened | CRD-EXP-003 | MDS §4.19 |

---

## 2.2 Non-Functional Requirements Matrix

| NFR ID | Category | Requirement | Validation Method | CRD Ref | TST Ref |
|---|---|---|---|---|---|
| NFR-PERF-001 | Latency | RADIUS auth round-trip ≤ 15 ms at peak load | `radperf` at 5,000 req/s for 10 min | — | TST §13.4 |
| NFR-PERF-002 | Latency | API p99 ≤ 200 ms at 500 concurrent users | k6 load test | — | TST §13.4 |
| NFR-PERF-003 | Latency | WhatsApp/SMS notification dispatch ≤ 5 s from triggering event | End-to-end timing in integration test | CRD-NOTIF-001 | TST §13.3 |
| NFR-SCAL-001 | Concurrency | 20,000 active concurrent PPPoE tunnels without starvation | Load test: ramp 30 min, hold 1h | CRD BO-006 | TST §13.4 |
| NFR-AVAIL-001 | Availability | Core control plane ≥ 99.99% uptime (rolling 12 months) | Synthetic probe every 30 s | — | TST §13.4 |
| NFR-DUR-001 | Durability | Accounting counts: zero data loss on single-node failure | Chaos test: Redis kill during storm | — | TST §13.4 |
| NFR-SEC-001 | Security | TLS 1.3 minimum on all external endpoints | `testssl.sh` scan | CRD-REG-002 | — |
| NFR-SEC-002 | Security | No plaintext PII in logs, DB, or error responses | Automated PII scanner in CI | CRD-REG-002 | TST §13.5 |
| NFR-BIZ-001 | Revenue | Unbilled-subscriber report must run within 60 s for 20,000 subscribers | Timed query test | CRD-REV-001 | TST §13.3 |
| NFR-DUR-002 | Durability | An archived document's SHA-256 is computed from the bytes actually written, not the bytes offered, so a truncated or corrupted write is detectable against the stored checksum; a document must never be purged before its `retain_until` date | Integration test: archive a document and confirm the recorded checksum matches the stored file's actual hash; attempt a purge before `retain_until` and confirm the DB constraint (`chk_archive_not_purged_before_retention`) rejects it. **Gap found drafting this NFR (2026-08-16): the archive `Store` interface has no retrieval method at all (`Put`/`Delete` only) — there is currently no code path to read a document back and re-verify it, only to detect corruption at write time. Restore/verify-on-read is unbuilt; see HANDOFF.md** | — | TST §13.12 |
| NFR-SEC-003 | Security | The captive portal (unauthenticated by design) must rate-limit voucher redemption attempts and fail closed — refuse rather than allow — if the limiter backend is unavailable | Integration test: exceed 10 attempts per MAC in 15 minutes and confirm refusal; kill Redis mid-window and confirm the endpoint refuses rather than admits | — | TST §13.12 |
| NFR-PERF-004 | Latency | Report export (CSV, synchronous) must complete within 4.5 s p99 at the maximum 120-month window, at a base of 20,000 subscribers / 50 franchises / ~430k invoices — measured empirically (2026-08-16: collection report, the worst of the three time-series reports, ran p50 840 ms / p99 1.69 s / max 1.69 s over 30 iterations against a seeded dataset at that scale; threshold set at 2.5× the measured p99). Growth (p99 57 ms) and ticket-resolution (p99 6.7 ms, served from a materialised view) are comfortably inside this budget already | Timed integration test against a seeded 120-month dataset per report type; re-measure if the underlying data volume assumption changes materially | — | TST §13.12 |
| NFR-AVAIL-002 | Availability | PostgreSQL must run with a streaming replica and automated failover — Redis already has this via Sentinel; the database is currently the single point of failure Redis is not | Chaos test: primary kill, measure promotion time | CRD-EXP-001 | TST §13.4 (extend) |
| NFR-TEN-001 | Scalability | Support multi-tenant SaaS hosting (isolated per-ISP-operator data), distinct from today's single-tenant on-premise deployment model | Architecture review + tenant-isolation test | CRD-EXP-004 | TST §13.4 (extend) |

---

## 2.3 Hardware & Software Dependencies

| Component | Minimum Version | Notes |
|---|---|---|
| Linux Kernel | 5.4 | Ubuntu 22.04 LTS or Debian 12 |
| Go | 1.21 | AAA daemon, API service, migration tooling |
| PostgreSQL | 15 | Primary + synchronous read replica |
| Redis | 7.2 | Sentinel HA cluster (3 nodes) |
| Gotenberg | 8.0 | Invoice PDF generation |
| WhatsApp Business API | Cloud API v17+ | Meta Business Account required; pre-approved templates |
| SMS Gateway | — | Provider configurable (e.g., Twilio, Exotel, MSG91) |
| Docker Engine | 24.0 | |
| Docker Compose | 2.0 | |

---

## 2.4 Requirements Traceability Summary

| FR Group | Count | Primary Doc Owner | Test Coverage |
|---|---|---|---|
| AAA | 7 | DDS §5.1–5.2 | TST INT-AAA-001..005 (extend) |
| Billing & Finance | 7 | DDS §5.4–5.6, MDS §4.3 | TST INT-BIL-001..006 |
| FUP & Session | 5 | DDS §5.3, MDS §4.2 | TST INT-FUP-001..004 |
| Security | 5 | DDS §5.5–5.7 | TST INT-SEC-001..004 |
| Notifications (WhatsApp/SMS/Email/Push) | 13 | MDS §4.7 | TST INT-NOTIF-001..008 (extend) |
| Observability | 5 | SAD §3.2, MDS §4.1 | TST INT-OBS-001..003 |
| CGNAT / Network | 3 | DBD §6.2 | TST INT-NET-001..002 |
| Revenue Assurance | 4 | MDS §4.8 | TST INT-REV-001..004 |
| Subscriber Portal | 5 | MDS §4.9 | TST INT-SUB-001..004 |
| Franchise / LCO | 6 | MDS §4.10 | TST INT-FRN-001..002 (extend) |
| NAS Vendor Support *(v3)* | 4 | MDS §4.1 (extend) | TBD — Module design pending |
| Hotspot / Captive Portal *(v3)* | 3 | new module | TBD — Module design pending |
| Partner / 3rd-Party Integration *(v3)* | 3 | new module | TBD — Module design pending |
| Mobile *(v3)* | 2 | MDS §4.9 (extend) | TBD — Module design pending |
| Helpdesk / SLA *(v3)* | 3 | MDS §4.13 | TBD — Module design complete (2026-08-12), implementation/tests pending |
| CRM / Lead Management *(v3)* | 3 | MDS §4.16 | TST INT-CRM-001..003 |
| Inventory / CPE *(v3)* | 3 | MDS §4.16 | TST INT-INV-001..003 |
| Task & Approval Workflows *(v3)* | 2 | MDS §4.15 | TST INT-WFL-001..003 |
| Announcements *(v3)* | 2 | MDS §4.17 | TST INT-ANN-001..002 |
| General Reporting *(v3)* | 3 | MDS §4.8 (extend) | TBD — Module design pending |
| Document Storage *(v3)* | 1 | new module | TBD — Module design pending |
| CPE Auto-Provisioning *(v3)* | 3 | new module | TBD — Module design pending |
| **Total** | **92** | | |

> The 40 FRs marked *(v3)* have no MDS/DDS/DBD/API design yet and no test
> IDs — they are requirements-stage only. Each gets its own module design
> pass when it's actually scheduled for implementation, the same way
> FR-NOTIF-007 went from a one-line SRS gap to a wired, tested feature in a
> single focused pass rather than everything landing at once.
