# Document 00: Master Traceability Index
**Version:** 3.0 | **Status:** Draft | **Date:** 2026-08-12
**Document ID:** IDX

This document maps every Business Outcome → Requirement ID → Architecture Component → Module → Database Object → API Endpoint → Test Case, providing end-to-end traceability across the entire documentation suite.

---

## Document Map

```
IDX  ← You are here (master index)
│
├── CRD  01_CRD_Customer_Requirements.md   Business outcomes, personas, scope, compliance
│     └── traces to all downstream docs
│
├── SRS  02_SRS_System_Requirements.md     92 FRs + 11 NFRs (52 FR/9 NFR baseline + 40 FR/2 NFR v3 expansion, requirements-stage)
│     ├── traces to SAD, MDS, DDS, DBD, API, TST
│
├── SAD  03_SAD_System_Architecture.md     11 components, data flows, HA/DR
│     ├── traces to MDS, DDS, IDD
│
├── MDS  04_MDS_Module_Design.md           10 modules, task catalogue, metrics
│     ├── traces to DDS, DBD, API, TST
│
├── DDS  05_DDS_Detailed_Design.md         Go code patterns, WhatsApp API, health API, revenue jobs
│     ├── traces to DBD, API, TST
│
├── DBD  06_DBD_Database_Design.md         All tables, indexes, partitioning, row security
│     ├── traces to API, TST
│
├── API  07_API_OpenAPI_Contract.md        Full OpenAPI 3.0 spec (25+ endpoints)
│     └── traces to TST
│
├── IDD  08_IDD_Infrastructure_Design.md  Docker Compose, Redis Sentinel, env vars, backups
│     ├── traces to DXD, OPS
│
├── SecD 09_SecD_Security_Design.md       STRIDE, encryption, JWT, RBAC, DND, webhook security
│     ├── traces to DDS, TST
│
├── DMP  10_DMP_Data_Migration.md         Migration phases, transformation, rollback, PII handling
│     └── traces to DXD
│
├── DXD  11_DXD_Developer_Setup.md        Local setup, test phases, seed data, troubleshooting
│     └── traces to TST
│
├── OPS  12_OPS_Operations_Runbook.md     Incident response, PoD/CoA procedures, Redis/PG failover
│
└── TST  13_TST_Test_Strategy.md          Test pyramid, 60+ test cases, NFR load tests, chaos tests
```

---

## Business Outcome → FR → Doc Traceability

| Business Outcome | BO ID | FR IDs | Primary Docs | Test IDs |
|---|---|---|---|---|
| Revenue leakage ≤ 0.5% | BO-001 | FR-REV-001..004 | MDS §4.8, DDS §5.10, DBD §6.2, API §7 | INT-REV-001..004 |
| Zero DoT/DPDP compliance risk | BO-002 | FR-SEC-002..003, FR-NET-002, FR-OBS-003 | DDS §5.5, DBD §6.2 | INT-PII-001..003, INT-SEC-003..004 |
| Staff cost reduction (2+ FTEs automated) | BO-003 | FR-BIL-004, FR-NOTIF-001..011, FR-SUB-001..005 | MDS §4.3, §4.7, §4.9 | INT-NOTIF-001..009, INT-SUB-001..004 |
| Franchise / LCO growth support | BO-004 | FR-FRN-001..003 | MDS §4.10, DBD §6.2, API §7 | INT-FRN-001..003 |
| ARPU + churn visibility | BO-005 | FR-REV-003..004 | MDS §4.8, API §7 | INT-REV-003..004 |
| Scale to 20,000 subscribers | BO-006 | NFR-SCAL-001 | SAD §3.1, IDD §8.2 | TST §13.4 |
| Full ISP operations-suite parity (CRM → field ops on one platform) | BO-007 | FR-NAS, FR-HSP, FR-API, FR-MOB, FR-SUP, FR-CRM, FR-INV, FR-WFL, FR-ANN, FR-FRN-004..006, FR-RPT, FR-DOC, FR-CPE, NFR-AVAIL-002, NFR-TEN-001 | CRD §1.11 | TBD — v3, requirements-stage |

---

## Persona → FR → Doc Traceability

| Persona | PER ID | Key FRs Serving This Persona | Docs |
|---|---|---|---|
| ISP Owner | PER-001 | FR-REV-001..004, FR-FRN-001..003, NFR-SCAL-001 | CRD §1.1, MDS §4.8, §4.10 |
| NOC Engineer | PER-002 | FR-OBS-004, FR-OBS-005, FR-FUP-002..003 | DDS §5.9, API §7, TST §13.10 |
| Billing Admin | PER-003 | FR-BIL-006..007, FR-REV-001..004 | MDS §4.3, §4.8, API §7 |
| CSR | PER-004 | FR-OBS-004, FR-NOTIF-009 | DDS §5.9, DBD §6.2, API §7 |
| Ground Technician | PER-005 | FR-OBS-004 | DDS §5.9, API §7 |
| End Subscriber | PER-006 | FR-NOTIF-001..011, FR-SUB-001..005, FR-BIL-007 | MDS §4.7, §4.9, DBD §6.2 |

---

## WhatsApp Notification Traceability

| Template ID | Event | FR | DDS | DBD Table | Test |
|---|---|---|---|---|---|
| TMPL-001 | FUP 80% warning | FR-NOTIF-002, FR-FUP-004 | DDS §5.8 | notification_log | INT-NOTIF-001 |
| TMPL-002 | FUP throttle applied | FR-NOTIF-003, FR-FUP-005 | DDS §5.8 | notification_log | INT-NOTIF-002 |
| TMPL-003 | Renewal reminder (T-7d/3d/1d) | FR-NOTIF-001, FR-BIL-004 | DDS §5.8 | notification_log | INT-NOTIF-003 |
| TMPL-004 | Soft suspension | FR-NOTIF-005, FR-BIL-004 | DDS §5.8 | notification_log | INT-NOTIF-004 |
| TMPL-005 | Hard suspension | FR-NOTIF-005, FR-BIL-004 | DDS §5.8 | notification_log | — |
| TMPL-006 | Service restored | FR-NOTIF-006, FR-BIL-004 | DDS §5.8 | notification_log | INT-NOTIF-005 |
| TMPL-007 | Payment received | FR-NOTIF-004 | DDS §5.8 | notification_log | — |
| TMPL-008 | Ticket update | FR-NOTIF-007 | DDS §5.8 | notification_log | — |

---

## New Tables in v2 (vs v1)

| Table | Purpose | FR | First Defined |
|---|---|---|---|
| `franchises` | LCO/franchise partners | FR-FRN-001 | DBD §6.2 |
| `lco_ledger` | Commission tracking per recharge | FR-FRN-002 | DBD §6.2 |
| `notification_log` | Full delivery log per subscriber per message | FR-NOTIF-009 | DBD §6.2 |
| `notification_templates` | WhatsApp/SMS template catalogue | FR-NOTIF-010 | DBD §6.2 |
| `tickets` | Subscriber support tickets | FR-SUB-004 | DBD §6.2 |
| `revenue_snapshots` | Nightly unbilled + variance snapshot | FR-REV-001 | DBD §6.2 |
| `collections_forecast` | 30-day forward collections | FR-REV-004 | DBD §6.2 |

---

## New API Endpoints in v2 (vs v1)

| Endpoint | FR | Module |
|---|---|---|
| `GET /api/v1/subscribers/{id}/health` | FR-OBS-004 | MOD-PORTAL, SAD-COMP-008 |
| `GET /api/v1/subscribers/{id}/usage` | FR-SUB-001 | MOD-PORTAL |
| `GET /api/v1/subscribers/{id}/notifications` | FR-NOTIF-009, FR-SUB-005 | MOD-NOTIF |
| `GET /api/v1/revenue/unbilled` | FR-REV-001 | MOD-REV |
| `GET /api/v1/revenue/reconciliation` | FR-REV-002 | MOD-REV |
| `GET /api/v1/revenue/collections-forecast` | FR-REV-004 | MOD-REV |
| `GET /api/v1/revenue/gstr1-export` | FR-BIL-006 | MOD-BIL |
| `GET /api/v1/franchises` | FR-FRN-001 | MOD-FRN |
| `GET /api/v1/franchises/{id}/pnl` | FR-FRN-003 | MOD-FRN |
| `GET /api/v1/franchises/consolidated-pnl` | FR-FRN-003 | MOD-FRN |
| `POST /webhooks/whatsapp` | FR-NOTIF-011 | MOD-NOTIF |
| `GET /webhooks/whatsapp` (verification) | FR-NOTIF-011 | MOD-NOTIF |

---

## Change Log: v1 → v2

| Change | Gap Addressed | Docs Affected |
|---|---|---|
| Added WhatsApp Business API notification channel (8 templates, delivery callbacks) | CRD-NOTIF-001 | CRD, SRS, MDS §4.7, DDS §5.8, DBD §6.2, API, IDD §8.7, TST §13.7 |
| Added notification_log table + CSR delivery history API | CRD-NOTIF-002, PER-004 | DBD, API, TST §13.7 |
| Added subscriber health endpoint (single-call diagnostic) | PER-002, PER-004, PER-005 | SAD §3.2, DDS §5.9, API, TST §13.10 |
| Added revenue assurance module (unbilled report, reconciliation, forecast) | BO-001, CRD-REV-001..002 | MDS §4.8, DDS §5.10, DBD, API, TST §13.8 |
| Added GSTR-1 compatible GST export | PER-003 | SRS FR-BIL-006, MDS §4.3, API |
| Added plain-language usage summary on invoice | PER-006 | SRS FR-BIL-007, MDS §4.3, DBD invoices table |
| Added subscriber self-service portal | PER-006, BO-003 | SRS FR-SUB-001..005, MDS §4.9, DBD, API, TST §13.9 |
| Added franchise / LCO multi-tenant module | BO-004 | CRD §1.9, SRS FR-FRN-001..003, MDS §4.10, DBD, API, TST §13.11 |
| Added 80% FUP warning notification | PER-006 | SRS FR-FUP-004..005, FR-NOTIF-002, MDS §4.2 |
| Added proactive NAS failure rate alert (FR-OBS-005) | PER-002 | SRS, SAD §3.2, MDS §4.1 |
| Added franchise_id to subscribers, plans, wallet_ledgers | BO-004 | DBD |
| Added dunning_state column to subscribers | PER-003 | DBD |
| Added gb_included, gb_used to invoices | PER-006, FR-BIL-007 | DBD |
| Added owner-level business outcome table (BO-001..006) | All personas | CRD §1.1 |
| Added persona "what they buy" framing to all personas | All personas | CRD §1.2 |
| Documented future phase items (NAS SNMP, mobile app, AI churn) | All | CRD §1.10 |
| Updated version to 2.0 across all 13 documents | — | All |

---

## Change Log: v2 → v3

Prompted by a 2026-08-12 gap analysis comparing this platform against a
reference commercial ISP-manager architecture (see CRD §1.11 for the full
rationale). The v2 core — AAA, billing, FUP, notifications, revenue assurance
— was confirmed sound and is unchanged. What's added is the operations-suite
breadth a full parity claim requires, adopted as a phased roadmap rather than
a flat feature checklist.

| Change | Gap Addressed | Docs Affected |
|---|---|---|
| Adopted BO-007: full ISP operations-suite parity as an explicit owner-level outcome | Reference-architecture gap analysis | CRD §1.1, §1.11, IDX |
| Added multi-vendor NAS attribute support (Cisco/Juniper/Huawei/ZTE, not just MikroTik) + per-device `nas_devices` table | Network layer only enforces bandwidth correctly on MikroTik today | SRS FR-NAS-001..004, CRD §1.11 Phase 2 |
| Added CHAP/EAP-MSCHAPv2 support requirement | PAP-only today, a direct consequence of bcrypt-only credential storage | SRS FR-AAA-005..006 |
| Added CoA-on-plan-change requirement | Mid-cycle upgrade doesn't reach an already-connected session today | SRS FR-AAA-007 |
| Added PostgreSQL HA/failover requirement | Redis has Sentinel HA; Postgres — where the money lives — is a single instance | SRS NFR-AVAIL-002 |
| Added hotspot/captive-portal + MAC-auth module | Only PPPoE-style auth exists today | SRS FR-HSP-001..003 |
| Added partner API-key auth + outbound webhooks | 3rd-party API reuses internal staff JWT; no outbound event mechanism exists | SRS FR-API-001..003 |
| Added mobile-facing API requirement | No mobile app or mobile-specific API surface exists | SRS FR-MOB-001..002 |
| Added helpdesk SLA (priority, due-by, breach alerting) | Tickets have no priority or SLA tracking today | SRS FR-SUP-001..003 |
| Added CRM/lead-pipeline module | No pre-subscriber prospect tracking exists | SRS FR-CRM-001..003 |
| Added inventory/CPE-tracking module | No device/stock/warehouse tracking exists | SRS FR-INV-001..003 |
| Added task & approval workflow module | No second-approver sign-off or ad hoc field-task assignment exists | SRS FR-WFL-001..002 |
| Added announcements/broadcast module | No staff-to-subscriber broadcast mechanism exists | SRS FR-ANN-001..002 |
| Extended franchise module from "defined but unreachable" to routed + portal + onboarding | `internal/revenue/franchise.go` exists (commission calc, roles) but has zero routes — confirmed via grep, no `/api/v1/franchises` endpoint registered anywhere in Go code | SRS FR-FRN-004..006 |
| Added general reporting module (growth/churn, plan-mix, ticket metrics, per-area collections) | Only revenue reconciliation exists as a report today | SRS FR-RPT-001..003 |
| Added external document storage requirement | PDFs render via self-hosted Gotenberg only, no S3/Drive/SFTP archival | SRS FR-DOC-001 |
| Added TR-069/CWMP CPE auto-provisioning requirement | No automated CPE provisioning exists — technicians configure by hand | SRS FR-CPE-001..003 |
| Added email and push as notification channels | Only WhatsApp + SMS exist as dispatch channels today | SRS FR-NOTIF-012..013 |
| Added multi-tenant SaaS hosting as a distinct, sequenced-last requirement | Current architecture is single-tenant on-premise; franchise multi-tenancy is row-level within one deployment, not a hosting model | SRS NFR-TEN-001, CRD §1.11 Phase 5 |
| Updated version to 3.0 on IDX, CRD, SRS | — | IDX, CRD, SRS |

**Deliberately not done in this pass:** MDS module design, DDS implementation
patterns, DBD schema, and API contracts for the 40 new FRs above. Those are
requirements-stage only — each gets its own design pass when it's actually
scheduled for implementation, matching how FR-NOTIF-007 went from a one-line
gap to a wired, tested, verified feature in a single focused slice rather
than every module landing at once with no depth. MDS/DDS/DBD/API remain at
v2 and should be treated as stale for anything in the v3 expansion list until
that module's design pass happens.
