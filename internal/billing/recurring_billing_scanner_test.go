package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// fakeRenewalScanQuerier is a package-level test double for
// billing.RenewalScanQuerier. Its DunningQuerier half is a per-subscriber
// state map (like dunning_scanner_test.go's scanDB, not billing_test.go's
// single-value fakeDunningQuerier) because these tests need multiple
// candidates to carry independent states — TransitionDunning's real state
// machine still runs unmocked; only the SQL is faked.
type fakeRenewalScanQuerier struct {
	states   map[int]billing.DunningState
	setCalls []dunningSetCall

	candidates []billing.RenewalCandidate
	listErr    error

	setExpiryCalls          map[int]time.Time
	failExpiryForSubscriber int // 0 = never fail

	invoiceCalls     []billing.Invoice
	createInvoiceErr error

	gstRate    billing.GstRate
	gstRateErr error
}

func newFakeRenewalScanQuerier() *fakeRenewalScanQuerier {
	return &fakeRenewalScanQuerier{
		states:         map[int]billing.DunningState{},
		setExpiryCalls: map[int]time.Time{},
		gstRate: billing.GstRate{
			ID: 1, CgstRate: decFromString(nil, "9"), SgstRate: decFromString(nil, "9"), IgstRate: decFromString(nil, "18"),
		},
	}
}

func (f *fakeRenewalScanQuerier) GetSubscriberDunningState(_ context.Context, id int) (billing.DunningState, time.Time, error) {
	return f.states[id], time.Time{}, nil
}

func (f *fakeRenewalScanQuerier) SetSubscriberDunningState(_ context.Context, id int, state billing.DunningState, status string) error {
	f.states[id] = state
	f.setCalls = append(f.setCalls, dunningSetCall{state: state, status: status})
	return nil
}

// decFromString parses a decimal, failing the test (or panicking, when t is
// nil — used only from newFakeRenewalScanQuerier's fixed constant) on a bad
// literal so a typo in a test's own fixture cannot masquerade as the
// production code being wrong.
func decFromString(t *testing.T, s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		if t != nil {
			t.Fatalf("parse decimal %q: %v", s, err)
		}
		panic(err)
	}
	return v
}

func (f *fakeRenewalScanQuerier) ListRenewalCandidates(context.Context) ([]billing.RenewalCandidate, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.candidates, nil
}

func (f *fakeRenewalScanQuerier) SetPlanExpiry(_ context.Context, subscriberID int, expiry time.Time) error {
	if f.failExpiryForSubscriber != 0 && subscriberID == f.failExpiryForSubscriber {
		return errors.New("db down")
	}
	f.setExpiryCalls[subscriberID] = expiry
	return nil
}

func (f *fakeRenewalScanQuerier) CreateInvoice(_ context.Context, inv billing.Invoice) (int, error) {
	if f.createInvoiceErr != nil {
		return 0, f.createInvoiceErr
	}
	f.invoiceCalls = append(f.invoiceCalls, inv)
	return len(f.invoiceCalls), nil
}

func (f *fakeRenewalScanQuerier) GetActiveGstRate(context.Context) (billing.GstRate, error) {
	if f.gstRateErr != nil {
		return billing.GstRate{}, f.gstRateErr
	}
	return f.gstRate, nil
}

// scanDay pins a fixed instant so plan_expiry arithmetic is deterministic —
// the same reasoning dunning_scanner_test.go's day() helper documents.
func scanDay(n float64) time.Time {
	return time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(n * float64(24*time.Hour)))
}

var renewalNow = scanDay(0)

// TestRecurringBillingScanner_RenewsFundedExpiredSubscriber is the happy
// path: debits the plan price, extends plan_expiry by validity_days from the
// (already-passed) expiry, and invoices the cycle.
func TestRecurringBillingScanner_RenewsFundedExpiredSubscriber(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "funded@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanVolumeGB: 100, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}}
	wallet := billing.NewWalletService(newFakeWalletQuerier("600.00"))
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	newExpiry, ok := db.setExpiryCalls[1]
	if !ok {
		t.Fatal("want plan_expiry to have been extended")
	}
	// max(now, currentExpiry) + 30 days: currentExpiry (scanDay(-1)) is
	// already in the past, so base is renewalNow.
	want := renewalNow.AddDate(0, 0, 30)
	if !newExpiry.Equal(want) {
		t.Errorf("new plan_expiry: want %v, got %v", want, newExpiry)
	}
	if len(db.invoiceCalls) != 1 {
		t.Fatalf("want exactly 1 invoice, got %d", len(db.invoiceCalls))
	}
	if !db.invoiceCalls[0].BaseAmount.Equal(decFromString(t, "500.00")) {
		t.Errorf("invoice base_amount: want 500.00, got %s", db.invoiceCalls[0].BaseAmount)
	}
	if db.invoiceCalls[0].GbIncluded != 100 {
		t.Errorf("invoice gb_included: want 100, got %d", db.invoiceCalls[0].GbIncluded)
	}
}

// TestRecurringBillingScanner_InsufficientBalanceIsNotAnError verifies the
// candidate/debit race (balance moved between the SQL query and the debit)
// is treated as "leave it for dunning," not a scan failure.
func TestRecurringBillingScanner_InsufficientBalanceIsNotAnError(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "raced@isp", RegisteredState: "TN",
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1),
		DunningState: billing.DunningActive,
	}}
	wallet := billing.NewWalletService(newFakeWalletQuerier("0.00")) // moved away since the candidate query ran
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail on an insufficient-balance race: %v", err)
	}
	if len(db.setExpiryCalls) != 0 {
		t.Error("plan_expiry must not move when the debit was rejected")
	}
	if len(db.invoiceCalls) != 0 {
		t.Error("no invoice may be created when the debit was rejected")
	}
}

// TestRecurringBillingScanner_RestoresDunningStateImmediately covers a
// subscriber already escalated before auto-renewal existed: renewal must not
// wait up to an hour for the next dunning tick to notice the future expiry.
func TestRecurringBillingScanner_RestoresDunningStateImmediately(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningHardSuspended}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "suspended@isp", RegisteredState: "TN",
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-10),
		DunningState: billing.DunningHardSuspended,
	}}
	wallet := billing.NewWalletService(newFakeWalletQuerier("600.00"))
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := db.states[1]; got != billing.DunningActive {
		t.Errorf("dunning_state after renewal: want active, got %s", got)
	}
}

// TestRecurringBillingScanner_InvoiceFailureDoesNotUndoTheDebit verifies the
// "money moved, log the rest" rule: an invoicing failure must not leave the
// subscriber uncharged on a retry by reversing the debit.
func TestRecurringBillingScanner_InvoiceFailureDoesNotUndoTheDebit(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	// A *persistence* failure, not a rate failure. The two are no longer the
	// same thing: the invoice is computed before the debit (its total is what
	// the subscriber owes), so an unavailable GST rate now stops the renewal
	// before any money moves — covered by
	// TestRecurringBillingScanner_MissingGstRateChargesNothing below. What
	// this test protects is the other half: once the debit has committed, a
	// failure to write the invoice row must not unwind it.
	db.createInvoiceErr = errors.New("invoices table unavailable")
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "sub@isp", RegisteredState: "TN",
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1),
		DunningState: billing.DunningActive,
	}}
	fakeWallet := newFakeWalletQuerier("600.00")
	wallet := billing.NewWalletService(fakeWallet)
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the batch on an invoice error: %v", err)
	}
	if fakeWallet.postingCalls != 1 {
		t.Errorf("the wallet debit must have been written despite the invoice failure, got %d postings", fakeWallet.postingCalls)
	}
	if _, ok := db.setExpiryCalls[1]; !ok {
		t.Error("plan_expiry must still have been extended despite the invoice failure")
	}
}

// TestRecurringBillingScanner_ChargesTheTaxInclusiveTotal pins the
// relationship the billing path got wrong: plans.price is GST-exclusive, so
// the amount debited must equal the invoice total, not the bare price.
//
// It previously debited the price while invoicing price + GST, so every
// renewal collected 18% less than it billed. That is not a rounding
// discrepancy — the invoice is what goes on GSTR-1, so the operator remitted
// tax on money never collected, and the shortfall compounded every cycle.
func TestRecurringBillingScanner_ChargesTheTaxInclusiveTotal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "sub@isp", RegisteredState: "TN", // intrastate: 9% + 9%
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1),
		DunningState: billing.DunningActive,
	}}
	fakeWallet := newFakeWalletQuerier("1000.00")
	wallet := billing.NewWalletService(fakeWallet)
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(db.invoiceCalls) != 1 {
		t.Fatalf("want 1 invoice, got %d", len(db.invoiceCalls))
	}
	inv := db.invoiceCalls[0]

	// 500 + 9% + 9% = 590.00
	if got := inv.TotalAmount.StringFixed(2); got != "590.00" {
		t.Errorf("invoice total: got %s, want 590.00", got)
	}
	if got := fakeWallet.lastPosting.Credit.Amount.StringFixed(2); got != "590.00" {
		t.Errorf("wallet was debited %s but invoiced %s — the subscriber is billed for tax "+
			"that was never collected, and the difference is remitted on GSTR-1 out of pocket",
			got, inv.TotalAmount.StringFixed(2))
	}
}

// TestRecurringBillingScanner_MissingGstRateChargesNothing — with no GST rate
// there is no lawful invoice, and charging without one takes money with no
// tax record behind it. Refusing to renew leaves the subscriber their balance
// and lets dunning handle them, which is the recoverable outcome.
func TestRecurringBillingScanner_MissingGstRateChargesNothing(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.gstRateErr = errors.New("gst_rates unavailable")
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "sub@isp", RegisteredState: "TN",
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1),
		DunningState: billing.DunningActive,
	}}
	fakeWallet := newFakeWalletQuerier("1000.00")
	wallet := billing.NewWalletService(fakeWallet)
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the batch: %v", err)
	}
	if fakeWallet.postingCalls != 0 {
		t.Errorf("charged %d time(s) with no GST rate available — money taken with no invoice behind it",
			fakeWallet.postingCalls)
	}
	if _, ok := db.setExpiryCalls[1]; ok {
		t.Error("plan_expiry was extended without a charge: free service, and the subscriber " +
			"is no longer a renewal candidate so it would not self-correct")
	}
}

// TestRecurringBillingScanner_OneFailureDoesNotStopTheRun verifies a
// subscriber whose plan_expiry write fails after their debit already
// committed does not prevent the next candidate in the batch from renewing
// fully — the same batch-isolation guarantee dunning_scanner_test.go's
// TestFR_BIL_004_Scanner_OneFailureDoesNotStopTheRun asserts for dunning.
func TestRecurringBillingScanner_OneFailureDoesNotStopTheRun(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive, 2: billing.DunningActive}
	db.failExpiryForSubscriber = 1
	db.candidates = []billing.RenewalCandidate{
		{SubscriberID: 1, Username: "broken@isp", RegisteredState: "TN",
			PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive},
		{SubscriberID: 2, Username: "fine@isp", RegisteredState: "TN",
			PlanPrice: decFromString(t, "300.00"), PlanValidityDays: 30, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive},
	}
	fakeWallet := newFakeWalletQuerier("1000.00")
	wallet := billing.NewWalletService(fakeWallet)
	scanner := billing.NewRecurringBillingScanner(db, wallet)
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the whole run: %v", err)
	}
	if _, ok := db.setExpiryCalls[1]; ok {
		t.Error("subscriber 1's plan_expiry write was made to fail — it must not appear as extended")
	}
	if _, ok := db.setExpiryCalls[2]; !ok {
		t.Error("subscriber 2 must still have been renewed despite subscriber 1's failure")
	}
	if len(db.invoiceCalls) != 1 {
		t.Errorf("want exactly 1 invoice (subscriber 2's; subscriber 1 failed before reaching invoicing), got %d", len(db.invoiceCalls))
	}
	// Both debits still committed — subscriber 1's money moved even though
	// the expiry write that should have followed it failed, matching the
	// "log for reconciliation, do not reverse" rule this scanner uses
	// throughout.
	if fakeWallet.postingCalls != 2 {
		t.Errorf("want both subscribers debited (2 postings), got %d", fakeWallet.postingCalls)
	}
}
