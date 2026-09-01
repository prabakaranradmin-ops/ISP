# Document 13: Test Strategy & NFR Validation Plan
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** TST
**Traces From:** [SRS](02_SRS_System_Requirements.md) → [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md)
**Traces To:** —

---

## 13.1 Testing Philosophy

Every development phase requires a passing test gate before proceeding. The testing pyramid is enforced at the CI pipeline level:

```
          ┌──────────────┐
          │   Load Tests  │  ← NFR validation; run pre-release
          ├──────────────┤
          │  Integration  │  ← Full stack; run on every PR
          ├──────────────┤
          │  Unit Tests   │  ← Isolated; run on every commit
          └──────────────┘
```

**Gate policy:**
- Unit tests must pass before integration tests run.
- Integration tests must pass before a PR can be merged.
- Load tests must pass before a release candidate tag is created.

---

## 13.2 Unit Test Coverage Requirements

| Package | Coverage Target | Key Test Cases |
|---|---|---|
| `pkg/crypto` | ≥ 90% | Encrypt/decrypt round-trip; versioned key rotation; tampered ciphertext rejection |
| `internal/billing` | ≥ 85% | GST intrastate vs interstate; banker's rounding edge cases; idempotency token dedup; HMAC validation |
| `internal/radius` | ≥ 80% | Dedup key logic; worker pool channel behavior under load simulation |
| `internal/fup` | ≥ 80% | FUP threshold breach detection; CoA task idempotency key construction |
| `internal/middleware` | ≥ 90% | Valid JWT accept; expired JWT reject; wrong role 403; missing token 401 |
| `internal/cgnat` | ≥ 80% | Port-block record insert; LEA lookup with/without CGNAT |

### Critical Unit Test Cases

```go
// pkg/crypto: key rotation round-trip
func TestEncryptDecryptAcrossKeyVersions(t *testing.T) {
    v1Enc := NewAESEncryptor(key1, "v1")
    ciphertext, _ := v1Enc.Encrypt("123456789012")  // Aadhaar
    // Simulate rotation: decrypt using v1, even when v2 is now active
    plaintext, _ := Decrypt(ciphertext, mockKeyStore{v1: key1, v2: key2})
    assert.Equal(t, "123456789012", plaintext)
}

// internal/billing: HMAC validation
func TestWebhookHMACRejectsInvalidSignature(t *testing.T) {
    err := ValidateRazorpaySignature([]byte(`{"event":"payment.captured"}`),
        "invalid_signature", "test_secret")
    assert.Error(t, err)
}

// internal/billing: wallet idempotency
func TestWalletRechargeIdempotency(t *testing.T) {
    // First call: credits wallet
    tx1, _ := svc.Recharge(ctx, RechargeRequest{Token: "tok_abc", Amount: "799.00"})
    // Second call with same token: returns same tx, no double credit
    tx2, _ := svc.Recharge(ctx, RechargeRequest{Token: "tok_abc", Amount: "799.00"})
    assert.Equal(t, tx1.ID, tx2.ID)
    // Balance incremented only once
    assert.Equal(t, "799.00", getBalance(subscriberID))
}
```

---

## 13.3 Integration Test Suite

Integration tests run against real local infrastructure (Docker Compose stack). Tagged with `//go:build integration`.

### RADIUS Authentication Flow

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-AAA-001 | `radtest` with valid credentials for active subscriber | Access-Accept; session written to Redis |
| INT-AAA-002 | `radtest` with invalid password | Access-Reject |
| INT-AAA-003 | `radtest` for suspended subscriber | Access-Reject |
| INT-AAA-004 | 11 rapid failures from same MAC in 60s | 12th attempt blocked for 15 minutes |
| INT-AAA-005 | Duplicate Interim-Update packet (same session ID + octets) | ACK returned; Redis counter not double-incremented |

### FUP & CoA Flow

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-FUP-001 | Subscriber usage reaches FUP threshold | Asynq CoA task enqueued within 10s |
| INT-FUP-002 | CoA task simulated NAS ACK | Task marked complete; no retry |
| INT-FUP-003 | CoA task simulated NAS timeout × 5 | Task moved to dead-letter; alert triggered |
| INT-FUP-004 | Manual FUP override via API | CoA task enqueued with correct rate limit string |

### Billing & Wallet Flow

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-BIL-001 | Wallet recharge via API | `wallet_ledgers` has debit + credit entry; balance updated atomically |
| INT-BIL-002 | Same `transaction_token` twice | Second call returns original tx; balance unchanged |
| INT-BIL-003 | Razorpay webhook with valid HMAC | Wallet credited; 200 returned |
| INT-BIL-004 | Razorpay webhook with invalid HMAC | 400 returned; wallet unchanged; `webhook_hmac_failures_total` incremented |
| INT-BIL-005 | GST invoice generation for TN subscriber | Invoice PDF generated with CGST + SGST; IGST = 0 |
| INT-BIL-006 | GST invoice generation for interstate subscriber | Invoice PDF with IGST; CGST = SGST = 0 |

### Security & RBAC Flow

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-SEC-001 | API request with no JWT | 401 Unauthorized |
| INT-SEC-002 | CSR role accessing billing admin route | 403 Forbidden |
| INT-SEC-003 | NOC role accessing LEA endpoint without `lea_access` flag | 403 Forbidden |
| INT-SEC-004 | LEA lookup with valid NOC + lea_access JWT | 200 + audit record written to `lea_audit_log` |

### PII Encryption Flow

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-PII-001 | Subscriber created with Aadhaar | `kyc_verifications.aadhaar_encrypted` contains version prefix; no plaintext in DB |
| INT-PII-002 | `pii_rotation` job triggered | All `kyc_verifications` records updated to new key version; decryption succeeds post-rotation |
| INT-PII-003 | Rotation job interrupted mid-batch | Re-run completes successfully; no duplicate re-encryption |

---

## 13.4 NFR Load Test Plan

### NFR-PERF-001: RADIUS Authentication Latency ≤ 15 ms p99

**Tool:** `radperf` (or `pyrad` load script)

```bash
radperf \
  --server localhost:1812 \
  --secret my_shared_radius_secret \
  --rate 5000 \           # 5,000 requests/second
  --duration 600 \        # 10 minutes
  --users scripts/test_users.txt \
  --report-interval 10

# Pass criteria:
#   p50 ≤ 5ms
#   p95 ≤ 10ms
#   p99 ≤ 15ms
#   Error rate ≤ 0.01%
```

Pre-condition: Redis pre-loaded with 20,000 active subscriber session profiles.

### NFR-SCAL-001: 20,000 Concurrent PPPoE Sessions

```bash
# Step 1: Pre-seed Redis with 20,000 subscriber profiles
go run scripts/load_test_seed.go --sessions 20000

# Step 2: Simulate authentication storm (all 20k sessions auth within 5 min)
go run scripts/auth_storm.go --sessions 20000 --rate 67/s --duration 300s

# Step 3: Hold all 20k sessions active for 60 minutes, sending Interim-Updates every 60s
go run scripts/session_hold.go --sessions 20000 --duration 3600s --interim-interval 60s

# Pass criteria:
#   No goroutine leak (pprof heap stable)
#   No worker pool starvation (radius_worker_queue_depth < 50 sustained)
#   RADIUS auth p99 remains ≤ 15ms throughout hold period
#   Asynq queue depth does not grow unbounded
```

### NFR-PERF-002: API p99 ≤ 200 ms at 500 Concurrent Users

```bash
# k6 load test script: scripts/k6_api_load.js
k6 run \
  --vus 500 \
  --duration 5m \
  scripts/k6_api_load.js

# Pass criteria:
#   p99 ≤ 200ms
#   p95 ≤ 100ms
#   Error rate ≤ 0.1%
```

### NFR-AVAIL-001: 99.99% Uptime Validation (Chaos Tests)

| Test | Procedure | Pass Criteria |
|---|---|---|
| PostgreSQL primary kill | `docker-compose stop postgres_primary` | API 500s for ≤ 5 min; operator escalates within 5 min per runbook |
| AAA daemon container crash | `docker-compose kill aaa_core_daemon` | Container restarts within 10s; auth resumes |
| Worker crash mid-CoA | `docker-compose kill aaa_core_daemon` while `network_commands` has in-flight tasks | Tasks left in `processing` return to the pool when their lease expires; none lost, none run twice |
| Network partition: NAS to AAA | `iptables` block UDP 1812/1813 | Existing sessions unaffected; new auths queue and recover on unblock |

**The Redis primary-kill drill was removed on 2026-09-01**, along with
`scripts/run_sentinel_failover_test.sh` that ran it. There is no Redis to
kill: session state moved to `live_sessions` (migration 036) and the task
queue to `jobqueue_tasks` (037), and the subscriber auth cache is now
in-process. What that drill protected — "the datastore dies and
authentication recovers" — is now entirely the PostgreSQL row above, which
is why IDD § 8.2a's automatic failover matters more than it did when a
second datastore shared the risk.

One finding from that drill is worth keeping, because it generalises beyond
Redis: the original 3s budget was unachievable and had to be retargeted to
8s, and the reason was that *detection*, not promotion, dominated the cycle.
A quorum-based failover cannot react faster than its members' timeout, and
lowering that timeout to chase a budget buys false-positive failovers on
ordinary network jitter. The same trade-off applies to the Patroni `ttl`
setting now governing PostgreSQL promotion — a target chosen without
measuring detection time will be wrong the same way.

---

## 13.5 CI Pipeline Test Gates

```yaml
# .github/workflows/ci.yml (conceptual)

jobs:
  unit-tests:
    steps:
      - run: go test -short -race -coverprofile=coverage.out ./...
      - run: go tool cover -func=coverage.out | grep total  # Assert ≥ 80%
      - run: golangci-lint run ./...

  integration-tests:
    needs: unit-tests
    services:
      postgres: postgres:15-alpine
      redis: redis:7-alpine
    steps:
      - run: goose -dir ./migrations postgres "$DSN" up
      - run: go test -tags=integration -v -race ./...

  security-scan:
    needs: unit-tests
    steps:
      - run: gosec ./...         # Static security analysis
      - run: go run scripts/pii_scan.go ./...  # Assert no plaintext PII in logs/errors

  load-tests:
    needs: integration-tests
    # Manual trigger only (pre-release)
    when: manual
    steps:
      - run: go run scripts/load_test_seed.go --sessions 20000
      - run: radperf --rate 5000 --duration 600 ...
      - run: k6 run scripts/k6_api_load.js
```

---

## 13.6 Test Data Management

- Unit tests use in-memory mocks; no persistent state.
- Integration tests use a dedicated `isp_bss_oss_test` database, truncated before each test run.
- Load tests use `isp_bss_oss_loadtest` database; never run against production.
- Seeded subscriber usernames in test environments are prefixed `lt_` to distinguish from real subscribers.
- Test AES keys are never used in production — the test key store returns a fixed key for deterministic testing.

---

## 13.7 Notification Integration Tests *(new — FR-NOTIF-001..011)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-NOTIF-001 | Subscriber reaches 80% FUP threshold | Asynq `notif:fup_warning` task enqueued; WhatsApp + SMS dispatched; `notification_log` record created with `status=sent` |
| INT-NOTIF-002 | FUP throttle applied | WhatsApp + SMS dispatched with TMPL-002; `notification_log` record created |
| INT-NOTIF-003 | Dunning T-7d trigger fires | WhatsApp + SMS + email dispatched; DND check executed first; `notification_log` record created per channel |
| INT-NOTIF-004 | Soft suspension (T+24h) | WhatsApp + SMS with payment link dispatched; subscriber status updated |
| INT-NOTIF-005 | Wallet recharged → service restored | WhatsApp + SMS (TMPL-006) dispatched; `notification_log` created |
| INT-NOTIF-006 | Subscriber has `dnd_opt_out=true`, dunning T-7d fires | No WhatsApp/SMS dispatched; `notification_log` created with `status=suppressed_dnd` |
| INT-NOTIF-007 | Meta delivery callback `delivered` received on `POST /webhooks/whatsapp` | `notification_log.delivery_status` updated from `sent` to `delivered` |
| INT-NOTIF-008 | Meta delivery callback `failed` received | `notification_log.delivery_status=failed`, `failure_reason` populated |
| INT-NOTIF-009 | CSR fetches `GET /api/v1/subscribers/{id}/notifications` | Returns last N notification log entries for subscriber |

## 13.8 Revenue Assurance Tests *(new — FR-REV-001..004)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-REV-001 | Active subscriber with no invoice this month exists | `GET /api/v1/revenue/unbilled` includes that subscriber |
| INT-REV-002 | Wallet balance matches ledger net | `GET /api/v1/revenue/reconciliation` returns `variance=0.00` |
| INT-REV-003 | Artificial ledger variance introduced | Reconciliation job triggers `ledger_variance_detected` alert |
| INT-REV-004 | 30-day forecast generated | `GET /api/v1/revenue/collections-forecast` returns segmented counts and revenue |

## 13.9 Subscriber Portal Tests *(new — FR-SUB-001..005)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-SUB-001 | Portal JWT authenticated subscriber requests usage | Returns GB used/total from Redis (not DB); responds in ≤ 200ms |
| INT-SUB-002 | Subscriber views notification history on portal | `GET /api/v1/subscribers/{id}/notifications` returns log entries |
| INT-SUB-003 | Subscriber initiates renewal via portal | Redirects to Razorpay deeplink with correct `transaction_token` |
| INT-SUB-004 | Subscriber-scoped JWT cannot access another subscriber's data | Returns 403 Forbidden |

## 13.10 Subscriber Health API Tests *(new — FR-OBS-004)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-HEALTH-001 | CSR calls health endpoint for active subscriber | Returns session, FUP status, wallet, last notification in single response ≤ 200ms |
| INT-HEALTH-002 | Health endpoint for subscriber with no active session | `active_session=null`; rest of fields populated |
| INT-HEALTH-003 | Technician role can access health endpoint | 200 OK |
| INT-HEALTH-004 | Subscriber-scoped JWT cannot access health endpoint (admin-only fields) | Returns restricted view; sensitive fields omitted |

## 13.11 Franchise / LCO Tests *(new — FR-FRN-001..003)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| INT-FRN-001 | LCO JWT queries subscriber list | Returns only subscribers belonging to that `franchise_id` |
| INT-FRN-002 | Subscriber recharge triggers LCO commission | `lco_ledger` record created with correct commission amount |
| INT-FRN-003 | Billing admin queries consolidated P&L | Returns aggregate across all franchises |

## 13.12 Archival, Captive Portal, and Report Export NFR Tests *(new — NFR-DUR-002, NFR-SEC-003, NFR-PERF-004)*

| Test ID | Scenario | Expected Result |
|---|---|---|
| NFR-DUR-002-1 | Archive a document, independently hash the stored file on disk | Matches the checksum recorded in `document_archives` — corruption during write is detectable |
| NFR-DUR-002-2 | Attempt to purge an archive before `retain_until` | `chk_archive_not_purged_before_retention` rejects the write |
| *(not yet buildable)* | Corrupt a stored file, retrieve it through the application | **No such path exists** — `archive.Store` has no `Get`/restore method. Add this test when one is built |
| NFR-SEC-003-1 | Exceed 10 voucher-redemption attempts per MAC within 15 minutes | 11th attempt is refused |
| NFR-SEC-003-2 | Redis (limiter backend) is unavailable during a redemption attempt | Request is refused, not admitted — fail closed |
| NFR-PERF-004 | Export each of growth, ticket-resolution, and collection at `months=120` against a seeded 20,000-subscriber / 50-franchise / ~430k-invoice dataset, 30 iterations each | p99 ≤ 4.5 s for every report type. Baseline measured 2026-08-16: collection p50 840 ms / p99 1.69 s (worst case); growth p99 57 ms; ticket-resolution p99 6.7 ms |
