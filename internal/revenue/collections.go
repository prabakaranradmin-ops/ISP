package revenue

import (
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// Collections reporting — FR-REV-003 | CRD-REV-002 | API §7.
//
// Two questions an owner asks about money that has not arrived: how much is
// sitting in each stage of the dunning ladder, and whether collection is
// improving or getting worse month over month.
//
// A note on what "outstanding" means here, because this system is prepaid
// and the word usually implies a receivable. subscribers.wallet_balance can
// never go negative (billing.ErrInsufficientBalance, backed by a CHECK
// constraint), so nobody carries a debit balance and there is no invoice
// status column to age. What a lapsed subscriber owes is the price of the
// plan they must renew to be current again - so that is what is summed
// here, per stage. It is an exposure figure, not a legal receivable, and
// the screen says so rather than letting it be mistaken for one.

// CollectionsStageRow is one dunning stage's exposure.
type CollectionsStageRow struct {
	// DunningState is the ladder position: remind_7d, remind_3d,
	// remind_1d, grace_period, soft_suspended, hard_suspended. 'active'
	// is excluded - a current subscriber owes nothing.
	DunningState string
	Subscribers  int
	// Outstanding is the sum of plan price across subscribers in this
	// stage: what it would take for all of them to become current.
	Outstanding decimal.Decimal
	// ServiceStopped distinguishes the stages where the subscriber is off
	// the network from the ones where they are still online and merely
	// being reminded. The distinction drives how urgently a collections
	// team works a stage, and reading it off the state name in a template
	// would put that rule in two places.
	ServiceStopped bool
}

// RecoveryMonth is one calendar month of collections.
//
// Derived from wallet_ledgers credits rather than from dunning
// transitions, because no table records those transitions over time -
// and money actually received is the more honest measure of recovery
// regardless.
type RecoveryMonth struct {
	Month     time.Time
	Collected decimal.Decimal
	// Payers is the number of distinct subscribers who paid, which
	// separates "one large recharge" from "many subscribers recovered".
	Payers int
}

// ServiceStoppedIn reports whether a dunning stage means service has
// actually been restricted.
//
// grace_period still leaves the subscriber online (the dunning scanner's
// own comment: entering remind_7d/3d/1d or grace_period still leaves the
// subscriber online); only the two suspensions stop service.
func ServiceStoppedIn(dunningState string) bool {
	switch dunningState {
	case string(billing.DunningSoftSuspended), string(billing.DunningHardSuspended):
		return true
	default:
		return false
	}
}

// RecoveryRate returns the month-over-month change in collections as a
// percentage, and whether it is meaningful.
//
// Not meaningful when the prior month collected nothing: the change from
// zero is arithmetically infinite, and rendering "+100%" or "+∞%" for a
// first month of operation would be worse than saying there is nothing to
// compare against.
func RecoveryRate(current, prior decimal.Decimal) (decimal.Decimal, bool) {
	if prior.IsZero() {
		return decimal.Zero, false
	}
	return current.Sub(prior).
		Div(prior).
		Mul(decimal.NewFromInt(100)).
		Round(1), true
}
