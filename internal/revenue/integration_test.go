//go:build integration

// Integration tests for the revenue reconciliation job and franchise isolation.
//
// Covers INT-REV-001 .. INT-REV-003 and INT-FRN-001 .. INT-FRN-002 from the
// Integration Tests tracker sheet. The tracker lists the franchise cases under
// ./internal/franchise/; that code lives in this package, so they run here.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/revenue -Tags integration
package revenue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

const itJWTSecret = "revenue_integration_secret_32ch!!"

// ── Seeded revenue store ────────────────────────────────────────────────────

// itRevenueStore is an in-memory RevenueQuerier seeded with a known deficit.
type itRevenueStore struct {
	mu sync.Mutex

	unbilledCount int
	variance      decimal.Decimal
	totalBalance  decimal.Decimal
	forecast      []revenue.CollectionsForecast

	snapshots     []revenue.RevenueSnapshot
	savedForecast []revenue.CollectionsForecast
}

func (s *itRevenueStore) GetUnbilledActiveSubscribers(context.Context) (int, error) {
	return s.unbilledCount, nil
}

func (s *itRevenueStore) GetLedgerVariance(context.Context) (decimal.Decimal, error) {
	return s.variance, nil
}

func (s *itRevenueStore) GetTotalWalletBalance(context.Context) (decimal.Decimal, error) {
	return s.totalBalance, nil
}

func (s *itRevenueStore) UpsertRevenueSnapshot(_ context.Context, snap revenue.RevenueSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, snap)
	return nil
}

func (s *itRevenueStore) BuildCollectionsForecast(context.Context, int) ([]revenue.CollectionsForecast, error) {
	return s.forecast, nil
}

func (s *itRevenueStore) UpsertCollectionsForecast(_ context.Context, f []revenue.CollectionsForecast) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedForecast = append(s.savedForecast, f...)
	return nil
}

func (s *itRevenueStore) lastSnapshot(t *testing.T) revenue.RevenueSnapshot {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snapshots) == 0 {
		t.Fatal("no revenue snapshot was written")
	}
	return s.snapshots[len(s.snapshots)-1]
}

// itAlerter records fired alerts.
type itAlerter struct {
	mu     sync.Mutex
	events []string
}

func (a *itAlerter) Trigger(event string, _ any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
}

func (a *itAlerter) fired(event string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e == event {
			return true
		}
	}
	return false
}

// ── INT-REV-001 ─────────────────────────────────────────────────────────────

// TestFR_REV_001_UnbilledReport verifies the reconciliation job records the seeded count of
// active subscribers missing an invoice.
//
// INT-REV-001 | FR-REV-001
func TestFR_REV_001_UnbilledReport(t *testing.T) {
	// Seed: 2 active subscribers whose current period has no invoice.
	store := &itRevenueStore{
		unbilledCount: 2,
		variance:      decimal.Zero,
		totalBalance:  decimal.RequireFromString("15400.00"),
	}
	job := revenue.NewReconcileJob(store, &itAlerter{})

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := store.lastSnapshot(t)
	if snap.UnbilledSubscriberCount != 2 {
		t.Errorf("unbilled_subscriber_count: want 2 (the seeded deficit), got %d", snap.UnbilledSubscriberCount)
	}
	if !snap.TotalWalletBalance.Equal(decimal.RequireFromString("15400.00")) {
		t.Errorf("total_wallet_balance: want 15400.00, got %s", snap.TotalWalletBalance)
	}
	if snap.SnapshotDate.IsZero() {
		t.Error("snapshot_date must be set")
	}
}

// TestFR_REV_001_UnbilledReport_CleanBooksRecordZero verifies a fully invoiced base records
// a zero deficit rather than skipping the snapshot.
//
// INT-REV-001 (supporting) | FR-REV-001
func TestFR_REV_001_UnbilledReport_CleanBooksRecordZero(t *testing.T) {
	store := &itRevenueStore{unbilledCount: 0, variance: decimal.Zero, totalBalance: decimal.Zero}
	job := revenue.NewReconcileJob(store, &itAlerter{})

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.lastSnapshot(t).UnbilledSubscriberCount; got != 0 {
		t.Errorf("want 0 unbilled on clean books, got %d", got)
	}
}

// ── INT-REV-002 ─────────────────────────────────────────────────────────────

// TestFR_REV_002_LedgerVariance verifies clean books produce a variance within tolerance
// and raise no alert, while a real discrepancy alerts.
//
// INT-REV-002 | FR-REV-002
func TestFR_REV_002_LedgerVariance(t *testing.T) {
	tolerance := decimal.RequireFromString("0.01")

	cases := []struct {
		name      string
		variance  string
		wantAlert bool
		withinTol bool
	}{
		{"balanced books", "0.00", false, true},
		{"rounding dust below tolerance", "0.005", false, true},
		{"exactly at tolerance", "0.01", false, true},
		{"just over tolerance", "0.011", true, false},
		{"missing payment", "-250.00", true, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			variance := decimal.RequireFromString(c.variance)
			store := &itRevenueStore{variance: variance, totalBalance: decimal.Zero}
			alerter := &itAlerter{}
			job := revenue.NewReconcileJob(store, alerter)

			if err := job.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			snap := store.lastSnapshot(t)
			if !snap.LedgerVariance.Equal(variance) {
				t.Errorf("recorded variance: want %s, got %s", variance, snap.LedgerVariance)
			}
			if got := alerter.fired("ledger_variance_detected"); got != c.wantAlert {
				t.Errorf("alert fired=%v, want %v for variance %s", got, c.wantAlert, c.variance)
			}
			if within := variance.Abs().LessThanOrEqual(tolerance); within != c.withinTol {
				t.Errorf("variance %s within tolerance=%v, want %v", c.variance, within, c.withinTol)
			}
		})
	}
}

// ── INT-REV-003 ─────────────────────────────────────────────────────────────

// TestFR_REV_004_CollectionsForecast verifies the 30-day forecast is persisted with both
// the will-renew and at-risk segments populated, using exact decimal amounts.
//
// INT-REV-003 | FR-REV-004
func TestFR_REV_004_CollectionsForecast(t *testing.T) {
	forecastDate := time.Now().UTC().Truncate(24 * time.Hour)
	store := &itRevenueStore{
		variance:     decimal.Zero,
		totalBalance: decimal.RequireFromString("15400.00"),
		forecast: []revenue.CollectionsForecast{
			{
				ForecastDate:     forecastDate,
				ForecastForDate:  forecastDate.AddDate(0, 0, 7),
				ExpectedRenewals: 3,
				AtRiskRenewals:   1,
				ExpectedRevenue:  decimal.RequireFromString("2397.00"), // 3 × 799.00
				AtRiskRevenue:    decimal.RequireFromString("799.00"),
			},
			{
				ForecastDate:     forecastDate,
				ForecastForDate:  forecastDate.AddDate(0, 0, 21),
				ExpectedRenewals: 5,
				AtRiskRenewals:   2,
				ExpectedRevenue:  decimal.RequireFromString("3995.00"),
				AtRiskRevenue:    decimal.RequireFromString("1598.00"),
			},
		},
	}

	job := revenue.NewReconcileJob(store, &itAlerter{})
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store.mu.Lock()
	saved := append([]revenue.CollectionsForecast(nil), store.savedForecast...)
	store.mu.Unlock()

	if len(saved) != 2 {
		t.Fatalf("want 2 forecast rows persisted, got %d", len(saved))
	}

	var totalWillRenew, totalAtRisk int
	var revenueWillRenew, revenueAtRisk decimal.Decimal
	for _, f := range saved {
		totalWillRenew += f.ExpectedRenewals
		totalAtRisk += f.AtRiskRenewals
		revenueWillRenew = revenueWillRenew.Add(f.ExpectedRevenue)
		revenueAtRisk = revenueAtRisk.Add(f.AtRiskRevenue)
		if f.ForecastForDate.Before(f.ForecastDate) {
			t.Errorf("forecast_for_date %s precedes forecast_date %s", f.ForecastForDate, f.ForecastDate)
		}
	}

	if totalWillRenew < 1 {
		t.Errorf("want at least 1 will_renew subscriber, got %d", totalWillRenew)
	}
	if totalAtRisk < 1 {
		t.Errorf("want at least 1 at_risk subscriber, got %d", totalAtRisk)
	}
	if want := decimal.RequireFromString("6392.00"); !revenueWillRenew.Equal(want) {
		t.Errorf("expected_revenue total: want %s, got %s", want, revenueWillRenew)
	}
	if want := decimal.RequireFromString("2397.00"); !revenueAtRisk.Equal(want) {
		t.Errorf("at_risk_revenue total: want %s, got %s", want, revenueAtRisk)
	}
}

// ── Franchise store ─────────────────────────────────────────────────────────

// itFranchiseStore holds franchises, their subscribers, and the lco_ledger.
type itFranchiseStore struct {
	mu          sync.Mutex
	franchises  map[int]*revenue.Franchise
	subscribers []revenue.SubscriberRow
	ledger      []revenue.LCOCommissionEntry
}

func (s *itFranchiseStore) GetFranchiseByID(_ context.Context, id int) (*revenue.Franchise, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.franchises[id]
	if !ok {
		return nil, fmt.Errorf("franchise %d not found", id)
	}
	return f, nil
}

func (s *itFranchiseStore) CalculateAndStoreLCOCommission(_ context.Context, entry revenue.LCOCommissionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = append(s.ledger, entry)
	return nil
}

// ListSubscribers applies the franchise filter the way a WHERE clause would.
// GetSubscriberFranchiseID resolves a subscriber to its franchise the way
// the real store's SQL does — from the seeded subscriber rows, returning nil
// for one signed up directly rather than through a partner (which is what
// SettleCommissionForRecharge reads as "no commission owed").
func (s *itFranchiseStore) GetSubscriberFranchiseID(_ context.Context, subscriberID int) (*int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.subscribers {
		if row.ID == subscriberID {
			return row.FranchiseID, nil
		}
	}
	return nil, nil
}

func (s *itFranchiseStore) ListSubscribers(_ context.Context, franchiseID *int) ([]revenue.SubscriberRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if franchiseID == nil {
		return append([]revenue.SubscriberRow(nil), s.subscribers...), nil
	}
	var out []revenue.SubscriberRow
	for _, row := range s.subscribers {
		if row.FranchiseID != nil && *row.FranchiseID == *franchiseID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *itFranchiseStore) ledgerRows() []revenue.LCOCommissionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]revenue.LCOCommissionEntry(nil), s.ledger...)
}

func itToken(t *testing.T, role string, franchiseID int) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		FranchiseID:      franchiseID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func intPtr(v int) *int { return &v }

// ── INT-FRN-001 ─────────────────────────────────────────────────────────────

// TestFR_FRN_001_FranchiseIsolation_BlocksCrossLCOAccess verifies an LCO listing subscribers
// sees only its own franchise's rows.
//
// INT-FRN-001 | FR-FRN-001
func TestFR_FRN_001_FranchiseIsolation_BlocksCrossLCOAccess(t *testing.T) {
	// Seed: franchise 1 has one subscriber, franchise 2 has another.
	store := &itFranchiseStore{
		subscribers: []revenue.SubscriberRow{
			{ID: 101, Username: "f1-sub@isp", FranchiseID: intPtr(1)},
			{ID: 202, Username: "f2-sub@isp", FranchiseID: intPtr(2)},
		},
	}

	handler := middleware.JWTMiddleware(itJWTSecret)(
		revenue.FranchiseMiddleware(revenue.ListSubscribersHandler(store)),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+itToken(t, "lco", 1))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	var rows []revenue.SubscriberRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LCO F1 must see exactly 1 subscriber, got %d: %+v", len(rows), rows)
	}
	if rows[0].ID != 101 {
		t.Errorf("want subscriber 101 (franchise 1), got %d", rows[0].ID)
	}
	if body := rec.Body.String(); containsAny(body, "f2-sub@isp", "202") {
		t.Errorf("response leaked franchise 2 data: %s", body)
	}
}

// TestFR_FRN_001_FranchiseIsolation_OwnerSeesEverything verifies ISP-wide roles are not
// narrowed by the franchise scope.
//
// INT-FRN-001 (supporting) | FR-FRN-001
func TestFR_FRN_001_FranchiseIsolation_OwnerSeesEverything(t *testing.T) {
	store := &itFranchiseStore{
		subscribers: []revenue.SubscriberRow{
			{ID: 101, Username: "f1-sub@isp", FranchiseID: intPtr(1)},
			{ID: 202, Username: "f2-sub@isp", FranchiseID: intPtr(2)},
		},
	}
	handler := middleware.JWTMiddleware(itJWTSecret)(
		revenue.FranchiseMiddleware(revenue.ListSubscribersHandler(store)),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+itToken(t, "isp_owner", 0))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var rows []revenue.SubscriberRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("isp_owner must see all subscribers, got %d", len(rows))
	}
}

// TestFR_FRN_001_FranchiseIsolation_UnboundLCORefused verifies an LCO token with no
// franchise claim is refused rather than being granted ISP-wide visibility.
//
// INT-FRN-001 (supporting) | FR-FRN-001
func TestFR_FRN_001_FranchiseIsolation_UnboundLCORefused(t *testing.T) {
	store := &itFranchiseStore{
		subscribers: []revenue.SubscriberRow{
			{ID: 101, Username: "f1-sub@isp", FranchiseID: intPtr(1)},
			{ID: 202, Username: "f2-sub@isp", FranchiseID: intPtr(2)},
		},
	}
	handler := middleware.JWTMiddleware(itJWTSecret)(
		revenue.FranchiseMiddleware(revenue.ListSubscribersHandler(store)),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+itToken(t, "lco", 0)) // no franchise binding
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for an LCO token with no franchise_id, got %d", rec.Code)
	}
	if containsAny(rec.Body.String(), "f1-sub@isp", "f2-sub@isp") {
		t.Error("refused request must not return subscriber data")
	}
}

// ── INT-FRN-002 ─────────────────────────────────────────────────────────────

// TestFR_FRN_002_Commission_CorrectAmount verifies a 10% commission on a ₹799.00 recharge
// is 79.90 and is written to lco_ledger with that exact decimal amount.
//
// INT-FRN-002 | FR-FRN-002
func TestFR_FRN_002_Commission_CorrectAmount(t *testing.T) {
	store := &itFranchiseStore{
		franchises: map[int]*revenue.Franchise{
			1: {ID: 1, Name: "Chennai LCO", CommissionRatePct: decimal.RequireFromString("10.00"), Status: "active"},
		},
	}

	commission, err := revenue.CalculateLCOCommission(context.Background(), store, revenue.LCOCommissionEntry{
		FranchiseID:    1,
		SubscriberID:   101,
		RechargeAmount: decimal.RequireFromString("799.00"),
		TransactionRef: "pay_commission_001",
	})
	if err != nil {
		t.Fatalf("CalculateLCOCommission: %v", err)
	}

	want := decimal.RequireFromString("79.90")
	if !commission.Equal(want) {
		t.Errorf("commission: want %s, got %s", want, commission)
	}

	rows := store.ledgerRows()
	if len(rows) != 1 {
		t.Fatalf("want 1 lco_ledger row, got %d", len(rows))
	}
	if !rows[0].CommissionAmount.Equal(want) {
		t.Errorf("lco_ledger commission_amount: want %s, got %s", want, rows[0].CommissionAmount)
	}
	if !rows[0].RechargeAmount.Equal(decimal.RequireFromString("799.00")) {
		t.Errorf("lco_ledger recharge_amount: want 799.00, got %s", rows[0].RechargeAmount)
	}
	if !rows[0].CommissionRate.Equal(decimal.RequireFromString("10.00")) {
		t.Errorf("lco_ledger commission_rate: want 10.00, got %s", rows[0].CommissionRate)
	}
	if rows[0].TransactionRef != "pay_commission_001" {
		t.Errorf("lco_ledger transaction_ref: want pay_commission_001, got %q", rows[0].TransactionRef)
	}
}

// TestFR_FRN_002_Commission_RateVariants checks the rounding boundary across the rates the
// business actually uses.
//
// INT-FRN-002 (supporting) | FR-FRN-002
func TestFR_FRN_002_Commission_RateVariants(t *testing.T) {
	cases := []struct {
		recharge, rate, want string
	}{
		{"799.00", "10.00", "79.90"},
		{"799.00", "5.00", "39.95"},
		{"999.00", "7.50", "74.93"}, // 74.925 rounds to 74.93
		{"1499.00", "12.25", "183.63"},
	}

	for _, c := range cases {
		t.Run(c.recharge+"@"+c.rate, func(t *testing.T) {
			store := &itFranchiseStore{
				franchises: map[int]*revenue.Franchise{
					1: {ID: 1, CommissionRatePct: decimal.RequireFromString(c.rate), Status: "active"},
				},
			}
			got, err := revenue.CalculateLCOCommission(context.Background(), store, revenue.LCOCommissionEntry{
				FranchiseID:    1,
				SubscriberID:   1,
				RechargeAmount: decimal.RequireFromString(c.recharge),
			})
			if err != nil {
				t.Fatalf("CalculateLCOCommission: %v", err)
			}
			if want := decimal.RequireFromString(c.want); !got.Equal(want) {
				t.Errorf("commission on %s at %s%%: want %s, got %s", c.recharge, c.rate, want, got)
			}
		})
	}
}

// TestFR_FRN_002_Commission_InactiveFranchiseEarnsNothing verifies a suspended franchise
// accrues no commission and writes no ledger row.
//
// INT-FRN-002 (supporting) | FR-FRN-002
func TestFR_FRN_002_Commission_InactiveFranchiseEarnsNothing(t *testing.T) {
	store := &itFranchiseStore{
		franchises: map[int]*revenue.Franchise{
			1: {ID: 1, CommissionRatePct: decimal.RequireFromString("10.00"), Status: "suspended"},
		},
	}

	got, err := revenue.CalculateLCOCommission(context.Background(), store, revenue.LCOCommissionEntry{
		FranchiseID:    1,
		SubscriberID:   101,
		RechargeAmount: decimal.RequireFromString("799.00"),
	})
	if err == nil {
		t.Fatal("expected an error for a non-active franchise")
	}
	if !got.IsZero() {
		t.Errorf("want zero commission for a suspended franchise, got %s", got)
	}
	if rows := store.ledgerRows(); len(rows) != 0 {
		t.Errorf("suspended franchise must write no lco_ledger row, got %d", len(rows))
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && len(haystack) >= len(n) {
			for i := 0; i+len(n) <= len(haystack); i++ {
				if haystack[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}
