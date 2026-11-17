package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Recurring-billing run history — FR-BIL-020 | migration 057.
//
// Each pass of RecurringBillingScanner is recorded: when it started and
// stopped, what it did, and every subscriber it could not finish. The
// counters and logs it already wrote are for operators watching dashboards;
// this is for billing staff asking "did last night's renewals go through,
// and who do I need to look at?"

// Run statuses.
const (
	RunRunning   = "running"
	RunCompleted = "completed" // the pass finished; some subscribers may still have failed
	RunFailed    = "failed"    // the pass itself could not run (the candidate list failed)
	RunAbandoned = "abandoned" // still "running" an hour later: the process stopped mid-pass
)

// Failure stages, in the order renew() reaches them. Charged says whether
// the wallet had already been debited when the step failed, which is what
// decides how urgent the failure is.
const (
	StagePrice      = "price"      // could not compute the invoice: nothing charged
	StageDebit      = "debit"      // the wallet debit failed: nothing charged
	StageExpiry     = "expiry"     // charged, but the plan was not extended
	StageActivation = "activation" // charged, but the first cycle was not activated
	StageInvoice    = "invoice"    // charged and renewed, but no invoice was written
	StageDunning    = "dunning"    // renewed, but the dunning state was not restored
)

// StageCharged reports whether a failure at stage happened after the
// wallet was debited.
func StageCharged(stage string) bool {
	switch stage {
	case StageExpiry, StageActivation, StageInvoice, StageDunning:
		return true
	}
	return false
}

// RunFailure is one subscriber a pass could not finish.
type RunFailure struct {
	SubscriberID int
	Username     string
	Stage        string
	Message      string
	Charged      bool
}

// RunSummary is what one pass did.
type RunSummary struct {
	Candidates          int
	Renewed             int // existing subscribers renewed
	Activated           int // pending subscribers' first cycles
	InsufficientBalance int // wallet no longer covered it by the time of the debit
	Failed              int // could not be renewed: an error before or after the charge
	InvoicesRaised      int
	InvoiceFailures     int // charged and renewed with no invoice written
	AmountCharged       decimal.Decimal
	Failures            []RunFailure
	// Error is set when the pass itself could not run.
	Error string
}

// RunRecorder records passes. Satisfied by *db.BillingStore. Optional: a
// scanner with no recorder renews exactly as before, and a recorder that
// fails is logged and ignored — renewals never wait on their own paperwork.
type RunRecorder interface {
	// StartRun opens a run and returns its id. It also closes any run left
	// "running" for over an hour as abandoned.
	StartRun(ctx context.Context, kind string) (int, error)
	FinishRun(ctx context.Context, runID int, status string, s RunSummary) error
}

// RunKindRecurringBilling names the auto-renewal scanner's runs.
const RunKindRecurringBilling = "recurring_billing"

// BillingRun is one recorded run, as listed.
type BillingRun struct {
	ID                  int
	Kind                string
	StartedAt           time.Time
	FinishedAt          *time.Time
	Status              string
	Candidates          int
	Renewed             int
	Activated           int
	InsufficientBalance int
	Failed              int
	InvoicesRaised      int
	InvoiceFailures     int
	AmountCharged       decimal.Decimal
	Error               string
	Failures            []RunFailure // filled by the single-run lookup only
}

// Duration is how long the run took, or zero while it runs.
func (r BillingRun) Duration() time.Duration {
	if r.FinishedAt == nil {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ChargedFailures counts failures that happened after the wallet was
// debited: the ones where a customer has paid without getting everything
// they paid for.
func (r BillingRun) ChargedFailures() int {
	n := 0
	for _, f := range r.Failures {
		if f.Charged {
			n++
		}
	}
	return n
}

// BillingRunFilter narrows a run list.
type BillingRunFilter struct {
	// IncludeIdle includes passes that found nobody to renew, which is most
	// of them on a 15-minute schedule.
	IncludeIdle bool
	// ProblemsOnly keeps runs that failed, were abandoned, or had any
	// subscriber failure.
	ProblemsOnly bool
	Limit        int
}
