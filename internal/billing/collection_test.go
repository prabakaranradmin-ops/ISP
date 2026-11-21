package billing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001.

func item(price, balance string) CollectionItem {
	return CollectionItem{
		SubscriberID: 1, Username: "door@isp", MobileNumber: "+919876543210",
		PlanName: "Fibre 100", PlanPrice: decimal.RequireFromString(price),
		Balance: decimal.RequireFromString(balance),
	}
}

// The figure a collector asks for at the door. Getting this wrong means
// either collecting too little — and leaving the subscriber still off
// service after a visit — or too much, which is somebody's money.
func TestFR_COL_001_AmountDueIsThePlanPriceLessTheWallet(t *testing.T) {
	cases := []struct {
		name           string
		price, balance string
		want           string
	}{
		{"nothing in the wallet", "799.00", "0.00", "799"},
		{"a part payment already made", "799.00", "300.00", "499"},
		{"exactly covered", "799.00", "799.00", "0"},
		// Never negative: a subscriber in credit is not owed money by the
		// collector, and a negative figure on a doorstep list would be read
		// as change to hand back.
		{"more than covered", "799.00", "1000.00", "0"},
		{"paise survive", "799.50", "99.25", "700.25"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := item(tc.price, tc.balance).AmountDue()
			if !got.Equal(decimal.RequireFromString(tc.want)) {
				t.Errorf("AmountDue() = %s, want %s", got, tc.want)
			}
			if got.IsNegative() {
				t.Errorf("AmountDue() is negative: %s", got)
			}
		})
	}
}

// A subscriber whose wallet already covers the plan stays on the list but is
// marked, rather than disappearing. A row that silently vanishes between one
// refresh and the next looks like a bug to the collector working the round.
func TestFR_COL_001_AnAlreadyCoveredSubscriberIsMarkedNotHidden(t *testing.T) {
	if !item("799.00", "799.00").AlreadyCovered() {
		t.Error("a subscriber whose wallet covers the plan is not marked as covered")
	}
	if !item("799.00", "1200.00").AlreadyCovered() {
		t.Error("a subscriber in credit is not marked as covered")
	}
	if item("799.00", "798.99").AlreadyCovered() {
		t.Error("a subscriber one paisa short is marked as covered")
	}
}

// A collector works the lapsed subscribers first: those customers are off
// service, and the visit is what restores them.
func TestFR_COL_001_OverdueIsAboutWhetherTheServiceHasLapsed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	tomorrow := now.Add(24 * time.Hour)

	lapsed := item("799.00", "0.00")
	lapsed.PlanExpiry = &yesterday
	if !lapsed.IsOverdue(now) {
		t.Error("a subscriber whose plan expired yesterday is not overdue")
	}

	soon := item("799.00", "0.00")
	soon.PlanExpiry = &tomorrow
	if soon.IsOverdue(now) {
		t.Error("a subscriber expiring tomorrow is already overdue")
	}

	// No expiry at all is not overdue. A subscriber with none has never
	// been billed a cycle, and treating that as lapsed would put every new
	// signup at the top of somebody's round.
	never := item("799.00", "0.00")
	if never.IsOverdue(now) {
		t.Error("a subscriber with no expiry is treated as overdue")
	}
}

// Cash is the line that matters in a reconciliation: a UPI payment taken at
// the door has already reached the bank, whereas cash is in somebody's
// pocket until it is handed in.
func TestFR_COL_001_TakingsSeparateCashFromEverythingElse(t *testing.T) {
	takings := CollectorTakings{
		Collector: "ravi",
		ByMethod: map[string]decimal.Decimal{
			MethodCash:   decimal.RequireFromString("2400.00"),
			MethodUPI:    decimal.RequireFromString("1600.00"),
			MethodCheque: decimal.RequireFromString("799.00"),
		},
		Count: 6,
		Total: decimal.RequireFromString("4799.00"),
	}

	if got := takings.CashTotal(); !got.Equal(decimal.RequireFromString("2400.00")) {
		t.Errorf("CashTotal() = %s, want 2400.00 — not the whole takings", got)
	}

	// A collector who took no cash owes nothing at the counter, and that
	// has to read as zero rather than panic on a missing key.
	none := CollectorTakings{Collector: "no-cash", ByMethod: map[string]decimal.Decimal{
		MethodUPI: decimal.RequireFromString("500.00"),
	}}
	if got := none.CashTotal(); !got.IsZero() {
		t.Errorf("CashTotal() with no cash = %s, want 0", got)
	}

	// And a zero-value Takings must not panic on a nil map.
	var empty CollectorTakings
	if got := empty.CashTotal(); !got.IsZero() {
		t.Errorf("CashTotal() on a zero value = %s, want 0", got)
	}
}
