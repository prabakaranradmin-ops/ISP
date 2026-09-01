# Document 11: Developer Environment Setup Guide (DXD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** DXD
**Traces From:** [IDD](08_IDD_Infrastructure_Design.md) → [DDS](05_DDS_Detailed_Design.md)
**Traces To:** [TST](13_TST_Test_Strategy.md)

---

## 11.1 Local Workspace Requirements

| Tool | Minimum Version | Purpose |
|---|---|---|
| Go | 1.21 | AAA daemon and API service |
| Docker Engine | 24.0 | Local infrastructure containers |
| Docker Compose | 2.0 | Orchestrate local service stack |
| `psql` | 15 | Database access and seed script execution |
| `radtest` | any | RADIUS authentication simulation |
| `golangci-lint` | 1.55+ | Linting (required for CI parity) |

---

## 11.2 Step-by-Step Initialization

### 1. Clone the repository

```bash
git clone https://github.com/your-repo/isp-bss-oss.git && cd isp-bss-oss
```

### 2. Configure environment variables

```bash
cp .env.example .env
# Edit .env — minimum required for local dev:
#   DB_SECURE_PASSWORD=localdevpassword
#   JWT_SECRET=localdevjwtsecret32chars!!
#   RAZORPAY_WEBHOOK_SECRET=localdevhmacsecret
```

### 3. Start infrastructure containers

```bash
# Starts PostgreSQL and Gotenberg — the only infrastructure the stack needs.
# There is no cache or queue tier to bring up: both live in PostgreSQL
# (migrations 036/037) and the auth cache is in-process. See IDD § 8.3.
docker-compose up -d postgres_primary gotenberg_engine
```

Wait for health checks to pass:

```bash
docker-compose ps
# postgres_primary  → healthy
# gotenberg_engine  → healthy
```

### 4. Run database migrations

```bash
# Install goose if not present
go install github.com/pressly/goose/v3/cmd/goose@latest

# Apply all migrations
goose -dir ./migrations postgres \
  "host=localhost user=postgres password=localdevpassword dbname=isp_bss_oss sslmode=disable" up
```

### 5. Seed test data

```bash
psql -h localhost -U postgres -d isp_bss_oss -f scripts/seed_local.sql
```

The seed script creates:
- Default GST rate (9% CGST + 9% SGST)
- Three test plans: 50 Mbps / 100 Mbps / 200 Mbps
- Two test subscribers: `test_user` (active) and `suspended_user` (suspended)
- Corresponding wallet balances and one test invoice

### 6. Run the test suite

```bash
# All tests — unit + integration (requires running infrastructure)
go test -v -race ./...

# Unit tests only (no infrastructure needed)
go test -v -short ./...

# With coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**All tests must pass before proceeding.** The CI pipeline enforces this gate.

### 7. Start the AAA daemon

```bash
go run cmd/radiusd/main.go
```

### 8. Start the API service

```bash
go run cmd/api/main.go
```

### 9. Verify RADIUS authentication

```bash
# Should return: Access-Accept
radtest test_user secret_password localhost:1812 0 my_shared_radius_secret

# Should return: Access-Reject
radtest suspended_user secret_password localhost:1812 0 my_shared_radius_secret
```

### 10. Verify API health

```bash
curl http://localhost:8080/health
# Expected: 200 OK
```

---

## 11.3 Development Testing Guidelines

Each feature or fix must include tests before the PR is opened. Follow these phases:

### Phase 1: Unit Tests

Test individual functions in isolation using mocks for PostgreSQL.

```bash
# Example: billing module unit tests
go test -v ./internal/billing/...
```

Coverage target: **≥ 80%** for `internal/billing`, `internal/radius`, `pkg/crypto`.

### Phase 2: Integration Tests

Test the full stack against real local infrastructure.

```bash
# Tag-based: integration tests are tagged to separate from unit tests
go test -v -tags=integration ./...
```

Key integration test scenarios:
- RADIUS Access-Accept for active subscriber
- RADIUS Access-Reject for suspended subscriber
- FUP breach → CoA task enqueued in Asynq
- Wallet recharge idempotency (same `transaction_token` twice → single credit)
- AES encrypt/decrypt round-trip across key versions
- GST calculation: TN intrastate vs interstate

### Phase 3: Load / Performance Tests (NFR Validation)

Run before any release candidate tag:

```bash
# Install radperf for RADIUS load testing
# Run: 5000 req/s for 10 minutes; assert p99 ≤ 15ms
radperf -server localhost:1812 -secret my_shared_radius_secret \
  -rate 5000 -duration 10m -users scripts/test_users.txt
```

```bash
# API load test with k6
k6 run scripts/k6_api_load.js
# Assert: p99 ≤ 200ms at 500 concurrent users
```

---

## 11.4 Local Seed Data Reference

```sql
-- scripts/seed_local.sql

INSERT INTO gst_rates (cgst_rate, sgst_rate, igst_rate, effective_from)
VALUES (9.00, 9.00, 18.00, NOW());

INSERT INTO plans (name, rate_limit_string, volume_gb, fup_threshold_bytes, fup_throttle_string, price, validity_days)
VALUES
  ('TN_Basic_50M',   '50M/50M',   1650, 1771674009600, '5M/5M',   499.00, 30),
  ('TN_Super_100M',  '100M/100M', 3300, 3543348019200, '10M/10M', 799.00, 30),
  ('TN_Ultra_200M',  '200M/200M', 5000, 5368709120000, '20M/20M', 1199.00, 30);

INSERT INTO subscribers
  (caf_number, username, password_hash, mobile_number, plan_id, status, wallet_balance, registered_state)
VALUES
  ('CAF-0001', 'test_user',      '$2a$12$...', '+919876543210', 2, 'active',    799.00, 'TN'),
  ('CAF-0002', 'suspended_user', '$2a$12$...', '+919876543211', 1, 'suspended',   0.00, 'TN');
```

---

## 11.5 Common Local Development Issues

| Issue | Cause | Fix |
|---|---|---|
| `radtest` returns `Access-Reject` for `test_user` | Subscriber missing, suspended, or the auth cache holds a stale entry (60s TTL) | Confirm the row in `subscribers`; restart `aaa_core_daemon` to drop the in-process cache |
| PostgreSQL connection refused | Healthcheck not yet passed | Wait 15s and retry; `docker-compose ps` to check |
| `go vet ./...` passes but the integration suite will not build | The suite is behind `//go:build integration`, so untagged commands skip it entirely | Run `go vet -tags=integration ./...` — it needs no database and catches drifted test doubles in seconds |
| `go test` fails with `connection refused` | Infrastructure not running | Run `docker-compose up -d` first; use `-short` flag for unit-only tests |
| Migration fails with `relation already exists` | Migration re-run on non-empty schema | Check `goose status`; migrations are idempotent only if using goose versioning |
