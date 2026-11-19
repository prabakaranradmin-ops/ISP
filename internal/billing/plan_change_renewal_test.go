package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Scheduled plan changes applied by the renewal path — FR-BIL-023. The
// fake store and the scanDay/renewalNow clock live in
// recurring_billing_scanner_test.go.

// plans used across these tests: Basic is what they are on, Fibre is what
// they are moving to. Different price, validity and volume, so a cycle
// billed on the wrong one is visible in every field rather than just one.
const (
	basicPlanID = 1
	fibrePlanID = 2
)

func scheduledChange(t *testing.T, id int, effectiveOn time.Time) *billing.ScheduledPlanChange {
	t.Helper()
	return &billing.ScheduledPlanChange{
		ID: id, SubscriberID: 1, Status: billing.PlanChangeScheduled,
		FromPlanID: basicPlanID, FromPlanName: "Basic",
		NewPlanID: fibrePlanID, NewPlanName: "Fibre 100",
		NewPlanPrice:        decFromString(t, "1200.00"),
		NewPlanValidityDays: 60,
		NewPlanVolumeGB:     500,
		EffectiveOn:         effectiveOn,
	}
}

// basicCandidate is a subscriber on Basic whose cycle has run out.
func basicCandidate(t *testing.T) billing.RenewalCandidate {
	t.Helper()
	return billing.RenewalCandidate{
		SubscriberID: 1, Username: "mover@isp", RegisteredState: "TN",
		PlanID: basicPlanID, PlanName: "Basic",
		PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30, PlanVolumeGB: 100,
		PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}
}

func planChangeScanner(t *testing.T, db *fakeRenewalScanQuerier, balance string) (*billing.RecurringBillingScanner, *fakeWalletQuerier) {
	t.Helper()
	walletDB := newFakeWalletQuerier(balance)
	s := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(s, func() time.Time { return renewalNow })
	return s, walletDB
}

// The whole point of the feature: the cycle is bought on the NEW plan —
// priced, debited, dated and invoiced as the new plan throughout.
func TestFR_BIL_023_DueChangeBillsTheNewPlanForTheWholeCycle(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	db.duePlanChange[1] = scheduledChange(t, 77, scanDay(-1))

	scanner, walletDB := planChangeScanner(t, db, "5000.00")
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(db.invoiceCalls) != 1 {
		t.Fatalf("want 1 invoice, got %d", len(db.invoiceCalls))
	}
	inv, lines := db.invoiceCalls[0], db.lineCalls[0]

	// Priced as Fibre (1200), not Basic (500). 1200 + 9% + 9% = 1416.
	if !inv.BaseAmount.Equal(decFromString(t, "1200.00")) {
		t.Errorf("invoice base %s, want 1200.00 — the cycle was billed on the wrong plan", inv.BaseAmount)
	}
	if !inv.TotalAmount.Equal(decFromString(t, "1416.00")) {
		t.Errorf("invoice total %s, want 1416.00", inv.TotalAmount)
	}
	// The usage summary describes the plan they now have.
	if inv.GbIncluded != 500 {
		t.Errorf("invoice gb_included %d, want 500", inv.GbIncluded)
	}
	// The plan line names the new plan, so the document explains itself.
	if len(lines) != 1 || lines[0].Description != "Internet Plan — Fibre 100" {
		t.Errorf("plan line reads %q", lines[0].Description)
	}
	// The wallet was debited the new plan's taxed total.
	if got := walletDB.lastPosting.Credit.Amount; !got.Equal(inv.TotalAmount) {
		t.Errorf("wallet debited %s, invoice says %s", got, inv.TotalAmount)
	}

	// Moved onto the new plan, dated by the NEW plan's validity (60 days),
	// with no proration bonus — the distinction from FR-LC-001.
	if len(db.applied) != 1 {
		t.Fatalf("want 1 applied change, got %d", len(db.applied))
	}
	got := db.applied[0]
	if got.changeID != 77 || got.subscriberID != 1 || got.newPlanID != fibrePlanID {
		t.Errorf("applied %+v", got)
	}
	wantExpiry := renewalNow.AddDate(0, 0, 60)
	if !got.expiry.Equal(wantExpiry) {
		t.Errorf("new expiry %v, want %v (60 days, no proration bonus)", got.expiry, wantExpiry)
	}
	if got.activate {
		t.Error("an existing subscriber was activated as a first cycle")
	}
	// The invoice is linked back to the change.
	if db.changeInvoice[77] == 0 {
		t.Error("the applied change was not linked to its invoice")
	}
	// The ordinary expiry path must not also have run.
	if len(db.activateCalls) != 0 {
		t.Error("ActivateSubscriber ran for a plan change on an active subscriber")
	}
}

// A change dated in the future is not touched: it waits for the renewal on
// or after its date, which is what "scheduled" means.
func TestFR_BIL_023_FutureChangeIsLeftAloneAndTheOldPlanRenews(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	// The store filters by date; a future change simply is not returned.
	// Modelled by leaving duePlanChange empty, which is what
	// GetDuePlanChange answers for it.
	scanner, _ := planChangeScanner(t, db, "5000.00")

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(db.applied) != 0 {
		t.Error("a change not yet due was applied")
	}
	// Renewed on Basic: 500 + 18% = 590, 30 days.
	if !db.invoiceCalls[0].TotalAmount.Equal(decFromString(t, "590.00")) {
		t.Errorf("invoice total %s, want 590.00 on the old plan", db.invoiceCalls[0].TotalAmount)
	}
	if got, want := db.setExpiryCalls[1], renewalNow.AddDate(0, 0, 30); !got.Equal(want) {
		t.Errorf("expiry %v, want %v", got, want)
	}
}

// A subscriber who cannot pay stays on the plan they have, with the change
// still waiting. Moving them first would hand over a plan they have not
// bought.
func TestFR_BIL_023_ChangeIsNotAppliedWhenTheDebitFails(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	db.duePlanChange[1] = scheduledChange(t, 77, scanDay(-1))

	// Enough for Basic, not for Fibre's taxed total — exactly the case the
	// candidate query's price-only pre-filter lets through.
	scanner, walletDB := planChangeScanner(t, db, "700.00")
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the pass over one short subscriber: %v", err)
	}

	if len(db.applied) != 0 {
		t.Error("the plan change was applied without the subscriber paying for it")
	}
	if len(db.invoiceCalls) != 0 {
		t.Error("an invoice was raised for a renewal that did not happen")
	}
	if len(db.setExpiryCalls) != 0 {
		t.Error("the expiry moved for a renewal that did not happen")
	}
	// No posting at all: WalletService.Post compares the balance before it
	// builds the double-entry, so an unaffordable renewal never reaches
	// the ledger rather than being written and rolled back.
	if walletDB.postingCalls != 0 {
		t.Errorf("wallet postings: %d, want none — the balance check should refuse first", walletDB.postingCalls)
	}
	// Still pending, so a later pass applies it once they top up.
	if db.duePlanChange[1] == nil {
		t.Error("the change was consumed despite not being applied")
	}
}

// If the move itself fails after the debit, it is reported as a charged
// failure at its own stage, so the Billing Runs screen names a customer who
// paid for a plan they did not get.
func TestFR_BIL_023_FailedMoveIsReportedAsAChargedFailure(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	db.duePlanChange[1] = scheduledChange(t, 77, scanDay(-1))
	db.applyErr = errors.New("db down")

	var recorded billing.RunSummary
	rec := &capturingRunRecorder{onFinish: func(sum billing.RunSummary) { recorded = sum }}
	scanner, walletDB := planChangeScanner(t, db, "5000.00")
	scanner.SetRunRecorder(rec)

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if walletDB.postingCalls != 1 {
		t.Fatalf("want the debit to have happened, postings=%d", walletDB.postingCalls)
	}
	if len(recorded.Failures) != 1 {
		t.Fatalf("want 1 recorded failure, got %+v", recorded.Failures)
	}
	f := recorded.Failures[0]
	if f.Stage != billing.StagePlanChange {
		t.Errorf("failure stage %q, want %q", f.Stage, billing.StagePlanChange)
	}
	if !f.Charged {
		t.Error("a plan-change failure after the debit was not recorded as charged")
	}
	if f.SubscriberID != 1 {
		t.Errorf("failure names subscriber %d", f.SubscriberID)
	}
	// No invoice: the renewal stopped at the failed move.
	if len(db.invoiceCalls) != 0 {
		t.Error("an invoice was written after the move failed")
	}
}

// A change naming the plan the subscriber is already on is not a move. The
// renewal proceeds normally rather than failing or double-handling it.
func TestFR_BIL_023_ChangeToTheCurrentPlanIsNotTreatedAsAMove(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	already := scheduledChange(t, 77, scanDay(-1))
	already.NewPlanID = basicPlanID // somebody already moved them here
	db.duePlanChange[1] = already

	scanner, _ := planChangeScanner(t, db, "5000.00")
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(db.applied) != 0 {
		t.Error("a no-op change was applied")
	}
	// Renewed on Basic as usual.
	if !db.invoiceCalls[0].TotalAmount.Equal(decFromString(t, "590.00")) {
		t.Errorf("invoice total %s, want 590.00", db.invoiceCalls[0].TotalAmount)
	}
	if len(db.setExpiryCalls) != 1 {
		t.Error("the ordinary expiry path did not run")
	}
}

// An unreadable schedule stops the renewal before anything is charged,
// rather than quietly renewing the plan the customer asked to leave.
func TestFR_BIL_023_UnreadableScheduleStopsTheRenewal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{basicCandidate(t)}
	db.dueChangeErr = errors.New("db down")

	scanner, walletDB := planChangeScanner(t, db, "5000.00")
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the whole pass: %v", err)
	}
	if walletDB.postingCalls != 0 {
		t.Error("the wallet was debited despite an unreadable schedule")
	}
	if len(db.invoiceCalls) != 0 || len(db.setExpiryCalls) != 0 {
		t.Error("the renewal proceeded despite an unreadable schedule")
	}
}

// A first cycle and a due change together: the subscriber is activated and
// moved in one step, because plan_id, plan_expiry and status must not be
// settable independently (migration 048).
func TestFR_BIL_023_FirstCycleWithADueChangeActivatesAndMovesTogether(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	c := basicCandidate(t)
	c.Status = billing.StatusPendingPayment
	c.PlanExpiry = time.Time{} // never had a cycle
	db.candidates = []billing.RenewalCandidate{c}
	db.duePlanChange[1] = scheduledChange(t, 77, scanDay(-1))

	scanner, _ := planChangeScanner(t, db, "5000.00")
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(db.applied) != 1 {
		t.Fatalf("want 1 applied change, got %d", len(db.applied))
	}
	if !db.applied[0].activate {
		t.Error("a first cycle was not activated by the plan-change path")
	}
	// Not through ActivateSubscriber: that would be a second statement,
	// which is the window migration 048 exists to close.
	if len(db.activateCalls) != 0 {
		t.Error("ActivateSubscriber ran alongside the plan-change path")
	}
}

// capturingRunRecorder is the minimum RunRecorder a test needs to read the
// summary a pass reported.
type capturingRunRecorder struct {
	onFinish func(billing.RunSummary)
}

func (r *capturingRunRecorder) StartRun(context.Context, string) (int, error) { return 1, nil }

func (r *capturingRunRecorder) FinishRun(_ context.Context, _ int, _ string, sum billing.RunSummary) error {
	if r.onFinish != nil {
		r.onFinish(sum)
	}
	return nil
}
