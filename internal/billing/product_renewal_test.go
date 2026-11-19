package billing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// Catalogue charges on a renewal — FR-BIL-022. The fake store and the
// scanDay/renewalNow clock live in recurring_billing_scanner_test.go.

// attachment builds a billable subscriber_products row for the fake.
func attachment(t *testing.T, id int, name, kind, hsn, price string, qty int) billing.SubscriberProduct {
	t.Helper()
	return billing.SubscriberProduct{
		ID: id, SubscriberID: 1, ProductID: id * 10, ProductCode: name,
		ProductName: name, Kind: kind, HSNSAC: hsn, Quantity: qty,
		UnitPrice: decFromString(t, price), Status: billing.AttachmentActive,
	}
}

// sumLines totals a set of lines, so a test can assert the header the
// scanner hands over is exactly its own lines.
func sumLines(lines []billing.InvoiceLine) (base, cgst, sgst, igst, total decimal.Decimal) {
	base, cgst, sgst, igst, total = decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	for _, l := range lines {
		base = base.Add(l.BaseAmount)
		cgst = cgst.Add(l.CGSTAmount)
		sgst = sgst.Add(l.SGSTAmount)
		igst = igst.Add(l.IGSTAmount)
		total = total.Add(l.TotalAmount)
	}
	return
}

// A recurring charge appears as its own GST-correct line, and the wallet is
// debited the whole invoice — plan plus charges plus tax — not just the plan.
func TestFR_BIL_022_RecurringChargeIsBilledOnTheRenewal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "rents@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanVolumeGB: 100, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}}
	db.billable[1] = []billing.SubscriberProduct{
		attachment(t, 7, "Router rental", billing.ProductRecurring, "8517", "150.00", 1),
	}
	walletDB := newFakeWalletQuerier("5000.00")
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(db.invoiceCalls) != 1 {
		t.Fatalf("want 1 invoice, got %d", len(db.invoiceCalls))
	}
	inv, lines := db.invoiceCalls[0], db.lineCalls[0]

	// 500 plan + 150 rental = 650 base; 9% each head = 58.50; total 767.00.
	if !inv.BaseAmount.Equal(decFromString(t, "650.00")) {
		t.Errorf("invoice base: want 650.00, got %s", inv.BaseAmount)
	}
	if !inv.TotalAmount.Equal(decFromString(t, "767.00")) {
		t.Errorf("invoice total: want 767.00, got %s", inv.TotalAmount)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0].Kind != billing.LineKindPlan {
		t.Errorf("line 1 is %s, want the plan", lines[0].Kind)
	}
	if lines[1].Kind != billing.LineKindProduct || lines[1].HSNSAC != "8517" {
		t.Errorf("line 2 is %s under %s, want a product under 8517", lines[1].Kind, lines[1].HSNSAC)
	}
	if lines[1].ProductID == nil || *lines[1].ProductID != 70 {
		t.Error("the product line does not name its product")
	}

	// The debit is the whole invoice. Billing a rental the subscriber was
	// never charged for would be revenue declared and not collected —
	// the same class of bug the plan price itself once had here.
	if got := walletDB.lastPosting.Credit.Amount; !got.Equal(inv.TotalAmount) {
		t.Errorf("wallet debited %s, invoice says %s", got, inv.TotalAmount)
	}
	// The ledger memo accounts for the extra, so a subscriber reading
	// their ledger can see why the cycle cost more than the plan.
	if memo := walletDB.lastPosting.Credit.Description; !strings.Contains(memo, "1 charge") {
		t.Errorf("ledger memo %q does not mention the extra charge", memo)
	}

	// A recurring charge stays billable for the next cycle.
	if len(db.closedOneTime) != 0 {
		t.Errorf("a recurring charge was closed: %v", db.closedOneTime)
	}
	if len(db.billable[1]) != 1 {
		t.Error("a recurring charge must still be billable next cycle")
	}
}

// A one-time charge is billed on the next renewal and then never again.
func TestFR_BIL_022_OneTimeChargeIsBilledOnceOnly(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	candidate := billing.RenewalCandidate{
		SubscriberID: 1, Username: "new@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanVolumeGB: 100, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}
	db.candidates = []billing.RenewalCandidate{candidate}
	db.billable[1] = []billing.SubscriberProduct{
		attachment(t, 9, "Installation", billing.ProductOneTime, "998432", "1500.00", 1),
		attachment(t, 7, "Router rental", billing.ProductRecurring, "8517", "150.00", 1),
	}
	walletDB := newFakeWalletQuerier("50000.00")
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	// First cycle: everything is billed.
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan 1: %v", err)
	}
	if n := len(db.lineCalls[0]); n != 3 {
		t.Fatalf("first cycle: want 3 lines, got %d", n)
	}
	if !db.invoiceCalls[0].BaseAmount.Equal(decFromString(t, "2150.00")) {
		t.Errorf("first cycle base: want 2150.00 (500 + 1500 + 150), got %s",
			db.invoiceCalls[0].BaseAmount)
	}
	if len(db.closedOneTime) != 1 || db.closedOneTime[0] != 9 {
		t.Errorf("the installation charge was not closed: %v", db.closedOneTime)
	}

	// Second cycle: the plan and the rental, never the installation again.
	db.candidates = []billing.RenewalCandidate{candidate}
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	if n := len(db.lineCalls[1]); n != 2 {
		t.Fatalf("second cycle: want 2 lines, got %d", n)
	}
	if !db.invoiceCalls[1].BaseAmount.Equal(decFromString(t, "650.00")) {
		t.Errorf("second cycle base: want 650.00, got %s — the one-time charge was billed twice",
			db.invoiceCalls[1].BaseAmount)
	}
	for _, l := range db.lineCalls[1] {
		if l.Description == "Installation" {
			t.Error("the installation charge was billed a second time")
		}
	}
}

// If the attached charges cannot be read, the renewal stops before the
// wallet is touched: billing the plan alone would under-bill silently, and
// nothing on the invoice would show that anything was missing.
func TestFR_BIL_022_UnreadableChargesStopTheRenewal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "rents@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}}
	db.billableErr = errors.New("db down")
	walletDB := newFakeWalletQuerier("5000.00")
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not fail the whole pass for one subscriber: %v", err)
	}
	if len(db.invoiceCalls) != 0 {
		t.Error("an invoice was raised despite unreadable charges")
	}
	if len(db.setExpiryCalls) != 0 {
		t.Error("plan_expiry moved despite unreadable charges")
	}
	if walletDB.postingCalls != 0 {
		t.Error("the wallet was debited despite unreadable charges")
	}
}

// Quantity reaches the line, and the header still equals the lines once
// quantities and the interstate regime are involved.
func TestFR_BIL_022_QuantityAndInterstateTaxOnARenewal(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "multi@isp", RegisteredState: "KA", // interstate: IGST
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}}
	db.billable[1] = []billing.SubscriberProduct{
		attachment(t, 7, "Static IP", billing.ProductRecurring, "998432", "100.00", 4),
	}
	walletDB := newFakeWalletQuerier("5000.00")
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	inv, lines := db.invoiceCalls[0], db.lineCalls[0]
	if lines[1].Quantity != 4 || !lines[1].BaseAmount.Equal(decFromString(t, "400.00")) {
		t.Errorf("4 × 100 came to quantity %d / base %s", lines[1].Quantity, lines[1].BaseAmount)
	}
	if !inv.BaseAmount.Equal(decFromString(t, "900.00")) {
		t.Errorf("invoice base: want 900.00, got %s", inv.BaseAmount)
	}
	// Interstate: IGST only, on every line.
	if !inv.CgstAmount.IsZero() || inv.IgstAmount.IsZero() {
		t.Errorf("interstate invoice carries cgst %s igst %s", inv.CgstAmount, inv.IgstAmount)
	}
	base, cgst, sgst, igst, total := sumLines(lines)
	if !inv.BaseAmount.Equal(base) || !inv.CgstAmount.Equal(cgst) || !inv.SgstAmount.Equal(sgst) ||
		!inv.IgstAmount.Equal(igst) || !inv.TotalAmount.Equal(total) {
		t.Error("the invoice the scanner hands over does not equal its own lines")
	}
}

// A subscriber with no catalogue charges invoices exactly as before this
// feature: one plan line, and a ledger memo that says only the plan.
func TestFR_BIL_022_PlainRenewalIsUnchanged(t *testing.T) {
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: billing.DunningActive}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "plain@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanVolumeGB: 100, PlanExpiry: scanDay(-1), DunningState: billing.DunningActive,
	}}
	walletDB := newFakeWalletQuerier("5000.00")
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(walletDB))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	inv, lines := db.invoiceCalls[0], db.lineCalls[0]
	if len(lines) != 1 || lines[0].Kind != billing.LineKindPlan {
		t.Fatalf("want a single plan line, got %d lines", len(lines))
	}
	// 500 + 9% + 9% = 590.00, exactly what CalculateGstInvoice produced.
	if !inv.TotalAmount.Equal(decFromString(t, "590.00")) {
		t.Errorf("invoice total: want 590.00, got %s", inv.TotalAmount)
	}
	if memo := walletDB.lastPosting.Credit.Description; strings.Contains(memo, "charge") {
		t.Errorf("ledger memo %q mentions charges on a plain renewal", memo)
	}
}
