package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
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
	// recurringBillingScanInterval is shorter than dunning's hourly tick so
	// this scanner reliably gets a chance to renew a funded subscriber first
	// — though because NextDunningState walks any suspended state back to
	// active once plan_expiry moves to the future, a renewal that lands
	// after an escalation self-heals on dunning's next tick regardless of
	// ordering.
	recurringBillingScanInterval = 15 * time.Minute
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
	SubscriberID     int
	Username         string
	FranchiseID      *int
	RegisteredState  string
	PlanName         string
	PlanPrice        decimal.Decimal
	PlanValidityDays int
	PlanVolumeGB     int
	PlanExpiry       time.Time
	DunningState     DunningState
}

// RenewalScanQuerier is the database surface the auto-renewal scanner needs.
type RenewalScanQuerier interface {
	DunningQuerier
	// ListRenewalCandidates returns subscribers whose plan has already
	// expired and whose wallet balance covers their current plan's price.
	ListRenewalCandidates(ctx context.Context) ([]RenewalCandidate, error)
	// SetPlanExpiry extends plan_expiry after a successful renewal.
	SetPlanExpiry(ctx context.Context, subscriberID int, expiry time.Time) error
	CreateInvoice(ctx context.Context, inv Invoice) (int, error)
	GetActiveGstRate(ctx context.Context) (GstRate, error)
}

// RecurringBillingScanner auto-renews subscribers from their existing wallet
// balance once their plan has expired, closing the gap dunning's purely
// time-based schedule leaves open.
type RecurringBillingScanner struct {
	db     RenewalScanQuerier
	wallet *WalletService
	now    func() time.Time // injectable for tests
	events EventEmitter
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
	ticker := time.NewTicker(recurringBillingScanInterval)
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

	candidates, err := s.db.ListRenewalCandidates(ctx)
	if err != nil {
		return fmt.Errorf("auto-renewal scan: list candidates: %w", err)
	}

	for _, c := range candidates {
		if err := s.renew(ctx, c); err != nil {
			// One subscriber's failure must not stop the rest of the batch.
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal failed")
			autorenewalTotal.WithLabelValues("error").Inc()
		}
	}
	return nil
}

// renew charges one subscriber for their current plan out of their existing
// wallet balance, extends plan_expiry, invoices the cycle, and — if the
// subscriber had already been escalated by dunning before this scanner
// caught them — restores dunning_state to active immediately rather than
// waiting up to an hour for the next dunning tick to notice.
func (s *RecurringBillingScanner) renew(ctx context.Context, c RenewalCandidate) error {
	now := s.now()

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
	inv, err := s.buildInvoice(ctx, c)
	if err != nil {
		return fmt.Errorf("price renewal for subscriber %d: %w", c.SubscriberID, err)
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
		Description:    fmt.Sprintf("auto-renewal: %s", c.PlanName),
	})
	if err != nil {
		if errors.Is(err, ErrInsufficientBalance) {
			// The candidate query and this debit are not atomic against a
			// concurrent debit on the same subscriber (a staff adjustment,
			// for instance) — losing that race is an expected outcome, not
			// a failure: the subscriber is simply left for dunning.
			autorenewalTotal.WithLabelValues("insufficient_balance").Inc()
			return nil
		}
		return fmt.Errorf("debit subscriber %d: %w", c.SubscriberID, err)
	}

	// max(now, currentExpiry): identical rule to cmd/api/main.go's
	// extendPlanExpiry for portal renewal, so early and auto renewal never
	// disagree about how a renewal extends. Candidates are already past
	// their expiry by construction, so this is normally just now +
	// validity_days, but the max() guards the same race the debit above
	// does — the expiry could have moved between the candidate query and here.
	base := now
	if c.PlanExpiry.After(base) {
		base = c.PlanExpiry
	}
	newExpiry := base.AddDate(0, 0, c.PlanValidityDays)
	if err := s.db.SetPlanExpiry(ctx, c.SubscriberID, newExpiry); err != nil {
		return fmt.Errorf("extend plan_expiry for subscriber %d: %w", c.SubscriberID, err)
	}

	if err := s.persistInvoice(ctx, inv); err != nil {
		// The wallet debit above already committed and must not be undone
		// just because persisting failed — log for reconciliation rather
		// than leaving a charged subscriber un-invoiced on a retry.
		autorenewalInvoiceFailures.Inc()
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal invoice failed")
	}

	if c.DunningState != DunningActive {
		if err := TransitionDunning(ctx, s.db, c.SubscriberID, DunningActive); err != nil {
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: auto-renewal dunning restore failed")
		}
	}

	autorenewalTotal.WithLabelValues("renewed").Inc()
	return nil
}

// invoice generates and persists the GST invoice for one auto-renewed cycle.
//
// gb_used is recorded as zero: this codebase has no per-cycle usage
// aggregation at renewal time (subscriber_session_history tracks per-session
// usage, not usage-since-last-invoice) — gb_included still carries the
// plan's volume so the invoice is not silently missing FR-BIL-007's summary,
// but the "used" half of that summary is a known limitation for the
// auto-renewal path until that aggregation exists.
// buildInvoice computes the cycle's invoice without persisting it, so renew
// can debit exactly what the invoice says is owed.
//
// Split from persistence deliberately: the total has to be known before the
// wallet is touched, and building it is pure computation that can fail
// (a missing GST rate) without any side effect to unwind.
func (s *RecurringBillingScanner) buildInvoice(ctx context.Context, c RenewalCandidate) (Invoice, error) {
	rate, err := s.db.GetActiveGstRate(ctx)
	if err != nil {
		return Invoice{}, fmt.Errorf("get active gst rate: %w", err)
	}
	inv := CalculateGstInvoice(c.PlanPrice, c.RegisteredState, rate)
	inv.SubscriberID = c.SubscriberID
	inv.GbIncluded = c.PlanVolumeGB
	inv.GbUsed = decimal.Zero
	return inv, nil
}

// persistInvoice writes the computed invoice and announces it.
func (s *RecurringBillingScanner) persistInvoice(ctx context.Context, inv Invoice) error {
	invoiceID, err := s.db.CreateInvoice(ctx, inv)
	if err != nil {
		return fmt.Errorf("create invoice: %w", err)
	}
	// After the row exists, never before: a partner told an invoice was
	// generated must be able to fetch it.
	if s.events != nil {
		s.events.Emit(ctx, "invoice.generated", invoiceID)
	}
	return nil
}
