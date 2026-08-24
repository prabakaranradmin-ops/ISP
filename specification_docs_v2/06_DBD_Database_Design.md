# Document 6: Database Design Document (DBD)
**Version:** 2.2 | **Status:** Draft | **Date:** 2026-08-12 — §6.2 SLA engine tables added, `tickets` altered (CRD §1.11 Phase 3); §6.2 Phase 2 tables/§6.6 unchanged from v2.1, rest unchanged from v2.0
**Document ID:** DBD
**Traces From:** [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md)
**Traces To:** [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 6.1 Entity-Relationship Overview

```
plans (1) ─────────── (N) subscribers
subscribers (1) ─────── (N) invoices
subscribers (1) ─────── (N) wallet_ledgers
subscribers (1) ─────── (N) subscriber_session_history  [partitioned monthly]
subscribers (1) ─────── (N) kyc_verifications
subscribers (1) ─────── (N) cgnat_allocations           [partitioned monthly]
subscribers (1) ─────── (N) tickets
subscribers (1) ─────── (N) notification_log            [NEW — FR-NOTIF-009]
subscribers (1) ─────── (N) usage_snapshots             [NEW — FR-SUB-001]
franchises  (1) ─────── (N) subscribers                 [NEW — FR-FRN-001]
franchises  (1) ─────── (N) lco_ledger                  [NEW — FR-FRN-002]
encryption_keys (1) ─── (N) kyc_verifications
notification_templates  ─── (N) notification_log        [NEW — FR-NOTIF-010]
revenue_snapshots ─────────── (standalone — FR-REV-001) [NEW]
collections_forecast ──────── (standalone — FR-REV-004) [NEW]
lea_audit_log ─────────────── (append-only — FR-OBS-003)
nas_devices (1) ─────── (N) plan_nas_profiles  [v3 — FR-NAS-002]
plans       (1) ─────── (N) plan_nas_profiles  [v3 — FR-NAS-001]
encryption_keys (1) ─── (N) nas_devices        [v3 — FR-NAS-002, secret encrypted at rest]
tickets     (1) ─────── (N) sla_events         [v3 — FR-SUP-002, append-only]
subscribers (1) ─────── (N) subscriber_status_history [NEW — FR-RPT-001, append-only, trigger-written]
tickets     (1) ─────── (N) ticket_status_history     [NEW — FR-RPT-001, append-only, trigger-written]
api_keys    (1) ─────── (N) webhook_endpoints         [NEW — FR-API-001..002]
webhook_endpoints (1) ─ (N) webhook_deliveries        [NEW — FR-API-003, audit trail]
franchises  (1) ─────── (N) ticket_routing_rules  [v3 — FR-SUP-003, franchise_id nullable]
staff_users (1) ─────── (N) tickets            [v3 — FR-SUP-003, assigned_to — the FK migration 009 promised and never added]
franchises  (1) ─────── (N) tickets            [v3 — FR-SUP-003, denormalized from subscribers.franchise_id]
sla_policies, category_priority_defaults, ticket_routing_rules ── (standalone lookup tables — FR-SUP-001..003)
```

> `nas_devices` is deliberately **not** a hard FK target of
> `subscriber_session_history.nas_ip_address` — see the note on that column
> below. The relationship is a runtime lookup by IP value, not a referential
> constraint, so accounting from an unregistered NAS is never rejected.

---

## 6.2 Data Dictionary

### Table: `plans`
**FR:** FR-AAA-004, FR-FUP-001 | **Module:** MOD-AAA, MOD-FUP

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `name` | `VARCHAR(100)` | NOT NULL | |
| `rate_limit_string` | `VARCHAR(50)` | NOT NULL | MikroTik format: `100M/100M` |
| `volume_gb` | `INTEGER` | NOT NULL | Included data volume |
| `fup_threshold_bytes` | `BIGINT` | DEFAULT 0 | Pre-computed byte cap; 0 = unlimited |
| `fup_throttle_string` | `VARCHAR(50)` | NULLABLE | Post-FUP rate limit |
| `price` | `NUMERIC(12,2)` | NOT NULL | Base price excl. tax |
| `validity_days` | `INTEGER` | NOT NULL | |
| `franchise_id` | `INTEGER` | FK → franchises.id, NULLABLE | NULL = all franchises |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `subscribers`
**FR:** FR-AAA-001..004, FR-BIL-001, FR-FRN-001 | **Module:** MOD-AAA, MOD-BIL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `caf_number` | `VARCHAR(50)` | UNIQUE NOT NULL | Official CAF registration index |
| `username` | `VARCHAR(100)` | UNIQUE NOT NULL | PPPoE/IPoE username |
| `password_hash` | `TEXT` | NOT NULL | bcrypt(cost=12); never plaintext |
| `mobile_number` | `VARCHAR(20)` | NOT NULL | E.164: `+91XXXXXXXXXX` |
| `email` | `VARCHAR(255)` | NULLABLE | |
| `plan_id` | `INTEGER` | FK → plans.id | |
| `franchise_id` | `INTEGER` | FK → franchises.id, NULLABLE | LCO owner; NULL = direct subscriber |
| `status` | `VARCHAR(20)` | NOT NULL | `active`, `grace_period`, `soft_suspended`, `hard_suspended`, `terminated` |
| `dunning_state` | `VARCHAR(20)` | NOT NULL DEFAULT 'active' | Maps to dunning state machine |
| `wallet_balance` | `NUMERIC(12,2)` | DEFAULT 0.00 | |
| `ipv4_address` | `INET` | NULLABLE | Static; NULL = dynamic |
| `registered_state` | `VARCHAR(10)` | NOT NULL | ISO state code for GST routing |
| `dnd_opt_out` | `BOOLEAN` | DEFAULT FALSE | TRAI DND flag |
| `kyc_status` | `VARCHAR(20)` | DEFAULT 'pending' | |
| `plan_expiry` | `TIMESTAMPTZ` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `franchises` *(new — FR-FRN-001)*
**Module:** MOD-FRN

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `name` | `VARCHAR(100)` | NOT NULL | LCO / franchise name |
| `owner_name` | `VARCHAR(100)` | NOT NULL | |
| `mobile_number` | `VARCHAR(20)` | NOT NULL | |
| `commission_rate_pct` | `NUMERIC(5,2)` | NOT NULL | Commission % per recharge |
| `status` | `VARCHAR(20)` | DEFAULT 'active' | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `lco_ledger` *(new — FR-FRN-002)*
**Module:** MOD-FRN

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `franchise_id` | `INTEGER` | FK → franchises.id NOT NULL | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | Subscriber whose recharge triggered this |
| `recharge_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `commission_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `transaction_ref` | `VARCHAR(100)` | | Links to wallet_ledgers.transaction_token |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `notification_log` *(new — FR-NOTIF-009)*
**Module:** MOD-NOTIF

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id NOT NULL | |
| `channel` | `VARCHAR(20)` | NOT NULL | `whatsapp`, `sms`, `email` |
| `template_id` | `VARCHAR(20)` | FK → notification_templates.id NULLABLE | e.g. `TMPL-001` |
| `triggered_by_event` | `VARCHAR(50)` | NOT NULL | e.g. `fup_warning`, `dunning_remind_7d` |
| `triggered_by_entity_id` | `INTEGER` | NULLABLE | e.g. invoice ID, session ID |
| `sent_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `delivery_status` | `VARCHAR(20)` | NOT NULL DEFAULT 'sent' | `sent`, `delivered`, `read`, `failed`, `suppressed_dnd` |
| `failure_reason` | `TEXT` | NULLABLE | Provider error message |
| `provider_message_id` | `VARCHAR(100)` | NULLABLE | WhatsApp message ID for callback matching |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | Updated on delivery status callback |

### Table: `notification_templates` *(new — FR-NOTIF-010)*
**Module:** MOD-NOTIF

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `VARCHAR(20)` | PK | e.g. `TMPL-001` |
| `channel` | `VARCHAR(20)` | NOT NULL | `whatsapp`, `sms`, `email` |
| `template_name` | `VARCHAR(100)` | NOT NULL | Meta-approved template name |
| `event_trigger` | `VARCHAR(50)` | NOT NULL | e.g. `fup_warning` |
| `variables_schema` | `JSONB` | | Variable names and order |
| `active` | `BOOLEAN` | DEFAULT TRUE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `invoices`
**FR:** FR-BIL-001..002, FR-BIL-007 | **Module:** MOD-BIL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `base_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `cgst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `sgst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `igst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `total_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `gst_rate_id` | `INTEGER` | FK → gst_rates.id | |
| `gb_included` | `INTEGER` | NOT NULL | Plan volume for usage summary on invoice |
| `gb_used` | `NUMERIC(10,2)` | NOT NULL | Actual usage for plain-language summary (FR-BIL-007) |
| `pdf_path` | `TEXT` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `CONSTRAINT chk_gst_logic` | | `(cgst_amount>0 AND igst_amount=0) OR (igst_amount>0 AND cgst_amount=0) OR (cgst_amount=0 AND igst_amount=0)` | |

### Table: `wallet_ledgers`
**FR:** FR-BIL-003, FR-BIL-005, FR-BIL-009..011, FR-REV-002 | **Module:** MOD-BIL, MOD-BILLC

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | For LCO tracking |
| `account` | `VARCHAR(40)` | NOT NULL DEFAULT `subscriber_wallet` | `subscriber_wallet`, `payment_gateway_clearing`, `revenue_clearing` *(new — migration 025)*, `adjustment_clearing` *(new — migration 025)* |
| `entry_type` | `VARCHAR(20)` | NOT NULL | `credit`, `debit` |
| `amount` | `NUMERIC(12,2)` | NOT NULL | |
| `balance_after` | `NUMERIC(12,2)` | NOT NULL | Running balance snapshot |
| `transaction_token` | `VARCHAR(100)` | UNIQUE NULLABLE | Idempotency key |
| `description` | `TEXT` | | |
| `adjusted_by_username` | `VARCHAR(100)` | NULLABLE *(new — migration 025)* | Staff username for adjustment/refund legs; NULL for recharge, auto-renewal and other non-staff-initiated postings |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

`subscribers.wallet_balance` gains `CONSTRAINT chk_wallet_balance_nonneg
CHECK (wallet_balance >= 0)` *(new — migration 025)* — the hard backstop
behind `WalletService.Post`'s application-level balance check (MDS §4.14).

### Table: `payment_refunds` *(new — FR-BIL-011, migration 025)*
**Module:** MOD-BILLC

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id NOT NULL | |
| `ledger_entry_id` | `INTEGER` | FK → wallet_ledgers.id NOT NULL | The wallet debit leg this refund posted |
| `amount` | `NUMERIC(12,2)` | NOT NULL CHECK > 0 | |
| `reason` | `TEXT` | NOT NULL | |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `processed` CHECK IN (`requested`,`processed`,`failed`) | This deployment has no live gateway refund API, so every refund is written as `processed` at creation; the column exists so a future asynchronous gateway refund can move through the full lifecycle without a schema change |
| `refunded_by_username` | `VARCHAR(100)` | NOT NULL | Staff attribution |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `subscriber_session_history`
**FR:** FR-NET-001..003 | Partitioned monthly on `start_time`

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `subscriber_id` | `INTEGER` FK | |
| `session_id` | `VARCHAR(255)` | RADIUS Acct-Session-Id |
| `nas_ip_address` | `INET` | Looked up against `nas_devices.ip` at runtime for vendor/secret resolution (FR-NAS-002) — not a hard FK; accounting from an unregistered NAS must never be rejected |
| `assigned_ipv4` | `INET` NULLABLE | |
| `assigned_ipv6_prefix` | `CIDR` NULLABLE | IPv6 PD |
| `start_time` | `TIMESTAMPTZ` | Partition key |
| `stop_time` | `TIMESTAMPTZ` NULLABLE | NULL = active |
| `input_octets` | `BIGINT` DEFAULT 0 | |
| `output_octets` | `BIGINT` DEFAULT 0 | |
| `terminate_cause` | `VARCHAR(50)` NULLABLE | |

### Table: `nas_devices` *(new — FR-NAS-002, v3)*
**Module:** MOD-NAS | Migration `022_create_nas_devices.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `ip` | `INET` | UNIQUE NOT NULL | Source IP the NAS sends Access-Requests/Accounting from |
| `vendor` | `VARCHAR(20)` | NOT NULL CHECK IN (`mikrotik`,`huawei`,`zte`,`cisco`,`juniper`,`cisco_wlc`,`aruba`,`ruckus`) | Selects the attribute builder (MDS §4.11). Corrected from this doc's original 6-value list (with one bucketed `wireless_generic`) during implementation: Cisco WLC (vendor 14179, Airespace-derived), Aruba (14823) and Ruckus each need their own attribute encoding and cannot share a builder — see migration `022_create_nas_devices.sql`'s header comment |
| `description` | `VARCHAR(100)` | NULLABLE | e.g. "POP-Chennai-Anna-Nagar edge router" |
| `radius_secret_encrypted` | `TEXT` | NOT NULL | `{key_version}:{base64(nonce+ct)}` — same AES-GCM-256 pattern as `kyc_verifications`; a RADIUS shared secret is a credential and must not sit in plaintext next to 500 other NAS rows |
| `key_version_id` | `VARCHAR(10)` | NOT NULL, FK → `encryption_keys.version_id` | |
| `coa_port` | `INTEGER` | NOT NULL DEFAULT 1700 | MikroTik's default (matches current hardcoded behavior); RFC 5176's standard default is 3799 — override per device, this is a known cross-vendor mismatch, not a typo |
| `pod_port` | `INTEGER` | NOT NULL DEFAULT 1700 | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

An IP with no row here is not an error — `internal/nas` falls back to vendor
`mikrotik` and the existing global `RADIUS_SECRET` (MDS §4.11 rollout note),
so this table starts empty in an upgraded deployment and fills in over time
as non-MikroTik NAS devices are registered, rather than requiring a
big-bang backfill before the release can ship.

### Table: `plan_nas_profiles` *(new — FR-NAS-001, v3)*
**Module:** MOD-NAS | Migration `022_create_nas_devices.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `plan_id` | `INTEGER` | NOT NULL, FK → `plans.id` | |
| `vendor` | `VARCHAR(20)` | NOT NULL | Same enum as `nas_devices.vendor` |
| `profile_name` | `VARCHAR(100)` | NOT NULL | Pre-provisioned NAS-side QoS policy/profile name this plan maps to for reference-model vendors (MDS §4.11) |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `CONSTRAINT uq_plan_vendor` | | UNIQUE (`plan_id`, `vendor`) | One profile mapping per plan per vendor |

Only needed for reference-model vendors (Cisco, Juniper, wireless — MDS
§4.11); dynamic-rate vendors (MikroTik, Huawei, ZTE) derive their attribute
directly from `plans.rate_limit_string` and need no row here. A
reference-vendor NAS whose plan has no matching row here is an
`nas_attribute_build_errors_total` metric increment, not a silent no-op —
see MDS §4.11.

### Table: `tickets`
**FR:** FR-SUB-004, FR-SUP-001..003 (v3) | **Module:** MOD-PORTAL, MOD-SUP

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `category` | `VARCHAR(50)` | NOT NULL | `connectivity`, `billing`, `plan_change`, `other` |
| `description` | `TEXT` | NOT NULL | |
| `status` | `VARCHAR(20)` | DEFAULT 'open' | `open`, `in_progress`, `resolved`, `closed` |
| `assigned_to` | `INTEGER` | FK → staff_users.id NULLABLE | **Correction, v3:** this doc previously said `FK → admin_users.id`, matching migration 009's own comment ("FK to admin_users.id added in future migration"). Neither was ever true — `admin_users` never existed, no FK was ever added, and `assigned_to` has been a bare unconstrained integer since migration 009. `staff_users` (migration 021) is the real staff table; migration 023 (below) adds the FK that should have existed from the start |
| `priority` | `VARCHAR(20)` | NOT NULL DEFAULT 'medium' | `low`, `medium`, `high`, `critical` — *(new, v3)*. Category-derived by default (`category_priority_defaults`), staff-overridable, never set directly by a subscriber (MDS §4.13) |
| `sla_response_due_at` | `TIMESTAMPTZ` | NULLABLE | *(new, v3)* — breached if `status` is still `open` once this passes |
| `sla_resolution_due_at` | `TIMESTAMPTZ` | NULLABLE | *(new, v3)* — breached if `status` is not `resolved`/`closed` once this passes |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | *(new, v3)* — denormalized from `subscribers.franchise_id` at creation, same pattern `wallet_ledgers.franchise_id` already uses, for routing-rule matching (MDS §4.13) without a join on every ticket read |
| `routed_role` | `VARCHAR(20)` | NULLABLE | *(new, v3)* — snapshot of the matching `ticket_routing_rules.target_role` at creation time, not recomputed if rules change later (MDS §4.13) |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `category_priority_defaults` *(new — FR-SUP-001, v3)*
**Module:** MOD-SUP | Migration `023_create_sla_engine.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `category` | `VARCHAR(50)` | PK | Matches `tickets.category`'s CHECK values |
| `default_priority` | `VARCHAR(20)` | NOT NULL | `low`, `medium`, `high`, `critical` |

Seeded with one row per `tickets.category` value — a category with no row
here is a configuration gap the ticket-creation insert should fail loudly
on (MDS §4.13), not silently default to something. Seed suggestion (not
prescriptive — an ops decision, not a schema one): `connectivity` → `high`
(no service is the most urgent category by default), `billing` →
`medium`, `plan_change` → `low`, `other` → `low`.

### Table: `sla_policies` *(new — FR-SUP-001, v3)*
**Module:** MOD-SUP | Migration `023_create_sla_engine.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `category` | `VARCHAR(50)` | NOT NULL | |
| `priority` | `VARCHAR(20)` | NOT NULL | |
| `response_minutes` | `INTEGER` | NOT NULL | Time to first response (ticket leaving `open`) |
| `resolution_minutes` | `INTEGER` | NOT NULL | Time to resolution (`resolved`/`closed`) |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `CONSTRAINT uq_sla_category_priority` | | UNIQUE (`category`, `priority`) | |

Keyed on the pair, not `priority` alone — a critical connectivity outage
and a critical billing dispute plausibly deserve different resolution
windows even at the same priority label (MDS §4.13). A ticket whose
`(category, priority)` has no row here is the same class of configuration
gap as a missing `category_priority_defaults` row — the insert should fail,
not create a ticket with no SLA at all.

### Table: `sla_events` *(new — FR-SUP-002, v3)*
**Module:** MOD-SUP | Migration `023_create_sla_engine.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `ticket_id` | `INTEGER` | NOT NULL, FK → tickets.id | |
| `event_type` | `VARCHAR(30)` | NOT NULL CHECK IN (`response_warning`,`response_breach`,`resolution_warning`,`resolution_breach`) | |
| `occurred_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `CONSTRAINT uq_sla_event` | | UNIQUE (`ticket_id`, `event_type`) | |

Append-only, same shape as `notification_log` and `lea_audit_log` — and the
uniqueness constraint on `(ticket_id, event_type)` **is** the SLA scanner's
idempotency mechanism (MDS §4.13): attempt the insert, act on the alert
only if a row was actually written, rather than a separate boolean column
per event type on `tickets` itself.

### Table: `ticket_routing_rules` *(new — FR-SUP-003, v3)*
**Module:** MOD-SUP | Migration `023_create_sla_engine.sql`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `category` | `VARCHAR(50)` | NULLABLE | NULL matches any category |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | NULL matches any franchise (including none) |
| `target_role` | `VARCHAR(20)` | NOT NULL CHECK IN (`isp_owner`,`noc_engineer`,`billing_admin`,`csr`,`technician`) | Same role enum `staff_users.role` already uses |
| `priority_order` | `INTEGER` | NOT NULL DEFAULT 100 | Lower matches first; explicit precedence, not inferred from how many columns are non-null (MDS §4.13) |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

Routes to a role, not a specific `staff_users` row — auto-assigning to an
individual would need a workload/availability model nothing in this schema
tracks (MDS §4.13). A ticket matching no rule leaves `tickets.routed_role`
null; it sits in the general queue rather than failing ticket creation —
unlike a missing SLA policy, an unrouted ticket is a normal, expected
outcome (not every category/franchise combination needs an explicit rule),
not a configuration error.

### Table: `approval_requests` *(new — FR-WFL-001, migration 026)*
**Module:** MOD-WFL | MDS §4.15

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `action_type` | `VARCHAR(30)` | NOT NULL CHECK IN (`wallet_credit`,`refund`,`terminate`) | |
| `subscriber_id` | `INTEGER` | NOT NULL, FK → subscribers.id | |
| `amount` | `NUMERIC(12,2)` | NULLABLE | Required (>0) for `wallet_credit`/`refund`; forbidden for `terminate` — `chk_approval_amount_by_type` |
| `reason` | `TEXT` | NOT NULL | |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `pending` CHECK IN (`pending`,`approved`,`rejected`,`executed`,`execution_failed`) | `approved` is the atomic-claim state between decision and execution completing (MDS §4.15) |
| `requested_by_username` | `VARCHAR(100)` | NOT NULL | |
| `decided_by_username` | `VARCHAR(100)` | NULLABLE | |
| `decision_reason` | `TEXT` | NULLABLE | Required by the API for a reject; not asked of an approve |
| `execution_error` | `TEXT` | NULLABLE | Set only when status = `execution_failed` |
| `ledger_entry_id` | `INTEGER` | FK → wallet_ledgers.id NULLABLE | Set on successful `wallet_credit`/`refund` execution |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `decided_at` | `TIMESTAMPTZ` | NULLABLE | |
| `CONSTRAINT chk_approval_distinct_approver` | | `decided_by_username IS NULL OR decided_by_username <> requested_by_username` | The second-approver guarantee, enforced at the schema level as well as in the handler |
| `CONSTRAINT chk_approval_amount_by_type` | | see Amount above | |

### Table: `field_tasks` *(new — FR-WFL-002, migration 026)*
**Module:** MOD-WFL | MDS §4.15

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `title` | `VARCHAR(200)` | NOT NULL | |
| `description` | `TEXT` | NULLABLE | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id NULLABLE | Not every ad hoc task is about one subscriber |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | |
| `assigned_to_username` | `VARCHAR(100)` | NOT NULL | |
| `created_by_username` | `VARCHAR(100)` | NOT NULL | |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `open` CHECK IN (`open`,`in_progress`,`completed`,`cancelled`) | |
| `due_date` | `DATE` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | `set_updated_at()` trigger (migration 003) |
| `completed_at` | `TIMESTAMPTZ` | NULLABLE | Set when status transitions to `completed` |

Deliberately independent of `tickets` — CRD-EXP-002 asks for task assignment
"independent of the ticket system," and the two serve different audiences
(subscriber-facing support vs. internal staff coordination) with different
lifecycles (no SLA engine, no routing rules here).

### Table: `announcements` *(new — FR-ANN-001..002, migration 028)*
**Module:** MOD-NOTIF | MDS §4.17

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `title` | `VARCHAR(200)` | NOT NULL | |
| `body` | `TEXT` | NOT NULL | |
| `channels` | `TEXT[]` | NOT NULL CHECK non-empty, each ∈ (`whatsapp`,`sms`,`email`,`push`) | Which dispatched channels to fan out to; the portal banner is separate (below) |
| `class` | `VARCHAR(20)` | NOT NULL DEFAULT `marketing` CHECK IN (`marketing`,`transactional`) | Marketing is the default so DND opt-out is honoured unless a human deliberately says otherwise |
| `segment_franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | NULL = all franchises |
| `segment_plan_id` | `INTEGER` | FK → plans.id NULLABLE | NULL = all plans |
| `segment_status` | `VARCHAR(20)` | NULLABLE | NULL = all statuses |
| `show_in_portal` | `BOOLEAN` | NOT NULL DEFAULT FALSE | Banner display; not a `notification_log` channel because nothing is transmitted |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `draft` CHECK IN (`draft`,`sending`,`sent`,`failed`) | `sending` is the atomic claim that stops a double-click broadcasting twice |
| `recipient_count` | `INTEGER` | NOT NULL DEFAULT 0 | Written back on completion — the operator's receipt |
| `created_by_username` | `VARCHAR(100)` | NOT NULL | |
| `created_at` / `sent_at` | `TIMESTAMPTZ` | DEFAULT NOW() / NULLABLE | |

Area targeting from CRD-EXP-002 is deliberately absent: `subscribers` carries
no address or region column, so an "area" filter would have to be
approximated by something that looks like area targeting and is not (MDS §4.17).

### Table: `subscriber_push_tokens` *(new — FR-NOTIF-013, FR-MOB-001, migration 028)*
**Module:** MOD-NOTIF | MDS §4.17

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | NOT NULL, FK → subscribers.id ON DELETE CASCADE | |
| `token` | `VARCHAR(255)` | NOT NULL UNIQUE | Provider device token; unique so a re-registering device updates rather than duplicates |
| `platform` | `VARCHAR(20)` | NOT NULL CHECK IN (`ios`,`android`,`web`) | |
| `created_at` / `last_seen_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

A table rather than a column on `subscribers`: one subscriber routinely has
several devices, and this is the same storage FR-MOB-001 needs.

`notification_log.channel`'s CHECK is widened to include `push`
*(migration 028)* — it has allowed `whatsapp`, `sms` and `email` since
migration 008.

### Table: `leads` *(new — FR-CRM-001..003, migration 027)*
**Module:** MOD-CRM | MDS §4.16

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `full_name` | `VARCHAR(200)` | NOT NULL | |
| `mobile_number` | `VARCHAR(20)` | NOT NULL | E.164, same `chk_leads_mobile_e164` rule as subscribers/franchises (migration 020) |
| `email` | `VARCHAR(255)` | NULLABLE | |
| `source` | `VARCHAR(50)` | NOT NULL | `walk_in`, `referral`, `website`, `campaign`, `franchise`, `other` — the dimension FR-CRM-003 reports conversion rate by |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `new` CHECK IN (`new`,`contacted`,`qualified`,`converted`,`lost`) | |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | Leads are franchise-scoped like subscribers, so an LCO's pipeline stays theirs |
| `assigned_to_username` | `VARCHAR(100)` | NULLABLE | Sales owner |
| `notes` | `TEXT` | NULLABLE | |
| `lost_reason` | `TEXT` | NULLABLE | |
| `converted_subscriber_id` | `INTEGER` | FK → subscribers.id NULLABLE | Set atomically with `status='converted'` |
| `created_at` / `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | `set_updated_at()` trigger |
| `converted_at` | `TIMESTAMPTZ` | NULLABLE | |
| `CONSTRAINT chk_lead_converted_has_subscriber` | | a `converted` lead must carry `converted_subscriber_id`, and a non-converted one must not | Makes "converted but pointing at nothing" unstorable — the state FR-CRM-003's conversion rate would otherwise silently miscount |

### Table: `cpe_device_types` *(new — FR-INV-001, FR-INV-003, migration 027)*
**Module:** MOD-INV | MDS §4.16

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `name` | `VARCHAR(100)` | NOT NULL UNIQUE | e.g. "TP-Link Archer C6" |
| `vendor` | `VARCHAR(100)` | NOT NULL | |
| `reorder_threshold` | `INTEGER` | NOT NULL DEFAULT 5 CHECK >= 0 | In-stock count at or below which FR-INV-003's low-stock alert fires |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `cpe_devices` *(new — FR-INV-001..002, migration 027)*
**Module:** MOD-INV | MDS §4.16

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `device_type_id` | `INTEGER` | NOT NULL, FK → cpe_device_types.id | |
| `serial_number` | `VARCHAR(100)` | NOT NULL UNIQUE | The physical identity; uniqueness is what stops one router being tracked twice |
| `mac_address` | `VARCHAR(17)` | NULLABLE UNIQUE | |
| `status` | `VARCHAR(20)` | NOT NULL DEFAULT `in_stock` CHECK IN (`in_stock`,`issued`,`returned`,`faulty`) | |
| `location` | `VARCHAR(100)` | NULLABLE | Warehouse / van / office |
| `subscriber_id` | `INTEGER` | FK → subscribers.id NULLABLE | Current holder; cleared on return (MDS §4.16 — current state only, no assignment ledger) |
| `issued_at` | `TIMESTAMPTZ` | NULLABLE | |
| `notes` | `TEXT` | NULLABLE | |
| `created_at` / `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | `set_updated_at()` trigger |
| `CONSTRAINT chk_cpe_issued_has_subscriber` | | `status='issued'` requires `subscriber_id`, and any other status forbids it | An "issued to nobody" or "in stock but assigned" row is not a state the warehouse can actually be in |

#### ACS extension to `cpe_devices` *(migration 030 — FR-CPE-001..003, MDS §4.19)*

| Column | Type | Constraints | Description |
|---|---|---|---|
| `oui` | `VARCHAR(6)` | NULLABLE | Vendor OUI from the Inform's DeviceId triple |
| `product_class` | `VARCHAR(64)` | NULLABLE | |
| `connection_request_url` | `TEXT` | NULLABLE | Recorded but **not used**: residential CPE sits behind this platform's own CGNAT, so the advertised address is usually unreachable from the ACS (MDS §4.19) |
| `software_version` / `hardware_version` | `VARCHAR(64)` | NULLABLE | Reported each Inform; what makes a firmware-upgrade campaign targetable |
| `last_inform_at` | `TIMESTAMPTZ` | NULLABLE | Indexed `DESC NULLS LAST` — "which devices have gone quiet" is the query the NOC actually runs |
| `last_inform_event` | `VARCHAR(32)` | NULLABLE | Comma-joined event codes of the most recent session |
| `provisioning_state` | `VARCHAR(24)` | NOT NULL DEFAULT `unknown` CHECK IN (`unknown`,`registered`,`provisioned`,`needs_reprovision`,`fault`) | Partial index on `needs_reprovision`/`fault` only: those are the two states an operator queries for, and indexing the healthy majority would be dead weight |
| `last_fault` | `TEXT` | NULLABLE | Most recent CWMP fault, kept so a failed provisioning is diagnosable after the session has gone |
| `acs_discovered` | `BOOLEAN` | NOT NULL DEFAULT FALSE | Device Informed with no warehouse record |
| `CONSTRAINT chk_cpe_discovered_not_in_stock` | | `acs_discovered` forbids status `in_stock` | An ACS-discovered device is physically in a subscriber's home; counting it as sellable stock would overstate inventory |

`cpe_device_types` also gains `provisioning_template JSONB` (NULLABLE): TR-069
parameter paths differ per model, so holding them as data makes a new router
model a row rather than a release.

### Table: `cpe_tasks` *(new — FR-CPE-003, migration 030)*
**Module:** MOD-CPE | MDS §4.19

One queued CWMP RPC. The queue exists because CWMP is CPE-initiated: an
operator action cannot be pushed and must wait for a session the device opens.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | Also the CWMP `ParameterKey`/`CommandKey`, which is how a device's response is correlated back to the request |
| `device_id` | `INTEGER` | NOT NULL, FK → cpe_devices.id ON DELETE CASCADE | |
| `rpc_type` | `VARCHAR(32)` | NOT NULL CHECK IN (`SetParameterValues`,`GetParameterValues`,`Reboot`,`Download`,`FactoryReset`) | The RPC subset this ACS implements; the CHECK is what stops an unsupported RPC being queued and silently never delivered |
| `params` | `JSONB` | NULLABLE | Parameter paths/values, or the firmware URL |
| `status` | `VARCHAR(16)` | NOT NULL DEFAULT `pending` CHECK IN (`pending`,`sent`,`completed`,`failed`,`expired`) | The `pending` predicate in the claim query is the exactly-once guard (MDS §4.19) |
| `priority` | `INTEGER` | NOT NULL DEFAULT 50 | Lower runs first: a technician-triggered reboot (10) overtakes routine provisioning |
| `created_by` | `VARCHAR(100)` | NOT NULL | Operator username, or `acs:auto-provision` for engine-queued work — a reboot must be attributable |
| `fault_code` / `fault_string` | `VARCHAR(16)` / `TEXT` | NULLABLE | CWMP fault the device returned |
| `created_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | Tiebreaks priority, so equal-priority tasks run FIFO |
| `sent_at` / `completed_at` | `TIMESTAMPTZ` | NULLABLE | |
| `expires_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() + 7 days | Expired tasks are skipped, not delivered: a reboot queued a fortnight ago arriving now is an unexplained outage |


### Table: `cpe_purchases` *(new — FR-INV-003, migration 027)*
**Module:** MOD-INV | MDS §4.16

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `device_type_id` | `INTEGER` | NOT NULL, FK → cpe_device_types.id | |
| `vendor` | `VARCHAR(100)` | NOT NULL | Recorded per purchase as well as per type: the same model is often sourced from different distributors |
| `quantity` | `INTEGER` | NOT NULL CHECK > 0 | |
| `unit_cost` | `NUMERIC(12,2)` | NOT NULL CHECK >= 0 | |
| `invoice_ref` | `VARCHAR(100)` | NULLABLE | |
| `purchased_by_username` | `VARCHAR(100)` | NOT NULL | |
| `purchased_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |

### Table: `subscriber_status_history` *(new — FR-RPT-001, migration 031)*
**Module:** MOD-RPT | MDS §4.20

Append-only record of every subscriber lifecycle transition. Exists because
`subscribers.status` is overwritten in place and `updated_at` is bumped by
unrelated edits, so nothing else in the schema can date a churn event.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `subscriber_id` | `INTEGER` | NOT NULL, FK → subscribers.id ON DELETE CASCADE | |
| `old_status` | `VARCHAR(20)` | NULLABLE | NULL means no predecessor: either a creation or the seeded baseline |
| `new_status` | `VARCHAR(20)` | NOT NULL | |
| `reason` | `VARCHAR(64)` | NULLABLE | `signup`, `operator`, `dunning`, `termination`, `lead_conversion`, `baseline` |
| `changed_by` | `VARCHAR(100)` | NOT NULL DEFAULT `unknown` | JWT subject, or `system:dunning-scanner` for workers. `unknown` when a write path supplied no context — the event is still captured |
| `plan_id` | `INTEGER` | FK → plans.id NULLABLE | Denormalised at the moment of the change: churn asks which plan was *left*, and the subscriber's current plan is a different question |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | Same reasoning; also the grouping FR-RPT-003 reports on |
| `is_baseline` | `BOOLEAN` | NOT NULL DEFAULT FALSE | TRUE only for the one-off snapshot seeded for pre-migration accounts. A snapshot is a starting position, not an event, and reporting views exclude these from growth and churn |
| `occurred_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `CONSTRAINT chk_ssh_status_changed` | | `old_status IS DISTINCT FROM new_status` | A save that changes nothing is not a transition; without this an operator re-saving an unchanged form inflates every count |
| `CONSTRAINT chk_ssh_baseline_has_no_predecessor` | | `NOT is_baseline OR old_status IS NULL` | A baseline carrying a predecessor is a transition claiming to be a snapshot |

Indexes: `idx_ssh_occurred (occurred_at DESC)` for period reports;
`idx_ssh_subscriber (subscriber_id, occurred_at DESC)` for one account's timeline.

### Table: `ticket_status_history` *(new — FR-RPT-001, migration 031)*
**Module:** MOD-RPT | MDS §4.20

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `ticket_id` | `INTEGER` | NOT NULL, FK → tickets.id ON DELETE CASCADE | |
| `old_status` | `VARCHAR(20)` | NULLABLE | |
| `new_status` | `VARCHAR(20)` | NOT NULL | |
| `changed_by` | `VARCHAR(100)` | NOT NULL DEFAULT `unknown` | |
| `is_baseline` | `BOOLEAN` | NOT NULL DEFAULT FALSE | |
| `occurred_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `CONSTRAINT chk_tsh_status_changed` | | `old_status IS DISTINCT FROM new_status` | |
| `CONSTRAINT chk_tsh_baseline_has_no_predecessor` | | `NOT is_baseline OR old_status IS NULL` | |

Index `idx_tsh_ticket (ticket_id, occurred_at)` serves the query every
resolution metric starts with: the **first** transition into `resolved`. First,
not last — a ticket closed, reopened and closed again is a support failure, and
taking the last timestamp would report it as one slow success while hiding the
reopen entirely.

### Capture triggers *(migration 031)*

`capture_subscriber_status()` and `capture_ticket_status()` fire `AFTER INSERT`
and `AFTER UPDATE OF status ... WHEN (OLD.status IS DISTINCT FROM NEW.status)`.
The `WHEN` clause means the function is never called for the far more frequent
`wallet_balance`, `plan_expiry` and `fup_active` writes, so hot paths are
unaffected.

Attribution reaches the trigger through the transaction-local settings
`app.actor` and `app.change_reason`, set by the same statement that performs
the write (MDS §4.20). Triggers guarantee the *event* is never lost; the
application supplies *who* and *why* when it can.

### Table: `revenue_snapshots` *(new — FR-REV-001)*
**Module:** MOD-REV

| Column | Type | Description |
|---|---|---|
| `id` | `SERIAL` PK | |
| `snapshot_date` | `DATE` NOT NULL | |
| `unbilled_subscriber_count` | `INTEGER` NOT NULL | |
| `ledger_variance` | `NUMERIC(12,2)` NOT NULL | Should be 0.00 |
| `total_wallet_balance` | `NUMERIC(14,2)` NOT NULL | |
| `created_at` | `TIMESTAMPTZ` DEFAULT NOW() | |

### Table: `collections_forecast` *(new — FR-REV-004)*
**Module:** MOD-REV

| Column | Type | Description |
|---|---|---|
| `id` | `SERIAL` PK | |
| `forecast_date` | `DATE` NOT NULL | Date forecast was generated |
| `forecast_for_date` | `DATE` NOT NULL | Future date being forecast |
| `expected_renewals` | `INTEGER` | Subscribers with wallet ≥ plan price |
| `at_risk_renewals` | `INTEGER` | Subscribers with wallet < plan price |
| `expected_revenue` | `NUMERIC(14,2)` | |
| `at_risk_revenue` | `NUMERIC(14,2)` | |

### Table: `kyc_verifications`
**FR:** FR-SEC-002..003 | **Module:** MOD-AUTH

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `aadhaar_encrypted` | `TEXT` | NULLABLE | `{key_version}:{base64_ciphertext}` |
| `pan_encrypted` | `TEXT` | NULLABLE | `{key_version}:{base64_ciphertext}` |
| `key_version_id` | `VARCHAR(10)` | FK → encryption_keys.version_id | |
| `verified_at` | `TIMESTAMPTZ` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `encryption_keys`
**FR:** FR-SEC-002..003 | **Module:** MOD-AUTH

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `version_id` | `VARCHAR(10)` | UNIQUE NOT NULL | e.g. `v1`, `v2`, `v3` |
| `key_hash` | `VARCHAR(64)` | NOT NULL | SHA-256 of key material for audit |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `rotated_at` | `TIMESTAMPTZ` | NULLABLE | |
| `status` | `VARCHAR(10)` | DEFAULT 'active' | `active` or `retired` |

### Table: `cgnat_allocations`
**FR:** FR-NET-001..002 | Partitioned monthly on `allocated_at`

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `subscriber_id` | `INTEGER` FK | |
| `public_ip` | `INET` NOT NULL | |
| `port_start` / `port_end` | `INTEGER` NOT NULL | |
| `nas_ip_address` | `INET` | |
| `allocated_at` | `TIMESTAMPTZ` | Partition key |
| `released_at` | `TIMESTAMPTZ` NULLABLE | |

### Table: `lea_audit_log`
**FR:** FR-OBS-003 | Append-only; row security policy (INSERT only)

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `accessor_identity` | `VARCHAR(255)` NOT NULL | JWT `sub` claim |
| `accessor_role` | `VARCHAR(50)` NOT NULL | |
| `queried_public_ip` | `INET` NOT NULL | |
| `queried_port` | `INTEGER` NULLABLE | |
| `queried_timestamp` | `TIMESTAMPTZ` NOT NULL | |
| `result_subscriber_id` | `INTEGER` NULLABLE | |
| `result_row_count` | `INTEGER` NOT NULL | |
| `accessed_at` | `TIMESTAMPTZ` DEFAULT NOW() | |

### General ledger *(new — CRD-EXP-006, design 2026-08-24, not yet built)*

Every monetary table this document already describes is correct for what it
was built to answer: `wallet_ledgers` is a true double-entry ledger for one
subscriber's prepaid balance, `lco_ledger` is a per-partner commission
record, `invoices`/`payment_refunds` are billing documents. None of them
answers "what did the business itself earn and spend this month" — there is
no chart of accounts, no trial balance, no P&L for the ISP's own books. This
is that design, split into two phases on purpose: Phase 1 is a real,
standalone general ledger that touches nothing else in the schema and can be
built and verified in isolation; Phase 2 wires it to the money-moving code
that already exists (wallet postings, franchise commission, procurement) and
is **not** authorized by this design alone — each integration point changes
a live financial code path and needs its own sign-off when it is actually
scoped, the same caution CRD-EXP-006 was flagged for in the first place.

#### Phase 1 — standalone ledger

##### Table: `chart_of_accounts`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `code` | `VARCHAR(20)` | UNIQUE NOT NULL | Traditional numeric code (`1000`, `4000`, ...), sorted display order for free |
| `name` | `VARCHAR(100)` | NOT NULL | |
| `account_type` | `VARCHAR(20)` | NOT NULL CHECK IN (`asset`,`liability`,`equity`,`income`,`expense`) | |
| `normal_balance` | `VARCHAR(6)` | NOT NULL CHECK IN (`debit`,`credit`) | Stored, not derived, so a report never has to re-decide it per row |
| `is_active` | `BOOLEAN` | NOT NULL DEFAULT true | Deactivated rather than deleted, matching this schema's convention everywhere else money or auth is involved |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

Seeded on migration with a minimal starter chart — the accounts Phase 2's
own integrations will need are named here now so a later migration only has
to add postings, not accounts:

| Code | Name | Type |
|---|---|---|
| 1000 | Cash / Bank | asset |
| 1200 | Subscriber Wallet Liability | liability |
| 2000 | Accounts Payable | liability |
| 3000 | Owner's Equity | equity |
| 4000 | Subscription Revenue | income |
| 5000 | Franchise Commission Expense | expense |
| 5100 | Operating Expenses | expense |

##### Table: `gl_journal_entries`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `entry_date` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `description` | `TEXT` | NOT NULL | |
| `source_type` | `VARCHAR(30)` | NOT NULL DEFAULT `manual` | `manual` in Phase 1; Phase 2 adds `wallet_recharge`, `lco_commission`, `purchase_order` |
| `source_id` | `INTEGER` | NULLABLE | The triggering row's id once Phase 2 exists; always NULL for a manual entry |
| `created_by` | `VARCHAR(100)` | NOT NULL | |
| `created_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |

##### Table: `gl_journal_lines`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `journal_entry_id` | `INTEGER` | NOT NULL FK → gl_journal_entries.id ON DELETE CASCADE | |
| `account_id` | `INTEGER` | NOT NULL FK → chart_of_accounts.id | |
| `debit` | `NUMERIC(14,2)` | NOT NULL DEFAULT 0 CHECK (debit >= 0) | |
| `credit` | `NUMERIC(14,2)` | NOT NULL DEFAULT 0 CHECK (credit >= 0) | |

Two more constraints than the columns above show, because "a balanced
journal entry" is a real, enforceable guarantee, not just an application
convention:

- `CHECK (NOT (debit > 0 AND credit > 0))` — one line is a debit or a
  credit, never both; a single line trying to be both would let a
  transposition error cancel itself out silently.
- `CHECK (debit > 0 OR credit > 0)` — no zero-amount no-op line.
- A `CONSTRAINT TRIGGER ... AFTER INSERT OR UPDATE OR DELETE ... DEFERRABLE
  INITIALLY DEFERRED` on `gl_journal_lines`, firing once per statement at
  transaction commit, that sums `debit - credit` for every
  `journal_entry_id` touched in the transaction and raises if any sum is
  non-zero. Deferred rather than immediate: an application posting a
  multi-line entry inserts its lines one at a time, and an immediate
  per-row check would reject every entry after its first line, before the
  balancing line has been written. This is the actual "double-entry" claim
  for the business's own books — not assumed of the application code the
  way `wallet_ledgers` currently is (see `WalletService.Post`'s own
  in-application balance arithmetic, which this does not replace or audit).

##### Reporting views

- `v_gl_trial_balance` — one row per account: `debit_total`, `credit_total`,
  and a signed `balance` oriented by the account's own `normal_balance`, so
  every account reads as a positive number when it is in its expected
  position and negative when it is not (an asset account with a credit
  balance, for instance, which is exactly the anomaly a trial balance exists
  to surface).
- `v_gl_income_statement(from, to)` — income and expense accounts only,
  summed within a date window, net income as the difference. A parameterised
  view is not directly expressible in plain SQL `CREATE VIEW`; implemented as
  a query function (`gl_income_statement(from date, to date)` returning a
  table) rather than a view for that reason.
- `v_gl_balance_sheet(as_of)` — asset/liability/equity accounts, balances as
  of a point in time; same function-not-view reasoning.

##### Console screen (Phase 1)

A `Ledger` (or `Accounts`, if that name were not already taken by Staff
Accounts — needs a distinct label) section, owner/billing_admin only,
matching every other financial screen's gating: chart of accounts (view;
editing which accounts exist is rare enough to not need a form in the first
cut), a manual journal-entry form (date, description, and two or more
account/debit-or-credit lines, rejected client- and server-side unless the
lines balance before ever reaching the deferred trigger), a journal listing,
and the two reports above.

#### Phase 2 — auto-posting integration (not authorized by this design; scope separately)

Three existing money-moving paths would each gain a journal entry, listed
here so the shape of that future work is visible now rather than rediscovered
later — this is a list of where the hooks belong, not permission to add them:

- **Wallet recharge** (`internal/billing/wallet.go`) — a credit posts Dr Cash
  / Bank, Cr Subscriber Wallet Liability; a debit (renewal/consumption)
  posts Dr Subscriber Wallet Liability, Cr Subscription Revenue.
- **Franchise commission** (`CalculateAndStoreLCOCommission`,
  `internal/db/revenue.go`) — Dr Franchise Commission Expense, Cr Accounts
  Payable (the partner is owed the commission, not yet paid it).
- **Purchase order received** (`internal/procurement`, migration 042) — Dr
  Operating Expenses (or an asset account, if the business wants CPE
  purchases capitalised rather than expensed — a real accounting-policy
  choice, not a technical default this design should make silently), Cr
  Accounts Payable.

Each of these touches a live, already-correct financial code path
(`WalletService.Post`'s balance guarantee, `CalculateAndStoreLCOCommission`'s
commission math, the procurement lifecycle just shipped) — adding a second
write to `gl_journal_entries`/`gl_journal_lines` alongside the existing one
is exactly the kind of change that deserves its own review of failure modes
(what happens if the GL write fails but the wallet write already committed?
same-transaction, or eventually-consistent via the job queue?) rather than
being folded into this design pass.

---

## 6.3 Partitioning Strategy

```sql
SELECT partman.create_parent('public.subscriber_session_history', 'start_time', 'monthly', 3);
SELECT partman.create_parent('public.cgnat_allocations', 'allocated_at', 'monthly', 3);
```

---

## 6.4 Index Definitions

```sql
-- LEA: IP-to-subscriber lookup
CREATE INDEX idx_lea_ipv4_time ON subscriber_session_history(assigned_ipv4, start_time DESC)
  INCLUDE (subscriber_id, stop_time);

-- LEA: CGNAT port-block lookup
CREATE INDEX idx_cgnat_lea ON cgnat_allocations(public_ip, allocated_at DESC)
  INCLUDE (subscriber_id, port_start, port_end, released_at);

-- AAA: fast subscriber auth
CREATE INDEX idx_sub_auth ON subscribers(username, status);

-- AAA: active session cleanup on NAS reconnect
CREATE INDEX idx_nas_active ON subscriber_session_history(nas_ip_address) WHERE stop_time IS NULL;

-- Billing: dunning expiry scan
CREATE INDEX idx_sub_expiry ON subscribers(plan_expiry, status) WHERE status IN ('active','grace_period');

-- Billing: wallet idempotency
CREATE UNIQUE INDEX idx_wallet_token ON wallet_ledgers(transaction_token) WHERE transaction_token IS NOT NULL;

-- Notifications: subscriber notification history (FR-SUB-005)
CREATE INDEX idx_notif_subscriber ON notification_log(subscriber_id, sent_at DESC);

-- Notifications: delivery status callback lookup by provider_message_id (FR-NOTIF-011)
CREATE INDEX idx_notif_provider_id ON notification_log(provider_message_id) WHERE provider_message_id IS NOT NULL;

-- Revenue: unbilled subscriber report (FR-REV-001)
CREATE INDEX idx_revenue_unbilled ON subscribers(status, plan_expiry) WHERE status = 'active';

-- Franchise: LCO subscriber isolation
CREATE INDEX idx_franchise_subscribers ON subscribers(franchise_id) WHERE franchise_id IS NOT NULL;

-- NAS: vendor/secret lookup on every Access-Request and CoA/PoD send (FR-NAS-002, v3)
-- Already covered by the UNIQUE constraint on nas_devices.ip; no separate index needed.

-- NAS: plan-to-profile resolution for reference-model vendors (FR-NAS-001, v3)
CREATE INDEX idx_plan_nas_vendor ON plan_nas_profiles(plan_id, vendor);

-- SLA scanner: the query that runs every 5 minutes (MDS §4.13) — open
-- tickets whose response/resolution clock is worth checking. Partial index
-- (WHERE status excludes closed states) keeps it small and keeps it useful
-- as the ticket table grows, rather than indexing rows the scanner will
-- never select.
CREATE INDEX idx_tickets_sla_resolution ON tickets(sla_resolution_due_at)
  WHERE status NOT IN ('resolved', 'closed');
CREATE INDEX idx_tickets_sla_response ON tickets(sla_response_due_at)
  WHERE status = 'open';

-- Tickets: subscriber's own ticket list (portal) and admin lookup by
-- subscriber_id (staffui) — surprisingly absent before v3; every query
-- against this table up to now was a sequential scan.
CREATE INDEX idx_tickets_subscriber ON tickets(subscriber_id);

-- Tickets: staff's assigned queue and franchise-scoped queue
CREATE INDEX idx_tickets_assigned_to ON tickets(assigned_to) WHERE assigned_to IS NOT NULL;
CREATE INDEX idx_tickets_franchise ON tickets(franchise_id) WHERE franchise_id IS NOT NULL;

-- SLA events: per-ticket event history lookup (also enforced by
-- uq_sla_event for the idempotency use, this index serves ordinary reads)
CREATE INDEX idx_sla_events_ticket ON sla_events(ticket_id, occurred_at DESC);

-- Routing: rule matching at ticket-creation time, ordered by precedence
CREATE INDEX idx_routing_rules_lookup ON ticket_routing_rules(category, franchise_id, priority_order);
```

---

## 6.5 Row Security — LEA Audit Log

```sql
ALTER TABLE lea_audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY lea_insert_only ON lea_audit_log FOR INSERT WITH CHECK (true);
```

---

## 6.6 Read Replica & Failover Considerations *(new — NFR-AVAIL-002, v3)*

Schema-level implications of MDS §4.12 (PostgreSQL HA). The failover
topology itself — which failover manager, standby count, DNS/VIP mechanics —
is an IDD §8 concern and out of scope here; this section covers what the
*schema and query patterns* need to already support before that topology
exists, so adding HA later doesn't require another migration pass.

**Replication role.** A dedicated `bss_replicator` role (`REPLICATION`
privilege only, no table grants) should be created alongside the existing
`bss_app` least-privilege role (migration `019_create_app_role.sql`) — the
same reasoning that keeps the application off the `postgres` superuser
applies to the replication stream: a compromised standby's credentials
should not be able to read table data directly, only the WAL stream.

**`synchronous_commit`.** Recommend `remote_write` (not `on`) for the
standby: `on` blocks every commit on the standby's disk fsync, which turns a
replica hiccup into primary-side write latency for `wallet_ledgers` and
`subscriber_session_history` — exactly the tables where FR-BIL-003's
atomic double-entry write and the RADIUS accounting path can least afford
added latency. `remote_write` still guarantees the standby has *received*
the WAL (no silent data loss on primary crash), just not fsynced it,
which is the right trade for this workload.

**No schema changes required for the tables in §6.2** — read-replica
routing (MDS §4.12's routing table) is a connection-string/query-routing
decision in `internal/db`, not a table design one. The one exception:
`revenue_snapshots` and `collections_forecast` (§6.2) are already
write-once/read-many nightly-batch tables, which makes them the safest
first candidates to route to a replica once one exists.

---

## 6.7 Reporting Views *(new — FR-RPT-001, FR-RPT-003, migration 032)*

**Module:** MOD-RPT | MDS §4.21

The first views in the schema. Three are plain, one materialised; all read the
capture tables §6.2 documents plus the existing current-state tables.

| Object | Kind | Grain | Notes |
|---|---|---|---|
| `v_plan_mix` | view | plan × franchise | `mrr` counts active subscribers only — a suspended account produces no revenue |
| `v_subscriber_growth_monthly` | view | month × franchise × plan | Excludes `is_baseline` rows. `suspended` is reported beside `churned`, never inside it |
| `mv_ticket_resolution` | materialised | month × category × priority × franchise | Median (not mean) hours to the **first** resolution; `reopens` counted separately |
| `v_franchise_collection` | view | franchise × month | `billed` from `invoices`, `collected` from `lco_ledger` — separate sources on purpose |

### `idx_mv_ticket_resolution_key`

```sql
CREATE UNIQUE INDEX idx_mv_ticket_resolution_key
    ON mv_ticket_resolution (month, category, priority, franchise_id) NULLS NOT DISTINCT;
```

Required by `REFRESH MATERIALIZED VIEW CONCURRENTLY`, which without it takes an
ACCESS EXCLUSIVE lock and blocks every reader for the duration of the rebuild.

The index must be over plain **columns**: Postgres rejects a concurrent refresh
backed by a partial or expression index. The obvious formulation,
`coalesce(franchise_id, -1)` to handle direct subscribers having no franchise,
is exactly an expression index and was verified to fail. `NULLS NOT DISTINCT`
(PostgreSQL 15+) achieves the same grouping while keeping the index eligible.

### `refresh_reporting_views()`

`SECURITY DEFINER`, `SET search_path = pg_catalog, public`, `EXECUTE` granted to
`bss_app` only.

PostgreSQL has no REFRESH privilege — the command requires *ownership* of the
materialised view. The application connects as `bss_app` (migration 019) while
migrations run as the superuser, so a direct `REFRESH` from the app fails with
`must be owner of materialized view`. Making `bss_app` the owner would fix that
and also grant it the right to drop the view; the function grants exactly the
refresh and nothing else. `search_path` is pinned because a SECURITY DEFINER
function with a caller-controlled search_path is a privilege-escalation vector.

### Grants

`SELECT` on all four objects is granted to `bss_app` explicitly, for the same
reason `staff_users` needed it in migration 021: `ALTER DEFAULT PRIVILEGES`
covers only objects created by the role that set it.

---

## 6.8 Partner API & Webhooks *(new — FR-API-001..003, migration 033)*

**Module:** MOD-API | MDS §4.22

### Table: `api_keys`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `partner_name` | `VARCHAR(100)` | NOT NULL | |
| `key_prefix` | `VARCHAR(24)` | NOT NULL UNIQUE | `pk_live_7f3a2b1c`. A lookup handle, not a secret — the server cannot search by a hashed key, so it parses this and fetches one row |
| `key_hash` | `TEXT` | NOT NULL | SHA-256. Not bcrypt: an API key is 192 bits of CSPRNG output, so there is no dictionary to slow down and bcrypt would cost ~100ms on every partner request (MDS §4.22) |
| `scopes` | `TEXT[]` | NOT NULL | Closed vocabulary, validated at creation |
| `active` | `BOOLEAN` | NOT NULL DEFAULT TRUE | |
| `last_used_at` | `TIMESTAMPTZ` | NULLABLE | Best-effort; never worth failing a request to write |
| `expires_at` | `TIMESTAMPTZ` | NULLABLE | NULL = no expiry |
| `created_by` | `VARCHAR(100)` | NOT NULL | Issuing operator |
| `created_at` / `revoked_at` | `TIMESTAMPTZ` | | `revoked_at` is written once by an atomic conditional claim, so a second revoke cannot overwrite when the key actually stopped working |
| `CONSTRAINT chk_api_key_scoped` | | `cardinality(scopes) >= 1` | `cardinality()` not `array_length()`: the latter returns NULL for an empty array and a CHECK passes on NULL — the same trap that let an earlier constraint in this schema admit the row it was written to reject |

Index `idx_api_keys_active (key_prefix) WHERE active` — the authentication
path is the only latency-sensitive query, and revoked keys are never looked up.

### Table: `webhook_endpoints`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `api_key_id` | `INTEGER` | NOT NULL, FK → api_keys.id ON DELETE CASCADE | Binds an endpoint to its owner; deactivation is scoped by it so one partner cannot disable another's |
| `url` | `TEXT` | NOT NULL | |
| `secret_encrypted` | `TEXT` | NOT NULL | `{key_version}:{base64(nonce+ct)}` via the AES keystore, same as `nas_devices.radius_secret_encrypted`. **Encrypted, not hashed** — HMAC needs the secret back, so SHA-256 is right for `api_keys` and wrong here |
| `key_version_id` | `VARCHAR(10)` | NOT NULL, FK → encryption_keys.version_id | Travels with the data so rotation stays possible |
| `events` | `TEXT[]` | NOT NULL | Closed vocabulary |
| `active` | `BOOLEAN` | NOT NULL DEFAULT TRUE | |
| `CONSTRAINT chk_webhook_https` | | `url LIKE 'https://%'` | Catches the trivial case. The SSRF range check cannot live in SQL — it must run in Go at registration *and* at dial time, because DNS can be re-pointed between them (MDS §4.22) |
| `CONSTRAINT chk_webhook_events` | | `cardinality(events) >= 1` | |

### Table: `webhook_deliveries`

The audit trail FR-API-003 requires. Asynq handles retrying; this table is what
a partner's support ticket is answered from weeks later.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `endpoint_id` | `INTEGER` | NOT NULL, FK → webhook_endpoints.id ON DELETE CASCADE | |
| `event_id` | `UUID` | NOT NULL | The partner's idempotency key, echoed in every retry |
| `event_type` | `VARCHAR(64)` | NOT NULL | |
| `payload` | `JSONB` | NOT NULL | Stored as sent. Thin by policy — identifiers only, so no PII lands here and DPDP retention does not apply to the log |
| `status` | `VARCHAR(16)` | NOT NULL CHECK IN (`pending`,`delivered`,`failed`,`abandoned`) | `abandoned` ≠ `failed`: failed is one bad attempt, abandoned means nobody will try again |
| `attempts` | `INTEGER` | NOT NULL DEFAULT 0 | |
| `response_status` / `response_excerpt` | `INTEGER` / `TEXT` | NULLABLE | Excerpt truncated by the writer — a partner's 500 page is not our audit log |
| `last_error`, `next_attempt_at`, `created_at`, `delivered_at` | | | |

`UNIQUE (endpoint_id, event_id)` is what makes a retry **update** the trail
rather than fork it. Without it, an Asynq retry after a mid-write crash logs
the same delivery twice and the attempt count — the number used to spot a
flapping partner — becomes meaningless.

### Grants

`SELECT, INSERT, UPDATE` to `bss_app` on all three, plus their sequences. **No
DELETE anywhere**: keys are revoked, endpoints deactivated, and the delivery
log is an audit trail — a partner dispute is settled from rows nobody could
quietly remove.
