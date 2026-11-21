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

## 2.1a Jaze ISP Manager Parity *(new — v4, gap analysis 2026-11-15, CRD §1.13)*

Derived from a 68-screen capture of Jaze ISP Manager compared module by
module against this platform's migrations, API routes and console. Status
legend: ✅ implemented and tested · ⏸ deferred, with the blocking reason ·
unmarked = open. Every console requirement inherits the rule the console
already follows (staffui.go): a screen's role list mirrors the API gate for
the same data, and a hidden control is never the only protection.

### Operations Console *(CRD-EXP-012 — screens over data the backend already holds)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-CON-001 | ✅ Console navigation is grouped into menus (Subscribers, Finance, Service, Network, Admin) plus a Dashboard link; a menu holding one allowed section renders as that section's link. Grouping never grants or hides reach — `AllowedSections` alone decides it — and every section must belong to exactly one menu | CRD-EXP-012 | staffui `render.go` |
| FR-CON-002 | ✅ A home dashboard is the landing screen for every ISP-wide role: online now, active, total with per-status breakdown, expiring in 7 days, expired-not-terminated, joined in 7 days, terminated in 30 days, and — only for roles that can open Support — open, past-SLA and unassigned tickets. Every figure links to the list it counts. "Online" uses the same 30-minute freshness rule as every other reader of `live_sessions` | CRD-EXP-012 | staffui `dashboard.go`, db `console.go` |
| FR-CON-003 | ✅ The subscriber directory shows status, plan, expiry, phone and online state; filters by status and by dashboard drill-down; and searches one box across username and phone/email (substring), id and CAF number (exact), and a live session's assigned IP. An unknown filter is reported and dropped, never widened silently to "everyone"; a search value too large for an id is searched as text, never failing the query | CRD-EXP-012 | staffui `screens.go`, db `console.go` |
| FR-CON-004 | ✅ A Sessions screen lists live sessions (IP, NAS, download/upload from the subscriber's side, speed, FUP flag, duration) and completed sessions over a date range capped at 31 days. Disconnect is limited to `noc_engineer`/`isp_owner`, requires confirmation, and runs the API's own PoD-enqueue and audit path (`api.Handler.RequestDisconnect`) | CRD-EXP-012 | staffui `sessions.go` |
| FR-CON-005 | ✅ A ticket queue spans all subscribers, filterable by status, priority, past-SLA, unassigned and assigned-to-me, with assignment and status change in place; a helpdesk summary shows open, past target, unassigned, closed in 30 days and median time to close (first arrival at resolved/closed, per FR-RPT-001). Assignees are limited to roles the API lets update tickets; a reassignment that leaves the status unchanged does not notify the subscriber | CRD-EXP-012 | staffui `screens.go`, db `console.go` |
| FR-CON-006 | ✅ Invoice and payment lists span all subscribers over a date range (default this month, capped at a year), searchable by subscriber, invoice number or payment reference, with totals over every match (not just the page), invoice PDF download through the API's own renderer, and CSV export with spreadsheet-formula neutralisation. A payment is a subscriber-wallet credit with no staff actor; adjustments and approved credits are excluded | CRD-EXP-012 | staffui `money.go`, db `console.go` |
| FR-CON-007 | ✅ A lead screen shows the pipeline by stage with funnel counts and conversion rate, and allows create, stage change (a reason is required to close as lost) and convert-to-subscriber. Every write runs the API's own rules (`api.Handler.NewLead`/`MoveLead`/`ConvertLeadToSubscriber`), shared with the HTTP routes; conversion is limited to billing admin and owner, as on the API, and creates a `pending_payment` subscriber | CRD-EXP-012 | staffui `leads.go`, api `crm_service.go` |
| FR-CON-008 | ✅ A CPE (TR-069) screen lists managed devices (filters: checked in, silent over 24 h, in fault, all) with subscriber, firmware, last inform and pending RPCs, and lets NOC/technician/owner queue reboot, firmware download (http/https only), parameter read and reprovision, stating that delivery happens at the device's next session (FR-CPE-003). Actions run `api.Handler.QueueCPEAction`, shared with the API routes, which previously had no tests of their own | CRD-EXP-012 | staffui `cpe.go`, api `cpe_service.go` |
| FR-CON-009 | ✅ An announcements screen composes drafts (channels, portal banner, promotional or service class, plan and status segment), shows each draft's current reach before sending, sends with confirmation, and lists past broadcasts with the messages queued. Compose and send run `api.Handler.ComposeAnnouncement`/`BroadcastAnnouncement`, shared with the API, so the atomic draft claim that stops a double-send applies to the console too. Owner and billing admin only, as on the API | CRD-EXP-012 | staffui `announcements.go`, api `announcement_service.go` |
| FR-CON-010 | ✅ Message logs: SMS/WhatsApp/email/push deliveries from `notification_log` (with the failure reason) and, for the owner only, partner webhook deliveries with attempts and response code, filterable by channel, status, date (capped at 31 days) and subscriber or partner. Webhook payloads are never selected | CRD-EXP-012 | staffui `logs.go`, db `console.go` |

### Billing & Catalogue Extensions *(CRD-EXP-013 — small backend additions)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-BIL-012 | ✅ Billing staff can request a credit note against an invoice from the API (POST /invoices/{id}/credit-notes) and the console; it is issued only when a different staff member approves it (approval kind credit_note, migration 053). On approval, one transaction writes the GST document in the invoice's own tax heads and rates (crediting what remains takes the remaining heads exactly, so credits sum to the invoice to the paisa), returns the total to the wallet, and reverses revenue and output GST in the general ledger. Bounded by the migration 049 trigger and an invoice row lock; included in GSTR-1 Table 9B as before | CRD-EXP-013 | billing `credit_note.go`, db `credit_notes.go`, api `credit_notes.go` |
| FR-BIL-013 | ✅ Every payment records its method (online, cash, cheque, bank transfer, UPI, TDS) in payment_details, written in the same transaction as the wallet credit (migration 054, with a backfill of past payments from their descriptions); cheques carry number, bank and date, and a cheque dated over three months ago is refused as stale. Recorded from POST /wallets/recharge (unknown legacy method strings still accepted, without details) and the console's Record payment form; a bank, UPI or gateway reference is the idempotency key, so one transfer recorded twice moves money once. The payments list shows and filters by method | CRD-EXP-013 | billing `payment.go`, db `billing.go`, api `payments.go` |
| FR-BIL-014 | ✅ A cleared cheque can be marked bounced with the bank's reason (API POST /payments/{id}/bounce, console Bounced button), which reverses its wallet credit through the ledger (wallet 1200 debited, Cash / Bank 1000 credited) in one transaction. If the subscriber has already spent the money the bounce is refused and nothing is written, since balances cannot go negative; the operator must collect or adjust first (open decision: whether a spent, bounced cheque should instead create dues and suspend) | CRD-EXP-013 | db `payments.go`, api `payments.go` |
| FR-BIL-015 | ✅ TDS deducted by a business customer is recorded as a payment with its certificate (Form 16A) number, posted against a new TDS Receivable account (GL 1300) instead of Cash / Bank, earns no franchise commission, and is totalled separately from money collected on the payments list | CRD-EXP-013 | billing `payment.go`, migration 054 |
| FR-BIL-016 | ✅ Billing staff and the owner can extend a single lapsed subscriber's grace — service on — until a date up to 30 days out, with a required reason, and end it again, without changing the global schedule. The extension lifts a suspension already in place on the scanner's next pass; when it lapses or is ended the ordinary schedule applies again. Reason, who set it and the date are kept on the subscriber and audited; a subscriber who has paid, or is terminated, cannot be given one | CRD-EXP-013 | billing `dunning_policy.go`, api `grace.go`, migration 055 |
| FR-BIL-017 | ✅ The reminder offsets, grace length and soft-suspension length are stored settings (Finance → Dunning Settings, owner and billing only), validated as a countdown within fixed bounds both in code and by table CHECKs, attributed and audited, and read by the dunning scanner at every pass — including how far ahead it looks for reminders. An unreadable setting falls back to the shipped schedule rather than skipping a pass | CRD-EXP-013 | billing `dunning_policy.go`, staffui `dunning.go`, migration 055 |
| FR-BIL-018 | ✅ Billing staff and the owner can issue a proforma (a quotation for a one-off charge, with GST estimated at today's rate and the subscriber's state, valid 1–90 days) from the API (POST /subscribers/{id}/proformas) and the subscriber's Billing tab, and download it as a PDF titled Proforma Invoice that states it is not a tax invoice. Proformas have their own PF- number series and table (migration 056), so they never take a gap-free INV- number or reach GSTR-1. Once the customer has paid, converting one (POST /proformas/{id}/convert, or Convert on Invoices & Payments → Proformas) debits the wallet, posts the tax to GST Payable as a renewal does, and writes the tax invoice with the proforma's description, all in one transaction, with GST recomputed at the rate in force that day. Conversion is refused when the wallet does not cover the total, when the proforma is past its validity, or when it is already converted or cancelled; a proforma converts once even under concurrent requests. Open proformas can be cancelled with a reason. CSRs can view and download but not issue, convert or cancel. Plan prepayments are recorded as payments rather than converted, since renewals invoice themselves | CRD-EXP-013 | billing `proforma.go`, api `proforma.go`, staffui `proformas.go`, migration 056 |
| FR-BIL-019 | ✅ A Billing Diagnostics screen (Finance menu, owner and billing only) and GET /api/v1/billing/diagnostics run three read-only checks afresh on every load, each list capped at 200 with a count of everything found: (1) active subscribers inside a paid-up cycle on a priced plan with no plan invoice since the cycle began — or two since the previous cycle began, so an early portal renewal is not flagged — which catches plan changes and hand-edited expiry dates; (2) subscribers whose stored wallet balance differs from the sum of their wallet-account ledger legs or from the newest leg's balance-after, largest difference first; (3) subscribers whose newest plan invoice charged a taxable amount other than their plan's catalogue price, ignoring one-off charges. Each row links to the subscriber and explains the usual causes; nothing is changed automatically, and a failed run is never shown as all clear | CRD-EXP-013 | billing `diagnostics.go`, db `diagnostics.go`, staffui `diagnostics.go` |
| FR-BIL-020 | ✅ Every pass of the auto-renewal scanner is recorded (migration 057): start, finish, status (completed, failed when the pass could not run, abandoned when a process stopped mid-pass, detected an hour later), subscribers due, renewed, first cycles activated, short of funds, failed, invoices raised, invoices missing, and amount charged. Each subscriber a pass could not finish is recorded with the step (price, debit, expiry, activation, invoice, dunning) and whether the wallet had already been debited, so a customer charged without being renewed or invoiced is named. Billing staff and the owner read it under Finance → Billing Runs (idle passes hidden by default, a problems-only view, a page per run with what to do for each step) and through GET /api/v1/billing/runs[/{id}]. Recording never holds up renewals: a recording failure is logged and the pass continues | CRD-EXP-013 | billing `billing_runs.go`, db `billing_runs.go`, staffui `billing_runs.go`, migration 057 |
| FR-BIL-021 | ✅ Every recorded payment (each credit to a subscriber's wallet that is not a staff adjustment: gateway, cash, cheque, transfer, UPI or TDS) has a receipt PDF, rendered on demand from the payment's own records by the same browser renderer as invoices: receipt number RCPT-<payment id>, date, the subscriber, the amount in figures and in words (Indian lakh/crore wording), the method with its cheque, TDS-certificate or reference details, the balance it left, who recorded it, and the supplier's name and GSTIN when configured. It states that it is not a tax invoice, a cheque receipt is marked subject to realisation, and a bounced cheque's receipt shows it was returned unpaid and no longer stands. Staff with invoice reach download it from the Payments list and GET /api/v1/payments/{id}/receipt; subscribers see their payments on the portal's Invoices & Payments page and download their own receipts, and are refused anyone else's | CRD-EXP-013 | billing `receipt.go`, db `receipts.go`, api `receipts.go`, portalui |
| FR-BIL-022 | ✅ A product catalogue (migration 058) of one-time and recurring charges — installation, router rental, static IP, add-ons — each with a short code, a GST-exclusive price and its own HSN/SAC code, which is required rather than defaulted since a router is goods and bandwidth is a service. Owner and billing staff manage it under Subscribers → Products & Charges and through /api/v1/products; a CSR may read it but not change it. Retiring a product hides it from new attachments and keeps billing whoever already holds it, as deactivating a plan does (FR-CAT-001); one nothing has ever used can be deleted, one in use cannot. Charges are attached to a subscriber from their Billing tab, with an optional quantity and price override; the price is snapshotted at attachment, so a later catalogue change never re-prices anyone already on it. The auto-renewal scanner bills the plan plus every active charge on the cycle: recurring ones every renewal until staff end them with a reason, one-time ones once — closed in the same transaction as the invoice that billed them, guarded so two concurrent passes cannot bill the same installation fee twice. A failure to read the attached charges stops the renewal rather than billing the plan alone and under-charging silently. Each charge is a separate line on the tax invoice (invoice_lines) with its own HSN/SAC code and its own tax, computed per line and summed — the order GST requires, since each line is its own supply — so the header is the sum of its lines rather than tax on one total, checked at commit by a deferred trigger. A plan-only invoice comes out exactly as before, and invoices raised before migration 058 carry no lines and still print from the header. The invoice PDF prints the itemised table with HSN/SAC, quantity and rate per line, for staff and on the portal. GSTR-1's HSN summary (Table 12) now reports one row per HSN/SAC code instead of filing everything as internet services. **Open decision:** a credit note carries no line detail of its own, so in the HSN summary it reduces the internet-services row rather than the row it actually credited. Correct for every credit note today, since credit notes are raised against plan renewals; wrong the first time one is raised against an invoice carrying goods lines, which needs per-line credit notes to fix | CRD-EXP-014 | billing `product.go`, db `products.go`, api `products.go`, staffui `products.go`, migration 058 |
| FR-BIL-023 | ✅ A plan change can be scheduled from a date (migration 059, at most one pending per subscriber, up to a year ahead) and is applied by the auto-renewal scanner at the first renewal on or after that date — so a date set mid-cycle waits for the renewal rather than firing on the day. The cycle is then bought on the new plan throughout: priced at the new plan's price, debited for that, dated by its validity and invoiced with its name and data allowance, with the invoice linked back to the change. There is no proration, unlike the immediate change (FR-LC-001): the old cycle was paid for and used to its last day, and prorating an end-of-cycle downgrade would hand out bonus days for service already consumed. The move happens only after the wallet debit commits, in one transaction with the new expiry (and with clearing a first cycle's pending_payment status), so a subscriber who cannot pay stays on the plan they have with the change still waiting, and two overlapping passes cannot both apply it. The renewal then drops the cached auth record and pushes a CoA to any live session, so the new rate limit does not wait for the subscriber to reconnect. A failure to apply after the charge is recorded as its own run-failure stage, marked charged (migration 059 widens migration 057's stage CHECK), and Billing Runs says what to do: they paid the new plan's price, are still on the old plan, and the next pass would charge them again. A change naming the plan they already hold is skipped rather than applied. An immediate change (FR-LC-001) in the meantime does not cancel a pending scheduled one: the instruction recorded is “put them on this plan”, so it still fires at the next renewal from wherever they then are — the subscriber's page shows the pending change so staff moving someone immediately can see it and cancel it if that is not what they want. Billing staff and the owner schedule and cancel it from the subscriber's page, with a reason, and through POST /api/v1/subscribers/{id}/scheduled-plan-change and /api/v1/scheduled-plan-changes/{id}/cancel; CSRs can see a pending change but not change it | CRD-EXP-014 | billing `plan_change.go`, db `plan_changes.go`, api `plan_changes.go`, staffui `plan_changes.go`, migration 059 |
| FR-CAT-001 | ✅ Plans can be edited, duplicated, retired and exported as CSV from Finance → Catalogue (migration 061). Editing changes the tariff in place and is attributed and audited; because the renewal path reads plans.price when it bills — unlike a product attachment, which snapshots it (FR-BIL-022) — an edit re-prices every subscriber on the plan from their next renewal, so the screen says how many that is before the change and after it, and a speed change reaches the routers as radiusd's process-local auth cache expires within its 60-second TTL. Retiring hides a plan from every dropdown that assigns one — new subscriber, lead conversion, plan change, scheduled plan change and the bulk action — and leaves the subscribers already on it exactly where they are: the renewal candidate query does not filter on it, so they go on renewing at that plan's price, which is the tariff they bought. The announcement audience filter deliberately still lists retired plans, since the subscribers on one are exactly the audience for a notice about its withdrawal. Plan names are unique among offered plans only, case-insensitively, so the usual way to re-price — retire the old plan, create its replacement under the same name, leave existing customers alone — works, and reinstating a plan whose name has since been taken is explained rather than failing on a constraint. Duplicating copies every billable field under “(copy)”, so varying a tariff is one edit instead of six. The CSV carries every plan, retired ones included, with status, subscriber count, FUP threshold in GB and who last edited it, because the reason to export is usually to reconcile what subscribers are being billed and some of them are on plans no longer sold | CRD-EXP-013 | staffui `plans.go`, db `catalogue.go`, migration 061 |
| FR-OBS-006 | ✅ The audit entries middleware.Audit has always logged are now persisted to staff_audit_log (migration 062) as well — same correlation id, actor, role, action, target and detail, the detail as JSONB so “which plan changes moved somebody to plan 7” is answerable. Wired through a package-level sink rather than a store threaded through the hundred-odd call sites, where one missed site would be a silently unaudited action, and wired in both binaries because a background pass's actions are as much part of the trail as an operator's. Persistence is best-effort and cannot fail the action it records: the log line is written first and unconditionally and remains the authority, the sink gets a short-deadline context of its own so a cancelled handler or a shutting-down scanner still writes its record, and a sink failure is logged loudly and swallowed. An action with no subject on the context is stored as “unknown” rather than empty, so a run of them shows up as the path that is not setting one. Append-only against the role the services actually run as, not by convention: bss_app holds INSERT and SELECT with UPDATE and DELETE revoked, plus insert/select-only RLS policies as defence-in-depth — the pattern migration 019 established for lea_audit_log after discovering that RLS alone never applied to the app's own role. Proven by assuming the bss_app role in a test and confirming it can read and append but cannot update, delete or truncate; re-granting those privileges fails the test. The owner browses it under Admin → Audit Trail, filtered by actor, action and date (the ‘to’ date inclusive), with the detail rendered in sorted key order so a page meant to be evidence does not reshuffle between refreshes, and the page says when the trail is longer than what it is showing. Owner-only, because the trail is a record about the staff as much as about the subscribers | CRD-EXP-013 | middleware `auditsink.go`, db `audit.go`, staffui `audit.go`, migration 062 |
| FR-NET-004 | ✅ Every Access-Request is recorded (migration 063): time, username as supplied, NAS address, MAC from Calling-Station-Id, accepted or rejected, the method that answered (pap, eap or mab) and, on a reject, why — from a fixed vocabulary (unknown_user, bad_password, not_authorised, locked_out, no_mab_device, eap_failed, internal_error) so the column can be grouped and counted rather than read. The reason is a required argument at each of the fourteen outcome sites rather than defaulted, because only the site knows it. **No password is recorded, in any form.** There is no field for one on AuthAttempt and no column for one in the table, and both are asserted — by reflection over the struct and by querying information_schema — so a later “store what the customer typed”, which is what the reference product does, fails a test rather than shipping a credential dump. Written off the hot path: NFR-PERF-001 holds authentication to a 15ms p99 at 5,000 requests a second, so the daemon's cost is one non-blocking channel send into a bounded buffer, drained by a batching goroutine as one multi-row INSERT (not pgx CopyFrom, which would route around haPool's SQLSTATE 25006 failover detection). A full buffer drops the row and counts it rather than making a subscriber wait on their own log entry; a failing store does not stop the writer; shutdown flushes what is queued. Bounded retention of 30 days, purged hourly in batches so one pass cannot hold a long transaction over the largest table in the database. Owner, NOC, CSR and technicians read it under Network → Auth Log — wider than IP Pools deliberately, since “why can't this customer get online” is the commonest support call — filtered by username, result, reason and date, led by a 24-hour summary of rejects by reason with what each usually means | CRD-EXP-013 | radius `authlog.go`, `authlogpurge.go`, db `authlog.go`, staffui `authlog.go`, migration 063 |
| FR-PAY-001 | ✅ Every payment link this system asks the gateway for and every callback the gateway sends is recorded (migration 064): gateway, kind, the gateway's event name, its order and payment ids, the subscriber when one could be matched, the amount in rupees, what this system did about it, and a sentence for a human. “Whether or not it credits a wallet” is taken literally — all six webhook exit paths write a row, including the ones that deliberately do nothing (an event this system does not act on) and the ones where authentication was impossible (no webhook secret set). The order side records a created link and also a failed attempt, since a subscriber who says “it wouldn't let me pay” otherwise leaves no trace at all. A test asserts the coverage path by path; removing any one hook fails it by name. No signature, secret or raw body is stored — authenticity is decided before anything is written, so keeping the signature would preserve a credential-shaped value for no operational gain — and the absence is asserted against information_schema as well as the Go types. There is deliberately no ‘duplicate’ status: WalletService.Recharge is idempotent on transaction_token (FR-BIL-005) and returns the original transaction for a repeated callback with no error and no signal that it was one, so a retry is recorded as credited (which is true) and appears as a second row sharing one payment id, which idx_pgw_payment exists to surface. A status nothing can produce would misdescribe what the system records. Recording never changes what the webhook does: a payment that credited a wallet has credited it whether or not its row was written, and failing the callback would make the gateway retry a credit that already happened. Append-only against the role the services run as, like the audit trail. Billing staff and the owner read it under Finance → Gateway Log, led by a seven-day summary that counts only the outcomes needing attention — money the gateway accepted that reached no wallet — with what to do about each | CRD-EXP-013 | billing `gatewaylog.go`, db `gatewaylog.go`, api `gatewaylog.go`, staffui `gatewaylog.go`, migration 064 |
| FR-NET-005 | ✅ IP pools (migration 060) carry a range, a purpose and exclusions, optionally tied to one router — names unique per router, bounds required to be single hosts so an INET mask cannot make a range ambiguous, and a CIDR block accepted as shorthand for all of its addresses including the network and broadcast ones, which are then excluded explicitly rather than guessed at. Only a 'static' pool is allocated from; 'dhcp' and 'cgnat' pools are recorded so the ranges the router owns and this system must not touch are written down, which is the first question asked when an address collides. A subscriber holds at most one address — Framed-IP-Address carries exactly one, so a second could not be returned — and at most one subscriber holds any address, both as partial unique indexes. Allocation takes the lowest free address (or a named one), skipping exclusions and every address already held *across all pools*, so two pools overlapping by operator error cannot hand out the same address; it runs under a lock on the pool row, so simultaneous allocations get different addresses rather than one failing on a constraint. The address is returned as Framed-IP-Address on every Access-Accept through the one shared path PAP, EAP-MSCHAPv2 and MAB all use, so it cannot depend on how the subscriber authenticated; it is carried on the cached auth record too, or a reconnect within the cache TTL would silently get a dynamic address. An IPv6 address is skipped rather than truncated (Framed-IP-Address is IPv4-only, RFC 2865 §5.8), and a malformed stored address is logged and dropped rather than putting a paying customer offline. Billed through FR-BIL-022: assigning an address may attach a recurring catalogue product, in the same transaction, and releasing the address ends that charge in the same transaction — so an address given back cannot go on being billed. Network engineers and the owner manage pools under Network → IP Pools and through /api/v1/ip-pools; assigning and releasing a subscriber's address is theirs and billing staff's, from the subscriber's page, since the address is a network resource and the charge is a billing one. Every staff role can see the address, because “what is my IP” is a front-desk question | CRD-EXP-014 | nas `ippool.go`, db `ippools.go`, api `ippools.go`, staffui `ippools.go`, radius `handlers.go`, migration 060 |
| FR-CAT-002 | ✅ Bandwidth profiles (migration 065) carry the ceiling, a committed floor, a burst (rate, threshold and duration) and the MikroTik queue priority, stored as bits per second rather than "50M" text so a string parse stays off the authentication hot path. Plans reference one through a nullable `plans.bandwidth_profile_id`: a plan without one shapes from its own `rate_limit_string` exactly as before, which is a supported steady state, not a migration window — hotspot vouchers and the demo seed both create plans that never touch the catalogue. A ceiling-only profile renders byte-for-byte what `rate_limit_string` already held, so adopting one changes no live session. **Only the MikroTik dialect receives the burst form.** The Huawei and ZTE builders turn the string into integer VSAs via `parseRateLimitString`, which splits on the first "/" — hand it the positional burst form and the parse fails, so a plan that merely gained a burst would stop authenticating on every Huawei and ZTE NAS in the estate. `RateProfile` therefore exposes the burst form only through an accessor the MikroTik builder uses, and a test pins both halves of the rule. Editing a profile re-shapes every plan on it from each subscriber’s next authentication; retiring it leaves those plans shaping from it unchanged, and a profile plans still reference cannot be deleted at all. The profile survives the subscriber auth cache, without which a subscriber reconnecting inside the 60-second TTL would silently lose their burst. Precedence is unchanged: a console speed override, then an active FUP throttle, then the plan’s profile — returning a burst alongside a throttle would hand back the speed the throttle exists to remove. 12 unit tests in internal/nas, 1 subscriber-cache test, 6 DB integration tests, 8 console tests and 5 browser tests. | CRD-EXP-013 | catalogue, nas |
| FR-SUP-004 | A subscriber can rate a resolved ticket (1–5) from the portal; ratings appear in ticket lists and the helpdesk summary | CRD-EXP-013 | tickets, portal |
| FR-INV-004 | Inventory and procurement share a vendor master (name, contact, GSTIN) instead of free-text vendor names | CRD-EXP-013 | inventory |
| FR-RPT-004 | A report catalogue groups the reports by area with a filter box, and adds at least: expiring subscribers, payments by method, collection by staff, invoice register, tax charged, top data users, FUP-exceeded, ticket turnaround, and plan change history | CRD-EXP-013 | reporting |

### New Modules *(CRD-EXP-014 — new data model and screens)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NET-006 | NAS interface traffic is polled and graphed (per NAS, last 24 h / 7 d / 30 d) | CRD-EXP-014 | nas (new) |
| FR-DOC-002 | A printable Customer Application Form (CAF) PDF is generated from subscriber and KYC data, using the invoice PDF pipeline | CRD-EXP-014 | documents |
| FR-DOC-003 | ✅ Archived documents are retrieved with their SHA-256 re-verified on every read, and browsed per subscriber. **The verification logic already existed** — `Store.Get`, an `Archiver.Retrieve` that hashes what it read and refuses to return bytes disagreeing with the recorded checksum, and metrics per outcome, all unit-tested since FR-DOC-001. What it had was no production caller at all: only `cmd/radiusd` built an `Archiver`, and only the purge sweep used it, so the archive could delete documents and never read one back. NFR-DUR-002’s gap stayed open while the code to close it sat there, which is exactly the class `scripts/check_wiring.sh` exists for — it now tracks this path. An `Archiver` is wired into `cmd/api`, and invoices are archived when their PDF is rendered, that being the only moment the bytes exist; best-effort, so a subscriber is never refused their invoice because archival storage is full, and idempotent, because `RecordArchive` upserts on (kind, entity) and the storage key derives from the same pair. `document_archives` holds only a doc_kind and an entity_id, so the subscriber is resolved **per kind by joining the table the entity lives in** — which also keeps the list honest, since an archive whose entity was deleted is attributed to nobody rather than to a subscriber it can no longer be tied to. Reports are excluded: a report belongs to the operator who asked for it. Ownership is checked against the row rather than trusted from the caller, so guessing an archive id cannot serve somebody else’s KYC scan, and that check runs **before** retrieval so corruption of a document the caller should not know about is not inferable. A corrupt copy is refused, not served with a warning — an operator who has downloaded a file will use it — and is reported as corrupt rather than as a generic failure, kept distinct from a storage error where the checksum was never compared at all. Verification is also available without downloading, so confirming an archive is intact does not pull a customer’s identity documents onto a laptop. Both retrieval and corruption are audit-logged; a purged document keeps its row (that it was held and disposed of on schedule is the record a retention audit reads) but is not offered for retrieval. Reach is billing and the owner only. Known gap: KYC document archival has no write path, so only invoices appear until one is wired; the screen says so. 5 API tests, 4 DB integration tests, 11 console tests, 3 browser tests, on top of the archive package’s own 6 retrieval tests. Verified against real storage by corrupting nine bytes of an archived 54KB invoice PDF on disk: the download was refused, the operator was told the copy had been altered, and an audit entry recorded both hashes — then restoring the file made it serve again. | CRD-EXP-014 | archive |
| FR-GRP-001 | Subscribers can be placed in named groups used as a filter in lists, reports and announcements | CRD-EXP-014 | subscribers |
| FR-COL-001 | ✅ Collector rounds (migration 068): a subscriber can be assigned to a staff member who collects at the door, and that collector gets a list of their own — soonest expiry first, so the off-service customers are visited before the ones merely approaching renewal — with the phone number, the plan price, the wallet balance and **the amount to ask for**, computed on page load rather than stored because a subscriber can pay online between the list being drawn and the visit. A subscriber whose wallet already covers the plan stays on the list marked *already paid* rather than vanishing, since a row that disappears between refreshes reads as a bug to whoever is working the round. Terminated subscribers are excluded outright. **The assignment is a permission, not a convenience.** Field collectors are not billing staff, so rather than widening the payment gate to them — which would let any technician record a payment for any subscriber from the subscriber page — a collector may record only for subscribers on their own round, checked server-side; owner and billing record unscoped, as they already can. Typing another collector’s id into the URL shows a technician their own round, not that one. There is deliberately **no second money path**: a collection is the existing wallet credit, so the method, the cheque number/bank/date, the stale-cheque refusal and `recorded_by` all come from FR-BIL-013 unchanged, and no collections ledger exists to disagree with the receipt the customer was given. Reconciliation covers everyone who recorded a payment, not only field collectors, because a day’s cash has to be accounted for at the counter as well as on the round; it sits on the same screen rather than in a report nobody opens — cash collection without it is where money goes missing — and separates **cash, which has to be handed in, from UPI, which has already reached the bank**; refunds, reversals and bounced cheques are excluded — keyed on `entry_type` and `payment_details.status`, not on the sign of the amount, because wallet_ledgers stores both directions positive and a bounce leaves its original credit in place. Takings group by `recorded_by` text, so deleting a staff account cannot erase a cash trail, and a departing collector’s subscribers fall back to unassigned instead of blocking the deletion. Known gap: `subscribers` has no address column, so a round is worked from the phone number — adding an address is its own change. 4 domain tests, 8 DB integration tests, 4 API tests covering the scoping refusal, 11 console tests, 5 browser tests including one that takes cash at the door and checks it reached the wallet. | CRD-EXP-014 | billing, workflow |
| FR-HSP-004 | The captive portal's logo, colours and text are configurable per deployment without editing templates | CRD-EXP-014 | hotspot |
| FR-CAT-003 | ✅ Reusable fair-use policies with up to 8 tiers (migration 067), each a cumulative threshold and a flat throttle rate, plus purchasable data top-ups. **The highest crossed tier applies, not the first** — a subscriber at triple quota has crossed every threshold, and a gentlest-wins reading would make every harder tier unreachable. Thresholds are cumulative rather than incremental because that is what the scanner compares against and what an operator reads off a tariff sheet. **A top-up raises the thresholds rather than clearing a flag.** Clearing a flag is the tempting shortcut and is wrong: it would leave the subscriber un-throttled for the rest of the cycle however much more they used, because nothing moved the thresholds and every tier above them would be unreachable. Raising them keeps all tiers live, so somebody who buys 50GB and uses 50GB more is throttled again. Top-ups expire with the cycle and do not roll over. The scanner’s question changed from “who is over quota and not yet throttled” to “whose applied rate is wrong”, recorded in a new `subscribers.fup_applied_rate`. The old boolean could not express *which* of several tiers was applied, so a subscriber crossing a second threshold was skipped as “already throttled” and the harder tier never arrived — reproduced by a negative control. One comparison now covers crossing a tier, tightening, easing after a top-up and releasing. The migration backfills the column for already-throttled subscribers, so a deploy does not emit a burst of redundant CoAs on its first tick. A manual un-throttle clears the applied rate, without which `GetSubscriberNASSession` would keep sending it and the un-throttle would silently do nothing. A speed override still outranks fair use. The 80% warning (FR-FUP-004) measures against the policy’s **first** tier, shifted by top-ups. Plans without a policy keep their own single threshold, a supported steady state. 12 tests in internal/fup (10 domain, 2 scanner), 9 DB integration tests, 9 console tests, 4 browser tests. | CRD-EXP-014 | fup |
| FR-CAT-004 | ✅ Per-plan, per-vendor reply attributes (migration 066), validated against a dictionary in `internal/nas/dictionary.go` rather than stored as free text. The reason is that **RADIUS is silent about attributes a NAS does not understand**: a misspelled name or a wrongly typed value is ignored on arrival, leaving a plan that looks configured and does nothing — the same silence `internal/nas` was created to stop reproducing. Each definition carries its wire encoding (standard RFC type, or vendor id plus sub-attribute), a value kind that is checked and encoded before storage, and operator-facing help, so the console refuses at entry. **Attributes a bandwidth builder already sends are reserved** (`Mikrotik-Rate-Limit`, Juniper’s `Filter-Id`, the Huawei/ZTE rates, the wireless roles, and the two Cisco QoS AVPair keys): two attributes of one type in a packet is undefined behaviour, so a plan could otherwise deliver a speed other than its profile’s. They are listed with their reason rather than hidden, since an operator who cannot see why one is missing assumes the dictionary is incomplete. The ceiling on a value is 253 bytes for a standard attribute but **247 for a vendor-specific one**, because the VSA envelope spends six — checking 253 for both would store a value that fails at encode time, which is a packet the subscriber never receives. Loaded and encoded on `nas.Resolver`’s 60-second refresh, so the authentication path copies a slice rather than parsing, and a bad stored value costs one log line a minute instead of one per authentication; one unsendable attribute never costs a subscriber their others or their bandwidth attribute. An attribute may be switched off without losing its value, and deleting a plan takes its attributes with it (CASCADE, unlike the RESTRICT on a shared bandwidth profile). Reach is the Routers tier, not Catalogue: an address list carries no price. 14 unit tests in internal/nas, 7 RADIUS tests, 7 DB integration tests, 10 console tests, 4 browser tests. | CRD-EXP-014 | nas |

### Deferred — blocked outside this codebase *(CRD-EXP-015)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-CAT-005 | ⏸ OTT, IPTV and voice packages. **Billing-only add-ons are covered by FR-BIL-022.** Provisioning needs an aggregator / IPTV-middleware / SIP-provider contract and its API; there is nothing to integrate against until one is chosen | CRD-EXP-015 | — |
| FR-NET-007 | ⏸ OLT/ONU monitoring (optical power, uptime). Every OLT make has its own SNMP MIB or CLI; needs the vendor and model list actually deployed | CRD-EXP-015 | — |
| FR-BIL-024 | ⏸ Postpaid ("pay later") billing with credit limits. The platform is prepaid by design (FR-BIL-009); needs an owner decision on whether postpaid customers exist | CRD-EXP-015 | — |
| FR-BIL-025 | ⏸ GST e-invoice IRN generation. Needs a GSP/IRP account and applies only to B2B invoicing above the turnover threshold | CRD-EXP-015 | — |
| FR-NET-008 | ⏸ Off-machine copy of NAT/flow logs and archived documents to S3-compatible storage. The `archive.Store` interface is the seam; needs a bucket and credentials | CRD-EXP-015 | archive |

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
| NFR-DUR-002 | Durability | An archived document's SHA-256 is computed from the bytes actually written, not the bytes offered, so a truncated or corrupted write is detectable against the stored checksum; a document must never be purged before its `retain_until` date | Integration test: archive a document and confirm the recorded checksum matches the stored file's actual hash; attempt a purge before `retain_until` and confirm the DB constraint (`chk_archive_not_purged_before_retention`) rejects it. **Gap closed 2026-11-21 (FR-DOC-003).** When this NFR was drafted the archive `Store` had `Put`/`Delete` only and there was no code path to read a document back. `Store.Get` and `Archiver.Retrieve` were added with FR-DOC-001 and verify on read, but had no production caller until FR-DOC-003 wired an `Archiver` into `cmd/api` and put retrieval behind the console’s Documents screen — so for a time the capability existed and the gap was still real. Verify-on-read is now reachable, every read goes through the comparison, and `scripts/check_wiring.sh` tracks the path so it cannot quietly lose its caller again. | — | TST §13.12 |
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
| Jaze parity — Operations Console *(v4)* | 10 | staffui | `internal/staffui/*_test.go`, `internal/db/console_integration_test.go`, `e2e/staff.spec.ts` |
| Jaze parity — Billing & Catalogue extensions *(v4)* | 18 | billing, catalogue, others | Per FR as implemented |
| Jaze parity — New modules *(v4)* | 11 | new modules | Per FR as implemented |
| Jaze parity — Deferred *(v4)* | 5 | — | None until unblocked |
| **Total** | **143** | | |

> Recounted 2026-11-15 from the table rows themselves (`^| FR-…-NNN |`):
> 99 before the v4 group, 143 after. The previous "92" had drifted — several
> groups above (Notifications, Franchise, Hotspot) grew without this table
> being updated, so the per-group counts above it are indicative only; the
> row count is the figure to trust.

> The 40 FRs marked *(v3)* have no MDS/DDS/DBD/API design yet and no test
> IDs — they are requirements-stage only. Each gets its own module design
> pass when it's actually scheduled for implementation, the same way
> FR-NOTIF-007 went from a one-line SRS gap to a wired, tested feature in a
> single focused pass rather than everything landing at once.
