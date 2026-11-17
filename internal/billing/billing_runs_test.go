package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Recurring-billing run history — FR-BIL-020.

type fakeRunRecorder struct {
	startErr  error
	started   int
	finished  int
	gotKind   string
	gotStatus string
	got       billing.RunSummary
}

func (f *fakeRunRecorder) StartRun(_ context.Context, kind string) (int, error) {
	if f.startErr != nil {
		return 0, f.startErr
	}
	f.started++
	f.gotKind = kind
	return 42, nil
}

func (f *fakeRunRecorder) FinishRun(_ context.Context, id int, status string, s billing.RunSummary) error {
	if id != 42 {
		return errors.New("wrong run id")
	}
	f.finished++
	f.gotStatus, f.got = status, s
	return nil
}

func runCandidate(t *testing.T, id int, price string, status string) billing.RenewalCandidate {
	return billing.RenewalCandidate{SubscriberID: id, Username: "sub", RegisteredState: "TN", PlanName: "Plan",
		PlanPrice: decFromString(t, price), PlanValidityDays: 30, PlanExpiry: scanDay(-1),
		DunningState: billing.DunningActive, Status: status}
}

func scanWith(t *testing.T, db *fakeRenewalScanQuerier, balance string, rec billing.RunRecorder) error {
	t.Helper()
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(newFakeWalletQuerier(balance)))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })
	if rec != nil {
		scanner.SetRunRecorder(rec)
	}
	return scanner.Scan(context.Background())
}

func TestFR_BIL_020_RunRecordsWhatThePassDid(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.failExpiryForSubscriber = 3
	// One wallet shared by all four (the fake's design): 400.00 covers three
	// 118.00 charges, leaving 46.00, which does not cover the fourth.
	db.candidates = []billing.RenewalCandidate{
		runCandidate(t, 1, "100.00", billing.StatusActive),
		runCandidate(t, 2, "100.00", billing.StatusPendingPayment),
		runCandidate(t, 3, "100.00", billing.StatusActive), // charged, then the expiry write fails
		runCandidate(t, 4, "10000.00", billing.StatusActive),
	}
	rec := &fakeRunRecorder{}
	if err := scanWith(t, db, "400.00", rec); err != nil {
		t.Fatal(err)
	}
	if rec.started != 1 || rec.finished != 1 || rec.gotKind != billing.RunKindRecurringBilling || rec.gotStatus != billing.RunCompleted {
		t.Fatalf("started %d finished %d kind %q status %q", rec.started, rec.finished, rec.gotKind, rec.gotStatus)
	}
	s := rec.got
	if s.Candidates != 4 || s.Renewed != 1 || s.Activated != 1 || s.Failed != 1 || s.InsufficientBalance != 1 {
		t.Errorf("counts: %+v", s)
	}
	if s.InvoicesRaised != 2 || s.InvoiceFailures != 0 {
		t.Errorf("invoices: raised %d, failed %d", s.InvoicesRaised, s.InvoiceFailures)
	}
	// Three charges went through, including the one whose expiry then failed.
	if s.AmountCharged.String() != "354" {
		t.Errorf("charged: want 354, got %s", s.AmountCharged)
	}
	if len(s.Failures) != 1 {
		t.Fatalf("failures: %+v", s.Failures)
	}
	f := s.Failures[0]
	if f.SubscriberID != 3 || f.Stage != billing.StageExpiry || !f.Charged || f.Message == "" {
		t.Errorf("a charge without the renewal must be named, with its step: %+v", f)
	}
}

func TestFR_BIL_020_InvoiceFailureIsRecordedAgainstARenewal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.createInvoiceErr = errors.New("invoices table locked")
	db.candidates = []billing.RenewalCandidate{runCandidate(t, 1, "100.00", billing.StatusActive)}
	rec := &fakeRunRecorder{}
	if err := scanWith(t, db, "200.00", rec); err != nil {
		t.Fatal(err)
	}
	s := rec.got
	// Renewed — the customer got their cycle — but with no invoice.
	if s.Renewed != 1 || s.Failed != 0 || s.InvoicesRaised != 0 || s.InvoiceFailures != 1 {
		t.Errorf("counts: %+v", s)
	}
	if len(s.Failures) != 1 || s.Failures[0].Stage != billing.StageInvoice || !s.Failures[0].Charged {
		t.Errorf("failures: %+v", s.Failures)
	}
}

func TestFR_BIL_020_FailureBeforeTheChargeChargesNothing(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.gstRateErr = errors.New("no GST rate in force")
	db.candidates = []billing.RenewalCandidate{runCandidate(t, 1, "100.00", billing.StatusActive)}
	rec := &fakeRunRecorder{}
	if err := scanWith(t, db, "200.00", rec); err != nil {
		t.Fatal(err)
	}
	s := rec.got
	if s.Failed != 1 || !s.AmountCharged.IsZero() || len(s.Failures) != 1 ||
		s.Failures[0].Stage != billing.StagePrice || s.Failures[0].Charged {
		t.Errorf("summary: %+v", s)
	}
}

func TestFR_BIL_020_APassThatCannotRunIsRecordedAsFailed(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.listErr = errors.New("connection refused")
	rec := &fakeRunRecorder{}
	if err := scanWith(t, db, "0.00", rec); err == nil {
		t.Fatal("the scan error must still be returned")
	}
	if rec.finished != 1 || rec.gotStatus != billing.RunFailed || rec.got.Error == "" {
		t.Errorf("status %q, error %q", rec.gotStatus, rec.got.Error)
	}
}

func TestFR_BIL_020_RenewalsNeverWaitOnTheRecord(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.candidates = []billing.RenewalCandidate{runCandidate(t, 1, "100.00", billing.StatusActive)}
	rec := &fakeRunRecorder{startErr: errors.New("billing_runs unavailable")}
	if err := scanWith(t, db, "200.00", rec); err != nil {
		t.Fatalf("a recorder failure must not fail the pass: %v", err)
	}
	if _, ok := db.setExpiryCalls[1]; !ok || len(db.invoiceCalls) != 1 {
		t.Error("the subscriber must still be renewed and invoiced")
	}
	if rec.finished != 0 {
		t.Error("with no run opened there is nothing to finish")
	}
}

func TestFR_BIL_020_StageCharged(t *testing.T) {
	for stage, want := range map[string]bool{
		billing.StagePrice: false, billing.StageDebit: false, billing.StageExpiry: true,
		billing.StageActivation: true, billing.StageInvoice: true, billing.StageDunning: true,
	} {
		if billing.StageCharged(stage) != want {
			t.Errorf("%s: want %v", stage, want)
		}
	}
}
