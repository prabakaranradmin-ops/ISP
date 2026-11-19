package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Scheduled plan changes — FR-BIL-023 | migration 059.
//
// The immediate plan change (FR-LC-001, api/subscribers_lifecycle.go) moves
// a subscriber now and prorates the unused value of the old plan into bonus
// days. A scheduled change does neither: it waits for the renewal, and the
// renewal buys the next cycle on the new plan at the new plan's price.
//
// There is no proration here, and that is the point rather than an
// omission. The old cycle was paid for and used to its last day, so there
// is no unused value to convert — and prorating an end-of-cycle downgrade
// would hand out bonus days for service the customer already had.

// Scheduled plan change statuses.
const (
	// PlanChangeScheduled is waiting for a renewal on or after its date.
	PlanChangeScheduled = "scheduled"
	// PlanChangeApplied: the renewal moved them. Terminal.
	PlanChangeApplied = "applied"
	// PlanChangeCancelled: withdrawn before it fired. Terminal.
	PlanChangeCancelled = "cancelled"
)

// PlanChangeMaxReason bounds the note staff attach, matching
// migration 059's chk_spc_reason.
const PlanChangeMaxReason = 200

// PlanChangeMaxHorizon is how far ahead a change may be scheduled. A year
// is well past any plan's validity, and short enough that a mistyped year
// is refused rather than parked in the table until 2031.
const PlanChangeMaxHorizon = 365

var (
	// ErrPlanChangeInvalid wraps an operator-facing sentence.
	ErrPlanChangeInvalid = errors.New("billing: invalid scheduled plan change")
	// ErrPlanChangeNotFound: no such scheduled change.
	ErrPlanChangeNotFound = errors.New("billing: scheduled plan change not found")
	// ErrPlanChangeNotPending: already applied or already cancelled.
	ErrPlanChangeNotPending = errors.New("billing: that plan change is no longer pending")
	// ErrPlanChangeAlreadyScheduled: this subscriber already has one
	// waiting, and migration 059 allows exactly one.
	ErrPlanChangeAlreadyScheduled = errors.New("billing: this subscriber already has a plan change scheduled")
	// ErrPlanChangeSamePlan: the target plan is the one they are already
	// on, so there is nothing to schedule.
	ErrPlanChangeSamePlan = errors.New("billing: the subscriber is already on that plan")
)

// PlanChangeInvalidReason returns the sentence inside an
// ErrPlanChangeInvalid, or "".
func PlanChangeInvalidReason(err error) string {
	if !errors.Is(err, ErrPlanChangeInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrPlanChangeInvalid.Error()+": ")
}

// SchedulePlanChangeRequest is staff scheduling a change.
type SchedulePlanChangeRequest struct {
	SubscriberID int
	NewPlanID    int
	// EffectiveOn is the date from which the renewal path may apply it.
	// Normally the subscriber's current plan_expiry.
	EffectiveOn time.Time
	Reason      string
}

// Normalise trims and validates the request against `today`, returning the
// cleaned copy.
//
// The date is truncated to a day in its own location: effective_on is a
// DATE in the database, and carrying a time of day through would make
// "effective on the 1st" mean different things either side of midnight.
func (r SchedulePlanChangeRequest) Normalise(today time.Time) (SchedulePlanChangeRequest, error) {
	r.Reason = strings.Join(strings.Fields(r.Reason), " ")
	r.EffectiveOn = DayOf(r.EffectiveOn)
	today = DayOf(today)

	switch {
	case r.SubscriberID <= 0:
		return r, fmt.Errorf("%w: no subscriber given", ErrPlanChangeInvalid)
	case r.NewPlanID <= 0:
		return r, fmt.Errorf("%w: choose the plan to move them to", ErrPlanChangeInvalid)
	case r.EffectiveOn.IsZero():
		return r, fmt.Errorf("%w: give the date the change should take effect from", ErrPlanChangeInvalid)
	case r.EffectiveOn.Before(today):
		return r, fmt.Errorf("%w: the date has already passed; to change the plan now, use Change plan instead", ErrPlanChangeInvalid)
	case r.EffectiveOn.After(today.AddDate(0, 0, PlanChangeMaxHorizon)):
		return r, fmt.Errorf("%w: a change can be scheduled up to %d days ahead", ErrPlanChangeInvalid, PlanChangeMaxHorizon)
	case len([]rune(r.Reason)) > PlanChangeMaxReason:
		return r, fmt.Errorf("%w: keep the note to %d characters", ErrPlanChangeInvalid, PlanChangeMaxReason)
	}
	return r, nil
}

// DayOf truncates t to midnight in its own location.
func DayOf(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// ScheduledPlanChange is one scheduled change as stored, joined to both
// plans for display.
type ScheduledPlanChange struct {
	ID           int
	SubscriberID int
	Username     string
	NewPlanID    int
	NewPlanName  string
	// NewPlanPrice, NewPlanValidityDays and NewPlanVolumeGB are what the
	// renewal path needs to price and date the first cycle on the new
	// plan. Read at apply time, not at scheduling time: a tariff change
	// between the two must be reflected, since the cycle is bought on the
	// day it is bought.
	NewPlanPrice        decimal.Decimal
	NewPlanValidityDays int
	NewPlanVolumeGB     int
	FromPlanID          int
	FromPlanName        string
	EffectiveOn         time.Time
	Status              string
	Reason              string
	ScheduledBy         string
	ScheduledAt         time.Time
	AppliedAt           *time.Time
	AppliedInvoiceID    *int
	CancelledAt         *time.Time
	CancelledBy         string
	CancelReason        string
}

// Pending reports whether this change is still waiting to fire.
func (c ScheduledPlanChange) Pending() bool { return c.Status == PlanChangeScheduled }

// Due reports whether a pending change may be applied on the given day.
func (c ScheduledPlanChange) Due(on time.Time) bool {
	return c.Pending() && !DayOf(on).Before(DayOf(c.EffectiveOn))
}

// StatusLabel is the operator-facing status.
func (c ScheduledPlanChange) StatusLabel() string {
	switch c.Status {
	case PlanChangeApplied:
		return "Applied"
	case PlanChangeCancelled:
		return "Cancelled"
	default:
		return "Scheduled"
	}
}

// AppliedInvoiceNumber is the document number of the invoice that billed
// the first cycle on the new plan, or "" when there is none.
//
// A method rather than letting a template format the id: AppliedInvoiceID
// is a *int, and while {{.AppliedInvoiceID}} indirects the pointer,
// {{printf "%06d" .AppliedInvoiceID}} does not — it would print the
// address. Deriving the number here keeps that trap out of the templates.
func (c ScheduledPlanChange) AppliedInvoiceNumber() string {
	if c.AppliedInvoiceID == nil {
		return ""
	}
	return InvoiceNumber(*c.AppliedInvoiceID)
}

// Summary reads "Basic → Fibre 100", for a list or an audit entry.
func (c ScheduledPlanChange) Summary() string {
	return c.FromPlanName + " → " + c.NewPlanName
}

// PlanChangeFilter narrows a listing of scheduled changes.
type PlanChangeFilter struct {
	Status       string // "" for any
	SubscriberID int    // 0 for any
	Limit        int
}
