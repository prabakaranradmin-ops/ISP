# External validation: penetration test, pilot and UAT

**Purpose:** these three cannot be self-certified, and they are what stands
between this platform and a defensible market-readiness claim. Everything
else on the readiness list is engineering we can do ourselves; these need an
outside party, a real ISP and real operators. They also have the longest lead
times, which is why this document exists now rather than when the code is
"finished".

Written 2026-11-23. Sections 1 and 2 are written to be handed to a vendor or
a pilot operator directly.

---

## 1. Penetration test — scoping brief

### 1.1 What the system is

An ISP business- and operations-support platform: subscriber management,
prepaid billing with GST, RADIUS authentication and accounting, FUP
enforcement, a captive portal, TR-069 CPE management, and a subscriber
self-service portal. Go, PostgreSQL, deployed either as a native Windows
service set or via Docker.

### 1.2 Attack surface to cover

| Surface | Detail | Why it matters |
|---|---|---|
| Operations console | 39 screens, session-cookie auth, 5 roles | Holds every subscriber's data; role gates are enforced per section |
| Subscriber portal | 9 JSON routes + 17 HTML pages, JWT | Customer-facing, internet-exposed |
| Captive portal | 5 routes, **unauthenticated by design** | Reachable by anyone on the hotspot network |
| JSON API | ~200 routes, JWT, 6 roles | Partner and mobile integration |
| RADIUS | UDP 1812 auth, 1813 accounting | Unauthenticated protocol surface; shared-secret trust |
| TR-069 ACS | CWMP over HTTP | Devices authenticate; the endpoint is reachable |
| Payment webhooks | Razorpay signature verification | Money path; replay and forgery |
| Partner webhooks (outbound) | SSRF guard with a dialler Control hook | Server-side request forgery |

### 1.3 Areas we specifically want attacked

These are where we believe we are strong, which is exactly why they need an
adversary rather than our own tests:

- **The two-audience JWT split.** Staff tokens and subscriber tokens are
  signed with different secrets so a leaked staff token cannot be replayed
  against subscriber endpoints. Two routes shipped for months verifying the
  wrong one — fixed, but it is the class of bug we most want hunted.
- **Role gates.** Enforced by a section list, not per-handler. We want the
  typed-URL case probed for every role, including the LEA lookup.
- **LEA lookup and its audit trail.** `lea_audit_log` is append-only via
  PostgreSQL RLS under a least-privilege role. We want attempts to write,
  update, truncate or bypass it.
- **KYC and PII at rest.** AES-GCM with a versioned key store. We want key
  handling, rotation and any path that returns plaintext examined.
- **Archived document retrieval.** Serves customer-supplied bytes with
  `Content-Disposition: attachment` and `nosniff`.
- **Multi-tenancy is NOT claimed.** One deployment serves one ISP. If the
  tester finds cross-ISP leakage it means something about the deployment
  model we do not know; there is deliberately no tenant isolation in the
  schema.

### 1.4 Explicitly out of scope

Physical security, social engineering, DDoS volumetrics, and the upstream
network. Third-party services (Razorpay, SMS/WhatsApp providers) are in scope
only at our integration boundary.

### 1.5 What we will provide

A dedicated instance with seeded data, accounts for all 5 console roles and a
subscriber, the OpenAPI contract (`specification_docs_v2/07_API`), the
security design including the STRIDE analysis (`09_SecD`), and source access
if a white-box test is preferred — we would prefer it.

### 1.6 Deliverable we expect

Findings with severity, reproduction steps and evidence; a remediation
retest included; and a summary letter suitable for sharing with prospective
customers under NDA. **The letter is half the point** — enterprise and
government ISP tenders ask for it.

### 1.7 Known gaps we are not asking you to find

Stated so the engagement is not spent confirming what we already know:
test coverage is 60.4% against our own 80% target; `internal/portal` is the
weakest at 63%. Accessibility is automated-clean but has had no manual
screen-reader pass. The SNMP poller has never polled a real router and the
S3 archival driver has never written to a real bucket.

---

## 2. Pilot deployment — requirements

### 2.1 What we need from a pilot ISP

| Requirement | Minimum | Why |
|---|---|---|
| Subscribers | 200+ | Below that, billing-run and FUP behaviour is not exercised |
| NAS / router | 1+ MikroTik or equivalent speaking RADIUS | **The single biggest unknown.** No real router has ever authenticated against this |
| Billing cycle | At least one full month | Renewal, grace, dunning and suspension only happen on the calendar |
| Payment gateway | A live Razorpay account | The money path is otherwise untested end to end |
| Staff | One owner, one billing operator, one field collector | The roles the console is built around |

### 2.2 What the pilot must prove

Not "does it work" — the tests say that. A pilot answers what tests cannot:

1. **Does a real NAS behave like the simulator?** Vendor quirks in RADIUS
   attributes, CoA/PoD handling and accounting intervals are where this will
   break first (FR-NET-006, FR-AAA-*).
2. **Does a real month of billing reconcile?** Invoices raised, payments
   taken, GST computed, commissions posted — against the operator's own
   books.
3. **Can an operator who has not seen it before run a day?** Without us on
   the phone.
4. **What does the support load look like?** Tickets raised per subscriber
   per month is the number that decides whether this is sellable.

### 2.3 Run it in parallel, not as a replacement

The pilot ISP should keep their existing system running for the first cycle.
This platform is prepaid-by-design with no postpaid support (FR-BIL-024,
closed by decision), so a mismatch there is a reason not to pilot with that
operator rather than a defect to fix mid-pilot.

### 2.4 Data protection

Real subscriber data means DPDP Act obligations from day one: purpose
limitation, the KYC encryption already in place, and a deletion path. A
written data-processing agreement with the pilot ISP should precede any
import. **This is the item most likely to delay a pilot start**, so begin it
early.

### 2.5 Exit criteria

One full billing cycle completed, books reconciled against the operator's
own, no Sev-1 incident unresolved beyond 24h, and the operator willing to be
a reference. Anything less is a trial, not a pilot.

---

## 3. User acceptance testing

### 3.1 Personas and the journeys each must sign off

Drawn from the five console roles the system already models:

| Persona | Must complete unaided |
|---|---|
| ISP owner | Read the dashboard, run a billing cycle, read revenue and the trial balance, add a staff account |
| Billing admin | Create a subscriber, take a payment, raise a credit note, handle a bounced cheque, run dunning |
| CSR | Find a subscriber by phone, read their 360 view, file and resolve a ticket, send an announcement |
| Field technician | Work a collection round, take cash at the door, issue and return CPE |
| LCO / franchise partner | See their own subscribers and P&L, and **not** see anybody else's |
| Subscriber | Sign in, see usage and invoices, pay, raise a ticket — **on a phone** |

### 3.2 How to run it

Scenario-based, not click-by-click: give each persona the outcome, not the
steps, and record where they hesitate. A script that tells someone which
button to press validates the script, not the software.

### 3.3 Sign-off criteria

Each persona completes their journeys without intervention; no severity-1 or
-2 defect open; and the owner signs the requirements baseline. That signature
is also what closes the weakest part of our requirements story — every
requirement currently traces to a competitor analysis or a domain assumption,
not to a named person's stated need.

---

## 4. Sequencing

These have long lead times and should start now, in parallel with the
remaining engineering:

| Item | Lead time | Start when |
|---|---|---|
| Pen-test vendor selection and scheduling | 2–6 weeks | Now — scoping brief above is ready to send |
| Pilot ISP identification and DPA | 4–8 weeks | Now — the agreement is the slow part |
| UAT | 1–2 weeks | After the pilot operator is identified; use their staff |

The engineering still outstanding — coverage to 80%, contract tests, ADRs, a
capacity model — does not block any of the three. Waiting for it to finish
before starting these would add a month to the critical path for nothing.
