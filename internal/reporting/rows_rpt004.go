package reporting

import (
	"time"

	"github.com/shopspring/decimal"
)

// Row types for the reports added by FR-RPT-004. The catalogue in
// catalogue.go describes them; the queries are in internal/db/reporting.go.
//
// Nullable aggregates stay pointers throughout, for the reason the original
// rows already document: rendering an absent median as 0.0 claims the fastest
// possible support, and an absent collection rate as 0% ranks a territory
// bottom of a league table it has not joined.

// ExpiringRow is one subscriber whose plan is about to lapse.
type ExpiringRow struct {
	SubscriberID int       `json:"subscriber_id"`
	Username     string    `json:"username"`
	MobileNumber string    `json:"mobile_number"`
	PlanName     string    `json:"plan_name"`
	Status       string    `json:"status"`
	FranchiseID  *int      `json:"franchise_id,omitempty"`
	PlanExpiry   time.Time `json:"plan_expiry"`
	// DaysLeft is negative for an expiry already past. Those are included
	// deliberately: a renewal list that drops them loses exactly the
	// customers who are already off service.
	DaysLeft      int             `json:"days_left"`
	PlanPrice     decimal.Decimal `json:"plan_price"`
	WalletBalance decimal.Decimal `json:"wallet_balance"`
}

// AmountDue is what to ask for at renewal — the plan price less whatever the
// wallet already holds, never negative.
//
// Computed rather than stored, as FR-COL-001's round does and for the same
// reason: a subscriber can pay online between the list being drawn and the
// call being made, so a stored figure is stale the moment it is written.
func (r ExpiringRow) AmountDue() decimal.Decimal {
	due := r.PlanPrice.Sub(r.WalletBalance)
	if due.IsNegative() {
		return decimal.Zero
	}
	return due
}

// PlanChangeRow is one scheduled upgrade or downgrade.
//
// Only scheduled changes: an immediate plan change from the subscriber page
// writes no history row (migration 031's trigger is AFTER UPDATE OF status,
// so a plan change with an unchanged status fires nothing), and those are in
// the owner-only audit trail. Pulling that in would widen a staff record to
// everyone who can read a report.
type PlanChangeRow struct {
	SubscriberID int       `json:"subscriber_id"`
	Username     string    `json:"username"`
	FromPlan     string    `json:"from_plan"`
	ToPlan       string    `json:"to_plan"`
	EffectiveOn  time.Time `json:"effective_on"`
	// Status is scheduled, applied or cancelled. All three are reported: what
	// was withdrawn before it fired is as much a part of the history as what
	// happened, and a list of only the applied ones cannot explain a
	// subscriber who expected a change and did not get one.
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	ScheduledBy string     `json:"scheduled_by"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	AppliedAt   *time.Time `json:"applied_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	CancelledBy string     `json:"cancelled_by"`
}

// PaymentMethodRow is one month of one payment method.
type PaymentMethodRow struct {
	Month  time.Time `json:"month"`
	Method string    `json:"method"`
	// Payments counts the credits, so an average can be taken without
	// re-querying: one ₹50,000 cheque and fifty ₹1,000 cash payments are a
	// different operational picture at the same total.
	Payments int             `json:"payments"`
	Total    decimal.Decimal `json:"total"`
}

// StaffCollectionRow is one month of one operator's takings.
//
// Cash apart from digital, because cash has to be handed in and UPI has
// already reached the bank — the same split FR-COL-001's reconciliation makes,
// and for the same reason: a single total cannot be checked against anything.
type StaffCollectionRow struct {
	Month        time.Time       `json:"month"`
	RecordedBy   string          `json:"recorded_by"`
	Payments     int             `json:"payments"`
	CashTotal    decimal.Decimal `json:"cash_total"`
	DigitalTotal decimal.Decimal `json:"digital_total"`
	Total        decimal.Decimal `json:"total"`
}

// InvoiceRegisterRow is one invoice.
//
// There is no paid/unpaid column: this schema settles invoices against the
// wallet rather than marking them, so a per-invoice status would have to be
// inferred and would be wrong as often as not. The register reports what was
// raised; what was collected is the collections reports.
type InvoiceRegisterRow struct {
	InvoiceID    int             `json:"invoice_id"`
	InvoiceDate  time.Time       `json:"invoice_date"`
	SubscriberID int             `json:"subscriber_id"`
	Username     string          `json:"username"`
	PlanName     string          `json:"plan_name"`
	FranchiseID  *int            `json:"franchise_id,omitempty"`
	Description  string          `json:"description"`
	BaseAmount   decimal.Decimal `json:"base_amount"`
	CGST         decimal.Decimal `json:"cgst_amount"`
	SGST         decimal.Decimal `json:"sgst_amount"`
	IGST         decimal.Decimal `json:"igst_amount"`
	Total        decimal.Decimal `json:"total_amount"`
}

// TaxRow is one month at one GST rate.
//
// CGST/SGST and IGST are reported in separate columns rather than summed: the
// split is the whole point, since an intra-state supply splits the rate
// between centre and state while an inter-state one carries IGST alone, and a
// combined figure cannot be reconciled against either.
type TaxRow struct {
	Month        time.Time       `json:"month"`
	CGSTRate     decimal.Decimal `json:"cgst_rate"`
	SGSTRate     decimal.Decimal `json:"sgst_rate"`
	IGSTRate     decimal.Decimal `json:"igst_rate"`
	Invoices     int             `json:"invoices"`
	TaxableValue decimal.Decimal `json:"taxable_value"`
	CGST         decimal.Decimal `json:"cgst_amount"`
	SGST         decimal.Decimal `json:"sgst_amount"`
	IGST         decimal.Decimal `json:"igst_amount"`
	TaxTotal     decimal.Decimal `json:"tax_total"`
}

// TurnaroundRow is one month of how long tickets actually took.
//
// Distinct from the ticket-resolution report beside it: that one counts
// tickets against their SLA, this one measures elapsed time. Both are needed —
// a month can be entirely within SLA and still be getting slower.
type TurnaroundRow struct {
	Month    time.Time `json:"month"`
	Category string    `json:"category"`
	Priority string    `json:"priority"`
	Raised   int       `json:"raised"`
	Resolved int       `json:"resolved"`
	// Nil when nothing in the group was resolved.
	MedianHours *float64 `json:"median_resolution_hours"`
	// P90, not the mean: the mean hides the tail, and the tail is what a
	// customer remembers.
	P90Hours *float64 `json:"p90_resolution_hours"`
	MaxHours *float64 `json:"max_resolution_hours"`
	// StillOpen counts tickets raised in the month that have never reached
	// resolved. Reported beside the durations because a fast median over a
	// month that left thirty tickets open is not a fast month.
	StillOpen int `json:"still_open"`
}

// DataUserRow is one subscriber's consumption in the window.
type DataUserRow struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	PlanName     string `json:"plan_name"`
	FranchiseID  *int   `json:"franchise_id,omitempty"`
	Sessions     int    `json:"sessions"`
	InputOctets  int64  `json:"input_octets"`
	OutputOctets int64  `json:"output_octets"`
	TotalBytes   int64  `json:"total_bytes"`
	// AllowanceBytes is the plan's fair-use threshold, 0 for an unlimited
	// plan. Reported beside the usage so a large number can be read as
	// "within what they bought" or "well past it" without a second lookup.
	AllowanceBytes int64 `json:"allowance_bytes"`
}

// FUPExceededRow is one subscriber currently over their allowance.
//
// "Currently" is measured the way the scanner measures it — summed across
// open sessions — so this report and the enforcement cannot disagree about
// who is over. A closed-session sum would list people whose throttle has
// already been lifted.
type FUPExceededRow struct {
	SubscriberID   int    `json:"subscriber_id"`
	Username       string `json:"username"`
	PlanName       string `json:"plan_name"`
	FranchiseID    *int   `json:"franchise_id,omitempty"`
	UsedBytes      int64  `json:"used_bytes"`
	AllowanceBytes int64  `json:"allowance_bytes"`
	// TopupBytes is extra allowance bought in this cycle (FR-CAT-003). Shown
	// because it raises the thresholds, so a subscriber listed as over
	// without it may be under it with one.
	TopupBytes int64 `json:"topup_bytes"`
	// AppliedRate is what they are actually throttled to right now, empty if
	// nothing has been applied yet. The gap between "over the allowance" and
	// "throttled" is the scanner's next pass, and seeing it is how an
	// operator tells a stuck scanner from a working one.
	AppliedRate string `json:"applied_rate"`
	FUPActive   bool   `json:"fup_active"`
}

// OveragePct is how far past the allowance the subscriber is, nil for an
// unlimited plan — where there is no allowance to be a percentage of.
func (r FUPExceededRow) OveragePct() *float64 {
	allowance := r.AllowanceBytes + r.TopupBytes
	if allowance <= 0 {
		return nil
	}
	pct := float64(r.UsedBytes) / float64(allowance) * 100
	return &pct
}
