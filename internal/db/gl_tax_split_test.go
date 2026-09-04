package db

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// splitTaxLeg is the whole of migration 047's correctness: it decides how
// much of a renewal charge is income and how much is a liability owed to the
// government. It needs no database, so it is tested here rather than behind
// the integration build tag — this is the arithmetic a reconciliation would
// otherwise catch months later, if at all.

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	v, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return v
}

// legsFor builds the two-leg shape postWalletGLEntry constructs for an
// auto-renewal: the counter leg credits revenue, the wallet leg debits the
// subscriber's liability balance.
func legsFor(t *testing.T, total string) []glLeg {
	t.Helper()
	return []glLeg{
		{code: "4000", entryType: "credit", amount: dec(t, total)},
		{code: "1200", entryType: "debit", amount: dec(t, total)},
	}
}

func TestSplitTaxLeg_SeparatesGstFromRevenue(t *testing.T) {
	// The real numbers from the renewal that exposed this: a 599.00 TN plan
	// at 9% CGST + 9% SGST.
	got, err := splitTaxLeg(legsFor(t, "706.82"), "4000", dec(t, "107.82"))
	if err != nil {
		t.Fatalf("splitTaxLeg: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 legs (revenue, GST, wallet), got %d", len(got))
	}

	byCode := map[string]glLeg{}
	for _, l := range got {
		byCode[l.code] = l
	}
	if want := dec(t, "599.00"); !byCode["4000"].amount.Equal(want) {
		t.Errorf("revenue leg: want %s, got %s", want, byCode["4000"].amount)
	}
	if want := dec(t, "107.82"); !byCode["2200"].amount.Equal(want) {
		t.Errorf("GST leg: want %s, got %s", want, byCode["2200"].amount)
	}
	if want := dec(t, "706.82"); !byCode["1200"].amount.Equal(want) {
		t.Errorf("wallet leg must not be split: want %s, got %s", want, byCode["1200"].amount)
	}
	// Both credit legs must sit on the same side as the leg they came from,
	// or the entry stops balancing and trg_gl_journal_balanced rejects it.
	if byCode["2200"].entryType != "credit" {
		t.Errorf("GST leg side: want credit, got %s", byCode["2200"].entryType)
	}
}

// The deferred balance trigger is the backstop, but an entry that does not
// balance is a bug here, not there — so assert it directly.
func TestSplitTaxLeg_KeepsTheEntryBalanced(t *testing.T) {
	got, err := splitTaxLeg(legsFor(t, "706.82"), "4000", dec(t, "107.82"))
	if err != nil {
		t.Fatalf("splitTaxLeg: %v", err)
	}
	debits, credits := decimal.Zero, decimal.Zero
	for _, l := range got {
		if l.entryType == "debit" {
			debits = debits.Add(l.amount)
		} else {
			credits = credits.Add(l.amount)
		}
	}
	if !debits.Equal(credits) {
		t.Errorf("entry does not balance: debits %s, credits %s", debits, credits)
	}
}

// A top-up, adjustment or refund carries no tax and must post exactly as it
// did before migration 047 — this is the regression guard for every posting
// that is not a plan charge.
func TestSplitTaxLeg_UntaxedPostingIsUnchanged(t *testing.T) {
	in := legsFor(t, "750.00")
	got, err := splitTaxLeg(in, "4000", decimal.Zero)
	if err != nil {
		t.Fatalf("splitTaxLeg: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want the original 2 legs, got %d", len(got))
	}
	for i := range in {
		if !got[i].amount.Equal(in[i].amount) || got[i].code != in[i].code {
			t.Errorf("leg %d changed: %+v -> %+v", i, in[i], got[i])
		}
	}
}

// Tax against an adjustment or gateway account would produce a 2200 balance
// no GST return could be filed from, so it is refused rather than posted.
func TestSplitTaxLeg_RejectsTaxOnANonRevenueAccount(t *testing.T) {
	legs := []glLeg{
		{code: "5200", entryType: "credit", amount: dec(t, "100.00")},
		{code: "1200", entryType: "debit", amount: dec(t, "100.00")},
	}
	if _, err := splitTaxLeg(legs, "5200", dec(t, "18.00")); err == nil {
		t.Fatal("want an error posting tax against Wallet Adjustments (5200), got none")
	}
}

// Tax equal to or larger than the charge means the caller's invoice and its
// charge have diverged; posting either reading would put a wrong number in
// the ledger.
func TestSplitTaxLeg_RejectsTaxThatExceedsTheCharge(t *testing.T) {
	if _, err := splitTaxLeg(legsFor(t, "100.00"), "4000", dec(t, "100.00")); err == nil {
		t.Fatal("want an error when tax consumes the whole charge, got none")
	}
}

// The GST account code the split posts to must be the one the migration
// actually created; a drift between them fails at insert time in production
// but is free to catch here.
func TestSplitTaxLeg_UsesTheMigratedGstAccountCode(t *testing.T) {
	if billing.GLAccountGSTPayable != "2200" {
		t.Errorf("GST payable account: want 2200 (migration 047), got %s", billing.GLAccountGSTPayable)
	}
}
