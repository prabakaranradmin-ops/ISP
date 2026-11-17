package billing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Billing diagnostics — FR-BIL-019.
//
// Three read-only checks for billing staff to work through. Each finds
// subscribers whose records disagree with each other; none of them fixes
// anything, because the right fix differs case by case (a missing invoice
// may need raising, or may be a cycle someone granted on purpose).

// UninvoicedCycle is a subscriber being served in a paid-up cycle with no
// invoice for it.
//
// "Covered" means an invoice raised since the current cycle began, or two
// raised since the previous one began. The second clause is a customer who
// renewed early from the portal: their invoice is dated before the cycle
// it pays for, inside the previous cycle alongside that cycle's own
// invoice. A day's allowance on each boundary absorbs the gap between a
// renewal computing the new expiry and writing its invoice.
type UninvoicedCycle struct {
	SubscriberID int
	Username     string
	Status       string
	PlanName     string
	PlanPrice    decimal.Decimal
	CycleStart   time.Time
	PlanExpiry   time.Time
	LastInvoice  *time.Time // nil: never invoiced
}

// BalanceMismatch is a subscriber whose stored wallet balance disagrees
// with their wallet ledger: with the sum of its credits less its debits,
// or with the balance the newest ledger row says it left.
type BalanceMismatch struct {
	SubscriberID int
	Username     string
	Stored       decimal.Decimal
	LedgerSum    decimal.Decimal
	LastAfter    *decimal.Decimal // nil: no ledger rows
	Entries      int
}

// Difference is the stored balance less what the ledger adds up to.
func (m BalanceMismatch) Difference() decimal.Decimal { return m.Stored.Sub(m.LedgerSum) }

// PriceDrift is a subscriber whose most recent plan invoice charged a
// different taxable amount from their plan's catalogue price today. One-off
// charges (a converted proforma) are not plan invoices and are ignored.
type PriceDrift struct {
	SubscriberID   int
	Username       string
	PlanName       string
	CataloguePrice decimal.Decimal
	BilledBase     decimal.Decimal
	InvoiceID      int
	InvoicedAt     time.Time
}

// Difference is what was billed less the catalogue price.
func (d PriceDrift) Difference() decimal.Decimal { return d.BilledBase.Sub(d.CataloguePrice) }

// Diagnostics is one run of all three checks. The counts are over
// everything found; the lists stop at the limit asked for.
type Diagnostics struct {
	Uninvoiced      []UninvoicedCycle
	UninvoicedCount int
	Mismatches      []BalanceMismatch
	MismatchCount   int
	Drifts          []PriceDrift
	DriftCount      int
	RanAt           time.Time
}

// Clean reports whether nothing was found.
func (d Diagnostics) Clean() bool {
	return d.UninvoicedCount == 0 && d.MismatchCount == 0 && d.DriftCount == 0
}
