package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// DunningScanner escalates a subscriber whose plan has lapsed on a schedule
// measured purely in days since plan_expiry — by design (MDS §4.13), it has
// no notion of wallet_balance. That is correct for what dunning does, but it
// means a subscriber who has already topped up enough to cover their next
// cycle is suspended on schedule anyway, with their money sitting uncharged
// in the wallet. This file is the missing half: it converts "has the money"
// into "renewed," before dunning ever gets a chance to escalate them.
//
// FR: FR-BIL-008, FR-BIL-009 | MDS §4.14

const (
	// RenewalScanInterval is shorter than dunning's hourly tick so this
	// scanner reliably gets a chance to renew a funded subscriber first —
	// though because NextDunningState walks any suspended state back to
	// active once plan_expiry moves to the future, a renewal that lands
	// after an escalation self-heals on dunning's next tick regardless of
	// ordering.
	//
	// Exported because it is the longest a paying subscriber waits for
	// service to come back, which the console and the portal tell people.
	RenewalScanInterval = 15 * time.Minute
)

var (
	autorenewalTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "billing_autorenewal_total",
		Help: "Auto-renewal attempts, by result",
	}, []string{"result"})
	autorenewalInvoiceFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "billing_autorenewal_invoice_failures_total",
		Help: "Auto-renewals where the wallet debit committed but invoice creation failed",
	})
	// LifecycleActionsTotal counts staff-initiated subscriber lifecycle
	// actions (plan change, terminate, adjustment, refund). Incremented by
	// the API handlers in internal/api, which already depend on this
	// package for WalletService — kept here rather than duplicated per
	// handler so the metric has one definition.
	LifecycleActionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "billing_lifecycle_actions_total",
		Help: "Staff-initiated subscriber lifecycle actions, by action",
	}, []string{"action"})
)

// RenewalCandidate is a subscriber the auto-renewal scanner may charge.
type RenewalCandidate struct {
	SubscriberID    int
	Username        string
	FranchiseID     *int
	RegisteredState string
	// PlanID is the plan the subscriber is on as the pass starts. Compared
	// against a due scheduled change's target so a change naming the plan
	// they already hold is not treated as a move (FR-BIL-023).
	PlanID           int
	PlanName         string
	PlanPrice        decimal.Decimal
	PlanValidityDays int
	PlanVolumeGB     int
	// PlanExpiry is the zero time for a subscriber who has never had a cycle
	// (status StatusPendingPayment); renew() starts them from now.
	PlanExpiry   time.Time
	DunningState DunningState
	// Status distinguishes a renewal from a first activation, which has to
	// clear the pending status as well as stamp the expiry.
	Status string
}

// StatusPendingPayment is where signup leaves a subscriber: on the books,
// billable, and granted nothing until their first cycle is actually paid for
// (migration 048). radius.AuthorisesService refuses it.
const StatusPendingPayment = "pending_payment"

// StatusActive is a subscriber whose current cycle is paid for.
const StatusActive = "active"

// RenewalScanQuerier is the database surface the auto-renewal scanner needs.
type RenewalScanQuerier interface {
	DunningQuerier
	// ListRenewalCandidates returns subscribers whose plan has already
	// expired and whose wallet balance covers their current plan's price.
	ListRenewalCandidates(ctx context.Context) ([]RenewalCandidate, error)
	// SetPlanExpiry extends plan_expiry after a successful renewal.
	SetPlanExpiry(ctx context.Context, subscriberID int, expiry time.Time) error
	// ActivateSubscriber ends a pending_payment subscriber's first cycle
	// purchase: it stamps plan_expiry and clears the status to active in one
	// statement, so the two can never disagree.
	ActivateSubscriber(ctx context.Context, subscriberID int, expiry time.Time) error
	// ListBillableProducts returns the catalogue charges to add to this
	// subscriber's cycle: recurring attachments, and one-time ones not yet
	// invoiced (FR-BIL-022).
	ListBillableProducts(ctx context.Context, subscriberID int) ([]SubscriberProduct, error)
	// CreateInvoiceWithLines writes the invoice, its lines, and closes
	// every one-time charge it billed, in one transaction.
	CreateInvoiceWithLines(ctx context.Context, inv Invoice, lines []InvoiceLine) (int, error)
	GetActiveGstRate(ctx context.Context) (GstRate, error)
	// GetDuePlanChange returns the scheduled plan change to apply to this
	// subscriber today, or (nil, nil) if there is none — the ordinary case
	// on almost every renewal (FR-BIL-023).
	GetDuePlanChange(ctx context.Context, subscriberID int, on time.Time) (*ScheduledPlanChange, error)
	// ApplyDuePlanChange moves the subscriber onto the new plan, stamps
	// the new expiry and marks the change applied, in one transaction.
	ApplyDuePlanChange(ctx context.Context, changeID, subscriberID, newPlanID int, newExpiry time.Time, activate bool, invoiceID *int) error
	// SetPlanChangeInvoice links an applied change to the invoice that
	// billed its first cycle, once that invoice exists.
	SetPlanChangeInvoice(ctx context.Context, changeID, invoiceID int) error
}

// CacheInvalidator drops a cached auth record so the next authentication
// reloads the subscriber's new plan. Satisfied by *cache.SubscriberCache.
type CacheInvalidator interface {
	InvalidateSubscriber(ctx context.Context, username string) error
}

// SetSubscriberCache attaches auth-cache invalidation, needed when a
// renewal changes the subscriber's plan (FR-BIL-023).
//
// Optional like the emitter and the queue, but the consequence of leaving
// it out is sharper: RADIUS would keep handing out the old plan's rate
// limit from cache until the entry expired, so a subscriber who paid for
// an upgrade would not get it until then.
func (s *RecurringBillingScanner) SetSubscriberCache(c CacheInvalidator) { s.subCache = c }

// SessionLookup finds a subscriber's live session, so a plan change can
// push a CoA to the NAS holding it. Satisfied by *cache.SessionStore.
type SessionLookup interface {
	GetActiveSession(ctx context.Context, subscriberID int) (*health.SessionSummary, error)
}

// SetSessionLookup attaches live-session lookup, enabling a CoA after a
// plan change applied by a renewal.
//
// Without it the new rate limit reaches the subscriber on their next
// authentication rather than immediately. That is a real gap only for
// someone renewed while still holding a session — a subscriber in grace,
// typically — since anyone actually cut off reconnects and picks up the
// new limit anyway.
func (s *RecurringBillingScanner) SetSessionLookup(l SessionLookup) { s.sessions = l }

// RecurringBillingScanner auto-renews subscribers from their existing wallet
// balance once their plan has expired, closing the gap dunning's purely
// time-based schedule leaves open.
type RecurringBillingScanner struct {
	db     RenewalScanQuerier
	wallet *WalletService
	now    func() time.Time // injectable for tests
	events EventEmitter
	tasks  TaskEnqueuer
	runs   RunRecorder
	// gstHomeState is the supplier's own state. Empty means
	// DefaultHomeState, preserving what CalculateGstInvoice assumed before
	// per-line invoicing made the home state explicit here.
	gstHomeState string
	// subCache and sessions are needed only when a renewal changes the
	// subscriber's plan (FR-BIL-023): the cached auth record has to be
	// dropped, and a live session told about the new rate limit.
	subCache CacheInvalidator
	sessions SessionLookup
}

// SetRunRecorder records each pass for billing staff (FR-BIL-020). Optional,
// like the emitter and the queue: renewals never depend on it.
func (s *RecurringBillingScanner) SetRunRecorder(r RunRecorder) { s.runs = r }

// TaskEnqueuer is the queue surface the scanner needs to send a restoration
// notice. Satisfied by *jobqueue.Client; nil in a deployment with no queue,
// which suppresses the message rather than failing the renewal.
type TaskEnqueuer interface {
	EnqueueContext(ctx context.Context, task *jobqueue.Task, opts ...jobqueue.Option) (*jobqueue.TaskInfo, error)
}

// SetTaskEnqueuer attaches the notification queue, enabling FR-NOTIF-006's
// service-restored message.
//
// Optional for the same reason SetEventEmitter is: billing must keep renewing
// subscribers whether or not notifications are configured. A renewal that
// succeeded and went unannounced is a missing message; a renewal refused
// because a queue was absent is a subscriber left offline.
func (s *RecurringBillingScanner) SetTaskEnqueuer(t TaskEnqueuer) { s.tasks = t }

// TaskTypeServiceRestored carries FR-NOTIF-006's service-restored message.
const TaskTypeServiceRestored = "notif:service_restored"

// ServiceRestoredPayload is the task payload for a restoration notice.
type ServiceRestoredPayload struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	PlanName     string `json:"plan_name"`
	// ValidUntil is the new plan expiry, formatted, so the message can tell
	// the subscriber what they have actually bought rather than only that
	// something happened.
	ValidUntil string `json:"valid_until"`
}

// EventEmitter publishes a lifecycle event to subscribed partners.
// Satisfied by *partner.Emitter; nil in deployments with no integrations.
type EventEmitter interface {
	Emit(ctx context.Context, eventType string, entityID int)
}

// SetEventEmitter attaches partner webhook emission (FR-API-002).
//
// Optional, following SetNASResolver: a deployment with no key store cannot
// decrypt signing secrets, and the scanner must keep renewing subscribers
// regardless — billing is not allowed to depend on a partner integration.
func (s *RecurringBillingScanner) SetEventEmitter(e EventEmitter) { s.events = e }

// NewRecurringBillingScanner constructs a RecurringBillingScanner.
func NewRecurringBillingScanner(db RenewalScanQuerier, wallet *WalletService) *RecurringBillingScanner {
	return &RecurringBillingScanner{db: db, wallet: wallet, now: time.Now}
}

// Run scans every 15 minutes until ctx is cancelled, scanning once
// immediately so a fresh deployment does not wait for the first tick.
func (s *RecurringBillingScanner) Run(ctx context.Context) {
	if err := s.Scan(ctx); err != nil {
		log.Error().Err(err).Msg("billing: auto-renewal scan error")
	}
	ticker := time.NewTicker(RenewalScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Scan(ctx); err != nil {
				log.Error().Err(err).Msg("billing: auto-renewal scan error")
			}
		}
	}
}

// Scan performs one pass. Exported so a test can drive a single pass without
// waiting on the ticker.
func (s *RecurringBillingScanner) Scan(ctx context.Context) error {
	// Name the scanner as the actor for migration 031's status-capture
	// trigger, exactly as DunningScanner.Scan does. Without this, the
	// restoration an auto-renewal performs is recorded as "unknown" while the
	// suspension that preceded it is correctly attributed to
	// system:dunning-scanner — so the history of one subscriber's cycle names
	// who cut them off but not who turned them back on.
	ctx = middleware.WithSubject(ctx, "system:auto-renewal-scanner")

	runID := s.startRun(ctx)
	sum := RunSummary{AmountCharged: decimal.Zero}

	candidates, err := s.db.ListRenewalCandidates(ctx)
	if err != nil {
		sum.Error = err.Error()
		s.finishRun(ctx, runID, RunFailed, sum)
		return fmt.Errorf("auto-renewal scan: list candidates: %w", err)
	}
	sum.Candidates = len(candidates)

	for _, c := range candidates {
		res := s.renew(ctx, c)
		switch res.outcome {
		case outcomeRenewed:
			sum.Renewed++
		case outcomeActivated:
			sum.Activated++
		case outcomeInsufficient:
			sum.InsufficientBalance++
		case outcomeFailed:
			// One subscriber's failure must not stop the rest of the batch.
			sum.Failed++
			log.Error().Err(res.err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal failed")
			autorenewalTotal.WithLabelValues("error").Inc()
		}
		sum.AmountCharged = sum.AmountCharged.Add(res.charged)
		if res.invoiced {
			sum.InvoicesRaised++
		}
		for _, f := range res.problems {
			if f.Stage == StageInvoice {
				sum.InvoiceFailures++
			}
			sum.Failures = append(sum.Failures, f)
		}
	}
	s.finishRun(ctx, runID, RunCompleted, sum)
	return nil
}

func (s *RecurringBillingScanner) startRun(ctx context.Context) int {
	if s.runs == nil {
		return 0
	}
	id, err := s.runs.StartRun(ctx, RunKindRecurringBilling)
	if err != nil {
		log.Warn().Err(err).Msg("billing: could not record the start of an auto-renewal run")
		return 0
	}
	return id
}

func (s *RecurringBillingScanner) finishRun(ctx context.Context, runID int, status string, sum RunSummary) {
	if s.runs == nil || runID == 0 {
		return
	}
	if err := s.runs.FinishRun(ctx, runID, status, sum); err != nil {
		log.Warn().Err(err).Int("run_id", runID).Msg("billing: could not record the end of an auto-renewal run")
	}
}

type renewOutcome int

const (
	outcomeFailed renewOutcome = iota
	outcomeRenewed
	outcomeActivated
	outcomeInsufficient
)

// renewResult is what one renewal did, for the run record.
type renewResult struct {
	outcome  renewOutcome
	err      error           // set when outcome is outcomeFailed
	charged  decimal.Decimal // what the wallet was debited, zero if nothing
	invoiced bool
	problems []RunFailure // every step that failed, including after the charge
	// planChanged is the scheduled change this renewal applied, nil when
	// the subscriber simply renewed what they were already on.
	planChanged *ScheduledPlanChange
}

func (r *renewResult) fail(c RenewalCandidate, stage string, err error) {
	r.problems = append(r.problems, RunFailure{
		SubscriberID: c.SubscriberID, Username: c.Username, Stage: stage,
		Message: err.Error(), Charged: StageCharged(stage),
	})
}

// renew charges one subscriber for their current plan out of their existing
// wallet balance, extends plan_expiry, invoices the cycle, and — if the
// subscriber had already been escalated by dunning before this scanner
// caught them — restores dunning_state to active immediately rather than
// waiting up to an hour for the next dunning tick to notice.
func (s *RecurringBillingScanner) renew(ctx context.Context, c RenewalCandidate) (res renewResult) {
	res.charged = decimal.Zero
	failed := func(stage string, err error) renewResult {
		res.outcome, res.err = outcomeFailed, err
		res.fail(c, stage, err)
		return res
	}
	now := s.now()

	// A scheduled plan change due today is resolved first, before anything
	// is priced (FR-BIL-023).
	//
	// It has to come first because it changes what this cycle costs: the
	// subscriber is buying the new plan, so the invoice, the debit and the
	// cycle length must all be the new plan's. Pricing the old plan and
	// moving them afterwards would charge them for what they are leaving.
	//
	// A failure to read the schedule stops the renewal rather than quietly
	// renewing the old plan: the alternative is a customer who asked to be
	// moved on the 1st being billed another month of the plan they are
	// leaving, which nobody would notice until they complained.
	change, err := s.duePlanChange(ctx, c)
	if err != nil {
		return failed(StagePrice, fmt.Errorf("plan change lookup for subscriber %d: %w", c.SubscriberID, err))
	}
	if change != nil {
		c = applyPlanToCandidate(c, change)
	}

	// The invoice is computed BEFORE the debit, because its total is what the
	// subscriber owes.
	//
	// plans.price is GST-exclusive, so a renewal costs price + GST. This
	// previously debited c.PlanPrice while invoicing price + GST on top — so
	// every renewal collected 18% less than it billed. That is not a rounding
	// discrepancy: the invoice is what gets declared on GSTR-1, so the
	// operator was remitting tax on money never collected, and the gap grew
	// with every cycle.
	//
	// Ordering it this way also means a missing GST rate stops the renewal
	// rather than producing an uninvoiced charge. Refusing to renew is the
	// safer failure: the subscriber keeps their money and dunning picks them
	// up, whereas charging without an invoice is money taken with no tax
	// record behind it.
	inv, lines, err := s.buildInvoice(ctx, c)
	if err != nil {
		return failed(StagePrice, fmt.Errorf("price renewal for subscriber %d: %w", c.SubscriberID, err))
	}

	// The tax is handed over rather than re-derived, so the GL's 2200 GST
	// Payable line and the invoice's CGST/SGST/IGST columns are the same
	// numbers — the ledger has to agree with the document that gets filed.
	_, err = s.wallet.Post(ctx, PostRequest{
		SubscriberID:   c.SubscriberID,
		FranchiseID:    c.FranchiseID,
		Amount:         inv.TotalAmount,
		TaxAmount:      inv.CgstAmount.Add(inv.SgstAmount).Add(inv.IgstAmount),
		Direction:      "debit",
		CounterAccount: AccountRevenueClearing,
		Description:    renewalMemo(c.PlanName, len(lines)),
	})
	if err != nil {
		if errors.Is(err, ErrInsufficientBalance) {
			// The candidate query and this debit are not atomic against a
			// concurrent debit on the same subscriber (a staff adjustment,
			// for instance) — losing that race is an expected outcome, not
			// a failure: the subscriber is simply left for dunning.
			//
			// Since FR-BIL-022 the candidate query's "balance covers the
			// plan price" test is necessary but no longer sufficient: a
			// subscriber carrying an installation charge owes plan plus
			// charges plus GST, so one whose balance covers only the plan
			// lands here every pass until they top up. That is the right
			// answer — they are short of what they owe — and the run
			// record (FR-BIL-020) counts them, so it is visible rather
			// than silent.
			autorenewalTotal.WithLabelValues("insufficient_balance").Inc()
			res.outcome = outcomeInsufficient
			return res
		}
		return failed(StageDebit, fmt.Errorf("debit subscriber %d: %w", c.SubscriberID, err))
	}
	res.charged = inv.TotalAmount

	// max(now, currentExpiry): a renewal never shortens a cycle already
	// paid for. This scanner is the only path that renews: portal payments
	// credit the wallet through the gateway webhook and are renewed here.
	// Candidates are already past their expiry by construction, so this is normally just now +
	// validity_days, but the max() guards the same race the debit above
	// does — the expiry could have moved between the candidate query and here.
	base := now
	if c.PlanExpiry.After(base) {
		base = c.PlanExpiry
	}
	newExpiry := base.AddDate(0, 0, c.PlanValidityDays)
	activate := c.Status == StatusPendingPayment
	switch {
	case change != nil:
		// A due plan change takes over stamping the expiry, because the
		// move and the new dates have to land together: plan_id and
		// plan_expiry disagreeing would mean a subscriber billed for one
		// plan and rate-limited by another, and the change is marked
		// applied in the same transaction so an overlapping pass cannot
		// apply it twice.
		//
		// Only now, after the debit has committed — a subscriber who could
		// not pay stays on the plan they have, with the change still
		// waiting for a pass where they can.
		if err := s.db.ApplyDuePlanChange(ctx, change.ID, c.SubscriberID, change.NewPlanID, newExpiry, activate, nil); err != nil {
			return failed(StagePlanChange, fmt.Errorf("apply plan change %d for subscriber %d: %w",
				change.ID, c.SubscriberID, err))
		}
		res.planChanged = change
	case activate:
		// First cycle: the expiry and the status have to move together. Two
		// statements would leave a window where the subscriber is charged and
		// dated but still unauthorised, or authorised with no expiry — which
		// is the exact state migration 048 exists to eliminate.
		if err := s.db.ActivateSubscriber(ctx, c.SubscriberID, newExpiry); err != nil {
			return failed(StageActivation, fmt.Errorf("activate subscriber %d: %w", c.SubscriberID, err))
		}
	default:
		if err := s.db.SetPlanExpiry(ctx, c.SubscriberID, newExpiry); err != nil {
			return failed(StageExpiry, fmt.Errorf("extend plan_expiry for subscriber %d: %w", c.SubscriberID, err))
		}
	}

	// The plan the subscriber authenticates against has just changed, so
	// the cached copy of it is now wrong. Done before the invoice because
	// it is what the customer actually notices: the invoice can be
	// reconciled later, a stale rate limit is the upgrade they paid for
	// not arriving.
	if change != nil {
		s.announcePlanChange(ctx, c, change)
	}

	if err := s.persistInvoice(ctx, inv, lines, change); err != nil {
		// The wallet debit above already committed and must not be undone
		// just because persisting failed — log for reconciliation rather
		// than leaving a charged subscriber un-invoiced on a retry.
		autorenewalInvoiceFailures.Inc()
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal invoice failed")
		res.fail(c, StageInvoice, err)
	} else {
		res.invoiced = true
	}

	if c.DunningState != DunningActive {
		if err := TransitionDunning(ctx, s.db, c.SubscriberID, DunningActive); err != nil {
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal dunning restore failed")
			res.fail(c, StageDunning, err)
		} else if wasCutOff(c.DunningState) {
			// A soft-suspended subscriber is still online, behind the walled
			// garden their session was accepted into. Paying does not change
			// that session; ending it does. See releaseSession.
			if radius.LiveSessionMustEnd(dunningToSubscriberStatus(c.DunningState), dunningToSubscriberStatus(DunningActive)) {
				s.releaseSession(ctx, c, newExpiry)
			}
			// FR-NOTIF-006, sent from here because here is where service is
			// actually restored.
			//
			// It used to be the payment webhook's job, which meant a
			// suspended subscriber was told they were back the moment their
			// money landed — while still cut off, with up to fifteen minutes
			// to wait for this scanner — and then told nothing at all when
			// service genuinely returned. The webhook now acknowledges the
			// payment and claims nothing about service.
			//
			// Only for a subscriber who was actually cut off: someone renewed
			// out of a reminder stage never lost service, and "your service
			// has been restored" would be the first they heard of a problem
			// that did not exist.
			s.enqueueRestorationNotice(ctx, c, newExpiry)
		}
	}

	autorenewalTotal.WithLabelValues("renewed").Inc()
	res.outcome = outcomeRenewed
	if c.Status == StatusPendingPayment {
		res.outcome = outcomeActivated
	}
	return res
}

// renewalMemo describes the debit on the wallet ledger. With catalogue
// charges on the cycle the plan name alone no longer accounts for the
// amount, and a subscriber reading their ledger should be able to see why
// this month cost more than the plan.
func renewalMemo(planName string, lineCount int) string {
	if lineCount <= 1 {
		return fmt.Sprintf("auto-renewal: %s", planName)
	}
	extras := lineCount - 1
	noun := "charges"
	if extras == 1 {
		noun = "charge"
	}
	return fmt.Sprintf("auto-renewal: %s + %d %s", planName, extras, noun)
}

// wasCutOff reports whether a dunning stage means service had actually been
// withdrawn, as opposed to the subscriber merely being chased for payment.
//
// grace_period is not included: it maps to a subscriber status RADIUS still
// authorises (radius.AuthorisesService), so nothing was ever restored.
func wasCutOff(state DunningState) bool {
	return state == DunningSoftSuspended || state == DunningHardSuspended
}

// releaseSession ends the live session of a subscriber this renewal has just
// lifted out of soft suspension, so the router redials and the new Accept
// carries full service instead of the walled garden.
//
// Without it a subscriber who paid would be told their service was restored
// (enqueueRestorationNotice, below) while still reaching nothing but the
// payment page, and would stay that way until their line happened to drop.
// The username rides on the disconnect so radiusd clears its auth cache just
// before sending it; the cache is cleared here too, in case the redial beats
// the queue.
//
// Failures are logged and swallowed for the reason the notice's are: the
// renewal has committed, and undoing it over a disconnect would be worse.
func (s *RecurringBillingScanner) releaseSession(ctx context.Context, c RenewalCandidate, newExpiry time.Time) {
	if s.subCache != nil {
		if err := s.subCache.InvalidateSubscriber(ctx, c.Username); err != nil {
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
				Msg("billing: auth-cache invalidation failed releasing a renewed subscriber")
		}
	}
	if s.tasks == nil {
		return
	}
	payload, err := json.Marshal(fup.PoDPayload{SubscriberID: c.SubscriberID, Username: c.Username})
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: release disconnect payload")
		return
	}
	// Keyed per renewal, like the notice, so an overlapping pass queues one.
	task := jobqueue.NewTask(fup.TaskTypePoD, payload,
		jobqueue.Queue(fup.QueueNetCommands),
		jobqueue.TaskID(fmt.Sprintf("renewal-release-%d-%d", c.SubscriberID, newExpiry.Unix())),
		jobqueue.MaxRetry(5),
		jobqueue.Retention(24*time.Hour))
	if _, err := s.tasks.EnqueueContext(ctx, task); err != nil && !errors.Is(err, jobqueue.ErrTaskIDConflict) {
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
			Msg("billing: could not queue the disconnect that lets a renewed subscriber out of the walled garden")
	}
}

// enqueueRestorationNotice tells a subscriber their service is back.
//
// Failures are logged and swallowed: the renewal has committed and the
// subscriber is online, and failing the renewal over an undelivered message
// would undo a restoration that has already happened.
func (s *RecurringBillingScanner) enqueueRestorationNotice(ctx context.Context, c RenewalCandidate, newExpiry time.Time) {
	if s.tasks == nil {
		return
	}
	payload, err := json.Marshal(ServiceRestoredPayload{
		SubscriberID: c.SubscriberID,
		Username:     c.Username,
		PlanName:     c.PlanName,
		ValidUntil:   newExpiry.Format("2006-01-02"),
	})
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: restoration notice payload marshal failed")
		return
	}
	// Keyed per subscriber per cycle, like the dunning notices: a scanner
	// restart or an overlapping run must not tell the same subscriber twice
	// that they are back online.
	task := jobqueue.NewTask(TaskTypeServiceRestored, payload,
		jobqueue.Queue(QueueNotifications),
		jobqueue.TaskID(fmt.Sprintf("restored-%d-%d", c.SubscriberID, newExpiry.Unix())),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))
	if _, err := s.tasks.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, jobqueue.ErrTaskIDConflict) {
			return
		}
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: restoration notice enqueue failed")
	}
}

// invoice generates and persists the GST invoice for one auto-renewed cycle.
//
// gb_used is recorded as zero: this codebase has no per-cycle usage
// aggregation at renewal time (subscriber_session_history tracks per-session
// usage, not usage-since-last-invoice) — gb_included still carries the
// plan's volume so the invoice is not silently missing FR-BIL-007's summary,
// but the "used" half of that summary is a known limitation for the
// auto-renewal path until that aggregation exists.
// buildInvoice computes the cycle's invoice and its lines without
// persisting either, so renew can debit exactly what the invoice says is
// owed.
//
// Split from persistence deliberately: the total has to be known before the
// wallet is touched, and building it is pure computation that can fail
// (a missing GST rate) without any side effect to unwind.
//
// The cycle is the plan line plus every catalogue charge the subscriber
// holds — recurring rentals every cycle, one-time charges once
// (FR-BIL-022). Each is taxed in its own right and the header is their
// sum; see ComposeInvoice.
//
// A failure to read the attached products stops the renewal rather than
// billing the plan alone. Under-billing silently is the worse outcome: the
// subscriber would be renewed, the rental would go uncharged for that
// cycle, and nothing in the invoice would show that anything was missing.
func (s *RecurringBillingScanner) buildInvoice(ctx context.Context, c RenewalCandidate) (Invoice, []InvoiceLine, error) {
	rate, err := s.db.GetActiveGstRate(ctx)
	if err != nil {
		return Invoice{}, nil, fmt.Errorf("get active gst rate: %w", err)
	}

	attached, err := s.db.ListBillableProducts(ctx, c.SubscriberID)
	if err != nil {
		return Invoice{}, nil, fmt.Errorf("list billable products: %w", err)
	}

	charges := make([]LineCharge, 0, len(attached)+1)
	charges = append(charges, PlanCharge(c.PlanName, c.PlanPrice))
	for _, sp := range attached {
		charges = append(charges, ProductCharge(sp))
	}

	lines, inv, err := ComposeInvoice(charges, c.RegisteredState, s.homeState(), rate)
	if err != nil {
		return Invoice{}, nil, fmt.Errorf("compose invoice: %w", err)
	}
	inv.SubscriberID = c.SubscriberID
	inv.GbIncluded = c.PlanVolumeGB
	inv.GbUsed = decimal.Zero
	return inv, lines, nil
}

// homeState is the supplier's own state, deciding CGST+SGST against IGST.
func (s *RecurringBillingScanner) homeState() string {
	if s.gstHomeState != "" {
		return s.gstHomeState
	}
	return DefaultHomeState
}

// SetGSTHomeState configures the supplier's own state. Without it the
// scanner uses DefaultHomeState, which is what CalculateGstInvoice did
// before this was configurable.
func (s *RecurringBillingScanner) SetGSTHomeState(state string) { s.gstHomeState = state }

// announcePlanChange makes a just-applied plan change take effect on the
// network: it drops the cached auth record and, if the subscriber is
// holding a session, pushes a CoA so the new rate limit applies without
// waiting for them to reconnect.
//
// Every failure here is logged and swallowed. The change has committed and
// the subscriber has paid for it; refusing the renewal now would undo a
// move that has already happened, and the worst case without these is a
// subscriber on the right plan at the old speed until their cached record
// expires or their session ends.
func (s *RecurringBillingScanner) announcePlanChange(ctx context.Context, c RenewalCandidate, change *ScheduledPlanChange) {
	if s.subCache != nil {
		if err := s.subCache.InvalidateSubscriber(ctx, c.Username); err != nil {
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
				Msg("billing: auth-cache invalidation failed after a scheduled plan change")
		}
	}
	if s.sessions == nil || s.tasks == nil {
		return
	}
	sess, err := s.sessions.GetActiveSession(ctx, c.SubscriberID)
	if err != nil || sess == nil {
		// No session, or we could not tell. Nothing to re-rate: the new
		// limit applies on their next authentication.
		return
	}
	payload, err := json.Marshal(fup.CoAPayload{SubscriberID: c.SubscriberID, NasIP: sess.NasIP})
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: CoA payload marshal failed")
		return
	}
	// Keyed per subscriber per change, so a retried or overlapping pass
	// does not queue the same re-rate twice.
	task := jobqueue.NewTask(fup.TaskTypeCoA, payload,
		jobqueue.Queue(fup.QueueNetCommands),
		jobqueue.TaskID(fmt.Sprintf("planchange-coa-%d-%d", c.SubscriberID, change.ID)),
		jobqueue.MaxRetry(5),
		jobqueue.Retention(24*time.Hour))
	if _, err := s.tasks.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, jobqueue.ErrTaskIDConflict) {
			return
		}
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
			Msg("billing: CoA enqueue failed after a scheduled plan change")
	}
}

// duePlanChange returns the scheduled plan change this renewal should
// apply, or nil.
//
// A terminated subscriber never reaches here (they are not renewal
// candidates), so the only filtering needed is the date, which the store
// does.
func (s *RecurringBillingScanner) duePlanChange(ctx context.Context, c RenewalCandidate) (*ScheduledPlanChange, error) {
	change, err := s.db.GetDuePlanChange(ctx, c.SubscriberID, s.now())
	if err != nil {
		return nil, err
	}
	if change == nil {
		return nil, nil
	}
	// Already on the target plan — an immediate change got there first, or
	// the same change was scheduled twice across a plan edit. There is
	// nothing to move, so the renewal proceeds as an ordinary one and the
	// change is left pending rather than silently consumed: closing it
	// here would need its own transaction and a reason, and leaving it
	// means an operator sees it still listed and can cancel it.
	if change.NewPlanID == c.PlanID {
		log.Info().Int("subscriber_id", c.SubscriberID).Int("change_id", change.ID).
			Msg("billing: scheduled plan change names the plan the subscriber is already on; renewing without it")
		return nil, nil
	}
	return change, nil
}

// applyPlanToCandidate rewrites the candidate's plan fields to the plan it
// is about to be moved to, so the cycle is priced, dated and invoiced as
// the new plan throughout.
//
// PlanExpiry and Status are left alone: they describe the cycle ending,
// not the one being bought.
func applyPlanToCandidate(c RenewalCandidate, change *ScheduledPlanChange) RenewalCandidate {
	c.PlanID = change.NewPlanID
	c.PlanName = change.NewPlanName
	c.PlanPrice = change.NewPlanPrice
	c.PlanValidityDays = change.NewPlanValidityDays
	c.PlanVolumeGB = change.NewPlanVolumeGB
	return c
}

// persistInvoice writes the computed invoice with its lines and announces
// it. Closing the one-time charges it billed is part of the same
// transaction — see CreateInvoiceWithLines.
//
// When this cycle applied a scheduled plan change, the invoice is linked
// back to it so "what did the change cost them" is answerable from the
// row. That link is a convenience, so failing to write it is logged and
// the renewal stands.
func (s *RecurringBillingScanner) persistInvoice(ctx context.Context, inv Invoice, lines []InvoiceLine, change *ScheduledPlanChange) error {
	invoiceID, err := s.db.CreateInvoiceWithLines(ctx, inv, lines)
	if err != nil {
		return fmt.Errorf("create invoice: %w", err)
	}
	if change != nil {
		if err := s.db.SetPlanChangeInvoice(ctx, change.ID, invoiceID); err != nil {
			log.Warn().Err(err).Int("change_id", change.ID).Int("invoice_id", invoiceID).
				Msg("billing: could not link an applied plan change to its invoice")
		}
	}
	// After the row exists, never before: a partner told an invoice was
	// generated must be able to fetch it.
	if s.events != nil {
		s.events.Emit(ctx, "invoice.generated", invoiceID)
	}
	return nil
}
