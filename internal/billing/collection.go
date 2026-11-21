package billing

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001 | migration 068.
//
// A collector's round: who to visit, how much to ask for, and what each
// collector has taken. Deliberately thin — payments go through the existing
// wallet credit with its method and recorded_by (FR-BIL-013), so there is no
// second money path here and nothing to reconcile between two tables.

// CollectionItem is one subscriber on a collector's round.
type CollectionItem struct {
	SubscriberID int
	Username     string
	// MobileNumber is what a collector needs in the field. There is no
	// address column on subscribers, so a round is worked from the phone
	// number and whatever the collector already knows; adding an address is
	// its own change, not something to smuggle in here.
	MobileNumber string

	PlanName   string
	PlanPrice  decimal.Decimal
	Balance    decimal.Decimal
	PlanExpiry *time.Time

	Status       string
	DunningState string
}

// AmountDue is what the collector should ask for: the plan's price less
// whatever is already in the wallet, never negative.
//
// Computed rather than stored because both halves move on their own — a
// subscriber can top up online between the list being drawn and the
// collector arriving, and a stored figure would send somebody to collect
// money that had already been paid.
func (c CollectionItem) AmountDue() decimal.Decimal {
	due := c.PlanPrice.Sub(c.Balance)
	if due.IsNegative() {
		return decimal.Zero
	}
	return due
}

// IsOverdue reports whether the plan has already lapsed, as opposed to
// being due soon. A collector works the lapsed ones first: those customers
// are off service and the visit restores them.
func (c CollectionItem) IsOverdue(now time.Time) bool {
	return c.PlanExpiry != nil && c.PlanExpiry.Before(now)
}

// AlreadyCovered reports whether the wallet already holds the plan price, so
// the visit is unnecessary. Shown rather than filtered out: a collector part
// way through a round needs to know why somebody dropped off it, and silently
// vanishing rows look like a bug.
func (c CollectionItem) AlreadyCovered() bool {
	return c.AmountDue().IsZero()
}

// CollectorTakings is one collector's recorded collections over a period,
// for the reconciliation a cash round needs.
//
// Cash collection without reconciliation is where money goes missing, so
// this is part of the feature rather than a later report: the figure a
// collector is asked to hand over has to come from the same rows the
// subscriber's receipt came from.
type CollectorTakings struct {
	Collector string
	// ByMethod is the total per payment method. Cash is the line that
	// matters — a UPI payment taken at the door has already reached the
	// bank, whereas cash is in somebody's pocket until it is handed in.
	ByMethod map[string]decimal.Decimal
	Count    int
	Total    decimal.Decimal
}

// CashTotal is the part of the takings that physically has to be handed in.
func (t CollectorTakings) CashTotal() decimal.Decimal {
	if t.ByMethod == nil {
		return decimal.Zero
	}
	return t.ByMethod[MethodCash]
}

// ErrNotYourCollection is returned when a collector tries to record a
// payment for a subscriber who is not on their round.
//
// The assignment is a permission, not a convenience. Field collectors are
// not billing staff: widening the general payment gate to them would let any
// of them record a payment for any subscriber from the subscriber page,
// which is a much larger grant than this requirement asks for. Scoping the
// write to their own round is what keeps it least-privilege.
var ErrNotYourCollection = errors.New("billing: that subscriber is not on your collection round")

// CollectorLoad is one staff member and how many subscribers are on their
// round, for the owner's view and the assignment dropdown.
//
// Includes staff with none assigned, so somebody can be given their first
// subscriber without having to exist on the list first.
type CollectorLoad struct {
	StaffID  int
	Username string
	Role     string
	Assigned int
}
